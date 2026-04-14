package main

import (
	"bufio"
	"encoding/json"
	"fmt"
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
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".codex")
}

type codexJSONLLine struct {
	Type    string         `json:"type"`
	Payload map[string]any `json:"payload"`
}

func (c *CodexProvider) parseSession(path string) (Session, error) {
	f, err := os.Open(path)
	if err != nil {
		return Session{}, err
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 256*1024), 2*1024*1024)

	session := Session{
		FilePath:     path,
		ProviderName: c.Name(),
	}

	var lastTimestamp time.Time
	messageCount := 0
	const maxMessages = 50

	for scanner.Scan() {
		var line codexJSONLLine
		if err := json.Unmarshal(scanner.Bytes(), &line); err != nil {
			continue
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
			if messageCount < maxMessages {
				session.Messages = append(session.Messages, ChatMessage{
					Role:    role,
					Content: content,
				})
			}
			messageCount++

		case "event_msg":
			if msg, ok := line.Payload["message"].(map[string]any); ok {
				role, _ := msg["role"].(string)
				if role == "user" {
					content, _ := msg["content"].(string)
					if content != "" && session.FirstMessage == "" {
						session.FirstMessage = truncateStr(content, 500)
					}
					if content != "" && messageCount < maxMessages {
						session.Messages = append(session.Messages, ChatMessage{
							Role:    "user",
							Content: truncateStr(content, 500),
						})
						messageCount++
					}
				}
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
	for _, c := range contentList {
		cm, ok := c.(map[string]any)
		if !ok {
			continue
		}
		ct, _ := cm["type"].(string)
		if ct == "input_text" || ct == "output_text" || ct == "text" {
			if text, ok := cm["text"].(string); ok && text != "" {
				return truncateStr(text, 500)
			}
		}
	}
	return ""
}

func strVal(m map[string]any, key string) string {
	v, _ := m[key].(string)
	return v
}
