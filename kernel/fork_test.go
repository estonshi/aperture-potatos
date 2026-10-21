package kernel_test

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"testing/synctest"

	"github.com/estonshi/aperture-potatos/event"
	"github.com/estonshi/aperture-potatos/kernel"
	"github.com/estonshi/aperture-potatos/message"
	"github.com/estonshi/aperture-potatos/provider"
	"github.com/estonshi/aperture-potatos/provider/providertest"
	"github.com/estonshi/aperture-potatos/store"
	"github.com/estonshi/aperture-potatos/store/jsonl"
)

func TestForkSendsTheSourcePrefix(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx := context.Background()
		h := newHarness(t, [][]providertest.Step{toolReply(call{"c1", "echo", `{"a":1}`}), textReply("echoed"), textReply("main again"), textReply("fork answer")},
			kernel.WithTool(&echoTool{name: "echo"}))
		first := ask(t, h.sess, "echo something")
		fork, err := h.sess.Fork(ctx)
		if err != nil {
			t.Fatal(err)
		}
		ask(t, h.sess, "main goes on")
		ask(t, fork, "fork asks")
		reqs := h.sp.Requests()
		mainReq, forkReq := reqs[2], reqs[3]
		n := len(mainReq.Messages) - 1
		if wire(t, &provider.Request{System: mainReq.System, Tools: mainReq.Tools, Messages: mainReq.Messages[:n]}) != wire(t, &provider.Request{System: forkReq.System, Tools: forkReq.Tools, Messages: forkReq.Messages[:n]}) {
			t.Fatal("the fork's first request does not start with the source's prefix")
		}
		if forkReq.Messages[n].Text() != "fork asks" || len(forkReq.Messages) != n+1 {
			t.Fatalf("fork request: %+v", forkReq.Messages)
		}
		if turns := fork.Turns(); len(turns) != 2 || turns[0].ID != first.TurnID {
			t.Fatalf("the fork keeps the source's turns: %+v", turns)
		}
		if fork.Parent() != nil {
			t.Fatal("a fork has no parent")
		}
		fid := fork.Info().SessionID
		h.close()
		lines, _ := h.store.Load(ctx, fid)
		b := lines[0].Boundary
		if b.Reason != "fork" || b.From == nil || b.From.SessionID != h.sess.Info().SessionID || b.From.Seq != 5 || b.Session == nil || b.Session.ParentSessionID != "" {
			t.Fatalf("fork boundary: %+v %+v", b, b.From)
		}
		if lines[1].CopyOf == "" || lines[1].TurnID != first.TurnID {
			t.Fatalf("copies: %+v", lines[1])
		}
		if roles(store.Current(h.lines())) != "system,user,assistant,tool,assistant,user,assistant" {
			t.Fatal("the fork changed its source")
		}
	})
}

func TestForkToolsRefusesOthersAtTheGate(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx := context.Background()
		echo := &echoTool{name: "echo"}
		h := newHarness(t, [][]providertest.Step{textReply("hi"), toolReply(call{"c1", "echo", `{}`}, call{"c2", "look", `{}`}), textReply("done")},
			kernel.WithTool(echo), kernel.WithTool(&echoTool{name: "look"}))
		ask(t, h.sess, "hello")
		fork, err := h.sess.Fork(ctx, kernel.ForkTools("look"))
		if err != nil {
			t.Fatal(err)
		}
		ask(t, fork, "try both")
		if echo.runsOf("c1") != 0 {
			t.Fatal("a tool outside the allow-list ran")
		}
		reqs := h.sp.Requests()
		if len(reqs[1].Tools) != 2 {
			t.Fatalf("the definitions sent stay the source's: %+v", reqs[1].Tools)
		}
		last := reqs[1].Messages[len(reqs[1].Messages)-1]
		if !strings.Contains(messagesJSON(t, []message.Message{last}), "Only these tools are available in this conversation: look.") {
			t.Fatalf("the first turn tells the model the allowed tools: %s", messagesJSON(t, []message.Message{last}))
		}
		tool := reqs[2].Messages[len(reqs[2].Messages)-1]
		r1, r2 := tool.Content[0].(*message.ToolResultBlock), tool.Content[1].(*message.ToolResultBlock)
		if r1.Outcome != message.OutcomeDenied || text(r1.Content) != "Not executed: echo is not available in this conversation." || r2.Outcome != message.OutcomeSuccess {
			t.Fatalf("results: %+v %+v", r1, r2)
		}
		h.close()
	})
}

func TestSideChatReopensUntilDropped(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx := context.Background()
		h := newHarness(t, [][]providertest.Step{textReply("main"), textReply("side one"), textReply("main two"), textReply("side fresh")})
		ask(t, h.sess, "hello")
		side, err := h.sess.SideChat(ctx, "notes")
		if err != nil {
			t.Fatal(err)
		}
		if again, _ := h.sess.SideChat(ctx, "notes"); again != side {
			t.Fatal("a second open returns the same chat")
		}
		if _, err := h.sess.SideChat(ctx, "Bad Name"); !errors.Is(err, kernel.ErrInvalidName) {
			t.Fatalf("bad name: %v", err)
		}
		mid := h.sess.Info().SessionID
		if side.Info().SessionID != mid+".side.notes" || side.Parent() != h.sess || side.Info().ParentSessionID != mid || h.sess.Info().ParentSessionID != "" {
			t.Fatalf("id %s parent %v", side.Info().SessionID, side.Parent())
		}
		if side.Info().SideChat != "notes" || h.sess.Info().SideChat != "" {
			t.Fatalf("side chat name: %q", side.Info().SideChat)
		}
		ask(t, side, "a side question")
		synctest.Wait()
		if got := h.sp.Requests()[1]; len(got.Messages) != 4 || got.Messages[0].Text() != "hello" || got.Messages[1].Text() != "main" || got.Messages[3].Text() != "a side question" || got.SharedPrefix != 2 || got.CacheKey != mid {
			t.Fatalf("a side request sends the main session's context first, under its cache key: %+v", got)
		}
		if hits, _ := side.SearchHistory(ctx, kernel.HistoryQuery{Words: []string{"hello"}}); len(hits) != 0 {
			t.Fatalf("the main part is not the side chat's own history: %+v", hits)
		}
		if p := partOf(side.Context(), "parent"); p.Count != 2 || p.Tokens <= 0 {
			t.Fatalf("the breakdown counts the main part: %+v", p)
		}
		if b := h.sess.Snapshot().Usage.Buckets["side"]; b.InputTokens != usage.InputTokens {
			t.Fatalf("the side chat's call counts in the main session's side bucket: %+v", h.sess.Snapshot().Usage.Buckets)
		}
		names, _ := h.sess.SideChats(ctx)
		if strings.Join(names, ",") != "notes" {
			t.Fatalf("side chats: %v", names)
		}
		ask(t, h.sess, "main goes on")
		if err := h.sess.DropSideChat(ctx, "notes"); err != nil {
			t.Fatal(err)
		}
		if names, _ := h.sess.SideChats(ctx); len(names) != 0 {
			t.Fatalf("dropped chat still listed: %v", names)
		}
		if _, err := h.ag.Resume(ctx, mid+".side.notes"); !errors.Is(err, kernel.ErrSessionNotFound) {
			t.Fatalf("a dropped chat does not resume: %v", err)
		}
		fresh, err := h.sess.SideChat(ctx, "notes")
		if err != nil {
			t.Fatal(err)
		}
		ask(t, fresh, "start over")
		reqs := h.sp.Requests()
		if got := reqs[3].Messages; len(got) != 6 || got[2].Text() != "main goes on" || got[5].Text() != "start over" {
			t.Fatalf("the reopened chat carries the main session as it is now: %+v", got)
		}
		h.close()
		if ph := fresh.Snapshot().Phase; ph != "closed" {
			t.Fatalf("closing the main session closes its side chats: %s", ph)
		}
		recs, _ := h.store.(store.CallStore).LoadCalls(ctx, mid)
		if n := strings.Count(messagesJSONAny(t, recs), `"bucket":"side"`); n != 2 {
			t.Fatalf("the forwarded calls reach the main session's calls file before it closes: %d", n)
		}
		lines, _ := h.store.Load(ctx, mid+".side.notes")
		var reasons []string
		for _, m := range lines {
			if m.Boundary != nil {
				reasons = append(reasons, m.Boundary.Reason)
			}
		}
		if strings.Join(reasons, ",") != "side_chat,dropped,side_chat" || lines[0].Boundary.Session.SideChat != "notes" || lines[0].Boundary.Session.ParentSessionID != mid || len(lines) != 9 {
			t.Fatalf("file: %v %+v", reasons, lines[0].Boundary.Session)
		}
		if cur := store.Current(lines); cur[0].Epoch != 3 || cur[0].Boundary.Session != nil {
			t.Fatalf("the reopened epoch continues the same file: %+v", cur[0])
		}
	})
}

