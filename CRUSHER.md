# Crusher fork notes

This file records fork-only behavior and the small upstream integration points
needed to keep the fork close to `charmbracelet/crush`. Do not move these
changes into upstream documentation files.

## Goal

The Goal feature gives the current backend process one in-memory goal slot.
The user starts it from `/` → **Set Goal**, enters a multiline prompt and
confirms with Ctrl+Enter (Cmd+Enter on macOS). Esc closes the editor without
making a request. A root
session owns the goal; child sessions cannot set one. While a goal is pending,
the command palette offers **Cancel Goal**. The **Current goal** strip appears
only after the initial prompt has been saved and the provider stream has
started successfully. It is omitted when there is insufficient terminal space.

The initial instruction and every automatic follow-up are ordinary user
messages in the session transcript. The initial instruction is visible;
follow-ups use the existing hidden-message field. Goal state, owner, pause state,
and automation metadata are never persisted. Restarting Crush drops an active
or paused goal, even though its prompt messages remain in history.

`complete_goal` is an ordinary main-agent tool with the required `result`
(string) and `success` (boolean) parameters. It is present in the main-agent
tool palette regardless of goal state, but absent when explicitly disabled in
`options.disabled_tools`. Either success value closes the active goal. Without
an active goal it returns a successful no-op response. The tool does not stop
the current turn, so the model can provide the requested result afterward.

## Runtime and stop policy

- A single `goal.Service` owns the slot. Local mode creates one service per App;
  server mode shares one service across all workspaces and clients. The slot is
  `empty → pending → active/paused → empty`; a second pending or active goal is
  rejected rather than replacing the first.
- `Pending` is only an accepted installation. It becomes `Active` only after
  the initial user message is persisted and the actual inner model `Stream`
  returns a non-nil stream without error. A queued initial call that is
  cancelled before dispatch does not activate or later reappear.
- Normal terminal stops of the owning main agent trigger one sequential
  follow-up worker. It stops when `complete_goal` closes the slot or the user
  selects **Cancel Goal**. Historical prompt messages are not recalled by
  Cancel Goal; the user can clarify or revoke the instruction with an ordinary
  chat message.
- A real agent cancellation or another non-normal stop pauses reminders while
  keeping the active goal visible. A later admitted main-agent run resumes it.
  Clearing ordinary queued prompts is not a cancellation. There are no
  user-facing Pause/Resume commands.
- Follow-ups are automatic model requests and may incur provider charges. There
  is no fixed reminder-count or cost limit; normal user/tool use, explicit
  cancellation, completion, or a non-normal stop controls the lifecycle.
- The server is authoritative in remote mode. Event snapshots and operation
  responses update clients; client-side state does not run automation. A lost
  event may leave an idle client's display stale until a subsequent snapshot
  fetch or operation response.

## API and configuration

The server exposes Goal-specific operations through the regular endpoint
registry: global snapshot GET, session-scoped Set Goal POST, and token-fenced
Cancel Goal POST. Goal events carry a public snapshot with a monotonically
increasing `Version`; `Revision` identifies one logical installation across
pending and active phases. The full owner token contains workspace ID, session
ID, and revision so late actions cannot cancel a later installation.

Goal is RAM-only and adds no migrations, SQL queries, or generated database
schema. `complete_goal` being listed in `options.disabled_tools` removes both
Goal palette actions and prevents Set Goal at App/backend boundaries. Applying
that disable during config reload cancels the current owner's pending or active
goal; enabling it later does not restore the previous goal.

## Fork-only code map

Most behavior lives in new files. Existing upstream files contain narrow
integration hooks:

- `internal/goal/`: singleton RAM state, pending/active transitions, revision
  tokens, pause/resume, and wake claims.
- `internal/agent/goal_*.go`, embedded templates, and
  `internal/agent/tools/complete_goal.*`: prompt rendering, trusted per-step
  context, provider-stream activation, terminal eligibility, and the tool.
- `internal/app/goal*.go`: local/shared service ownership, API methods, event
  fan-in, owner cleanup, initial delivery, and follow-up worker.
- `internal/backend/goal.go`: server-wide singleton and owner-workspace routing.
- `internal/proto/goal.go`, `internal/pubsub/goal.go`,
  `internal/server/goal.go`, and `internal/client/goal.go`: wire contract and
  Goal-specific HTTP handling.
- `internal/workspace/goal.go`: optional capability implemented by local and
  remote workspaces without expanding the general `Workspace` interface.
- Small integration changes in `internal/agent/agent.go` and
  `internal/agent/coordinator.go` carry the typed marker through normal runs,
  expose the tool only to main agents, and report terminal lifecycle outcomes.
- Small integration changes in `internal/app/app.go` and
  `internal/backend/backend.go` inject the service and stop its owner runtime
  before workspace teardown.
- `internal/server/endpoints.go`, `internal/server/events.go`,
  `internal/client/proto.go`, and `internal/workspace/client_workspace.go`
  register/translate endpoints and global snapshots.
- `internal/ui/dialog/goal.go`, `internal/ui/model/goal*.go`, and small hooks in
  `internal/ui/dialog/commands.go` and `internal/ui/model/ui.go` implement the
  editor, palette action, snapshot handling, and responsive goal strip. The
  existing development notice remains as a commented-out line in `ui.go`.

The existing prompt-history navigation, session schema, general tool hooks,
subagent implementation, and generic server/client error decoders are not
changed for this feature.
