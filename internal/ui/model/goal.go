package model

import (
	"context"
	"errors"
	"slices"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/crush/internal/agent/tools"
	"github.com/charmbracelet/crush/internal/goal"
	"github.com/charmbracelet/crush/internal/session"
	"github.com/charmbracelet/crush/internal/ui/dialog"
	"github.com/charmbracelet/crush/internal/ui/util"
	"github.com/charmbracelet/crush/internal/workspace"
)

type goalUIState struct {
	snapshot             goal.Snapshot
	appliedVersion       uint64
	requestGeneration    uint64
	connectionGeneration uint64
}

type goalLoadedMsg struct {
	generation uint64
	connection uint64
	snapshot   goal.Snapshot
	err        error
}

type goalSetMsg struct {
	generation     uint64
	connection     uint64
	sessionID      string
	createdSession *session.Session
	snapshot       goal.Snapshot
	err            error
}

type goalCanceledMsg struct {
	generation uint64
	connection uint64
	snapshot   goal.Snapshot
	err        error
}

func (m *UI) goalAccess() (workspace.GoalAccess, bool) {
	if m.com == nil || m.com.Workspace == nil {
		return nil, false
	}
	access, ok := m.com.Workspace.(workspace.GoalAccess)
	return access, ok
}

func (m *UI) goalAvailable() bool {
	if _, ok := m.goalAccess(); !ok {
		return false
	}
	cfg := m.com.Workspace.Config()
	return cfg == nil || cfg.Options == nil || !slices.Contains(cfg.Options.DisabledTools, tools.CompleteGoalToolName)
}

func (m *UI) refreshGoalCommandState() {
	commands, ok := m.dialog.Dialog(dialog.CommandsID).(*dialog.Commands)
	if !ok {
		return
	}
	commands.SetGoalState(m.goal.snapshot, m.goalAvailable())
}

func (m *UI) fetchGoalState() tea.Cmd {
	access, ok := m.goalAccess()
	if !ok {
		return nil
	}
	m.goal.requestGeneration++
	generation := m.goal.requestGeneration
	connection := m.goal.connectionGeneration
	return func() tea.Msg {
		snapshot, err := access.GetGoal(context.Background())
		return goalLoadedMsg{
			generation: generation,
			connection: connection,
			snapshot:   snapshot,
			err:        err,
		}
	}
}

func (m *UI) applyGoalSnapshot(snapshot goal.Snapshot) bool {
	if snapshot.Version < m.goal.appliedVersion {
		return false
	}
	previous := m.goal.snapshot
	m.goal.snapshot = snapshot
	m.goal.appliedVersion = snapshot.Version
	m.refreshGoalCommandState()
	if previous != snapshot {
		m.updateLayoutAndSize()
		m.invalidateFrames()
	}
	return true
}

func (m *UI) openGoalDialog() tea.Cmd {
	if m.dialog.ContainsDialog(dialog.GoalID) {
		m.dialog.BringToFront(dialog.GoalID)
		return nil
	}
	if _, ok := m.goalAccess(); !ok {
		return util.ReportError(errors.New("goal management is not supported by this workspace"))
	}
	if !m.goalAvailable() {
		return util.ReportError(goal.ErrGoalDisabled)
	}
	if m.goal.snapshot.Active || m.goal.snapshot.Pending {
		return util.ReportWarn("A goal is already active or pending. Cancel it before setting another.")
	}

	sessionID := ""
	if m.session != nil {
		sessionID = m.session.ID
	}
	m.textarea.Blur()
	if m.activeInline != nil {
		m.activeInline.SetFocused(false)
	}
	m.dialog.OpenDialog(dialog.NewGoal(m.com, sessionID))
	return nil
}

func (m *UI) restoreGoalEditorFocus() tea.Cmd {
	if m.focus != uiFocusEditor {
		return nil
	}
	if m.activeInline != nil {
		m.activeInline.SetFocused(true)
		return nil
	}
	return m.textarea.Focus()
}