func TestSideChatRestoredAfterRestart(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx := context.Background()
		st := jsonl.New(t.TempDir())
		first := resumeAgent(t, st, textReply("main"), textReply("side"))
		sess, err := first.NewSession(ctx)
		if err != nil {
			t.Fatal(err)
		}
		ask(t, sess, "hello")
		side, err := sess.SideChat(ctx, "q")
		if err != nil {
			t.Fatal(err)
		}
		ask(t, side, "side question")
		id := sess.Info().SessionID
		_ = first.Close(ctx)

		second := resumeAgent(t, st, textReply("side again"))
		back, err := second.Resume(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		if names, _ := back.SideChats(ctx); !slices.Equal(names, []string{"q"}) {
			t.Fatalf("stored side chats: %v", names)
		}
		restored, err := back.SideChat(ctx, "q")
		if err != nil {
			t.Fatal(err)
		}
		if p := partOf(restored.Context(), "parent"); p.Count != 2 {
			t.Fatalf("a restored side chat weighs its main part before its first turn: %+v", p)
		}
		res := ask(t, restored, "and again")
		if res.FinalText != "side again" || restored.Parent() != back {
			t.Fatalf("%+v", res)
		}
		lines, _ := st.Load(ctx, restored.Info().SessionID)
		if len(store.Current(lines)) != 6 || lines[0].Boundary.Reason != "side_chat" {
			t.Fatalf("the restored chat continues its own epoch: %s", roles(store.Current(lines)))
		}
		_ = second.Close(ctx)
		synctest.Wait()
	})
}

func TestSideChatsKeptByCompactionDroppedByClear(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx := context.Background()
		h := newHarness(t, [][]providertest.Step{textReply("one"), textReply("side a"), textReply("Summary."), textReply("side a again"), textReply("side b"), textReply("fresh a")},
			kernel.Use(kernel.ExtensionFunc("comp", func(r kernel.Registrar) error { _, err := r.Compactor(&keepTurnCompactor{}); return err })),
			kernel.WithContextPolicy(kernel.ContextPolicy{MinSummaryTokens: 1, MaxSummaryTokens: 50}),
		)
		ask(t, h.sess, "hello")
		a, err := h.sess.SideChat(ctx, "a")
		if err != nil {
			t.Fatal(err)
		}
		ask(t, a, "side question")
		if _, err := h.sess.Compact(ctx, ""); err != nil {
			t.Fatal(err)
		}
		if ph := a.Snapshot().Phase; ph == "closed" {
			t.Fatal("a compaction keeps the side chat")
		}
		ask(t, a, "after the compaction")
		got := h.sp.Requests()[3].Messages
		if len(got) != 5 || !strings.Contains(messagesJSON(t, got[:1]), "Summary.") || got[2].Text() != "side question" {
			t.Fatalf("the next side turn carries the compacted main context: %s", messagesJSON(t, got))
		}
		if r, ok := got[4].Content[0].(*message.ReminderBlock); !ok || r.Source != "side_chat" || got[4].Text() != "after the compaction" {
			t.Fatalf("the input says the main session changed: %s", messagesJSON(t, got[4:]))
		}
		b, err := h.sess.SideChat(ctx, "b")
		if err != nil {
			t.Fatal(err)
		}
		ask(t, b, "b question")
		if err := h.sess.Clear(ctx); err != nil {
			t.Fatal(err)
		}
		synctest.Wait()
		var dropped []string
		for _, ev := range h.rec.all() {
			if d, ok := ev.Data.(*event.SideChatDropped); ok {
				dropped = append(dropped, d.Name+":"+d.Reason)
			}
		}
		if strings.Join(dropped, ",") != "a:cleared,b:cleared" {
			t.Fatalf("side_chat.dropped: %v", dropped)
		}
		if names, _ := h.sess.SideChats(ctx); len(names) != 0 {
			t.Fatalf("dropped chats still listed: %v", names)
		}
		fresh, err := h.sess.SideChat(ctx, "a")
		if err != nil {
			t.Fatal(err)
		}
		ask(t, fresh, "start over")
		lines, _ := h.store.Load(ctx, fresh.Info().SessionID)
		var reasons []string
		for _, m := range lines {
			if m.Boundary != nil {
				reasons = append(reasons, m.Boundary.Reason)
			}
		}
		if strings.Join(reasons, ",") != "side_chat,dropped,side_chat" {
			t.Fatalf("file: %v", reasons)
		}
		h.close()
	})
}

