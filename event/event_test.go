package event

import (
	"bytes"
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"github.com/estonshi/aperture-potatos/message"
)

func TestKernelPayloadsComplete(t *testing.T) {
	seen := map[Type]bool{}
	for _, c := range kernelPayloads {
		p := c.New()
		if p.EventType() != c.Type || seen[c.Type] {
			t.Fatalf("constructor %q builds %q or is listed twice", c.Type, p.EventType())
		}
		seen[c.Type] = true
		if reflect.TypeOf(p).Kind() != reflect.Pointer {
			t.Fatalf("%s: payloads are pointers so a type switch matches what the Decoder returns", c.Type)
		}
	}
	bulk := map[Type]bool{TypeMessageDelta: true, TypeReasoningDelta: true, TypeToolCallDelta: true, TypeToolProgress: true, TypeTaskProgress: true, TypeCommandOutput: true, TypeProviderRaw: true}
	for _, c := range kernelPayloads {
		b, ok := c.New().(Bulk)
		if (ok && b.Bulk()) != bulk[c.Type] {
			t.Fatalf("%s: bulk=%v, want %v", c.Type, ok, bulk[c.Type])
		}
	}
}

func TestDecodeRoundTrip(t *testing.T) {
	pct := 0.5
	evs := []Event{
		{Type: TypeMessageDelta, Seq: 3, SessionID: "ses_1", Agent: "Tater", TurnID: "turn_1", Step: 1, Data: &MessageDelta{MessageID: "msg_1", Channel: "text", Text: "hi"}},
		{Type: TypeMessageCompleted, Seq: 4, SessionID: "ses_1", Agent: "Tater", Data: &MessageCompleted{Message: message.UserText("hi")}},
		{Type: TypeTaskProgress, Seq: 5, SessionID: "ses_2", Agent: "Tater", Path: []Hop{{Kind: "task", ID: "task_1", Name: "subagent", SessionID: "ses_1", Seq: 9}}, Data: &TaskProgress{TaskID: "task_1", Percent: &pct}},
	}
	d := NewDecoder()
	for _, ev := range evs {
		ev.Time = time.Date(2026, 9, 27, 8, 0, 0, 0, time.UTC)
		b, err := json.Marshal(ev)
		if err != nil {
			t.Fatal(err)
		}
		got, err := d.Decode(b)
		if err != nil {
			t.Fatal(err)
		}
		b2, _ := json.Marshal(got)
		if !bytes.Equal(b, b2) {
			t.Fatalf("round trip\n in %s\nout %s", b, b2)
		}
	}
}

type custom struct {
	N int `json:"n"`
}

func (*custom) EventType() Type { return "x_acme.ping" }

func TestUnknownAndExtensionTypes(t *testing.T) {
	line := []byte(`{"type":"x_acme.ping","seq":1,"time":"2026-09-27T08:00:00Z","session_id":"s","agent":"a","data":{"n":2,"later":true}}`)
	ev, err := NewDecoder().Decode(line)
	if err != nil {
		t.Fatal(err)
	}
	u, ok := ev.Data.(*Unknown)
	if !ok || u.TypeName != "x_acme.ping" {
		t.Fatalf("want *Unknown, got %#v", ev.Data)
	}
	if b, _ := json.Marshal(ev); !bytes.Equal(b, line) {
		t.Fatalf("unknown payload must be written back verbatim\n%s", b)
	}
	ev, err = NewDecoder(Constructor{Type: "x_acme.ping", New: func() Payload { return new(custom) }}).Decode(line)
	if err != nil || ev.Data.(*custom).N != 2 {
		t.Fatalf("extension payload: %#v %v", ev.Data, err)
	}
	shadow := Constructor{Type: TypeNotice, New: func() Payload { return new(custom) }}
	ev, _ = NewDecoder(shadow).Decode([]byte(`{"type":"notice","seq":1,"time":"2026-09-27T08:00:00Z","session_id":"s","agent":"a","data":{"source":"x","text":"y"}}`))
	if _, ok := ev.Data.(*Notice); !ok {
		t.Fatalf("an extension must not replace a kernel payload, got %#v", ev.Data)
	}
}

func TestDecodeErrors(t *testing.T) {
	for _, b := range []string{`{}`, `[]`, `{"type":"notice","data":{"text":1}}`} {
		if _, err := NewDecoder().Decode([]byte(b)); err == nil {
			t.Fatalf("want error for %s", b)
		}
	}
}

func FuzzDecode(f *testing.F) {
	f.Add([]byte(`{"type":"message.delta","seq":1,"time":"2026-09-27T08:00:00Z","session_id":"s","agent":"a","data":{"message_id":"m","block_index":0,"channel":"text","text":"x"}}`))
	d := NewDecoder()
	f.Fuzz(func(t *testing.T, b []byte) {
		ev, err := d.Decode(b)
		if err == nil {
			_, _ = json.Marshal(ev)
		}
	})
}
