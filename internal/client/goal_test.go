package client

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/charmbracelet/crush/internal/goal"
	"github.com/charmbracelet/crush/internal/proto"
	"github.com/charmbracelet/crush/internal/pubsub"
	"github.com/stretchr/testify/require"
)

func TestGoalClientRequests(t *testing.T) {
	t.Parallel()

	var gotSet proto.SetGoalRequest
	var gotCancel proto.CancelGoalRequest
	var requests []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests = append(requests, r.Method+" "+r.URL.Path)
		switch r.URL.Path {
		case "/v1/workspaces/ws-a/sessions/session-a/goal":
			require.Equal(t, http.MethodPost, r.Method)
			require.NoError(t, json.NewDecoder(r.Body).Decode(&gotSet))
			_ = json.NewEncoder(w).Encode(proto.GoalState{
				WorkspaceID: "ws-a",
				SessionID:   "session-a",
				Revision:    7,
				Version:     3,
				Text:        gotSet.Text,
				Pending:     true,
			})
		case "/v1/workspaces/ws-b/goal":
			require.Equal(t, http.MethodGet, r.Method)
			_ = json.NewEncoder(w).Encode(proto.GoalState{
				WorkspaceID: "ws-a",
				SessionID:   "session-a",
				Revision:    7,
				Version:     4,
				Text:        "shared goal",
				Active:      true,
			})
		case "/v1/workspaces/ws-b/goal/cancel":
			require.Equal(t, http.MethodPost, r.Method)
			require.NoError(t, json.NewDecoder(r.Body).Decode(&gotCancel))
			_ = json.NewEncoder(w).Encode(proto.GoalState{Version: 5})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	c := captureClient(t, server)
	set, err := c.SetGoal(t.Context(), "ws-a", "session-a", "follow the report")
	require.NoError(t, err)
	require.True(t, set.Pending)
	require.Equal(t, "follow the report", gotSet.Text)

	state, err := c.GetGoal(t.Context(), "ws-b")
	require.NoError(t, err)
	require.True(t, state.Active)
	require.Equal(t, "ws-a", state.WorkspaceID, "GET returns the global snapshot, not the request workspace")

	token := goal.Token{WorkspaceID: "ws-a", SessionID: "session-a", Revision: 7}
	state, err = c.CancelGoal(t.Context(), "ws-b", token)
	require.NoError(t, err)
	require.Equal(t, uint64(5), state.Version)
	require.Equal(t, proto.GoalToken{
		WorkspaceID: "ws-a",
		SessionID:   "session-a",
		Revision:    7,
	}, gotCancel.ExpectedToken)
	require.Equal(t, []string{
		"POST /v1/workspaces/ws-a/sessions/session-a/goal",
		"GET /v1/workspaces/ws-b/goal",
		"POST /v1/workspaces/ws-b/goal/cancel",
	}, requests)
}

func TestGoalClientTypedErrors(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/workspaces/ws/goal":
			w.WriteHeader(http.StatusForbidden)
			_ = json.NewEncoder(w).Encode(proto.GoalError{Code: string(GoalErrorCodeDisabled), Message: "disabled"})
		case "/v1/workspaces/ws/goal/cancel":
			w.WriteHeader(http.StatusConflict)
			_ = json.NewEncoder(w).Encode(proto.GoalError{Code: string(GoalErrorCodeStale), Message: "stale token"})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	c := captureClient(t, server)
	_, err := c.GetGoal(t.Context(), "ws")
	require.ErrorIs(t, err, goal.ErrGoalDisabled)
	var goalErr *GoalError
	require.ErrorAs(t, err, &goalErr)
	require.Equal(t, GoalErrorCodeDisabled, goalErr.Code)
	require.Equal(t, http.StatusForbidden, goalErr.StatusCode)

	_, err = c.CancelGoal(t.Context(), "ws", goal.Token{WorkspaceID: "ws", SessionID: "s", Revision: 2})
	require.ErrorIs(t, err, goal.ErrGoalConflict)
	require.ErrorAs(t, err, &goalErr)
	require.Equal(t, GoalErrorCodeStale, goalErr.Code)
}

func TestGoalClientDistinguishesUnsupportedFromMissingWorkspace(t *testing.T) {
	t.Parallel()

	t.Run("old server with existing workspace", func(t *testing.T) {
		t.Parallel()
		var probed atomic.Bool
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/v1/workspaces/ws/goal" {
				http.NotFound(w, r)
				return
			}
			if r.URL.Path == "/v1/workspaces/ws" {
				probed.Store(true)
				_ = json.NewEncoder(w).Encode(proto.Workspace{ID: "ws"})
				return
			}
			http.NotFound(w, r)
		}))
		defer server.Close()

		_, err := captureClient(t, server).GetGoal(t.Context(), "ws")
		require.ErrorIs(t, err, ErrGoalUnsupported)
		require.ErrorIs(t, err, ErrUnsupported)
		var goalErr *GoalError
		require.ErrorAs(t, err, &goalErr)
		require.Equal(t, GoalErrorCodeUnsupported, goalErr.Code)
		require.True(t, probed.Load())
	})

	t.Run("missing workspace", func(t *testing.T) {
		t.Parallel()
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.NotFound(w, r)
		}))
		defer server.Close()

		_, err := captureClient(t, server).GetGoal(t.Context(), "missing")
		require.ErrorIs(t, err, ErrNotFound)
		require.NotErrorIs(t, err, ErrGoalUnsupported)
		var goalErr *GoalError
		require.ErrorAs(t, err, &goalErr)
		require.Equal(t, GoalErrorCodeWorkspaceMissing, goalErr.Code)
	})
}

