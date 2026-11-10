package agent

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"

	"charm.land/fantasy"
	"github.com/charmbracelet/crush/internal/csync"
	"github.com/charmbracelet/crush/internal/goal"
	"github.com/charmbracelet/crush/internal/message"
)

type goalCancelTarget struct {
	token     goal.Token
	hasToken  bool
	ticket    goal.Ticket
	hasTicket bool
}

// GoalInitialQueue removes pending initial goal prompts without touching other
// queued calls.
type GoalInitialQueue interface {
	RemoveGoalInitial(goal.Token)
}

// GoalWakeDeferrer records a reminder that could not start because its owner
// session is occupied.
type GoalWakeDeferrer interface {
	DeferGoalWake(goal.Token)
}

type goalRunState struct {
	generation    uint64
	token         goal.Token
	hasToken      bool
	dispatched    bool
	activated     bool
	continued     bool
	suppressed    bool
	summaryFailed bool
	stopTurn      atomic.Bool
	initialTicket *goal.Ticket
	handedOff     bool
}

func (a *sessionAgent) nextGoalRunGeneration(sessionID string) uint64 {
	a.goalRunSeqMu.Lock()
	defer a.goalRunSeqMu.Unlock()
	a.goalRunSeq[sessionID]++
	if a.goalRunSeq[sessionID] == 0 {
		a.goalRunSeq[sessionID]++
	}
	return a.goalRunSeq[sessionID]
}

func (a *sessionAgent) goalRunGenerationIsCurrent(sessionID string, generation uint64) bool {
	a.goalRunSeqMu.Lock()
	defer a.goalRunSeqMu.Unlock()
	return a.goalRunSeq[sessionID] == generation
}

func (a *sessionAgent) goalInitialPending(call SessionAgentCall) bool {
	ticket := call.GoalMarker.Ticket
	return !a.isSubAgent && a.goal != nil && ticket.Token.WorkspaceID == a.goalWorkspaceID &&
		ticket.Token.SessionID == call.SessionID && ticket.Token.Revision != 0 && a.goal.IsPending(ticket)
}

func (a *sessionAgent) goalInitialMayContinue(call SessionAgentCall, delivery *goalDelivery) bool {
	return delivery != nil && delivery.started() || a.goalInitialPending(call)
}

func (a *sessionAgent) admitGoalFollowupLocked(call SessionAgentCall, registerActive func()) bool {
	if a.isSubAgent || a.goal == nil {
		return false
	}
	token := call.GoalMarker.Token
	if token.WorkspaceID != a.goalWorkspaceID || token.SessionID != call.SessionID || token.Revision == 0 {
		return false
	}
	if a.IsSessionBusy(call.SessionID) {
		if a.goalWakeAdmission != nil {
			a.goalWakeAdmission(call.SessionID, call.GoalMarker.IngressGeneration, token, func() bool { return false })
		}
		return false
	}
	if queued, _ := a.messageQueue.Get(call.SessionID); len(queued) != 0 {
		if a.goalWakeAdmission != nil {
			a.goalWakeAdmission(call.SessionID, call.GoalMarker.IngressGeneration, token, func() bool { return false })
		}
		return false
	}

	// Lock order is session dispatch -> accepted bookkeeping -> Goal service.
	// The service invokes only the short callback that installs activeRequests,
	// so Cancel either invalidates this wake first or observes the admitted run.
	a.acceptedMu.Lock()
	defer a.acceptedMu.Unlock()
	accepted, _ := a.acceptedRuns.Get(call.SessionID)
	if accepted != 0 {
		if a.goalPendingWake == nil {
			a.goalPendingWake = make(map[string]goal.Token)
		}
		a.goalPendingWake[call.SessionID] = token
		return false
	}
	wake := call.GoalMarker.Wake
	if wake.Token.Revision == 0 {
		wake.Token = token
	}
	admitWake := func() bool {
		return a.goal.AdmitWake(wake, func() bool {
			registerActive()
			return true
		})
	}
	if a.goalWakeAdmission != nil {
		return a.goalWakeAdmission(call.SessionID, call.GoalMarker.IngressGeneration, token, admitWake)
	}
	return admitWake()
}

