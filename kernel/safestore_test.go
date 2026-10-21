package kernel_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"testing/synctest"

	"github.com/estonshi/aperture-potatos/kernel"
	"github.com/estonshi/aperture-potatos/message"
	"github.com/estonshi/aperture-potatos/provider"
	"github.com/estonshi/aperture-potatos/provider/providertest"
	"github.com/estonshi/aperture-potatos/store"
)

// panicky is a store whose every method panics, as a buggy caller store would.
type panicky struct{}

func (panicky) Append(context.Context, string, []message.Message) error      { panic("append bug") }
func (panicky) Load(context.Context, string) ([]message.Message, error)      { panic("load bug") }
func (panicky) List(context.Context, string) ([]string, error)               { panic("list bug") }
func (panicky) AppendCalls(context.Context, string, []json.RawMessage) error { panic("calls bug") }
func (panicky) LoadCalls(context.Context, string) ([]json.RawMessage, error) { panic("calls bug") }

var _ store.CallStore = panicky{}

// TestStorePanicIsAnError: a panic in the caller's store reaches the caller as an error instead of crashing the host.
func TestStorePanicIsAnError(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		sp := providertest.NewScriptProvider(provider.APIAnthropicMessages, textReply("hi"))
		ag, err := kernel.New(kernel.WithModel(provider.ModelRef{Provider: sp, Model: "m"}), kernel.WithStore(panicky{}), kernel.WithStorePolicy(kernel.StoreStrict))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := ag.NewSession(t.Context()); err == nil || !strings.Contains(err.Error(), "append bug") {
			t.Fatalf("new session: %v", err)
		}
		if _, err := ag.Resume(t.Context(), "ses_x"); err == nil || !strings.Contains(err.Error(), "load bug") {
			t.Fatalf("resume: %v", err)
		}
		_ = ag.Close(t.Context())

		loose, _ := kernel.New(kernel.WithModel(provider.ModelRef{Provider: sp, Model: "m"}), kernel.WithStore(panicky{}))
		s, err := loose.NewSession(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		rc, _ := s.Send(kernel.Text("hello")) // its call goes to the calls writer, whose store panics too
		if _, err := rc.Wait(t.Context()); err != nil {
			t.Fatal(err)
		}
		if _, err := s.SideChats(t.Context()); err == nil || !strings.Contains(err.Error(), "list bug") {
			t.Fatalf("side chats: %v", err)
		}
		_ = loose.Close(t.Context())
	})
}
