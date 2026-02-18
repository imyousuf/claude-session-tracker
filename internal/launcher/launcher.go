package launcher

import (
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/key"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/imyousuf/claude-session-tracker/internal/procutil"
	"github.com/imyousuf/claude-session-tracker/internal/store"
)

// Result holds the outcome of the TUI session picker.
type Result struct {
	SessionID string
	Project   string
}

type keyMap struct {
	Up     key.Binding
	Down   key.Binding
	Enter  key.Binding
	Tab    key.Binding
	Delete key.Binding
	Quit   key.Binding
	Search key.Binding
}

var keys = keyMap{
	Up:     key.NewBinding(key.WithKeys("up", "k"), key.WithHelp("↑/k", "up")),
	Down:   key.NewBinding(key.WithKeys("down", "j"), key.WithHelp("↓/j", "down")),
	Enter:  key.NewBinding(key.WithKeys("enter"), key.WithHelp("enter", "resume")),
	Tab:    key.NewBinding(key.WithKeys("tab"), key.WithHelp("tab", "toggle all/project")),
	Delete: key.NewBinding(key.WithKeys("d"), key.WithHelp("d", "delete")),
	Quit:   key.NewBinding(key.WithKeys("q", "esc", "ctrl+c"), key.WithHelp("q/esc", "quit")),
	Search: key.NewBinding(key.WithKeys("/"), key.WithHelp("/", "search")),
}

// Model is the Bubbletea model for the session picker TUI.
type Model struct {
	store      *store.Store
	sessions   []store.Session
	prompts    []store.Prompt
	cursor     int
	project    string
	showAll    bool
	width      int
	height     int
	err        error
	result     *Result
	statusMsg  string
	searching  bool
	searchText string
	filtered   []int // indices into sessions
	confirming bool  // delete confirmation
}

// New creates a new launcher Model.
func New(s *store.Store, project string, showAll bool) Model {
	return Model{
		store:   s,
		project: project,
		showAll: showAll,
	}
}

type sessionsLoaded struct {
	sessions []store.Session
	err      error
}

type promptsLoaded struct {
	prompts []store.Prompt
}

func loadSessions(s *store.Store, project string, showAll bool) tea.Cmd {
	return func() tea.Msg {
		// Refresh active sessions first
		s.RefreshActive(procutil.IsProcessAlive)

		var sessions []store.Session
		var err error
		if showAll || project == "" {
			sessions, err = s.ListAll()
		} else {
			sessions, err = s.ListByProject(project)
		}
		return sessionsLoaded{sessions: sessions, err: err}
	}
}

func loadPrompts(s *store.Store, sessionID string) tea.Cmd {
	return func() tea.Msg {
		prompts, _ := s.GetPrompts(sessionID, 10)
		return promptsLoaded{prompts: prompts}
	}
}

// Init implements tea.Model.
func (m Model) Init() tea.Cmd {
	return loadSessions(m.store, m.project, m.showAll)
}

// Update implements tea.Model.
func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		return m, nil

	case sessionsLoaded:
		m.sessions = msg.sessions
		m.err = msg.err
		m.buildFilter()
		if len(m.filtered) > 0 {
			return m, loadPrompts(m.store, m.sessions[m.filtered[0]].ID)
		}
		return m, nil

	case promptsLoaded:
		m.prompts = msg.prompts
		return m, nil

	case tea.KeyMsg:
		return m.handleKey(msg)
	}
	return m, nil
}

