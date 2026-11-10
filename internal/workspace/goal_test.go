package workspace

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/crush/internal/client"
	"github.com/charmbracelet/crush/internal/config"
	"github.com/charmbracelet/crush/internal/goal"
	"github.com/charmbracelet/crush/internal/proto"
	"github.com/charmbracelet/crush/internal/pubsub"
	"github.com/stretchr/testify/require"
)

func TestClientWorkspaceGoalMethods(t *testing.T) {
	t.Parallel()

	var gotSet proto.SetGoalRequest
	var gotCancel proto.CancelGoalRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/workspaces/ws-1/sessions/session-1/goal":
			require.NoError(t, json.NewDecoder(r.Body).Decode(&gotSet))
			_ = json.NewEncoder(w).Encode(proto.GoalState{
				WorkspaceID: "ws-1",
				SessionID:   "session-1",
				Revision:    5,
				Version:     1,
				Text:        gotSet.Text,
				Pending:     true,
			})
		case "/v1/workspaces/ws-1/goal":
			_ = json.NewEncoder(w).Encode(proto.GoalState{
				WorkspaceID: "ws-1",
				SessionID:   "session-1",
				Revision:    5,
				Version:     2,
				Text:        "saved goal",
				Active:      true,
			})
		case "/v1/workspaces/ws-1/goal/cancel":
			require.NoError(t, json.NewDecoder(r.Body).Decode(&gotCancel))
			_ = json.NewEncoder(w).Encode(proto.GoalState{Version: 3})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	parsed, err := url.Parse(server.URL)
	require.NoError(t, err)
	c, err := client.NewClient(t.TempDir(), "tcp", parsed.Host)
	require.NoError(t, err)
	w := NewClientWorkspace(c, proto.Workspace{ID: "ws-1"})

	pending, err := w.SetGoal(t.Context(), "session-1", "write the report")
	require.NoError(t, err)
	require.Equal(t, goal.Snapshot{
		WorkspaceID: "ws-1",
		SessionID:   "session-1",
		Revision:    5,
		Version:     1,
		Text:        "write the report",
		Pending:     true,
	}, pending)
	require.Equal(t, "write the report", gotSet.Text)

	active, err := w.GetGoal(t.Context())
	require.NoError(t, err)
	require.True(t, active.Active)
	require.Equal(t, uint64(2), active.Version)

	token := goal.Token{WorkspaceID: "ws-1", SessionID: "session-1", Revision: 5}
	empty, err := w.CancelGoal(t.Context(), token)
	require.NoError(t, err)
	require.Equal(t, goal.Snapshot{Version: 3}, empty)
	require.Equal(t, proto.GoalToken{
		WorkspaceID: "ws-1",
		SessionID:   "session-1",
		Revision:    5,
	}, gotCancel.ExpectedToken)
	require.Equal(t, empty, w.cachedGoalSnapshot())
}

func TestClientWorkspaceDiscardsLateGoalResponseFromPreviousRuntime(t *testing.T) {
	t.Parallel()

	oldStarted := make(chan struct{})
	releaseOld := make(chan struct{})
	var oldRequest sync.Once
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/workspaces/ws-old/goal":
			oldRequest.Do(func() { close(oldStarted) })
			<-releaseOld
			_ = json.NewEncoder(w).Encode(proto.GoalState{
				WorkspaceID: "owner-old",
				SessionID:   "session-old",
				Revision:    1,
				Version:     100,
				Text:        "stale goal",
				Active:      true,
			})
		case r.Method == http.MethodPost && r.URL.Path == "/v1/workspaces":
			_ = json.NewEncoder(w).Encode(proto.Workspace{ID: "ws-new", Path: "/tmp/recover"})
		case r.Method == http.MethodGet && r.URL.Path == "/v1/workspaces/ws-new/goal":
			_ = json.NewEncoder(w).Encode(proto.GoalState{
				WorkspaceID: "owner-new",
				SessionID:   "session-new",
				Revision:    1,
				Version:     2,
				Text:        "current goal",
				Active:      true,
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	parsed, err := url.Parse(server.URL)
	require.NoError(t, err)
	c, err := client.NewClient(t.TempDir(), "tcp", parsed.Host)
	require.NoError(t, err)
	w := NewClientWorkspace(c, proto.Workspace{ID: "ws-old", Path: "/tmp/recover"})

	type getResult struct {
		snapshot goal.Snapshot
		err      error
	}
	oldResult := make(chan getResult, 1)
	go func() {
		snapshot, err := w.GetGoal(t.Context())
		oldResult <- getResult{snapshot: snapshot, err: err}
	}()
	select {
	case <-oldStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("old goal request did not start")
	}
	require.NoError(t, w.recoverWorkspace())
	close(releaseOld)

	select {
	case result := <-oldResult:
		require.ErrorIs(t, result.err, ErrGoalWorkspaceChanged)
	case <-time.After(2 * time.Second):
		t.Fatal("old goal request did not finish")
	}
	require.Equal(t, goal.Snapshot{}, w.cachedGoalSnapshot(), "the stale high-version snapshot must not poison the recovered cache")

	current, err := w.GetGoal(t.Context())
	require.NoError(t, err)
	require.Equal(t, uint64(2), current.Version)
	require.Equal(t, "owner-new", current.WorkspaceID)
	require.Equal(t, current, w.cachedGoalSnapshot())
}

func TestClientWorkspaceTranslatesGoalEvent(t *testing.T) {
	t.Parallel()

	w := NewClientWorkspace(nil, proto.Workspace{})
	wire := pubsub.Event[proto.GoalState]{
		Type: pubsub.UpdatedEvent,
		Payload: proto.GoalState{
			WorkspaceID: "owner-ws",
			SessionID:   "owner-session",
			Revision:    4,
			Version:     8,
			Text:        "research",
			Active:      true,
		},
	}

	translated, ok := w.translateEvent(wire).(pubsub.Event[goal.Snapshot])
	require.True(t, ok, "expected a domain goal event")
	require.Equal(t, wire.Type, translated.Type)
	require.Equal(t, goal.Snapshot{
		WorkspaceID: "owner-ws",
		SessionID:   "owner-session",
		Revision:    4,
		Version:     8,
		Text:        "research",
		Active:      true,
	}, translated.Payload)
	require.Equal(t, translated.Payload, w.cachedGoalSnapshot())
}

func TestClientWorkspaceResyncsGoalOnFirstSubscription(t *testing.T) {
	t.Parallel()

	var gotSubscription atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/workspaces/ws-1/events":
			gotSubscription.Store(true)
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			w.(http.Flusher).Flush()
			<-r.Context().Done()
		case "/v1/workspaces/ws-1/goal":
			_ = json.NewEncoder(w).Encode(proto.GoalState{
				WorkspaceID: "owner",
				SessionID:   "session",
				Revision:    2,
				Version:     6,
				Text:        "global goal",
				Active:      true,
			})
		default:
			w.WriteHeader(http.StatusOK)
		}
	}))
	defer server.Close()

	parsed, err := url.Parse(server.URL)
	require.NoError(t, err)
	c, err := client.NewClient(t.TempDir(), "tcp", parsed.Host)
	require.NoError(t, err)
	w := NewClientWorkspace(c, proto.Workspace{ID: "ws-1"})
	sent := make(chan tea.Msg, 4)
	done := make(chan struct{})
	go func() {
		w.runSubscription(func(msg tea.Msg) { sent <- msg })
		close(done)
	}()

	select {
	case msg := <-sent:
		event, ok := msg.(pubsub.Event[goal.Snapshot])
		require.True(t, ok, "expected goal resync event, got %T", msg)
		require.Equal(t, "owner", event.Payload.WorkspaceID)
		require.Equal(t, uint64(6), event.Payload.Version)
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for the first-subscription goal resync")
	}
	require.True(t, gotSubscription.Load())

	w.Shutdown()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("subscription did not stop after Shutdown")
	}
}

