# ext/peer texts

The texts ext/peer sends to a model. Each section is one text; `{{name}}` is replaced by its value.

## catalog

You can talk to other agents with the a2a_send tool. Each one works on its own and sees only the messages you send it.

<available_agents>
{{agents}}
</available_agents>

## catalog_item

<agent>
<name>{{name}}</name>
<description>{{description}}</description>
{{skills}}</agent>

## catalog_skills

<skills>
{{skills}}
</skills>

## catalog_skill

<skill name={{name}}>{{description}}</skill>

## added

New agents are available through a2a_send:

## added_item

- {{name}}: {{description}}

## removed

These agents are no longer available through a2a_send: {{names}}.

## reply

<agent_reply agent={{agent}} task_id={{task_id}}>
{{text}}
</agent_reply>

## no_reply

(The agent sent no text.)

## outcome

(The agent's turn ended as {{outcome}}.)

## unknown_agent

There is no agent named "{{agent}}" to talk to. Available agents: {{agents}}.

## agent_gone

Agent {{agent}} is no longer available.

## cyclic

Cyclic agent call {{chain}} refused: {{agent}} is already waiting on this conversation.

## needs_input

{{agent}} needs more input.

## ended

{{agent}} ended the exchange as {{state}}.

## ended_with

{{agent}} ended the exchange as {{state}}: {{message}}

## failed

The exchange with {{agent}} failed: {{error}}

## auth_failed

{{agent}} requires authorization that was not provided.

## waiting

Waiting for task {{task_id}}, which is talking to {{agent}} in the same conversation.