func (m Model) handleKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	// Handle search mode input
	if m.searching {
		switch {
		case key.Matches(msg, key.NewBinding(key.WithKeys("esc"))):
			m.searching = false
			m.searchText = ""
			m.buildFilter()
			return m, nil
		case key.Matches(msg, key.NewBinding(key.WithKeys("enter"))):
			m.searching = false
			return m, nil
		case key.Matches(msg, key.NewBinding(key.WithKeys("backspace"))):
			if len(m.searchText) > 0 {
				m.searchText = m.searchText[:len(m.searchText)-1]
				m.buildFilter()
			}
			return m, nil
		default:
			if len(msg.String()) == 1 {
				m.searchText += msg.String()
				m.buildFilter()
			}
			return m, nil
		}
	}

	// Handle delete confirmation
	if m.confirming {
		switch msg.String() {
		case "y", "Y":
			m.confirming = false
			if len(m.filtered) > 0 {
				idx := m.filtered[m.cursor]
				sess := m.sessions[idx]
				if err := m.store.DeleteSession(sess.ID); err != nil {
					m.statusMsg = "Error deleting: " + err.Error()
				} else {
					m.statusMsg = "Deleted session " + sess.ID[:8]
				}
				return m, loadSessions(m.store, m.project, m.showAll)
			}
			return m, nil
		default:
			m.confirming = false
			m.statusMsg = ""
			return m, nil
		}
	}

	m.statusMsg = ""

	switch {
	case key.Matches(msg, keys.Quit):
		return m, tea.Quit

	case key.Matches(msg, keys.Up):
		if m.cursor > 0 {
			m.cursor--
			return m, loadPrompts(m.store, m.sessions[m.filtered[m.cursor]].ID)
		}

	case key.Matches(msg, keys.Down):
		if m.cursor < len(m.filtered)-1 {
			m.cursor++
			return m, loadPrompts(m.store, m.sessions[m.filtered[m.cursor]].ID)
		}

	case key.Matches(msg, keys.Enter):
		if len(m.filtered) == 0 {
			return m, nil
		}
		idx := m.filtered[m.cursor]
		sess := m.sessions[idx]
		if sess.Active {
			m.statusMsg = "Cannot resume an active session"
			return m, nil
		}
		m.result = &Result{SessionID: sess.ID, Project: sess.Project}
		return m, tea.Quit

	case key.Matches(msg, keys.Tab):
		m.showAll = !m.showAll
		m.cursor = 0
		return m, loadSessions(m.store, m.project, m.showAll)

	case key.Matches(msg, keys.Delete):
		if len(m.filtered) > 0 {
			idx := m.filtered[m.cursor]
			sess := m.sessions[idx]
			if sess.Active {
				m.statusMsg = "Cannot delete an active session"
				return m, nil
			}
			m.confirming = true
			m.statusMsg = fmt.Sprintf("Delete session %s? (y/N)", sess.ID[:8])
		}

	case key.Matches(msg, keys.Search):
		m.searching = true
		m.searchText = ""
	}

	return m, nil
}

func (m *Model) buildFilter() {
	m.filtered = nil
	search := strings.ToLower(m.searchText)
	for i, sess := range m.sessions {
		if search != "" {
			text := strings.ToLower(sess.LastPrompt + " " + sess.Project + " " + sess.Model)
			if !strings.Contains(text, search) {
				continue
			}
		}
		m.filtered = append(m.filtered, i)
	}
	if m.cursor >= len(m.filtered) {
		m.cursor = max(0, len(m.filtered)-1)
	}
}

// View implements tea.Model.
func (m Model) View() string {
	if m.err != nil {
		return errorStyle.Render("Error: " + m.err.Error())
	}

	if m.width == 0 {
		return "Loading..."
	}

	var b strings.Builder

	// Header
	title := "Claude Code Sessions"
	if !m.showAll && m.project != "" {
		title += "  " + hintStyle.Render(m.project)
	} else if m.showAll {
		title += "  " + hintStyle.Render("(all projects)")
	}
	b.WriteString(headerStyle.Render(title))
	b.WriteString("\n")

	if len(m.filtered) == 0 {
		b.WriteString(hintStyle.Render("No sessions found."))
		if !m.showAll {
			b.WriteString("\n" + hintStyle.Render("Press Tab to show all projects."))
		}
		b.WriteString("\n\n")
		b.WriteString(m.renderHints())
		return b.String()
	}

	// Calculate pane widths
	previewWidth := min(m.width/2, 60)
	listWidth := m.width - previewWidth - 3 // 3 for separator

	// Build list pane
	listContent := m.renderList(listWidth)

	// Build preview pane
	previewContent := m.renderPreview(previewWidth)

	// Join horizontally
	joined := lipgloss.JoinHorizontal(lipgloss.Top,
		listContent,
		"  ",
		previewContent,
	)
	b.WriteString(joined)
	b.WriteString("\n")

	// Status / search bar
	if m.searching {
		b.WriteString(fmt.Sprintf("Search: %s█", m.searchText))
	} else if m.statusMsg != "" {
		if m.confirming {
			b.WriteString(errorStyle.Render(m.statusMsg))
		} else {
			b.WriteString(hintStyle.Render(m.statusMsg))
		}
	}
	b.WriteString("\n")

	// Hints
	b.WriteString(m.renderHints())

	return b.String()
}

