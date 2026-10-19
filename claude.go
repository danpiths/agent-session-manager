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

// ClaudeProvider reads sessions from ~/.claude/projects/.
type ClaudeProvider struct{}

func (c *ClaudeProvider) Name() string        { return "Claude Code" }
func (c *ClaudeProvider) Description() string { return "~/.claude/projects/" }
func (c *ClaudeProvider) CanRename() bool     { return true }

func (c *ClaudeProvider) LoadProjects() ([]Project, error) {
	dir := filepath.Join(claudeDir(), "projects")
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading projects dir: %w", err)
	}

	var projects []Project
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		projPath := filepath.Join(dir, entry.Name())
		sessions, err := c.loadSessions(projPath, entry.Name())
		if err != nil || len(sessions) == 0 {
			continue
		}

		sort.Slice(sessions, func(i, j int) bool {
			return sessions[i].Timestamp.After(sessions[j].Timestamp)
		})

		projects = append(projects, Project{
			ID:       entry.Name(),
			Path:     sessions[0].Project,
			Sessions: sessions,
		})
	}

	sortProjects(projects)
	return projects, nil
}

func (c *ClaudeProvider) DeleteSession(session Session) error {
	if err := os.Remove(session.FilePath); err != nil {
		return fmt.Errorf("removing session file: %w", err)
	}
	dirPath := strings.TrimSuffix(session.FilePath, ".jsonl")
	if info, err := os.Stat(dirPath); err == nil && info.IsDir() {
		if err := os.RemoveAll(dirPath); err != nil {
			return fmt.Errorf("removing session dir: %w", err)
		}
	}
	return nil
}

func (c *ClaudeProvider) RenameSession(session Session, newName string) error {
	f, err := os.Open(session.FilePath)
	if err != nil {
		return err
	}

	var lines []json.RawMessage
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 256*1024), 128*1024*1024)
	for scanner.Scan() {
		raw := make(json.RawMessage, len(scanner.Bytes()))
		copy(raw, scanner.Bytes())
		lines = append(lines, raw)
	}
	if err := scanner.Err(); err != nil {
		f.Close()
		return fmt.Errorf("reading conversation: %w", err)
	}
	f.Close()

	out, err := os.Create(session.FilePath)
	if err != nil {
		return err
	}
	defer out.Close()

	writer := bufio.NewWriter(out)
	for _, raw := range lines {
		var obj map[string]any
		if err := json.Unmarshal(raw, &obj); err != nil {
			writer.Write(raw)
			writer.WriteByte('\n')
			continue
		}
		if _, hasSlug := obj["slug"]; hasSlug {
			obj["slug"] = newName
			if rewritten, err := json.Marshal(obj); err == nil {
				writer.Write(rewritten)
			} else {
				writer.Write(raw)
			}
		} else {
			writer.Write(raw)
		}
		writer.WriteByte('\n')
	}
	return writer.Flush()
}

// ── Internal ────────────────────────────────────────────────────────

func claudeDir() string {
	if root := os.Getenv("CLAUDE_CONFIG_DIR"); root != "" {
		return root
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".claude")
}

func claudeProjectIDToPath(id string) string {
	return strings.ReplaceAll(id, "-", "/")
}

func (c *ClaudeProvider) loadSessions(projDir, projectID string) ([]Session, error) {
	entries, err := os.ReadDir(projDir)
	if err != nil {
		return nil, err
	}

	var sessions []Session
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".jsonl") {
			continue
		}
		sessionID := strings.TrimSuffix(entry.Name(), ".jsonl")
		filePath := filepath.Join(projDir, entry.Name())

		session, err := c.parseSession(filePath, sessionID, projectID)
		if err != nil {
			continue
		}
		sessions = append(sessions, session)
	}
	return sessions, nil
}

type claudeJSONLLine struct {
	CWD       string    `json:"cwd,omitempty"`
	Type      string    `json:"type"`
	Message   *msgField `json:"message,omitempty"`
	Slug      string    `json:"slug,omitempty"`
	Timestamp string    `json:"timestamp,omitempty"`
}

type msgField struct {
	Role    string `json:"role"`
	Content any    `json:"content"`
}

func (c *ClaudeProvider) parseSession(path, sessionID, projectID string) (Session, error) {
	f, err := os.Open(path)
	if err != nil {
		return Session{}, err
	}
	defer f.Close()

	session := Session{
		ID:           sessionID,
		ProjectID:    projectID,
		FilePath:     path,
		ProviderName: c.Name(),
	}

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 256*1024), 128*1024*1024)

	var lastTimestamp time.Time

	for scanner.Scan() {
		var line claudeJSONLLine
		if err := json.Unmarshal(scanner.Bytes(), &line); err != nil {
			continue
		}

		if line.Slug != "" && session.Slug == "" {
			session.Slug = line.Slug
		}
		if line.CWD != "" {
			session.Project = line.CWD
		}

		if line.Type != "user" && line.Type != "assistant" {
			continue
		}
		if line.Message == nil {
			continue
		}

		ts, _ := time.Parse(time.RFC3339Nano, line.Timestamp)
		if ts.After(lastTimestamp) {
			lastTimestamp = ts
		}

		content := extractContent(line.Message.Content)
		if content == "" {
			continue
		}

		if session.FirstMessage == "" && line.Message.Role == "user" {
			session.FirstMessage = content
		}

		session.Messages = append(session.Messages, ChatMessage{
			Role:      line.Message.Role,
			Content:   content,
			Timestamp: ts,
		})
	}
	if err := scanner.Err(); err != nil {
		return Session{}, fmt.Errorf("reading conversation: %w", err)
	}

	if len(session.Messages) == 0 {
		return Session{}, fmt.Errorf("no messages")
	}

	session.Timestamp = lastTimestamp
	if session.Project == "" {
		session.Project = claudeProjectIDToPath(projectID)
	}
	return session, nil
}

func extractContent(raw any) string {
	switch v := raw.(type) {
	case string:
		return v
	case []any:
		var texts []string
		for _, item := range v {
			m, ok := item.(map[string]any)
			if !ok {
				continue
			}
			if m["type"] == "text" {
				if text, ok := m["text"].(string); ok {
					texts = append(texts, text)
				}
			}
		}
		return strings.Join(texts, "\n")
	}
	return ""
}

func (c *ClaudeProvider) ID() string      { return "claude" }
func (c *ClaudeProvider) CanImport() bool { return true }
