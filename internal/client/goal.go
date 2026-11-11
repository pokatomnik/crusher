package client

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"

	"github.com/charmbracelet/crush/internal/goal"
	"github.com/charmbracelet/crush/internal/proto"
	"github.com/charmbracelet/crush/internal/pubsub"
)

const (
	maxGoalRequestBodyBytes  = 64 << 10
	maxGoalResponseBodyBytes = 1 << 20
)

// Goal error codes returned by the goal endpoints.
type GoalErrorCode string

const (
	GoalErrorCodeDisabled         GoalErrorCode = "goal_disabled"
	GoalErrorCodeConflict         GoalErrorCode = "goal_conflict"
	GoalErrorCodeStale            GoalErrorCode = "goal_stale"
	GoalErrorCodeWorkspaceMissing GoalErrorCode = "workspace_not_found"
	GoalErrorCodeSessionMissing   GoalErrorCode = "session_not_found"
	GoalErrorCodeUnsupported      GoalErrorCode = "goal_unsupported"
)

// ErrGoalUnsupported reports that the connected server predates the Goal
// endpoints or explicitly does not provide the capability.
var ErrGoalUnsupported = errors.New("goal is not supported by this server")

// GoalError preserves the machine-readable code and HTTP status returned by
// the Goal API. Errors can be matched with errors.Is for shared domain
// outcomes such as disabled, conflict, and unsupported.
type GoalError struct {
	Code       GoalErrorCode
	Message    string
	StatusCode int
}

func (e *GoalError) Error() string {
	if e.Message == "" {
		if e.Code != "" {
			return string(e.Code)
		}
		return fmt.Sprintf("goal request failed with status %d", e.StatusCode)
	}
	if e.Code == "" {
		return fmt.Sprintf("goal request failed with status %d: %s", e.StatusCode, e.Message)
	}
	return fmt.Sprintf("%s: %s", e.Code, e.Message)
}

func (e *GoalError) Is(target error) bool {
	switch e.Code {
	case GoalErrorCodeDisabled:
		return target == goal.ErrGoalDisabled
	case GoalErrorCodeConflict, GoalErrorCodeStale:
		return target == goal.ErrGoalConflict
	case GoalErrorCodeWorkspaceMissing:
		return target == ErrNotFound
	case GoalErrorCodeUnsupported:
		return target == ErrGoalUnsupported || target == ErrUnsupported
	default:
		return false
	}
}

// SetGoal reserves a goal for the specified workspace and session.
func (c *Client) SetGoal(ctx context.Context, workspaceID, sessionID, text string) (proto.GoalState, error) {
	body := jsonBody(proto.SetGoalRequest{Text: text})
	if body.Len() > maxGoalRequestBodyBytes {
		return proto.GoalState{}, &GoalError{
			Code:       "invalid_request",
			Message:    "goal request body exceeds the size limit",
			StatusCode: http.StatusRequestEntityTooLarge,
		}
	}
	path := fmt.Sprintf("/workspaces/%s/sessions/%s/goal", workspaceID, sessionID)
	return c.goalRequest(ctx, http.MethodPost, path, workspaceID, body)
}

// GetGoal retrieves the global goal snapshot for the backend serving the
// specified workspace. The returned snapshot may be owned by another
// workspace on that backend.
func (c *Client) GetGoal(ctx context.Context, workspaceID string) (proto.GoalState, error) {
	path := fmt.Sprintf("/workspaces/%s/goal", workspaceID)
	return c.goalRequest(ctx, http.MethodGet, path, workspaceID, nil)
}

// CancelGoal cancels the goal only when expectedToken still identifies the
// goal selected by the caller.
func (c *Client) CancelGoal(ctx context.Context, workspaceID string, expectedToken goal.Token) (proto.GoalState, error) {
	request := proto.CancelGoalRequest{ExpectedToken: proto.GoalToken{
		WorkspaceID: expectedToken.WorkspaceID,
		SessionID:   expectedToken.SessionID,
		Revision:    expectedToken.Revision,
	}}
	body := jsonBody(request)
	path := fmt.Sprintf("/workspaces/%s/goal/cancel", workspaceID)
	return c.goalRequest(ctx, http.MethodPost, path, workspaceID, body)
}