func (a *sessionAgent) cancelDroppedGoalInitial(call SessionAgentCall) {
	ticket := call.GoalMarker.Ticket
	if a.goal == nil || call.GoalMarker.Kind != goal.CallInitial || ticket.Token.WorkspaceID != a.goalWorkspaceID || ticket.Token.SessionID != call.SessionID {
		return
	}
	a.goal.CancelPending(ticket)
}

// RemoveGoalInitial drops only a queued initial prompt matching token. It is
// called after the service has invalidated the pending installation.
func (a *sessionAgent) RemoveGoalInitial(token goal.Token) {
	if a.isSubAgent || a.goal == nil || token.WorkspaceID != a.goalWorkspaceID || token.SessionID == "" {
		return
	}

	mu := a.sessionMu(token.SessionID)
	mu.Lock()
	queued, _ := a.messageQueue.Get(token.SessionID)
	kept := make([]SessionAgentCall, 0, len(queued))
	var dropped []SessionAgentCall
	for _, call := range queued {
		if call.GoalMarker.Kind == goal.CallInitial && call.GoalMarker.Ticket.Token == token {
			dropped = append(dropped, call)
			continue
		}
		kept = append(kept, call)
	}
	if len(kept) == 0 {
		a.messageQueue.Del(token.SessionID)
	} else {
		a.messageQueue.Set(token.SessionID, kept)
	}
	mu.Unlock()
	a.settleDroppedCalls(dropped)
}

func (a *sessionAgent) skipStaleGoalInitialLocked(ctx context.Context, call SessionAgentCall, sessionMu *sync.Mutex) (*fantasy.AgentResult, error) {
	if call.Accepted != nil {
		call.Accepted.Close()
	}
	queued, _ := a.messageQueue.Get(call.SessionID)
	var next *SessionAgentCall
	var dropped []SessionAgentCall
	var kept []SessionAgentCall
	for _, queuedCall := range queued {
		if queuedCall.GoalMarker.Kind == goal.CallInitial && !a.goalInitialPending(queuedCall) {
			dropped = append(dropped, queuedCall)
			continue
		}
		kept = append(kept, queuedCall)
	}
	if len(kept) > 0 {
		queuedCall := kept[0]
		queuedCall.Accepted = a.beginAcceptedCallLocked(call.SessionID, queuedCall.GoalMarker)
		next = &queuedCall
		kept = kept[1:]
	}
	if len(kept) == 0 {
		a.messageQueue.Del(call.SessionID)
	} else {
		a.messageQueue.Set(call.SessionID, kept)
	}
	sessionMu.Unlock()
	a.settleDroppedCalls(dropped)
	if next != nil {
		return a.Run(ctx, *next)
	}
	return nil, nil
}

func (a *sessionAgent) resumeGoalForCall(call SessionAgentCall) {
	if a.isSubAgent || a.goal == nil || call.GoalMarker.Kind != goal.CallOrdinary {
		return
	}
	a.goal.ResumeOnMainRun(a.goalWorkspaceID, call.SessionID)
}

func (a *sessionAgent) goalCancelState(call SessionAgentCall) *activeCancel {
	state := &activeCancel{}
	if a.isSubAgent || a.goal == nil {
		return state
	}

	switch call.GoalMarker.Kind {
	case goal.CallInitial:
		ticket := call.GoalMarker.Ticket
		if ticket.Token.WorkspaceID == a.goalWorkspaceID && ticket.Token.SessionID == call.SessionID {
			state.setGoalTicket(ticket)
		}
	case goal.CallFollowup, goal.CallContinuation:
		state.setGoalToken(call.GoalMarker.Token)
	default:
		snapshot := a.goal.Get()
		if snapshot.Active && snapshot.WorkspaceID == a.goalWorkspaceID && snapshot.SessionID == call.SessionID {
			state.setGoalToken(goalTokenForSnapshot(snapshot))
		}
	}
	return state
}

