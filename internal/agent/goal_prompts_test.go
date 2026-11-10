package agent

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestGoalPromptsRenderUserTextAsData(t *testing.T) {
	t.Parallel()

	goalText := "Research {{.Goal}}\nsecond line — café"
	initial, err := GoalInitialPrompt(goalText)
	require.NoError(t, err)
	require.Contains(t, initial, goalText)
	require.Contains(t, initial, "research, analysis, planning, implementation")
	require.Contains(t, initial, "complete_goal")
	require.Contains(t, initial, "including for research and planning goals")

	followup, err := GoalFollowupPrompt(goalText)
	require.NoError(t, err)
	require.Contains(t, followup, goalText)
	require.Contains(t, followup, "automatic reminder")
	require.Contains(t, followup, "A report, a plan, or a ready marker does not close the goal")
	require.Contains(t, followup, "success=false")
	require.NotContains(t, strings.ToLower(followup), "work at any cost")
}
