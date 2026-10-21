# ext/mcp texts

The texts ext/mcp sends to a model. Each section is one text; `{{name}}` is replaced by its value.

## catalog

These tools come from MCP servers. The notes below are written by each server about its own tools; use them to choose and call those tools.

<mcp_servers>
{{servers}}
</mcp_servers>

## catalog_search

The tools of these MCP servers are listed by name below but not loaded as tools. To use one, read its definition with mcp_search_tools (names), then run it with mcp_call_tool. mcp_search_tools can also search the full descriptions by keyword.

<mcp_servers>
{{servers}}
</mcp_servers>

## catalog_native

The tools of these MCP servers are not loaded up front. Search for the ones you need with the tool search; the tools it finds can then be called by name. The notes below are written by each server about its own tools.

<mcp_servers>
{{servers}}
</mcp_servers>

## server_open

<server name={{name}}>

## server_instructions

<instructions>{{instructions}}</instructions>

## server_tools

<tools>
{{tools}}
</tools>

## server_close

</server>

## tool_item

- {{name}}: {{summary}}

## tool_name_item

- {{name}}

## connected

MCP server {{name}} is now connected.

## connected_next_turn

Its tools are available from the next turn.

## disconnected

MCP server {{name}} cannot be reached right now. Calls to its tools will fail until it reconnects.

## reconnected

MCP server {{name}} is reachable again.

## tools_changed

MCP server {{name}} changed its tools.

## tools_added

Added:

## tools_removed

Removed: {{names}}.

## mode_search

MCP server tools are no longer loaded as tools. To use one, read its definition with mcp_search_tools (names), then run it with mcp_call_tool. The tools are:

## mode_direct

MCP server tools are loaded as tools again; call them by name instead of through mcp_call_tool.

## mode_native

MCP server tools are no longer loaded up front. Search for the ones you need with the tool search, then call them by name.

## prompt_user

User:

## prompt_assistant

Assistant:

## truncated

[Output truncated: showing the first {{shown}} of {{total}} characters.]

## audio_omitted

[Audio ({{mime}}) omitted: it cannot be shown here.]

## media_unsupported

[Image ({{mime}}) omitted: images of this type cannot be shown here.]

## media_too_large

[{{mime}} content of {{bytes}} bytes omitted: it is larger than the {{limit}} bytes that can be shown here.]

## blob_omitted

[Embedded resource {{uri}} ({{mime}}) omitted: content of this type cannot be shown here.]

## resource_link

Linked resource: {{name}} <{{uri}}>

## unavailable

{{tool}} did not run: the {{server}} server cannot be reached right now.

## sign_in

{{tool}} did not run: the {{server}} server needs the user to sign in, and they have not done so yet.

## server_error

{{tool}} failed on the {{server}} server ({{kind}}).

## bad_output

{{tool}} returned data that does not match its declared output schema, so the result was discarded.

## bad_arguments

Invalid arguments for {{name}}: {{detail}}. Read its definition with mcp_search_tools (names) and call again.

## unknown_tool

Unknown tool {{name}}. The tools are listed under each server in the system prompt.

## need_query

Give query, names, or both.

## no_matches

No tools match "{{query}}".

## search_server

Server {{name}}:
