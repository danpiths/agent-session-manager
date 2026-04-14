package main

import (
	"flag"
	"fmt"
	"image/color"
	"os"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
)

// Panel focus states
const (
	panelProjects = iota
	panelSessions
	panelPreview
)

// App modes
const (
	modePickProvider = iota
	modeNormal
	modeConfirmDelete
	modeRename
)

// ── Theme ───────────────────────────────────────────────────────────

type theme struct {
	accent    color.Color
	accentDim color.Color
	fg        color.Color
	fgDim     color.Color
	fgMuted   color.Color
	bg        color.Color
	border    color.Color
	borderFoc color.Color
	user      color.Color
	assistant color.Color
	err       color.Color
	statusBg  color.Color
	statusFg  color.Color
	titleBg   color.Color
	titleFg   color.Color
}

func newTheme(isDark bool) theme {
	ld := lipgloss.LightDark(isDark)
	return theme{
		accent:    ld(lipgloss.Color("#0d9373"), lipgloss.Color("#33c09e")),
		accentDim: ld(lipgloss.Color("#0d3b43"), lipgloss.Color("#1a5c4a")),
		fg:        ld(lipgloss.Color("#111827"), lipgloss.Color("#ededed")),
		fgDim:     ld(lipgloss.Color("#52525B"), lipgloss.Color("#a1a1aa")),
		fgMuted:   ld(lipgloss.Color("#71717A"), lipgloss.Color("#71717A")),
		bg:        ld(lipgloss.Color("#f9fafb"), lipgloss.Color("#0a0a0a")),
		border:    ld(lipgloss.Color("#e4e4e7"), lipgloss.Color("#374151")),
		borderFoc: ld(lipgloss.Color("#0d9373"), lipgloss.Color("#33c09e")),
		user:      ld(lipgloss.Color("#0284c7"), lipgloss.Color("#82AAFF")),
		assistant: ld(lipgloss.Color("#15803d"), lipgloss.Color("#C3E88D")),
		err:       ld(lipgloss.Color("#dc2626"), lipgloss.Color("#FF6B6B")),
		statusBg:  ld(lipgloss.Color("#e4e4e7"), lipgloss.Color("#1f2937")),
		statusFg:  ld(lipgloss.Color("#111827"), lipgloss.Color("#ededed")),
		titleBg:   ld(lipgloss.Color("#0d9373"), lipgloss.Color("#0d3b43")),
		titleFg:   ld(lipgloss.Color("#ffffff"), lipgloss.Color("#33c09e")),
	}
}

// ── Model ───────────────────────────────────────────────────────────

type model struct {
	providers    []SessionProvider // all registered providers
	provider     SessionProvider   // currently selected provider
	pickerCursor int

	projects      []Project
	activePanel   int
	mode          int
	projectCursor int
	sessionCursor int
	previewScroll int
	renameInput   string
	width         int
	height        int
	statusMessage string
	statusExpiry  time.Time
	isDark        bool
	th            theme
	err           error
}

type reloadMsg struct{}
type statusClearMsg struct{}

func initialModel(providerFlag string) model {
	providers := AllProviders()
	m := model{
		providers: providers,
		isDark:    true,
		mode:      modePickProvider,
	}
	m.th = newTheme(m.isDark)

	// If --provider flag matches a registered provider name, skip the picker
	if providerFlag != "" {
		for i, p := range providers {
			if strings.EqualFold(p.Name(), providerFlag) {
				m.provider = p
				m.pickerCursor = i
				m.mode = modeNormal
				m.loadProjects()
				break
			}
		}
	}

	return m
}

func (m *model) loadProjects() {
	if m.provider == nil {
		return
	}
	projects, err := m.provider.LoadProjects()
	m.projects = projects
	m.err = err
}

func (m model) Init() tea.Cmd {
	return tea.Batch(tea.RequestWindowSize, func() tea.Msg {
		return tea.RequestBackgroundColor()
	})
}

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.BackgroundColorMsg:
		m.isDark = msg.IsDark()
		m.th = newTheme(m.isDark)
		return m, nil

	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		return m, nil

	case reloadMsg:
		prevProjectID := ""
		if m.projectCursor < len(m.projects) {
			prevProjectID = m.projects[m.projectCursor].ID
		}
		m.loadProjects()
		m.projectCursor = 0
		for i, p := range m.projects {
			if p.ID == prevProjectID {
				m.projectCursor = i
				break
			}
		}
		if m.projectCursor >= len(m.projects) {
			m.projectCursor = max(0, len(m.projects)-1)
		}
		m.fixSessionCursor()
		return m, nil

	case statusClearMsg:
		if time.Now().After(m.statusExpiry) {
			m.statusMessage = ""
		}
		return m, nil

	case tea.KeyPressMsg:
		return m.handleKey(msg)
	}
	return m, nil
}

