package dialog

import (
	"strings"

	"charm.land/bubbles/v2/help"
	"charm.land/bubbles/v2/key"
	"charm.land/bubbles/v2/textarea"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	goalstate "github.com/charmbracelet/crush/internal/goal"
	"github.com/charmbracelet/crush/internal/ui/common"
	uv "github.com/charmbracelet/ultraviolet"
)

// GoalID identifies the goal editor dialog.
const GoalID = "goal"

// ActionSetGoal requests installation of a goal for the session captured when
// the dialog was opened. An empty SessionID means the UI was on the landing
// screen and must create a session before installing the goal.
type ActionSetGoal struct {
	SessionID string
	Text      string
}

// ActionCancelGoal requests cancellation of the goal identified by its
// snapshot token.
type ActionCancelGoal struct {
	ExpectedToken goalstate.Token
}

type goalCommandState struct {
	snapshot goalstate.Snapshot
	enabled  bool
}

// SetGoalState updates the goal item in the open command palette. It preserves
// the current filter and selected command when rebuilding the list.
func (c *Commands) SetGoalState(snapshot goalstate.Snapshot, enabled bool) {
	state := goalCommandState{snapshot: snapshot, enabled: enabled}
	if c.goalCommand == state {
		return
	}
	c.goalCommand = state
	if c.selected != SystemCommands || c.list == nil {
		return
	}

	filter := c.input.Value()
	selectedID := ""
	if item, ok := c.list.SelectedItem().(*CommandItem); ok && item != nil {
		selectedID = item.ID()
	}

	c.setCommandItems(c.selected)
	c.input.SetValue(filter)
	c.list.SetFilter(filter)
	if selectedID == "" {
		return
	}
	for i, item := range c.list.FilteredItems() {
		if command, ok := item.(*CommandItem); ok && command.ID() == selectedID {
			c.list.SetSelected(i)
			c.list.ScrollToSelected()
			return
		}
	}
}

func (c *Commands) goalCommandItem() *CommandItem {
	if !c.goalCommand.enabled {
		return nil
	}
	if c.goalCommand.snapshot.Active || c.goalCommand.snapshot.Pending {
		snapshot := c.goalCommand.snapshot
		return NewCommandItem(c.com.Styles, "goal", "Cancel Goal", "", ActionCancelGoal{
			ExpectedToken: goalstate.Token{
				WorkspaceID: snapshot.WorkspaceID,
				SessionID:   snapshot.SessionID,
				Revision:    snapshot.Revision,
			},
		})
	}
	return NewCommandItem(c.com.Styles, "goal", "Set Goal", "", ActionOpenDialog{DialogID: GoalID})
}

// Goal is a multiline editor for setting the current goal.
type Goal struct {
	com       *common.Common
	sessionID string
	input     textarea.Model
	help      help.Model
	keyMap    struct {
		Confirm key.Binding
		Close   key.Binding
	}
	validationError string
}

var (
	_ Dialog         = (*Goal)(nil)
	_ help.KeyMap    = (*Goal)(nil)
	_ StyleRefresher = (*Goal)(nil)
)

// NewGoal creates a focused goal editor for the specified session.
func NewGoal(com *common.Common, sessionID string) *Goal {
	input := textarea.New()
	input.SetStyles(com.Styles.Editor.Textarea)
	input.Placeholder = "Describe your goal..."
	input.ShowLineNumbers = false
	input.CharLimit = 0
	input.MaxWidth = 0
	input.MaxHeight = 0
	input.MaxContentHeight = 0
	input.DynamicHeight = false
	input.MinHeight = 1
	input.SetVirtualCursor(false)
	input.SetPromptFunc(0, func(textarea.PromptInfo) string { return "" })
	input.SetHeight(3)
	input.Focus()

	d := &Goal{
		com:       com,
		sessionID: sessionID,
		input:     input,
		help:      help.New(),
	}
	d.help.Styles = com.Styles.DialogHelpStyles()
	d.keyMap.Confirm = key.NewBinding(key.WithKeys("ctrl+d"), key.WithHelp("Ctrl+D", "- confirm"))
	d.closeKey()
	return d
}

