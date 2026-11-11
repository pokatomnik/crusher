package agent

import (
	"context"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"charm.land/catwalk/v3/pkg/catwalk"
	"charm.land/fantasy"
	agenttools "github.com/charmbracelet/crush/internal/agent/tools"
	"github.com/charmbracelet/crush/internal/config"
	"github.com/charmbracelet/crush/internal/csync"
	"github.com/charmbracelet/crush/internal/goal"
	"github.com/charmbracelet/crush/internal/message"
	"github.com/stretchr/testify/require"
)

func TestEffectiveGoalToolNamesAreMainOnlyAndConfigDisableWins(t *testing.T) {
	t.Parallel()

	configured := []string{"view"}
	require.Equal(t, []string{"view", agenttools.CompleteGoalToolName}, effectiveGoalToolNames(configured, false, true, nil))
	require.Equal(t, []string{"view"}, effectiveGoalToolNames(configured, true, true, nil))
	require.Equal(t, []string{"view"}, effectiveGoalToolNames(configured, false, true, []string{agenttools.CompleteGoalToolName}))
	require.Equal(t, []string{"view"}, effectiveGoalToolNames(configured, false, false, nil), "without a Goal runtime, keep the upstream tool list")
	require.Equal(t, []string{"view"}, configured, "effective names must not mutate config")

	cfg := config.NewTestStore(&config.Config{Options: &config.Options{DisabledTools: []string{agenttools.CompleteGoalToolName}}})
	coordinator := &coordinator{cfg: cfg}
	require.Equal(t, []string{"view"}, coordinator.effectiveToolNames(config.Agent{AllowedTools: configured}, false))
	require.Equal(t, []string{"view"}, coordinator.effectiveToolNames(config.Agent{AllowedTools: configured}, true))
}

func TestCompleteGoalToolInfoIsIndependentOfGoalState(t *testing.T) {
	t.Parallel()

	service := goal.NewService()
	toolInfo := func() any { return agenttools.NewCompleteGoalTool(goalCompleter(service)).Info() }
	initial := toolInfo()
	require.Equal(t, initial, agenttools.NewCompleteGoalTool(goalCompleter(nil)).Info())

	ticket, _, err := service.RequestInstallation("workspace", "session", "do the work")
	require.NoError(t, err)
	require.Equal(t, initial, toolInfo(), "pending must not change the tool schema")
	_, ok := service.Activate(ticket)
	require.True(t, ok)
	require.Equal(t, initial, toolInfo(), "active must not change the tool schema")
	require.True(t, service.Pause(ticket.Token))
	require.Equal(t, initial, toolInfo(), "paused must not change the tool schema")
	_, err = service.Complete(ticket.Token, goal.Completion{Result: "done", Success: true})
	require.NoError(t, err)
	require.Equal(t, initial, toolInfo(), "closed must not change the tool schema")
}

type goalBoundaryModel struct {
	*finishStreamModel
	t                      *testing.T
	service                *goal.Service
	ticket                 goal.Ticket
	messages               message.Service
	sessionID              string
	wantHidden             bool
	pendingBeforeStream    bool
	activeBeforeStream     bool
	activeBeforeFirstEvent bool
}

func (m *goalBoundaryModel) Stream(ctx context.Context, call fantasy.Call) (fantasy.StreamResponse, error) {
	m.t.Helper()
	messages, err := m.messages.List(ctx, m.sessionID)
	require.NoError(m.t, err)
	var latestUser *message.TextContent
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].Role != message.User {
			continue
		}
		for _, part := range messages[i].Parts {
			if text, ok := part.(message.TextContent); ok {
				latestUser = &text
				break
			}
		}
		if latestUser != nil {
			break
		}
	}
	require.NotNil(m.t, latestUser, "user prompt must be persisted before provider dispatch")
	require.Equal(m.t, m.wantHidden, latestUser.Hidden)

	snapshot := m.service.Get()
	if m.pendingBeforeStream {
		require.True(m.t, m.service.IsPending(m.ticket), "goal must still be pending when the inner Stream is entered")
	}
	if m.activeBeforeStream {
		require.True(m.t, snapshot.Active)
	}

	inner, err := m.finishStreamModel.Stream(ctx, call)
	if err != nil {
		return nil, err
	}
	return func(yield func(fantasy.StreamPart) bool) {
		if m.activeBeforeFirstEvent {
			require.True(m.t, m.service.Get().Active, "activation must happen after Stream returns but before its first event is read")
		}
		inner(yield)
	}, nil
}

func addGoalTestSeedMessage(t *testing.T, env fakeEnv, sessionID string) {
	t.Helper()
	_, err := env.messages.Create(t.Context(), sessionID, message.CreateMessageParams{
		Role:  message.User,
		Parts: []message.ContentPart{message.TextContent{Text: "earlier conversation"}},
	})
	require.NoError(t, err)
}

type cancelPendingOnAssistantCreate struct {
	message.Service
	service *goal.Service
	ticket  goal.Ticket
}

func (m cancelPendingOnAssistantCreate) Create(ctx context.Context, sessionID string, params message.CreateMessageParams) (message.Message, error) {
	if params.Role == message.Assistant {
		m.service.CancelPending(m.ticket)
	}
	return m.Service.Create(ctx, sessionID, params)
}