func TestClientWorkspaceConfigRefreshNotifiesGoalAvailability(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/v1/workspaces/ws-1", r.URL.Path)
		_ = json.NewEncoder(w).Encode(proto.Workspace{
			ID: "ws-1",
			Config: &config.Config{
				Options: &config.Options{DisabledTools: []string{"complete_goal"}},
			},
		})
	}))
	defer server.Close()

	parsed, err := url.Parse(server.URL)
	require.NoError(t, err)
	c, err := client.NewClient(t.TempDir(), "tcp", parsed.Host)
	require.NoError(t, err)
	w := NewClientWorkspace(c, proto.Workspace{
		ID: "ws-1",
		Config: &config.Config{
			Options: &config.Options{DisabledTools: []string{}},
		},
	})
	current := goal.Snapshot{WorkspaceID: "owner", SessionID: "s", Revision: 3, Version: 9, Active: true}
	_, generation := w.goalRuntime()
	w.cacheGoalSnapshot(generation, current)

	incoming := make(chan any, 1)
	incoming <- pubsub.Event[proto.ConfigChanged]{}
	close(incoming)
	var sent []tea.Msg
	w.consumeEvents(incoming, func(msg tea.Msg) { sent = append(sent, msg) })

	require.Equal(t, []string{"complete_goal"}, w.Config().Options.DisabledTools)
	require.Len(t, sent, 1)
	event, ok := sent[0].(pubsub.Event[goal.Snapshot])
	require.True(t, ok)
	require.Equal(t, current, event.Payload)
}

func TestClientWorkspaceFailedConfigRefreshPreservesCache(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()

	parsed, err := url.Parse(server.URL)
	require.NoError(t, err)
	c, err := client.NewClient(t.TempDir(), "tcp", parsed.Host)
	require.NoError(t, err)
	cached := &config.Config{Options: &config.Options{DisabledTools: []string{"keep"}}}
	w := NewClientWorkspace(c, proto.Workspace{ID: "ws-1", Config: cached})

	incoming := make(chan any, 1)
	incoming <- pubsub.Event[proto.ConfigChanged]{}
	close(incoming)
	var sent []tea.Msg
	w.consumeEvents(incoming, func(msg tea.Msg) { sent = append(sent, msg) })

	require.Same(t, cached, w.Config())
	require.Equal(t, []string{"keep"}, w.Config().Options.DisabledTools)
	require.Empty(t, sent)
}

func TestGoalSnapshotConversionPreservesEmptyAndPendingStates(t *testing.T) {
	t.Parallel()

	for _, state := range []proto.GoalState{
		{},
		{WorkspaceID: "ws", SessionID: "s", Revision: 2, Version: 4, Text: "pending", Pending: true},
	} {
		got := goalStateToSnapshot(state)
		require.Equal(t, state.WorkspaceID, got.WorkspaceID)
		require.Equal(t, state.SessionID, got.SessionID)
		require.Equal(t, state.Revision, got.Revision)
		require.Equal(t, state.Version, got.Version)
		require.Equal(t, state.Text, got.Text)
		require.Equal(t, state.Active, got.Active)
		require.Equal(t, state.Pending, got.Pending)
	}
}

func TestGoalSetRequestIsBoundedInClient(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer server.Close()
	parsed, err := url.Parse(server.URL)
	require.NoError(t, err)
	c, err := client.NewClient(t.TempDir(), "tcp", parsed.Host)
	require.NoError(t, err)

	_, err = c.SetGoal(t.Context(), "ws", "session", strings.Repeat("x", 64<<10))
	require.Error(t, err)
	require.Contains(t, err.Error(), "size limit")
}