func TestGoalClientBoundsBodies(t *testing.T) {
	t.Parallel()

	requestServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprint(w, `{}`)
	}))
	defer requestServer.Close()
	c := captureClient(t, requestServer)
	_, err := c.SetGoal(t.Context(), "ws", "session", strings.Repeat("x", maxGoalRequestBodyBytes))
	require.Error(t, err)
	require.Contains(t, err.Error(), "exceeds the size limit")

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprint(w, `{"text":"`+strings.Repeat("x", maxGoalResponseBodyBytes)+`"}`)
	}))
	defer server.Close()
	_, err = captureClient(t, server).GetGoal(t.Context(), "ws")
	require.Error(t, err)
	require.Contains(t, err.Error(), "exceeds")
}

func TestSubscribeEventsDecodesGoalPayload(t *testing.T) {
	t.Parallel()

	wrapped, err := json.Marshal(pubsub.Payload{
		Type: pubsub.PayloadTypeGoal,
		Payload: mustMarshalJSON(t, pubsub.Event[proto.GoalState]{
			Type: pubsub.UpdatedEvent,
			Payload: proto.GoalState{
				WorkspaceID: "ws",
				SessionID:   "session",
				Revision:    9,
				Version:     12,
				Text:        "goal",
				Active:      true,
			},
		}),
	})
	require.NoError(t, err)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprintf(w, "data: %s\n\n", wrapped)
	}))
	defer server.Close()

	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	events, err := captureClient(t, server).SubscribeEvents(ctx, "ws")
	require.NoError(t, err)
	select {
	case ev := <-events:
		got, ok := ev.(pubsub.Event[proto.GoalState])
		require.True(t, ok, "expected a typed goal event, got %T", ev)
		require.Equal(t, "ws", got.Payload.WorkspaceID)
		require.Equal(t, uint64(12), got.Payload.Version)
	case <-ctx.Done():
		require.Fail(t, "timed out waiting for the goal event")
	}
}

func mustMarshalJSON(t *testing.T, value any) json.RawMessage {
	t.Helper()
	data, err := json.Marshal(value)
	require.NoError(t, err)
	return data
}

func TestGoalErrorIsNotStringMatched(t *testing.T) {
	t.Parallel()

	err := &GoalError{Code: GoalErrorCodeSessionMissing, Message: goal.ErrGoalDisabled.Error(), StatusCode: http.StatusNotFound}
	require.False(t, errors.Is(err, goal.ErrGoalDisabled))
}
