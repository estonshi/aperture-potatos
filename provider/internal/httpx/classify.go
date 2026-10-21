package httpx

import (
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/estonshi/aperture-potatos/provider"
	"github.com/estonshi/aperture-potatos/spec"
)

type rule struct {
	API          provider.API        `json:"api"`
	MidStream    bool                `json:"mid_stream"`
	Status       []int               `json:"status"`
	StatusRange  []int               `json:"status_range"`
	Type         []string            `json:"type"`
	Code         []string            `json:"code"`
	CodeMatch    string              `json:"code_match"`
	Message      string              `json:"message"`
	HeaderAbsent []string            `json:"header_absent"`
	Class        provider.ErrorClass `json:"class"`
	Rule         string              `json:"rule"`
	Limit        string              `json:"limit"`

	message, limit, codeMatch *regexp.Regexp
}

var rules = sync.OnceValue(func() []rule {
	var doc struct {
		Rules []rule `json:"rules"`
	}
	if err := json.Unmarshal([]byte(spec.MustText("data/errors.json")), &doc); err != nil {
		panic(fmt.Sprintf("httpx: errors.json: %v", err))
	}
	for i := range doc.Rules {
		r := &doc.Rules[i]
		if r.Message != "" {
			r.message = regexp.MustCompile(r.Message)
		}
		if r.Limit != "" {
			r.limit = regexp.MustCompile(r.Limit)
		}
		if r.CodeMatch != "" {
			r.codeMatch = regexp.MustCompile(r.CodeMatch)
		}
	}
	return doc.Rules
})

// wireError is the part of an error body the rules look at; both Anthropic's and OpenAI's shapes fit it.
type wireError struct {
	Error struct {
		Type    string          `json:"type"`
		Code    json.RawMessage `json:"code"`
		Message string          `json:"message"`
		Details struct {
			ErrorCode string `json:"error_code"`
		} `json:"details"`
	} `json:"error"`
	Message   string `json:"message"`
	RequestID string `json:"request_id"`
}

func (w wireError) code() string {
	var s string
	if json.Unmarshal(w.Error.Code, &s) == nil && s != "" {
		return s
	}
	if n, err := strconv.ParseInt(string(w.Error.Code), 10, 64); err == nil {
		return strconv.FormatInt(n, 10)
	}
	return w.Error.Details.ErrorCode
}

// Classify maps an HTTP error response, or an error event received after HTTP 200, to a classified error by spec/data/errors.json.
func Classify(api provider.API, status int, header http.Header, body []byte, midStream bool) *provider.Error {
	var w wireError
	isJSON := json.Unmarshal(body, &w) == nil
	e := &provider.Error{API: api, Status: status, Type: w.Error.Type, Code: w.code(), Message: w.Error.Message, MidStream: midStream, RequestID: w.RequestID}
	if e.Message == "" {
		e.Message = w.Message
	}
	if e.Message == "" {
		e.Message = strings.TrimSpace(string(body[:min(len(body), 500)]))
	}
	if e.Message == "" {
		e.Message = http.StatusText(status)
	}
	if isJSON && len(body) > 0 {
		e.Raw = append(json.RawMessage(nil), body...)
	}
	if header != nil {
		if e.RequestID == "" {
			e.RequestID = RequestID(header)
		}
		e.RetryAfter = RetryAfter(header, time.Now())
		e.NoRetry = strings.EqualFold(header.Get("x-should-retry"), "false")
	}
	for _, r := range rules() {
		if r.matches(api, e, header) {
			e.Class, e.Rule = r.Class, r.Rule
			if r.limit != nil {
				if m := r.limit.FindStringSubmatch(e.Message); len(m) > 1 {
					e.Limit, _ = strconv.Atoi(m[1])
				}
			}
			return e
		}
	}
	e.Class = provider.ClassInvalidRequest
	if midStream || status >= 500 {
		e.Class = provider.ClassTransient
	}
	return e
}

func (r *rule) matches(api provider.API, e *provider.Error, h http.Header) bool {
	switch {
	case r.API != api, r.MidStream != e.MidStream:
		return false
	case len(r.Status) > 0 && !slices.Contains(r.Status, e.Status):
		return false
	case len(r.StatusRange) == 2 && (e.Status < r.StatusRange[0] || e.Status > r.StatusRange[1]):
		return false
	case len(r.Type) > 0 && !slices.Contains(r.Type, e.Type):
		return false
	case len(r.Code) > 0 && !slices.Contains(r.Code, e.Code):
		return false
	case r.codeMatch != nil && !r.codeMatch.MatchString(e.Code):
		return false
	case r.message != nil && !r.message.MatchString(e.Message):
		return false
	}
	for _, name := range r.HeaderAbsent {
		if h != nil && h.Get(name) != "" {
			return false
		}
	}
	return true
}
