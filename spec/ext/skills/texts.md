# ext/skills texts

The texts ext/skills sends to a model. Each section is one text; `{{name}}` is replaced by its value.

## catalog

Skills hold instructions for specialized tasks. When a task matches a skill's description, load the skill with the skill tool before you start, then follow its instructions.

<available_skills>
{{skills}}
</available_skills>

## catalog_item

<skill>
<name>{{name}}</name>
<description>{{description}}</description>
<location>{{location}}</location>
</skill>

## content

<skill_content name={{name}} location={{location}}>
{{body}}
</skill_content>

## resources

<skill_resources>
{{files}}
</skill_resources>

## resource_file

<file>{{path}}</file>

## already_active

Skill {{name}} is already active; its instructions are above.

## unknown_skill

There is no skill named "{{name}}" that you can load. Available skills: {{names}}.

## denied

Loading skill {{name}} was denied: {{reason}}

## no_resource

Skill {{name}} has no resource file "{{path}}".

## invalid_resource

"{{path}}" is not a valid resource path. Use a relative path listed in <skill_resources>.

## resource_binary

"{{path}}" is a binary file and cannot be shown as text.

## resource_truncated

[The file is longer than {{limit}} bytes; only the first {{limit}} bytes are shown.]

## added

New skills are available to load with the skill tool:

## added_item

- {{name}}: {{description}}

## removed

These skills are no longer available: {{names}}.
