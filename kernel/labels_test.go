package kernel_test

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"strings"
	"sync"
	"testing"
	"testing/synctest"

	"github.com/estonshi/aperture-potatos/kernel"
	"github.com/estonshi/aperture-potatos/message"
	"github.com/estonshi/aperture-potatos/provider/providertest"
	"github.com/estonshi/aperture-potatos/store"
	"github.com/estonshi/aperture-potatos/store/jsonl"
)

// plainStore hides every optional interface of the store it wraps.
type plainStore struct{ in store.SessionStore }

func (p plainStore) Append(ctx context.Context, id string, msgs []message.Message) error {
	return p.in.Append(ctx, id, msgs)
}
func (p plainStore) Load(ctx context.Context, id string) ([]message.Message, error) {
	return p.in.Load(ctx, id)
}
func (p plainStore) List(ctx context.Context, prefix string) ([]string, error) {
	return p.in.List(ctx, prefix)
}

// failingLabels refuses to save labels once fail is set.
type failingLabels struct {
	store.SessionStore
	store.LabelStore
	fail bool
}

func (f *failingLabels) SaveLabels(ctx context.Context, rec store.LabelRecord) error {
	if f.fail {
		return errors.New("disk full")
	}
	return f.LabelStore.SaveLabels(ctx, rec)
}

func TestSetLabelsMergesAndSurvivesResume(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx := context.Background()
		st := jsonl.New(t.TempDir())
		first := resumeAgent(t, st)
		sess, err := first.NewSession(ctx, kernel.WithLabels(map[string]string{"user_id": "u1", "project_id": "p1"}))
		if err != nil {
			t.Fatal(err)
		}
		if err := sess.SetLabels(ctx, map[string]string{"user_id": "u2", "project_id": "", "tier": "gold"}); err != nil {
			t.Fatal(err)
		}
		want := map[string]string{"user_id": "u2", "tier": "gold"}
		if got := sess.Info().Labels; !maps.Equal(got, want) {
			t.Fatalf("merged at once: %v", got)
		}
		if err := sess.SetLabels(ctx, map[string]string{"": "x"}); !errors.Is(err, kernel.ErrInvalidName) {
			t.Fatalf("an empty key: %v", err)
		}
		id := sess.Info().SessionID
		lines, _ := st.Load(ctx, id)
		if got := lines[0].Boundary.Session.Labels; got["user_id"] != "u1" || got["project_id"] != "p1" {
			t.Fatalf("the first line keeps the labels at creation: %v", got)
		}
		_ = first.Close(ctx)
		if err := sess.SetLabels(ctx, map[string]string{"tier": "x"}); !errors.Is(err, kernel.ErrSessionClosed) {
			t.Fatalf("a closed session: %v", err)
		}

		second := resumeAgent(t, st)
		back, err := second.Resume(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		if got := back.Info().Labels; !maps.Equal(got, want) {
			t.Fatalf("a resume reads the record: %v", got)
		}
		_ = second.Close(ctx)

		third := resumeAgent(t, st)
		again, err := third.Resume(ctx, id, kernel.WithLabels(map[string]string{"tier": "silver", "region": "eu"}))
		if err != nil {
			t.Fatal(err)
		}
		want = map[string]string{"user_id": "u2", "tier": "silver", "region": "eu"}
		if got := again.Info().Labels; !maps.Equal(got, want) {
			t.Fatalf("WithLabels on a resume merges like SetLabels: %v", got)
		}
		_ = third.Close(ctx)
		if found, _ := resumeAgent(t, st).FindSessions(ctx, map[string]string{"region": "eu"}); len(found) != 1 || !maps.Equal(found[0].Labels, want) {
			t.Fatalf("and stores what it merged: %+v", found)
		}
		synctest.Wait()
	})
}

