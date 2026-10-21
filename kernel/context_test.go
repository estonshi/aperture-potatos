package kernel_test

import (
	"testing"
	"testing/synctest"

	"github.com/estonshi/aperture-potatos/kernel"
	"github.com/estonshi/aperture-potatos/provider/providertest"
)

func TestContextReportAddsUp(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newHarness(t, [][]providertest.Step{toolReply(call{"c1", "echo", `{"a":1}`}), textReply("done")}, kernel.WithTool(&echoTool{name: "echo"}))
		ask(t, h.sess, "echo")
		rep := h.sess.Context()
		kinds := map[string]bool{}
		var sum int64
		for _, p := range rep.Parts {
			kinds[p.Kind] = true
			sum += p.Tokens
		}
		for _, k := range []string{"system", "tool", "user", "assistant", "tool_call", "tool_result"} {
			if !kinds[k] {
				t.Fatalf("no %s part: %+v", k, rep.Parts)
			}
		}
		if rep.Used <= 0 || sum > rep.Used || rep.Used-sum > int64(len(rep.Parts)) || rep.Epoch != 1 || rep.Threshold <= 0 || rep.Usable <= rep.Threshold {
			t.Fatalf("report: %+v (parts add up to %d)", rep, sum)
		}
		h.close()
	})
}
