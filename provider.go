package main

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"
)

// SessionProvider is the interface that each coding agent provider must implement.
// To add a new provider, create a new file (e.g. cursor.go), implement this
// interface, and register it in AllProviders() below.
type SessionProvider interface {
	// ID is the short name accepted by command-line provider selection.
	ID() string
	// Name returns the display name shown in the picker and title bar.
	Name() string

	// Description returns a short hint shown next to the name in the picker
	// (typically the session storage path).
	Description() string

	// LoadProjects discovers all projects and their sessions from disk.
	// Sessions within each project must be sorted newest-first.
	LoadProjects() ([]Project, error)

	// DeleteSession removes a session's files from disk.
	DeleteSession(session Session) error

	// RenameSession changes the display name of a session.
	// Return an error if the provider doesn't support renaming.
	RenameSession(session Session, newName string) error

	// CanRename reports whether this provider supports session renaming.
	CanRename() bool

	// CaptureSession snapshots native state and builds portable structured history.
	CaptureSession(session Session) (SessionTransfer, error)
	CanImport() bool
	// ImportSession restores its own native format or translates another provider.
	ImportSession(transfer SessionTransfer, home string, duplicate bool) (duplicateResult, error)
}

func findProvider(name string) (SessionProvider, error) {
	for _, p := range AllProviders() {
		if strings.EqualFold(name, p.ID()) || strings.EqualFold(name, p.Name()) || strings.EqualFold(name, strings.ReplaceAll(p.Name(), " ", "-")) {
			return p, nil
		}
	}
	return nil, fmt.Errorf("unknown provider %q", name)
}

func importProviders() []SessionProvider {
	var providers []SessionProvider
	for _, p := range AllProviders() {
		if p.CanImport() {
			providers = append(providers, p)
		}
	}
	return providers
}

// AllProviders returns every registered provider in display order.
// To add a new provider, append it here.
func AllProviders() []SessionProvider {
	return []SessionProvider{
		&ClaudeProvider{},
		&CodexProvider{},
		&ForgeProvider{},
	}
}

// ── Shared types ────────────────────────────────────────────────────

type Session struct {
	ID           string
	Slug         string
	Project      string
	ProjectID    string
	Timestamp    time.Time
	Messages     []ChatMessage
	FilePath     string
	FirstMessage string           // fallback display name when slug is empty
	ProviderName string           // set by the loader, used for display
	History      []ChatMessage    `json:",omitempty"` // full portable history, including tools and instructions
	Tools        []ToolDefinition `json:",omitempty"`
}

type ToolDefinition struct {
	Name        string
	Description string
	InputSchema json.RawMessage
}

type ChatMessage struct {
	Role      string
	Content   string
	Timestamp time.Time
	Blocks    []ContentBlock `json:",omitempty"`
	Model     string         `json:",omitempty"`
	Phase     string         `json:",omitempty"`
}

// ContentBlock represents transferable content without a harness's wire format.
type ContentBlock struct {
	Kind      string
	Format    string          `json:",omitempty"`
	Text      string          `json:",omitempty"`
	ID        string          `json:",omitempty"`
	Name      string          `json:",omitempty"`
	Arguments json.RawMessage `json:",omitempty"`
	Data      string          `json:",omitempty"`
	MediaType string          `json:",omitempty"`
	URL       string          `json:",omitempty"`
	IsError   bool            `json:",omitempty"`
	Children  []ContentBlock  `json:",omitempty"`
}

type SessionTransfer struct {
	Session  Session
	Native   map[string][]byte
	Warnings []string
}

type Project struct {
	ID       string
	Path     string
	Sessions []Session
}

// DisplayName returns the best human-readable name for a session.
func (s Session) DisplayName() string {
	if s.Slug != "" {
		return s.Slug
	}
	if s.FirstMessage != "" {
		name := strings.Join(strings.Fields(s.FirstMessage), " ")
		if len([]rune(name)) > 50 {
			name = string([]rune(name)[:47]) + "..."
		}
		return name
	}
	if len(s.ID) >= 8 {
		return s.ID[:8]
	}
	return s.ID
}

// ── Shared helpers ──────────────────────────────────────────────────

func sortProjects(projects []Project) {
	sort.SliceStable(projects, func(i, j int) bool {
		ti := projects[i].Sessions[0].Timestamp
		tj := projects[j].Sessions[0].Timestamp
		if !ti.Equal(tj) {
			return ti.After(tj)
		}
		return projects[i].Path < projects[j].Path
	})
}

func truncateStr(s string, maxLen int) string {
	if len([]rune(s)) > maxLen {
		return string([]rune(s)[:maxLen])
	}
	return s
}