func (m *UI) handleSetGoal(action dialog.ActionSetGoal) tea.Cmd {
	m.dialog.CloseDialog(dialog.GoalID)
	focusCmd := m.restoreGoalEditorFocus()

	access, ok := m.goalAccess()
	if !ok {
		return tea.Batch(focusCmd, util.ReportError(errors.New("goal management is not supported by this workspace")))
	}
	if !m.goalAvailable() {
		m.refreshGoalCommandState()
		return tea.Batch(focusCmd, util.ReportError(goal.ErrGoalDisabled), m.fetchGoalState())
	}

	m.goal.requestGeneration++
	generation := m.goal.requestGeneration
	connection := m.goal.connectionGeneration
	ws := m.com.Workspace
	sessionID := action.SessionID
	text := action.Text
	return tea.Batch(focusCmd, func() tea.Msg {
		var created *session.Session
		if sessionID == "" {
			createdSession, err := ws.CreateSession(context.Background(), "")
			if err != nil {
				return goalSetMsg{generation: generation, connection: connection, err: err}
			}
			created = &createdSession
			sessionID = createdSession.ID
		}
		snapshot, err := access.SetGoal(context.Background(), sessionID, text)
		return goalSetMsg{
			generation:     generation,
			connection:     connection,
			sessionID:      sessionID,
			createdSession: created,
			snapshot:       snapshot,
			err:            err,
		}
	})
}

func (m *UI) handleCancelGoal(action dialog.ActionCancelGoal) tea.Cmd {
	m.dialog.CloseDialog(dialog.CommandsID)
	focusCmd := m.restoreGoalEditorFocus()
	access, ok := m.goalAccess()
	if !ok {
		return tea.Batch(focusCmd, util.ReportError(errors.New("goal management is not supported by this workspace")))
	}

	m.goal.requestGeneration++
	generation := m.goal.requestGeneration
	connection := m.goal.connectionGeneration
	expected := action.ExpectedToken
	return tea.Batch(focusCmd, func() tea.Msg {
		snapshot, err := access.CancelGoal(context.Background(), expected)
		return goalCanceledMsg{
			generation: generation,
			connection: connection,
			snapshot:   snapshot,
			err:        err,
		}
	})
}

func (m *UI) applyGoalLoaded(msg goalLoadedMsg) []tea.Cmd {
	if msg.connection != m.goal.connectionGeneration || msg.generation != m.goal.requestGeneration {
		return nil
	}
	if msg.err != nil {
		return []tea.Cmd{util.ReportError(msg.err)}
	}
	m.applyGoalSnapshot(msg.snapshot)
	return nil
}

func (m *UI) applyGoalSet(msg goalSetMsg) []tea.Cmd {
	if msg.connection != m.goal.connectionGeneration {
		return nil
	}
	var cmds []tea.Cmd
	if msg.createdSession != nil && m.session == nil {
		cmds = append(cmds, m.loadSession(msg.sessionID))
	}
	if msg.err != nil {
		if msg.generation != m.goal.requestGeneration {
			return cmds
		}
		cmds = append(cmds, util.ReportError(msg.err))
		if errors.Is(msg.err, goal.ErrGoalDisabled) {
			m.refreshGoalCommandState()
		}
		cmds = append(cmds, m.fetchGoalState())
		return cmds
	}

	m.applyGoalSnapshot(msg.snapshot)
	m.invalidateBusyCaches()
	m.invalidatePromptQueue()
	if cmd := m.dispatchBusyRefresh(); cmd != nil {
		cmds = append(cmds, cmd)
	}
	if cmd := m.dispatchPromptQueueRefresh(); cmd != nil {
		cmds = append(cmds, cmd)
	}
	return cmds
}

func (m *UI) applyGoalCanceled(msg goalCanceledMsg) []tea.Cmd {
	if msg.connection != m.goal.connectionGeneration {
		return nil
	}
	if msg.err != nil {
		if msg.generation != m.goal.requestGeneration {
			return nil
		}
		return []tea.Cmd{util.ReportError(msg.err), m.fetchGoalState()}
	}

	m.applyGoalSnapshot(msg.snapshot)
	m.invalidateBusyCaches()
	m.invalidatePromptQueue()
	var cmds []tea.Cmd
	if cmd := m.dispatchBusyRefresh(); cmd != nil {
		cmds = append(cmds, cmd)
	}
	if cmd := m.dispatchPromptQueueRefresh(); cmd != nil {
		cmds = append(cmds, cmd)
	}
	return cmds
}
