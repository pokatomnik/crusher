package agent

import (
	"testing"

	"github.com/charmbracelet/crush/internal/config"
	"github.com/charmbracelet/crush/internal/goal"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestEffectiveToolNamesByRunMode pins how run mode picks a delegation
// style. A TUI can receive a subagent's report as a later turn, so
// delegation detaches; a headless run exits before one could arrive, so
// delegation blocks instead.
func TestEffectiveToolNamesByRunMode(t *testing.T) {
	t.Parallel()

	agentCfg := config.Agent{
		AllowedTools: []string{
			AgentToolName,
			AgentDispatchToolName,
			AgentSendToolName,
			"question",
			"view",
		},
	}

	tests := []struct {
		name        string
		interactive bool
		isSubAgent  bool
		want        []string
	}{
		{
			name:        "interactive delegates detached",
			interactive: true,
			want:        []string{AgentDispatchToolName, AgentSendToolName, "question", "view", "complete_goal"},
		},
		{
			name:        "headless delegates blocking",
			interactive: false,
			want:        []string{AgentToolName, "view", "complete_goal"},
		},
		{
			name:        "subagents neither fan out nor ask",
			interactive: true,
			isSubAgent:  true,
			want:        []string{"view"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			c := &coordinator{interactive: tt.interactive, goal: goal.NewService(), goalWorkspaceID: "workspace"}
			assert.Equal(t, tt.want, c.effectiveToolNames(agentCfg, tt.isSubAgent))
		})
	}
}

// TestEffectiveToolNamesKeepsConfigAuthoritative checks that goal completion
// is added to main agents independently of AllowedTools but still honors an
// explicit configuration disable.
func TestEffectiveToolNamesKeepsConfigAuthoritative(t *testing.T) {
	t.Parallel()

	agentCfg := config.Agent{AllowedTools: []string{"view"}}
	for _, interactive := range []bool{true, false} {
		c := &coordinator{interactive: interactive, goal: goal.NewService(), goalWorkspaceID: "workspace"}
		assert.Equal(t, []string{"view", "complete_goal"}, c.effectiveToolNames(agentCfg, false))
		assert.Equal(t, []string{"view"}, c.effectiveToolNames(agentCfg, true))
	}

	cfg := config.NewTestStore(&config.Config{Options: &config.Options{DisabledTools: []string{"complete_goal"}}})
	c := &coordinator{cfg: cfg, goal: goal.NewService(), goalWorkspaceID: "workspace"}
	assert.Equal(t, []string{"view"}, c.effectiveToolNames(agentCfg, false))
}

// TestResolveSubAgentAccess covers how a requested access level maps onto a
// subagent variant, including plan mode, which may only delegate work it
// could have done itself.
func TestResolveSubAgentAccess(t *testing.T) {
	t.Parallel()

	read := &mockSessionAgent{}
	write := &mockSessionAgent{}
	both := map[string]SessionAgent{accessRead: read, accessWrite: write}
	readOnly := map[string]SessionAgent{accessRead: read}

	t.Run("empty access defaults to read", func(t *testing.T) {
		t.Parallel()
		agent, refusal := resolveSubAgent(both, "", false)
		require.Empty(t, refusal)
		assert.Same(t, read, agent)
	})

	t.Run("write is available to a writing parent", func(t *testing.T) {
		t.Parallel()
		agent, refusal := resolveSubAgent(both, accessWrite, false)
		require.Empty(t, refusal)
		assert.Same(t, write, agent)
	})

	t.Run("plan mode refuses write with a reason", func(t *testing.T) {
		t.Parallel()
		agent, refusal := resolveSubAgent(readOnly, accessWrite, true)
		assert.Nil(t, agent)
		assert.Contains(t, refusal, "read-only")
	})

	t.Run("plan mode still delegates reads", func(t *testing.T) {
		t.Parallel()
		agent, refusal := resolveSubAgent(readOnly, accessRead, true)
		require.Empty(t, refusal)
		assert.Same(t, read, agent)
	})

	t.Run("nonsense access is rejected", func(t *testing.T) {
		t.Parallel()
		agent, refusal := resolveSubAgent(both, "admin", false)
		assert.Nil(t, agent)
		assert.Contains(t, refusal, `"read" or "write"`)
	})
}

// TestReadOnlyParent documents which parents may only launch read-only
// subagents.
func TestReadOnlyParent(t *testing.T) {
	t.Parallel()

	assert.True(t, readOnlyParent(config.AgentPlan))
	assert.False(t, readOnlyParent(config.AgentCoder))
	assert.False(t, readOnlyParent(""))
}
