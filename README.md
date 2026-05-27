> [!WARNING]
> This is a vibe coded project and hasn't been thoroughly tested. There might be bugs and edge cases. Please [open an issue](https://github.com/danpiths/agent-session-manager/issues) if you find any. If you want another agent, feel free to open a PR or ask your agent to do it by reading this README to get started.

# Agent Session Manager

A terminal UI for browsing, previewing, deleting, and renaming your coding agent
sessions.

Built with [Bubbletea v2](https://charm.land/bubbletea/v2),
[Lipgloss v2](https://charm.land/lipgloss/v2), and
[x/ansi](https://github.com/charmbracelet/x/tree/main/ansi). Theme colors are
used from [Bifrost](https://github.com/maximhq/bifrost).

## Install

### Download a binary

Pick your platform and run one command — it downloads the binary and installs it to your PATH:

**macOS (Apple Silicon)**
```bash
curl -L https://github.com/danpiths/agent-session-manager/releases/latest/download/agent-session-manager-darwin-arm64 -o /usr/local/bin/agent-session-manager && chmod +x /usr/local/bin/agent-session-manager
```

**macOS (Intel)**
```bash
curl -L https://github.com/danpiths/agent-session-manager/releases/latest/download/agent-session-manager-darwin-amd64 -o /usr/local/bin/agent-session-manager && chmod +x /usr/local/bin/agent-session-manager
```

**Linux (x86_64)**
```bash
sudo curl -L https://github.com/danpiths/agent-session-manager/releases/latest/download/agent-session-manager-linux-amd64 -o /usr/local/bin/agent-session-manager && sudo chmod +x /usr/local/bin/agent-session-manager
```

**Linux (ARM64)**
```bash
sudo curl -L https://github.com/danpiths/agent-session-manager/releases/latest/download/agent-session-manager-linux-arm64 -o /usr/local/bin/agent-session-manager && sudo chmod +x /usr/local/bin/agent-session-manager
```

**Windows (PowerShell)**
```powershell
Invoke-WebRequest -Uri https://github.com/danpiths/agent-session-manager/releases/latest/download/agent-session-manager-windows-amd64.exe -OutFile "$env:LOCALAPPDATA\Microsoft\WindowsApps\agent-session-manager.exe"
```

Then run `agent-session-manager` from any terminal.

### Go install

```bash
go install github.com/danpiths/agent-session-manager@latest
```

### Build from source

```bash
git clone https://github.com/danpiths/agent-session-manager.git
cd agent-session-manager
go build -o agent-session-manager .
```

## Usage

```bash
# Interactive provider picker
./agent-session-manager

# Skip picker, go straight to a provider
./agent-session-manager --provider claude
./agent-session-manager --provider codex
./agent-session-manager --provider forge
```

The `--provider` flag matches against provider names case-insensitively.

## Keybindings

| Key                 | Context    | Action                                   |
| ------------------- | ---------- | ---------------------------------------- |
| `j` / `k`           | All panels | Navigate up/down                         |
| `Enter`             | Projects   | Drill into sessions                      |
| `Enter`             | Sessions   | Drill into preview                       |
| `Esc`               | Preview    | Back to sessions                         |
| `Esc`               | Sessions   | Back to projects                         |
| `Esc`               | Projects   | Back to provider picker                  |
| `d`                 | Sessions   | Delete session (with confirmation)       |
| `r`                 | Sessions   | Rename session (if provider supports it) |
| `d` / `u`           | Preview    | Half-page down/up                        |
| `Ctrl+d` / `Ctrl+u` | All panels | Half-page down/up                        |
| `Tab` / `Shift+Tab` | All panels | Cycle panel focus                        |
| `q` / `Ctrl+c`      | Anywhere   | Quit                                     |

## Architecture

```
provider.go   ← SessionProvider interface, shared types, registry
claude.go     ← Claude Code provider implementation
codex.go      ← Codex provider implementation
forge.go      ← Forge Code provider implementation
main.go       ← Bubbletea UI (model, update, view, helpers)
```

### The `SessionProvider` interface (`provider.go`)

Every provider implements this interface:

```go
type SessionProvider interface {
    Name() string                                          // display name
    Description() string                                   // hint for picker (e.g. path)
    LoadProjects() ([]Project, error)                      // discover sessions grouped by project
    DeleteSession(session Session) error                   // remove session files
    RenameSession(session Session, newName string) error   // change display name
    CanRename() bool                                       // whether rename is supported
}
```

Providers are registered in `AllProviders()` at the top of `provider.go`. The UI
reads this slice — it never references provider types directly. The picker,
title bar, status bar keybindings, and assistant label in the preview all derive
from the interface.

### Shared types (`provider.go`)

- `Session` — parsed session with ID, slug, messages, timestamps, file path, and
  `ProviderName` string
- `ChatMessage` — role + content + timestamp
- `Project` — groups sessions by working directory path
- `Session.DisplayName()` — returns slug, falls back to first user message
  (truncated to 50 chars), then session ID prefix

### Provider implementations

**`claude.go` — Claude Code**

- Reads `~/.claude/projects/<encoded-path>/*.jsonl`
- Each subdirectory under `projects/` is one project. The directory name is a
  dash-encoded absolute path (e.g. `-Users-foo-myproject`).
- Session files are `<uuid>.jsonl`. Each line is a JSON object with a `type`
  field. Messages have `type: "user"` or `type: "assistant"` with a
  `message.content` field (string or array of content blocks). The `slug` field
  on messages is the session display name.
- Delete removes the `.jsonl` file and associated `<uuid>/` directory (stores
  attachments).
- Rename rewrites the JSONL, updating all `slug` fields in-place.

**`codex.go` — OpenAI Codex**

- Reads `~/.codex/sessions/YYYY/MM/DD/*.jsonl`
- Each JSONL starts with a `session_meta` line containing `id`, `cwd`,
  `timestamp`.
- Messages come from `response_item` (structured content with
  `input_text`/`output_text` in a `content` array) and `event_msg` (user
  messages as a `message.content` string).
- Projects are grouped by `cwd` from the session metadata.
- Rename is not supported (no slug field in the format).

**`forge.go` — Forge Code**

- Reads from the SQLite database at `~/.config/forgecode/.forge.db`.
- Queries the `conversations` table for `conversation_id`, `title`,
  `workspace_id`, `context`, `created_at`, and `updated_at`.
- The `context` column is a JSON blob containing the conversation messages.
  Each message has a `message.text` object with `role` and `content` fields.
- The project path is extracted from the `<current_working_directory>` tag in
  the first system message, identifying where the conversation was started.
- Projects are grouped by workspace ID and project path.
- Rename is supported — updates the `title` column in the database.
- Delete removes the row from the `conversations` table.

### UI layer (`main.go`)

**App modes** (the `mode` field on `model`):

- `modePickProvider` — initial provider selection screen
- `modeNormal` — three-panel browsing (projects, sessions, preview)
- `modeConfirmDelete` — awaiting `y`/`n` for deletion
- `modeRename` — inline text input for new session name

**Panel system** (the `activePanel` field):

- `panelProjects` (0) — left panel, 25% width
- `panelSessions` (1) — middle panel, 30% width
- `panelPreview` (2) — right panel, remaining width

**Theme and light/dark mode:**

- Colors defined in `newTheme(isDark bool)` using `lipgloss.LightDark()`.
- Auto-detection via `tea.BackgroundColorMsg` from Bubbletea v2.
- Colors based on [Bifrost](https://github.com/maximhq/bifrost) design system
  (teal accent `#33c09e`, gray scale from the project's Tailwind config).

**Layout rendering — the `fitContent` pattern:**

This is the critical rendering detail. Lipgloss `Width()` wraps content (adding
lines), and borders are drawn around the full wrapped content. `MaxHeight()`
then clips from the bottom — cutting the bottom border off. To avoid this:

1. Each panel renders content as a plain string with `joinLines(lines, height)`
   capping line count.
2. `fitContent(s, width, height)` post-processes: truncates each line with
   `ansi.Truncate` (ANSI-aware), pads to exact width with spaces, emits exactly
   `height` lines.
3. The border style is applied with **no**
   `Width`/`Height`/`MaxWidth`/`MaxHeight` — content is already pixel-perfect.

This ensures all three panels always have intact borders regardless of content
length.

**Status bar:**

- Context-aware keybinding hints change based on active panel and mode.
- The `r` (rename) hint only appears if `provider.CanRename()` returns true.
- All styled segments carry explicit `Background(th.statusBg)` to prevent bare
  gaps between ANSI runs.

## Adding a new provider

To add support for a new coding agent (e.g. Cursor, Windsurf, Aider):

### 1. Create a new file (e.g. `cursor.go`)

Implement the `SessionProvider` interface:

```go
package main

type CursorProvider struct{}

func (c *CursorProvider) Name() string        { return "Cursor" }
func (c *CursorProvider) Description() string { return "~/.cursor/sessions/" }
func (c *CursorProvider) CanRename() bool     { return false }

func (c *CursorProvider) LoadProjects() ([]Project, error) {
    // 1. Find the session storage directory
    // 2. Walk the directory, parsing session files
    // 3. Group sessions by project (working directory)
    // 4. Call sortProjects(projects) before returning
    // 5. Set ProviderName on each Session to c.Name()
    return nil, nil
}

func (c *CursorProvider) DeleteSession(session Session) error {
    return os.Remove(session.FilePath)
}

func (c *CursorProvider) RenameSession(_ Session, _ string) error {
    return fmt.Errorf("cursor sessions cannot be renamed")
}
```

### 2. Register it in `provider.go`

Add one line to `AllProviders()`:

```go
func AllProviders() []SessionProvider {
    return []SessionProvider{
        &ClaudeProvider{},
        &CodexProvider{},
        &ForgeProvider{},
        &CursorProvider{},  // ← add here
    }
}
```

That's it. The picker, title, keybindings, and preview all adapt automatically.

### Implementation checklist for new providers

- [ ] Find where the agent stores sessions on disk (check `~/.<agent>/` or
      `~/.config/<agent>/`)
- [ ] Understand the session file format (usually JSONL, sometimes SQLite)
- [ ] Implement `LoadProjects()` — discover all sessions, parse timestamps and
      messages, group by project
- [ ] Set `Session.ProviderName` to your provider's `Name()` on every parsed
      session
- [ ] Set `Session.Slug` if the format has a display name field, otherwise leave
      empty (falls back to first user message)
- [ ] Set `Session.FirstMessage` to the first user message text for display name
      fallback
- [ ] Keep `Session.FilePath` pointing to the main session file (used for
      delete/rename)
- [ ] Implement `DeleteSession()` — remove session file and any associated
      directories
- [ ] Implement `RenameSession()` or return an error if the format doesn't
      support it
- [ ] Call `sortProjects(projects)` before returning from `LoadProjects()`
- [ ] Ensure sessions within each project are sorted newest-first

### Known session storage locations

| Agent       | Typical path                                                       | Format                 |
| ----------- | ------------------------------------------------------------------ | ---------------------- |
| Claude Code | `~/.claude/projects/<encoded-path>/*.jsonl`                        | JSONL, one per session |
| Codex       | `~/.codex/sessions/YYYY/MM/DD/*.jsonl`                             | JSONL, one per session |
| Forge Code  | `~/.config/forgecode/.forge.db`                                    | SQLite database        |
| Cursor      | `~/.cursor/` or `~/Library/Application Support/Cursor/`            | Varies                 |
| Windsurf    | `~/.windsurf/` or `~/Library/Application Support/Windsurf/`        | Varies                 |
| Aider       | `~/.aider.chat.history.md` or `.aider.chat.history.md` per project | Markdown               |

## Dependencies

| Package                           | Purpose                                        |
| --------------------------------- | ---------------------------------------------- |
| `charm.land/bubbletea/v2`         | TUI framework (model-update-view)              |
| `charm.land/lipgloss/v2`          | Terminal styling and layout                    |
| `github.com/charmbracelet/x/ansi` | ANSI-aware string truncation (`ansi.Truncate`) |
| `modernc.org/sqlite`              | Pure-Go SQLite driver (for Forge Code)         |

All other dependencies are transitive.

## Gotchas for contributors

- **Don't use `Width()`/`MaxHeight()` on bordered styles** — see the
  `fitContent` section above. Content must be pre-sized.
- **All status bar sub-styles need explicit `Background()`** — otherwise you get
  bare white/black gaps between ANSI-styled segments.
- **`lipgloss.Width()` measures display width** (ignores ANSI codes, handles
  wide chars). Use it instead of `len()` for anything that will be rendered.
- **`ansi.Truncate(s, n, "")` is the correct way to truncate styled strings** —
  it respects ANSI escape sequences and wide characters.
- **Bubbletea v2 uses `tea.KeyPressMsg`** (not `tea.KeyMsg`). Escape key is
  `"esc"` (not `"escape"`). Space is `"space"` (not `" "`).
- **`tea.View` is a struct** returned from `View()`, not a string. Use
  `tea.NewView(s)` and set `v.AltScreen = true`.
- **Light/dark detection** — listen for `tea.BackgroundColorMsg` in `Update()`,
  call `msg.IsDark()`.

## Build

Requires Go 1.24+.

```bash
go build -o agent-session-manager .
```
