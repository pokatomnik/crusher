package agent

import (
	"context"
	"errors"
	"reflect"
	"slices"
	"sync"

	"charm.land/fantasy"
	agenttools "github.com/charmbracelet/crush/internal/agent/tools"
	"github.com/charmbracelet/crush/internal/goal"
)

var (
	errGoalDeliveryRevoked  = errors.New("goal initial delivery was revoked before provider dispatch")
	errGoalDeliveryNoStream = errors.New("goal initial model returned a nil stream")
)

type goalDelivery struct {
	service *goal.Service
	ticket  goal.Ticket

	mu                  sync.Mutex
	streamStarted       bool
	activationAttempted bool
	// foldedMessage identifies only the queued initial part, so revoking it
	// before dispatch does not abort the unrelated turn that carried it.
	foldedMessage      *fantasy.Message
	foldedMatchesAfter int
}

func newGoalDelivery(service *goal.Service, ticket goal.Ticket) *goalDelivery {
	return &goalDelivery{service: service, ticket: ticket}
}

func (d *goalDelivery) started() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.streamStarted
}

func (d *goalDelivery) canDispatch() bool {
	d.mu.Lock()
	started := d.streamStarted
	d.mu.Unlock()
	return started || d.service.IsPending(d.ticket)
}

func (d *goalDelivery) setFoldedMessage(message fantasy.Message, matchesAfter int) {
	d.mu.Lock()
	d.foldedMessage = &message
	d.foldedMatchesAfter = matchesAfter
	d.mu.Unlock()
}

func (d *goalDelivery) hasFoldedMessage() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.foldedMessage != nil
}

func (d *goalDelivery) withoutFoldedMessage(call fantasy.Call) (fantasy.Call, bool) {
	d.mu.Lock()
	if d.foldedMessage == nil {
		d.mu.Unlock()
		return call, false
	}
	target := *d.foldedMessage
	matchesAfter := d.foldedMatchesAfter
	d.mu.Unlock()

	prompt := slices.Clone(call.Prompt)
	for i := len(prompt) - 1; i >= 0; i-- {
		if !sameGoalPromptMessage(prompt[i], target) {
			continue
		}
		if matchesAfter > 0 {
			matchesAfter--
			continue
		}
		call.Prompt = slices.Delete(prompt, i, i+1)
		return call, true
	}
	return call, false
}

func sameGoalPromptMessage(left, right fantasy.Message) bool {
	if left.Role != right.Role || len(left.Content) != len(right.Content) {
		return false
	}
	for i, leftPart := range left.Content {
		if text, ok := leftPart.(fantasy.TextPart); ok {
			rightText, ok := right.Content[i].(fantasy.TextPart)
			if !ok || text.Text != rightText.Text {
				return false
			}
			continue
		}
		if !reflect.DeepEqual(leftPart, right.Content[i]) {
			return false
		}
	}
	return true
}

type goalDeliveryModel struct {
	fantasy.LanguageModel
	delivery  *goalDelivery
	enabled   func() bool
	onStarted func(activated bool)
}

func (a *sessionAgent) goalToolEnabled() bool {
	if a.cfg == nil {
		return true
	}
	cfg := a.cfg.Config()
	return cfg == nil || cfg.Options == nil || !slices.Contains(cfg.Options.DisabledTools, agenttools.CompleteGoalToolName)
}

func (m goalDeliveryModel) Stream(ctx context.Context, call fantasy.Call) (fantasy.StreamResponse, error) {
	foldedRevoked := false
	if !m.delivery.canDispatch() || m.enabled != nil && !m.enabled() {
		if !m.delivery.hasFoldedMessage() {
			return nil, errGoalDeliveryRevoked
		}
		call, _ = m.delivery.withoutFoldedMessage(call)
		// A folded initial is only one message in this already-admitted run.
		foldedRevoked = true
	}

	stream, err := m.LanguageModel.Stream(ctx, call)
	if err != nil {
		return nil, err
	}
	if stream == nil {
		return nil, errGoalDeliveryNoStream
	}
	if foldedRevoked {
		if m.onStarted != nil {
			m.onStarted(false)
		}
		return stream, nil
	}

	m.delivery.mu.Lock()
	firstSuccessfulStream := !m.delivery.streamStarted
	m.delivery.streamStarted = true
	activate := firstSuccessfulStream && !m.delivery.activationAttempted
	if activate {
		m.delivery.activationAttempted = true
	}
	m.delivery.mu.Unlock()

	activated := false
	if activate {
		_, activated = m.delivery.service.Activate(m.delivery.ticket)
		if !activated {
			activated = matchesGoalToken(m.delivery.service.Get(), m.delivery.ticket.Token)
		}
	}
	if m.onStarted != nil {
		m.onStarted(activated)
	}
	return stream, nil
}