type foldedCancellationModel struct {
	*finishStreamModel
	t                 *testing.T
	ordinaryPrompt    string
	revokedGoalPrompt string
}

func (m *foldedCancellationModel) Stream(ctx context.Context, call fantasy.Call) (fantasy.StreamResponse, error) {
	m.t.Helper()
	foundOrdinaryPrompt := false
	foundRevokedGoalPrompt := false
	for _, promptMessage := range call.Prompt {
		for _, part := range promptMessage.Content {
			text, ok := part.(fantasy.TextPart)
			if !ok {
				continue
			}
			foundOrdinaryPrompt = foundOrdinaryPrompt || strings.Contains(text.Text, m.ordinaryPrompt)
			foundRevokedGoalPrompt = foundRevokedGoalPrompt || strings.Contains(text.Text, m.revokedGoalPrompt)
		}
	}
	require.True(m.t, foundOrdinaryPrompt, "the unrelated turn must still be sent")
	require.False(m.t, foundRevokedGoalPrompt, "the canceled folded initial must be removed before dispatch")
	return m.finishStreamModel.Stream(ctx, call)
}

func newGoalBoundaryAgent(env fakeEnv, service *goal.Service, model fantasy.LanguageModel, tools ...fantasy.AgentTool) *sessionAgent {
	large := Model{
		Model: model,
		CatwalkCfg: catwalk.Model{
			ContextWindow:    200000,
			DefaultMaxTokens: 10000,
		},
	}
	return NewSessionAgent(SessionAgentOptions{
		Models:          fixedModels(large, large),
		Sessions:        env.sessions,
		Messages:        env.messages,
		Goal:            service,
		GoalWorkspaceID: "workspace",
		Tools:           tools,
	}).(*sessionAgent)
}

func TestGoalInitialPersistsVisibleAndActivatesAtStreamBoundary(t *testing.T) {
	t.Parallel()

	env := testEnv(t)
	sess, err := env.sessions.Create(t.Context(), "goal session")
	require.NoError(t, err)
	addGoalTestSeedMessage(t, env, sess.ID)
	service := goal.NewService()
	ticket, _, err := service.RequestInstallation("workspace", sess.ID, "finish the research")
	require.NoError(t, err)
	prompt, err := GoalInitialPrompt(ticket.Text)
	require.NoError(t, err)

	model := &goalBoundaryModel{
		finishStreamModel:      &finishStreamModel{text: "Research complete."},
		t:                      t,
		service:                service,
		ticket:                 ticket,
		messages:               env.messages,
		sessionID:              sess.ID,
		pendingBeforeStream:    true,
		activeBeforeFirstEvent: true,
	}
	sa := newGoalBoundaryAgent(env, service, model)
	result, err := sa.Run(t.Context(), SessionAgentCall{
		SessionID:  sess.ID,
		Prompt:     prompt,
		GoalMarker: goal.CallMarker{Kind: goal.CallInitial, Ticket: ticket},
	})
	require.NoError(t, err)
	require.NotNil(t, result)
	require.True(t, service.Get().Active)

	messages, err := env.messages.List(t.Context(), sess.ID)
	require.NoError(t, err)
	var userMessages []message.Message
	for _, msg := range messages {
		if msg.Role == message.User {
			userMessages = append(userMessages, msg)
		}
	}
	require.Len(t, userMessages, 2)
	text, ok := userMessages[len(userMessages)-1].Parts[0].(message.TextContent)
	require.True(t, ok)
	require.False(t, text.Hidden)
	require.Equal(t, prompt, text.Text)
}

func TestGoalFollowupPersistsHiddenAndUsesNormalRunPath(t *testing.T) {
	t.Parallel()

	env := testEnv(t)
	sess, err := env.sessions.Create(t.Context(), "goal session")
	require.NoError(t, err)
	addGoalTestSeedMessage(t, env, sess.ID)
	service := goal.NewService()
	ticket, _, err := service.RequestInstallation("workspace", sess.ID, "finish the research")
	require.NoError(t, err)
	_, ok := service.Activate(ticket)
	require.True(t, ok)
	prompt, err := GoalFollowupPrompt(ticket.Text)
	require.NoError(t, err)

	model := &goalBoundaryModel{
		finishStreamModel:  &finishStreamModel{text: "Continuing the research."},
		t:                  t,
		service:            service,
		ticket:             ticket,
		messages:           env.messages,
		sessionID:          sess.ID,
		wantHidden:         true,
		activeBeforeStream: true,
	}
	sa := newGoalBoundaryAgent(env, service, model)
	call := SessionAgentCall{
		SessionID:  sess.ID,
		Prompt:     prompt,
		GoalMarker: goal.CallMarker{Kind: goal.CallFollowup, Token: ticket.Token},
	}
	sa.activeRequests.Set(sess.ID, &activeCancel{cancel: func() {}})
	result, err := sa.Run(t.Context(), call)
	require.NoError(t, err)
	require.Nil(t, result)
	require.Zero(t, sa.QueuedPrompts(sess.ID), "a busy automatic follow-up must not be queued")
	sa.activeRequests.Del(sess.ID)

	result, err = sa.Run(t.Context(), call)
	require.NoError(t, err)
	require.NotNil(t, result)

	messages, err := env.messages.List(t.Context(), sess.ID)
	require.NoError(t, err)
	require.Len(t, messages, 3)
	text, ok := messages[len(messages)-2].Parts[0].(message.TextContent)
	require.True(t, ok)
	require.True(t, text.Hidden)
	require.Equal(t, prompt, text.Text)
}

