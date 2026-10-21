package message

import (
	"bytes"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

var sampleLines = []string{
	`{"v":1,"id":"msg_01","session_id":"ses_01","seq":1,"epoch":1,"role":"system","origin":{"kind":"system"},"content":[{"type":"text","text":"<identity>\nYour name is Tater.\n</identity>"}],"boundary":{"reason":"new","session":{"agent":"Tater","spec_version":"0.1.0"}},"tools":[{"name":"get_weather","description":"Get the current weather for a city.","input_schema":{"type":"object","properties":{"city":{"type":"string"}},"required":["city"],"additionalProperties":false}}],"ts":"2026-09-27T08:00:00Z"}`,
	`{"v":1,"id":"msg_02","session_id":"ses_01","seq":2,"epoch":1,"role":"user","origin":{"kind":"human"},"content":[{"type":"text","text":"What's the weather in Osaka?"}],"turn_id":"turn_01","ts":"2026-09-27T08:00:05Z"}`,
	`{"v":1,"id":"msg_03","session_id":"ses_01","seq":3,"epoch":1,"role":"assistant","origin":{"kind":"model"},"content":[{"type":"reasoning","kind":"summary","text":"Call get_weather.","native":{"api":"anthropic.messages","raw":{"type":"thinking","thinking":"Call get_weather.","signature":"EqQB"}}},{"type":"tool_call","id":"toolu_01","name":"get_weather","input":{"city":"Osaka"}}],"producer":{"api":"anthropic.messages","endpoint":"api.anthropic.com","model":"claude-opus-5-5","response_id":"msg_01Rk7Qz","request_id":"req_01Hx3Vb","headers":{"x-litellm-call-id":"c1"}},"usage":{"input_tokens":1210,"cache_read_tokens":0,"cache_write_tokens":1180,"output_tokens":58,"reasoning_tokens":21},"stop_reason":"tool_use","turn_id":"turn_01","ts":"2026-09-27T08:00:07Z"}`,
	`{"v":1,"id":"msg_04","session_id":"ses_01","seq":4,"epoch":1,"role":"tool","origin":{"kind":"tool"},"content":[{"type":"tool_result","call_id":"toolu_01","name":"get_weather","content":[{"type":"text","text":"Sunny, 24°C"},{"type":"image","media_type":"image/png","data":"iVBORw0KGgo="}],"outcome":"success","pin":{"key":"weather","content":"Osaka: sunny"}}],"tasks":[{"id":"task_01","kind":"tool_exec","status":"running","attempt":1,"spec":{"kind":"tool_exec","title":"t","input":{}}}],"turn_id":"turn_01","ts":"2026-09-27T08:00:08Z"}`,
	`{"v":1,"id":"msg_05","session_id":"ses_01","seq":5,"epoch":1,"role":"user","origin":{"kind":"epoch_head"},"content":[{"type":"reminder","source":"pin:weather","text":"Osaka: sunny"},{"type":"document","media_type":"application/pdf","url":"https://example.com/a.pdf","filename":"a.pdf"},{"type":"refusal","text":"no"},{"type":"opaque","kind":"anthropic.messages:server_tool_use","native":{"api":"anthropic.messages","raw":{"type":"server_tool_use"}}}],"copy_of":"msg_02","batch":"b_01","batch_end":true,"ts":"2026-09-27T08:00:09.123Z"}`,
}

func canonical(t *testing.T, b []byte) any {
	t.Helper()
	d := json.NewDecoder(bytes.NewReader(b))
	d.UseNumber()
	var v any
	if err := d.Decode(&v); err != nil {
		t.Fatalf("decode %s: %v", b, err)
	}
	return v
}

func roundTrip(t *testing.T, line string) (Message, []byte) {
	t.Helper()
	var m Message
	if err := json.Unmarshal([]byte(line), &m); err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, line)
	}
	out, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !reflect.DeepEqual(canonical(t, []byte(line)), canonical(t, out)) {
		t.Fatalf("round trip changed the line\n in: %s\nout: %s", line, out)
	}
	return m, out
}

func TestRoundTripSampleLines(t *testing.T) {
	for _, line := range sampleLines {
		roundTrip(t, line)
	}
}

func TestUnknownFieldsKept(t *testing.T) {
	line := `{"v":1,"id":"m","session_id":"s","seq":1,"epoch":1,"role":"user","origin":{"kind":"human"},"content":[{"type":"text","text":"hi","lang":"en"}],"ts":"2026-09-27T08:00:00Z","future":{"a":[1,2]},"Seq":9}`
	m, _ := roundTrip(t, line)
	if m.Seq != 1 {
		t.Fatalf("a key that differs only in case must not fill the field, got seq %d", m.Seq)
	}
	if tb := m.Content[0].(*TextBlock); tb.Text != "hi" || string(tb.extra["lang"]) != `"en"` {
		t.Fatalf("block extra lost: %+v", tb)
	}
}

