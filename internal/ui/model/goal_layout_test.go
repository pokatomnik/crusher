package model

import (
	"image"
	"strings"
	"testing"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/crush/internal/goal"
	"github.com/charmbracelet/crush/internal/session"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/require"
)

func TestGenerateLayoutReservesThreeRowGoalStripWithoutOverlap(t *testing.T) {
	t.Parallel()

	ui := newTestUI()
	ui.session = &session.Session{ID: "session", Todos: []session.Todo{{Status: session.TodoStatusPending, Content: "work"}}}
	ui.width = 140
	ui.height = 45
	baseline := ui.generateLayout(ui.width, ui.height)
	require.NotEmpty(t, baseline.pills)

	ui.goal.snapshot = goal.Snapshot{Active: true, Text: "complete the task"}
	layout := ui.generateLayout(ui.width, ui.height)

	require.Equal(t, goalStripHeight, layout.goal.Dy())
	require.Equal(t, layout.editor.Min.Y, layout.goal.Max.Y)
	require.Equal(t, baseline.editor, layout.editor)
	require.Equal(t, baseline.status, layout.status)
	require.Equal(t, baseline.pills.Dy(), layout.pills.Dy())
	require.Equal(t, baseline.pills.Min.Y-goalStripHeight, layout.pills.Min.Y)
	require.Equal(t, layout.goal.Min.Y, layout.pills.Max.Y)
	require.LessOrEqual(t, layout.main.Max.Y, layout.pills.Min.Y)
	require.True(t, layout.goal.Intersect(layout.editor).Empty())
	require.True(t, layout.goal.Intersect(layout.status).Empty())
	require.Equal(t, layout.editor.Min.X, layout.goal.Min.X)
	require.Equal(t, layout.editor.Max.X, layout.goal.Max.X)
}

func TestGoalStripLandingAndResizeVisibility(t *testing.T) {
	t.Parallel()

	ui := newTestUI()
	ui.state = uiLanding
	ui.goal.snapshot = goal.Snapshot{Active: true, Text: "keep working"}

	landing := ui.generateLayout(140, 45)
	require.Equal(t, goalStripHeight, landing.goal.Dy())
	require.True(t, landing.goal.Intersect(landing.editor).Empty())
	require.True(t, landing.goal.Intersect(landing.status).Empty())

	tooShort := ui.generateLayout(140, 10)
	require.True(t, tooShort.goal.Empty(), "the strip is hidden when there is not room for all three rows")
	require.True(t, ui.goal.snapshot.Active, "hiding the strip does not change goal state")

	resized := ui.generateLayout(140, 45)
	require.Equal(t, goalStripHeight, resized.goal.Dy(), "the strip returns when the terminal has room")
	require.True(t, ui.goal.snapshot.Active)
}

func TestApplyGoalLayoutHidesWhenWidthOrHistoryIsInsufficient(t *testing.T) {
	t.Parallel()

	ui := newTestUI()
	ui.goal.snapshot = goal.Snapshot{Active: true, Text: "goal"}
	layout := uiLayout{
		main:   image.Rect(1, 1, 20, 3),
		editor: image.Rect(1, 6, 20, 8),
	}
	got := ui.applyGoalLayout(layout)
	require.True(t, got.goal.Empty(), "a two-row history region cannot fit the strip")
	require.Equal(t, layout.main, got.main)

	layout.main = image.Rect(1, 1, 20, 6)
	layout.editor = image.Rect(1, 8, 2, 10)
	got = ui.applyGoalLayout(layout)
	require.True(t, got.goal.Empty(), "a one-column editor region cannot fit a bordered strip")
	require.Equal(t, layout.main, got.main)
}

func TestRenderGoalUsesThreeRowsAndANSIWidthAwareTruncation(t *testing.T) {
	t.Parallel()

	ui := newTestUI()
	normalStyle, contentWidth := goalFrameStyle(ui.com.Styles, 48)
	require.Equal(t, 2, normalStyle.GetPaddingLeft())
	require.Zero(t, normalStyle.GetPaddingTop())
	require.Zero(t, normalStyle.GetPaddingBottom())
	require.Positive(t, contentWidth)

	snapshot := goal.Snapshot{
		Text: "\x1b[31m界🙂 finish a very long multi-line goal\x1b[0m\nthen continue",
	}
	view := ui.renderGoal(snapshot, 32)
	width, height := lipgloss.Size(view)
	plain := ansi.Strip(view)
	require.Equal(t, goalStripHeight, height)
	require.LessOrEqual(t, width, 32)
	require.Contains(t, plain, "Current goal: ")
	require.Contains(t, plain, "...")
	require.NotContains(t, plain, "\x1b[")
	require.NotContains(t, plain, "\nthen continue")

	narrowStyle, narrowContentWidth := goalFrameStyle(ui.com.Styles, 4)
	require.Zero(t, narrowStyle.GetPaddingLeft(), "horizontal padding shrinks before text is truncated")
	require.Positive(t, narrowContentWidth)
	narrowView := ui.renderGoal(goal.Snapshot{Text: strings.Repeat("界", 20)}, 4)
	narrowWidth, narrowHeight := lipgloss.Size(narrowView)
	require.LessOrEqual(t, narrowWidth, 4)
	require.Equal(t, goalStripHeight, narrowHeight)

	_, tinyContentWidth := goalFrameStyle(ui.com.Styles, 2)
	require.Zero(t, tinyContentWidth)
	require.Empty(t, ui.renderGoal(goal.Snapshot{Text: "hidden"}, 2))
}
