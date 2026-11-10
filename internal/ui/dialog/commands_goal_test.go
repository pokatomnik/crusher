package dialog

import (
	"testing"

	"github.com/charmbracelet/crush/internal/config"
	"github.com/charmbracelet/crush/internal/goal"
	"github.com/charmbracelet/crush/internal/ui/common"
	"github.com/charmbracelet/crush/internal/workspace"
	"github.com/stretchr/testify/require"
)

type goalCommandsWorkspace struct {
	workspace.Workspace
	cfg *config.Config
}

func (w *goalCommandsWorkspace) Config() *config.Config { return w.cfg }

func newGoalCommands(t *testing.T) *Commands {
	t.Helper()
	ws := &goalCommandsWorkspace{cfg: &config.Config{
		Options: &config.Options{SkillsPaths: []string{"skills"}, TUI: &config.TUIOptions{}},
	}}
	return newGoalCommandsWithCommon(t, common.DefaultCommon(ws))
}

func newGoalCommandsWithCommon(t *testing.T, com *common.Common) *Commands {
	t.Helper()
	commands, err := NewCommands(com, "", false, false, false, false, nil, nil)
	require.NoError(t, err)
	return commands
}

func commandByID(t *testing.T, commands *Commands, id string) (*CommandItem, int) {
	t.Helper()
	for i, item := range commands.list.Items() {
		if command, ok := item.(*CommandItem); ok && command.ID() == id {
			return command, i
		}
	}
	return nil, -1
}

func TestCommandsGoalItemAvailabilityAndPlacement(t *testing.T) {
	t.Parallel()

	commands := newGoalCommands(t)
	commands.SetGoalState(goal.Snapshot{}, true)
	set, goalIndex := commandByID(t, commands, "goal")
	_, skillsIndex := commandByID(t, commands, "toggle_skills")
	require.NotNil(t, set)
	require.Equal(t, skillsIndex+1, goalIndex)
	require.Equal(t, "Set Goal", set.title)
	require.Equal(t, ActionOpenDialog{DialogID: GoalID}, set.Action())

	pending := goal.Snapshot{WorkspaceID: "workspace", SessionID: "session", Revision: 4, Pending: true}
	commands.SetGoalState(pending, true)
	cancel, _ := commandByID(t, commands, "goal")
	require.NotNil(t, cancel)
	require.Equal(t, "Cancel Goal", cancel.title)
	require.Equal(t, ActionCancelGoal{ExpectedToken: goal.Token{
		WorkspaceID: "workspace",
		SessionID:   "session",
		Revision:    4,
	}}, cancel.Action())

	active := goal.Snapshot{WorkspaceID: "workspace", SessionID: "session", Revision: 5, Active: true}
	commands.SetGoalState(active, true)
	cancel, _ = commandByID(t, commands, "goal")
	require.NotNil(t, cancel)
	require.Equal(t, "Cancel Goal", cancel.title)
	require.Equal(t, goal.Token{WorkspaceID: "workspace", SessionID: "session", Revision: 5}, cancel.Action().(ActionCancelGoal).ExpectedToken)

	commands.SetGoalState(active, false)
	missing, _ := commandByID(t, commands, "goal")
	require.Nil(t, missing, "disabled complete_goal hides both Set and Cancel")
}

func TestCommandsGoalItemAppearsWithoutSkills(t *testing.T) {
	t.Parallel()

	ws := &goalCommandsWorkspace{cfg: &config.Config{
		Options: &config.Options{TUI: &config.TUIOptions{}},
	}}
	commands := newGoalCommandsWithCommon(t, common.DefaultCommon(ws))
	commands.SetGoalState(goal.Snapshot{}, true)

	set, _ := commandByID(t, commands, "goal")
	_, skillsIndex := commandByID(t, commands, "toggle_skills")
	require.NotNil(t, set)
	require.Equal(t, -1, skillsIndex)
	require.Equal(t, "Set Goal", set.title)
}

func TestCommandsGoalStateRefreshPreservesFilterAndSelection(t *testing.T) {
	t.Parallel()

	commands := newGoalCommands(t)
	commands.SetGoalState(goal.Snapshot{}, true)
	commands.input.SetValue("goal")
	commands.list.SetFilter("goal")
	commands.list.SetSelected(0)

	snapshot := goal.Snapshot{WorkspaceID: "ws", SessionID: "s", Revision: 8, Active: true}
	commands.SetGoalState(snapshot, true)

	require.Equal(t, "goal", commands.input.Value())

	selected, ok := commands.list.SelectedItem().(*CommandItem)
	require.True(t, ok)
	require.Equal(t, "goal", selected.ID())
	require.Equal(t, "Cancel Goal", selected.title)
	cancel, ok := selected.Action().(ActionCancelGoal)
	require.True(t, ok)
	require.Equal(t, goal.Token{WorkspaceID: "ws", SessionID: "s", Revision: 8}, cancel.ExpectedToken)
}
