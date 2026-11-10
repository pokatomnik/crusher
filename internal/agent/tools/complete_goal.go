package tools

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"

	"charm.land/fantasy"
	"github.com/charmbracelet/crush/internal/goal"
)

//go:embed complete_goal.md
var completeGoalDescription string

// CompleteGoalToolName is the stable name of the main-agent-only tool.
const CompleteGoalToolName = "complete_goal"

// CompleteGoalParams are the model-provided outcome fields.
type CompleteGoalParams struct {
	Result  string `json:"result" jsonschema:"required" description:"A concise outcome or blocker"`
	Success bool   `json:"success" jsonschema:"required" description:"Whether the goal was achieved"`
}

// UnmarshalJSON validates required fields after PreToolUse hooks have had a
// chance to rewrite the input.
func (p *CompleteGoalParams) UnmarshalJSON(data []byte) error {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return err
	}
	resultJSON, ok := fields["result"]
	if !ok || string(resultJSON) == "null" {
		return fmt.Errorf("result is required and must be a string")
	}
	if err := json.Unmarshal(resultJSON, &p.Result); err != nil {
		return fmt.Errorf("result must be a string: %w", err)
	}
	successJSON, ok := fields["success"]
	if !ok || string(successJSON) == "null" {
		return fmt.Errorf("success is required and must be a boolean")
	}
	if err := json.Unmarshal(successJSON, &p.Success); err != nil {
		return fmt.Errorf("success must be a boolean: %w", err)
	}
	return nil
}

// GoalCompleter is the narrow RAM-service contract consumed by this tool.
type GoalCompleter interface {
	Get() goal.Snapshot
	Complete(goal.Token, goal.Completion) (goal.Snapshot, error)
}

// NewCompleteGoalTool creates the completion tool. Its registration is
// controlled by the coordinator, not by goal state.
func NewCompleteGoalTool(completer GoalCompleter) fantasy.AgentTool {
	return fantasy.NewAgentTool(
		CompleteGoalToolName,
		completeGoalDescription,
		func(ctx context.Context, params CompleteGoalParams, _ fantasy.ToolCall) (fantasy.ToolResponse, error) {
			const noGoal = "No current goal is set. Nothing was changed."
			snapshot := completer.Get()
			if !snapshot.Active {
				return fantasy.NewTextResponse(noGoal), nil
			}

			token, ok := goal.TrustedTokenFromContext(ctx)
			if !ok || token.WorkspaceID != snapshot.WorkspaceID || token.SessionID != snapshot.SessionID || token.Revision != snapshot.Revision || GetSessionFromContext(ctx) != token.SessionID {
				return fantasy.NewTextErrorResponse("The goal context is stale or does not belong to this session; nothing was changed."), nil
			}

			_, err := completer.Complete(token, goal.Completion{Result: params.Result, Success: params.Success})
			if err != nil {
				if errors.Is(err, goal.ErrNoActiveGoal) {
					return fantasy.NewTextResponse(noGoal), nil
				}
				return fantasy.NewTextErrorResponse("The goal changed before it could be completed; nothing was changed."), nil
			}
			return fantasy.NewTextResponse(fmt.Sprintf(
				"Goal closed. Outcome: %s\n\nNow present the requested result and an honest outcome in the conversation language, following your active agent's required output format. Do not claim success if the supplied success field is false.",
				params.Result,
			)), nil
		},
	)
}
