package dialog

import (
	"image"
	"runtime"
	"strconv"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/crush/internal/ui/common"
	uv "github.com/charmbracelet/ultraviolet"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/require"
)

func goalConfirmKeyMsg() tea.KeyPressMsg {
	if runtime.GOOS == "darwin" {
		return tea.KeyPressMsg{Code: tea.KeyEnter, Mod: tea.ModSuper}
	}
	return tea.KeyPressMsg{Code: tea.KeyEnter, Mod: tea.ModCtrl}
}

func TestGoalConfirmBindingIsPlatformSpecific(t *testing.T) {
	t.Parallel()

	tests := []struct {
		goos       string
		shortcut   string
		help       string
		confirmMsg tea.KeyPressMsg
		otherMsg   tea.KeyPressMsg
	}{
		{
			goos:       "darwin",
			shortcut:   "super+enter",
			help:       "Cmd+Enter",
			confirmMsg: tea.KeyPressMsg{Code: tea.KeyEnter, Mod: tea.ModSuper},
			otherMsg:   tea.KeyPressMsg{Code: tea.KeyEnter, Mod: tea.ModCtrl},
		},
		{
			goos:       "linux",
			shortcut:   "ctrl+enter",
			help:       "Ctrl+Enter",
			confirmMsg: tea.KeyPressMsg{Code: tea.KeyEnter, Mod: tea.ModCtrl},
			otherMsg:   tea.KeyPressMsg{Code: tea.KeyEnter, Mod: tea.ModSuper},
		},
		{
			goos:       "windows",
			shortcut:   "ctrl+enter",
			help:       "Ctrl+Enter",
			confirmMsg: tea.KeyPressMsg{Code: tea.KeyEnter, Mod: tea.ModCtrl},
			otherMsg:   tea.KeyPressMsg{Code: tea.KeyEnter, Mod: tea.ModSuper},
		},
	}

	for _, tt := range tests {
		t.Run(tt.goos, func(t *testing.T) {
			t.Parallel()

			d := NewGoal(common.DefaultCommon(nil), "session-1")
			d.keyMap.Confirm = goalConfirmBinding(tt.goos)
			d.input.SetValue("ship the release")

			binding := d.keyMap.Confirm
			require.Equal(t, []string{tt.shortcut}, binding.Keys())
			require.Equal(t, tt.help, binding.Help().Key)

			_, confirmed := d.HandleMsg(tt.otherMsg).(ActionSetGoal)
			require.False(t, confirmed, "a non-platform modifier must not confirm the goal")
			require.Empty(t, d.validationError)

			d.input.SetValue("ship the release")
			action := d.HandleMsg(tt.confirmMsg)
			setGoal, ok := action.(ActionSetGoal)
			require.True(t, ok)
			require.Equal(t, "ship the release", setGoal.Text)
		})
	}
}

func TestGoalDialogConfirmAndCancel(t *testing.T) {
	t.Parallel()

	d := NewGoal(common.DefaultCommon(nil), "session-1")
	d.input.SetValue("first line\nsecond line")

	action := d.HandleMsg(goalConfirmKeyMsg())
	setGoal, ok := action.(ActionSetGoal)
	require.True(t, ok)
	require.Equal(t, "session-1", setGoal.SessionID)
	require.Equal(t, "first line\nsecond line", setGoal.Text)

	require.IsType(t, ActionClose{}, d.HandleMsg(tea.KeyPressMsg{Code: tea.KeyEscape}))

	help := d.ShortHelp()
	require.Equal(t, d.keyMap.Confirm.Help().Key, help[0].Help().Key)
	require.Equal(t, "- confirm", help[0].Help().Desc)
	require.Equal(t, "Esc", help[1].Help().Key)
	require.Equal(t, "- cancel", help[1].Help().Desc)
}

func TestGoalDialogEnterInsertsNewlineAndEmptyConfirmStaysOpen(t *testing.T) {
	t.Parallel()

	d := NewGoal(common.DefaultCommon(nil), "session-1")
	d.input.SetValue("first")
	require.Nil(t, d.HandleMsg(tea.KeyPressMsg{Code: tea.KeyEnter}))
	require.Equal(t, "first\n", d.input.Value())

	d.input.SetValue(" \n\t ")
	require.Nil(t, d.HandleMsg(goalConfirmKeyMsg()))
	require.Equal(t, "Enter a goal before confirming.", d.validationError)
	_, ok := d.HandleMsg(tea.KeyPressMsg{Code: tea.KeyEscape}).(ActionClose)
	require.True(t, ok)
}

func TestGoalDialogPasteAndFixedViewportPreserveLongInput(t *testing.T) {
	t.Parallel()

	d := NewGoal(common.DefaultCommon(nil), "session-1")
	content := strings.Repeat("line\n", 120) + "last line"
	d.HandleMsg(tea.PasteMsg{Content: content})
	require.Equal(t, content, d.input.Value())
	require.False(t, d.input.DynamicHeight)
	require.Zero(t, d.input.CharLimit)
	require.Zero(t, d.input.MaxHeight)
	require.Zero(t, d.input.MaxContentHeight)
	require.Zero(t, d.input.MaxWidth)

	scr := uv.NewScreenBuffer(70, 12)
	cursor := d.Draw(scr, image.Rect(0, 0, 70, 12))
	require.Equal(t, content, d.input.Value())
	require.Greater(t, d.input.ScrollYOffset(), 0, "the textarea should scroll when its content exceeds the viewport")
	require.NotNil(t, cursor, "the cursor should stay visible in the compact dialog")
}