func TestStaleSideChatMarkedOnReopen(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx := context.Background()
		st := jsonl.New(t.TempDir())
		first := resumeAgent(t, st, textReply("main"), textReply("side"))
		sess, err := first.NewSession(ctx)
		if err != nil {
			t.Fatal(err)
		}
		ask(t, sess, "hello")
		side, err := sess.SideChat(ctx, "q")
		if err != nil {
			t.Fatal(err)
		}
		ask(t, side, "side question")
		id := sess.Info().SessionID
		_ = first.Close(ctx)

		second := resumeAgent(t, st)
		back, err := second.Resume(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		if err := back.Clear(ctx); err != nil { // the side chat is not open, so nothing marks it now
			t.Fatal(err)
		}
		_ = second.Close(ctx)

		third := resumeAgent(t, st, textReply("fresh"))
		again, err := third.Resume(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		if names, _ := again.SideChats(ctx); len(names) != 0 {
			t.Fatalf("a side chat older than the clear is not listed after a restart: %v", names)
		}
		if _, err := third.Resume(ctx, side.Info().SessionID); !errors.Is(err, kernel.ErrSideChat) {
			t.Fatalf("resuming the side chat by id skips its main session's checks: %v", err)
		}
		fresh, err := again.SideChat(ctx, "q")
		if err != nil {
			t.Fatal(err)
		}
		if res := ask(t, fresh, "start over"); res.FinalText != "fresh" {
			t.Fatalf("%+v", res)
		}
		lines, _ := st.Load(ctx, fresh.Info().SessionID)
		var reasons []string
		for _, m := range lines {
			if m.Boundary != nil {
				reasons = append(reasons, m.Boundary.Reason)
			}
		}
		if strings.Join(reasons, ",") != "side_chat,dropped,side_chat" {
			t.Fatalf("reopening marks the stale chat, then opens anew: %v", reasons)
		}
		_ = third.Close(ctx)
		synctest.Wait()
	})
}

func TestForkToolsKeptOnResume(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx := context.Background()
		st := jsonl.New(t.TempDir())
		first := resumeAgent(t, st, textReply("hi"))
		sess, err := first.NewSession(ctx)
		if err != nil {
			t.Fatal(err)
		}
		ask(t, sess, "hello")
		fork, err := sess.Fork(ctx, kernel.ForkTools()) // allows no tool at all, which must survive as an empty list
		if err != nil {
			t.Fatal(err)
		}
		id := fork.Info().SessionID
		_ = first.Close(ctx)

		second := resumeAgent(t, st, toolReply(call{"c1", "echo", `{}`}), textReply("done"))
		back, err := second.Resume(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		ask(t, back, "use echo")
		_ = second.Close(ctx)
		synctest.Wait()
		lines, _ := st.Load(ctx, id)
		if b := lines[0].Boundary; b.ToolsAllowed == nil || len(*b.ToolsAllowed) != 0 {
			t.Fatalf("the fork line records the empty allow-list: %+v", b)
		}
		var got *message.ToolResultBlock
		for _, m := range lines {
			for _, bl := range m.Content {
				if r, ok := bl.(*message.ToolResultBlock); ok && r.CallID == "c1" {
					got = r
				}
			}
		}
		if got == nil || got.Outcome != message.OutcomeDenied {
			t.Fatalf("the resumed fork still refuses tools outside its list: %+v", got)
		}
	})
}

// slowLoadStore holds every Load of the ids that match until release closes.
type slowLoadStore struct {
	store.SessionStore
	match   func(id string) bool
	release chan struct{}
}

