package server

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/charmbracelet/crush/internal/apigen"
	"github.com/charmbracelet/crush/internal/app"
	"github.com/charmbracelet/crush/internal/backend"
	"github.com/charmbracelet/crush/internal/goal"
	"github.com/charmbracelet/crush/internal/proto"
)

const maxGoalRequestBodyBytes = 64 << 10

// goalBackend is intentionally a feature interface so a server assembled
// without the optional backend capability can return a typed unsupported
// response instead of panicking or falling through to a generic error.
type goalBackend interface {
	GetGoal(context.Context) (goal.Snapshot, error)
	SetGoal(context.Context, string, string, string) (goal.Snapshot, error)
	CancelGoal(context.Context, goal.Token) (goal.Snapshot, error)
}

func (c *controllerV1) goalEndpoints() []apigen.Endpoint {
	return []apigen.Endpoint{
		apigen.Get("/v1/workspaces/{id}/goal").
			Summary("Get the global goal state").
			Description("Returns the backend's global goal snapshot, which may be owned by another workspace.").
			Tags("goals").
			PathParam("id", "Workspace ID used to access this backend").
			Responds(proto.GoalState{}).
			Fails(404, 500, 503).
			Handle(c.handleGetWorkspaceGoal),

		apigen.Post("/v1/workspaces/{id}/sessions/{sid}/goal").
			Summary("Set a goal").
			Description("Reserves the backend's single goal slot for the specified session.").
			Tags("goals").
			PathParam("id", "Workspace ID").
			PathParam("sid", "Session ID").
			Accepts(proto.SetGoalRequest{}).
			Responds(proto.GoalState{}).
			Fails(400, 403, 404, 409, 413, 500, 503).
			Handle(c.handlePostWorkspaceSessionGoal),

		apigen.Post("/v1/workspaces/{id}/goal/cancel").
			Summary("Cancel a goal").
			Description("Cancels the goal only when expected_token still identifies the current installation.").
			Tags("goals").
			PathParam("id", "Workspace ID used to access this backend").
			Accepts(proto.CancelGoalRequest{}).
			Responds(proto.GoalState{}).
			Fails(400, 404, 409, 413, 500, 503).
			Handle(c.handlePostWorkspaceGoalCancel),
	}
}

func (c *controllerV1) handleGetWorkspaceGoal(w http.ResponseWriter, r *http.Request) {
	workspaceID := r.PathValue("id")
	if !c.requireGoalWorkspace(w, r, workspaceID) {
		return
	}
	access, ok := c.goalBackend(w)
	if !ok {
		return
	}

	snapshot, err := access.GetGoal(r.Context())
	if err != nil {
		c.writeGoalOperationError(w, err, false)
		return
	}
	jsonEncode(w, goalSnapshotToProto(snapshot))
}

func (c *controllerV1) handlePostWorkspaceSessionGoal(w http.ResponseWriter, r *http.Request) {
	var request proto.SetGoalRequest
	if status := decodeGoalRequest(w, r, &request); status != 0 {
		writeGoalError(w, status, "invalid_request", "invalid goal request body")
		return
	}
	if strings.TrimSpace(request.Text) == "" {
		writeGoalError(w, http.StatusBadRequest, "invalid_request", "goal text must not be empty")
		return
	}

	workspaceID := r.PathValue("id")
	if !c.requireGoalWorkspace(w, r, workspaceID) {
		return
	}
	access, ok := c.goalBackend(w)
	if !ok {
		return
	}

	snapshot, err := access.SetGoal(r.Context(), workspaceID, r.PathValue("sid"), request.Text)
	if err != nil {
		c.writeGoalOperationError(w, err, false)
		return
	}
	jsonEncode(w, goalSnapshotToProto(snapshot))
}

