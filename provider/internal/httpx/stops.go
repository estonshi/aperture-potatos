package httpx

import (
	"encoding/json"
	"fmt"
	"sync"

	"github.com/estonshi/aperture-potatos/provider"
	"github.com/estonshi/aperture-potatos/spec"
)

var stops = sync.OnceValue(func() (doc struct {
	Fallback provider.StopReason                             `json:"fallback"`
	APIs     map[provider.API]map[string]provider.StopReason `json:"apis"`
}) {
	if err := json.Unmarshal([]byte(spec.MustText("data/stop_reasons.json")), &doc); err != nil {
		panic(fmt.Sprintf("httpx: stop_reasons.json: %v", err))
	}
	return doc
})

// StopReason maps a wire stop value by spec/data/stop_reasons.json; unlisted values become other.
func StopReason(api provider.API, raw string) provider.StopReason {
	if r, ok := stops().APIs[api][raw]; ok {
		return r
	}
	return stops().Fallback
}