func TestFoldedGoalRemovalTargetsTheQueuedOccurrence(t *testing.T) {
	t.Parallel()

	duplicate := fantasy.Message{
		Role:    fantasy.MessageRoleUser,
		Content: []fantasy.MessagePart{fantasy.TextPart{Text: "same prompt"}},
	}
	other := fantasy.Message{
		Role:    fantasy.MessageRoleUser,
		Content: []fantasy.MessagePart{fantasy.TextPart{Text: "other prompt"}},
	}
	delivery := newGoalDelivery(goal.NewService(), goal.Ticket{})
	delivery.setFoldedMessage(duplicate, 1)
	filtered, ok := delivery.withoutFoldedMessage(fantasy.Call{Prompt: fantasy.Prompt{duplicate, other, duplicate}})
	require.True(t, ok)
	require.Equal(t, fantasy.Prompt{other, duplicate}, filtered.Prompt, "an identical earlier prompt must not be removed")
}

type countedGoalStreamModel struct {
	*finishStreamModel
	calls atomic.Int32
}

func (m *countedGoalStreamModel) Stream(ctx context.Context, call fantasy.Call) (fantasy.StreamResponse, error) {
	m.calls.Add(1)
	return m.finishStreamModel.Stream(ctx, call)
}

func TestDisabledGoalDeliveryDoesNotEnterProviderStream(t *testing.T) {
	t.Parallel()

	service := goal.NewService()
	ticket, _, err := service.RequestInstallation("workspace", "session", "goal")
	require.NoError(t, err)
	model := &countedGoalStreamModel{finishStreamModel: &finishStreamModel{text: "should not run"}}
	delivery := newGoalDelivery(service, ticket)
	wrapped := goalDeliveryModel{LanguageModel: model, delivery: delivery, enabled: func() bool { return false }}

	_, err = wrapped.Stream(t.Context(), fantasy.Call{})
	require.ErrorIs(t, err, errGoalDeliveryRevoked)
	require.Zero(t, model.calls.Load())
	require.True(t, service.IsPending(ticket), "the dispatch fence must not mutate the service itself")
}

func TestRevokedFoldedDeliveryAllowsLaterSteps(t *testing.T) {
	t.Parallel()

	service := goal.NewService()
	ticket, _, err := service.RequestInstallation("workspace", "session", "goal")
	require.NoError(t, err)
	_, ok := service.CancelPending(ticket)
	require.True(t, ok)
	delivery := newGoalDelivery(service, ticket)
	folded := fantasy.Message{Role: fantasy.MessageRoleUser, Content: []fantasy.MessagePart{fantasy.TextPart{Text: "folded goal"}}}
	delivery.setFoldedMessage(folded, 0)
	model := goalDeliveryModel{LanguageModel: &finishStreamModel{text: "continued"}, delivery: delivery}

	_, err = model.Stream(t.Context(), fantasy.Call{Prompt: fantasy.Prompt{folded}})
	require.NoError(t, err)
	_, err = model.Stream(t.Context(), fantasy.Call{Prompt: fantasy.Prompt{{Role: fantasy.MessageRoleUser, Content: []fantasy.MessagePart{fantasy.TextPart{Text: "later step"}}}}})
	require.NoError(t, err, "a later step without the revoked folded prompt must continue normally")
}

func TestContinuationKeepsItsTokenUnlessItFoldsAnInitialDelivery(t *testing.T) {
	t.Parallel()

	service := goal.NewService()
	ticket, _, err := service.RequestInstallation("workspace", "session", "new goal")
	require.NoError(t, err)
	oldToken := goal.Token{WorkspaceID: "workspace", SessionID: "session", Revision: ticket.Token.Revision + 1}
	sa := &sessionAgent{goal: service, goalWorkspaceID: "workspace"}
	call := SessionAgentCall{
		SessionID:  "session",
		GoalMarker: goal.CallMarker{Kind: goal.CallContinuation, Token: oldToken},
	}

	token, ok := sa.goalTokenForStep(call, nil)
	require.True(t, ok)
	require.Equal(t, oldToken, token, "a continuation must preserve its accepted chain token")

	delivery := newGoalDelivery(service, ticket)
	delivery.setFoldedMessage(fantasy.Message{Role: fantasy.MessageRoleUser, Content: []fantasy.MessagePart{fantasy.TextPart{Text: "new initial"}}}, 0)
	for _, marker := range []goal.CallMarker{
		{Kind: goal.CallInitial, Ticket: goal.Ticket{Token: oldToken}},
		{Kind: goal.CallFollowup, Token: oldToken},
		{Kind: goal.CallContinuation, Token: oldToken},
	} {
		call.GoalMarker = marker
		token, ok = sa.goalTokenForStep(call, delivery)
		require.True(t, ok)
		require.Equal(t, ticket.Token, token, "the actually folded initial authorizes its own step")
	}
}

type switchGoalModel struct {
	*finishStreamModel
	steps        atomic.Int32
	token        chan goal.Token
	hasGoalText  chan bool
	goalText     string
	completeGoal bool
}

