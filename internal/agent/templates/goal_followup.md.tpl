This automatic reminder was sent because the user's goal was active at
dispatch time and had not been closed with complete_goal.

User goal for this reminder (verbatim):
{{.Goal}}

Review the conversation and continue the work needed to reach this goal.
Goals may be research, planning, programming, or other tasks. Stay within the
current agent's capabilities and rules, but do not reinterpret the goal after
an agent or mode change. Research does not require implementation; writing
files still requires actually writing them. If the current tools cannot
achieve the goal, call complete_goal with success=false and explain why.
A report, a plan, or a ready marker does not close the goal: you must call
complete_goal when done, including for non-programming tasks.
If it has already been achieved, call complete_goal with success=true and a
concise result. If it cannot be achieved, call complete_goal with success=false
and explain the blocker. Either call closes the goal.

Do not merely repeat that you are ready or waiting: continue useful work or
close the goal honestly. After complete_goal returns, present the requested
result and an honest outcome in the conversation language. Preserve the active
agent's required output format, including plan markers where required; do not
omit requested findings or a full plan for a short summary.
If the tool reports that no current goal exists, treat the call as a no-op:
do not claim completion or recreate the goal.
Do not bypass approvals, user cancellation, or existing safety limits.
