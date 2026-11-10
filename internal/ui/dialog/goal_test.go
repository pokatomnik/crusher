package dialog

import (
	"image"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/crush/internal/ui/common"
	uv "github.com/charmbracelet/ultraviolet"
	"github.com/stretchr/testify/require"
)

func TestGoalDialogConfirmAndCancel(t *testing.T) {
	t.Parallel()

	d := NewGoal(common.DefaultCommon(nil), "session-1")
	d.input.SetValue("first line\nsecond line")

	action := d.HandleMsg(tea.KeyPressMsg{Code: 'd', Mod: tea.ModCtrl})
	setGoal, ok := action.(ActionSetGoal)
	require.True(t, ok)
	require.Equal(t, "session-1", setGoal.SessionID)
	require.Equal(t, "first line\nsecond line", setGoal.Text)

	require.IsType(t, ActionClose{}, d.HandleMsg(tea.KeyPressMsg{Code: tea.KeyEscape}))

	help := d.ShortHelp()
	require.Equal(t, "Ctrl+D", help[0].Help().Key)
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
	require.Nil(t, d.HandleMsg(tea.KeyPressMsg{Code: 'd', Mod: tea.ModCtrl}))
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
	d.Draw(scr, image.Rect(0, 0, 70, 12))
	require.Equal(t, content, d.input.Value())
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

func TestGoalDialogUsesAvailableVerticalSpace(t *testing.T) {
	t.Parallel()

	d := NewGoal(common.DefaultCommon(nil), "session-1")
	scr := uv.NewScreenBuffer(70, 45)
	d.Draw(scr, image.Rect(0, 0, 70, 45))
	require.Greater(t, d.input.Height(), defaultDialogHeight, "the goal textarea should fill the available dialog area")
}
