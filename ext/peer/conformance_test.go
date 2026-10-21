package peer_test

import (
	"context"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/estonshi/aperture-potatos/conformance"
	"github.com/estonshi/aperture-potatos/ext/peer"
	"github.com/estonshi/aperture-potatos/kernel"
	"github.com/estonshi/aperture-potatos/provider"
	"github.com/estonshi/aperture-potatos/provider/providertest"
	"github.com/estonshi/aperture-potatos/std"
	"github.com/estonshi/aperture-potatos/store"
)

var update = flag.Bool("update", false, "rewrite expected files from the Go reference implementation")

// caseRound is one scripted answer of a fake peer; its texts may name {{text}}, {{context_id}} and {{task_id}} of the message it received.
type caseRound struct {
	Events    []peer.Event `json:"events"`
	StartMS   int64        `json:"start_ms"` // before the first event
	DelayMS   int64        `json:"delay_ms"` // before the events after the first
	Hold      bool         `json:"hold"`     // after the events, until a cancel
	AuthError *struct {
		Status int    `json:"status"`
		Detail string `json:"detail"`
	} `json:"auth_error"`
}

type casePeer struct {
	Name        string                  `json:"name"`
	Description string                  `json:"description"`
	Skills      []peer.CardSkill        `json:"skills"`
	Rounds      []caseRound             `json:"rounds"`
	Subscribe   map[string][]peer.Event `json:"subscribe"`
}

// caseReply is one answer of an in-process peer's model: text, or one tool call, after an optional hold.
type caseReply struct {
	Text   string          `json:"text"`
	Tool   string          `json:"tool"`
	Input  json.RawMessage `json:"input"`
	HoldMS int64           `json:"hold_ms"`
}

type caseLocal struct {
	Nickname     string      `json:"nickname"`
	Description  string      `json:"description"`
	Instructions string      `json:"instructions"`
	Replies      []caseReply `json:"replies"`
	Peers        []casePeer  `json:"peers"` // its own fake peers
}

type caseOptions struct {
	Peers []casePeer  `json:"peers"`
	Local []caseLocal `json:"local"`
}

func fakeOf(cp casePeer) *fake {
	f := newFake(cp.Name)
	f.card.Description, f.card.Skills = cp.Description, cp.Skills
	for _, r := range cp.Rounds {
		rd := round{events: r.Events, start: time.Duration(r.StartMS) * time.Millisecond, delay: time.Duration(r.DelayMS) * time.Millisecond, hold: r.Hold}
		if a := r.AuthError; a != nil {
			rd.err = &peer.AuthError{Status: a.Status, Detail: a.Detail}
		}
		f.rounds = append(f.rounds, rd)
	}
	for id, evs := range cp.Subscribe {
		f.subs[id] = evs
	}
	return f
}

// localAgent is an in-process peer with a scripted model, its own clock and ids apart from the case's.
func localAgent(cl caseLocal) (*kernel.Agent, error) {
	var scripts [][]providertest.Step
	for i, r := range cl.Replies {
		var steps []providertest.Step
		if r.HoldMS > 0 {
			steps = append(steps, providertest.Step{Hold: time.Duration(r.HoldMS) * time.Millisecond})
		}
		if r.Tool != "" {
			steps = append(steps, calls(call{"call_" + string(rune('a'+i)), r.Tool, string(r.Input)})...)
		} else {
			steps = append(steps, reply(r.Text)...)
		}
		scripts = append(scripts, steps)
	}
	sp := providertest.NewScriptProvider(provider.APIAnthropicMessages, scripts...)
	opts := []kernel.Option{
		kernel.WithNickname(cl.Nickname), kernel.WithDescription(cl.Description), kernel.WithInstructions(cl.Instructions),
		kernel.WithModel(provider.ModelRef{Provider: sp, Model: "m"}), kernel.WithStore(store.NewMemory()),
		kernel.WithClock(stepClock()), otherIDs(cl.Nickname + "_"), kernel.WithPanicOnInternalError(true),
	}
	if len(cl.Peers) > 0 {
		var ps []peer.Peer
		for _, cp := range cl.Peers {
			ps = append(ps, fakeOf(cp))
		}
		opts = append(opts, kernel.Use(peer.New(ps...)))
	}
	return kernel.New(opts...)
}

// build makes this extension from the case's options and takes any other one from std, so a case that combines them skips once that one is deleted.
func build(name string, raw json.RawMessage) (kernel.Extension, error) {
	if name != peer.Name {
		if i := slices.IndexFunc(std.Extensions(), func(e kernel.Extension) bool { return e.Name() == name }); i >= 0 {
			return std.Extensions()[i], nil
		}
		return nil, conformance.ErrExtensionMissing
	}
	var o caseOptions
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &o); err != nil {
			return nil, err
		}
	}
	var ps []peer.Peer
	for _, cp := range o.Peers {
		ps = append(ps, fakeOf(cp))
	}
	var locals []*kernel.Agent
	for _, cl := range o.Local {
		ag, err := localAgent(cl)
		if err != nil {
			return nil, err
		}
		locals = append(locals, ag)
		ps = append(ps, peer.Local(ag))
	}
	ext := peer.New(ps...)
	if len(locals) == 0 {
		return ext, nil
	}
	return kernel.ExtensionFunc(peer.Name, func(r kernel.Registrar) error {
		if err := ext.Install(r); err != nil {
			return err
		}
		// the case's session is its agent's only one, so its end is when the in-process peers close
		_, err := kernel.On(r, kernel.SessionEnd, "close-local-peers", func(ctx context.Context, _ kernel.SessionEndInput) (kernel.SessionEndOutput, error) {
			for _, ag := range locals {
				_ = ag.Close(context.WithoutCancel(ctx))
			}
			return kernel.SessionEndOutput{}, nil
		})
		return err
	}), nil
}

// TestConformance runs conformance/loop/ext/peer, the cases this extension owns and deletes with itself.
func TestConformance(t *testing.T) {
	root := filepath.Join("..", "..", "conformance", "loop", "ext", "peer")
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Skip("no cases")
	}
	for _, e := range entries {
		dir := filepath.Join(root, e.Name())
		t.Run(e.Name(), func(t *testing.T) {
			res := conformance.RunLoopWith(t, dir, build)
			if t.Failed() {
				return
			}
			if err := res.Expected(dir, *update); err != nil {
				t.Fatal(err)
			}
		})
	}
}