func TestFindSessionsKeepsToTopLevelSessions(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx := context.Background()
		h := newHarness(t, [][]providertest.Step{textReply("main")})
		bare := h.sess
		main, err := h.ag.NewSession(ctx, kernel.WithLabels(map[string]string{"team": "a"}))
		if err != nil {
			t.Fatal(err)
		}
		ask(t, main, "hello")
		b, err := h.ag.NewSession(ctx, kernel.WithLabels(map[string]string{"team": "b"}))
		if err != nil {
			t.Fatal(err)
		}
		fork, err := main.Fork(ctx)
		if err != nil {
			t.Fatal(err)
		}
		side, err := main.SideChat(ctx, "s")
		if err != nil {
			t.Fatal(err)
		}
		if side.Info().Labels["team"] != "a" {
			t.Fatalf("a side chat starts with its main session's labels: %v", side.Info().Labels)
		}
		ids := func(found []kernel.SessionLabels) string {
			var out []string
			for _, f := range found {
				out = append(out, f.SessionID)
			}
			return strings.Join(out, ",")
		}
		mainID, forkID := main.Info().SessionID, fork.Info().SessionID
		if found, err := h.ag.FindSessions(ctx, map[string]string{"team": "a"}); err != nil || ids(found) != mainID+","+forkID {
			t.Fatalf("the main session and its fork, not the side chat: %s %v", ids(found), err)
		}
		if found, _ := h.ag.FindSessions(ctx, nil); ids(found) != strings.Join([]string{bare.Info().SessionID, mainID, b.Info().SessionID, forkID}, ",") {
			t.Fatalf("no condition lists every top-level session in id order: %s", ids(found))
		}
		if found, _ := h.ag.FindSessions(ctx, map[string]string{"team": ""}); ids(found) != bare.Info().SessionID {
			t.Fatalf("an empty value matches a missing key: %s", ids(found))
		}
		if err := side.SetLabels(ctx, map[string]string{"team": "c"}); err != nil {
			t.Fatal(err)
		}
		if found, _ := h.ag.FindSessions(ctx, map[string]string{"team": "c"}); len(found) != 0 || main.Info().Labels["team"] != "a" {
			t.Fatalf("a side chat changes only its own labels, and stays out of queries: %s", ids(found))
		}
		h.close()
	})
}

func TestLabelsNeedALabelStore(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx := context.Background()
		mem := store.NewMemory()
		ag := resumeAgent(t, plainStore{mem})
		sess, err := ag.NewSession(ctx, kernel.WithLabels(map[string]string{"user_id": "u1"}))
		if err != nil {
			t.Fatal(err)
		}
		if err := sess.SetLabels(ctx, map[string]string{"user_id": "u2"}); !errors.Is(err, kernel.ErrNoLabelStore) {
			t.Fatalf("set: %v", err)
		}
		if _, err := ag.FindSessions(ctx, nil); !errors.Is(err, kernel.ErrNoLabelStore) {
			t.Fatalf("find: %v", err)
		}
		id := sess.Info().SessionID
		_ = ag.Close(ctx)
		again := resumeAgent(t, plainStore{mem})
		back, err := again.Resume(ctx, id, kernel.WithLabels(map[string]string{"tier": "gold"}))
		if err != nil {
			t.Fatal(err)
		}
		if got := back.Info().Labels; !maps.Equal(got, map[string]string{"tier": "gold"}) {
			t.Fatalf("without a record WithLabels holds for this opening only, as before: %v", got)
		}
		_ = again.Close(ctx)

		fresh := resumeAgent(t, mem) // the same session under a store that keeps labels gets a record from its first line
		if _, err := fresh.Resume(ctx, id); err != nil {
			t.Fatal(err)
		}
		if found, _ := fresh.FindSessions(ctx, map[string]string{"user_id": "u1"}); len(found) != 1 || found[0].SessionID != id {
			t.Fatalf("a session from before label records is found once resumed: %+v", found)
		}
		_ = fresh.Close(ctx)
		synctest.Wait()
	})
}

func TestSetLabelsKeepsLabelsWhenTheStoreFails(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx := context.Background()
		mem := store.NewMemory()
		fl := &failingLabels{SessionStore: mem, LabelStore: mem.(store.LabelStore)}
		ag := resumeAgent(t, fl)
		sess, err := ag.NewSession(ctx, kernel.WithLabels(map[string]string{"user_id": "u1"}))
		if err != nil {
			t.Fatal(err)
		}
		fl.fail = true
		if err := sess.SetLabels(ctx, map[string]string{"user_id": "u2"}); err == nil || sess.Info().Labels["user_id"] != "u1" {
			t.Fatalf("a failed save changes nothing: %v %v", err, sess.Info().Labels)
		}
		_ = ag.Close(ctx)
		synctest.Wait()
	})
}

func TestConcurrentSetLabelsKeepEveryKey(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx := context.Background()
		h := newHarness(t, nil)
		var wg sync.WaitGroup
		for i := range 20 {
			wg.Go(func() {
				if err := h.sess.SetLabels(ctx, map[string]string{fmt.Sprintf("k%02d", i): "v"}); err != nil {
					t.Error(err)
				}
			})
		}
		wg.Wait()
		if got := h.sess.Info().Labels; len(got) != 20 {
			t.Fatalf("every merge lands: %v", got)
		}
		if found, _ := h.ag.FindSessions(ctx, map[string]string{"k00": "v", "k19": "v"}); len(found) != 1 {
			t.Fatalf("and the record holds them all: %+v", found)
		}
		h.close()
	})
}