func TestUnknownBlockTypeOpaque(t *testing.T) {
	line := `{"v":1,"id":"m","session_id":"s","seq":1,"epoch":1,"role":"assistant","origin":{"kind":"model"},"content":[{"type":"video","url":"https://x/v.mp4","text":"a clip"}],"ts":"2026-09-27T08:00:00Z"}`
	m, out := roundTrip(t, line)
	o, ok := m.Content[0].(*OpaqueBlock)
	if !ok || o.Kind != "video" || o.Text != "a clip" {
		t.Fatalf("unknown block should decode to *OpaqueBlock with its type as kind: %#v", m.Content[0])
	}
	if !strings.Contains(string(out), `{"type":"video","url":"https://x/v.mp4","text":"a clip"}`) {
		t.Fatalf("unknown block not written back verbatim: %s", out)
	}
}

func TestCaseMismatchedBlockKeyStaysUnknown(t *testing.T) {
	var b TextBlock
	if err := json.Unmarshal([]byte(`{"type":"text","Text":"x"}`), &b); err != nil {
		t.Fatal(err)
	}
	if b.Text != "" || string(b.extra["Text"]) != `"x"` {
		t.Fatalf("got %+v", b)
	}
}

func TestBlockTypeMismatchRejected(t *testing.T) {
	var b TextBlock
	if err := json.Unmarshal([]byte(`{"type":"image","text":"x"}`), &b); err == nil {
		t.Fatal("want an error for a mismatched type")
	}
	var c Content
	if err := json.Unmarshal([]byte(`[{"text":"no type"}]`), &c); err == nil {
		t.Fatal("want an error for a block without type")
	}
}

func TestMarshalOmitsEmptyOptionals(t *testing.T) {
	out, err := json.Marshal(Message{V: Version, ID: "m", SessionID: "s", Seq: 1, Epoch: 1, Role: RoleAssistant, Origin: Origin{Kind: OriginModel}, Content: Content{&ToolCallBlock{ID: "c", Name: "n"}}})
	if err != nil {
		t.Fatal(err)
	}
	want := `{"v":1,"id":"m","session_id":"s","seq":1,"epoch":1,"role":"assistant","origin":{"kind":"model"},"content":[{"type":"tool_call","id":"c","name":"n","input":{}}],"ts":"0001-01-01T00:00:00Z"}`
	if string(out) != want {
		t.Fatalf("\n got %s\nwant %s", out, want)
	}
	out, _ = json.Marshal(Message{})
	if !strings.Contains(string(out), `"content":[]`) {
		t.Fatalf("nil content must encode as an empty array: %s", out)
	}
}

func TestHelpers(t *testing.T) {
	m := Message{Content: Content{&TextBlock{Text: "a"}, &ToolCallBlock{ID: "1", Name: "x"}, &TextBlock{Text: "b"}}}
	if m.Text() != "ab" || len(m.ToolCalls()) != 1 || m.Empty() {
		t.Fatalf("text %q calls %d empty %v", m.Text(), len(m.ToolCalls()), m.Empty())
	}
	if !(Message{Content: Content{&TextBlock{}}}).Empty() {
		t.Fatal("a message with only empty text blocks is empty")
	}
	if (Message{Content: Content{&ReasoningBlock{Kind: ReasoningRedacted}}}).Empty() {
		t.Fatal("a redacted reasoning block has no text but is not empty")
	}
	if u := UserText("hi"); u.Role != RoleUser || u.Origin.Kind != OriginHuman || u.Text() != "hi" {
		t.Fatalf("UserText: %+v", u)
	}
	if a := AssistantText("ok"); a.Producer == nil || a.Producer.API != "synthetic" {
		t.Fatalf("AssistantText: %+v", a)
	}
}

func FuzzUnmarshalLine(f *testing.F) {
	for _, l := range sampleLines {
		f.Add([]byte(l))
	}
	f.Fuzz(func(t *testing.T, b []byte) {
		var m Message
		if json.Unmarshal(b, &m) != nil {
			return
		}
		out, err := json.Marshal(m)
		if err != nil {
			return
		}
		var m2 Message
		if err := json.Unmarshal(out, &m2); err != nil {
			t.Fatalf("re-decode of own output failed: %v\n%s", err, out)
		}
		out2, err := json.Marshal(m2)
		if err != nil || !bytes.Equal(out, out2) {
			t.Fatalf("encoding not stable\n%s\n%s", out, out2)
		}
	})
}