func (s *slowLoadStore) Load(ctx context.Context, id string) ([]message.Message, error) {
	if s.match(id) {
		select {
		case <-s.release:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return s.SessionStore.Load(ctx, id)
}

// TestSlowSideChatHoldsOnlyItsName: a side chat whose store is slow to load does not hold up opening another.
func TestSlowSideChatHoldsOnlyItsName(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx := context.Background()
		st := &slowLoadStore{SessionStore: store.NewMemory(), match: func(id string) bool { return strings.HasSuffix(id, ".side.slow") }, release: make(chan struct{})}
		h := newHarness(t, [][]providertest.Step{textReply("main")}, kernel.WithStore(st))
		ask(t, h.sess, "hello")
		slow := make(chan error, 1)
		go func() {
			_, err := h.sess.SideChat(ctx, "slow")
			slow <- err
		}()
		synctest.Wait()
		if _, err := h.sess.SideChat(ctx, "quick"); err != nil {
			t.Fatal(err)
		}
		select {
		case err := <-slow:
			t.Fatalf("the slow open finished before its load was released: %v", err)
		default:
		}
		close(st.release)
		if err := <-slow; err != nil {
			t.Fatal(err)
		}
		names, _ := h.sess.SideChats(ctx)
		if strings.Join(names, ",") != "quick,slow" {
			t.Fatalf("side chats: %v", names)
		}
		h.close()
	})
}

func TestSideChatRefusesNestingAndForkAt(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx := context.Background()
		h := newHarness(t, [][]providertest.Step{textReply("main")})
		ask(t, h.sess, "hello")
		side, err := h.sess.SideChat(ctx, "s")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := side.SideChat(ctx, "inner"); !errors.Is(err, kernel.ErrSideChatUnsupported) {
			t.Fatalf("a side chat opens no side chat: %v", err)
		}
		turns := h.sess.Turns()
		if _, err := h.sess.SideChat(ctx, "at", kernel.ForkAt(turns[0].ID)); !errors.Is(err, kernel.ErrSideChatUnsupported) {
			t.Fatalf("a side chat always carries the current context: %v", err)
		}
		if names, _ := h.sess.SideChats(ctx); !slices.Equal(names, []string{"s"}) {
			t.Fatalf("a refused open leaves nothing behind: %v", names)
		}
		h.close()
	})
}

func TestSideChatThatCopiedTheMainSessionStartsAnew(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx := context.Background()
		h := newHarness(t, [][]providertest.Step{textReply("main"), textReply("fresh")})
		ask(t, h.sess, "hello")
		mid := h.sess.Info().SessionID
		id := mid + ".side.old"
		main := h.sess.Messages()
		sys := main[0]
		sys.SessionID, sys.Seq, sys.Epoch, sys.ID = id, 1, 1, "old_1"
		sys.Boundary = &message.Boundary{Reason: "fork", From: &message.ForkFrom{SessionID: mid, Seq: 3}, Session: &message.SessionMeta{Agent: "Tater", SideChat: "old", ParentSessionID: mid}}
		old := []message.Message{sys}
		for i, m := range main[1:] { // the copies an older version wrote
			cp := m
			cp.SessionID, cp.Seq, cp.Epoch, cp.ID, cp.CopyOf = id, int64(i+2), 1, "old_"+string(rune('2'+i)), m.ID
			old = append(old, cp)
		}
		if err := h.store.Append(ctx, id, old); err != nil {
			t.Fatal(err)
		}
		if names, _ := h.sess.SideChats(ctx); len(names) != 0 {
			t.Fatalf("an old copy is not listed, since opening it starts anew: %v", names)
		}
		side, err := h.sess.SideChat(ctx, "old")
		if err != nil {
			t.Fatal(err)
		}
		ask(t, side, "again")
		if got := h.sp.Requests()[1].Messages; strings.Count(messagesJSON(t, got), `"text":"hello"`) != 1 {
			t.Fatalf("the main session goes out once: %s", messagesJSON(t, got))
		}
		lines, _ := h.store.Load(ctx, id)
		var reasons []string
		for _, m := range lines {
			if m.Boundary != nil {
				reasons = append(reasons, m.Boundary.Reason)
			}
		}
		if strings.Join(reasons, ",") != "fork,dropped,side_chat" {
			t.Fatalf("file: %v", reasons)
		}
		h.close()
	})
}