func TestGoalDialogDrawClampsSmallAreasAndRefreshesStyles(t *testing.T) {
	t.Parallel()

	com := common.DefaultCommon(nil)
	d := NewGoal(com, "session-1")
	scr := uv.NewScreenBuffer(5, 3)
	d.Draw(scr, image.Rect(0, 0, 5, 3))

	newStyles := common.LoadThemeStyles("")
	com.Styles = &newStyles
	d.RefreshStyles()
	require.NotNil(t, d.input.Cursor())
}

func TestGoalDialogKeepsInputVisibleInTinyAreas(t *testing.T) {
	t.Parallel()

	for _, areaHeight := range []int{2, 3, 4} {
		t.Run(strconv.Itoa(areaHeight), func(t *testing.T) {
			t.Parallel()

			com := common.DefaultCommon(nil)
			d := NewGoal(com, "session-1")
			area := image.Rect(0, 0, 60, areaHeight)
			scr := uv.NewScreenBuffer(area.Dx(), area.Dy())
			cursor := d.Draw(scr, area)
			require.NotNil(t, cursor, "the input cursor must stay visible when the dialog is tiny")

			width := min(defaultDialogMaxWidth, area.Dx()-2)
			view, viewCursor := d.renderView(width, area.Dy()/2)
			require.NotNil(t, viewCursor)
			require.Contains(t, ansi.Strip(view), "Describe your goal...")
			require.LessOrEqual(t, lipgloss.Height(view), area.Dy()/2)
		})
	}
}

func TestGoalDialogHasPaddingAndUsesAtMostHalfTheScreenHeight(t *testing.T) {
	t.Parallel()

	com := common.DefaultCommon(nil)
	d := NewGoal(com, "session-1")
	area := image.Rect(0, 0, 90, 45)
	scr := uv.NewScreenBuffer(area.Dx(), area.Dy())
	d.Draw(scr, area)

	width := min(defaultDialogMaxWidth, area.Dx()-2)
	innerWidth := width - com.Styles.Dialog.View.GetHorizontalFrameSize()
	inputStyle := lipgloss.NewStyle().Padding(goalInputPadding).Width(innerWidth)
	require.Equal(t, innerWidth-inputStyle.GetHorizontalFrameSize(), d.input.Width(), "the textarea should leave room for horizontal padding")

	rc := NewRenderContext(com.Styles, width)
	rc.Title = "Set Goal"
	rc.AddPart(inputStyle.Render(d.input.View()))
	rc.Help = renderDialogHelp(com.Styles, &d.help, d, innerWidth)
	view := rc.Render()
	require.Equal(t, width, lipgloss.Width(view), "the dialog width should remain unchanged")
	require.LessOrEqual(t, lipgloss.Height(view), area.Dy()/2, "the rendered dialog should use no more than half the screen height")
}

func TestGoalDialogFitsCompactHeightsWithValidation(t *testing.T) {
	t.Parallel()

	const areaWidth = 90
	for _, areaHeight := range []int{5, 6, 7, 10, 12, 14, 16, 18} {
		t.Run(strconv.Itoa(areaHeight), func(t *testing.T) {
			d := NewGoal(common.DefaultCommon(nil), "session-1")
			d.HandleMsg(goalConfirmKeyMsg())

			width := min(defaultDialogMaxWidth, areaWidth-2)
			view, cursor := d.renderView(width, areaHeight/2)
			require.LessOrEqual(t, lipgloss.Height(view), areaHeight/2)
			require.NotNil(t, cursor, "the input cursor should remain visible")
			require.Contains(t, view, d.validationError)
			require.Contains(t, view, d.keyMap.Confirm.Help().Key)
			require.Contains(t, view, "Esc")
		})
	}
}

func TestGoalDialogTruncatesValidationErrorOnNarrowTerminal(t *testing.T) {
	t.Parallel()

	d := NewGoal(common.DefaultCommon(nil), "session-1")
	d.HandleMsg(goalConfirmKeyMsg())

	width := min(defaultDialogMaxWidth, 16-2)
	view, cursor := d.renderView(width, 14/2)
	require.LessOrEqual(t, lipgloss.Height(view), 14/2)
	require.NotNil(t, cursor)
}

func TestGoalDialogValidationMessageDoesNotShiftInputCursor(t *testing.T) {
	t.Parallel()

	d := NewGoal(common.DefaultCommon(nil), "session-1")
	d.HandleMsg(goalConfirmKeyMsg())

	view, cursor := d.renderView(defaultDialogMaxWidth, 7)
	require.NotNil(t, cursor)
	lines := strings.Split(ansi.Strip(view), "\n")
	require.Contains(t, lines[cursor.Y], "Describe your goal...")
}
