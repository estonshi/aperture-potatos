package kernel_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/estonshi/aperture-potatos/event"
	"github.com/estonshi/aperture-potatos/kernel"
	"github.com/estonshi/aperture-potatos/message"
	"github.com/estonshi/aperture-potatos/provider/providertest"
)

func toolResults(msgs []message.Message) []*message.ToolResultBlock {
	var out []*message.ToolResultBlock
	for _, m := range msgs {
		for _, b := range m.Content {
			if r, ok := b.(*message.ToolResultBlock); ok {
				out = append(out, r)
			}
		}
	}
	return out
}

func firstText(r *message.ToolResultBlock) string {
	if len(r.Content) == 0 {
		return ""
	}
	if t, ok := r.Content[0].(*message.TextBlock); ok {
		return t.Text
	}
	return ""
}

type bookIn struct {
	Hotel  string `json:"hotel"`
	Nights int    `json:"nights"`
}

func TestKernelToolResults(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var booked atomic.Int32
		book := kernel.NewTool(kernel.ToolSpec{Name: "book", InputSchema: json.RawMessage(`{"type":"object"}`), Concurrency: kernel.Serial},
			func(ctx context.Context, tc *kernel.ToolContext, in bookIn) (kernel.ToolResult, error) {
				booked.Add(1)
				return kernel.TextResult("booked " + in.Hotel), nil
			})
		boom := kernel.NewTool(kernel.ToolSpec{Name: "boom", Concurrency: kernel.Serial}, func(context.Context, *kernel.ToolContext, struct{}) (kernel.ToolResult, error) {
			panic("kaput")
		})
		bad := toolReply(call{"toolu_1", "missing", `{}`}, call{"toolu_2", "book", `{"Hotel":"A"}`}, call{"toolu_3", "book", `{"hotel":"A","extra":1}`},
			call{"toolu_4", "book", `{"hotel":"A","hotel":"B"}`}, call{"toolu_5", "book", `{"hotel":"A","nights":"two"}`}, call{"toolu_6", "boom", `{}`}, call{"toolu_7", "book", `{"hotel":"Ok"}`})
		bad[len(bad)-1].Event.Message.Content = append(bad[len(bad)-1].Event.Message.Content, &message.ToolCallBlock{ID: "toolu_8", Name: "book", Input: json.RawMessage(`{}`), RawInput: `{"hotel":`, InputError: message.InputInvalidJSON})
		h := newHarness(t, [][]providertest.Step{bad, textReply("done")}, kernel.WithTool(book), kernel.WithTool(boom))
		rc, _ := h.sess.Send(kernel.Text("book"))
		rc.Wait(t.Context())
		h.close()
		want := map[string]string{
			"toolu_1": "unknown_tool:There is no tool named \"missing\"",
			"toolu_2": "invalid_input:Invalid input: unknown field \"Hotel\"",
			"toolu_3": "invalid_input:Invalid input: unknown field \"extra\"",
			"toolu_4": "invalid_input:Invalid input: field \"hotel\" appears more than once",
			"toolu_5": "invalid_input:Invalid input: field \"nights\" must be a number, got string",
			"toolu_6": "error:panic: kaput",
			"toolu_7": "success:booked Ok",
			"toolu_8": "invalid_input:Invalid input: the arguments are not a JSON object. Arguments received: {\"hotel\":",
		}
		got := toolResults(h.lines())
		if len(got) != len(want) {
			t.Fatalf("%d results", len(got))
		}
		for _, r := range got {
			if s := string(r.Outcome) + ":" + firstText(r); !strings.HasPrefix(s, want[r.CallID]) {
				t.Errorf("%s: %s", r.CallID, s)
			}
		}
		if booked.Load() != 1 {
			t.Fatalf("book ran %d times", booked.Load())
		}
	})
}

func TestToolTimeoutAndAbandon(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		timeout := &echoTool{name: "slow", delay: time.Hour}
		h := newHarness(t, [][]providertest.Step{toolReply(call{"toolu_1", "slow", `{}`}, call{"toolu_2", "stubborn", `{}`}), textReply("done")},
			kernel.WithTool(kernel.NewTool(kernel.ToolSpec{Name: "slow", Concurrency: kernel.Serial, Timeout: 2 * time.Second}, func(ctx context.Context, tc *kernel.ToolContext, _ struct{}) (kernel.ToolResult, error) {
				return timeout.Call(ctx, tc, nil)
			})),
			kernel.WithTool(stubborn{d: time.Minute}), kernel.WithLimits(kernel.Limits{CloseTimeout: time.Second}))
		rc, _ := h.sess.Send(kernel.Text("go"))
		rc.Wait(t.Context())
		err := h.sess.Close(t.Context())
		var leak *kernel.LeakError
		if !errors.As(err, &leak) || leak.Workers[0].Name != "tool toolu_2" {
			t.Fatalf("close: %v", err)
		}
		time.Sleep(time.Minute) // let the abandoned call return, since a bubble may not end with a goroutine still blocked
		h.close()
		got := toolResults(h.lines())
		if !strings.Contains(firstText(got[0]), "time limit of 2s") || !strings.Contains(firstText(got[1]), "time limit of 1s") {
			t.Fatalf("results %q / %q", firstText(got[0]), firstText(got[1]))
		}
		if h.rec.count(event.TypeToolAbandoned) != 1 || h.sess.Snapshot().Usage.Abandons != 1 {
			t.Fatal("the stubborn call was not abandoned")
		}
	})
}