func (m Model) renderList(width int) string {
	var lines []string
	availableHeight := m.height - 6 // header + hints + margins

	// Calculate visible window
	start := 0
	if m.cursor >= availableHeight {
		start = m.cursor - availableHeight + 1
	}
	end := min(start+availableHeight, len(m.filtered))

	for i := start; i < end; i++ {
		idx := m.filtered[i]
		sess := m.sessions[idx]
		line := m.renderSessionLine(sess, width)
		if i == m.cursor {
			line = selectedStyle.Render(line)
		}
		lines = append(lines, line)
	}

	// Scroll indicators
	if start > 0 {
		lines = append([]string{hintStyle.Render("  ↑ more")}, lines...)
	}
	if end < len(m.filtered) {
		lines = append(lines, hintStyle.Render("  ↓ more"))
	}

	return strings.Join(lines, "\n")
}

func (m Model) renderSessionLine(sess store.Session, width int) string {
	var status string
	if sess.Active {
		status = activeStatusStyle.Render("● ACTIVE")
	} else {
		status = inactiveStatusStyle.Render("○ idle  ")
	}

	relTime := FormatRelativeTime(sess.LastActivity)
	model := shortModel(sess.Model)

	// Prompt text gets remaining space
	promptWidth := width - 10 - 16 - 10 // status + time + model
	if promptWidth < 10 {
		promptWidth = 10
	}
	prompt := sess.LastPrompt
	if prompt == "" {
		prompt = "(no prompts yet)"
	}
	if len(prompt) > promptWidth {
		prompt = prompt[:promptWidth-3] + "..."
	}

	return fmt.Sprintf("  %s %s %s %s",
		status,
		timeStyle.Render(relTime),
		modelStyle.Render(model),
		promptStyle.Render(prompt),
	)
}

func (m Model) renderPreview(width int) string {
	if len(m.filtered) == 0 {
		return ""
	}

	idx := m.filtered[m.cursor]
	sess := m.sessions[idx]

	var lines []string

	// Session header
	idShort := sess.ID
	if len(idShort) > 8 {
		idShort = idShort[:8]
	}
	lines = append(lines, previewHeaderStyle.Render(fmt.Sprintf("Session %s", idShort)))
	lines = append(lines, fmt.Sprintf("Project: %s", sess.Project))
	lines = append(lines, fmt.Sprintf("CWD:     %s", sess.CWD))
	lines = append(lines, fmt.Sprintf("Model:   %s", sess.Model))
	lines = append(lines, fmt.Sprintf("Started: %s", formatAbsoluteTime(sess.StartedAt)))
	lines = append(lines, fmt.Sprintf("Active:  %s", formatAbsoluteTime(sess.LastActivity)))
	lines = append(lines, "")

	// Prompts
	if len(m.prompts) > 0 {
		lines = append(lines, previewHeaderStyle.Render("Recent prompts:"))
		for _, p := range m.prompts {
			relTime := FormatRelativeTime(p.Timestamp)
			text := p.Text
			maxLen := width - 14
			if maxLen < 10 {
				maxLen = 10
			}
			if len(text) > maxLen {
				text = text[:maxLen-3] + "..."
			}
			lines = append(lines, fmt.Sprintf("  %s  %s",
				previewTimeStyle.Render(relTime),
				previewPromptStyle.Render(text),
			))
		}
	} else {
		lines = append(lines, hintStyle.Render("No prompts recorded"))
	}

	content := strings.Join(lines, "\n")
	return previewStyle.Width(width).Render(content)
}

func (m Model) renderHints() string {
	hints := []string{
		keys.Up.Help().Key + "/" + keys.Down.Help().Key + " navigate",
		keys.Enter.Help().Key + " resume",
		keys.Tab.Help().Key + " toggle scope",
		keys.Search.Help().Key + " search",
		keys.Delete.Help().Key + " delete",
		keys.Quit.Help().Key + " quit",
	}
	return statusBarStyle.Render(strings.Join(hints, "  │  "))
}

// GetResult returns the selected session, or nil if the user quit without selecting.
func (m Model) GetResult() *Result {
	return m.result
}

// --- Formatting helpers ---

// FormatRelativeTime formats a millisecond timestamp as a relative time string.
func FormatRelativeTime(tsMs int64) string {
	if tsMs == 0 {
		return "never"
	}
	d := time.Since(time.UnixMilli(tsMs))
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds ago", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	}
}

func formatAbsoluteTime(tsMs int64) string {
	if tsMs == 0 {
		return "unknown"
	}
	return time.UnixMilli(tsMs).Format("2006-01-02 15:04")
}

func shortModel(model string) string {
	// "claude-sonnet-4-6" -> "sonnet-4-6"
	model = strings.TrimPrefix(model, "claude-")
	if len(model) > 14 {
		model = model[:14]
	}
	return model
}
