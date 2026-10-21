[← Previous: Tasks](12-tasks.md) · [Contents](README.md) · [Next: Built-in extensions →](14-builtin-extensions.md)

# 13. Extensions

An extension packages a set of your own capabilities, such as tools, commands, prompt sections and hooks, so that you can install them per agent or per session. This chapter covers how to write an extension, scopes, registering and revoking at runtime, and how to install and turn off the built-in extensions. After reading it, you can package a complete feature as an extension and decide which level to install it at.

In this chapter: [13.1 Extension and Registrar](#131-extension-and-registrar) · [13.2 Scopes](#132-scopes) · [13.3 Registering and revoking at runtime](#133-registering-and-revoking-at-runtime) · [13.4 Installing the default extensions with std](#134-installing-the-default-extensions-with-std) · [13.5 Private state, events and hook points of an extension](#135-private-state-events-and-hook-points-of-an-extension) · [13.6 Derived agents](#136-derived-agents) · [13.7 Best practices](#137-best-practices)

## 13.1 Extension and Registrar

The SDK's built-in capabilities and the extensions you write are the same type. Both implement the `Extension` interface:

```go
type Extension interface {
	Name() string              // must be unique; std.Without filters by it, and "app" is reserved for content registered directly through WithTool and similar options
	Install(r Registrar) error // called once per scope
}
```

If you do not want to define a separate type for an extension, create one with `kernel.ExtensionFunc(name, install)`. The following example registers a tool, a hook and a ContextProvider in a single extension:

```go
repo := kernel.ExtensionFunc("repo", func(r kernel.Registrar) error {
	if _, err := r.Tool(searchTool); err != nil {
		return err
	}
	if _, err := kernel.On(r, kernel.PreToolUse, "repo-guard", guard, kernel.Match(kernel.MatchNames("repo_write"))); err != nil {
		return err
	}
	_, err := r.ContextProvider(kernel.ContextProvider{Name: "repo", When: kernel.AtTurnStart, Provide: provide})
	return err
})
```

The `Registrar` passed to `Install` provides the following methods:

| Method | What it registers |
|---|---|
| `Tool(t)` | A tool. See [7](07-tools.md). |
| `HostedTool(t)` | A server-side tool. See [4.7](04-models.md#47-server-side-tools). |
| `Command(c)` | A command. See [10](10-commands.md). |
| `PromptSection(ps)` | A system prompt section. See [11.3](11-prompt.md#113-extension-sections). |
| `ContextProvider(p)` | Per-turn dynamic content. See [11.4](11-prompt.md#114-per-turn-dynamic-content). |
| `Compactor(c)` | A compaction strategy. Each scope can have only one. See [15.6](15-context.md#156-custom-compaction-strategies). |
| `TaskKind(k)` | A task kind. See [12](12-tasks.md). |
| `Hide(kind, name)` | Shadows an inherited registration in this scope. `kind` can be `KindTool`, `KindCommand`, `KindPromptSection`, `KindContextProvider`, `KindCompactor`, `KindTaskKind` or `KindHostedTool`. |
| `Scope()`, `Agent()`, `Session()` | Return the current scope, the owning agent and the owning session. At agent level, `Session()` returns nil. |

Hooks are registered with `kernel.On(r, ...)` or `kernel.OnAsync(r, ...)`. See [9.1](09-hooks.md#91-registering-hooks).

Every registration returns a `*kernel.Registration`. Call its `Remove()` to revoke that registration; calling `Remove()` more than once is safe. If an extension manages resources of its own, such as a connection, and you want them to go through the same revocation flow, wrap the cleanup logic as a registration with `kernel.NewRegistration(func() { ... })`.

## 13.2 Scopes

You can install an extension at agent level or at session level. The two scopes differ as follows:

| Scope | How to install | Description |
|---|---|---|
| agent | `kernel.Use(exts...)`, `std.Defaults(...)` | Shared by every session of this agent. `Install` is called once, in `kernel.New`. |
| session | `ag.NewSession(ctx, kernel.SessionUse(exts...))` | Applies only to this session. `Install` is called when the session is created. A registration with the same name shadows the agent-level one. |

Content registered through direct options such as `kernel.WithTool`, `WithCommand` and `WithHook` belongs to an agent-level extension named `app`. `app` is therefore a reserved name, and your own extensions must not use it.

## 13.3 Registering and revoking at runtime

Besides registering when an extension is installed, you can add or revoke capabilities dynamically while a session runs. `sess.Registrar()` returns the session-scope Registrar, which you use the same way as the Registrar in `Install`:

```go
reg, err := sess.Registrar().Tool(t)
// ...
reg.Remove()
```

A change to the registrations takes effect from the next turn. For models that declare `ThinkingPrefixBound`, it takes effect from the next epoch. Every change emits a `registry.changed` event with the fields `Scope`, `Kind`, `Name`, `Op` and `Effective`, where `Effective` is either `turn` or `epoch`. Within a turn, tool definitions that are already frozen do not change. If a tool's definition itself changes, meaning `Spec()` returns something different, call `kernel.ToolChanged(r, name)` to announce it.

## 13.4 Installing the default extensions with std

The `std` package installs the SDK's default set of extensions with a single option, and lets you add to it, replace extensions in it or remove extensions from it:

```go
kernel.New(
	std.Defaults(
		std.With(skills.New(skills.Options{Sources: []skills.Source{skills.FS(os.DirFS("./skills"))}})),
		std.Without("subagent"),
	),
	...
)
```

The `std` package provides the following functions:

| Function | What it does |
|---|---|
| `std.Defaults(opts...)` | Installs the default set and returns a `kernel.Option`. |
| `std.With(exts...)` | Adds extensions, or replaces default extensions with the same name. |
| `std.Without(names...)` | Removes the named default extensions. |
| `std.Extensions(opts...)` | Returns the list of extensions for you to install yourself, for example at session level. |

The default set includes `compact`, `history`, `tasks` and `subagent`, plus seven commands: `cmd.help`, `cmd.usage`, `cmd.compact`, `cmd.clear`, `cmd.rewind`, `cmd.fork` and `cmd.context`. skills, memory, peer and mcp need configuration, so they are not in the default set; add them with `std.With`.

## 13.5 Private state, events and hook points of an extension

Besides registering capabilities, an extension can keep its own state and define its own events and hook points.

**Private state**. An extension can attach private state to an agent or a session. This state is type-safe, but it lives only in memory, so a resumed session does not have it.

```go
var counterKey = kernel.NewKey[int]("repo.counter")

kernel.Set(sess, counterKey, 0)
n, ok := kernel.Get(sess, counterKey)
kernel.Update(sess, counterKey, func(old int, ok bool) int { return old + 1 }) // the read and the update happen in one atomic operation
```

**Events**. To define events of its own, an extension defines payload types that implement `EventType() event.Type` and emits them with `tc.Emit`, `sess.Emit` or `rt.Emit`. The extension should also provide a `Payloads() []event.Constructor` so that cross-process decoders can recognize these payload types. See [6.7](06-events.md#67-passing-events-across-processes).

**Hook points**. An extension can define its own hook points with `kernel.NewHookPoint(kernel.HookPointDef[In, Out]{...})` and fire them with `kernel.Fire(ctx, sess, point, in)`. See [9.7](09-hooks.md#97-hook-points-defined-by-extensions).

**Logging**. An extension should log through `r.Agent().Logger()`, so that its warnings end up in the same place as the host's logs.

## 13.6 Derived agents

A derived agent is a new agent created on top of an existing one. It can have its own identity, instructions, model and tool set, and it inherits the rest of its configuration from the parent agent:

```go
child, err := ag.Derive(kernel.DeriveOptions{
	Nickname:   "Researcher",
	ToolFilter: func(s kernel.ToolSpec) bool { return s.Name != "book_hotel" },
})
```

`DeriveOptions` has the following fields. Except for `InheritHooks`, a field left at its zero value keeps the parent agent's setting. The zero value of `InheritHooks` is false, which means the parent agent's hooks are not inherited.

| Field | Description |
|---|---|
| `Nickname`, `Description` | The derived agent's own identity. |
| `Instructions *string` | The derived agent's own instructions section. |
| `Model *provider.ModelRef` | The derived agent's own model. |
| `ToolFilter` | Decides which tools to keep. Server-side tools also go through this filter, but only with their names. |
| `InheritHooks` | Decides whether to inherit the parent agent's hooks. Approval rules and approvers are **always inherited**, so a derived agent cannot get around approval. |
| `Use` | Additional extensions to install. |
| `Limits` | The derived agent's own limits. Fields left at their zero value use the parent agent's settings. |

A derived agent shares the parent agent's registrations as a read-only layer, so it also sees changes the parent makes later. The subagent extension uses this mechanism to create an agent for each kind of subagent.

## 13.7 Best practices

- Map one feature to one extension. Put a tool, its prompt section and its hooks in the same `Install`, so that turning the feature off only means removing one `std.With` line.
- Give `Name()` a short lowercase name that does not clash with a built-in extension. Command extensions follow the `cmd.<name>` naming convention.
- `Install` must be safe to call more than once, because it is called once at agent level and once for each session at session level. Do not put logic in it that can only run once. Create shared connections in `New`, and let `Install` do nothing but register.
- Capabilities that each user can switch on or off are best installed at session level. You can use `kernel.SessionUse(ext)`, or register them at runtime through `sess.Registrar()`.
- An extension should store any state that must persist on its own. `kernel.Key` lives only in memory. For state that must survive a restart, use `Pin`, `SaveState` or your own storage.

---

[← Previous: Tasks](12-tasks.md) · [Contents](README.md) · [Next: Built-in extensions →](14-builtin-extensions.md)
