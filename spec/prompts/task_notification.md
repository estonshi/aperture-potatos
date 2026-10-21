{{#input}}Task {{task_id}} needs input: {{question}}{{#task_send}}
Answer with task_send; you will be notified when it finishes.{{/task_send}}{{#unread}}
Unread messages already in its inbox: {{unread}}.{{/unread}}{{/input}}{{#succeeded}}Task "{{title}}" (attempt {{attempt}}) succeeded.{{/succeeded}}{{#failed}}Task "{{title}}" (attempt {{attempt}}) failed: {{error}}{{/failed}}{{#canceled}}Task "{{title}}" (attempt {{attempt}}) was stopped: {{error}}{{/canceled}}{{#lost}}Task "{{title}}" (attempt {{attempt}}) was still running when the process restarted, so its outcome is unknown; do not assume it finished.{{#task_restart}} Use task_restart to run it again.{{/task_restart}}{{/lost}}{{#summary}}
Summary:
{{summary}}{{/summary}}{{#unprocessed}}
Messages sent to it that it did not process: {{unprocessed}}.{{/unprocessed}}
