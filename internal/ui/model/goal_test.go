package model

import (
	"context"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/crush/internal/agent/tools"
	"github.com/charmbracelet/crush/internal/config"
	"github.com/charmbracelet/crush/internal/goal"
	"github.com/charmbracelet/crush/internal/message"
	"github.com/charmbracelet/crush/internal/session"
	"github.com/charmbracelet/crush/internal/ui/attachments"
	"github.com/charmbracelet/crush/internal/ui/dialog"
	uv "github.com/charmbracelet/ultraviolet"
	"github.com/stretchr/testify/require"
)

type goalPaletteStub struct{}

func (goalPaletteStub) ID() string                      { return dialog.CommandsID }
func (goalPaletteStub) HandleMsg(tea.Msg) dialog.Action { return nil }
func (goalPaletteStub) Draw(uv.Screen, uv.Rectangle) *tea.Cursor {
	return nil
}

type goalWorkspaceStub struct {
	*testWorkspace

	calls          []string
	createdSession session.Session
	setSessionID   string
	setText        string
	setSnapshot    goal.Snapshot
	getSnapshot    goal.Snapshot
	cancelSnapshot goal.Snapshot
	cancelToken    goal.Token
	getCalls       int
}

func newGoalWorkspaceStub(disabled bool) *goalWorkspaceStub {
	disabledTools := []string(nil)
	if disabled {
		disabledTools = []string{tools.CompleteGoalToolName}
	}
	return &goalWorkspaceStub{
		testWorkspace: &testWorkspace{cfg: &config.Config{
			Options: &config.Options{DisabledTools: disabledTools},
		}},
	}
}

func (w *goalWorkspaceStub) CreateSession(context.Context, string) (session.Session, error) {
	w.calls = append(w.calls, "create")
	if w.createdSession.ID == "" {
		w.createdSession = session.Session{ID: "landing-session"}
	}
	return w.createdSession, nil
}

func (w *goalWorkspaceStub) SetGoal(_ context.Context, sessionID, text string) (goal.Snapshot, error) {
	w.calls = append(w.calls, "set")
	w.setSessionID = sessionID
	w.setText = text
	return w.setSnapshot, nil
}

func (w *goalWorkspaceStub) GetGoal(context.Context) (goal.Snapshot, error) {
	w.getCalls++
	return w.getSnapshot, nil
}

func (w *goalWorkspaceStub) CancelGoal(_ context.Context, expected goal.Token) (goal.Snapshot, error) {
	w.calls = append(w.calls, "cancel")
	w.cancelToken = expected
	return w.cancelSnapshot, nil
}

func newGoalTestUI(ws *goalWorkspaceStub) *UI {
	ui := newTestUI()
	ui.com.Workspace = ws
	ui.dialog = dialog.NewOverlay()
	ui.frames = newFrameCache(frameCacheTTL, frameCacheMaxEntries)
	return ui
}

func findGoalSetMessage(t *testing.T, msgs []tea.Msg) goalSetMsg {
	t.Helper()
	for _, msg := range msgs {
		if set, ok := msg.(goalSetMsg); ok {
			return set
		}
	}
	t.Fatal("goal set command did not return goalSetMsg")
	return goalSetMsg{}
}

func findGoalLoadedMessage(t *testing.T, msgs []tea.Msg) goalLoadedMsg {
	t.Helper()
	for _, msg := range msgs {
		if loaded, ok := msg.(goalLoadedMsg); ok {
			return loaded
		}
	}
	t.Fatal("goal fetch command did not return goalLoadedMsg")
	return goalLoadedMsg{}
}

func findGoalCanceledMessage(t *testing.T, msgs []tea.Msg) goalCanceledMsg {
	t.Helper()
	for _, msg := range msgs {
		if canceled, ok := msg.(goalCanceledMsg); ok {
			return canceled
		}
	}
	t.Fatal("goal cancel command did not return goalCanceledMsg")
	return goalCanceledMsg{}
}

func TestGoalSetCommandCapturesSessionAndCreatesLandingSessionFirst(t *testing.T) {
	t.Parallel()

	t.Run("session target is captured before async work", func(t *testing.T) {
		t.Parallel()

		ws := newGoalWorkspaceStub(false)
		ws.setSnapshot = goal.Snapshot{WorkspaceID: "ws", SessionID: "selected", Revision: 1, Pending: true, Version: 1}
		ui := newGoalTestUI(ws)
		ui.session = &session.Session{ID: "selected"}
		ui.textarea.SetValue("unfinished user prompt")
		ui.promptQueue = 2
		ui.attachments = attachments.New(nil, attachments.Keymap{})
		attachment := message.Attachment{FileName: "draft.txt"}
		ui.attachments.Update(attachment)

		cmd := ui.handleSetGoal(dialog.ActionSetGoal{SessionID: "selected", Text: "write the report"})
		ui.session = &session.Session{ID: "changed-while-request-was-pending"}
		set := findGoalSetMessage(t, runCmd(cmd))

		require.Equal(t, []string{"set"}, ws.calls)
		require.Equal(t, "selected", ws.setSessionID)
		require.Equal(t, "write the report", ws.setText)
		require.Nil(t, set.createdSession)
		_ = ui.applyGoalSet(set)
		require.Equal(t, set.snapshot, ui.goal.snapshot)
		require.Equal(t, "unfinished user prompt", ui.textarea.Value())
		require.Equal(t, 2, ui.promptQueue)
		require.Equal(t, []message.Attachment{attachment}, ui.attachments.List())
	})

	t.Run("landing session is created before setting goal", func(t *testing.T) {
		t.Parallel()

		ws := newGoalWorkspaceStub(false)
		ws.setSnapshot = goal.Snapshot{WorkspaceID: "ws", SessionID: "landing-session", Revision: 1, Pending: true, Version: 1}
		ui := newGoalTestUI(ws)
		ui.session = nil

		set := findGoalSetMessage(t, runCmd(ui.handleSetGoal(dialog.ActionSetGoal{Text: "start a project"})))

		require.Equal(t, []string{"create", "set"}, ws.calls)
		require.Equal(t, "landing-session", ws.setSessionID)
		require.Equal(t, "start a project", ws.setText)
		require.NotNil(t, set.createdSession)
		require.Equal(t, "landing-session", set.createdSession.ID)
	})
}