func (m model) handleKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	key := msg.String()

	if key == "ctrl+c" {
		return m, tea.Quit
	}

	if m.mode == modePickProvider {
		return m.handlePickProvider(key)
	}

	switch m.mode {
	case modeConfirmDelete:
		return m.handleDeleteConfirm(key)
	case modeRename:
		return m.handleRenameInput(msg, key)
	default:
		return m.handleNormal(key)
	}
}

func (m model) handlePickProvider(key string) (tea.Model, tea.Cmd) {
	switch key {
	case "q":
		return m, tea.Quit
	case "j", "down":
		if m.pickerCursor < len(m.providers)-1 {
			m.pickerCursor++
		}
		return m, nil
	case "k", "up":
		if m.pickerCursor > 0 {
			m.pickerCursor--
		}
		return m, nil
	case "enter":
		return m.selectProvider()
	default:
		// Number keys 1-9 for quick selection
		if len(key) == 1 && key[0] >= '1' && key[0] <= '9' {
			idx := int(key[0] - '1')
			if idx < len(m.providers) {
				m.pickerCursor = idx
				return m.selectProvider()
			}
		}
	}
	return m, nil
}

func (m model) selectProvider() (tea.Model, tea.Cmd) {
	if m.pickerCursor < len(m.providers) {
		m.provider = m.providers[m.pickerCursor]
		m.mode = modeNormal
		m.loadProjects()
	}
	return m, nil
}

func (m model) handleNormal(key string) (tea.Model, tea.Cmd) {
	switch key {
	case "q":
		return m, tea.Quit

	case "enter":
		switch m.activePanel {
		case panelProjects:
			if len(m.currentSessions()) > 0 {
				m.activePanel = panelSessions
				m.sessionCursor = 0
			}
		case panelSessions:
			if m.hasSelectedSession() {
				m.activePanel = panelPreview
				m.previewScroll = 0
			}
		}
		return m, nil
	case "esc":
		switch m.activePanel {
		case panelPreview:
			m.activePanel = panelSessions
		case panelSessions:
			m.activePanel = panelProjects
		case panelProjects:
			m.mode = modePickProvider
			m.provider = nil
			m.projects = nil
			m.projectCursor = 0
			m.sessionCursor = 0
			m.previewScroll = 0
		}
		return m, nil

	case "tab":
		m.activePanel = (m.activePanel + 1) % 3
		return m, nil
	case "shift+tab":
		m.activePanel = (m.activePanel + 2) % 3
		return m, nil

	case "j", "down":
		m.moveCursor(1)
		return m, nil
	case "k", "up":
		m.moveCursor(-1)
		return m, nil

	case "d":
		if m.activePanel == panelPreview {
			m.moveCursor(m.halfPage())
			return m, nil
		}
		if m.activePanel == panelSessions && m.hasSelectedSession() {
			m.mode = modeConfirmDelete
		}
		return m, nil
	case "u":
		if m.activePanel == panelPreview {
			m.moveCursor(-m.halfPage())
			return m, nil
		}
		return m, nil

	case "ctrl+d":
		m.moveCursor(m.halfPage())
		return m, nil
	case "ctrl+u":
		m.moveCursor(-m.halfPage())
		return m, nil

	case "r":
		if m.activePanel == panelSessions && m.hasSelectedSession() {
			if m.provider != nil && !m.provider.CanRename() {
				m.statusMessage = fmt.Sprintf("%s sessions cannot be renamed", m.provider.Name())
				m.statusExpiry = time.Now().Add(2 * time.Second)
				return m, tea.Tick(2*time.Second, func(time.Time) tea.Msg { return statusClearMsg{} })
			}
			m.mode = modeRename
			m.renameInput = m.selectedSession().DisplayName()
		}
		return m, nil
	}
	return m, nil
}