func TestSideChatLeavesItsSmallPartUncompacted(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx := context.Background()
		big := message.Usage{InputTokens: 400, OutputTokens: 7500}
		long := textReply(strings.Repeat("A long answer. ", 2000))
		long[len(long)-1].Event.Usage, long[len(long)-1].Event.Message.Usage = &big, &big
		comp := &keepTurnCompactor{}
		h := newHarness(t, [][]providertest.Step{long, textReply("side")},
			kernel.Use(kernel.ExtensionFunc("comp", func(r kernel.Registrar) error { _, err := r.Compactor(comp); return err })),
			kernel.WithContextPolicy(kernel.ContextPolicy{MaxContextTokens: 70000, MinSummaryTokens: 1000, MaxSummaryTokens: 2000}),
		)
		ask(t, h.sess, "hello")
		side, err := h.sess.SideChat(ctx, "s")
		if err != nil {
			t.Fatal(err)
		}
		rep := side.Context()
		var parent kernel.ContextPart
		for _, p := range rep.Parts {
			if p.Kind == "parent" {
				parent = p
			}
		}
		if rep.Used < rep.Threshold || parent.Count != 2 || parent.Tokens == 0 {
			t.Fatalf("the main part counts from the start: used %d of %d, part %+v", rep.Used, rep.Threshold, parent)
		}
		ask(t, side, "a side question")
		if comp.calls != 0 || len(h.sp.Requests()) != 2 {
			t.Fatalf("compacting the side chat's own lines would win nothing: %d compactions, %d requests", comp.calls, len(h.sp.Requests()))
		}
		h.close()
	})
}

func partOf(rep kernel.ContextReport, kind string) kernel.ContextPart {
	for _, p := range rep.Parts {
		if p.Kind == kind {
			return p
		}
	}
	return kernel.ContextPart{}
}

func hasSideReminder(m message.Message, text string) bool {
	for _, b := range m.Content {
		if r, ok := b.(*message.ReminderBlock); ok && r.Source == "side_chat" && strings.Contains(r.Text, text) {
			return true
		}
	}
	return false
}

func TestSideChatChangeReminderFollowsTheLatestReply(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx := context.Background()
		h := newHarness(t, [][]providertest.Step{textReply("main"), textReply("r1"), textReply("main two"), textReply("r2"), textReply("aside"), textReply("r3"), textReply("r4")})
		ask(t, h.sess, "hello")
		side, err := h.sess.SideChat(ctx, "s")
		if err != nil {
			t.Fatal(err)
		}
		ask(t, side, "q1")
		ask(t, h.sess, "more")
		second := ask(t, side, "q2")
		if got := h.sp.Requests()[3].Messages; !hasSideReminder(got[len(got)-1], "has changed") {
			t.Fatal("the main session moved after the first reply")
		}
		if _, _, err := side.SideCall(ctx, kernel.SideRequest{IncludeHistory: true, Messages: []message.Message{{Role: message.RoleUser, Content: message.Content{&message.TextBlock{Text: "aside"}}}}}); err != nil {
			t.Fatal(err)
		}
		if _, err := side.Rewind(ctx, second.TurnID); err != nil {
			t.Fatal(err)
		}
		ask(t, side, "q3")
		got := h.sp.Requests()[5].Messages
		if last := got[len(got)-1]; !hasSideReminder(last, "has changed") {
			t.Fatalf("after a rewind the latest reply is r1 again, written before the main session moved: %s", messagesJSON(t, got))
		}
		ask(t, side, "q4")
		if got := h.sp.Requests()[6].Messages; hasSideReminder(got[len(got)-1], "has changed") {
			t.Fatal("nothing moved since r3")
		}
		h.close()
	})
}

// scopeCompactor summarizes twice, once with the whole context and once with lines of its own, and reports what it was given.
type scopeCompactor struct {
	tokens, threshold int64
}

func (c *scopeCompactor) Compact(ctx context.Context, req kernel.CompactRequest) (kernel.CompactResult, error) {
	c.tokens, c.threshold = req.Tokens, req.Threshold
	if _, _, err := req.Summarize(ctx, nil, "Summarize."); err != nil {
		return kernel.CompactResult{}, err
	}
	sum, _, err := req.Summarize(ctx, req.History[req.HeadLen:], "Merge.")
	return kernel.CompactResult{Summary: sum, Strategy: "test"}, err
}