func (m *switchGoalModel) Stream(ctx context.Context, call fantasy.Call) (fantasy.StreamResponse, error) {
	if len(call.Tools) == 0 {
		return m.finishStreamModel.Stream(ctx, call)
	}
	switch m.steps.Add(1) {
	case 1:
		return func(yield func(fantasy.StreamPart) bool) {
			if !yield(fantasy.StreamPart{
				Type:          fantasy.StreamPartTypeToolCall,
				ID:            "switch-goal-call",
				ToolCallName:  "switch_goal",
				ToolCallInput: "{}",
			}) {
				return
			}
			yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeFinish, FinishReason: fantasy.FinishReasonToolCalls})
		}, nil
	case 2:
		token, _ := goal.TrustedTokenFromContext(ctx)
		m.token <- token
		foundGoalText := false
		for _, prompt := range call.Prompt {
			for _, part := range prompt.Content {
				if text, ok := part.(fantasy.TextPart); ok && strings.Contains(text.Text, m.goalText) {
					foundGoalText = true
				}
			}
		}
		m.hasGoalText <- foundGoalText
		if !m.completeGoal {
			return m.finishStreamModel.Stream(ctx, call)
		}
		return func(yield func(fantasy.StreamPart) bool) {
			if !yield(fantasy.StreamPart{
				Type:          fantasy.StreamPartTypeToolCall,
				ID:            "complete-goal-call",
				ToolCallName:  agenttools.CompleteGoalToolName,
				ToolCallInput: `{"result":"completed by the test","success":true}`,
			}) {
				return
			}
			yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeFinish, FinishReason: fantasy.FinishReasonToolCalls})
		}, nil
	default:
		return m.finishStreamModel.Stream(ctx, call)
	}
}

func newGoalSwitchTool(service *goal.Service, first goal.Ticket, sessionID string, sa **sessionAgent, ticketCh chan<- goal.Ticket) fantasy.AgentTool {
	return fantasy.NewAgentTool("switch_goal", "Replace the test goal.", func(context.Context, struct{}, fantasy.ToolCall) (fantasy.ToolResponse, error) {
		if _, err := service.Cancel(first.Token); err != nil {
			return fantasy.ToolResponse{}, err
		}
		ticket, _, err := service.RequestInstallation("workspace", sessionID, "finish the replacement goal")
		if err != nil {
			return fantasy.ToolResponse{}, err
		}
		prompt, err := GoalInitialPrompt(ticket.Text)
		if err != nil {
			return fantasy.ToolResponse{}, err
		}
		(*sa).enqueueCall(SessionAgentCall{
			SessionID: sessionID,
			Prompt:    prompt,
			GoalMarker: goal.CallMarker{
				Kind:   goal.CallInitial,
				Ticket: ticket,
			},
		})
		ticketCh <- ticket
		return fantasy.NewTextResponse("replaced"), nil
	})
}

type blockGoalTerminalUpdate struct {
	message.Service
	started chan struct{}
	release chan struct{}
	once    sync.Once
}

func (m *blockGoalTerminalUpdate) Update(ctx context.Context, msg message.Message) error {
	if msg.Role == message.Assistant && msg.FinishReason() == message.FinishReasonEndTurn {
		m.once.Do(func() {
			close(m.started)
			<-m.release
		})
		ctx = context.WithoutCancel(ctx)
	}
	return m.Service.Update(ctx, msg)
}

func TestNewFoldedGoalGetsItsOwnDeliveryAndTrustedToken(t *testing.T) {
	t.Parallel()

	env := testEnv(t)
	sess, err := env.sessions.Create(t.Context(), "goal session")
	require.NoError(t, err)
	addGoalTestSeedMessage(t, env, sess.ID)
	service := goal.NewService()
	first, _, err := service.RequestInstallation("workspace", sess.ID, "finish the first goal")
	require.NoError(t, err)
	firstPrompt, err := GoalInitialPrompt(first.Text)
	require.NoError(t, err)

	var sa *sessionAgent
	secondTicket := make(chan goal.Ticket, 1)
	switchTool := newGoalSwitchTool(service, first, sess.ID, &sa, secondTicket)
	model := &switchGoalModel{
		finishStreamModel: &finishStreamModel{text: "The replacement goal is complete."},
		token:             make(chan goal.Token, 1),
		hasGoalText:       make(chan bool, 1),
		goalText:          "finish the replacement goal",
		completeGoal:      true,
	}
	sa = newGoalBoundaryAgent(env, service, model, switchTool, agenttools.NewCompleteGoalTool(service))
	sa.enqueueCall(SessionAgentCall{
		SessionID: sess.ID,
		Prompt:    firstPrompt,
		GoalMarker: goal.CallMarker{
			Kind:   goal.CallInitial,
			Ticket: first,
		},
	})

	result, err := sa.Run(t.Context(), SessionAgentCall{SessionID: sess.ID, Prompt: "start the work"})
	require.NoError(t, err)
	require.NotNil(t, result)
	var replacement goal.Ticket
	select {
	case replacement = <-secondTicket:
	case <-time.After(2 * time.Second):
		t.Fatal("replacement goal initial was not queued")
	}
	select {
	case got := <-model.token:
		require.Equal(t, replacement.Token, got, "the replacement initial must authorize its own tool call")
	case <-time.After(2 * time.Second):
		t.Fatal("replacement goal step did not start")
	}
	select {
	case found := <-model.hasGoalText:
		require.True(t, found, "the replacement initial must be present in the model prompt")
	case <-time.After(2 * time.Second):
		t.Fatal("replacement goal prompt was not inspected")
	}
	require.False(t, service.Get().Active, "complete_goal from the replacement step must close the replacement")
	require.False(t, service.Get().Pending)
}

