package main

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"
)

func (c *CodexProvider) CaptureSession(s Session) (SessionTransfer, error) {
	root, err := storageRoot(s.FilePath, "sessions")
	if err != nil {
		root, err = storageRoot(s.FilePath, "archived_sessions")
	}
	if err != nil {
		return SessionTransfer{}, err
	}
	rel, err := filepath.Rel(root, s.FilePath)
	if err != nil {
		return SessionTransfer{}, err
	}
	transfer := SessionTransfer{Session: s, Native: make(map[string][]byte)}
	manifest := nativeManifest{Root: root, Main: filepath.ToSlash(rel)}
	if err = c.captureRollout(root, s.FilePath, nil, &transfer, &manifest, make(map[string]bool)); err != nil {
		return transfer, err
	}
	logical, _, err := c.readHistory(s.FilePath, nil, make(map[string]bool))
	if err != nil {
		return transfer, err
	}
	records, err := decodeJSONLines(logical)
	if err != nil {
		return transfer, err
	}
	transfer.Session.History = nil
	for _, record := range records {
		payload, _ := record["payload"].(map[string]any)
		if payload == nil {
			continue
		}
		stamp, _ := time.Parse(time.RFC3339Nano, strVal(record, "timestamp"))
		if strVal(record, "type") == "session_meta" {
			if base, ok := payload["base_instructions"].(map[string]any); ok && strVal(base, "text") != "" {
				transfer.Session.History = append(transfer.Session.History, ChatMessage{Role: "system", Content: strVal(base, "text"), Timestamp: stamp})
			}
			continue
		}
		if strVal(record, "type") != "response_item" {
			continue
		}
		if item, ok := payload["item"].(map[string]any); ok {
			payload = item
		}
		m := ChatMessage{Role: "assistant", Timestamp: stamp, Phase: strVal(payload, "phase")}
		switch strVal(payload, "type") {
		case "message":
			m.Role = strVal(payload, "role")
			blocks, _ := payload["content"].([]any)
			for _, value := range blocks {
				block, _ := value.(map[string]any)
				if block == nil {
					continue
				}
				switch strVal(block, "type") {
				case "input_text", "output_text", "text":
					m.Blocks = append(m.Blocks, ContentBlock{Kind: "text", Text: strVal(block, "text")})
				case "input_image":
					if source := strVal(block, "image_url"); source != "" {
						m.Blocks = append(m.Blocks, portableImage(source, "", ""))
					} else {
						m.Blocks = append(m.Blocks, ContentBlock{Kind: "context", Text: "Remote image file reference: " + strVal(block, "file_id")})
						transfer.Warnings = append(transfer.Warnings, "A remote image file ID cannot be resolved offline in a different harness; its reference is preserved.")
					}
				case "input_audio":
					b := portableImage(strVal(block, "audio_url"), "", "")
					b.Kind = "audio"
					m.Blocks = append(m.Blocks, b)
				default:
					raw, _ := json.Marshal(block)
					m.Blocks = append(m.Blocks, ContentBlock{Kind: "context", Text: string(raw)})
					transfer.Warnings = append(transfer.Warnings, "Unsupported source media or content is retained as labeled JSON context.")
				}
			}
		case "function_call", "custom_tool_call":
			arguments := json.RawMessage(strVal(payload, "arguments"))
			if strVal(payload, "type") == "custom_tool_call" {
				arguments, _ = json.Marshal(map[string]string{"input": strVal(payload, "input")})
			}
			if !json.Valid(arguments) {
				arguments, _ = json.Marshal(map[string]string{"input": strVal(payload, "arguments")})
			}
			m.Blocks = []ContentBlock{{Kind: "tool_call", ID: strVal(payload, "call_id"), Name: strVal(payload, "name"), Arguments: arguments}}
			if strVal(payload, "type") == "custom_tool_call" {
				m.Blocks[0].Format = "text"
				m.Blocks[0].Text = strVal(payload, "input")
			}
		case "function_call_output", "custom_tool_call_output":
			m.Role = "tool"
			b := ContentBlock{Kind: "tool_result", ID: strVal(payload, "call_id")}
			if output, ok := payload["output"].(string); ok {
				b.Text = output
			} else if blocks, ok := payload["output"].([]any); ok {
				for _, value := range blocks {
					block, _ := value.(map[string]any)
					if block == nil {
						continue
					}
					if strVal(block, "type") == "input_image" {
						b.Children = append(b.Children, portableImage(strVal(block, "image_url"), "", ""))
					} else if text := strVal(block, "text"); text != "" {
						b.Children = append(b.Children, ContentBlock{Kind: "text", Text: text})
					} else {
						raw, _ := json.Marshal(block)
						b.Children = append(b.Children, ContentBlock{Kind: "context", Text: string(raw)})
					}
				}
			} else {
				raw, _ := json.Marshal(payload["output"])
				b.Text = string(raw)
			}
			m.Blocks = []ContentBlock{b}
		case "reasoning":
			for _, field := range []string{"summary", "content"} {
				if values, ok := payload[field].([]any); ok {
					for _, value := range values {
						part, _ := value.(map[string]any)
						if text := strVal(part, "text"); text != "" {
							m.Blocks = append(m.Blocks, ContentBlock{Kind: "reasoning", Text: text})
						}
					}
				}
			}
			if strVal(payload, "encrypted_content") != "" {
				transfer.Warnings = append(transfer.Warnings, "Encrypted model reasoning cannot be decrypted or reused by another harness; available reasoning text is preserved.")
			}
		default:
			raw, _ := json.Marshal(payload)
			m.Blocks = []ContentBlock{{Kind: "context", Text: string(raw)}}
			transfer.Warnings = append(transfer.Warnings, "A harness-specific response item is preserved as labeled JSON context.")
		}
		if len(m.Blocks) > 0 {
			transfer.Session.History = append(transfer.Session.History, m)
		}
	}
	if len(transfer.Session.History) == 0 {
		transfer.Session.History = s.Messages
	}
	if err = captureMedia(&transfer, &manifest); err != nil {
		return transfer, err
	}
	if err = capturePortableMetadata(manifest.Root, s.ID, &transfer); err != nil {
		return transfer, err
	}
	if err = captureMedia(&transfer, &manifest); err != nil {
		return transfer, err
	}
	for _, dbName := range []string{"state_5.sqlite", "thread_history_1.sqlite"} {
		filename := filepath.Join(root, dbName)
		if _, err = os.Stat(filename); os.IsNotExist(err) {
			continue
		} else if err != nil {
			return transfer, err
		}
		keep, err := c.databaseFilters(filename, s.ID)
		if err != nil {
			return transfer, err
		}
		data, err := snapshotDatabase(filename, keep)
		if err != nil {
			return transfer, err
		}
		transfer.Native["databases/"+dbName] = data
	}
	if err = captureTree(root, filepath.Join("shell_snapshots", s.ID+".sh"), transfer.Native); err != nil {
		return transfer, err
	}
	if err = addNativeManifest(transfer.Native, manifest); err != nil {
		return transfer, err
	}
	transfer.Warnings = uniqueWarnings(transfer.Warnings)
	return transfer, nil
}

