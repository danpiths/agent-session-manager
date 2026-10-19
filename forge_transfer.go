package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"
)

func (f *ForgeProvider) CaptureSession(s Session) (SessionTransfer, error) {
	transfer := SessionTransfer{Session: s, Native: make(map[string][]byte)}
	db, err := readOnlyDB(s.FilePath)
	if err != nil {
		return transfer, err
	}
	defer db.Close()
	var raw string
	if err = db.QueryRow("SELECT context FROM conversations WHERE conversation_id=?", s.ID).Scan(&raw); err != nil {
		return transfer, err
	}
	var content map[string]any
	if err = json.Unmarshal([]byte(raw), &content); err != nil {
		return transfer, err
	}
	transfer.Session.History = nil
	entries, _ := content["messages"].([]any)
	for _, entry := range entries {
		wrapper, _ := entry.(map[string]any)
		value, _ := wrapper["message"].(map[string]any)
		if value == nil {
			value = wrapper
		}
		m := ChatMessage{Timestamp: s.Timestamp}
		if text, ok := value["text"].(map[string]any); ok {
			m.Role = strings.ToLower(strVal(text, "role"))
			m.Model = strVal(text, "model")
			if t := strVal(text, "content"); t != "" {
				m.Content = t
				m.Blocks = append(m.Blocks, ContentBlock{Kind: "text", Text: t})
			}
			if calls, ok := text["tool_calls"].([]any); ok {
				for _, value := range calls {
					call, _ := value.(map[string]any)
					args, _ := json.Marshal(call["arguments"])
					m.Blocks = append(m.Blocks, ContentBlock{Kind: "tool_call", ID: strVal(call, "call_id"), Name: strVal(call, "name"), Arguments: args})
				}
			}
			if details, ok := text["reasoning_details"].([]any); ok {
				for _, value := range details {
					detail, _ := value.(map[string]any)
					if t := strVal(detail, "text"); t != "" {
						m.Blocks = append(m.Blocks, ContentBlock{Kind: "reasoning", Text: t})
					}
					if strVal(detail, "signature") != "" || strVal(detail, "data") != "" {
						transfer.Warnings = append(transfer.Warnings, "Signed or opaque reasoning data cannot be applied to a different model; readable reasoning is preserved.")
					}
				}
			}
		} else if tool, ok := value["tool"].(map[string]any); ok {
			m.Role = "tool"
			output, _ := tool["output"].(map[string]any)
			b := ContentBlock{Kind: "tool_result", ID: strVal(tool, "call_id"), Name: strVal(tool, "name")}
			b.IsError, _ = output["is_error"].(bool)
			values, _ := output["values"].([]any)
			for _, value := range values {
				item, _ := value.(map[string]any)
				if text := strVal(item, "text"); text != "" {
					b.Children = append(b.Children, ContentBlock{Kind: "text", Text: text})
				} else if image, ok := item["image"].(map[string]any); ok {
					b.Children = append(b.Children, portableImage(strVal(image, "url"), strVal(image, "mime_type"), ""))
				} else {
					raw, _ := json.Marshal(value)
					b.Children = append(b.Children, ContentBlock{Kind: "context", Text: string(raw)})
				}
			}
			m.Blocks = []ContentBlock{b}
		} else if image, ok := value["image"].(map[string]any); ok {
			m.Role = "user"
			m.Blocks = []ContentBlock{portableImage(strVal(image, "url"), strVal(image, "mime_type"), "")}
		}
		if len(m.Blocks) > 0 {
			transfer.Session.History = append(transfer.Session.History, m)
		}
	}
	if tools, ok := content["tools"].([]any); ok {
		for _, value := range tools {
			tool, _ := value.(map[string]any)
			schema, _ := json.Marshal(tool["input_schema"])
			transfer.Session.Tools = append(transfer.Session.Tools, ToolDefinition{Name: strVal(tool, "name"), Description: strVal(tool, "description"), InputSchema: schema})
		}
	}
	data, err := snapshotDatabase(s.FilePath, map[string]rowFilter{"conversations": {Where: "conversation_id=?", Args: []any{s.ID}}, "__diesel_schema_migrations": {Where: "1"}})
	if err != nil {
		return transfer, err
	}
	transfer.Native["databases/conversations.sqlite"] = data
	// Keep the original context independently readable as well as in the native DB.
	transfer.Native["context.json"] = []byte(raw)
	manifest := nativeManifest{Root: filepath.Dir(s.FilePath), Main: filepath.Base(s.FilePath)}
	if err = captureMedia(&transfer, &manifest); err != nil {
		return transfer, err
	}
	if err = capturePortableMetadata(manifest.Root, s.ID, &transfer); err != nil {
		return transfer, err
	}
	if err = captureMedia(&transfer, &manifest); err != nil {
		return transfer, err
	}
	var created, updated string
	if err = db.QueryRow("SELECT CAST(created_at AS TEXT),CAST(COALESCE(updated_at,created_at) AS TEXT) FROM conversations WHERE conversation_id=?", s.ID).Scan(&created, &updated); err != nil {
		return transfer, err
	}
	start := forgeStoredTime(created)
	end := forgeStoredTime(updated)
	if err = f.captureSnapshots(content, manifest.Root, s.Project, start, end, transfer.Native); err != nil {
		return transfer, err
	}
	if err = addNativeManifest(transfer.Native, manifest); err != nil {
		return transfer, err
	}
	transfer.Warnings = uniqueWarnings(transfer.Warnings)
	return transfer, nil
}