func (a *sessionAgent) pauseGoalForCancel(sessionID string, active *activeCancel) {
	if a.isSubAgent || a.goal == nil || active == nil {
		return
	}
	token, hasToken, ticket, hasTicket := active.goalState()
	if hasTicket {
		snapshot := a.goal.Get()
		if snapshot.WorkspaceID != a.goalWorkspaceID || snapshot.SessionID != sessionID || snapshot.Revision != ticket.Token.Revision {
			return
		}
		if snapshot.Pending {
			a.goal.CancelPending(ticket)
		} else if snapshot.Active {
			a.goal.Pause(ticket.Token)
		}
		return
	}
	if hasToken && token.WorkspaceID == a.goalWorkspaceID && token.SessionID == sessionID {
		a.goal.Pause(token)
	}
}

func (a *sessionAgent) goalTokenForStep(call SessionAgentCall, foldedDelivery *goalDelivery) (goal.Token, bool) {
	if a.isSubAgent || a.goal == nil {
		return goal.Token{}, false
	}
	if foldedDelivery != nil && foldedDelivery.hasFoldedMessage() {
		token := foldedDelivery.ticket.Token
		return token, token.WorkspaceID == a.goalWorkspaceID && token.SessionID == call.SessionID && token.Revision != 0
	}
	switch call.GoalMarker.Kind {
	case goal.CallInitial:
		return call.GoalMarker.Ticket.Token, call.GoalMarker.Ticket.Token.WorkspaceID == a.goalWorkspaceID && call.GoalMarker.Ticket.Token.SessionID == call.SessionID
	case goal.CallFollowup:
		token := call.GoalMarker.Token
		return token, token.WorkspaceID == a.goalWorkspaceID && token.SessionID == call.SessionID && token.Revision != 0
	case goal.CallContinuation:
		if foldedDelivery != nil && foldedDelivery.hasFoldedMessage() {
			token := foldedDelivery.ticket.Token
			return token, token.WorkspaceID == a.goalWorkspaceID && token.SessionID == call.SessionID && token.Revision != 0
		}
		token := call.GoalMarker.Token
		return token, token.WorkspaceID == a.goalWorkspaceID && token.SessionID == call.SessionID && token.Revision != 0
	default:
		if foldedDelivery != nil && foldedDelivery.hasFoldedMessage() {
			token := foldedDelivery.ticket.Token
			return token, token.WorkspaceID == a.goalWorkspaceID && token.SessionID == call.SessionID && token.Revision != 0
		}
		snapshot := a.goal.Get()
		if snapshot.Active && snapshot.WorkspaceID == a.goalWorkspaceID && snapshot.SessionID == call.SessionID {
			return goalTokenForSnapshot(snapshot), true
		}
		return goal.Token{}, false
	}
}

func goalTerminalIsNormal(result *fantasy.AgentResult, retErr error, state *goalRunState, assistant *message.Message, answered *csync.Map[string, struct{}]) bool {
	if retErr != nil || result == nil || state == nil || state.continued || state.suppressed || state.summaryFailed || state.stopTurn.Load() || len(result.Steps) == 0 {
		return false
	}
	if result.Steps[len(result.Steps)-1].FinishReason != fantasy.FinishReasonStop {
		return false
	}

	toolResults := make(map[string]struct{})
	for _, step := range result.Steps {
		for _, toolResult := range step.Content.ToolResults() {
			if toolResult.StopTurn {
				return false
			}
			toolResults[toolResult.ToolCallID] = struct{}{}
		}
	}
	for _, step := range result.Steps {
		for _, toolCall := range step.Content.ToolCalls() {
			if _, ok := toolResults[toolCall.ToolCallID]; !ok {
				return false
			}
		}
	}
	if assistant != nil {
		for _, toolCall := range assistant.ToolCalls() {
			if _, ok := answered.Get(toolCall.ID); !ok {
				return false
			}
		}
	}
	return true
}

