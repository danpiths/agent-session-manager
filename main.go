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
	modeDuplicate
	modeResult
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
	exportDir     string
	marked        map[string]bool
	busy          bool
	result        string
	resultScroll  int
}

type reloadMsg struct{}
type statusClearMsg struct{}
type actionResultMsg struct {
	text string
	err  error
}

func initialModel(providerFlag string) model {
	providers := AllProviders()
	m := model{
		providers: providers,
		isDark:    true,
		mode:      modePickProvider,
	}
	m.th = newTheme(m.isDark)
	m.exportDir, m.err = os.Getwd()
	m.marked = make(map[string]bool)

	// If --provider flag matches a registered provider name, skip the picker
	if providerFlag != "" {
		for i, p := range providers {
			if strings.EqualFold(p.Name(), providerFlag) || strings.EqualFold(p.ID(), providerFlag) {
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
		m.previewScroll = min(m.previewScroll, m.maxPreviewScroll())
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
	case actionResultMsg:
		m.busy = false
		m.mode = modeResult
		m.result = msg.text
		m.resultScroll = 0
		if msg.err != nil {
			m.result = "Action failed\n\n" + msg.err.Error()
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
	if m.busy {
		return m, nil
	}
	if m.mode == modeResult {
		if key == "enter" || key == "esc" {
			m.mode = modeNormal
		}
		lines := strings.Split(wordWrap(m.result, max(1, m.width-4)), "\n")
		limit := max(0, len(lines)-max(1, m.height-4))
		switch key {
		case "j", "down":
			m.resultScroll = min(limit, m.resultScroll+1)
		case "k", "up":
			m.resultScroll = max(0, m.resultScroll-1)
		case "ctrl+d", "pgdown":
			m.resultScroll = min(limit, m.resultScroll+max(1, m.height/2))
		case "ctrl+u", "pgup":
			m.resultScroll = max(0, m.resultScroll-max(1, m.height/2))
		}
		return m, nil
	}
	if m.mode == modeDuplicate {
		if key == "esc" {
			m.mode = modeNormal
			return m, nil
		}
		for i, p := range importProviders() {
			if key == fmt.Sprint(i+1) {
				return m.startDuplicate(p)
			}
		}
		return m, nil
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
		m.projectCursor, m.sessionCursor, m.previewScroll = 0, 0, 0
		m.activePanel = panelProjects
		m.marked = make(map[string]bool)
		m.loadProjects()
	}
	return m, nil
}

func (m model) handleNormal(key string) (tea.Model, tea.Cmd) {
	switch key {
	case "space":
		if m.activePanel != panelProjects && m.hasSelectedSession() {
			s := m.selectedSession()
			id := s.FilePath + ":" + s.ID
			m.marked[id] = !m.marked[id]
		}
		return m, nil
	case "e":
		sessions := m.actionSessions()
		if len(sessions) == 0 {
			return m, nil
		}
		label := sessions[0].DisplayName()
		if m.activePanel == panelProjects {
			label = m.projects[m.projectCursor].Path
		}
		m.busy = true
		return m, func() tea.Msg {
			path, err := exportSessions(m.exportDir, label, sessions)
			return actionResultMsg{text: fmt.Sprintf("Exported %d chat(s)\n\n%s", len(sessions), path), err: err}
		}
	case "c":
		if len(m.actionSessions()) > 0 {
			m.mode = modeDuplicate
		}
		return m, nil
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
			runes := []rune(m.renameInput)
			m.renameInput = string(runes[:len(runes)-1])
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
		clear(m.marked)
	case panelSessions:
		sessions := m.currentSessions()
		m.sessionCursor = clamp(m.sessionCursor+delta, 0, max(0, len(sessions)-1))
		m.previewScroll = 0
	case panelPreview:
		m.previewScroll = clamp(m.previewScroll+delta, 0, m.maxPreviewScroll())
	}
}

func (m model) halfPage() int {
	return max(1, m.panelInnerHeight()/2)
}

func (m model) panelInnerHeight() int {
	return max(m.height-6, 1)
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
		v := tea.NewView(fitContent(wordWrap(fmt.Sprintf("Error loading sessions: %v\n\nEsc to choose another provider. q to quit.", m.err), m.width), m.width, m.height))
		v.AltScreen = true
		return v
	}

	th := m.th

	if m.width < 16 || m.height < 8 {
		v := tea.NewView(fitContent("Resize terminal\nq quit", m.width, m.height))
		v.AltScreen = true
		return v
	}
	if m.mode == modeDuplicate || m.mode == modeResult || m.mode == modeRename || m.mode == modeConfirmDelete || m.busy {
		content := "Working..."
		if m.mode == modeConfirmDelete && m.hasSelectedSession() {
			content = "Delete chat?\n\n" + singleLine(m.selectedSession().DisplayName()) + "\n\nThis permanently removes the source chat.\n\ny delete · n or Esc cancel"
		}
		if m.mode == modeRename {
			content = "Rename chat\n\n" + m.renameInput + "█\n\nEnter save · Esc cancel"
		}
		if m.mode == modeDuplicate {
			content = fmt.Sprintf("Duplicate %d chat(s)\n\n", len(m.actionSessions()))
			for i, p := range importProviders() {
				content += fmt.Sprintf("%d  %s\n", i+1, p.Name())
			}
			content += "\nRestores native state within the same provider.\nTranslates structured history across providers.\n\nEsc cancel"
		}
		if m.mode == modeResult {
			lines := strings.Split(wordWrap(m.result, max(1, m.width-4)), "\n")
			start := min(m.resultScroll, max(0, len(lines)-1))
			content = fitContent(strings.Join(lines[start:], "\n"), m.width-2, max(1, m.height-4)) + "\n\nj/k scroll · Enter or Esc return"
		}
		if m.mode != modeResult {
			content = wordWrap(content, m.width-4)
		}
		box := lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(th.borderFoc).Render(fitContent(content, m.width-2, m.height-2))
		v := tea.NewView(box)
		v.AltScreen = true
		return v
	}
	panelOuterHeight := m.height - 4
	panelInnerHeight := panelOuterHeight - 2

	projectOuter := m.width * 25 / 100
	sessionOuter := m.width * 30 / 100
	previewOuter := m.width - projectOuter - sessionOuter

	projectInner := clamp(projectOuter-2, 1, m.width)
	sessionInner := clamp(sessionOuter-2, 1, m.width)
	previewInner := clamp(previewOuter-2, 1, m.width)

	makeBorder := func(active bool) lipgloss.Style {
		bc := th.border
		if active {
			bc = th.borderFoc
		}
		return lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			BorderForeground(bc)
	}

	var panels string
	if m.width < 110 {
		inner := m.width - 2
		var content string
		switch m.activePanel {
		case panelProjects:
			content = m.renderProjectPanel(inner, panelInnerHeight)
		case panelSessions:
			content = m.renderSessionPanel(inner, panelInnerHeight)
		case panelPreview:
			content = m.renderPreviewPanel(inner, panelInnerHeight)
		}
		panels = makeBorder(true).Render(fitContent(content, inner, panelInnerHeight))
	} else {
		projectContent := fitContent(m.renderProjectPanel(projectInner, panelInnerHeight), projectInner, panelInnerHeight)
		sessionContent := fitContent(m.renderSessionPanel(sessionInner, panelInnerHeight), sessionInner, panelInnerHeight)
		previewContent := fitContent(m.renderPreviewPanel(previewInner, panelInnerHeight), previewInner, panelInnerHeight)
		projectBox := makeBorder(m.activePanel == panelProjects).Render(projectContent)
		sessionBox := makeBorder(m.activePanel == panelSessions).Render(sessionContent)
		previewBox := makeBorder(m.activePanel == panelPreview).Render(previewContent)
		panels = lipgloss.JoinHorizontal(lipgloss.Top, projectBox, sessionBox, previewBox)
	}

	titleStyle := lipgloss.NewStyle().
		Bold(true).
		Foreground(th.titleFg).
		Background(th.titleBg).
		Padding(0, 1)
	title := titleStyle.Render(fitContent(fmt.Sprintf("%s / Session Manager", m.providerName()), max(1, m.width-2), 1))
	context := fmt.Sprintf(" %d folders · %d chats", len(m.projects), len(m.currentSessions()))
	if len(m.projects) > 0 {
		context += " · " + singleLine(m.projects[m.projectCursor].Path)
	}
	context = lipgloss.NewStyle().Foreground(th.fgDim).Render(fitContent(context, m.width, 1))

	statusBar := m.renderStatusBar()

	view := lipgloss.JoinVertical(lipgloss.Left, title, context, panels, statusBar)

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

	return fitContent(lipgloss.Place(m.width, m.height, lipgloss.Center, lipgloss.Center, b.String()), m.width, m.height)
}

func (m model) renderProjectPanel(width, height int) string {
	th := m.th
	header := lipgloss.NewStyle().Foreground(th.accent).Bold(true).Render("FOLDERS")
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

	selectedStyle := lipgloss.NewStyle().Foreground(th.accent).Background(th.accentDim).Bold(true)
	normalStyle := lipgloss.NewStyle().Foreground(th.fg)
	countStyle := lipgloss.NewStyle().Foreground(th.fgDim)

	for i := startIdx; i < len(m.projects) && i-startIdx < maxVisible; i++ {
		proj := m.projects[i]
		display := shortenPath(proj.Path, width-8)
		count := countStyle.Render(fmt.Sprintf(" (%d)", len(proj.Sessions)))

		if i == m.projectCursor {
			lines = append(lines, selectedStyle.Render(fitContent(fmt.Sprintf("▸ %s (%d)", display, len(proj.Sessions)), width, 1)))
		} else {
			lines = append(lines, normalStyle.Render("  "+display)+count)
		}
	}

	return joinLines(lines, height)
}

func (m model) renderSessionPanel(width, height int) string {
	th := m.th
	header := lipgloss.NewStyle().Foreground(th.accent).Bold(true).Render("CHATS · space to mark")
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

	selectedStyle := lipgloss.NewStyle().Foreground(th.accent).Background(th.accentDim).Bold(true)
	normalStyle := lipgloss.NewStyle().Foreground(th.fg)
	ageStyle := lipgloss.NewStyle().Foreground(th.fgDim)

	for i := startIdx; i < len(sessions) && i-startIdx < maxVisible; i++ {
		sess := sessions[i]
		name := singleLine(sess.DisplayName())
		age := ageStyle.Render(" " + formatAge(sess.Timestamp))
		name = ansi.Truncate(name, max(1, width-lipgloss.Width(age)-2), "…")
		marker := "▸ "
		if m.marked[sess.FilePath+":"+sess.ID] {
			marker = "✓ "
		}

		if i == m.sessionCursor {
			lines = append(lines, selectedStyle.Render(fitContent(marker+name+" "+formatAge(sess.Timestamp), width, 1)))
		} else {
			if marker != "✓ " {
				marker = "  "
			}
			lines = append(lines, normalStyle.Render(marker+name)+age)
		}
	}

	return joinLines(lines, height)
}

func (m model) renderPreviewPanel(width, height int) string {
	th := m.th
	header := lipgloss.NewStyle().Foreground(th.accent).Bold(true).Render("CONVERSATION")
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

	msgLines := m.previewLines(width)

	scroll := min(m.previewScroll, max(0, len(msgLines)-max(1, height-len(lines))))
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

	first := fitContent(status, m.width, 1)
	second := " e export chat · c duplicate · space mark · tab panels"
	if m.activePanel == panelProjects {
		second = " e export folder · c duplicate folder · tab panels"
	}
	if m.mode != modeNormal {
		second = ""
	}
	return lipgloss.NewStyle().Foreground(th.statusFg).Background(th.statusBg).Render(first + "\n" + fitContent(second, m.width, 1))
}

// ── Helpers ──────────────────────────────────────────────────────────

func fitContent(s string, width, height int) string {
	width, height = max(0, width), max(0, height)
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
	path = singleLine(path)
	if maxLen < 5 {
		maxLen = 5
	}
	if lipgloss.Width(path) <= maxLen {
		return path
	}
	parts := strings.Split(path, "/")
	if len(parts) <= 2 {
		return ansi.Truncate(path, maxLen, "…")
	}
	short := "…/" + strings.Join(parts[len(parts)-2:], "/")
	if lipgloss.Width(short) > maxLen {
		return ansi.Truncate(short, maxLen, "…")
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
	return ansi.Hardwrap(terminalText(s), max(1, width), true)
}

// Strip terminal control sequences from stored chat content before rendering.
func terminalText(s string) string {
	s = strings.ReplaceAll(ansi.Strip(s), "\t", "    ")
	return strings.Map(func(r rune) rune {
		if r == '\n' || r >= 32 && r != 127 {
			return r
		}
		return -1
	}, s)
}

func singleLine(s string) string {
	return strings.Join(strings.Fields(terminalText(s)), " ")
}

func (m model) previewWidth() int {
	if m.width < 110 {
		return max(1, m.width-2)
	}
	return max(1, m.width-m.width*25/100-m.width*30/100-2)
}

func (m model) previewLines(width int) []string {
	if !m.hasSelectedSession() {
		return nil
	}
	var lines []string
	for _, msg := range m.selectedSession().Messages {
		label := lipgloss.NewStyle().Foreground(m.th.assistant).Bold(true).Render("◀ " + m.providerName())
		if msg.Role == "user" {
			label = lipgloss.NewStyle().Foreground(m.th.user).Bold(true).Render("▶ You")
		}
		lines = append(lines, label)
		for line := range strings.SplitSeq(wordWrap(msg.Content, max(1, width-2)), "\n") {
			lines = append(lines, "  "+line)
		}
		lines = append(lines, "")
	}
	return lines
}

func (m model) maxPreviewScroll() int {
	return max(0, len(m.previewLines(m.previewWidth()))-max(1, m.panelInnerHeight()-5))
}

func (m model) actionSessions() []Session {
	if m.activePanel == panelProjects {
		return append([]Session(nil), m.currentSessions()...)
	}
	var selected []Session
	for _, s := range m.currentSessions() {
		if m.marked[s.FilePath+":"+s.ID] {
			selected = append(selected, s)
		}
	}
	if len(selected) > 0 {
		return selected
	}
	if m.hasSelectedSession() {
		return []Session{m.selectedSession()}
	}
	return nil
}

func (m model) startDuplicate(target SessionProvider) (tea.Model, tea.Cmd) {
	sessions := m.actionSessions()
	m.busy = true
	m.mode = modeNormal
	return m, func() tea.Msg {
		home, err := os.UserHomeDir()
		if err != nil {
			return actionResultMsg{err: err}
		}
		var result strings.Builder
		fmt.Fprintf(&result, "Copied to %s. Resume from the original project directory.\n", target.Name())
		for i, s := range sessions {
			copy, err := duplicateSession(s, target, home)
			if err != nil {
				fmt.Fprintf(&result, "\nStopped after %d of %d chats: %v", i, len(sessions), err)
				return actionResultMsg{text: result.String()}
			}
			fmt.Fprintf(&result, "\n%s [%s]\n  %s\n", singleLine(s.DisplayName()), copy.Mode, copy.Resume)
			for _, warning := range copy.Warnings {
				fmt.Fprintf(&result, "  Notice: %s\n", singleLine(warning))
			}
		}
		return actionResultMsg{text: result.String()}
	}
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
	providerFlag := flag.String("provider", "", "Provider ID. Skips the picker.")
	importFlag := flag.Bool("import", false, "Restore native state or translate an exported ZIP.")
	harnessFlag := flag.String("harness", "", "Provider ID of the import destination.")
	flag.Parse()
	if *importFlag {
		if flag.NArg() != 1 || *harnessFlag == "" {
			fmt.Fprintln(os.Stderr, "Usage: agent-session-manager --import --harness=<provider> ./chats.zip")
			os.Exit(2)
		}
		home, err := os.UserHomeDir()
		if err == nil {
			err = importArchive(flag.Arg(0), *harnessFlag, home, os.Stdout)
		}
		if err != nil {
			fmt.Fprintln(os.Stderr, "Import failed:", err)
			os.Exit(1)
		}
		return
	}
	if *harnessFlag != "" || flag.NArg() != 0 {
		fmt.Fprintln(os.Stderr, "Use --import --harness=<provider> with a ZIP path")
		os.Exit(2)
	}

	m := initialModel(*providerFlag)
	p := tea.NewProgram(m)
	if _, err := p.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
}
