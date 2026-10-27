[← Previous: Extensions](13-extensions.md) · [Contents](README.md) · [Next: Context management →](15-context.md)

# 14. Built-in extensions

The SDK ships with a set of extensions that cover compaction, history search, task management, subagents, skills, long-term memory, calling other agents, connecting to MCP servers, slash commands and LiteLLM cost queries. This chapter goes through them one by one: what each gives the model, how to configure it, and what to watch out for when you use it. After reading it, you can tell which capabilities you can simply take from a built-in extension instead of implementing them yourself.

In this chapter: [14.1 ext/compact](#141-extcompact) · [14.2 ext/history](#142-exthistory) · [14.3 ext/tasks](#143-exttasks) · [14.4 ext/subagent](#144-extsubagent) · [14.5 ext/skills](#145-extskills) · [14.6 ext/memory](#146-extmemory) · [14.7 ext/peer](#147-extpeer) · [14.8 ext/mcp](#148-extmcp) · [14.9 ext/cmd](#149-extcmd) · [14.10 ext/litellm](#1410-extlitellm)

The following table lists each built-in extension's package path, its extension name, whether it is installed by default, and what it gives the model. "Installed by default" means that `std.Defaults()` already includes the extension.

| Package | Extension name | Installed by default | What it gives the model |
|---|---|---|---|
| `ext/compact` | `compact` | Yes | A compaction strategy. The model does not see it directly. |
| `ext/history` | `history` | Yes | The `search_history` tool. |
| `ext/tasks` | `tasks` | Yes | Four tools: `task_status`, `task_send`, `task_stop` and `task_restart`. |
| `ext/subagent` | `subagent` | Yes | The `subagent` tool, plus `ask_parent` inside subagents. |
| `ext/skills` | `skills` | No | The `skill` tool, the `<available_skills>` catalog and `/<skill>` commands. |
| `ext/memory` | `memory` | No | Writes instruction files and memories at the start of every epoch, and provides the `memory_save` and `memory_forget` tools. |
| `ext/peer` | `peer` | No | The `a2a_send` tool and the `<available_agents>` catalog. |
| `ext/mcp` | `mcp` | No | The server's tools, `mcp_search_tools` and `mcp_call_tool`, and commands generated from the server's prompts. |
| `ext/cmd/*` | `cmd.<name>` | Yes | Seven slash commands. |
| `ext/litellm` | Not an extension | N/A | Tags requests and serves as a cost source. |

## 14.1 ext/compact

`ext/compact` is the default compaction strategy. When it compacts, it has the model write a summary of the current context. You can also have it run a cheaper cleanup pass before calling the model, which replaces old tool results with placeholders. If the context is small enough after this cleanup, the model is not called at all.

```go
std.Defaults(std.With(compact.New(compact.Options{
	SummaryPrompt:   "...",                                   // replaces the default summary prompt
	DropCurrentTurn: false,                                   // when true, compaction also folds the running turn into the summary
	Prune:           &compact.PruneOptions{KeepRecent: 4, MinTokens: 2000}, // clears old tool results first and skips the model call if the context is then small enough
})))
```

`Options` has the following fields.

| Field | Description |
|---|---|
| `SummaryPrompt` | The prompt used for the summary. The default prompt is in `spec/ext/compact/summary.md`. |
| `DropCurrentTurn` | By default, the turn that is running is kept as it is and is not compacted. |
| `Prune` | Settings for clearing old tool results. `KeepRecent` is the number of most recent tool results to leave alone; 0 uses the default, and a negative value clears all of them. `MinTokens` leaves alone any tool result smaller than that many tokens. |

A cleared tool result has its `Cleared` field set to true in events and in history. The original text is still kept in the store, and the model can retrieve it with `search_history`.

## 14.2 ext/history

`ext/history` gives the model the `search_history` tool, which searches this session's history, including the parts that have already been compacted away. The tool accepts four parameters: `query`, `seq`, `parent` and `limit`. `query` searches by keyword, and `seq` reads the full text of a single row. In a subagent, passing `parent: true` searches the parent session's current context. Each turn of a side chat already carries the main session's current context, so passing `parent: true` in a side chat returns an error.

The host can run the same searches with `sess.SearchHistory` and `sess.HistoryLine`. See [15.5](15-context.md#155-searching-history).

`history.New()` has no options.

## 14.3 ext/tasks

`ext/tasks` gives the model four task management tools. These tools operate on the same task manager as `sess.Tasks()`, so the model and the host see the same set of tasks.

| Tool | Parameters |
|---|---|
| `task_status` | With an empty `task_id`, it lists all tasks. `include_result` controls whether the result is returned, and `wait_ms` sets how long to wait before returning. |
| `task_send` | Accepts `task_id`, `message` and `mode`, where `mode` is `next_loop`, `queue` or `interrupt`. |
| `task_stop` | Accepts `task_id` and `reason`. |
| `task_restart` | Accepts `task_id` and an optional new input, `input`. |

Besides these four tools, the extension adds a section on using tasks to the system prompt and, after compaction, lists for the model the tasks that have not yet ended. `tasks.New()` has no options.

## 14.4 ext/subagent

`ext/subagent` lets the model hand a piece of work to a derived agent, referred to below as a subagent. A subagent runs in its own child session, and the model can wait for it to finish or move it to the background. The extension name, the tool name and the task kind of this extension are all `subagent`.

```go
std.Defaults(std.With(subagent.New(subagent.Options{
	Defs: []subagent.Def{{
		Name:         "researcher",
		Description:  "Finds facts on the web and reports with sources.",
		Instructions: "You are a research assistant. ...",
		Tools:        []string{"web_fetch", "search_history"}, // nil inherits all of the parent session's tools; an empty slice provides no tools
		Model:        &provider.ModelRef{Provider: cheap, Model: "claude-haiku-5-5"},
		MaxSteps:     30,
	}},
	MaxConcurrent: 4,
})))
```

A `Def` describes one type of subagent and has the following fields.

| Field | Description |
|---|---|
| `Name`, `Description` | The type name and description. The model uses them to choose which subagent to use. |
| `Instructions` | The subagent's instructions section. |
| `Tools`, `DenyTools` | The tools that are allowed and denied, respectively. |
| `Model` | The model the subagent uses. When nil, it uses the parent session's current model. |
| `FixedModel` | When true, the call's `model` and `effort` are ignored, so the type always follows the parent's model even when `Model` is nil. |
| `MaxSteps` | The maximum number of steps the child session runs per turn. The default is 50. |
| `AskTimeout` | How long `ask_parent` waits for an answer. 0 waits indefinitely. |

`Options.MaxConcurrent` limits how many subagents can run at the same time in one session. The default is 8. `Options.Models` and `Options.Efforts` declare the model and effort choices a call may name; they become the tool schema's enums, and an empty list leaves that parameter out. `Options.ResolveModel` turns a name from `Models` into a `provider.ModelRef`; `inherit` needs no resolution and runs on the parent session's model. To add a subagent type at runtime, call `subagent.Define(r, def)`. A type defined at session level is visible only to that session.

When the model calls the `subagent` tool, it passes the following parameters: `agent_type` selects the type, `description` is a summary of 3 to 8 words, `prompt` holds the complete instructions and context, and `background` decides whether to run the subagent in the background; optional `model` and `effort` come from the host's enums, and `model: inherit` runs on the parent session's model. The subagent cannot see the parent session, so `prompt` must contain all the information it needs. The subagent can query the parent session with `search_history{parent:true}` and ask the parent a question with `ask_parent{question}`. After it asks, the task enters the waiting-for-input state, and the parent (the model or the host) answers with `task_send`. A subagent cannot use subagents of its own.

The extension defines two hook points. `subagent.Start` can block the start, modify the prompt, choose the model or effort, or add context for the child session. `subagent.Stop` can keep the child session running. See [9.7](09-hooks.md#97-hook-points-defined-by-extensions). Events emitted by the child session carry a `Path` field whose first hop has `Kind` set to `task` and `Name` set to `subagent`.

## 14.5 ext/skills

`ext/skills` supports Agent Skills, that is, directories organized as `<name>/SKILL.md`. The extension lists the available skills in the system prompt. The model loads one with the `skill` tool, and the user loads one with the `/<name>` command. A skill that has been loaded is pinned, so it stays in the context after compaction.

```go
std.Defaults(std.With(skills.New(skills.Options{
	Sources: []skills.Source{
		skills.FS(os.DirFS("./skills")),
		skills.FS(embeddedSkills, skills.WithLogger(logger)),
	},
})))
```

`Options` has the following fields.

| Field | Description |
|---|---|
| `Sources` | The list of skill sources. Earlier sources take precedence. You can also merge several sources into one first with `skills.Merge(sources...)`. |
| `MaxDescriptionChars` | The character limit for each skill's description in the catalog. The default is 1536. |
| `PinTokens` | The number of tokens pinned for each loaded skill. The default is 5000. |

`skills.FS(fsys, opts...)` scans an `embed.FS` or an `os.DirFS` as a source. It accepts two options. `WithFrontmatterParser(fn)` swaps in a full YAML parser; the default parser understands only the subset that Agent Skills uses. `WithLogger(l)` receives warnings when a skill is loaded leniently or skipped. If your skills live in a database or a remote service, implement the `skills.Source` interface yourself; it has three methods, `List`, `Load` and `Open`. To add a source at runtime, call `skills.AddSource(r, src)`.

`Frontmatter` has the following fields: `Name`, `Description`, `License`, `Compatibility`, `Metadata`, `AllowedTools`, `UserInvocable`, `ModelInvocable`, `Extra` and `Location`. `AllowedTools` is parsed but not enforced. `UserInvocable` and `ModelInvocable` both default to true. `Extra` keeps fields defined by other vendors exactly as they are.

The extension emits two events. `skills.Activated` records who loaded which skill and an estimate of how many tokens it takes up. `skills.CatalogChanged` signals that the catalog has changed. The `skills.Activate` hook point can block loading.

## 14.6 ext/memory

`ext/memory` gives an agent long-term memory. It writes instruction files (such as `AGENTS.md`) and long-term memories about the user and the project at the start of every epoch. The model can change memories with `memory_save` and `memory_forget`. When a session ends or is compacted, the extension has the model consolidate anything worth remembering into memory.

```go
std.Defaults(std.With(memory.New(memory.Options{
	Instructions: memory.Files(os.DirFS("."), "AGENTS.md", ".agent/instructions.md"),
	Store:        myStore, // implements memory.Store; use memory.NewInMemory() in tests
})))

ag.NewSession(ctx, kernel.WithLabels(map[string]string{"user_id": "u_42", "project_id": "acme/web"}))
```

`Options` has the following fields.

| Field | Description |
|---|---|
| `Instructions` | The instruction sources. `memory.Files(fsys, names...)` reads the files in order, skips files that do not exist, and caps the total size at 25KB. For a custom source, implement the `InstructionSource` interface. |
| `Store` | The memory store. It must implement two methods, `List(ctx, scope)` and `Apply(ctx, scope, changes)`, and `Apply` must either succeed completely or fail completely. If the store also implements `Ingester`, it can extract memories from the conversation on its own. |
| `ScopeFunc` | Computes `Scope{User, Project, Agent, Session}` from the session. By default, it takes `Labels["user_id"]` and `Labels["project_id"]`. |
| `ConsolidateOn` | When to consolidate memory. The default is `{"pre_compact"}`. If you add `"session_end"`, raise `Limits.CloseTimeout` so that it leaves enough time for one model call, or call `memory.Consolidate(ctx, sess)` yourself before calling `Close`. |
| `UserTokens`, `SharedTokens` | Limit the tokens taken up by memories with a user label and by memories without one, respectively. Memories without a user label are shared by all users. Both default to 10000. |
| `NoTools` | When true, the model is not given `memory_save` and `memory_forget`, so memory is read-only to the model. |
| `SharedWrites` | When true, the model may add, rewrite and delete memories shared by all users (memories without a user label). It is off by default: the model's writes land only under the current session's user, and only you can maintain shared memories, through `Store.Apply`. This keeps a single prompt injection from affecting every user. |

`memory_save` accepts four parameters. `content` is a single sentence of no more than 40 words. `user` and `project` set the scope of the memory, and each must be empty or equal to the current session's label. An empty `project` applies to all projects. An empty `user` is replaced with the current session's user when the session has a user label and `SharedWrites` is off; otherwise the memory applies to all users. `replaces` names the memory to rewrite; under the same conditions, rewriting a shared memory returns a `shared_read_only` error.

The extension emits two events. `memory.Loaded` records which memories were written into this epoch, and `memory.Saved` records what changed and what triggered the change.

## 14.7 ext/peer

`ext/peer` lets the model send messages to another agent. The other side can be another `*kernel.Agent` in the same process or a remote A2A service. Each exchange is a task of kind `a2a`, which the model can wait for or move to the background.

```go
remote, err := a2a.NewRemotePeer(ctx, "https://research.example.com", a2a.WithHeaders(map[string]string{"Authorization": "Bearer " + token}))
local := peer.Local(otherAgent, peer.IdleClose(10*time.Minute), peer.MaxSessions(100))

std.Defaults(std.With(peer.New(remote, peer.Named(local, "writer"))))
```

The following table lists the functions that construct and register peers.

| Function | Description |
|---|---|
| `a2a.NewRemotePeer(ctx, baseURL, opts...)` | Fetches the other side's agent card and selects an interface. It supports JSON-RPC or HTTP+JSON, with protocol version v0.3 or v1.0. The available options are `WithHeaders`, `WithDoer`, `WithBinding`, `WithProtocolVersion` and `WithAllowedHosts`. `WithDoer` customizes the HTTP client, which is where authentication and signing logic can go. By default, the interface address in the card must be on the same host as `baseURL` (and when `baseURL` is https, the interface must be https too), because the credentials from `WithHeaders` are sent with every request. When the interface is on another host, allow it with `WithAllowedHosts("api.example.com")`; otherwise the call returns `a2a.ErrInterfaceHost`. |
| `peer.Local(agent, opts...)` | Wraps an agent in the same process without any serialization, with each conversation mapped to one of its sessions. `IdleClose(d)` sets how long a session can stay idle before it is closed; when a store is configured, the session is resumed the next time a message arrives. `MaxSessions(n)` limits how many sessions can be open at the same time. |
| `peer.Named(p, name)` | Sets the name the model uses. You need it when the name in the agent card does not match `[a-zA-Z0-9_-]`. |
| `peer.Register(r, p)` | Adds a peer at runtime. |

When the model calls `a2a_send`, it passes the following parameters: `agent` names the other side, `message` is the message to send, `new_conversation` starts a new conversation, and `background` decides whether to run the exchange in the background. The other side cannot see this conversation, so `message` must contain all the information it needs.

If the other side requires authentication, the `peer.AuthRequired` hook fires and a `peer.auth_required` event is emitted. Fix the credentials in the hook and return `Retry: true` to retry.

To plug in your own transport, implement the `peer.Peer` interface, which has four methods: `Card`, `Send`, `Subscribe` and `Cancel`.

## 14.8 ext/mcp

`ext/mcp` connects to MCP servers and makes the tools they provide available to the model. It supports two transports, stdio and Streamable HTTP. It also supports OAuth 2.1 and elicitation (the server asking the user for input during a call), and it can register a server's prompts as slash commands. It is not among the default extensions, so you need to add it yourself with `kernel.Use` or `kernel.SessionUse`.

```go
m := mcp.New(mcp.Options{
	Servers: []mcp.Server{
		{Name: "fs", Command: func() *exec.Cmd {
			return exec.Command("npx", "-y", "@modelcontextprotocol/server-filesystem", "/data")
		}},
		{Name: "jira", URL: "https://mcp.example.com/jira", OAuth: &mcp.OAuth{
			ClientMetadataURL: "https://app.example.com/oauth/client.json",
			RedirectURL:       "https://app.example.com/oauth/callback",
			Authorize:         authorizer.Authorize, // mcp.NewCallbackAuthorizer() or mcp.NewLoopbackAuthorizer(nil) works here
			Tokens:            tokenStore,           // implements mcp.TokenStore and keeps authorization across process restarts
		}, Prompts: true},
	},
})
defer m.Close(ctx)

kernel.New(std.Defaults(std.With(m)), kernel.WithApprovalRules(mcp.ApprovalRules(rules...)...), ...)
```

An `mcp.Server` describes one server and has the following fields.

| Field | Description |
|---|---|
| `Name` | The server name, which must match `^[a-z0-9][a-z0-9-]{0,31}$`. It appears in tool names in the form `mcp__<server>__<tool>`. |
| `Command`, `URL` | Set one or the other. `Command` denotes a stdio server. It is a function that returns an `*exec.Cmd` and is called once for every connection, reconnections included. Put credentials in the `Env` of the command it returns; fields such as `SysProcAttr` and `Stderr` take effect exactly as set. `URL` denotes a Streamable HTTP server. |
| `Headers`, `HTTPClient` | The request headers and the client used for an HTTP server. |
| `OAuth`, `OAuthHandler` | Set one or the other. `OAuth` uses the built-in OAuth 2.1 flow, and `OAuthHandler` uses your own implementation of the MCP SDK's `auth.OAuthHandler`. |
| `Tools`, `DenyTools` | The allowed and denied tools, by MCP tool name. An empty `Tools` allows every tool. |
| `Trusted` | When true, the annotations the server provides are trusted, and tools that the server marks as read-only can run in parallel. |
| `Timeout` | The timeout for a single call. 0 uses `Limits.ToolTimeout`. |
| `Background` | Lets the model run these tools in the background. |
| `Prompts` | Registers the server's prompts as `/mcp__<server>__<prompt>` commands. |
| `ProtocolVersion` | The protocol version to propose first. When empty, the latest version is used. |

`mcp.Options` also has the following fields.

| Field | Description |
|---|---|
| `MaxResultChars` | The character limit for a tool result handed to the model. The default is 50000. |
| `MaxMediaBytes` | The byte limit for a single image or PDF in a tool result. The default is 5 MiB. Content over the limit, and images of any type other than png, jpeg, gif or webp, are replaced with a short explanatory note so that the model service does not reject the request. |
| `ConnectTimeout` | How long `Install` waits for the first connection. The default is 30s. |
| `Discovery` | The tool search mode. See below. |
| `NativeSearch` | Decides whether `Auto` mode switches to `Native`. See below. |
| `DiscoveryRatio` | In `Auto` mode, search is used once the tool definitions take up more than this fraction of the context window. The default is 0.02. |
| `CatalogRatio` | In `Search` mode, the fraction of the budget the catalog section may use. Beyond it, only tool names are listed, without summaries. The default is 0.05. |
| `ContextWindow` | When the extension is installed at agent level, the window size that `Auto` mode uses to compute the fraction. The default is 200000. |
| `Search` | Replaces the built-in keyword search. |

When a server provides many tools, sending every tool definition to the model takes up a lot of context. `Options.Discovery` decides how these tools are presented to the model. The available modes are:

| Mode | Behavior |
|---|---|
| `Auto` (default) | Uses `Direct` when the tool definitions take up less than `DiscoveryRatio` of the window, and `Search` otherwise. If `NativeSearch` is true, it uses `Native` instead. |
| `Direct` | Provides a separate definition for each of the server's tools. |
| `Search` | Provides only two tools, `mcp_search_tools` and `mcp_call_tool`, and lists the tool names in the catalog in the system prompt. `mcp_search_tools` accepts the parameters `names`, `query`, `servers` and `detail`, and `mcp_call_tool` accepts `name` and `arguments`. |
| `Native` | Uses deferred loading of tools together with the vendor's server-side tool search. See [7.8](07-tools.md#78-deferred-loading-and-tool-search). |

MCP tools are subject to approval rules as well. In `Search` mode, the tool the model actually calls is `mcp_call_tool`, so wrap your rules with `mcp.ApprovalRules(rules...)`. For every rule that targets a server tool, it adds a mirror rule on `mcp_call_tool`, so that both modes reach the same approval decision.

### OAuth sign-in

When a server requires sign-in, the extension emits an `mcp.auth_required` event with `State` set to `waiting` and an `AuthURL`. Give this URL to the user to open; `Authorize` keeps waiting until the user has completed authorization. The SDK provides two authorizers:

- Desktop programs and CLIs use `mcp.NewLoopbackAuthorizer(nil)`. It starts a callback port on the local machine and opens the browser. Set `OAuth.RedirectURL` to its `RedirectURL()`.
- Server programs use `mcp.NewCallbackAuthorizer()`. Mount it on your own HTTP routes; the route path is the `RedirectURL`.

The authorization result is saved through a `TokenStore`. The interface has two methods, `Load` and `Save`, which read and write entries by `mcp.TokenKey`. A `TokenKey` contains the server name `Server`, the server address `Resource`, and, when the extension is installed at session level, `Session` (the session id) and `Labels` (the session's labels, such as `user_id`). When several users share one store, include `Labels["user_id"]` or `Session` in the key so that users never use each other's tokens. Include `Resource` in the key as well, so that old tokens are not sent to a new address after the server moves.

### Elicitation

When a server needs input from the user during a call, the `mcp.Elicitation` hook fires. `Mode` is either `form` or `url`, and a `form` request comes with a `Schema`. Return an `Action` from the hook: `accept` (together with `Content`), `decline` or `cancel`. If this hook is not registered, every request is declined. Content the user provides goes back only to the server; the model never sees it. When several users share one MCP server and you need elicitation, install the extension per session with `kernel.SessionUse`. A connection installed at agent level is shared by all sessions, so a request that the server sends on its own, outside any call, might be handed to a call that a different user has in progress.

The extension emits three events. `mcp.server_status` reports the connection state, which is connecting, ready, failed or closed, along with the number of tools. The other two are `mcp.auth_required` and `mcp.elicitation`.

An mcp extension installed with `kernel.Use` shares its connections across all sessions of that agent, while one installed with `kernel.SessionUse` opens separate connections for a single session. Before your process exits, call `m.Close(ctx)` to close the connections.

## 14.9 ext/cmd

`ext/cmd` contains seven slash commands. Each command is its own package and a separate extension. For the list of commands and how to use them, see [10.5](10-commands.md#105-built-in-commands).

## 14.10 ext/litellm

`ext/litellm` is not an extension but a pair of helper functions. `litellm.RequestHeaders(o)` tags every request with the id of the root session, and `litellm.CostSource(o)` queries cost from LiteLLM's spend logs. For details, see [17.4](17-usage-cost.md#174-querying-cost).

---

[← Previous: Extensions](13-extensions.md) · [Contents](README.md) · [Next: Context management →](15-context.md)
