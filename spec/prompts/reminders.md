# Fixed runtime texts

Each level-two section is one text the runtime sends to the model. The heading is the key; the body, trimmed of surrounding blank lines, is the text byte for byte. Bodies are templates in the subset of spec/runtime.md, and the variables each one takes are listed in the runtime's fixed-text table.

## dangling_tool_call

Not executed: no result was recorded for this call, so it did not run. Do not assume it had any effect.

## resume_lost

Outcome unknown: the process restarted before this call reported a result. Do not assume it ran; check state before retrying.

## turn_ended_not_executed

Not executed: the turn ended ({{outcome}}) before this call ran. Do not assume it had any effect.

## truncated_call_not_executed

Not executed: the response was cut off before this call was complete. Do not assume it had any effect.

## tool_not_started

Not executed: the turn was interrupted before this call started.

## tool_interrupted

Interrupted: the turn was stopped while this call was running, so it may not have finished. Check its effects before you retry it.

## tool_abandoned

Interrupted: the call did not stop within the grace period after the turn was stopped. Treat its effects as unknown.

## tool_timed_out

Timed out: the call did not finish within its time limit of {{seconds}}s. Treat its effects as unknown.

## tool_result_truncated

[{{omitted}} of the result's {{total}} characters are left out here: it is longer than can be shown. Narrow the request if you need the middle.]

## unknown_tool

There is no tool named "{{name}}". Use one of the tools you have been given.

## invalid_input

Invalid input: the arguments are not a JSON object. Arguments received: {{raw}}

## invalid_input_detail

Invalid input: {{detail}}

## empty_reply_continue

Please continue.

## truncated_text_continue

Your previous response was cut off at the output limit. Continue exactly where it stopped, without repeating what you already wrote.

## interrupted_note

The previous turn was interrupted before it finished.{{#partial}} Its last reply was cut off.{{/partial}}{{#calls}} These tool calls did not complete: {{calls}}.{{/calls}} Do not assume the interrupted work was done.

## merged_omitted

({{count}} earlier items omitted)

## reminder_truncated

[truncated: {{count}} characters omitted]

## tool_result_media

The result of tool call {{call_id}} included these attachments:

## document_link

Document: {{url}}

## tool_removed

This tool is no longer available. Do not call it.

## denied_by_hook

Denied{{#reason}}: {{reason}}{{/reason}}

## denied_by_rule

Denied by rule {{id}}{{#reason}}: {{reason}}{{/reason}}

## denied_by_reviewer

Denied by reviewer{{#reason}}: {{reason}}{{/reason}}

## approval_interrupted

Not executed: interrupted while awaiting approval.

## approval_stopped

Not executed: the turn was stopped by a reviewer while this call awaited approval.

## stop_continue

Continue working: the task is not finished yet.

## section_updated

The section "{{name}}" was updated and supersedes the earlier version:
{{text}}

## section_removed

The section "{{name}}" was removed; disregard what it said.

## compact_continue

Continue the current task from where you left off.

## pin_truncated

(truncated; re-activate for the full text)

## tool_result_cleared

Result cleared to save context. Call the tool again if you still need it.

## pin_removed

The pinned context "{{key}}" no longer applies.

## summary_too_long

A previous attempt at this summary ran past its length limit and was cut off. Write it again more concisely, in about {{target}} tokens: keep every section short, and end with the current progress and the exact next step.

## tools_allowed

Only these tools are available in this conversation: {{names}}. Calls to any other tool are refused.

## tool_not_allowed

Not executed: {{name}} is not available in this conversation.

## side_chat_head

The messages above are the main conversation as it is right now, including any call it is still waiting on. This is a side conversation: the main conversation goes on by itself and does not see it. Earlier replies in this side conversation were written against an older state of the main conversation, so trust the messages above over them.

## side_chat_main_changed

The main conversation above has changed since your last reply in this side conversation{{#first}}: everything from {{first}} on is new{{/first}}. Answer from the main conversation as it is now, not from your earlier replies.

## main_call_pending

No result yet: this call is still in progress in the main conversation.

## side_chat_compact_scope

Summarize only this side conversation, which starts at the note saying the messages before it are the main conversation. The main conversation is always sent in full, so leave it out of the summary.

## task_limit

Too many tasks are running (limit {{limit}}); wait for one to finish or stop one first.

## task_not_started

Task not started: {{reason}}

## task_start_failed

Task could not start: {{error}}

## task_ended

Task {{task_id}} ended ({{status}}): {{error}}

## foreign_truncated

[truncated: {{count}} characters omitted]
