package server

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/charmbracelet/crush/internal/app"
	"github.com/charmbracelet/crush/internal/backend"
	"github.com/charmbracelet/crush/internal/goal"
	"github.com/charmbracelet/crush/internal/proto"
	"github.com/charmbracelet/crush/internal/pubsub"
	"github.com/stretchr/testify/require"
)

func TestGoalEndpointsRegistered(t *testing.T) {
	t.Parallel()

	controller := &controllerV1{}
	paths := map[string]bool{}
	for _, endpoint := range controller.endpoints() {
		paths[endpoint.Method()+" "+endpoint.Path()] = true
	}
	require.True(t, paths["GET /v1/workspaces/{id}/goal"])
	require.True(t, paths["POST /v1/workspaces/{id}/sessions/{sid}/goal"])
	require.True(t, paths["POST /v1/workspaces/{id}/goal/cancel"])
}

func TestGoalSnapshotToProto(t *testing.T) {
	t.Parallel()

	got := goalSnapshotToProto(goal.Snapshot{
		WorkspaceID: "owner-ws",
		SessionID:   "owner-session",
		Revision:    11,
		Version:     15,
		Text:        "complete the research",
		Active:      true,
	})
	require.Equal(t, proto.GoalState{
		WorkspaceID: "owner-ws",
		SessionID:   "owner-session",
		Revision:    11,
		Version:     15,
		Text:        "complete the research",
		Active:      true,
	}, got)
}

func TestWrapGoalEvent(t *testing.T) {
	t.Parallel()

	wrapped := wrapEvent(pubsub.Event[goal.Snapshot]{
		Type: pubsub.UpdatedEvent,
		Payload: goal.Snapshot{
			WorkspaceID: "owner",
			SessionID:   "session",
			Revision:    2,
			Version:     3,
			Text:        "goal",
			Pending:     true,
		},
	})
	require.NotNil(t, wrapped)
	require.Equal(t, pubsub.PayloadTypeGoal, wrapped.Type)

	var event pubsub.Event[proto.GoalState]
	require.NoError(t, json.Unmarshal(wrapped.Payload, &event))
	require.Equal(t, pubsub.UpdatedEvent, event.Type)
	require.Equal(t, "owner", event.Payload.WorkspaceID)
	require.True(t, event.Payload.Pending)
	require.False(t, event.Payload.Active)
}

func TestGoalRequestBodyIsBounded(t *testing.T) {
	t.Parallel()

	controller := &controllerV1{}
	request := httptest.NewRequest(http.MethodPost, "/v1/workspaces/ws/sessions/s/goal", strings.NewReader(`{"text":"`+strings.Repeat("x", maxGoalRequestBodyBytes)+`"}`))
	request.SetPathValue("id", "ws")
	request.SetPathValue("sid", "s")
	response := httptest.NewRecorder()

	controller.handlePostWorkspaceSessionGoal(response, request)

	require.Equal(t, http.StatusRequestEntityTooLarge, response.Code)
	var apiError proto.GoalError
	require.NoError(t, json.Unmarshal(response.Body.Bytes(), &apiError))
	require.Equal(t, "invalid_request", apiError.Code)
}

func TestGoalRequestRejectsMalformedJSON(t *testing.T) {
	t.Parallel()

	controller := &controllerV1{}
	request := httptest.NewRequest(http.MethodPost, "/v1/workspaces/ws/sessions/s/goal", strings.NewReader(`{"text":`))
	request.SetPathValue("id", "ws")
	request.SetPathValue("sid", "s")
	response := httptest.NewRecorder()

	controller.handlePostWorkspaceSessionGoal(response, request)

	require.Equal(t, http.StatusBadRequest, response.Code)
	var apiError proto.GoalError
	require.NoError(t, json.Unmarshal(response.Body.Bytes(), &apiError))
	require.Equal(t, "invalid_request", apiError.Code)
}

func TestGoalOperationErrorCodes(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name   string
		err    error
		cancel bool
		status int
		code   string
	}{
		{name: "disabled", err: goal.ErrGoalDisabled, status: http.StatusForbidden, code: "goal_disabled"},
		{name: "set conflict", err: goal.ErrGoalAlreadyPending, status: http.StatusConflict, code: "goal_conflict"},
		{name: "stale cancel", err: goal.ErrGoalConflict, cancel: true, status: http.StatusConflict, code: "goal_stale"},
		{name: "missing session", err: sql.ErrNoRows, status: http.StatusNotFound, code: "session_not_found"},
		{name: "child session", err: app.ErrGoalChildSession, status: http.StatusBadRequest, code: "invalid_request"},
		{name: "goal service unavailable", err: backend.ErrGoalServiceUnavailable, status: http.StatusServiceUnavailable, code: "goal_unavailable"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			response := httptest.NewRecorder()
			(&controllerV1{}).writeGoalOperationError(response, tc.err, tc.cancel)

			require.Equal(t, tc.status, response.Code)
			var apiError proto.GoalError
			require.NoError(t, json.Unmarshal(response.Body.Bytes(), &apiError))
			require.Equal(t, tc.code, apiError.Code)
		})
	}
}

func TestGoalGetMissingWorkspaceReturnsTypedNotFound(t *testing.T) {
	t.Parallel()

	controller := newTestController()
	request := httptest.NewRequest(http.MethodGet, "/v1/workspaces/missing/goal", nil)
	request.SetPathValue("id", "missing")
	response := httptest.NewRecorder()

	controller.handleGetWorkspaceGoal(response, request)

	require.Equal(t, http.StatusNotFound, response.Code)
	var apiError proto.GoalError
	require.NoError(t, json.Unmarshal(response.Body.Bytes(), &apiError))
	require.Equal(t, "workspace_not_found", apiError.Code)
}