func (c *CodexProvider) captureRollout(root, filename string, end *codexHistoryPosition, transfer *SessionTransfer, manifest *nativeManifest, visited map[string]bool) error {
	if visited[filename] || len(visited) >= 64 {
		return fmt.Errorf("cyclic inherited rollout")
	}
	visited[filename] = true
	defer delete(visited, filename)
	data, err := os.ReadFile(filename)
	if err != nil {
		return err
	}
	if end != nil {
		if end.EndByteOffset > 0 {
			if end.EndByteOffset > int64(len(data)) {
				return fmt.Errorf("invalid inherited byte boundary")
			}
			data = data[:end.EndByteOffset]
		} else {
			var prefix bytes.Buffer
			for i, line := range bytes.Split(data, []byte("\n")) {
				if len(line) == 0 {
					continue
				}
				var ordinal struct {
					Ordinal *uint64 `json:"ordinal"`
				}
				if err = json.Unmarshal(line, &ordinal); err != nil {
					return err
				}
				n := uint64(i)
				if ordinal.Ordinal != nil {
					n = *ordinal.Ordinal
				}
				if n >= end.EndOrdinal {
					break
				}
				prefix.Write(line)
				prefix.WriteByte('\n')
			}
			data = prefix.Bytes()
		}
	}
	rel, err := filepath.Rel(root, filename)
	if err != nil || !validArchivePath(filepath.ToSlash(rel)) {
		return fmt.Errorf("invalid rollout path")
	}
	transfer.Native["files/"+filepath.ToSlash(rel)] = data
	if end != nil {
		manifest.Prefixes = append(manifest.Prefixes, filepath.ToSlash(rel))
	}
	if len(data) == 0 {
		return nil
	}
	first, _, _ := bytes.Cut(data, []byte("\n"))
	var meta struct {
		Payload struct {
			Base *codexHistoryPosition `json:"history_base"`
		} `json:"payload"`
	}
	if err = json.Unmarshal(first, &meta); err != nil {
		return err
	}
	if meta.Payload.Base != nil {
		parent, err := c.historyPath(meta.Payload.Base.ThreadID)
		if err != nil {
			return err
		}
		return c.captureRollout(root, parent, meta.Payload.Base, transfer, manifest, visited)
	}
	return nil
}

