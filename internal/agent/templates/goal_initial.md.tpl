The user is setting a goal for this conversation.

User goal (verbatim):
{{.Goal}}

A goal may involve research, analysis, planning, implementation, or other work;
it does not have to involve programming or changing files.
Work toward the user's actual objective using the available tools and the
conversation context, within the current agent's capabilities and rules.
An agent or mode change does not change the goal. Do not reinterpret a goal
to fit the tools: a file-writing goal still requires writing the files, while
a research or planning goal does not require an implementation phase.
If your current capabilities prevent achieving the goal, close it honestly
with complete_goal(success=false) and explain the missing capability.
Do not stop merely because one turn or one intermediate step is finished.
Only the user can set or replace a goal.

When the goal is achieved, call complete_goal with success=true and a concise
result. If the goal cannot be achieved, call complete_goal with success=false
and a concise explanation of the blocker. Either call closes the goal.

Returning a report or a plan does not close the goal; you must call
complete_goal, including for research and planning goals.
After the tool returns, present the requested result and an honest outcome in
the language of the conversation. Follow the active agent's output format,
including plan markers where required; do not replace the requested findings
or full plan with only a completion notice. Do not claim success if success=false.
If the tool reports that no current goal exists, nothing was completed by
that call; do not claim otherwise or recreate the goal.
Follow all existing instructions, approval requirements, and safety limits.