func (m model) handleDeleteConfirm(key string) (tea.Model, tea.Cmd) {
	switch key {
	case "y":
		if !m.hasSelectedSession() || m.provider == nil {
			m.mode = modeNormal
			return m, nil
		}
		sess := m.selectedSession()
		if err := m.provider.DeleteSession(sess); err != nil {
			m.statusMessage = fmt.Sprintf("Error: %v", err)
		} else {
			m.statusMessage = fmt.Sprintf("Deleted: %s", sess.DisplayName())
		}
		m.mode = modeNormal
		m.statusExpiry = time.Now().Add(3 * time.Second)
		return m, tea.Batch(
			func() tea.Msg { return reloadMsg{} },
			tea.Tick(3*time.Second, func(time.Time) tea.Msg { return statusClearMsg{} }),
		)
	case "n", "esc":
		m.mode = modeNormal
		return m, nil
	}
	return m, nil
}

func (m model) handleRenameInput(msg tea.KeyPressMsg, key string) (tea.Model, tea.Cmd) {
	switch key {
	case "enter":
		if m.renameInput != "" && m.hasSelectedSession() && m.provider != nil {
			sess := m.selectedSession()
			if err := m.provider.RenameSession(sess, m.renameInput); err != nil {
				m.statusMessage = fmt.Sprintf("Error: %v", err)
			} else {
				m.statusMessage = fmt.Sprintf("Renamed to: %s", m.renameInput)
			}
			m.mode = modeNormal
			m.renameInput = ""
			m.statusExpiry = time.Now().Add(3 * time.Second)
			return m, tea.Batch(
				func() tea.Msg { return reloadMsg{} },
				tea.Tick(3*time.Second, func(time.Time) tea.Msg { return statusClearMsg{} }),
			)
		}
		return m, nil
	case "esc":
		m.mode = modeNormal
		m.renameInput = ""
		return m, nil
	case "backspace":
		if len(m.renameInput) > 0 {
			m.renameInput = m.renameInput[:len(m.renameInput)-1]
		}
		return m, nil
	default:
		if msg.Text != "" {
			m.renameInput += msg.Text
		}
		return m, nil
	}
}

func (m *model) moveCursor(delta int) {
	switch m.activePanel {
	case panelProjects:
		m.projectCursor = clamp(m.projectCursor+delta, 0, max(0, len(m.projects)-1))
		m.sessionCursor = 0
		m.previewScroll = 0
	case panelSessions:
		sessions := m.currentSessions()
		m.sessionCursor = clamp(m.sessionCursor+delta, 0, max(0, len(sessions)-1))
		m.previewScroll = 0
	case panelPreview:
		m.previewScroll = max(0, m.previewScroll+delta)
	}
}

func (m model) halfPage() int {
	return max(1, m.panelInnerHeight()/2)
}

func (m model) panelInnerHeight() int {
	return max(m.height-4, 1)
}

func (m model) currentSessions() []Session {
	if m.projectCursor < len(m.projects) {
		return m.projects[m.projectCursor].Sessions
	}
	return nil
}

func (m model) hasSelectedSession() bool {
	sessions := m.currentSessions()
	return len(sessions) > 0 && m.sessionCursor < len(sessions)
}

func (m model) selectedSession() Session {
	return m.currentSessions()[m.sessionCursor]
}

func (m *model) fixSessionCursor() {
	sessions := m.currentSessions()
	if m.sessionCursor >= len(sessions) {
		m.sessionCursor = max(0, len(sessions)-1)
	}
}

func (m model) providerName() string {
	if m.provider != nil {
		return m.provider.Name()
	}
	return "Session Manager"
}

// ── View ────────────────────────────────────────────────────────────