func TestGoalSetDoesNotCreateLandingSessionWhenToolIsDisabled(t *testing.T) {
	t.Parallel()

	ws := newGoalWorkspaceStub(true)
	ui := newGoalTestUI(ws)
	ui.session = nil

	_ = ui.openGoalDialog()
	require.False(t, ui.dialog.ContainsDialog(dialog.GoalID), "a stale Set Goal action cannot open while disabled")

	msgs := runCmd(ui.handleSetGoal(dialog.ActionSetGoal{Text: "must not be accepted"}))
	for _, msg := range msgs {
		_, isSet := msg.(goalSetMsg)
		require.False(t, isSet, "a disabled goal request must not create or set a session")
	}
	require.Empty(t, ws.calls)
	require.Empty(t, ws.createdSession.ID)
}

func TestGoalSnapshotOrderingAndPendingActivation(t *testing.T) {
	t.Parallel()

	ui := newGoalTestUI(newGoalWorkspaceStub(false))
	pending := goal.Snapshot{
		WorkspaceID: "ws",
		SessionID:   "owner",
		Revision:    4,
		Version:     10,
		Text:        "ship the feature",
		Pending:     true,
	}
	require.True(t, ui.applyGoalSnapshot(pending))
	require.True(t, ui.layout.goal.Empty(), "pending goals do not render a strip")

	active := pending
	active.Version++
	active.Pending = false
	active.Active = true
	require.True(t, ui.applyGoalSnapshot(active))
	require.Equal(t, goalStripHeight, ui.layout.goal.Dy())

	require.False(t, ui.applyGoalSnapshot(pending), "a late pending snapshot must not replace an active snapshot")
	require.Equal(t, active, ui.goal.snapshot)
	require.Equal(t, active.Version, ui.goal.appliedVersion)
}

func TestGoalCancelNoOpResponseClearsCurrentStrip(t *testing.T) {
	t.Parallel()

	ws := newGoalWorkspaceStub(false)
	ws.cancelSnapshot = goal.Snapshot{Version: 9}
	ui := newGoalTestUI(ws)
	ui.session = &session.Session{ID: "another-session"}
	active := goal.Snapshot{
		WorkspaceID: "ws",
		SessionID:   "goal-owner",
		Revision:    7,
		Version:     8,
		Text:        "finish the task",
		Active:      true,
	}
	ui.goal.snapshot = active
	ui.goal.appliedVersion = active.Version
	ui.updateLayoutAndSize()
	require.Equal(t, goalStripHeight, ui.layout.goal.Dy())

	ui.dialog.OpenDialog(goalPaletteStub{})

	expected := goal.Token{WorkspaceID: "ws", SessionID: "goal-owner", Revision: 7}
	msg := findGoalCanceledMessage(t, runCmd(ui.handleCancelGoal(dialog.ActionCancelGoal{ExpectedToken: expected})))
	require.Equal(t, expected, ws.cancelToken)
	require.False(t, ui.dialog.ContainsDialog(dialog.CommandsID), "cancel closes the palette immediately")
	require.NoError(t, msg.err)

	_ = ui.applyGoalCanceled(msg)

	require.Equal(t, ws.cancelSnapshot, ui.goal.snapshot)
	require.True(t, ui.layout.goal.Empty(), "an authoritative empty no-op response removes the stale strip")
	require.True(t, ui.frameDirty)
	require.Zero(t, ws.getCalls, "successful no-op cancellation does not require another GET")
}

func TestGoalLoadedMessageDropsSupersededRequest(t *testing.T) {
	t.Parallel()

	ws := newGoalWorkspaceStub(false)
	ui := newGoalTestUI(ws)
	first := ui.fetchGoalState()
	_ = ui.fetchGoalState()
	loaded := findGoalLoadedMessage(t, runCmd(first))
	require.Empty(t, ui.applyGoalLoaded(loaded))
	require.Empty(t, ui.goal.snapshot.SessionID)
	require.Equal(t, 1, ws.getCalls)
}