func TestAgentCancelPausesReplacementGoalInSameRun(t *testing.T) {
	t.Parallel()

	env := testEnv(t)
	sess, err := env.sessions.Create(t.Context(), "goal session")
	require.NoError(t, err)
	addGoalTestSeedMessage(t, env, sess.ID)
	service := goal.NewService()
	first, _, err := service.RequestInstallation("workspace", sess.ID, "finish the first goal")
	require.NoError(t, err)
	firstPrompt, err := GoalInitialPrompt(first.Text)
	require.NoError(t, err)

	var sa *sessionAgent
	replacementTicket := make(chan goal.Ticket, 1)
	switchTool := newGoalSwitchTool(service, first, sess.ID, &sa, replacementTicket)
	model := &switchGoalModel{
		finishStreamModel: &finishStreamModel{text: "The replacement goal is still active."},
		token:             make(chan goal.Token, 1),
		hasGoalText:       make(chan bool, 1),
		goalText:          "finish the replacement goal",
	}
	sa = newGoalBoundaryAgent(env, service, model, switchTool)
	blockedUpdate := &blockGoalTerminalUpdate{
		Service: env.messages,
		started: make(chan struct{}),
		release: make(chan struct{}),
	}
	sa.messages = blockedUpdate
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(blockedUpdate.release) }) }
	t.Cleanup(release)
	sa.enqueueCall(SessionAgentCall{
		SessionID: sess.ID,
		Prompt:    firstPrompt,
		GoalMarker: goal.CallMarker{
			Kind:   goal.CallInitial,
			Ticket: first,
		},
	})

	runDone := make(chan error, 1)
	go func() {
		_, err := sa.Run(t.Context(), SessionAgentCall{SessionID: sess.ID, Prompt: "start the work"})
		runDone <- err
	}()
	var replacement goal.Ticket
	select {
	case replacement = <-replacementTicket:
	case <-time.After(2 * time.Second):
		t.Fatal("replacement goal initial was not queued")
	}
	select {
	case <-blockedUpdate.started:
	case <-time.After(2 * time.Second):
		t.Fatal("replacement run did not reach its terminal message update")
	}
	require.True(t, service.Get().Active)
	require.Equal(t, replacement.Token, goalTokenForSnapshot(service.Get()))

	sa.Cancel(sess.ID)
	require.True(t, service.IsPaused(replacement.Token), "AgentCancel must pause the currently delivered replacement, not its stale predecessor")
	release()
	select {
	case <-runDone:
	case <-time.After(2 * time.Second):
		t.Fatal("replacement run did not exit after its terminal update was released")
	}
}

func TestRemoveGoalInitialDropsOnlyTheCanceledQueuedTicket(t *testing.T) {
	t.Parallel()

	env := testEnv(t)
	sess, err := env.sessions.Create(t.Context(), "goal session")
	require.NoError(t, err)
	service := goal.NewService()
	pending, _, err := service.RequestInstallation("workspace", sess.ID, "cancel this")
	require.NoError(t, err)
	other := goal.Ticket{Token: goal.Token{WorkspaceID: "workspace", SessionID: sess.ID, Revision: pending.Token.Revision + 1}}
	sa := newGoalBoundaryAgent(env, service, &finishStreamModel{text: "unused"})
	sa.enqueueCall(SessionAgentCall{SessionID: sess.ID, Prompt: "canceled initial", GoalMarker: goal.CallMarker{Kind: goal.CallInitial, Ticket: pending}})
	sa.enqueueCall(SessionAgentCall{SessionID: sess.ID, Prompt: "ordinary work"})
	sa.enqueueCall(SessionAgentCall{SessionID: sess.ID, Prompt: "other initial", GoalMarker: goal.CallMarker{Kind: goal.CallInitial, Ticket: other}})

	_, err = service.Cancel(pending.Token)
	require.NoError(t, err)
	sa.RemoveGoalInitial(pending.Token)
	require.Equal(t, []string{"ordinary work", "other initial"}, sa.QueuedPromptsList(sess.ID))
}

