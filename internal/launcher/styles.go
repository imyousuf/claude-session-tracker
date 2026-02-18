package launcher

import "github.com/charmbracelet/lipgloss"

var (
	// Colors
	activeColor   = lipgloss.Color("#00BFFF") // Cyan for active sessions
	inactiveColor = lipgloss.Color("#888888") // Gray for inactive
	selectedBg    = lipgloss.Color("#333366") // Highlight background
	headerColor   = lipgloss.Color("#FFD700") // Gold for header
	promptColor   = lipgloss.Color("#AAAAAA") // Light gray for prompts
	errorColor    = lipgloss.Color("#FF4444") // Red for errors
	hintColor     = lipgloss.Color("#666666") // Dim for hints
	previewBorder = lipgloss.Color("#444444") // Border for preview pane

	// Styles
	headerStyle = lipgloss.NewStyle().
			Bold(true).
			Foreground(headerColor).
			MarginBottom(1)

	activeStatusStyle = lipgloss.NewStyle().
				Bold(true).
				Foreground(activeColor)

	inactiveStatusStyle = lipgloss.NewStyle().
				Foreground(inactiveColor)

	selectedStyle = lipgloss.NewStyle().
			Background(selectedBg).
			Bold(true)

	promptStyle = lipgloss.NewStyle().
			Foreground(promptColor)

	timeStyle = lipgloss.NewStyle().
			Foreground(inactiveColor).
			Width(10)

	modelStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("#88AAFF")).
			Width(16)

	previewStyle = lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			BorderForeground(previewBorder).
			Padding(1, 2)

	previewHeaderStyle = lipgloss.NewStyle().
				Bold(true).
				Foreground(headerColor).
				MarginBottom(1)

	previewPromptStyle = lipgloss.NewStyle().
				Foreground(promptColor)

	previewTimeStyle = lipgloss.NewStyle().
				Foreground(inactiveColor).
				Width(10)

	hintStyle = lipgloss.NewStyle().
			Foreground(hintColor)

	errorStyle = lipgloss.NewStyle().
			Foreground(errorColor).
			Bold(true)

	statusBarStyle = lipgloss.NewStyle().
			Foreground(hintColor).
			MarginTop(1)
)
