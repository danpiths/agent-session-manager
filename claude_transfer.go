package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode"

	"github.com/google/uuid"
)

func (c *ClaudeProvider) CaptureSession(s Session) (SessionTransfer, error) {
	root, err := storageRoot(s.FilePath, "projects")
	if err != nil {
		return SessionTransfer{}, err
	}
	rel, err := filepath.Rel(root, s.FilePath)
	if err != nil {
		return SessionTransfer{}, err
	}
	transfer := SessionTransfer{Session: s, Native: make(map[string][]byte)}
	if err = captureFile(root, rel, transfer.Native); err != nil {
		return transfer, err
	}
	for _, folder := range []string{strings.TrimSuffix(rel, ".jsonl"), filepath.Join("file-history", s.ID), filepath.Join("session-env", s.ID), filepath.Join("tasks", s.ID), filepath.Join("sessions", s.ID+".json")} {
		if err = captureTree(root, folder, transfer.Native); err != nil {
			return transfer, err
		}
	}
	todos, _ := filepath.Glob(filepath.Join(root, "todos", s.ID+"*"))
	for _, filename := range todos {
		relative, _ := filepath.Rel(root, filename)
		if err = captureTree(root, relative, transfer.Native); err != nil {
			return transfer, err
		}
	}
	records, err := decodeJSONLines(transfer.Native["files/"+filepath.ToSlash(rel)])
	if err != nil {
		return transfer, err
	}
	transfer.Session.History = nil
	for _, record := range records {
		message, _ := record["message"].(map[string]any)
		if message == nil {
			if strVal(record, "type") == "system" {
				if content := strVal(record, "content"); content != "" {
					transfer.Session.History = append(transfer.Session.History, ChatMessage{Role: "system", Content: content})
				}
			}
			continue
		}
		role := strVal(message, "role")
		if role == "" {
			role = strVal(record, "type")
		}
		m := ChatMessage{Role: role, Model: strVal(message, "model")}
		m.Timestamp, _ = time.Parse(time.RFC3339Nano, strVal(record, "timestamp"))
		if text, ok := message["content"].(string); ok {
			m.Content = text
			m.Blocks = []ContentBlock{{Kind: "text", Text: text}}
		}
		if blocks, ok := message["content"].([]any); ok {
			for _, value := range blocks {
				block, _ := value.(map[string]any)
				if block == nil {
					continue
				}
				switch strVal(block, "type") {
				case "text":
					m.Blocks = append(m.Blocks, ContentBlock{Kind: "text", Text: strVal(block, "text")})
				case "thinking":
					m.Blocks = append(m.Blocks, ContentBlock{Kind: "reasoning", Text: strVal(block, "thinking")})
					if strVal(block, "signature") != "" {
						transfer.Warnings = append(transfer.Warnings, "Model-signed reasoning cannot retain its signature in another harness; reasoning text is preserved.")
					}
				case "redacted_thinking":
					transfer.Warnings = append(transfer.Warnings, "Opaque redacted reasoning has no cross-harness representation; its original bytes remain in the native snapshot.")
				case "tool_use":
					args, _ := json.Marshal(block["input"])
					m.Blocks = append(m.Blocks, ContentBlock{Kind: "tool_call", ID: strVal(block, "id"), Name: strVal(block, "name"), Arguments: args})
				case "tool_result":
					b := ContentBlock{Kind: "tool_result", ID: strVal(block, "tool_use_id")}
					b.IsError, _ = block["is_error"].(bool)
					if text, ok := block["content"].(string); ok {
						b.Text = text
					} else {
						b.Children = claudeMediaContent(block["content"])
						if len(b.Children) == 0 {
							raw, _ := json.Marshal(block["content"])
							b.Text = string(raw)
						}
					}
					m.Blocks = append(m.Blocks, b)
				case "image", "document":
					m.Blocks = append(m.Blocks, claudeMediaBlock(block))
				default:
					raw, _ := json.Marshal(block)
					m.Blocks = append(m.Blocks, ContentBlock{Kind: "context", Text: string(raw)})
					transfer.Warnings = append(transfer.Warnings, "An unsupported source content block is preserved as labeled JSON context.")
				}
			}
		}
		if len(m.Blocks) > 0 {
			transfer.Session.History = append(transfer.Session.History, m)
		}
	}
	if len(transfer.Session.History) == 0 {
		transfer.Session.History = s.Messages
	}
	manifest := nativeManifest{Root: root, Main: filepath.ToSlash(rel)}
	if err = captureMedia(&transfer, &manifest); err != nil {
		return transfer, err
	}
	if err = capturePortableMetadata(manifest.Root, s.ID, &transfer); err != nil {
		return transfer, err
	}
	if err = captureMedia(&transfer, &manifest); err != nil {
		return transfer, err
	}
	index := filepath.Join(filepath.Dir(s.FilePath), "sessions-index.json")
	if data, readErr := os.ReadFile(index); readErr == nil {
		var content map[string]json.RawMessage
		if err = json.Unmarshal(data, &content); err != nil {
			return transfer, err
		}
		var entries []map[string]any
		if err = json.Unmarshal(content["entries"], &entries); err != nil {
			return transfer, err
		}
		var selected []map[string]any
		for _, entry := range entries {
			if strVal(entry, "sessionId") == s.ID {
				selected = append(selected, entry)
			}
		}
		content["entries"], _ = json.Marshal(selected)
		transfer.Native["index.json"], _ = json.Marshal(content)
	} else if !os.IsNotExist(readErr) {
		return transfer, readErr
	}
	if err = addNativeManifest(transfer.Native, manifest); err != nil {
		return transfer, err
	}
	transfer.Warnings = uniqueWarnings(transfer.Warnings)
	return transfer, nil
}

