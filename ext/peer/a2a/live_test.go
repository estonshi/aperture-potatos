//go:build live

package a2a

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/estonshi/aperture-potatos/ext/peer"
)

// liveConfig reads A2A_BASE_URL and A2A_HEADERS (a JSON object of headers sent on every request, credentials included).
func liveConfig(t *testing.T) (string, map[string]string) {
	base := os.Getenv("A2A_BASE_URL")
	if base == "" {
		t.Skip("set A2A_BASE_URL (and A2A_HEADERS) to run against a remote A2A agent")
	}
	var h map[string]string
	if raw := os.Getenv("A2A_HEADERS"); raw != "" {
		if err := json.Unmarshal([]byte(raw), &h); err != nil {
			t.Fatalf("A2A_HEADERS: %v", err)
		}
	}
	return base, h
}

func liveMessage(text, ctxID, taskID string) peer.Message {
	return peer.Message{ID: newID(), ContextID: ctxID, TaskID: taskID, Role: "user", Parts: []peer.Part{{Text: text}}}
}

func newID() string { return "live-" + time.Now().Format("150405.000000000") }

// round sends one message and logs what came back; it returns the last state, the ids and the reply text.
func round(t *testing.T, ctx context.Context, p peer.Peer, msg peer.Message) (peer.TaskState, string, string, string) {
	t.Helper()
	var state peer.TaskState
	var ctxID, taskID, reply string
	for ev, err := range p.Send(ctx, msg, peer.SendOptions{}) {
		if err != nil {
			t.Fatalf("send: %v", err)
		}
		if ev.ContextID != "" {
			ctxID = ev.ContextID
		}
		if ev.TaskID != "" {
			taskID = ev.TaskID
		}
		switch ev.Kind {
		case peer.EventMessage:
			state, reply = peer.StateCompleted, text(ev.Message)
		case peer.EventArtifact:
			for _, part := range ev.Artifact.Parts {
				if ev.Append {
					reply += part.Text
				} else {
					reply = part.Text
				}
			}
		default:
			if ev.State != "" {
				state = ev.State
			}
			if s := text(ev.Message); s != "" && ev.Kind == peer.EventStatus {
				t.Logf("  status %s: %q", ev.State, s)
			}
		}
		t.Logf("  %s %s", ev.Kind, ev.State)
	}
	return state, ctxID, taskID, reply
}

// TestLiveRemote talks to the agent in every version and binding its card offers: a streamed exchange, a second message in the same context, an answer when it asks for input, and a cancel.
func TestLiveRemote(t *testing.T) {
	base, headers := liveConfig(t)
	for _, v := range []string{v10, v03} {
		for _, b := range []Binding{BindingJSONRPC, BindingHTTPJSON} {
			t.Run(v+"/"+string(b), func(t *testing.T) {
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
				defer cancel()
				p, err := NewRemotePeer(ctx, base, WithHeaders(headers), WithProtocolVersion(v), WithBinding(b))
				if errors.Is(err, ErrNoSupportedInterface) {
					t.Skip("the card does not offer it")
				}
				if err != nil {
					t.Fatal(err)
				}
				c := p.Card()
				t.Logf("card %q version %q streaming %v url %s skills %d", c.Name, c.Version, c.Streaming, c.URL, len(c.Skills))

				for _, err := range p.Subscribe(ctx, "no-such-task-"+newID()) {
					t.Logf("unknown task recognized %v: %v", errors.Is(err, peer.ErrUnknownTask), err) // logged, not failed: a2a-sdk's v0.3 JSON-RPC reports it as -32603
					break
				}

				state, ctxID, taskID, reply := round(t, ctx, p, liveMessage("Hello! Please answer with one short sentence.", "", ""))
				t.Logf("first: %s context %s task %s reply %q", state, ctxID, taskID, reply)
				if !state.EndsRound() {
					t.Fatalf("the round ended in %s", state)
				}
				if state == peer.StateInputRequired {
					state, _, _, reply = round(t, ctx, p, liveMessage("Just say hello.", ctxID, taskID))
					t.Logf("answered: %s reply %q", state, reply)
				}
				if state != peer.StateCompleted || reply == "" {
					t.Fatalf("first exchange: %s %q", state, reply)
				}

				state, ctx2, _, reply := round(t, ctx, p, liveMessage("What did I ask you a moment ago? Answer in one sentence.", ctxID, ""))
				t.Logf("same context: %s context %s reply %q", state, ctx2, reply)
				if !state.EndsRound() {
					t.Fatalf("second round ended in %s", state)
				}

				// a long request: GetTask while it works, then a cancel with its stream still open, which should end the round as canceled
				var long string
				var last peer.TaskState
				canceled := false
				for ev, err := range p.Send(ctx, liveMessage("Write a 2000-word essay about the history of tea.", "", ""), peer.SendOptions{}) {
					if err != nil {
						t.Fatalf("long request: %v", err)
					}
					if ev.State != "" {
						last = ev.State
					}
					if ev.TaskID == "" || ev.Kind == peer.EventMessage || canceled {
						continue
					}
					long, canceled = ev.TaskID, true
					for snap, err := range p.Subscribe(ctx, long) {
						t.Logf("GetTask while it works: %s %v", snap.State, err)
						break
					}
					if err := p.Cancel(ctx, long); err != nil {
						t.Fatalf("cancel %s: %v", long, err)
					}
				}
				t.Logf("the long request ended %s (task %q)", last, long)
				if long != "" && last != peer.StateCanceled {
					t.Fatalf("the canceled task ended %s", last)
				}
			})
		}
	}
}