func (c *CodexProvider) databaseFilters(filename, id string) (map[string]rowFilter, error) {
	db, err := readOnlyDB(filename)
	if err != nil {
		return nil, err
	}
	defer db.Close()
	tables, err := databaseTables(db)
	if err != nil {
		return nil, err
	}
	keep := make(map[string]rowFilter)
	for _, table := range tables {
		if table == "_sqlx_migrations" {
			keep[table] = rowFilter{Where: "1"}
			continue
		}
		rows, err := db.Query("PRAGMA table_info(" + quoteIdentifier(table) + ")")
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var a, b, f int
			var column, kind string
			var def any
			if err = rows.Scan(&a, &column, &kind, &b, &def, &f); err != nil {
				rows.Close()
				return nil, err
			}
			if column == "thread_id" || column == "session_id" {
				keep[table] = rowFilter{Where: quoteIdentifier(column) + " = ?", Args: []any{id}}
			}
		}
		rows.Close()
		if table == "threads" {
			keep[table] = rowFilter{Where: "id = ?", Args: []any{id}}
		}
		if table == "thread_sections" {
			keep[table] = rowFilter{Where: "id IN (SELECT thread_section_id FROM threads WHERE id=?)", Args: []any{id}}
		}
		if table == "projects" {
			keep[table] = rowFilter{Where: "id IN (SELECT project_id FROM threads WHERE id=?)", Args: []any{id}}
		}
	}
	return keep, nil
}

func (c *CodexProvider) ImportSession(transfer SessionTransfer, home string, duplicate bool) (duplicateResult, error) {
	root := os.Getenv("CODEX_HOME")
	if root == "" {
		root = filepath.Join(home, ".codex")
	}
	if transfer.Session.ProviderName == c.Name() {
		return c.restoreSession(transfer, root, duplicate)
	}
	if err := validatePortable(transfer.Session); err != nil {
		return duplicateResult{}, err
	}
	s := transfer.Session
	now := time.Now().UTC()
	id := uuid.NewString()
	filename := filepath.Join(root, "sessions", now.Format("2006/01/02"), "rollout-"+now.Format("2006-01-02T15-04-05")+"-"+id+".jsonl")
	warnings := append([]string(nil), transfer.Warnings...)
	if err := materializePortableFiles(root, id, &s); err != nil {
		return duplicateResult{}, err
	}
	err := writeImportedJSONL(filename, func(encoder *json.Encoder) error {
		stamp := now.Format(time.RFC3339Nano)
		write := func(kind string, payload any) error {
			return encoder.Encode(map[string]any{"timestamp": stamp, "type": kind, "payload": payload})
		}
		if err := write("session_meta", map[string]any{"id": id, "session_id": id, "timestamp": stamp, "cwd": s.Project, "originator": "agent-session-manager", "cli_version": "0.162.1", "source": "cli", "model_provider": "openai", "history_mode": "legacy"}); err != nil {
			return err
		}
		for _, message := range portableHistory(s) {
			stamp = now.Format(time.RFC3339Nano)
			if !message.Timestamp.IsZero() {
				stamp = message.Timestamp.Format(time.RFC3339Nano)
			}
			role := message.Role
			if role == "system" {
				role = "developer"
			}
			var content []map[string]any
			flush := func() error {
				if len(content) == 0 {
					return nil
				}
				item := map[string]any{"type": "message", "role": role, "content": content}
				if role == "assistant" {
					phase := message.Phase
					if phase == "" {
						phase = "final_answer"
					}
					item["phase"] = phase
				}
				if err := write("response_item", item); err != nil {
					return err
				}
				if role == "user" || role == "assistant" {
					event := "user_message"
					if role == "assistant" {
						event = "agent_message"
					}
					var texts []string
					for _, block := range content {
						if text, ok := block["text"].(string); ok {
							texts = append(texts, text)
						}
					}
					if err := write("event_msg", map[string]any{"type": event, "message": strings.Join(texts, "\n")}); err != nil {
						return err
					}
				}
				content = nil
				return nil
			}
			for _, block := range messageBlocks(message) {
				switch block.Kind {
				case "tool_call":
					if err := flush(); err != nil {
						return err
					}
					item := map[string]any{"type": "function_call", "name": block.Name, "call_id": block.ID, "arguments": string(block.Arguments)}
					if block.Format == "text" {
						item = map[string]any{"type": "custom_tool_call", "name": block.Name, "call_id": block.ID, "input": block.Text}
					}
					if err := write("response_item", item); err != nil {
						return err
					}
				case "tool_result":
					if err := flush(); err != nil {
						return err
					}
					var output any = block.Text
					if block.IsError {
						output = "Historical tool error: " + block.Text
					}
					if len(block.Children) > 0 {
						var parts []map[string]any
						if block.IsError {
							parts = append(parts, map[string]any{"type": "input_text", "text": "Historical tool error"})
						}
						for _, child := range block.Children {
							if child.Kind == "image" {
								parts = append(parts, map[string]any{"type": "input_image", "image_url": mediaURL(child)})
							} else {
								parts = append(parts, map[string]any{"type": "input_text", "text": blockText(child)})
							}
						}
						output = parts
					}
					if err := write("response_item", map[string]any{"type": "function_call_output", "call_id": block.ID, "output": output}); err != nil {
						return err
					}
				case "image":
					content = append(content, map[string]any{"type": "input_image", "image_url": mediaURL(block)})
				case "audio":
					content = append(content, map[string]any{"type": "input_audio", "audio_url": mediaURL(block)})
				default:
					text := blockText(block)
					if block.Kind == "reasoning" {
						text = "Historical reasoning:\n" + text
						warnings = append(warnings, "Reasoning from another harness is retained as text because model-signed reasoning cannot be reused.")
					}
					if block.Kind == "file" || block.Kind == "audio" {
						text = "Imported " + block.Kind + " attachment: " + block.URL
						warnings = append(warnings, "This attachment format is retained as a data URI because the destination history format has no equivalent attachment block.")
					}
					kind := "input_text"
					if role == "assistant" {
						kind = "output_text"
					}
					content = append(content, map[string]any{"type": kind, "text": text})
				}
			}
			if err := flush(); err != nil {
				return err
			}
		}
		return nil
	})
	result := duplicateResult{ID: id, Path: filename, Resume: "codex resume " + id, Mode: "translated", Warnings: uniqueWarnings(warnings)}
	if err != nil {
		return result, err
	}
	if err = preservePortableMetadata(c, root, result, s, transfer); err != nil {
		return result, err
	}
	return result, nil
}

