package agent

import (
	"slices"

	"github.com/charmbracelet/crush/internal/agent/tools"
	"github.com/charmbracelet/crush/internal/goal"
)

func effectiveGoalToolNames(configured []string, isSubAgent, goalEnabled bool, disabled []string) []string {
	allowed := slices.DeleteFunc(slices.Clone(configured), func(name string) bool {
		return name == tools.CompleteGoalToolName
	})
	if isSubAgent || !goalEnabled || slices.Contains(disabled, tools.CompleteGoalToolName) {
		return allowed
	}
	return append(allowed, tools.CompleteGoalToolName)
}

type noGoalCompleter struct{}

func (noGoalCompleter) Get() goal.Snapshot {
	return goal.Snapshot{}
}

func (noGoalCompleter) Complete(goal.Token, goal.Completion) (goal.Snapshot, error) {
	return goal.Snapshot{}, goal.ErrNoActiveGoal
}

func goalCompleter(service *goal.Service) tools.GoalCompleter {
	if service == nil {
		return noGoalCompleter{}
	}
	return service
}