func (a *sessionAgent) finishGoalRun(sessionID string, state *goalRunState, result *fantasy.AgentResult, retErr error, assistant *message.Message, answered *csync.Map[string, struct{}]) {
	if a.isSubAgent || a.goal == nil || state == nil {
		return
	}
	if state.initialTicket != nil && !state.activated {
		a.goal.CancelPending(*state.initialTicket)
	}
	if !state.hasToken || state.continued || state.suppressed || state.handedOff {
		return
	}
	if !a.goalRunGenerationIsCurrent(sessionID, state.generation) {
		return
	}
	if !state.dispatched {
		if retErr != nil {
			a.pauseGoalForGeneration(sessionID, state.token, state.generation)
		}
		return
	}
	snapshot := a.goal.Get()
	if !matchesGoalToken(snapshot, state.token) {
		return
	}

	if !goalTerminalIsNormal(result, retErr, state, assistant, answered) {
		a.pauseGoalForGeneration(sessionID, state.token, state.generation)
		return
	}
	if a.goal.IsPaused(state.token) || a.QueuedPrompts(sessionID) != 0 || a.IsSessionBusy(sessionID) {
		return
	}

	a.acceptedMu.Lock()
	accepted, _ := a.acceptedRuns.Get(sessionID)
	if accepted > 0 {
		a.goalPendingWake[sessionID] = state.token
		a.acceptedMu.Unlock()
		return
	}
	delete(a.goalPendingWake, sessionID)
	a.acceptedMu.Unlock()
	a.emitGoalWake(state.token)
}

func (a *sessionAgent) emitGoalWake(token goal.Token) {
	if a.isSubAgent || a.goal == nil || a.goalWake == nil {
		return
	}
	snapshot := a.goal.Get()
	if !matchesGoalToken(snapshot, token) || a.goal.IsPaused(token) || a.QueuedPrompts(token.SessionID) != 0 || a.IsSessionBusy(token.SessionID) {
		return
	}
	a.goalWake(token)
}

func (a *sessionAgent) currentGoalRunGeneration(sessionID string) uint64 {
	a.goalRunSeqMu.Lock()
	defer a.goalRunSeqMu.Unlock()
	return a.goalRunSeq[sessionID]
}

func (a *sessionAgent) pauseGoalForGeneration(sessionID string, token goal.Token, generation uint64) {
	if a.isSubAgent || a.goal == nil {
		return
	}
	mu := a.sessionMu(sessionID)
	mu.Lock()
	if a.goalRunGenerationIsCurrent(sessionID, generation) {
		a.goal.Pause(token)
	}
	mu.Unlock()
}

func (a *sessionAgent) pauseGoalFromContextForGeneration(ctx context.Context, sessionID string, generation uint64) {
	if a.isSubAgent || a.goal == nil {
		return
	}
	token, ok := goal.TrustedTokenFromContext(ctx)
	if ok {
		a.pauseGoalForGeneration(sessionID, token, generation)
	}
}

func isGoalDeliveryRevoked(err error) bool {
	return errors.Is(err, errGoalDeliveryRevoked)
}

func (c *coordinator) beginGoalRun(sessionID string, marker goal.CallMarker) bool {
	admitted, _ := c.beginGoalRunWithIngress(sessionID, marker, false)
	return admitted
}