func forgeStoredTime(value string) time.Time {
	for _, layout := range []string{time.RFC3339Nano, "2006-01-02 15:04:05.999999999-07:00", "2006-01-02 15:04:05.999999999"} {
		if stamp, err := time.Parse(layout, value); err == nil {
			return stamp
		}
	}
	return time.Time{}
}

func (f *ForgeProvider) captureSnapshots(value any, root, project string, start, end time.Time, files map[string][]byte) error {
	paths := make(map[string]bool)
	var visit func(any)
	visit = func(value any) {
		switch v := value.(type) {
		case map[string]any:
			for key, item := range v {
				if text, ok := item.(string); ok && (key == "path" || key == "file_path" || key == "absolute_path") {
					if !filepath.IsAbs(text) {
						text = filepath.Join(project, text)
					}
					paths[text] = true
				}
				visit(item)
			}
		case []any:
			for _, item := range v {
				visit(item)
			}
		}
	}
	visit(value)
	for file := range paths {
		hash := fnv.New64a()
		_, _ = hash.Write([]byte(file))
		relative := fmt.Sprintf("snapshots/%x", hash.Sum64())
		entries, err := os.ReadDir(filepath.Join(root, relative))
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return err
		}
		for _, entry := range entries {
			name := strings.TrimSuffix(entry.Name(), ".snap")
			split := strings.LastIndexByte(name, '-')
			if split < 0 {
				continue
			}
			stamp, err := time.Parse("2006-01-02_15-04-05.000000000", name[:split]+"."+name[split+1:])
			if err != nil {
				continue
			}
			if !start.IsZero() && !end.IsZero() && !stamp.Before(start) && !stamp.After(end) {
				if err = captureFile(root, filepath.Join(relative, entry.Name()), files); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func (f *ForgeProvider) ImportSession(transfer SessionTransfer, home string, duplicate bool) (duplicateResult, error) {
	root := os.Getenv("FORGE_CONFIG")
	if root == "" {
		root = filepath.Join(home, ".config", "forgecode")
	}
	if transfer.Session.ProviderName == f.Name() {
		return f.restoreSession(transfer, root, duplicate)
	}
	if err := validatePortable(transfer.Session); err != nil {
		return duplicateResult{}, err
	}
	if err := initializeForgeStore(root); err != nil {
		return duplicateResult{}, err
	}
	s := transfer.Session
	id := uuid.NewString()
	warnings := append([]string(nil), transfer.Warnings...)
	if err := materializePortableFiles(root, id, &s); err != nil {
		return duplicateResult{}, err
	}
	var messages []any
	messages = append(messages, map[string]any{"message": map[string]any{"text": map[string]any{"role": "System", "content": "<current_working_directory>" + s.Project + "</current_working_directory>"}}})
	for _, message := range portableHistory(s) {
		role := message.Role
		if role == "developer" {
			role = "system"
		}
		var textParts []string
		var calls []map[string]any
		flush := func() {
			if len(textParts) == 0 && len(calls) == 0 {
				return
			}
			text := map[string]any{"role": strings.ToUpper(role[:1]) + role[1:], "content": strings.Join(textParts, "\n")}
			if len(calls) > 0 {
				text["tool_calls"] = calls
			}
			messages = append(messages, map[string]any{"message": map[string]any{"text": text}})
			textParts = nil
			calls = nil
		}
		for _, block := range messageBlocks(message) {
			switch block.Kind {
			case "text":
				textParts = append(textParts, block.Text)
			case "tool_call":
				var args any
				if json.Unmarshal(block.Arguments, &args) != nil {
					args = map[string]any{"input": string(block.Arguments)}
				}
				calls = append(calls, map[string]any{"name": block.Name, "call_id": block.ID, "arguments": args})
			case "tool_result":
				flush()
				var values []any
				if block.Text != "" {
					values = append(values, map[string]any{"text": block.Text})
				}
				for _, child := range block.Children {
					if child.Kind == "image" {
						values = append(values, map[string]any{"image": map[string]any{"url": mediaURL(child), "mime_type": child.MediaType}})
					} else {
						values = append(values, map[string]any{"text": blockText(child)})
					}
				}
				messages = append(messages, map[string]any{"message": map[string]any{"tool": map[string]any{"name": block.Name, "call_id": block.ID, "output": map[string]any{"is_error": block.IsError, "values": values}}}})
			case "image":
				flush()
				messages = append(messages, map[string]any{"message": map[string]any{"image": map[string]any{"url": mediaURL(block), "mime_type": block.MediaType}}})
			case "reasoning":
				textParts = append(textParts, "Historical reasoning:\n"+block.Text)
				warnings = append(warnings, "Foreign model reasoning is retained as text; its signature cannot be reused.")
			default:
				textParts = append(textParts, "Imported "+block.Kind+": "+blockText(block))
				warnings = append(warnings, "Unsupported content is retained as labeled context.")
			}
		}
		flush()
	}
	var tools []map[string]any
	for _, tool := range s.Tools {
		var schema any
		_ = json.Unmarshal(tool.InputSchema, &schema)
		tools = append(tools, map[string]any{"name": tool.Name, "description": tool.Description, "input_schema": schema})
	}
	context := map[string]any{"conversation_id": id, "messages": messages}
	if len(tools) > 0 {
		context["tools"] = tools
	}
	raw, err := json.Marshal(context)
	if err != nil {
		return duplicateResult{}, err
	}
	db, err := sql.Open("sqlite", filepath.Join(root, ".forge.db"))
	if err != nil {
		return duplicateResult{}, err
	}
	defer db.Close()
	stamp := time.Now().UTC().Format("2006-01-02 15:04:05.999999999")
	_, err = db.Exec("INSERT INTO conversations (conversation_id,title,workspace_id,context,created_at,updated_at) VALUES (?,?,?,?,?,?)", id, s.DisplayName(), int64(1), string(raw), stamp, stamp)
	result := duplicateResult{ID: id, Path: filepath.Join(root, ".forge.db"), Resume: "forge --conversation-id " + id, Mode: "translated", Warnings: uniqueWarnings(warnings)}
	if err != nil {
		return result, err
	}
	if err = preservePortableMetadata(f, root, result, s, transfer); err != nil {
		return result, err
	}
	return result, nil
}

func initializeForgeStore(root string) error {
	filename := filepath.Join(root, ".forge.db")
	if _, err := os.Stat(filename); err == nil {
		return nil
	} else if !os.IsNotExist(err) {
		return err
	}
	if err := os.MkdirAll(root, 0700); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, "forge", "conversation", "new")
	command.Env = append(os.Environ(), "FORGE_CONFIG="+root)
	if output, err := command.CombinedOutput(); err != nil {
		return fmt.Errorf("initializing Forge store requires the Forge CLI: %w: %s", err, output)
	}
	return nil
}

func (f *ForgeProvider) restoreSession(transfer SessionTransfer, root string, duplicate bool) (duplicateResult, error) {
	m, err := readNativeManifest(transfer)
	if err != nil {
		return duplicateResult{}, err
	}
	oldID, id := transfer.Session.ID, transfer.Session.ID
	if _, err = uuid.Parse(id); err != nil {
		return duplicateResult{}, err
	}
	if duplicate {
		id = uuid.NewString()
	}
	data := transfer.Native["databases/conversations.sqlite"]
	if len(data) == 0 {
		return duplicateResult{}, fmt.Errorf("native conversation database missing; re-export the chat")
	}
	files, replacements, err := prepareNativeMedia(transfer, m, root, id)
	if err != nil {
		return duplicateResult{}, err
	}
	for key, contents := range transfer.Native {
		if !strings.HasPrefix(key, "files/") {
			continue
		}
		rel := strings.TrimPrefix(key, "files/")
		if !strings.HasPrefix(rel, "snapshots/") && rel != ".session-manager/"+oldID+".json" && !strings.HasPrefix(rel, ".session-manager/"+oldID+"/") {
			return duplicateResult{}, fmt.Errorf("unexpected native file %q", rel)
		}
		if rel == ".session-manager/"+oldID+".json" {
			key = "files/.session-manager/" + id + ".json"
			contents = replaceJSONValues(contents, replacements)
		}
		if strings.HasPrefix(rel, ".session-manager/"+oldID+"/") {
			key = "files/.session-manager/" + id + "/" + strings.TrimPrefix(rel, ".session-manager/"+oldID+"/")
		}
		files[key] = contents
	}
	filename, cleanup, err := temporaryDatabase(data)
	if err != nil {
		return duplicateResult{}, err
	}
	defer cleanup()
	db, err := sql.Open("sqlite", filename)
	if err != nil {
		return duplicateResult{}, err
	}
	var raw string
	var count int
	if err = db.QueryRow("SELECT count(*) FROM conversations").Scan(&count); err != nil || count != 1 {
		db.Close()
		return duplicateResult{}, fmt.Errorf("native snapshot must contain exactly one conversation")
	}
	if err = db.QueryRow("SELECT context FROM conversations WHERE conversation_id=?", oldID).Scan(&raw); err != nil {
		db.Close()
		return duplicateResult{}, err
	}
	updated := replaceJSONValues(relocateJSON([]byte(raw), m.Root, root, m.Files), replacements)
	if duplicate {
		var content map[string]json.RawMessage
		if err = json.Unmarshal(updated, &content); err != nil {
			db.Close()
			return duplicateResult{}, err
		}
		content["conversation_id"], _ = json.Marshal(id)
		updated, _ = json.Marshal(content)
	}
	if duplicate || string(updated) != raw {
		if _, err = db.Exec("UPDATE conversations SET conversation_id=?,context=? WHERE conversation_id=?", id, string(updated), oldID); err != nil {
			db.Close()
			return duplicateResult{}, err
		}
	}
	if err = db.Close(); err != nil {
		return duplicateResult{}, err
	}
	data, err = os.ReadFile(filename)
	if err != nil {
		return duplicateResult{}, err
	}
	undo, err := restoreNativeFiles(root, files, nil, func(string) bool { return true }, m.Files)
	if err != nil {
		return duplicateResult{}, err
	}
	if err = os.MkdirAll(root, 0700); err != nil {
		undo()
		return duplicateResult{}, err
	}
	if err = restoreDatabase(filepath.Join(root, ".forge.db"), data, nil, []string{"__diesel_schema_migrations"}); err != nil {
		undo()
		return duplicateResult{}, err
	}
	return duplicateResult{ID: id, Path: filepath.Join(root, ".forge.db"), Resume: "forge --conversation-id " + id, Mode: "native"}, nil
}