func (c *controllerV1) handlePostWorkspaceGoalCancel(w http.ResponseWriter, r *http.Request) {
	var request proto.CancelGoalRequest
	if status := decodeGoalRequest(w, r, &request); status != 0 {
		writeGoalError(w, status, "invalid_request", "invalid goal cancellation body")
		return
	}
	expected := request.ExpectedToken
	if expected.WorkspaceID == "" || expected.SessionID == "" || expected.Revision == 0 {
		writeGoalError(w, http.StatusBadRequest, "invalid_request", "expected_token must identify a goal revision")
		return
	}

	if !c.requireGoalWorkspace(w, r, r.PathValue("id")) {
		return
	}
	access, ok := c.goalBackend(w)
	if !ok {
		return
	}

	snapshot, err := access.CancelGoal(r.Context(), goal.Token{
		WorkspaceID: expected.WorkspaceID,
		SessionID:   expected.SessionID,
		Revision:    expected.Revision,
	})
	if err != nil {
		c.writeGoalOperationError(w, err, true)
		return
	}
	jsonEncode(w, goalSnapshotToProto(snapshot))
}

func (c *controllerV1) requireGoalWorkspace(w http.ResponseWriter, r *http.Request, workspaceID string) bool {
	if c.backend == nil {
		writeGoalError(w, http.StatusServiceUnavailable, "goal_unsupported", "goal management is not available")
		return false
	}
	if _, err := c.backend.GetWorkspace(workspaceID); err != nil {
		c.writeGoalOperationError(w, err, false)
		return false
	}
	return true
}

func (c *controllerV1) goalBackend(w http.ResponseWriter) (goalBackend, bool) {
	if c.backend == nil {
		writeGoalError(w, http.StatusServiceUnavailable, "goal_unsupported", "goal management is not available")
		return nil, false
	}
	access, ok := any(c.backend).(goalBackend)
	if !ok {
		writeGoalError(w, http.StatusServiceUnavailable, "goal_unsupported", "goal management is not supported by this backend")
		return nil, false
	}
	return access, true
}

func (c *controllerV1) writeGoalOperationError(w http.ResponseWriter, err error, cancel bool) {
	status := http.StatusInternalServerError
	code := "internal_error"
	message := err.Error()
	switch {
	case errors.Is(err, backend.ErrWorkspaceNotFound):
		status = http.StatusNotFound
		code = "workspace_not_found"
		message = "workspace not found"
	case errors.Is(err, sql.ErrNoRows):
		status = http.StatusNotFound
		code = "session_not_found"
		message = "session not found"
	case errors.Is(err, goal.ErrGoalDisabled):
		status = http.StatusForbidden
		code = "goal_disabled"
	case errors.Is(err, goal.ErrGoalAlreadyActive), errors.Is(err, goal.ErrGoalAlreadyPending):
		status = http.StatusConflict
		code = "goal_conflict"
	case errors.Is(err, goal.ErrGoalConflict):
		status = http.StatusConflict
		if cancel {
			code = "goal_stale"
		} else {
			code = "goal_conflict"
		}
	case errors.Is(err, app.ErrGoalChildSession):
		status = http.StatusBadRequest
		code = "invalid_request"
	case errors.Is(err, app.ErrGoalCoordinatorNotReady):
		status = http.StatusServiceUnavailable
		code = "goal_not_ready"
	case errors.Is(err, app.ErrGoalRuntimeStopped),
		errors.Is(err, backend.ErrGoalServiceUnavailable),
		errors.Is(err, backend.ErrWorkspaceClosing),
		errors.Is(err, backend.ErrServerShuttingDown):
		status = http.StatusServiceUnavailable
		code = "goal_unavailable"
	}
	writeGoalError(w, status, code, message)
}

func decodeGoalRequest(w http.ResponseWriter, r *http.Request, destination any) int {
	r.Body = http.MaxBytesReader(w, r.Body, maxGoalRequestBodyBytes)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		return goalRequestDecodeStatus(err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			return http.StatusBadRequest
		}
		return goalRequestDecodeStatus(err)
	}
	return 0
}

func goalRequestDecodeStatus(err error) int {
	var tooLarge *http.MaxBytesError
	if errors.As(err, &tooLarge) {
		return http.StatusRequestEntityTooLarge
	}
	return http.StatusBadRequest
}

func writeGoalError(w http.ResponseWriter, status int, code, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(proto.GoalError{Code: code, Message: message})
}

func goalSnapshotToProto(snapshot goal.Snapshot) proto.GoalState {
	return proto.GoalState{
		WorkspaceID: snapshot.WorkspaceID,
		SessionID:   snapshot.SessionID,
		Revision:    snapshot.Revision,
		Version:     snapshot.Version,
		Text:        snapshot.Text,
		Active:      snapshot.Active,
		Pending:     snapshot.Pending,
	}
}
