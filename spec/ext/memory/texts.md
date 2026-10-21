# ext/memory texts

The texts ext/memory sends to a model. Each section is one text; `{{name}}` is replaced by its value.

## instruction_doc

Contents of {{source}}:

{{content}}

## instructions_truncated

[Cut here: the instruction files together are longer than {{limit}} bytes.]

## loaded

Long-term memory for this conversation (user={{user}} project={{project}}). Follow it like the instructions above; when the user now says otherwise, what the user says wins.

{{memories}}

## memory

- [{{id}} user={{user}} project={{project}}] {{content}}

## empty

(empty)

## changed

Long-term memory was changed in another conversation. It now reads:

{{memories}}

## consolidate

Review the conversation above and decide what, if anything, to change in long-term memory.

Long-term memory is loaded at the start of every later conversation it applies to and followed like standing instructions, so keep it small and durable. Each memory has two tags. user is this conversation's user ({{user}}) or empty for every user; project is this conversation's project ({{project}}) or empty for every project. Tag a memory with the user for the user's own preferences, with the project for what holds for anyone in the project, with both for this user in this project.

Save only:
- preferences and working conventions the user stated: tools, style, language, formats, how they want you to work;
- corrections the user made to how you work ("don't do X", "always do Y");
- stable facts about the user or the project that will still matter in later conversations.

Never save:
- the current task: its details, progress, results or next steps;
- anything you inferred that the user did not state or confirm;
- one-off requests, temporary states, or what the project's instruction files already say;
- secrets, credentials, or personal data the user did not ask you to remember.

Current memory:
<memory>
{{memories}}
</memory>

Limits: memories with a user tag use {{user_used}} of {{user_limit}} tokens (about {{user_words}} words); the others use {{shared_used}} of {{shared_limit}} tokens (about {{shared_words}} words).

Rules:
- Prefer changing an existing memory to adding one: update it when the conversation refines or contradicts it, delete it when it is no longer true.
- Each memory is one self-contained sentence of at most 40 words that makes sense without this conversation.
- Both limits must hold after your changes. When a change would pass a limit, first merge related memories, then delete the least useful ones.
- Most conversations need no change. When nothing qualifies, return an empty list.

Reply with only this JSON object and no other text:
{"changes": [{"op": "add", "content": "...", "user": "...", "project": "..."}, {"op": "update", "id": "...", "content": "...", "user": "...", "project": "..."}, {"op": "delete", "id": "..."}]}
In an update, leave out a field to keep it.

## saved

Saved as {{id}}.

## forgotten

Forgot {{id}}.

## shared_read_only

Memory {{id}} is shared with other users, so it cannot be changed or deleted from this conversation.

## bad_tag

The {{tag}} tag must be "{{value}}" (this conversation's) or empty.

## no_memory

There is no memory {{id}}.

## over_limit

Memory is full: memories with {{which}} would use {{used}} of {{limit}} tokens. Merge related memories with replaces, or forget one, first.

## need_content

Give the content to save.
