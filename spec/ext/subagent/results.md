# ext/subagent results

The texts ext/subagent sends to a model. Each section is one text; `{{name}}` is replaced by its value.

## types

Agent types:

## type

- {{name}}: {{description}}

## general

General-purpose agent with the same tools as you.

## result

<subagent_result agent_type="{{agent_type}}" outcome="{{outcome}}" steps="{{steps}}">
{{text}}
</subagent_result>

## not_completed

The subagent stopped before it finished ({{outcome}}), so this answer may be incomplete.

## no_answer

The subagent gave no final answer.

## unknown_type

Unknown agent_type "{{agent_type}}". Use one of: {{types}}.

## nested

A subagent cannot start subagents of its own.

## too_many

Too many subagents are running at once (at most {{max}}). Wait for one to finish, or run this one later.

## denied

The subagent was not started: {{reason}}

## cannot_continue

Cannot continue this subagent: its session is gone. Use task_restart to run it again.

## ask_timeout

No answer arrived within {{duration}}. Continue on your own judgment, and state the assumption you made in your final answer.

## no_one_to_ask

Nobody is waiting on this task right now, so there is no one to ask. Continue on your own judgment, and state the assumption you made in your final answer.

## no_model_choice

This agent type takes no model choice; drop model from the call.

## unknown_model

Unknown model "{{model}}". Use one of: {{models}}.

## no_effort_choice

This agent type takes no effort choice; drop effort from the call.

## unknown_effort

Unknown effort "{{effort}}". Use one of: {{efforts}}.

## no_model_resolver

The host resolves no model for subagents, so "{{model}}" cannot start this one.

## no_model_provider

The model "{{model}}" resolved without a provider.
