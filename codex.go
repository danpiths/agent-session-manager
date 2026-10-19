package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"github.com/google/uuid"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// CodexProvider reads sessions from ~/.codex/sessions/.
type CodexProvider struct{}

func (c *CodexProvider) Name() string        { return "Codex" }
func (c *CodexProvider) Description() string { return "~/.codex/sessions/" }
func (c *CodexProvider) CanRename() bool     { return false }

func (c *CodexProvider) RenameSession(_ Session, _ string) error {
	return fmt.Errorf("codex sessions cannot be renamed (no slug field)")
}

func (c *CodexProvider) LoadProjects() ([]Project, error) {
	sessDir := filepath.Join(codexDir(), "sessions")
	if _, err := os.Stat(sessDir); os.IsNotExist(err) {
		return nil, nil
	}

	projectMap := make(map[string]*Project)

	err := filepath.Walk(sessDir, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() || !strings.HasSuffix(path, ".jsonl") {
			return nil
		}
		session, parseErr := c.parseSession(path)
		if parseErr != nil {
			return nil
		}
		proj, ok := projectMap[session.ProjectID]
		if !ok {
			proj = &Project{
				ID:   session.ProjectID,
				Path: session.Project,
			}
			projectMap[session.ProjectID] = proj
		}
		proj.Sessions = append(proj.Sessions, session)
		return nil
	})
	if err != nil {
		return nil, err
	}

	var projects []Project
	for _, proj := range projectMap {
		sort.Slice(proj.Sessions, func(i, j int) bool {
			return proj.Sessions[i].Timestamp.After(proj.Sessions[j].Timestamp)
		})
		projects = append(projects, *proj)
	}

	sortProjects(projects)
	return projects, nil
}

func (c *CodexProvider) DeleteSession(session Session) error {
	return os.Remove(session.FilePath)
}

// ── Internal ────────────────────────────────────────────────────────

func codexDir() string {
	if root := os.Getenv("CODEX_HOME"); root != "" {
		return root
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".codex")
}

type codexJSONLLine struct {
	Type    string         `json:"type"`
	Payload map[string]any `json:"payload"`
}

func (c *CodexProvider) parseSession(path string) (Session, error) {
	data, _, err := c.readHistory(path, nil, make(map[string]bool))
	if err != nil {
		return Session{}, err
	}

	scanner := bufio.NewScanner(bytes.NewReader(data))
	scanner.Buffer(make([]byte, 0, 256*1024), 128*1024*1024)

	session := Session{
		FilePath:     path,
		ProviderName: c.Name(),
	}

	var lastTimestamp time.Time
	var eventMessages []ChatMessage

	for scanner.Scan() {
		var line codexJSONLLine
		if err := json.Unmarshal(scanner.Bytes(), &line); err != nil {
			continue
		}
		var envelope struct {
			Timestamp time.Time `json:"timestamp"`
		}
		_ = json.Unmarshal(scanner.Bytes(), &envelope)
		if envelope.Timestamp.After(lastTimestamp) {
			lastTimestamp = envelope.Timestamp
		}

		switch line.Type {
		case "session_meta":
			session.ID = strVal(line.Payload, "id")
			cwd := strVal(line.Payload, "cwd")
			session.Project = cwd
			session.ProjectID = cwd
			if ts, err := time.Parse(time.RFC3339Nano, strVal(line.Payload, "timestamp")); err == nil {
				lastTimestamp = ts
			}

		case "response_item":
			item, _ := line.Payload["item"].(map[string]any)
			if item == nil {
				item = line.Payload
			}
			itype, _ := item["type"].(string)
			if itype != "message" {
				continue
			}
			role, _ := item["role"].(string)
			if role == "developer" || (role != "user" && role != "assistant") {
				continue
			}

			content := extractCodexContent(item)
			if content == "" {
				continue
			}

			if session.FirstMessage == "" && role == "user" {
				session.FirstMessage = content
			}
			session.Messages = append(session.Messages, ChatMessage{
				Role:      role,
				Content:   content,
				Timestamp: envelope.Timestamp,
			})

		case "event_msg":
			// Structured response items are authoritative. Older rollouts may
			// contain only events, which are used below as a fallback.
			kind := strVal(line.Payload, "type")
			if kind == "user_message" || kind == "agent_message" {
				role := "user"
				if kind == "agent_message" {
					role = "assistant"
				}
				if content := strVal(line.Payload, "message"); content != "" {
					eventMessages = append(eventMessages, ChatMessage{Role: role, Content: content, Timestamp: envelope.Timestamp})
				}
			}
			if msg, ok := line.Payload["message"].(map[string]any); ok {
				role, _ := msg["role"].(string)
				if role == "user" {
					content, _ := msg["content"].(string)
					if content != "" && session.FirstMessage == "" {
						session.FirstMessage = content
					}
					if content != "" {
						eventMessages = append(eventMessages, ChatMessage{
							Role:      "user",
							Content:   content,
							Timestamp: envelope.Timestamp,
						})
					}
				}
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return Session{}, fmt.Errorf("reading conversation: %w", err)
	}
	if len(session.Messages) == 0 {
		session.Messages = eventMessages
	}
	if session.FirstMessage == "" {
		for _, msg := range session.Messages {
			if msg.Role == "user" {
				session.FirstMessage = msg.Content
				break
			}
		}
	}

	if session.ID == "" || len(session.Messages) == 0 {
		return Session{}, fmt.Errorf("no usable content")
	}
	if !lastTimestamp.IsZero() {
		session.Timestamp = lastTimestamp
	}
	return session, nil
}

func extractCodexContent(item map[string]any) string {
	contentList, ok := item["content"].([]any)
	if !ok {
		return ""
	}
	var texts []string
	for _, c := range contentList {
		cm, ok := c.(map[string]any)
		if !ok {
			continue
		}
		ct, _ := cm["type"].(string)
		if ct == "input_text" || ct == "output_text" || ct == "text" {
			if text, ok := cm["text"].(string); ok && text != "" {
				texts = append(texts, text)
			}
		}
	}
	return strings.Join(texts, "\n")
}

func strVal(m map[string]any, key string) string {
	v, _ := m[key].(string)
	return v
}

func (c *CodexProvider) ID() string      { return "codex" }
func (c *CodexProvider) CanImport() bool { return true }

type codexHistoryPosition struct {
	ThreadID      string `json:"thread_id"`
	EndOrdinal    uint64 `json:"end_ordinal_exclusive"`
	EndByteOffset int64  `json:"end_byte_offset"`
}

// Paginated forks inherit only a prefix of the parent's history. Resolve that
// prefix before loading or exporting, without including later parent turns.
func (c *CodexProvider) readHistory(path string, end *codexHistoryPosition, visited map[string]bool) ([]byte, bool, error) {
	if visited[path] || len(visited) >= 64 {
		return nil, false, fmt.Errorf("cyclic or excessively deep inherited history")
	}
	visited[path] = true
	defer delete(visited, path)
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, false, err
	}
	if end != nil {
		if end.EndByteOffset > 0 {
			if end.EndByteOffset > int64(len(data)) {
				return nil, false, fmt.Errorf("inherited history is shorter than its saved boundary")
			}
			data = data[:end.EndByteOffset]
		} else {
			var prefix bytes.Buffer
			for i, line := range bytes.Split(data, []byte("\n")) {
				if len(line) == 0 {
					continue
				}
				var record struct {
					Ordinal *uint64 `json:"ordinal"`
				}
				if err := json.Unmarshal(line, &record); err != nil {
					return nil, false, err
				}
				ordinal := uint64(i)
				if record.Ordinal != nil {
					ordinal = *record.Ordinal
				}
				if ordinal >= end.EndOrdinal {
					break
				}
				prefix.Write(line)
				prefix.WriteByte('\n')
			}
			data = prefix.Bytes()
		}
	}
	if len(data) == 0 {
		return nil, false, nil
	}
	first, body, _ := bytes.Cut(data, []byte("\n"))
	var metadata struct {
		Payload struct {
			HistoryBase *codexHistoryPosition `json:"history_base"`
		} `json:"payload"`
	}
	if err := json.Unmarshal(first, &metadata); err != nil {
		return nil, false, err
	}
	base := metadata.Payload.HistoryBase
	if base == nil {
		return data, false, nil
	}
	if _, err := uuid.Parse(base.ThreadID); err != nil {
		return nil, false, fmt.Errorf("invalid inherited thread ID: %w", err)
	}
	parent, err := c.historyPath(base.ThreadID)
	if err != nil {
		return nil, false, err
	}
	inherited, _, err := c.readHistory(parent, base, visited)
	if err != nil {
		return nil, false, err
	}
	_, inheritedBody, _ := bytes.Cut(inherited, []byte("\n"))
	// The additional resolved backup must stand alone, without a reference to a
	// parent file outside the archive. The original leaf is also preserved.
	var envelope map[string]json.RawMessage
	var payload map[string]json.RawMessage
	if err := json.Unmarshal(first, &envelope); err != nil {
		return nil, false, err
	}
	if err := json.Unmarshal(envelope["payload"], &payload); err != nil {
		return nil, false, err
	}
	delete(payload, "history_base")
	envelope["payload"], err = json.Marshal(payload)
	if err != nil {
		return nil, false, err
	}
	first, err = json.Marshal(envelope)
	if err != nil {
		return nil, false, err
	}
	var resolved bytes.Buffer
	resolved.Write(first)
	resolved.WriteByte('\n')
	resolved.Write(inheritedBody)
	if len(inheritedBody) > 0 && inheritedBody[len(inheritedBody)-1] != '\n' {
		resolved.WriteByte('\n')
	}
	resolved.Write(body)
	return resolved.Bytes(), true, nil
}

func (c *CodexProvider) historyPath(id string) (string, error) {
	for _, root := range []string{filepath.Join(codexDir(), "sessions"), filepath.Join(codexDir(), "archived_sessions")} {
		var found string
		err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
			if os.IsNotExist(err) {
				return nil
			}
			if err != nil {
				return err
			}
			if !entry.IsDir() && (entry.Name() == id+".jsonl" || strings.HasSuffix(entry.Name(), "-"+id+".jsonl")) {
				found = path
				return filepath.SkipAll
			}
			return nil
		})
		if err != nil {
			return "", err
		}
		if found != "" {
			return found, nil
		}
	}
	return "", fmt.Errorf("inherited Codex history %s is unavailable", id)
}
