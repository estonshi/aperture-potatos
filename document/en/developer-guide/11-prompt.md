[← Previous: Commands](10-commands.md) · [Contents](README.md) · [Next: Tasks →](12-tasks.md)

# 11. System prompt and dynamic context

This chapter explains what makes up the prompt the model sees: where your instructions go, where extensions put their descriptions, and how to send the model information that changes every turn without breaking the cache. After reading it, you can write instructions for your agent, add prompt sections from an extension, and inspect the system prompt that is finally rendered.

In this chapter: [11.1 Structure of the system prompt](#111-structure-of-the-system-prompt) · [11.2 Writing your instructions](#112-writing-your-instructions) · [11.3 Extension sections](#113-extension-sections) · [11.4 Per-turn dynamic content](#114-per-turn-dynamic-content) · [11.5 Viewing the rendered prompt](#115-viewing-the-rendered-prompt) · [11.6 Model-facing text](#116-model-facing-text) · [11.7 Overriding the fixed texts](#117-overriding-the-fixed-texts) · [11.8 Best practices](#118-best-practices)

## 11.1 Structure of the system prompt

The kernel determines the structure of the system prompt. You cannot replace it as a whole; you can only fill in some of its sections. The sections appear in the following order:

| Order | Section | Where the content comes from |
|---|---|---|
| 1 | core | Written by the kernel. It explains the runtime conventions, that is, what reminders, task notifications and messages from other agents each mean. |
| 2 | identity | The nickname and description, set with `WithNickname` and `WithDescription`. |
| 3 | capabilities | Usage notes for the installed capabilities, written by extensions through `SlotCapabilities`. |
| 4 | user | Your instructions, which take precedence over the capability notes above. Set them with the `WithInstructions` family of options. |
| 5 | catalog | Catalogs such as skills and the agents that can be called, written by extensions through `SlotCatalog`. |
| 6 | environment | Facts about the environment, written by extensions through `SlotEnvironment`. |

The whole system prompt stays the same within an epoch and is rendered again only when a session is created, when it is compacted, and on `/clear`. Rewind, fork and switching models also start a new epoch, but they copy the system prompt over unchanged. This lets requests keep hitting the model's prompt cache and leaves thinking signatures intact.

## 11.2 Writing your instructions

Your instructions go in the user section. The SDK offers three ways to set them: one for fixed text, one for text generated per session, and one for text specific to a single session:

| Option | Scope | Description |
|---|---|---|
| `kernel.WithInstructions(text)` | agent | Sets fixed instruction text. |
| `kernel.WithInstructionsFunc(fn)` | agent | Computed each time the system prompt is rendered again, that is, when a session is created, compacted or cleared. `fn` receives a `SessionInfo` with fields such as `SessionID`, `Labels` and `Epoch`, so you can generate different instructions for each tenant or project. |
| `kernel.SessionInstructions(text)` | session | Sets instructions for this session, overriding the agent-level instructions. |

The following example loads the instructions for the tenant named in the session labels:

```go
kernel.WithInstructionsFunc(func(ctx context.Context, info kernel.SessionInfo) (string, error) {
	tenant := info.Labels["tenant"]
	return loadInstructions(ctx, tenant) // the instructions are read from your configuration service
})
```

## 11.3 Extension sections

Extensions can write content into three sections: capabilities, catalog and environment. To do so, register a prompt section in `Install`:

```go
r.PromptSection(kernel.PromptSection{
	Name:  "repo-facts",
	Slot:  kernel.SlotEnvironment,
	Order: 10,
	Render: func(ctx context.Context, rc kernel.RenderContext) (string, error) {
		return "Repository: acme/web, default branch main.", nil // returning "" omits this section
	},
})
```

`PromptSection` has the following fields:

| Field | Description |
|---|---|
| `Name` | The section name, which appears in the `system.rendered` event. |
| `Slot` | One of `SlotCapabilities`, `SlotCatalog` or `SlotEnvironment`. The core, identity and user sections are not open to extensions. |
| `Order` | The position within the slot. Sections with the same order are sorted by name. |
| `AnnounceChanges` | When true, if the content changes within an epoch, the model is told about the change in a reminder instead of waiting for the next epoch. |
| `Render` | Called once each time the system prompt is rendered again, that is, when a session is created, compacted or cleared. Rewind, fork and switching models reuse the existing text. `RenderContext` contains `Agent`, `Session`, `Tools` and `Epoch`, where `Tools` holds the tool definitions the model sees in this epoch. |

If you do not need to write an extension, you can also register a section directly at agent level with `kernel.WithPromptSection(ps)`.

## 11.4 Per-turn dynamic content

Content that changes every turn, such as the current time or the user's status, should not go into the system prompt, because it would invalidate the cache. Deliver this kind of content as a reminder through a `ContextProvider`:

```go
kernel.WithContextProvider(kernel.ContextProvider{
	Name: "clock",
	When: kernel.AtTurnStart, // kernel.AtEveryStep also works
	Provide: func(ctx context.Context, pc kernel.ProvideContext) (string, error) {
		return "Today is " + time.Now().Format("2006-01-02") + ".", nil // returning "" adds nothing this time
	},
})
```

`ContextProvider` has the following fields:

| Field | Description |
|---|---|
| `Name` | The source label of the reminder. |
| `When` | With `AtTurnStart`, the content is delivered together with the input at the start of each turn. With `AtEveryStep`, it is delivered before every model call. |
| `Provide` | The function that generates the content. `ProvideContext` contains `Session`, `TurnID`, `Step` and `Inputs`. With `AtTurnStart`, `Inputs` holds the inputs of this turn, so you can decide what background to provide based on what the user sent. |

In an extension, register it with `r.ContextProvider(p)`. When the content is delivered, a `context.injected` event is emitted with the fields `Sources`, `Count` and `Truncated`.

There are also two kinds of one-off dynamic content. `sess.Announce(source, text, payload)` sends a reminder that does not wake the session. A hook's output can also attach content through `AdditionalContext`; see [9.2](09-hooks.md#92-synchronous-and-asynchronous-hooks).

## 11.5 Viewing the rendered prompt

When you debug a prompt, you can see what was actually rendered in several ways:

- A `system.rendered` event is emitted after each epoch is rendered. Its `Sections` lists the slot, name and character count of each section, and you can use `Hash` to tell whether two renders produced the same result.
- `sess.Messages()[0]` is the system row of the current epoch. Its `Content` holds the rendered text, split into blocks by section.
- `sess.Context()` lists token usage by source. The entries whose `Kind` is `system` are the prompt sections.
- In tests, you can get the `System` that was actually sent from the `Requests()` of `providertest.ScriptProvider`. See [21](21-testing.md).

## 11.6 Model-facing text

Reminders, task notifications and messages from other agents all arrive wrapped in tags, and the core section tells the model that this content is not something the user said and does not grant any authorization. Before text from other agents or from tasks is delivered, tag characters in it are escaped and overly long parts are truncated, so that the text cannot pose as the runtime.

The model also sees the tool descriptions, command templates and section text that you write yourself. Write them in English with neutral wording, and avoid an adversarial tone such as "you must".

If you want the prompt to refer to the runtime's conventions, for example to tell the model the format of reminders, you do not need to explain them again, because the core section already states them.

## 11.7 Overriding the fixed texts

Every fixed text the runtime sends to the model has an English default. A host that needs another language or wording can override them at agent scope with `kernel.WithTexts`:

```go
ag, err := kernel.New(
	kernel.WithModel(model),
	kernel.WithTexts(map[string]string{
		"unknown_tool": "There is no tool named {{name}}.",
		"identity":     "<identity>\nYour name is {{nickname}}.\n</identity>",
		"system.core":  coreText, // the whole core section
	}),
)
```

The keys come in two kinds:

- Every level-two heading of `spec/prompts/reminders.md` is a key, such as `unknown_tool`, `denied_by_rule` and `approval_stopped`. The body uses the same template syntax, so variables such as `{{name}}` expand as usual.
- Prompt file names, such as `identity`, `user`, `system.core`, `agent_message`, `task_started` and `task_notification`.

Rules and limits:

- Only the keys you give change; everything else keeps the built-in text, and `New` rejects a misspelled key with `ErrInvalidName`.
- `system.core` may be replaced whole, but it must still mention every tag in `spec/data/runtime_tags.json` and close `</runtime-conventions>`, or `New` fails. This way the model never loses the explanation of what reminders, task notifications and messages from other agents mean.
- The override is agent scope, inherited by derived agents, and cannot change inside a session. An epoch already rendered keeps its text; a new epoch picks the change up.
- The fixed texts a provider adapter writes (`dangling_tool_call`, `tool_removed`, `document_link`, `tool_result_media`) arrive through `provider.Request.Texts`, which the kernel fills from the same table. A provider you write yourself may leave it unset and get the built-in texts.
- Other hardcoded wording is translated through structured fields: a `notice` the kernel writes itself carries `Kind` and `Detail`, so a host can decide what to show from `Kind` instead of parsing `Text`. See [6](06-events.md).

## 11.8 Best practices

- Put static content in the system prompt and deliver dynamic content through a `ContextProvider`. A good test is whether the text could change within a day. If it could, keep it out of the system prompt.
- For multi-tenant setups, combine `WithLabels` with `WithInstructionsFunc`. One Agent can then serve every tenant, with instructions generated from the labels.
- Keep the content of extension sections as stable as possible. The text that `Render` returns is not updated until the next render, so even if the content changes before then, the model does not see the change unless `AnnounceChanges` is set. Content that really needs to change often should use a `ContextProvider` instead.
- Use `AtEveryStep` sparingly. A reminder delivered at every step stays in history, and in a long conversation it uses a lot of tokens. `AtTurnStart` is enough for most cases.
- To monitor cache hits, check the `Hash` of `system.rendered`. The hash should stay the same across consecutive epochs of the same session. If it changes, the content of some section is changing.

---

[← Previous: Commands](10-commands.md) · [Contents](README.md) · [Next: Tasks →](12-tasks.md)