func (c *Client) goalRequest(ctx context.Context, method, path, workspaceID string, body io.Reader) (proto.GoalState, error) {
	var headers http.Header
	if body != nil {
		headers = http.Header{"Content-Type": []string{"application/json"}}
	}
	rsp, err := c.sendReq(ctx, method, path, nil, body, headers)
	if err != nil {
		return proto.GoalState{}, fmt.Errorf("goal request failed: %w", err)
	}
	data, err := readGoalBody(rsp.Body)
	if err != nil {
		return proto.GoalState{}, fmt.Errorf("failed to read goal response: %w", err)
	}
	if rsp.StatusCode != http.StatusOK {
		return proto.GoalState{}, c.decodeGoalError(ctx, workspaceID, rsp.StatusCode, data)
	}

	var state proto.GoalState
	if err := json.Unmarshal(data, &state); err != nil {
		return proto.GoalState{}, fmt.Errorf("failed to decode goal response: %w", err)
	}
	return state, nil
}

func (c *Client) decodeGoalError(ctx context.Context, workspaceID string, status int, body []byte) error {
	var apiError proto.GoalError
	if json.Unmarshal(body, &apiError) == nil && apiError.Code != "" {
		return &GoalError{
			Code:       GoalErrorCode(apiError.Code),
			Message:    apiError.Message,
			StatusCode: status,
		}
	}

	message := ""
	var legacyError proto.Error
	if json.Unmarshal(body, &legacyError) == nil {
		message = legacyError.Message
	}

	if status == http.StatusNotFound {
		exists, known, err := c.probeGoalWorkspace(ctx, workspaceID)
		if err == nil && known {
			if !exists {
				return &GoalError{
					Code:       GoalErrorCodeWorkspaceMissing,
					Message:    "workspace not found",
					StatusCode: status,
				}
			}
			return &GoalError{
				Code:       GoalErrorCodeUnsupported,
				Message:    "the running server does not support goal management",
				StatusCode: status,
			}
		}
	}

	goalErr := &GoalError{Message: message, StatusCode: status}
	if status == http.StatusServiceUnavailable {
		return fmt.Errorf("%w: %w", ErrServerShuttingDown, goalErr)
	}
	return goalErr
}

// probeGoalWorkspace is used only for an untyped 404, where an older server
// may not have registered the Goal route. A missing workspace remains a
// normal not-found result instead of being mislabeled as unsupported.
func (c *Client) probeGoalWorkspace(ctx context.Context, workspaceID string) (exists, known bool, err error) {
	rsp, err := c.get(ctx, fmt.Sprintf("/workspaces/%s", workspaceID), nil, nil)
	if err != nil {
		return false, false, err
	}
	if _, err := readGoalBody(rsp.Body); err != nil {
		return false, false, err
	}
	switch rsp.StatusCode {
	case http.StatusOK:
		return true, true, nil
	case http.StatusNotFound:
		return false, true, nil
	default:
		return false, false, nil
	}
}

func readGoalBody(body io.ReadCloser) ([]byte, error) {
	defer body.Close()
	data, err := io.ReadAll(io.LimitReader(body, maxGoalResponseBodyBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxGoalResponseBodyBytes {
		return nil, fmt.Errorf("response exceeds %d bytes", maxGoalResponseBodyBytes)
	}
	return data, nil
}

func decodeGoalEvent(payload json.RawMessage) (pubsub.Event[proto.GoalState], error) {
	var event pubsub.Event[proto.GoalState]
	if err := json.Unmarshal(payload, &event); err != nil {
		return pubsub.Event[proto.GoalState]{}, err
	}
	return event, nil
}