func TestSideChatCompactorSeesItsOwnPart(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx := context.Background()
		comp := &scopeCompactor{}
		h := newHarness(t, [][]providertest.Step{textReply("main"), textReply("side"), textReply("one"), textReply("two")},
			kernel.Use(kernel.ExtensionFunc("comp", func(r kernel.Registrar) error { _, err := r.Compactor(comp); return err })),
			kernel.WithContextPolicy(kernel.ContextPolicy{MinSummaryTokens: 1, MaxSummaryTokens: 50}),
		)
		ask(t, h.sess, "hello")
		side, err := h.sess.SideChat(ctx, "s")
		if err != nil {
			t.Fatal(err)
		}
		ask(t, side, "q1")
		rep, parent := side.Context(), partOf(side.Context(), "parent")
		if _, err := side.Compact(ctx, ""); err != nil {
			t.Fatal(err)
		}
		if comp.tokens != rep.Used-parent.Tokens || comp.threshold != rep.Threshold-parent.Tokens {
			t.Fatalf("the compactor weighs the side chat's own lines against the room left: %d/%d, context %d/%d, main %d", comp.tokens, comp.threshold, rep.Used, rep.Threshold, parent.Tokens)
		}
		reqs := h.sp.Requests()
		whole, own := messagesJSON(t, reqs[2].Messages), messagesJSON(t, reqs[3].Messages)
		if !strings.Contains(whole, `"text":"hello"`) || !strings.Contains(whole, "Summarize only this side conversation") {
			t.Fatalf("the whole context goes with the scope note: %s", whole)
		}
		if strings.Contains(own, `"text":"hello"`) || strings.Contains(own, "Summarize only this side conversation") {
			t.Fatalf("lines of its own carry no main part, so no note: %s", own)
		}
		h.close()
	})
}

// upToCompactor keeps everything from the cut and writes a fixed summary, which on a prefix-bound model marks the epoch replay_fallback.
type upToCompactor struct{}

func (upToCompactor) Compact(_ context.Context, req kernel.CompactRequest) (kernel.CompactResult, error) {
	return kernel.CompactResult{Summary: "Earlier: a greeting.", Replace: req.History[req.Range.Cut:], Strategy: "test"}, nil
}

func TestSideChatFollowsTheMainFallbackReplay(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx := context.Background()
		h := newHarness(t, [][]providertest.Step{textReply("one"), textReply("two"), textReply("side")},
			kernel.Use(kernel.ExtensionFunc("comp", func(r kernel.Registrar) error { _, err := r.Compactor(upToCompactor{}); return err })),
			kernel.WithCapabilities(provider.APIAnthropicMessages, "m", provider.Capabilities{ThinkingPrefixBound: true}),
		)
		ask(t, h.sess, "hello")
		second := ask(t, h.sess, "again")
		if _, err := h.sess.Compact(ctx, "", kernel.CompactUpTo(second.TurnID)); err != nil {
			t.Fatal(err)
		}
		side, err := h.sess.SideChat(ctx, "s")
		if err != nil {
			t.Fatal(err)
		}
		ask(t, side, "q")
		if got := h.sp.Requests()[2]; got.Replay != provider.ReplayFallback || !got.Caps.ThinkingPrefixBound {
			t.Fatalf("the main part kept after a changed prefix renders with the fallback replay: %v", got.Replay)
		}
		h.close()
	})
}

func TestSideChatClearKeepsTheMainPart(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx := context.Background()
		h := newHarness(t, [][]providertest.Step{textReply("main"), textReply("side"), textReply("fresh")})
		ask(t, h.sess, "hello")
		side, err := h.sess.SideChat(ctx, "s")
		if err != nil {
			t.Fatal(err)
		}
		ask(t, side, "q1")
		if err := side.Clear(ctx); err != nil {
			t.Fatal(err)
		}
		ask(t, side, "q2")
		got := h.sp.Requests()[2].Messages
		if len(got) != 4 || got[0].Text() != "hello" || !hasSideReminder(got[2], "main conversation as it is right now") || got[3].Text() != "q2" {
			t.Fatalf("a cleared side chat still carries the main part, then a fresh head: %s", messagesJSON(t, got))
		}
		h.close()
	})
}