func claudeMediaBlock(block map[string]any) ContentBlock {
	source, _ := block["source"].(map[string]any)
	b := portableImage(strVal(source, "url"), strVal(source, "media_type"), strVal(source, "data"))
	if strVal(block, "type") == "document" {
		b.Kind = "file"
	}
	return b
}

func claudeMediaContent(value any) []ContentBlock {
	var result []ContentBlock
	blocks, _ := value.([]any)
	for _, value := range blocks {
		block, _ := value.(map[string]any)
		if block == nil {
			continue
		}
		if strVal(block, "type") == "text" {
			result = append(result, ContentBlock{Kind: "text", Text: strVal(block, "text")})
		} else if strVal(block, "type") == "image" || strVal(block, "type") == "document" {
			result = append(result, claudeMediaBlock(block))
		} else {
			raw, _ := json.Marshal(block)
			result = append(result, ContentBlock{Kind: "context", Text: string(raw)})
		}
	}
	return result
}

func claudeProjectDirectory(project string) string {
	return strings.Map(func(r rune) rune {
		if r <= unicode.MaxASCII && (unicode.IsLetter(r) || unicode.IsDigit(r)) {
			return r
		}
		return '-'
	}, project)
}

func (c *ClaudeProvider) ImportSession(transfer SessionTransfer, home string, duplicate bool) (duplicateResult, error) {
	root := os.Getenv("CLAUDE_CONFIG_DIR")
	if root == "" {
		root = filepath.Join(home, ".claude")
	}
	if transfer.Session.ProviderName == c.Name() {
		return c.restoreSession(transfer, root, duplicate)
	}
	if err := validatePortable(transfer.Session); err != nil {
		return duplicateResult{}, err
	}
	s := transfer.Session
	id := uuid.NewString()
	filename := filepath.Join(root, "projects", claudeProjectDirectory(s.Project), id+".jsonl")
	warnings := append([]string(nil), transfer.Warnings...)
	if err := materializePortableFiles(root, id, &s); err != nil {
		return duplicateResult{}, err
	}
	var parent *string
	err := writeImportedJSONL(filename, func(encoder *json.Encoder) error {
		for _, message := range portableHistory(s) {
			role := message.Role
			var blocks []map[string]any
			if role == "system" || role == "developer" {
				role = "user"
				warnings = append(warnings, "Source system/developer instructions are retained as labeled context; the destination harness keeps its own system prompt.")
				blocks = append(blocks, map[string]any{"type": "text", "text": "Imported " + message.Role + " instructions:"})
			}
			if role == "tool" {
				role = "user"
			}
			for _, b := range messageBlocks(message) {
				switch b.Kind {
				case "text":
					blocks = append(blocks, map[string]any{"type": "text", "text": b.Text})
				case "tool_call":
					var input any
					if json.Unmarshal(b.Arguments, &input) != nil {
						input = map[string]any{"input": string(b.Arguments)}
					}
					if _, ok := input.(map[string]any); !ok {
						input = map[string]any{"input": input}
						warnings = append(warnings, "Non-object tool arguments were wrapped in an input field for the destination format.")
					}
					blocks = append(blocks, map[string]any{"type": "tool_use", "id": b.ID, "name": b.Name, "input": input})
				case "tool_result":
					var content any = b.Text
					var media []ContentBlock
					if len(b.Children) > 0 {
						var result []ContentBlock
						for _, child := range b.Children {
							if child.Kind == "image" || child.Kind == "file" {
								media = append(media, child)
							} else {
								result = append(result, child)
							}
						}
						content = claudeEncodeMedia(result, &warnings)
					}
					blocks = append(blocks, map[string]any{"type": "tool_result", "tool_use_id": b.ID, "content": content, "is_error": b.IsError})
					if len(media) > 0 {
						// Claude Code removes nested tool-result images before sending
						// history. Sibling media blocks survive its normalization.
						blocks = append(blocks, map[string]any{"type": "text", "text": "Media returned by historical tool call " + b.ID + ":"})
						blocks = append(blocks, claudeEncodeMedia(media, &warnings)...)
						warnings = append(warnings, "Tool-result media is placed beside its result because the destination CLI removes nested media.")
					}
				case "image", "file":
					blocks = append(blocks, claudeEncodeMedia([]ContentBlock{b}, &warnings)...)
				case "reasoning":
					blocks = append(blocks, map[string]any{"type": "text", "text": "Historical reasoning:\n" + b.Text})
					warnings = append(warnings, "Reasoning from a different harness is preserved as text because it cannot carry a valid model signature.")
				default:
					blocks = append(blocks, map[string]any{"type": "text", "text": "Imported " + b.Kind + ": " + blockText(b)})
					warnings = append(warnings, "Unsupported content is preserved as labeled context.")
				}
			}
			if len(blocks) == 0 {
				continue
			}
			messageID := uuid.NewString()
			stamp := message.Timestamp
			if stamp.IsZero() {
				stamp = time.Now().UTC()
			}
			body := map[string]any{"role": role, "content": blocks}
			if role == "assistant" {
				body["id"] = "msg_" + messageID
				body["type"] = "message"
				body["model"] = "imported"
				body["stop_reason"] = "end_turn"
				body["usage"] = map[string]int{"input_tokens": 0, "output_tokens": 0}
				for _, b := range blocks {
					if b["type"] == "tool_use" {
						body["stop_reason"] = "tool_use"
					}
				}
			}
			if err := encoder.Encode(map[string]any{"type": role, "uuid": messageID, "parentUuid": parent, "sessionId": id, "cwd": s.Project, "timestamp": stamp.Format(time.RFC3339Nano), "isSidechain": false, "userType": "external", "version": "2.1.300", "slug": s.DisplayName(), "message": body}); err != nil {
				return err
			}
			parent = &messageID
		}
		return nil
	})
	result := duplicateResult{ID: id, Path: filename, Resume: "claude --resume " + id, Mode: "translated", Warnings: uniqueWarnings(warnings)}
	if err != nil {
		return result, err
	}
	if err = preservePortableMetadata(c, root, result, s, transfer); err != nil {
		return result, err
	}
	return result, nil
}

