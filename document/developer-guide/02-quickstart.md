[← 上一章：简介](01-overview.md) · [目录](README.md) · [下一章：核心对象 →](03-core-objects.md)

# 2. 快速开始

本章通过一个可以直接运行的最小例子，完整展示一个 agent 从配置、创建会话到接收流式回答的写法。后续各章会分别展开这个例子中的每个部分。

本章内容：[2.1 完整示例](#21-完整示例) · [2.2 代码说明](#22-代码说明) · [2.3 后续阅读](#23-后续阅读)

## 2.1 完整示例

下面的程序实现了一个最小 agent。它注册了一个查询天气的工具，在调用工具之前先请审查者确认，并把模型的回答以流式方式打印到终端。

```go
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"

	"github.com/estonshi/aperture-potatos/event"
	"github.com/estonshi/aperture-potatos/kernel"
	"github.com/estonshi/aperture-potatos/provider"
	"github.com/estonshi/aperture-potatos/provider/anthropic"
	"github.com/estonshi/aperture-potatos/std"
	"github.com/estonshi/aperture-potatos/store/jsonl"
)

type weatherIn struct {
	City string `json:"city"`
}

func main() {
	ctx := context.Background()

	// 1. 模型：一个 Provider 实例可以给很多 session 共用
	claude := anthropic.New(anthropic.Config{APIKey: os.Getenv("ANTHROPIC_API_KEY")})

	// 2. 工具：输入按 json tag 严格解码
	weather := kernel.NewTool(kernel.ToolSpec{
		Name:        "get_weather",
		Description: "Current weather of a city.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"city":{"type":"string"}},"required":["city"],"additionalProperties":false}`),
		Concurrency: kernel.Parallel,
	}, func(ctx context.Context, tc *kernel.ToolContext, in weatherIn) (kernel.ToolResult, error) {
		return kernel.TextResult(in.City + ": sunny, 22°C"), nil
	})

	// 3. 审查规则：调用 get_weather 之前先问审查者
	rules, err := kernel.ParseApprovalRules([]byte(`[{"id":"weather","tool":"get_weather","action":"ask"}]`))
	if err != nil {
		log.Fatal(err)
	}

	// 4. Agent：一份配置
	ag, err := kernel.New(
		std.Defaults(), // 默认扩展：压缩、历史检索、任务管理工具、subagent、/compact 等命令
		kernel.WithNickname("Tudou"),
		kernel.WithInstructions("Answer in the user's language."),
		kernel.WithModel(provider.ModelRef{Provider: claude, Model: "claude-sonnet-5-5"}),
		kernel.WithStore(jsonl.New("./sessions")),
		kernel.WithTool(weather),
		kernel.WithApprovalRules(rules...),
		kernel.WithApprover(func(ctx context.Context, req kernel.ApprovalRequest) (kernel.ApprovalDecision, error) {
			fmt.Printf("\n[approve] %s %s\n", req.Call.Name, req.Call.Input)
			return kernel.ApprovalDecision{Verdict: kernel.VerdictApprove}, nil
		}),
	)
	if err != nil {
		log.Fatal(err)
	}
	defer ag.Close(ctx)

	// 5. Session：一段对话
	sess, err := ag.NewSession(ctx)
	if err != nil {
		log.Fatal(err)
	}
	defer sess.Close(ctx)

	// 6. Run 发送一条输入，并产出这一轮的全部事件，直到 turn.completed
	for ev, err := range sess.Run(ctx, kernel.Text("东京天气怎么样？")) {
		if err != nil {
			log.Fatal(err)
		}
		switch d := ev.Data.(type) {
		case *event.MessageDelta:
			fmt.Print(d.Text)
		case *event.ToolCompleted:
			fmt.Printf("\n[tool %s: %s]\n", d.Name, d.Outcome)
		case *event.TurnCompleted:
			fmt.Printf("\n[turn %s, %d steps]\n", d.Outcome, d.Steps)
		}
	}
}
```

## 2.2 代码说明

- `kernel.New` 不会自动装配默认扩展，因此需要显式写上 `std.Defaults()`。不写也能运行，只是没有自动压缩、斜杠命令和任务管理工具。
- 模型由你先创建好 Provider，再通过 `WithModel` 交给 kernel。如果要更换模型，只需修改这一行。
- 如果不配置 `WithStore`，会话只保存在内存中，进程退出后就会丢失。
- `Run` 适合命令行这类“发送一句话，等待一轮结束”的场景。如果要向 Web 页面推送流式输出，请使用 `Send` 加订阅的方式，见 [5.10](05-input.md#510-常见集成方式)。
- 示例中的审查者直接批准了调用。在实际场景中，审查者应当把 `req.Call` 展示给人，等待人做出决定，见第 [8](08-approval.md) 章。

## 2.3 后续阅读

| 需求 | 参阅章节 |
|---|---|
| 换用 OpenAI、DeepSeek 或自己的网关。 | [4. 接入模型](04-models.md) |
| 把 agent 接入 HTTP 服务，向前端推送流式输出。 | [5. 发送输入](05-input.md)、[6. 订阅事件](06-events.md) |
| 注册更多工具。 | [7. 注册工具](07-tools.md) |
| 让人来审批危险操作。 | [8. 审查](08-approval.md) |
| 关闭或替换某个默认扩展。 | [13.4 std](13-extensions.md#134-使用-std-安装默认扩展) |

---

[← 上一章：简介](01-overview.md) · [目录](README.md) · [下一章：核心对象 →](03-core-objects.md)
