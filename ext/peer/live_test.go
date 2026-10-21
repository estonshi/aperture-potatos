//go:build live

package peer_test

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/estonshi/aperture-potatos/ext/peer"
	"github.com/estonshi/aperture-potatos/ext/peer/a2a"
	"github.com/estonshi/aperture-potatos/kernel"
	"github.com/estonshi/aperture-potatos/provider"
	"github.com/estonshi/aperture-potatos/provider/openaichat"
	"github.com/estonshi/aperture-potatos/store"
)

func liveModel(t *testing.T) provider.ModelRef {
	model := os.Getenv("OPENAI_MODEL")
	if model == "" || os.Getenv("OPENAI_API_KEY") == "" {
		t.Skip("set OPENAI_API_KEY and OPENAI_MODEL (and OPENAI_BASE_URL for a gateway)")
	}
	p := openaichat.New(openaichat.Config{APIKey: os.Getenv("OPENAI_API_KEY"), BaseURL: os.Getenv("OPENAI_BASE_URL")})
	return provider.ModelRef{Provider: p, Model: model, Params: provider.Params{MaxOutputTokens: 1024}}
}

func liveAgent(t *testing.T, name, desc, instr string, opts ...kernel.Option) *kernel.Agent {
	t.Helper()
	base := []kernel.Option{kernel.WithStore(store.NewMemory()), kernel.WithModel(liveModel(t)), kernel.WithCapabilityResolver(nil),
		kernel.WithNickname(name), kernel.WithDescription(desc), kernel.WithInstructions(instr)}
	ag, err := kernel.New(append(base, opts...)...)
	if err != nil {
		t.Fatal(err)
	}
	return ag
}

// liveAsk runs one turn and logs what it did with the agents.
func liveAsk(t *testing.T, ctx context.Context, s *kernel.Session, text string) kernel.TurnResult {
	t.Helper()
	rc, err := s.Send(kernel.Text(text))
	if err != nil {
		t.Fatal(err)
	}
	res, err := rc.Wait(ctx)
	if err != nil || res.Err != nil {
		t.Fatalf("turn: %v %v", err, res.Err)
	}
	t.Logf("%q -> %q", text, res.FinalText)
	return res
}

func a2aTasks(t *testing.T, s *kernel.Session) []kernel.TaskInfo {
	var out []kernel.TaskInfo
	for _, info := range s.Tasks().List() {
		if info.Kind == peer.Kind {
			t.Logf("task %s %s %q state %s summary %q", info.ID, info.Status, info.Title, info.State, func() string {
				if info.Result != nil {
					return info.Result.Summary
				}
				return info.Error
			}())
			out = append(out, info)
		}
	}
	return out
}

// TestLiveLocal: the model asks an in-process peer, also driven by a model, and uses its reply; a follow-up continues the same conversation.
func TestLiveLocal(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	fr := liveAgent(t, "translator", "Translates English text into French.", "Reply with the French translation only.")
	defer fr.Close(context.Background())
	ag := liveAgent(t, "Tater", "A helpful assistant.", "Be brief. Use other agents when they are better suited.", kernel.Use(peer.New(peer.Local(fr))))
	defer ag.Close(context.Background())
	s, err := ag.NewSession(ctx)
	if err != nil {
		t.Fatal(err)
	}
	res := liveAsk(t, ctx, s, "Ask the translator agent to translate 'good morning, my friend' and tell me its answer.")
	tasks := a2aTasks(t, s)
	if len(tasks) == 0 || tasks[0].Status != kernel.TaskSucceeded || !strings.Contains(strings.ToLower(res.FinalText), "bonjour") {
		t.Fatalf("no successful exchange behind %q", res.FinalText)
	}
	liveAsk(t, ctx, s, "Ask the translator to also translate 'see you tomorrow'.")
	tasks = a2aTasks(t, s)
	var first, last struct {
		ContextID string `json:"context_id"`
	}
	_ = json.Unmarshal(tasks[0].State, &first)
	_ = json.Unmarshal(tasks[len(tasks)-1].State, &last)
	if len(tasks) < 2 || first.ContextID == "" || first.ContextID != last.ContextID {
		t.Fatalf("the follow-up did not continue the conversation: %d tasks, %q then %q", len(tasks), first.ContextID, last.ContextID)
	}
}

// TestLiveRemoteThroughModel: the model talks to the remote A2A agent in A2A_BASE_URL through a2a_send.
func TestLiveRemoteThroughModel(t *testing.T) {
	base := os.Getenv("A2A_BASE_URL")
	if base == "" {
		t.Skip("set A2A_BASE_URL (and A2A_HEADERS)")
	}
	var h map[string]string
	_ = json.Unmarshal([]byte(os.Getenv("A2A_HEADERS")), &h)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	for _, v := range []string{"1.0", "0.3"} {
		t.Run(v, func(t *testing.T) {
			rp, err := a2a.NewRemotePeer(ctx, base, a2a.WithHeaders(h), a2a.WithProtocolVersion(v))
			if err != nil {
				t.Skip(err)
			}
			name := "remote" // the card's name may not be one the model can use
			ag := liveAgent(t, "Tater", "A helpful assistant.", "Be brief.", kernel.Use(peer.New(peer.Named(rp, name))))
			defer ag.Close(context.Background())
			s, err := ag.NewSession(ctx)
			if err != nil {
				t.Fatal(err)
			}
			liveAsk(t, ctx, s, "Send a short hello to the agent "+name+" with a2a_send and tell me what it answered.")
			tasks := a2aTasks(t, s)
			if len(tasks) == 0 || tasks[0].Status != kernel.TaskSucceeded && tasks[0].Status != kernel.TaskWaitingInput {
				t.Fatalf("no exchange with the remote agent: %+v", tasks)
			}
		})
	}
}