func (m model) View() tea.View {
	if m.width == 0 {
		return tea.NewView("Loading...")
	}

	if m.mode == modePickProvider {
		v := tea.NewView(m.renderProviderPicker())
		v.AltScreen = true
		return v
	}

	if m.err != nil {
		v := tea.NewView(fmt.Sprintf("Error loading sessions: %v\n\nPress q to quit.", m.err))
		v.AltScreen = true
		return v
	}

	th := m.th

	panelOuterHeight := max(m.height-2, 3)
	panelInnerHeight := panelOuterHeight - 2

	projectOuter := m.width * 25 / 100
	sessionOuter := m.width * 30 / 100
	previewOuter := m.width - projectOuter - sessionOuter

	projectInner := clamp(projectOuter-2, 1, m.width)
	sessionInner := clamp(sessionOuter-2, 1, m.width)
	previewInner := clamp(previewOuter-2, 1, m.width)

	projectContent := m.renderProjectPanel(projectInner, panelInnerHeight)
	sessionContent := m.renderSessionPanel(sessionInner, panelInnerHeight)
	previewContent := m.renderPreviewPanel(previewInner, panelInnerHeight)

	projectContent = fitContent(projectContent, projectInner, panelInnerHeight)
	sessionContent = fitContent(sessionContent, sessionInner, panelInnerHeight)
	previewContent = fitContent(previewContent, previewInner, panelInnerHeight)

	makeBorder := func(active bool) lipgloss.Style {
		bc := th.border
		if active {
			bc = th.borderFoc
		}
		return lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			BorderForeground(bc)
	}

	projectBox := makeBorder(m.activePanel == panelProjects).Render(projectContent)
	sessionBox := makeBorder(m.activePanel == panelSessions).Render(sessionContent)
	previewBox := makeBorder(m.activePanel == panelPreview).Render(previewContent)

	panels := lipgloss.JoinHorizontal(lipgloss.Top, projectBox, sessionBox, previewBox)

	titleStyle := lipgloss.NewStyle().
		Bold(true).
		Foreground(th.titleFg).
		Background(th.titleBg).
		Padding(0, 1)
	title := titleStyle.Render(fmt.Sprintf(" %s Session Manager ", m.providerName()))

	statusBar := m.renderStatusBar()

	view := lipgloss.JoinVertical(lipgloss.Left, title, panels, statusBar)

	v := tea.NewView(view)
	v.AltScreen = true
	return v
}

func (m model) renderProviderPicker() string {
	th := m.th

	titleStyle := lipgloss.NewStyle().
		Bold(true).
		Foreground(th.titleFg).
		Background(th.titleBg).
		Padding(0, 1)

	headerStyle := lipgloss.NewStyle().
		Foreground(th.fgMuted).
		MarginBottom(1)

	selectedStyle := lipgloss.NewStyle().
		Foreground(th.accent).
		Bold(true)

	normalStyle := lipgloss.NewStyle().
		Foreground(th.fg)

	dimStyle := lipgloss.NewStyle().
		Foreground(th.fgDim)

	var b strings.Builder
	b.WriteString(titleStyle.Render(" Agent Session Manager "))
	b.WriteString("\n\n")
	b.WriteString(headerStyle.Render("Select a provider:"))
	b.WriteString("\n\n")

	for i, p := range m.providers {
		if i == m.pickerCursor {
			b.WriteString(selectedStyle.Render(fmt.Sprintf("  ▸ %s", p.Name())))
		} else {
			b.WriteString(normalStyle.Render(fmt.Sprintf("    %s", p.Name())))
		}
		b.WriteString(dimStyle.Render(fmt.Sprintf("  %s", p.Description())))
		b.WriteString("\n")
	}

	b.WriteString("\n")
	b.WriteString(dimStyle.Render("  ↵ select  q quit"))

	return lipgloss.Place(m.width, m.height, lipgloss.Center, lipgloss.Center, b.String())
}

func (m model) renderProjectPanel(width, height int) string {
	th := m.th
	header := lipgloss.NewStyle().Foreground(th.fgMuted).Render("PROJECTS")
	sep := lipgloss.NewStyle().Foreground(th.border).Render(strings.Repeat("─", width))

	var lines []string
	lines = append(lines, header)
	lines = append(lines, sep)

	if len(m.projects) == 0 {
		lines = append(lines, lipgloss.NewStyle().Foreground(th.fgMuted).Render("No projects found"))
		return joinLines(lines, height)
	}

	maxVisible := height - 2
	startIdx := scrollOffset(m.projectCursor, maxVisible, len(m.projects))

	selectedStyle := lipgloss.NewStyle().Foreground(th.accent).Bold(true)
	normalStyle := lipgloss.NewStyle().Foreground(th.fg)
	countStyle := lipgloss.NewStyle().Foreground(th.fgDim)

	for i := startIdx; i < len(m.projects) && i-startIdx < maxVisible; i++ {
		proj := m.projects[i]
		display := shortenPath(proj.Path, width-8)
		count := countStyle.Render(fmt.Sprintf(" (%d)", len(proj.Sessions)))

		if i == m.projectCursor {
			lines = append(lines, selectedStyle.Render("▸ "+display)+count)
		} else {
			lines = append(lines, normalStyle.Render("  "+display)+count)
		}
	}

	return joinLines(lines, height)
}

func (m model) renderSessionPanel(width, height int) string {
	th := m.th
	header := lipgloss.NewStyle().Foreground(th.fgMuted).Render("SESSIONS")
	sep := lipgloss.NewStyle().Foreground(th.border).Render(strings.Repeat("─", width))

	var lines []string
	lines = append(lines, header)
	lines = append(lines, sep)

	sessions := m.currentSessions()
	if len(sessions) == 0 {
		lines = append(lines, lipgloss.NewStyle().Foreground(th.fgMuted).Render("No sessions"))
		return joinLines(lines, height)
	}

	maxVisible := height - 2
	startIdx := scrollOffset(m.sessionCursor, maxVisible, len(sessions))

	selectedStyle := lipgloss.NewStyle().Foreground(th.accent).Bold(true)
	normalStyle := lipgloss.NewStyle().Foreground(th.fg)
	ageStyle := lipgloss.NewStyle().Foreground(th.fgDim)

	for i := startIdx; i < len(sessions) && i-startIdx < maxVisible; i++ {
		sess := sessions[i]
		name := sess.DisplayName()
		age := ageStyle.Render(" " + formatAge(sess.Timestamp))

		if i == m.sessionCursor {
			lines = append(lines, selectedStyle.Render("▸ "+name)+age)
		} else {
			lines = append(lines, normalStyle.Render("  "+name)+age)
		}
	}

	return joinLines(lines, height)
}

func (m model) renderPreviewPanel(width, height int) string {
	th := m.th
	header := lipgloss.NewStyle().Foreground(th.fgMuted).Render("PREVIEW")
	sep := lipgloss.NewStyle().Foreground(th.border).Render(strings.Repeat("─", width))

	var lines []string
	lines = append(lines, header)
	lines = append(lines, sep)

	if !m.hasSelectedSession() {
		lines = append(lines, lipgloss.NewStyle().Foreground(th.fgMuted).Render("Select a session"))
		return joinLines(lines, height)
	}

	sess := m.selectedSession()
	metaStyle := lipgloss.NewStyle().Foreground(th.fgDim)

	lines = append(lines, metaStyle.Render(fmt.Sprintf("ID: %s", sess.ID[:min(12, len(sess.ID))])))
	lines = append(lines, metaStyle.Render(fmt.Sprintf("Time: %s", sess.Timestamp.Local().Format("2006-01-02 15:04"))))
	lines = append(lines, sep)

	userStyle := lipgloss.NewStyle().Foreground(th.user).Bold(true)
	asstStyle := lipgloss.NewStyle().Foreground(th.assistant)
	asstLabel := m.providerName()

	var msgLines []string
	for _, msg := range sess.Messages {
		if msg.Role == "user" {
			msgLines = append(msgLines, userStyle.Render("▶ You:"))
		} else {
			msgLines = append(msgLines, asstStyle.Render("◀ "+asstLabel+":"))
		}
		wrapped := wordWrap(msg.Content, width-2)
		for l := range strings.SplitSeq(wrapped, "\n") {
			msgLines = append(msgLines, "  "+l)
		}
		msgLines = append(msgLines, "")
	}

	scroll := min(m.previewScroll, len(msgLines))
	msgLines = msgLines[scroll:]

	avail := height - len(lines)
	if avail > 0 && len(msgLines) > avail {
		msgLines = msgLines[:avail]
	}

	lines = append(lines, msgLines...)
	return joinLines(lines, height)
}

