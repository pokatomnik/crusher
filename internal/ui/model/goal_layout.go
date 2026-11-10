package model

import (
	"image"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/crush/internal/goal"
	"github.com/charmbracelet/crush/internal/ui/styles"
	uv "github.com/charmbracelet/ultraviolet"
	"github.com/charmbracelet/x/ansi"
)

const goalStripHeight = 3

func goalFrameStyle(t *styles.Styles, width int) (lipgloss.Style, int) {
	for padding := 2; padding >= 0; padding-- {
		style := t.Pills.Focused.Padding(0, padding)
		contentWidth := width - style.GetHorizontalFrameSize()
		if contentWidth > 0 {
			return style.Width(width), contentWidth
		}
	}
	return t.Pills.Focused.Padding(0, 0).Width(width), 0
}

func (m *UI) applyGoalLayout(layout uiLayout) uiLayout {
	layout.goal = image.Rectangle{}
	if !m.goal.snapshot.Active || (m.state != uiChat && m.state != uiLanding) {
		return layout
	}
	if _, contentWidth := goalFrameStyle(m.com.Styles, layout.editor.Dx()); contentWidth == 0 {
		return layout
	}

	goalBottom := layout.editor.Min.Y
	pills := layout.pills
	if m.state == uiChat && pills.Dy() > 0 {
		goalBottom = pills.Max.Y
	}
	goalTop := goalBottom - goalStripHeight
	if layout.main.Dy() < goalStripHeight || goalTop < layout.main.Min.Y || goalBottom > layout.editor.Min.Y {
		return layout
	}

	layout.main.Max.Y -= goalStripHeight
	if m.state == uiChat && pills.Dy() > 0 {
		layout.pills = image.Rect(pills.Min.X, pills.Min.Y-goalStripHeight, pills.Max.X, pills.Max.Y-goalStripHeight)
	}
	layout.goal = image.Rect(layout.editor.Min.X, goalTop, layout.editor.Max.X, goalBottom)
	return layout
}

func (m *UI) renderGoal(snapshot goal.Snapshot, width int) string {
	style, contentWidth := goalFrameStyle(m.com.Styles, width)
	if contentWidth <= 0 {
		return ""
	}

	prefix := "Current goal: "
	text := singleLine(ansi.Strip(snapshot.Text))
	var content string
	if ansi.StringWidth(prefix) > contentWidth {
		content = m.com.Styles.Pills.TodoLabel.Render(ansi.Truncate(prefix+text, contentWidth, "..."))
	} else {
		textWidth := contentWidth - ansi.StringWidth(prefix)
		content = m.com.Styles.Pills.TodoLabel.Render(prefix) +
			m.com.Styles.Pills.TodoCurrentTask.Render(ansi.Truncate(text, textWidth, "..."))
	}
	return style.Render(content)
}

func (m *UI) drawGoal(scr uv.Screen, area uv.Rectangle) {
	if !m.goal.snapshot.Active || area.Dx() <= 0 || area.Dy() < goalStripHeight {
		return
	}
	view := m.renderGoal(m.goal.snapshot, area.Dx())
	if view == "" {
		return
	}
	uv.NewStyledString(view).Draw(scr, area)
}