type trip struct {
	Legs []struct {
		From string `json:"from"`
		To   string `json:"to"`
	} `json:"legs"`
	Hotel *struct {
		Name string `json:"name"`
	} `json:"hotel,omitempty"`
	Tags map[string]struct {
		Note string `json:"note"`
	} `json:"tags"`
	Skip string `json:"-"`
}

func TestNewToolStrictNested(t *testing.T) {
	tool := kernel.NewTool(kernel.ToolSpec{Name: "trip"}, func(ctx context.Context, tc *kernel.ToolContext, in trip) (kernel.ToolResult, error) {
		return kernel.TextResult(in.Legs[0].To), nil
	})
	cases := map[string]string{
		`{"legs":[{"from":"A","to":"B"}],"hotel":{"name":"H"},"tags":{"x":{"note":"n"}}}`: "",
		`{"legs":[{"from":"A","To":"B"}]}`:                                                `unknown field "legs[0].To"`,
		`{"hotel":{"name":"H","stars":5}}`:                                                `unknown field "hotel.stars"`,
		`{"tags":{"x":{"note":"n","extra":1}}}`:                                           `unknown field "tags.x.extra"`,
		`{"legs":[],"legs":[]}`:                                                           `field "legs" appears more than once`,
		`{"Skip":"x"}`:                                                                    `unknown field "Skip"`,
		`{"legs":"none"}`:                                                                 `field "legs" must be an array`,
	}
	for input, want := range cases {
		_, err := tool.Call(t.Context(), nil, json.RawMessage(input))
		var ie *kernel.InputError
		switch {
		case want == "" && err != nil:
			t.Errorf("%s: %v", input, err)
		case want != "" && (!errors.As(err, &ie) || !strings.Contains(ie.Detail, want)):
			t.Errorf("%s: got %v, want %s", input, err, want)
		}
	}
}

func TestToolEmitAndErrorResult(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newHarness(t, [][]providertest.Step{toolReply(call{"toolu_1", "ping", `{}`}), textReply("ok")}, kernel.WithTool(emitter{}))
		rc, _ := h.sess.Send(kernel.Text("go"))
		rc.Wait(t.Context())
		h.close()
		var got string
		for _, ev := range h.rec.all() {
			if d, ok := ev.Data.(*pinged); ok && ev.TurnID == "turn_01" && ev.Step == 1 {
				got = d.Who
			}
		}
		res := toolResults(h.lines())[0]
		if got != "toolu_1" || !res.IsError || res.Outcome != message.OutcomeError || firstText(res) != "failed 2 times" {
			t.Fatalf("emit %q, result %+v", got, res)
		}
	})
}

// stubborn ignores cancellation, which the kernel survives by abandoning it after the grace period.
type stubborn struct{ d time.Duration }

func (s stubborn) Spec() kernel.ToolSpec {
	return kernel.ToolSpec{Name: "stubborn", Concurrency: kernel.Serial, Timeout: time.Second}
}

func (s stubborn) Call(context.Context, *kernel.ToolContext, json.RawMessage) (kernel.ToolResult, error) {
	time.Sleep(s.d)
	return kernel.TextResult("finally"), nil
}

// emitter publishes an extension event from inside its call.
type emitter struct{}

type pinged struct {
	Who string `json:"who"`
}

func (*pinged) EventType() event.Type { return "x_test.pinged" }

func (emitter) Spec() kernel.ToolSpec { return kernel.ToolSpec{Name: "ping"} }

func (emitter) Call(_ context.Context, tc *kernel.ToolContext, _ json.RawMessage) (kernel.ToolResult, error) {
	if tc.Session() == nil || tc.Path() != nil {
		return kernel.ErrorResult("context of a root call"), nil
	}
	tc.Emit(&pinged{Who: tc.CallID()})
	return kernel.ErrorResult("failed %d times", 2), nil
}