func (d *Goal) closeKey() {
	closeKey := CloseKey
	closeKey.SetHelp("Esc", "- cancel")
	d.keyMap.Close = closeKey
}

// ID implements Dialog.
func (d *Goal) ID() string { return GoalID }

// HandleMsg implements Dialog.
func (d *Goal) HandleMsg(msg tea.Msg) Action {
	if keyMsg, ok := msg.(tea.KeyPressMsg); ok {
		switch {
		case key.Matches(keyMsg, d.keyMap.Close):
			return ActionClose{}
		case key.Matches(keyMsg, d.keyMap.Confirm):
			if strings.TrimSpace(d.input.Value()) == "" {
				d.validationError = "Enter a goal before confirming."
				return nil
			}
			return ActionSetGoal{SessionID: d.sessionID, Text: d.input.Value()}
		}
		d.validationError = ""
	}
	if _, ok := msg.(tea.PasteMsg); ok {
		d.validationError = ""
	}

	var cmd tea.Cmd
	d.input, cmd = d.input.Update(msg)
	if cmd == nil {
		return nil
	}
	return ActionCmd{Cmd: cmd}
}

// ShortHelp implements help.KeyMap.
func (d *Goal) ShortHelp() []key.Binding {
	return []key.Binding{d.keyMap.Confirm, d.keyMap.Close}
}

// FullHelp implements help.KeyMap.
func (d *Goal) FullHelp() [][]key.Binding {
	return [][]key.Binding{{d.keyMap.Confirm, d.keyMap.Close}}
}

// RefreshStyles implements StyleRefresher.
func (d *Goal) RefreshStyles() {
	d.help.Styles = d.com.Styles.DialogHelpStyles()
	d.input.SetStyles(d.com.Styles.Editor.Textarea)
}

// Draw implements Dialog.
func (d *Goal) Draw(scr uv.Screen, area uv.Rectangle) *tea.Cursor {
	t := d.com.Styles
	width := max(0, min(defaultDialogMaxWidth, area.Dx()-2))
	height := max(0, area.Dy()-2)
	viewStyle := t.Dialog.View.Width(width)
	innerWidth := max(0, width-viewStyle.GetHorizontalFrameSize())

	heightOffset := viewStyle.GetVerticalFrameSize() +
		t.Dialog.Title.GetVerticalFrameSize() + titleContentHeight +
		t.Dialog.HelpView.GetVerticalFrameSize() + 1
	if d.validationError != "" {
		heightOffset++
	}
	d.input.SetWidth(innerWidth)
	d.input.SetHeight(max(1, height-heightOffset))

	rc := NewRenderContext(t, width)
	rc.Title = "Set Goal"
	rc.AddPart(d.input.View())
	if d.validationError != "" {
		rc.AddPart(t.Dialog.TitleError.Width(innerWidth).Render(d.validationError))
	}
	rc.Help = renderDialogHelp(t, &d.help, d, innerWidth)
	view := rc.Render()

	cursor := d.input.Cursor()
	viewWidth, viewHeight := lipgloss.Size(view)
	if cursor != nil {
		cursor.X += viewStyle.GetBorderLeftSize() + viewStyle.GetPaddingLeft() + viewStyle.GetMarginLeft()
		cursor.Y += viewStyle.GetBorderTopSize() + viewStyle.GetPaddingTop() + viewStyle.GetMarginTop() +
			t.Dialog.Title.GetVerticalFrameSize() + titleContentHeight
		if cursor.X < 0 || cursor.X >= viewWidth || cursor.Y < 0 || cursor.Y >= viewHeight || viewHeight > area.Dy() {
			cursor = nil
		}
	}
	DrawCenterCursor(scr, area, view, cursor)
	return cursor
}