func (c *CodexProvider) restoreSession(transfer SessionTransfer, root string, duplicate bool) (duplicateResult, error) {
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
	main := m.Main
	if duplicate {
		main = strings.ReplaceAll(main, oldID, id)
	}
	files, replacements, err := prepareNativeMedia(transfer, m, root, id)
	if err != nil {
		return duplicateResult{}, err
	}
	for key, data := range transfer.Native {
		if !strings.HasPrefix(key, "files/") {
			continue
		}
		rel := strings.TrimPrefix(key, "files/")
		allowed := (strings.HasPrefix(rel, "sessions/") || strings.HasPrefix(rel, "archived_sessions/")) && strings.HasSuffix(rel, ".jsonl")
		if rel == "shell_snapshots/"+oldID+".sh" {
			allowed = true
		}
		if rel == ".session-manager/"+oldID+".json" || strings.HasPrefix(rel, ".session-manager/"+oldID+"/") {
			allowed = true
		}
		if !allowed {
			return duplicateResult{}, fmt.Errorf("unexpected native file %q", rel)
		}
		if rel == m.Main {
			key = "files/" + main
			if duplicate {
				data, err = rewriteJSONLines(data, func(record map[string]any) {
					if strVal(record, "type") == "session_meta" {
						payload, _ := record["payload"].(map[string]any)
						payload["id"] = id
						payload["session_id"] = id
					}
				})
				if err != nil {
					return duplicateResult{}, err
				}
			}
		}
		if duplicate && (rel == ".session-manager/"+oldID+".json" || strings.HasPrefix(rel, ".session-manager/"+oldID+"/")) {
			key = "files/" + strings.Replace(rel, ".session-manager/"+oldID, ".session-manager/"+id, 1)
		}
		if strings.HasSuffix(key, ".jsonl") && !strings.Contains(key, "/original/") {
			data = relocateJSONL(data, m.Root, root, m.Files)
		}
		if !strings.Contains(key, "/original/") {
			data = replaceJSONLValues(data, replacements)
		}
		if duplicate && rel == "shell_snapshots/"+oldID+".sh" {
			key = "files/shell_snapshots/" + id + ".sh"
		}
		files[key] = data
	}
	if len(files["files/"+main]) == 0 {
		return duplicateResult{}, fmt.Errorf("native rollout is missing")
	}
	fileInfo := make(map[string]nativeFileInfo)
	for relative, info := range m.Files {
		if duplicate {
			switch {
			case relative == m.Main:
				relative = main
			case relative == "shell_snapshots/"+oldID+".sh":
				relative = "shell_snapshots/" + id + ".sh"
			case strings.HasPrefix(relative, ".session-manager/"+oldID):
				relative = strings.Replace(relative, ".session-manager/"+oldID, ".session-manager/"+id, 1)
			}
		}
		fileInfo[relative] = info
	}
	undo, err := restoreNativeFiles(root, files, m.Prefixes, func(string) bool { return true }, fileInfo)
	if err != nil {
		return duplicateResult{}, err
	}
	for _, name := range []string{"state_5.sqlite", "thread_history_1.sqlite"} {
		data, ok := transfer.Native["databases/"+name]
		if !ok {
			continue
		}
		if duplicate {
			data, err = c.cloneDatabase(data, oldID, id, m.Root, root, m.Main, main)
			if err != nil {
				undo()
				return duplicateResult{}, err
			}
		}
		relocations := map[string]map[string]string{}
		if name == "state_5.sqlite" {
			relocations["threads"] = map[string]string{"rollout_path": m.Root + "\x00" + root}
		}
		if err = restoreDatabase(filepath.Join(root, name), data, relocations, []string{"_sqlx_migrations"}); err != nil {
			undo()
			return duplicateResult{}, err
		}
	}
	return duplicateResult{ID: id, Path: filepath.Join(root, filepath.FromSlash(main)), Resume: "codex resume " + id, Mode: "native"}, nil
}

func (c *CodexProvider) cloneDatabase(data []byte, oldID, id, oldRoot, root, oldMain, main string) ([]byte, error) {
	filename, cleanup, err := temporaryDatabase(data)
	if err != nil {
		return nil, err
	}
	defer cleanup()
	db, err := sql.Open("sqlite", filename)
	if err != nil {
		return nil, err
	}
	tables, err := databaseTables(db)
	if err != nil {
		db.Close()
		return nil, err
	}
	for _, table := range tables {
		rows, err := db.Query("PRAGMA table_info(" + quoteIdentifier(table) + ")")
		if err != nil {
			db.Close()
			return nil, err
		}
		var columns []string
		for rows.Next() {
			var a, b, f int
			var column, kind string
			var def any
			if err = rows.Scan(&a, &column, &kind, &b, &def, &f); err != nil {
				rows.Close()
				db.Close()
				return nil, err
			}
			if column == "thread_id" || column == "session_id" || (table == "threads" && column == "id") {
				columns = append(columns, column)
			}
		}
		rows.Close()
		for _, column := range columns {
			if _, err = db.Exec("UPDATE "+quoteIdentifier(table)+" SET "+quoteIdentifier(column)+"=? WHERE "+quoteIdentifier(column)+"=?", id, oldID); err != nil {
				db.Close()
				return nil, err
			}
		}
		if table == "threads" {
			if _, err = db.Exec("UPDATE threads SET rollout_path=? WHERE id=?", filepath.Join(root, filepath.FromSlash(main)), id); err != nil {
				db.Close()
				return nil, err
			}
		}
	}
	if err = db.Close(); err != nil {
		return nil, err
	}
	return os.ReadFile(filename)
}
