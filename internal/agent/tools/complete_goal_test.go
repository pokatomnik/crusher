package tools

import (
	"context"
	"encoding/json"
	"testing"

	"charm.land/fantasy"
	"github.com/charmbracelet/crush/internal/goal"
	"github.com/stretchr/testify/require"
)

func TestCompleteGoalToolNoGoalIsSuccessfulNoop(t *testing.T) {
	t.Parallel()

	service := goal.NewService()
	tool := NewCompleteGoalTool(service)
	response, err := tool.Run(context.Background(), fantasy.ToolCall{Input: `{"result":"done","success":true}`})
	require.NoError(t, err)
	require.False(t, response.IsError)
	require.False(t, response.StopTurn)
	require.Equal(t, "No current goal is set. Nothing was changed.", response.Content)
	require.Equal(t, uint64(0), service.Get().Version)
}

func TestCompleteGoalToolClosesGoalRegardlessOfOutcome(t *testing.T) {
	t.Parallel()

	service := goal.NewService()
	ticket, _, err := service.RequestInstallation("workspace", "session", "research this")
	require.NoError(t, err)
	_, ok := service.Activate(ticket)
	require.True(t, ok)

	ctx := context.WithValue(context.Background(), SessionIDContextKey, "session")
	ctx = goal.WithTrustedToken(ctx, ticket.Token)
	tool := NewCompleteGoalTool(service)
	response, err := tool.Run(ctx, fantasy.ToolCall{Input: `{"result":"blocked by missing access","success":false}`})
	require.NoError(t, err)
	require.False(t, response.IsError)
	require.False(t, response.StopTurn)
	require.Contains(t, response.Content, "Goal closed.")
	require.Contains(t, response.Content, "Do not claim success if the supplied success field is false.")
	require.False(t, service.Get().Active)
}

func TestCompleteGoalToolSuccessResponseAllowsHonestSuccess(t *testing.T) {
	t.Parallel()

	service := goal.NewService()
	ticket, _, err := service.RequestInstallation("workspace", "session", "research this")
	require.NoError(t, err)
	_, ok := service.Activate(ticket)
	require.True(t, ok)

	ctx := context.WithValue(context.Background(), SessionIDContextKey, "session")
	ctx = goal.WithTrustedToken(ctx, ticket.Token)
	response, err := NewCompleteGoalTool(service).Run(ctx, fantasy.ToolCall{Input: `{"result":"implemented and verified","success":true}`})
	require.NoError(t, err)
	require.Contains(t, response.Content, "Goal closed.")
	require.Contains(t, response.Content, "Do not claim success if the supplied success field is false.")
	require.NotContains(t, response.Content, "Do not claim success if success=true")
	require.False(t, service.Get().Active)
}

func TestCompleteGoalToolRejectsStaleTrustedToken(t *testing.T) {
	t.Parallel()

	service := goal.NewService()
	first, _, err := service.RequestInstallation("workspace", "session", "first")
	require.NoError(t, err)
	_, ok := service.Activate(first)
	require.True(t, ok)
	_, err = service.Cancel(first.Token)
	require.NoError(t, err)
	second, _, err := service.RequestInstallation("workspace", "session", "second")
	require.NoError(t, err)
	_, ok = service.Activate(second)
	require.True(t, ok)

	ctx := context.WithValue(context.Background(), SessionIDContextKey, "session")
	ctx = goal.WithTrustedToken(ctx, first.Token)
	response, err := NewCompleteGoalTool(service).Run(ctx, fantasy.ToolCall{Input: `{"result":"stale","success":true}`})
	require.NoError(t, err)
	require.True(t, response.IsError)
	require.True(t, service.Get().Active)
	require.Equal(t, second.Token.Revision, service.Get().Revision)
}

func TestCompleteGoalParamsRequireBothTypedFields(t *testing.T) {
	t.Parallel()

	for _, input := range []string{
		`{}`,
		`{"result":null,"success":true}`,
		`{"result":"x"}`,
		`{"result":"x","success":null}`,
		`{"result":4,"success":true}`,
		`{"result":"x","success":"false"}`,
	} {
		t.Run(input, func(t *testing.T) {
			var params CompleteGoalParams
			require.Error(t, json.Unmarshal([]byte(input), &params))
		})
	}

	var params CompleteGoalParams
	require.NoError(t, json.Unmarshal([]byte(`{"result":"","success":false}`), &params))
	require.Empty(t, params.Result)
	require.False(t, params.Success)
}
