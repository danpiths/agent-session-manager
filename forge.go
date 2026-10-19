package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

var forgeCurrentWorkingDirRE = regexp.MustCompile(`(?s)<current_working_directory>(.*?)</current_working_directory>`)

// ForgeProvider reads sessions from ~/.config/forgecode/.forge.db.
type ForgeProvider struct{}

func (f *ForgeProvider) Name() string        { return "Forge Code" }
func (f *ForgeProvider) Description() string { return "~/.config/forgecode/.forge.db" }
func (f *ForgeProvider) CanRename() bool     { return true }

func (f *ForgeProvider) LoadProjects() ([]Project, error) {
	if _, err := os.Stat(forgeDBPath()); os.IsNotExist(err) {
		return nil, nil
	}
	db, err := sql.Open("sqlite", forgeDBPath())
	if err != nil {
		return nil, fmt.Errorf("opening forge database: %w", err)
	}
	defer db.Close()

	rows, err := db.Query(`
		SELECT conversation_id, title, workspace_id, context, created_at, updated_at
		FROM conversations
		ORDER BY datetime(COALESCE(updated_at, created_at)) DESC
	`)
	if err != nil {
		return nil, fmt.Errorf("querying forge conversations: %w", err)
	}
	defer rows.Close()

	projectMap := make(map[string]*Project)
	for rows.Next() {
		var (
			conversationID string
			title          sql.NullString
			workspaceID    int64
			contextJSON    string
			createdAt      time.Time
			updatedAt      sql.NullTime
		)
		if err := rows.Scan(&conversationID, &title, &workspaceID, &contextJSON, &createdAt, &updatedAt); err != nil {
			continue
		}

		session, err := f.parseConversation(conversationID, workspaceID, title, contextJSON, createdAt, updatedAt)
		if err != nil {
			continue
		}

		projectID := session.ProjectID
		proj, ok := projectMap[projectID]
		if !ok {
			proj = &Project{ID: projectID, Path: session.Project}
			projectMap[projectID] = proj
		}
		proj.Sessions = append(proj.Sessions, session)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating forge conversations: %w", err)
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

func (f *ForgeProvider) DeleteSession(session Session) error {
	db, err := sql.Open("sqlite", forgeDBPath())
	if err != nil {
		return fmt.Errorf("opening forge database: %w", err)
	}
	defer db.Close()

	result, err := db.Exec(`DELETE FROM conversations WHERE conversation_id = ?`, session.ID)
	if err != nil {
		return fmt.Errorf("deleting forge conversation: %w", err)
	}
	rowsAffected, err := result.RowsAffected()
	if err == nil && rowsAffected == 0 {
		return fmt.Errorf("forge conversation not found: %s", session.ID)
	}
	return nil
}

func (f *ForgeProvider) RenameSession(session Session, newName string) error {
	trimmed := strings.TrimSpace(newName)
	if trimmed == "" {
		return fmt.Errorf("session name cannot be empty")
	}

	db, err := sql.Open("sqlite", forgeDBPath())
	if err != nil {
		return fmt.Errorf("opening forge database: %w", err)
	}
	defer db.Close()

	result, err := db.Exec(`
		UPDATE conversations
		SET title = ?, updated_at = CURRENT_TIMESTAMP
		WHERE conversation_id = ?
	`, trimmed, session.ID)
	if err != nil {
		return fmt.Errorf("renaming forge conversation: %w", err)
	}
	rowsAffected, err := result.RowsAffected()
	if err == nil && rowsAffected == 0 {
		return fmt.Errorf("forge conversation not found: %s", session.ID)
	}
	return nil
}

type forgeConversationContext struct {
	ConversationID string                `json:"conversation_id"`
	Messages       []forgeContextMessage `json:"messages"`
}

type forgeContextMessage struct {
	Message *forgeEnvelope `json:"message"`
}

type forgeEnvelope struct {
	Text *forgeText `json:"text"`
}

type forgeText struct {
	Role    string `json:"role"`
	Content string `json:"content"`
	Model   string `json:"model"`
}

func (f *ForgeProvider) parseConversation(conversationID string, workspaceID int64, title sql.NullString, contextJSON string, createdAt time.Time, updatedAt sql.NullTime) (Session, error) {
	var ctx forgeConversationContext
	if err := json.Unmarshal([]byte(contextJSON), &ctx); err != nil {
		return Session{}, fmt.Errorf("parsing forge context: %w", err)
	}

	timestamp := createdAt
	if updatedAt.Valid {
		timestamp = updatedAt.Time
	}

	session := Session{
		ID:           conversationID,
		Slug:         strings.TrimSpace(title.String),
		Timestamp:    timestamp,
		FilePath:     forgeDBPath(),
		ProviderName: f.Name(),
	}

	projectPath := forgeProjectPath(ctx.Messages)
	if projectPath == "" {
		projectPath = fmt.Sprintf("workspace:%d", workspaceID)
	}
	session.Project = projectPath
	session.ProjectID = fmt.Sprintf("%d:%s", workspaceID, projectPath)

	for _, msg := range ctx.Messages {
		role, content := forgeMessageText(msg)
		if content == "" {
			continue
		}

		normalizedRole := forgeNormalizeRole(role)
		if normalizedRole == "" {
			continue
		}
		if normalizedRole == "user" && session.FirstMessage == "" {
			session.FirstMessage = truncateStr(content, 500)
		}
		session.Messages = append(session.Messages, ChatMessage{
			Role:      normalizedRole,
			Content:   content,
			Timestamp: timestamp,
		})
	}

	if len(session.Messages) == 0 {
		return Session{}, fmt.Errorf("no usable forge messages")
	}

	return session, nil
}

func forgeDBPath() string {
	if root := os.Getenv("FORGE_CONFIG"); root != "" {
		return filepath.Join(root, ".forge.db")
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".config", "forgecode", ".forge.db")
}

func forgeMessageText(msg forgeContextMessage) (string, string) {
	if msg.Message == nil || msg.Message.Text == nil {
		return "", ""
	}
	role := strings.TrimSpace(msg.Message.Text.Role)
	content := msg.Message.Text.Content
	return role, content
}

func forgeNormalizeRole(role string) string {
	switch strings.ToLower(strings.TrimSpace(role)) {
	case "user":
		return "user"
	case "assistant":
		return "assistant"
	default:
		return ""
	}
}

func forgeProjectPath(messages []forgeContextMessage) string {
	// Use the initial working directory where the conversation was started,
	// not the last directory navigated to via cd commands.
	for _, msg := range messages {
		_, content := forgeMessageText(msg)
		if content == "" {
			continue
		}

		if match := forgeCurrentWorkingDirRE.FindStringSubmatch(content); len(match) == 2 {
			return strings.TrimSpace(match[1])
		}
	}
	return ""
}

func (f *ForgeProvider) ID() string      { return "forge" }
func (f *ForgeProvider) CanImport() bool { return true }
