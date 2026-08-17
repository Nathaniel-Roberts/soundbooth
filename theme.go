package main

import "github.com/charmbracelet/lipgloss"

// Catppuccin Mocha palette.
var (
	mochaBase     = lipgloss.Color("#1e1e2e")
	mochaSurface0 = lipgloss.Color("#313244")
	mochaOverlay0 = lipgloss.Color("#6c7086")
	mochaOverlay1 = lipgloss.Color("#7f849c")
	mochaSubtext0 = lipgloss.Color("#a6adc8")
	mochaText     = lipgloss.Color("#cdd6f4")
	mochaBlue     = lipgloss.Color("#89b4fa")
	mochaLavender = lipgloss.Color("#b4befe")
	mochaSapphire = lipgloss.Color("#74c7ec")
	mochaGreen    = lipgloss.Color("#a6e3a1")
	mochaYellow   = lipgloss.Color("#f9e2af")
	mochaRed      = lipgloss.Color("#f38ba8")
	mochaMauve    = lipgloss.Color("#cba6f7")
)

var (
	titleStyle = lipgloss.NewStyle().Bold(true).Foreground(mochaMauve)
	dimStyle   = lipgloss.NewStyle().Foreground(mochaOverlay0)
	labelStyle = lipgloss.NewStyle().Foreground(mochaSubtext0)
	valueStyle = lipgloss.NewStyle().Foreground(mochaText)
	focusStyle = lipgloss.NewStyle().Foreground(mochaBlue).Bold(true)
	okStyle    = lipgloss.NewStyle().Foreground(mochaGreen)
	warnStyle  = lipgloss.NewStyle().Foreground(mochaYellow)
	errStyle   = lipgloss.NewStyle().Foreground(mochaRed)

	waveEnvStyle  = lipgloss.NewStyle().Foreground(mochaBlue)
	waveCoreStyle = lipgloss.NewStyle().Foreground(mochaLavender)
	waveClipStyle = lipgloss.NewStyle().Foreground(mochaRed)
	waveMidStyle  = lipgloss.NewStyle().Foreground(mochaOverlay0)

	panelStyle = lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			BorderForeground(mochaSurface0).
			Padding(0, 1)

	keyStyle  = lipgloss.NewStyle().Foreground(mochaSapphire)
	descStyle = lipgloss.NewStyle().Foreground(mochaOverlay1)
)

func keyHint(pairs ...string) string {
	out := ""
	for i := 0; i+1 < len(pairs); i += 2 {
		if i > 0 {
			out += dimStyle.Render("  ·  ")
		}
		out += keyStyle.Render(pairs[i]) + " " + descStyle.Render(pairs[i+1])
	}
	return out
}
