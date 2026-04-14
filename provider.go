package main

import (
	"sort"
	"time"
)

// SessionProvider is the interface that each coding agent provider must implement.
// To add a new provider, create a new file (e.g. cursor.go), implement this
// interface, and register it in AllProviders() below.
type SessionProvider interface {
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
}

// AllProviders returns every registered provider in display order.
// To add a new provider, append it here.
func AllProviders() []SessionProvider {
	return []SessionProvider{
		&ClaudeProvider{},
		&CodexProvider{},
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
	FirstMessage string // fallback display name when slug is empty
	ProviderName string // set by the loader, used for display
}

type ChatMessage struct {
	Role      string
	Content   string
	Timestamp time.Time
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
		name := s.FirstMessage
		if len(name) > 50 {
			name = name[:47] + "..."
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
	if len(s) > maxLen {
		return s[:maxLen]
	}
	return s
}
