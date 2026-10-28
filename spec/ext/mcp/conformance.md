# ext/mcp conformance

Cases live in `conformance/loop/ext/mcp/`. A case names the extension in `agent.extensions` as `"mcp": options`; an absent field takes the extension's default (`README.md`). The options:

| Field | Meaning |
|---|---|
| `servers` | The servers, in catalog order: `name` (the host's key, its case kept), `instructions`, `tools` and `prompts`. The binding serves each entry from an MCP server of its own, in process, and connects the extension to it, so a case names no address. The server is stateful and streamed, since a server asks the client for input during a call |
| `discovery` | `direct`, `search` or `native`; absent is `auto` |
| `discovery_ratio`, `catalog_ratio`, `context_window` | The extension's ratios and the window `auto` measures against; absent takes the defaults |

A tool is `name`, `description`, `input_schema` (absent means an object schema with a required string `text`), and how it answers: `echo` answers with its `text` argument, `text` answers with that text (with `is_error`, as an error result), `error` fails the call with a protocol error carrying that text, which never reaches the model, and `elicit` asks the client for input — `mode` (`form` or `url`), `message`, `schema` for a form and `url` for a URL — then answers with the action and content the server got. The request fires `mcp_elicitation` in the session whose call asked, which a case scripts like any other point of the extension; without a hook, or with one that fails or declines, the request is declined. A prompt is `name`, `title`, `description`, `arguments` (`name`, `required`) and `text`, where `{{name}}` is replaced by the argument's value; the prompt answers one user message with that text. No case's output carries the address the binding served a server at, since the catalog names it by the host's key alone.