func TestCanceledFoldedInitialDoesNotAbortOrdinaryRun(t *testing.T) {
	t.Parallel()

	env := testEnv(t)
	sess, err := env.sessions.Create(t.Context(), "goal session")
	require.NoError(t, err)
	addGoalTestSeedMessage(t, env, sess.ID)
	service := goal.NewService()
	ticket, _, err := service.RequestInstallation("workspace", sess.ID, "finish the research")
	require.NoError(t, err)
	initialPrompt, err := GoalInitialPrompt(ticket.Text)
	require.NoError(t, err)

	model := &foldedCancellationModel{
		finishStreamModel: &finishStreamModel{text: "Ordinary work continues."},
		t:                 t,
		ordinaryPrompt:    "ordinary work",
		revokedGoalPrompt: strings.TrimSpace(initialPrompt),
	}
	sa := newGoalBoundaryAgent(env, service, model)
	sa.messages = cancelPendingOnAssistantCreate{Service: env.messages, service: service, ticket: ticket}
	sa.enqueueCall(SessionAgentCall{
		SessionID: sess.ID,
		Prompt:    initialPrompt,
		GoalMarker: goal.CallMarker{
			Kind:   goal.CallInitial,
			Ticket: ticket,
		},
	})

	result, err := sa.Run(t.Context(), SessionAgentCall{SessionID: sess.ID, Prompt: "ordinary work"})
	require.NoError(t, err)
	require.NotNil(t, result, "revoking the folded initial must not discard the unrelated run")
	require.False(t, service.Get().Active)
	require.False(t, service.Get().Pending)

	messages, err := env.messages.List(t.Context(), sess.ID)
	require.NoError(t, err)
	var persistedInitial bool
	for _, msg := range messages {
		if msg.Role != message.User || msg.Content().String() != initialPrompt {
			continue
		}
		text, ok := msg.Parts[0].(message.TextContent)
		if ok && !text.Hidden {
			persistedInitial = true
		}
	}
	require.True(t, persistedInitial, "the already-persisted visible initial remains in history")
}

func TestCanceledAcceptedInitialClearsPendingInstallation(t *testing.T) {
	t.Parallel()

	env := testEnv(t)
	sess, err := env.sessions.Create(t.Context(), "goal session")
	require.NoError(t, err)
	service := goal.NewService()
	ticket, _, err := service.RequestInstallation("workspace", sess.ID, "do the work")
	require.NoError(t, err)
	sa := NewSessionAgent(SessionAgentOptions{
		Models:          noModels,
		Sessions:        env.sessions,
		Messages:        env.messages,
		Goal:            service,
		GoalWorkspaceID: "workspace",
	}).(*sessionAgent)
	accepted := sa.BeginAccepted(sess.ID)
	sa.Cancel(sess.ID)

	_, err = sa.Run(t.Context(), SessionAgentCall{
		SessionID:  sess.ID,
		Prompt:     "visible initial goal prompt",
		GoalMarker: goal.CallMarker{Kind: goal.CallInitial, Ticket: ticket},
		Accepted:   accepted,
	})
	require.NoError(t, err)
	require.False(t, service.Get().Pending, "a canceled initial accepted before dispatch cannot remain pending")

	messages, err := env.messages.List(t.Context(), sess.ID)
	require.NoError(t, err)
	require.Len(t, messages, 2)
	text, ok := messages[0].Parts[0].(message.TextContent)
	require.True(t, ok)
	require.False(t, text.Hidden)
}

func TestStaleInitialQueueDropDoesNotTouchOtherCalls(t *testing.T) {
	t.Parallel()

	env := testEnv(t)
	sess, err := env.sessions.Create(t.Context(), "goal session")
	require.NoError(t, err)
	service := goal.NewService()
	ticket, _, err := service.RequestInstallation("workspace", sess.ID, "finish the research")
	require.NoError(t, err)
	sa := newGoalBoundaryAgent(env, service, &finishStreamModel{text: "unused"})
	sa.enqueueCall(SessionAgentCall{
		SessionID: sess.ID,
		Prompt:    "initial",
		GoalMarker: goal.CallMarker{
			Kind:   goal.CallInitial,
			Ticket: ticket,
		},
	})
	sa.enqueueCall(SessionAgentCall{SessionID: sess.ID, Prompt: "ordinary queued prompt"})
	_, ok := service.CancelPending(ticket)
	require.True(t, ok)

	fold, dropped := sa.drainQueueForStep(sess.ID)
	require.Len(t, dropped, 1)
	require.Equal(t, goal.CallInitial, dropped[0].GoalMarker.Kind)
	require.Len(t, fold, 1)
	require.Equal(t, "ordinary queued prompt", fold[0].Prompt)
	sa.settleDroppedCalls(dropped)
}