func (c *coordinator) beginGoalRunWithIngress(sessionID string, marker goal.CallMarker, userIngress bool) (bool, goal.CallMarker) {
	if c.goal == nil {
		return true, marker
	}
	c.goalRunMu.Lock()
	defer c.goalRunMu.Unlock()
	if c.goalRuns == nil {
		c.goalRuns = make(map[string]int)
	}
	if c.goalWakePending == nil {
		c.goalWakePending = make(map[string]goal.Token)
	}
	if c.goalIngressGeneration == nil {
		c.goalIngressGeneration = make(map[string]uint64)
	}
	if userIngress {
		c.goalIngressGeneration[sessionID]++
		if c.goalIngressGeneration[sessionID] == 0 {
			c.goalIngressGeneration[sessionID]++
		}
	}
	if marker.Kind == goal.CallFollowup {
		marker.IngressGeneration = c.goalIngressGeneration[sessionID]
		if c.goalRuns[sessionID] > 0 {
			token := marker.Token
			if matchesGoalToken(c.goal.Get(), token) && !c.goal.IsPaused(token) {
				c.goalWakePending[sessionID] = token
			}
			return false, marker
		}
	}
	c.goalRuns[sessionID]++
	return true, marker
}

func (c *coordinator) admitGoalWake(sessionID string, ingressGeneration uint64, token goal.Token, admit func() bool) bool {
	c.goalRunMu.Lock()
	defer c.goalRunMu.Unlock()
	if admit == nil {
		return false
	}
	if c.goalIngressGeneration[sessionID] != ingressGeneration {
		c.deferGoalWakeLocked(sessionID, token)
		return false
	}
	if !admit() {
		c.deferGoalWakeLocked(sessionID, token)
		return false
	}
	return true
}

func (c *coordinator) deferGoalWakeLocked(sessionID string, token goal.Token) {
	if !matchesGoalToken(c.goal.Get(), token) || c.goal.IsPaused(token) {
		return
	}
	if c.goalWakePending == nil {
		c.goalWakePending = make(map[string]goal.Token)
	}
	c.goalWakePending[sessionID] = token
}

// DeferGoalWake remembers an app-worker wake while the owner session is busy.
// The pending wake is published when the coordinator's current operation ends.
func (c *coordinator) DeferGoalWake(token goal.Token) {
	if c.goal == nil || token.SessionID == "" {
		return
	}
	c.goalRunMu.Lock()
	if !matchesGoalToken(c.goal.Get(), token) || c.goal.IsPaused(token) {
		c.goalRunMu.Unlock()
		return
	}
	if c.goalRuns[token.SessionID] > 0 || c.IsSessionBusy(token.SessionID) {
		c.deferGoalWakeLocked(token.SessionID, token)
		c.goalRunMu.Unlock()
		return
	}
	c.goalRunMu.Unlock()
	c.publishGoalWake(token)
}

func (c *coordinator) endGoalRun(sessionID string) {
	if c.goal == nil {
		return
	}
	var pending goal.Token
	var hasPending bool
	c.goalRunMu.Lock()
	if c.goalRuns[sessionID] <= 1 {
		delete(c.goalRuns, sessionID)
		pending, hasPending = c.goalWakePending[sessionID]
		delete(c.goalWakePending, sessionID)
	} else {
		c.goalRuns[sessionID]--
	}
	c.goalRunMu.Unlock()
	if hasPending {
		c.publishGoalWake(pending)
	}
}

func (c *coordinator) signalGoalWake(token goal.Token) {
	if c.goal == nil || !matchesGoalToken(c.goal.Get(), token) || c.goal.IsPaused(token) {
		return
	}
	c.goalRunMu.Lock()
	if c.goalRuns[token.SessionID] > 0 {
		if c.goalWakePending == nil {
			c.goalWakePending = make(map[string]goal.Token)
		}
		c.goalWakePending[token.SessionID] = token
		c.goalRunMu.Unlock()
		return
	}
	c.goalRunMu.Unlock()
	c.publishGoalWake(token)
}

func (c *coordinator) publishGoalWake(token goal.Token) {
	if c.goal == nil || c.goalWake == nil || !matchesGoalToken(c.goal.Get(), token) || c.goal.IsPaused(token) {
		return
	}
	c.goalWake()
}