func claudeEncodeMedia(blocks []ContentBlock, warnings *[]string) []map[string]any {
	var result []map[string]any
	for _, b := range blocks {
		if b.Kind == "text" {
			result = append(result, map[string]any{"type": "text", "text": b.Text})
			continue
		}
		if b.Kind != "image" && b.Kind != "file" {
			result = append(result, map[string]any{"type": "text", "text": blockText(b)})
			continue
		}
		source := map[string]any{"type": "url", "url": b.URL}
		if b.Data != "" {
			source = map[string]any{"type": "base64", "media_type": b.MediaType, "data": b.Data}
		}
		kind := "image"
		if b.Kind == "file" {
			kind = "document"
		}
		result = append(result, map[string]any{"type": kind, "source": source})
	}
	return result
}

func (c *ClaudeProvider) restoreSession(transfer SessionTransfer, root string, duplicate bool) (duplicateResult, error) {
	s := transfer.Session
	m, err := readNativeManifest(transfer)
	if err != nil {
		return duplicateResult{}, err
	}
	if _, err = uuid.Parse(s.ID); err != nil {
		return duplicateResult{}, err
	}
	oldID, id := s.ID, s.ID
	if duplicate {
		id = uuid.NewString()
	}
	files, replacements, err := prepareNativeMedia(transfer, m, root, id)
	if err != nil {
		return duplicateResult{}, err
	}
	main := strings.ReplaceAll(m.Main, oldID, id)
	projectDir := filepath.ToSlash(filepath.Dir(m.Main))
	for key, data := range transfer.Native {
		if !strings.HasPrefix(key, "files/") {
			continue
		}
		rel := strings.TrimPrefix(key, "files/")
		allowed := rel == m.Main || strings.HasPrefix(rel, strings.TrimSuffix(m.Main, ".jsonl")+"/")
		for _, dir := range []string{"file-history/", "session-env/", "tasks/", "sessions/", "todos/"} {
			if strings.HasPrefix(rel, dir+oldID) {
				allowed = true
			}
		}
		if rel == ".session-manager/"+oldID+".json" || strings.HasPrefix(rel, ".session-manager/"+oldID+"/") {
			allowed = true
		}
		if !allowed {
			return duplicateResult{}, fmt.Errorf("unexpected session-owned file %q", rel)
		}
		if strings.HasPrefix(rel, ".session-manager/"+oldID) {
			key = "files/" + strings.Replace(rel, ".session-manager/"+oldID, ".session-manager/"+id, 1)
		} else {
			key = strings.ReplaceAll(key, oldID, id)
		}
		if strings.HasSuffix(key, ".jsonl") && !strings.Contains(key, "/original/") {
			data = relocateJSONL(data, m.Root, root, m.Files)
			if duplicate {
				data, err = rewriteJSONLines(data, func(record map[string]any) {
					if record["sessionId"] == oldID {
						record["sessionId"] = id
					}
				})
				if err != nil {
					return duplicateResult{}, err
				}
			}
		}
		if !strings.Contains(key, "/original/") {
			data = replaceJSONLValues(data, replacements)
		}
		files[key] = data
	}
	if len(files["files/"+main]) == 0 {
		return duplicateResult{}, fmt.Errorf("native transcript is missing")
	}
	fileInfo := make(map[string]nativeFileInfo)
	for relative, info := range m.Files {
		if duplicate {
			if strings.HasPrefix(relative, ".session-manager/"+oldID) {
				relative = strings.Replace(relative, ".session-manager/"+oldID, ".session-manager/"+id, 1)
			} else {
				relative = strings.ReplaceAll(relative, oldID, id)
			}
		}
		fileInfo[relative] = info
	}
	undo, err := restoreNativeFiles(root, files, nil, func(string) bool { return true }, fileInfo)
	if err != nil {
		return duplicateResult{}, err
	}
	if index, ok := transfer.Native["index.json"]; ok {
		if duplicate {
			index = replaceJSONValues(index, map[string]string{oldID: id, filepath.Join(m.Root, filepath.FromSlash(m.Main)): filepath.Join(root, filepath.FromSlash(main))})
		}
		if err = mergeClaudeIndex(filepath.Join(root, filepath.FromSlash(projectDir), "sessions-index.json"), relocateJSON(index, m.Root, root, m.Files)); err != nil {
			undo()
			return duplicateResult{}, err
		}
	}
	return duplicateResult{ID: id, Path: filepath.Join(root, filepath.FromSlash(main)), Resume: "claude --resume " + id, Mode: "native"}, nil
}

func mergeClaudeIndex(filename string, incoming []byte) error {
	var source, dest map[string]json.RawMessage
	if err := json.Unmarshal(incoming, &source); err != nil {
		return err
	}
	var a, b []map[string]any
	if err := json.Unmarshal(source["entries"], &a); err != nil {
		return err
	}
	data, err := os.ReadFile(filename)
	if os.IsNotExist(err) {
		return os.WriteFile(filename, incoming, 0600)
	}
	if err != nil {
		return err
	}
	if err = json.Unmarshal(data, &dest); err != nil {
		return err
	}
	if err = json.Unmarshal(dest["entries"], &b); err != nil {
		return err
	}
	for _, entry := range a {
		found := false
		for _, existing := range b {
			if strVal(entry, "sessionId") == strVal(existing, "sessionId") {
				x, _ := json.Marshal(entry)
				y, _ := json.Marshal(existing)
				if string(x) != string(y) {
					return fmt.Errorf("session index conflicts with existing chat")
				}
				found = true
				break
			}
		}
		if !found {
			b = append(b, entry)
		}
	}
	dest["entries"], _ = json.Marshal(b)
	data, err = json.Marshal(dest)
	if err != nil {
		return err
	}
	return os.WriteFile(filename, data, 0600)
}