func TestGoalTerminalEligibilityAndPauseResume(t *testing.T) {
	t.Parallel()

	answered := csync.NewMap[string, struct{}]()
	normal := &fantasy.AgentResult{Steps: []fantasy.StepResult{{Response: fantasy.Response{FinishReason: fantasy.FinishReasonStop}}}}
	state := &goalRunState{}
	require.True(t, goalTerminalIsNormal(normal, nil, state, nil, answered))
	require.False(t, goalTerminalIsNormal(nil, nil, state, nil, answered))
	require.False(t, goalTerminalIsNormal(&fantasy.AgentResult{Steps: []fantasy.StepResult{{Response: fantasy.Response{FinishReason: fantasy.FinishReasonLength}}}}, nil, state, nil, answered))
	require.False(t, goalTerminalIsNormal(normal, context.Canceled, state, nil, answered))
	state.stopTurn.Store(true)
	require.False(t, goalTerminalIsNormal(normal, nil, state, nil, answered))

	env := testEnv(t)
	sess, err := env.sessions.Create(t.Context(), "goal session")
	require.NoError(t, err)
	addGoalTestSeedMessage(t, env, sess.ID)
	service := goal.NewService()
	ticket, _, err := service.RequestInstallation("workspace", sess.ID, "do the work")
	require.NoError(t, err)
	_, ok := service.Activate(ticket)
	require.True(t, ok)
	sa := newGoalBoundaryAgent(env, service, &finishStreamModel{text: "done"})
	generation := sa.nextGoalRunGeneration(sess.ID)
	sa.finishGoalRun(sess.ID, &goalRunState{
		generation: generation,
		token:      ticket.Token,
		hasToken:   true,
		dispatched: true,
	}, &fantasy.AgentResult{Steps: []fantasy.StepResult{{Response: fantasy.Response{FinishReason: fantasy.FinishReasonContentFilter}}}}, nil, nil, answered)
	require.True(t, service.IsPaused(ticket.Token))

	sa.activeRequests.Set(sess.ID, &activeCancel{cancel: func() {}})
	sa.enqueueCall(SessionAgentCall{SessionID: sess.ID, Prompt: "queued"})
	sa.ClearQueue(sess.ID)
	require.True(t, service.IsPaused(ticket.Token), "ClearQueue alone must not resume or pause a goal")
	sa.activeRequests.Del(sess.ID)

	sa.resumeGoalForCall(SessionAgentCall{SessionID: sess.ID, GoalMarker: goal.CallMarker{Kind: goal.CallContinuation, Token: ticket.Token}})
	require.True(t, service.IsPaused(ticket.Token), "continuations do not resume paused goals")
	result, err := sa.Run(t.Context(), SessionAgentCall{SessionID: sess.ID, Prompt: "real user input"})
	require.NoError(t, err)
	require.NotNil(t, result)
	require.False(t, service.IsPaused(ticket.Token), "an admitted ordinary main run resumes the active goal")
}

func TestFinishGoalRunCancelsUnstartedInitialTicket(t *testing.T) {
	t.Parallel()

	service := goal.NewService()
	ticket, _, err := service.RequestInstallation("workspace", "session", "do the work")
	require.NoError(t, err)
	_, ok := service.Activate(ticket)
	require.True(t, ok)
	_, err = service.Cancel(ticket.Token)
	require.NoError(t, err)
	ticket, _, err = service.RequestInstallation("workspace", "session", "do the next work")
	require.NoError(t, err)

	env := testEnv(t)
	sa := newGoalBoundaryAgent(env, service, &finishStreamModel{text: "unused"})
	sa.finishGoalRun("session", &goalRunState{initialTicket: &ticket}, nil, nil, nil, csync.NewMap[string, struct{}]())
	require.False(t, service.IsPending(ticket), "a terminal pre-activation failure must not leave a pending installation")
}

func TestGoalWakeAdmissionYieldsToUserIngressDuringPreflight(t *testing.T) {
	t.Parallel()

	service := goal.NewService()
	ticket, _, err := service.RequestInstallation("workspace", "session", "do the work")
	require.NoError(t, err)
	_, ok := service.Activate(ticket)
	require.True(t, ok)
	wake, ok := service.ClaimWake(ticket.Token)
	require.True(t, ok)
	wakes := make(chan struct{}, 1)
	c := &coordinator{
		goal:                  service,
		goalWake:              func() { wakes <- struct{}{} },
		goalRuns:              make(map[string]int),
		goalWakePending:       make(map[string]goal.Token),
		goalIngressGeneration: make(map[string]uint64),
	}
	admitted, marker := c.beginGoalRunWithIngress("session", goal.CallMarker{
		Kind:  goal.CallFollowup,
		Token: ticket.Token,
		Wake:  wake,
	}, false)
	require.True(t, admitted)
	admitted, _ = c.beginGoalRunWithIngress("session", goal.CallMarker{}, true)
	require.True(t, admitted)

	sa := &sessionAgent{
		goal:              service,
		goalWorkspaceID:   "workspace",
		goalWakeAdmission: c.admitGoalWake,
		activeRequests:    csync.NewMap[string, *activeCancel](),
		messageQueue:      csync.NewMap[string, []SessionAgentCall](),
		acceptedRuns:      csync.NewMap[string, int](),
	}
	sa.activeRequests.Set("session", &activeCancel{})
	registered := false
	require.False(t, sa.admitGoalFollowupLocked(SessionAgentCall{
		SessionID:  "session",
		GoalMarker: marker,
	}, func() { registered = true }))
	require.False(t, registered, "an ordinary request received during preflight must fence the fresh wake")
	require.Equal(t, ticket.Token, c.goalWakePending["session"], "the wake must be deferred until the accepted operation finishes")
	service.ReleaseWake(wake)
	sa.activeRequests.Del("session")
	c.endGoalRun("session")
	select {
	case <-wakes:
		t.Fatal("a deferred reminder must wait for the ordinary operation")
	default:
	}
	c.endGoalRun("session")
	select {
	case <-wakes:
	case <-time.After(2 * time.Second):
		t.Fatal("the deferred reminder was lost after the ordinary operation")
	}
}

func TestCoordinatorDefersGoalWakeUntilBusyOperationEnds(t *testing.T) {
	t.Parallel()

	service := goal.NewService()
	ticket, _, err := service.RequestInstallation("workspace", "session", "do the work")
	require.NoError(t, err)
	_, ok := service.Activate(ticket)
	require.True(t, ok)
	wakes := make(chan struct{}, 1)
	c := &coordinator{
		goal:                  service,
		goalWake:              func() { wakes <- struct{}{} },
		goalRuns:              make(map[string]int),
		goalWakePending:       make(map[string]goal.Token),
		goalIngressGeneration: make(map[string]uint64),
	}
	admitted, _ := c.beginGoalRunWithIngress("session", goal.CallMarker{}, false)
	require.True(t, admitted)
	c.DeferGoalWake(ticket.Token)
	select {
	case <-wakes:
		t.Fatal("the reminder must wait for the busy coordinator operation")
	default:
	}

	c.endGoalRun("session")
	select {
	case <-wakes:
	case <-time.After(2 * time.Second):
		t.Fatal("the deferred reminder was not published when the operation ended")
	}
}

func TestGoalWakeWaitsForCoordinatorRunToFinish(t *testing.T) {
	t.Parallel()

	service := goal.NewService()
	ticket, _, err := service.RequestInstallation("workspace", "session", "do the work")
	require.NoError(t, err)
	_, ok := service.Activate(ticket)
	require.True(t, ok)
	wakes := make(chan struct{}, 1)
	c := &coordinator{goal: service, goalWake: func() { wakes <- struct{}{} }}
	require.True(t, c.beginGoalRun("session", goal.CallMarker{}))
	c.signalGoalWake(ticket.Token)
	select {
	case <-wakes:
		t.Fatal("wake was emitted before the coordinator run settled")
	default:
	}
	c.endGoalRun("session")
	select {
	case <-wakes:
	default:
		t.Fatal("wake was not emitted after the coordinator run settled")
	}
}

func TestCancelPausesOnlyTheGoalOwnedByTheActiveRun(t *testing.T) {
	t.Parallel()

	env := testEnv(t)
	sess, err := env.sessions.Create(t.Context(), "goal session")
	require.NoError(t, err)
	service := goal.NewService()
	first, _, err := service.RequestInstallation("workspace", sess.ID, "first goal")
	require.NoError(t, err)
	_, ok := service.Activate(first)
	require.True(t, ok)
	sa := newGoalBoundaryAgent(env, service, &finishStreamModel{text: "unused"})
	var canceled bool
	sa.activeRequests.Set(sess.ID, &activeCancel{
		cancel:       func() { canceled = true },
		goalToken:    first.Token,
		hasGoalToken: true,
	})
	sa.Cancel(sess.ID)
	require.True(t, canceled)
	require.True(t, service.IsPaused(first.Token))

	_, err = service.Cancel(first.Token)
	require.NoError(t, err)
	second, _, err := service.RequestInstallation("workspace", sess.ID, "replacement goal")
	require.NoError(t, err)
	_, ok = service.Activate(second)
	require.True(t, ok)
	sa.activeRequests.Set(sess.ID, &activeCancel{
		cancel:       func() {},
		goalToken:    first.Token,
		hasGoalToken: true,
	})
	sa.Cancel(sess.ID)
	require.False(t, service.IsPaused(second.Token), "a stale run cancel must not pause a replacement goal")
}

func TestCancelAcceptedRunPausesItsCapturedGoalAndDropsWake(t *testing.T) {
	t.Parallel()

	env := testEnv(t)
	sess, err := env.sessions.Create(t.Context(), "goal session")
	require.NoError(t, err)
	service := goal.NewService()
	ticket, _, err := service.RequestInstallation("workspace", sess.ID, "do the work")
	require.NoError(t, err)
	_, ok := service.Activate(ticket)
	require.True(t, ok)
	sa := newGoalBoundaryAgent(env, service, &finishStreamModel{text: "unused"})
	accepted := sa.BeginAccepted(sess.ID)
	defer accepted.Close()
	sa.acceptedMu.Lock()
	sa.goalPendingWake[sess.ID] = ticket.Token
	sa.acceptedMu.Unlock()

	sa.Cancel(sess.ID)
	require.True(t, service.IsPaused(ticket.Token), "canceling an accepted main run pauses its captured goal")
	sa.acceptedMu.Lock()
	_, wakePending := sa.goalPendingWake[sess.ID]
	sa.acceptedMu.Unlock()
	require.False(t, wakePending, "a real cancel invalidates the pending terminal wake")
}

func TestSubagentGoalHooksRemainNoops(t *testing.T) {
	t.Parallel()

	service := goal.NewService()
	ticket, _, err := service.RequestInstallation("workspace", "session", "do the work")
	require.NoError(t, err)
	_, ok := service.Activate(ticket)
	require.True(t, ok)
	sa := NewSessionAgent(SessionAgentOptions{
		IsSubAgent:      true,
		Goal:            service,
		GoalWorkspaceID: "workspace",
	}).(*sessionAgent)

	_, ok = sa.goalTokenForStep(SessionAgentCall{SessionID: "session"}, nil)
	require.False(t, ok)
	sa.pauseGoalForCancel("session", &activeCancel{goalToken: ticket.Token, hasGoalToken: true})
	require.False(t, service.IsPaused(ticket.Token))
	require.True(t, service.Pause(ticket.Token))
	sa.resumeGoalForCall(SessionAgentCall{SessionID: "session"})
	require.True(t, service.IsPaused(ticket.Token))
	require.Equal(t, []string{"view"}, effectiveGoalToolNames([]string{"view"}, true, true, nil))
}
