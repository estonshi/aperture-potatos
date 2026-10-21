package std

import (
	"context"
	"slices"
	"testing"
	"testing/synctest"

	"github.com/estonshi/aperture-potatos/event"
	"github.com/estonshi/aperture-potatos/kernel"
	"github.com/estonshi/aperture-potatos/provider"
	"github.com/estonshi/aperture-potatos/provider/providertest"
)

// extensions installs opt and reports the extensions a new session lists, without naming any default, so deleting one never breaks this test.
func extensions(t *testing.T, opt kernel.Option) []string {
	var names []string
	synctest.Test(t, func(t *testing.T) {
		ag, err := kernel.New(kernel.WithModel(provider.ModelRef{Provider: providertest.NewScriptProvider(provider.APIAnthropicMessages), Model: "m"}), opt)
		if err != nil {
			t.Fatal(err)
		}
		defer ag.Close(context.Background())
		_, err = ag.NewSession(t.Context(), kernel.WithSessionFunc(func(_ context.Context, ev event.Event) error {
			if d, ok := ev.Data.(*event.SessionStarted); ok {
				names = d.Extensions
			}
			return nil
		}))
		if err != nil {
			t.Fatal(err)
		}
	})
	return names
}

func TestDefaultsWithWithout(t *testing.T) {
	var all []string
	for _, e := range defaults() {
		all = append(all, e.Name())
	}
	if got := extensions(t, Defaults()); !slices.Equal(got, all) {
		t.Fatalf("defaults %v, want %v", got, all)
	}
	extra := kernel.ExtensionFunc("test.extra", func(kernel.Registrar) error { return nil })
	if got := extensions(t, Defaults(With(extra), Without(all...))); !slices.Equal(got, []string{"test.extra"}) {
		t.Fatalf("with and without %v", got)
	}
	if len(all) == 0 {
		return
	}
	replaced := false
	same := kernel.ExtensionFunc(all[0], func(kernel.Registrar) error { replaced = true; return nil })
	got := extensions(t, Defaults(With(same)))
	if !replaced || len(got) != len(all) || got[len(got)-1] != all[0] {
		t.Fatalf("a With of a default's name replaces it: %v, installed %v", got, replaced)
	}
}