func (m model) renderStatusBar() string {
	th := m.th
	barStyle := lipgloss.NewStyle().
		Foreground(th.statusFg).
		Background(th.statusBg).
		Padding(0, 1).
		Width(m.width - 2)

	keyStyle := lipgloss.NewStyle().
		Foreground(th.accent).
		Background(th.statusBg).
		Bold(true)
	descStyle := lipgloss.NewStyle().
		Foreground(th.fgDim).
		Background(th.statusBg)
	spaceStyle := lipgloss.NewStyle().
		Background(th.statusBg)

	sp := spaceStyle.Render("  ")
	sm := spaceStyle.Render(" ")

	var status string
	switch m.mode {
	case modeConfirmDelete:
		errStyle := lipgloss.NewStyle().
			Foreground(th.err).
			Background(th.statusBg).
			Bold(true)
		if m.hasSelectedSession() {
			sess := m.selectedSession()
			status = errStyle.Render(fmt.Sprintf("Delete '%s'? ", sess.DisplayName())) +
				keyStyle.Render("y") + descStyle.Render("/") + keyStyle.Render("n")
		} else {
			status = errStyle.Render("No session selected")
		}
	case modeRename:
		accentStyle := lipgloss.NewStyle().
			Foreground(th.accent).
			Background(th.statusBg).
			Bold(true)
		fgStyle := lipgloss.NewStyle().
			Foreground(th.fg).
			Background(th.statusBg)
		status = accentStyle.Render("Rename: ") +
			fgStyle.Render(m.renameInput+"█") +
			descStyle.Render("  enter confirm  esc cancel")
	default:
		if m.statusMessage != "" {
			status = lipgloss.NewStyle().
				Foreground(th.accent).
				Background(th.statusBg).
				Render(m.statusMessage)
		} else {
			var pairs [][2]string

			switch m.activePanel {
			case panelProjects:
				pairs = append(pairs, [2]string{"↵", "sessions"})
				pairs = append(pairs, [2]string{"esc", "provider"})
			case panelSessions:
				pairs = append(pairs, [2]string{"↵", "preview"})
				pairs = append(pairs, [2]string{"esc", "projects"})
			case panelPreview:
				pairs = append(pairs, [2]string{"esc", "sessions"})
			}

			pairs = append(pairs, [2]string{"j/k", "nav"})

			switch m.activePanel {
			case panelSessions:
				pairs = append(pairs, [2]string{"d", "delete"})
				if m.provider != nil && m.provider.CanRename() {
					pairs = append(pairs, [2]string{"r", "rename"})
				}
			case panelPreview:
				pairs = append(pairs, [2]string{"d/u", "½page"})
			}

			pairs = append(pairs, [2]string{"q", "quit"})

			var parts []string
			for _, p := range pairs {
				parts = append(parts, keyStyle.Render(p[0])+sm+descStyle.Render(p[1]))
			}
			status = strings.Join(parts, sp)
		}
	}

	return barStyle.Render(status)
}

// ── Helpers ──────────────────────────────────────────────────────────

func fitContent(s string, width, height int) string {
	lines := strings.Split(s, "\n")

	out := make([]string, height)
	for i := range height {
		var raw string
		if i < len(lines) {
			raw = lines[i]
		}
		w := lipgloss.Width(raw)
		if w > width {
			raw = ansi.Truncate(raw, width, "")
			w = lipgloss.Width(raw)
		}
		if w < width {
			raw += strings.Repeat(" ", width-w)
		}
		out[i] = raw
	}
	return strings.Join(out, "\n")
}

func joinLines(lines []string, height int) string {
	if len(lines) > height {
		lines = lines[:height]
	}
	return strings.Join(lines, "\n")
}

func scrollOffset(cursor, visible, total int) int {
	if total <= visible {
		return 0
	}
	start := cursor - visible/2
	if start < 0 {
		return 0
	}
	if start > total-visible {
		return total - visible
	}
	return start
}

func shortenPath(path string, maxLen int) string {
	if maxLen < 5 {
		maxLen = 5
	}
	if len(path) <= maxLen {
		return path
	}
	parts := strings.Split(path, "/")
	if len(parts) <= 2 {
		return path[:maxLen-1] + "…"
	}
	short := "…/" + strings.Join(parts[len(parts)-2:], "/")
	if len(short) > maxLen {
		return short[:maxLen-1] + "…"
	}
	return short
}

func formatAge(t time.Time) string {
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	default:
		days := int(d.Hours() / 24)
		if days == 1 {
			return "1d ago"
		}
		return fmt.Sprintf("%dd ago", days)
	}
}

func wordWrap(s string, width int) string {
	if width <= 0 {
		return s
	}
	var result strings.Builder
	for line := range strings.SplitSeq(s, "\n") {
		if len(line) <= width {
			if result.Len() > 0 {
				result.WriteByte('\n')
			}
			result.WriteString(line)
			continue
		}
		words := strings.Fields(line)
		currentLen := 0
		first := true
		for _, word := range words {
			if currentLen+len(word)+1 > width && !first {
				result.WriteByte('\n')
				currentLen = 0
				first = true
			}
			if !first {
				result.WriteByte(' ')
				currentLen++
			}
			result.WriteString(word)
			currentLen += len(word)
			first = false
		}
		result.WriteByte('\n')
	}
	return strings.TrimRight(result.String(), "\n")
}

func clamp(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

// ── Main ────────────────────────────────────────────────────────────

func main() {
	providerFlag := flag.String("provider", "", "Provider name (e.g. 'claude', 'codex'). Skips the picker.")
	flag.Parse()

	m := initialModel(*providerFlag)
	p := tea.NewProgram(m)
	if _, err := p.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
}
