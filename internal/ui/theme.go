package ui

import "github.com/charmbracelet/lipgloss"

// Taro palette: violet accent with cool neutrals, adaptive light/dark.
var (
	colAccent = lipgloss.AdaptiveColor{Light: "#6C4FD8", Dark: "#A78BFA"}
	colDim    = lipgloss.AdaptiveColor{Light: "#8B8B98", Dark: "#6A6A78"}
	colUser   = lipgloss.AdaptiveColor{Light: "#1F8A5D", Dark: "#6EE7A8"}
	colErr    = lipgloss.AdaptiveColor{Light: "#D03050", Dark: "#FB7185"}
	colOK     = lipgloss.AdaptiveColor{Light: "#1F8A5D", Dark: "#6EE7A8"}
	colWarn   = lipgloss.AdaptiveColor{Light: "#B45309", Dark: "#FBBF24"}
	colBorder = lipgloss.AdaptiveColor{Light: "#D9D9E3", Dark: "#33333E"}
	colThink  = lipgloss.AdaptiveColor{Light: "#8E44AD", Dark: "#D8B4FE"}
	colTool   = lipgloss.AdaptiveColor{Light: "#1D6FB8", Dark: "#7CC7FF"}
	colCodeBg = lipgloss.AdaptiveColor{Light: "#F1F1F6", Dark: "#232330"}
)

// agentColors are cycled through to give each agent an identity color.
var agentColors = []lipgloss.AdaptiveColor{
	{Light: "#6C4FD8", Dark: "#A78BFA"}, // violet
	{Light: "#1D6FB8", Dark: "#7CC7FF"}, // blue
	{Light: "#0F766E", Dark: "#5EEAD4"}, // teal
	{Light: "#1F8A5D", Dark: "#6EE7A8"}, // green
	{Light: "#BE3B78", Dark: "#F9A8D4"}, // pink
	{Light: "#B45309", Dark: "#FBBF24"}, // amber
}

var (
	styAppTitle   = lipgloss.NewStyle().Bold(true).Foreground(colAccent)
	styDim        = lipgloss.NewStyle().Foreground(colDim)
	styErr        = lipgloss.NewStyle().Foreground(colErr)
	styOK         = lipgloss.NewStyle().Foreground(colOK)
	styWarn       = lipgloss.NewStyle().Foreground(colWarn)
	styUser       = lipgloss.NewStyle().Bold(true).Foreground(colUser)
	styThink      = lipgloss.NewStyle().Foreground(colThink).Italic(true)
	styTool       = lipgloss.NewStyle().Foreground(colTool)
	styCode       = lipgloss.NewStyle().Background(colCodeBg)
	styFlash      = lipgloss.NewStyle().Foreground(colAccent).Bold(true)
	styPanelTitle = lipgloss.NewStyle().Bold(true).Foreground(colAccent)
	styAccent     = lipgloss.NewStyle().Foreground(colAccent)

	styTabActive = lipgloss.NewStyle().Bold(true).Foreground(colAccent).Underline(true)
	styTabIdle   = lipgloss.NewStyle().Foreground(colDim)

	stySelected = lipgloss.NewStyle().Bold(true).Foreground(colAccent).Reverse(true)

	borderStyle = func(focused bool) lipgloss.Style {
		c := colBorder
		if focused {
			c = colAccent
		}
		return lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(c)
	}
)

func agentColor(id int) lipgloss.AdaptiveColor {
	return agentColors[(id-1+len(agentColors))%len(agentColors)]
}
