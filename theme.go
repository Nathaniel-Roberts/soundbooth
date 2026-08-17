package main

import "github.com/charmbracelet/lipgloss"

// Theme is a Catppuccin-style palette. All colours are hex strings.
type Theme struct {
	Name     string
	Base     string
	Surface0 string
	Overlay0 string
	Overlay1 string
	Subtext0 string
	Text     string
	Blue     string
	Lavender string
	Sapphire string
	Green    string
	Yellow   string
	Red      string
	Mauve    string
}

var themes = []Theme{
	{
		Name: "mocha", Base: "#1e1e2e", Surface0: "#313244",
		Overlay0: "#6c7086", Overlay1: "#7f849c", Subtext0: "#a6adc8",
		Text: "#cdd6f4", Blue: "#89b4fa", Lavender: "#b4befe",
		Sapphire: "#74c7ec", Green: "#a6e3a1", Yellow: "#f9e2af",
		Red: "#f38ba8", Mauve: "#cba6f7",
	},
	{
		Name: "macchiato", Base: "#24273a", Surface0: "#363a4f",
		Overlay0: "#6e738d", Overlay1: "#8087a2", Subtext0: "#a5adcb",
		Text: "#cad3f5", Blue: "#8aadf4", Lavender: "#b7bdf8",
		Sapphire: "#7dc4e4", Green: "#a6da95", Yellow: "#eed49f",
		Red: "#ed8796", Mauve: "#c6a0f6",
	},
	{
		Name: "frappe", Base: "#303446", Surface0: "#414559",
		Overlay0: "#737994", Overlay1: "#838ba7", Subtext0: "#a5adce",
		Text: "#c6d0f5", Blue: "#8caaee", Lavender: "#babbf1",
		Sapphire: "#85c1dc", Green: "#a6d189", Yellow: "#e5c890",
		Red: "#e78284", Mauve: "#ca9ee6",
	},
	{
		Name: "latte", Base: "#eff1f5", Surface0: "#ccd0da",
		Overlay0: "#9ca0b0", Overlay1: "#8c8fa1", Subtext0: "#6c6f85",
		Text: "#4c4f69", Blue: "#1e66f5", Lavender: "#7287fd",
		Sapphire: "#209fb5", Green: "#40a02b", Yellow: "#df8e1d",
		Red: "#d20f39", Mauve: "#8839ef",
	},
}

// th is the active theme; applyTheme rebuilds every style from it.
var th = themes[0]

func themeNames() []string {
	names := make([]string, len(themes))
	for i, t := range themes {
		names[i] = t.Name
	}
	return names
}

func themeByName(name string) Theme {
	for _, t := range themes {
		if t.Name == name {
			return t
		}
	}
	return themes[0]
}

// applyOverrides lets config pin individual colours, e.g. {"blue": "#0000ff"}.
func applyOverrides(t Theme, overrides map[string]string) Theme {
	for k, v := range overrides {
		switch k {
		case "base":
			t.Base = v
		case "surface0":
			t.Surface0 = v
		case "overlay0":
			t.Overlay0 = v
		case "overlay1":
			t.Overlay1 = v
		case "subtext0":
			t.Subtext0 = v
		case "text":
			t.Text = v
		case "blue":
			t.Blue = v
		case "lavender":
			t.Lavender = v
		case "sapphire":
			t.Sapphire = v
		case "green":
			t.Green = v
		case "yellow":
			t.Yellow = v
		case "red":
			t.Red = v
		case "mauve":
			t.Mauve = v
		}
	}
	return t
}

var (
	titleStyle lipgloss.Style
	dimStyle   lipgloss.Style
	labelStyle lipgloss.Style
	valueStyle lipgloss.Style
	focusStyle lipgloss.Style
	okStyle    lipgloss.Style
	warnStyle  lipgloss.Style
	errStyle   lipgloss.Style
	panelStyle lipgloss.Style
	keyStyle   lipgloss.Style
	descStyle  lipgloss.Style
)

func applyTheme(t Theme) {
	th = t
	titleStyle = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color(t.Mauve))
	dimStyle = lipgloss.NewStyle().Foreground(lipgloss.Color(t.Overlay0))
	labelStyle = lipgloss.NewStyle().Foreground(lipgloss.Color(t.Subtext0))
	valueStyle = lipgloss.NewStyle().Foreground(lipgloss.Color(t.Text))
	focusStyle = lipgloss.NewStyle().Foreground(lipgloss.Color(t.Blue)).Bold(true)
	okStyle = lipgloss.NewStyle().Foreground(lipgloss.Color(t.Green))
	warnStyle = lipgloss.NewStyle().Foreground(lipgloss.Color(t.Yellow))
	errStyle = lipgloss.NewStyle().Foreground(lipgloss.Color(t.Red))
	panelStyle = lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(lipgloss.Color(t.Surface0)).
		Padding(0, 1)
	keyStyle = lipgloss.NewStyle().Foreground(lipgloss.Color(t.Sapphire))
	descStyle = lipgloss.NewStyle().Foreground(lipgloss.Color(t.Overlay1))
}

func init() { applyTheme(themes[0]) }

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
