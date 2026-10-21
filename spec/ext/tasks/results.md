# ext/tasks results

The texts the task tools return to the model. Each section is one text; `{{name}}` is replaced by its value.

## not_found

No task {{task_id}} in this conversation.

## no_instructions

Task {{task_id}} does not accept messages while it runs.

## not_continuable

Task {{task_id}} has finished and cannot be continued. Use task_restart to run it again.

## not_restartable

Task {{task_id}} cannot be restarted.

## busy

Task {{task_id}} is being restarted or continued right now; try again shortly.

## queue_full

Task {{task_id}} has too many unread messages; wait before sending more.

## delivered

Delivered to task {{task_id}} ({{mode}}).

## continuing

Task {{task_id}} is continuing (attempt {{attempt}}); you will be notified when it finishes or needs input.

## stopped

Task {{task_id}} stopped.

## already_finished

Task {{task_id}} had already finished ({{status}}).

## abandoned

Task {{task_id}} did not stop in time and was abandoned; treat its side effects as unknown.

## restarted

Task {{task_id}} restarted (attempt {{attempt}}); you will be notified when it finishes or needs input.

## unfinished

These tasks were still unfinished when the conversation was summarized; they report back on their own:
