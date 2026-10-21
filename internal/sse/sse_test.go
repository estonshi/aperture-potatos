package sse

import (
	"errors"
	"io"
	"strings"
	"testing"
	"testing/iotest"
)

func all(t *testing.T, r io.Reader) ([]Event, error) {
	t.Helper()
	sr := NewReader(r)
	var out []Event
	for {
		ev, err := sr.Next()
		if err != nil {
			return out, err
		}
		out = append(out, ev)
	}
}

func TestFraming(t *testing.T) {
	in := "\xef\xbb\xbfevent: message_start\r\ndata: {\"a\":1}\r\n\r\n" +
		": keep-alive\n\n" +
		"data: line one\ndata:line two\nid: 7\nretry: 100\n\n" +
		"event: ping\ndata: {}\n\n" +
		"data: [DONE]\n\n" +
		"data: cut off"
	evs, err := all(t, iotest.OneByteReader(strings.NewReader(in)))
	if !errors.Is(err, io.EOF) {
		t.Fatalf("want EOF, got %v", err)
	}
	if len(evs) != 4 {
		t.Fatalf("got %d events: %+v", len(evs), evs)
	}
	if evs[0].Name != "message_start" || string(evs[0].Data) != `{"a":1}` {
		t.Fatalf("BOM and CRLF: %+v", evs[0])
	}
	if string(evs[1].Data) != "line one\nline two" || evs[1].ID != "7" || evs[1].Name != "" {
		t.Fatalf("multi-line data: %+v", evs[1])
	}
	if !evs[3].Done() || evs[2].Done() {
		t.Fatal("[DONE] detection")
	}
}

func TestEventWithoutTrailingBlankLineIsDropped(t *testing.T) {
	evs, err := all(t, strings.NewReader("data: a\n\ndata: b\n"))
	if !errors.Is(err, io.EOF) || len(evs) != 1 {
		t.Fatalf("got %v %+v", err, evs)
	}
}

func TestLongLines(t *testing.T) {
	big := strings.Repeat("x", 1<<20)
	evs, err := all(t, strings.NewReader("data: "+big+"\n\n"))
	if !errors.Is(err, io.EOF) || len(evs) != 1 || len(evs[0].Data) != len(big) {
		t.Fatalf("1MB line: %v %d", err, len(evs))
	}
	_, err = all(t, io.MultiReader(strings.NewReader("data: "), strings.NewReader(strings.Repeat("y", MaxLine)), strings.NewReader("\n\n")))
	if !errors.Is(err, ErrLineTooLong) {
		t.Fatalf("want ErrLineTooLong, got %v", err)
	}
}

func TestReadErrorPassesThrough(t *testing.T) {
	boom := errors.New("boom")
	_, err := all(t, io.MultiReader(strings.NewReader("data: a\n"), iotest.ErrReader(boom)))
	if !errors.Is(err, boom) {
		t.Fatalf("got %v", err)
	}
}

func FuzzReader(f *testing.F) {
	f.Add([]byte("event: x\ndata: y\n\n: c\r\ndata: [DONE]\n\n"))
	f.Fuzz(func(t *testing.T, b []byte) {
		r := NewReader(strings.NewReader(string(b)))
		for range 10000 {
			if _, err := r.Next(); err != nil {
				return
			}
		}
	})
}

func TestBareCR(t *testing.T) {
	evs, err := all(t, iotest.OneByteReader(strings.NewReader("event: a\rdata: one\rdata: two\r\rdata: three\r\n\r\n")))
	if !errors.Is(err, io.EOF) || len(evs) != 2 || evs[0].Name != "a" || string(evs[0].Data) != "one\ntwo" || string(evs[1].Data) != "three" {
		t.Fatalf("got %v %+v", err, evs)
	}
}

// TestCRDoesNotWait: an event ended by bare CRs arrives before the server sends anything more.
func TestCRDoesNotWait(t *testing.T) {
	pr, pw := io.Pipe()
	defer pw.Close()
	go func() { _, _ = pw.Write([]byte("data: now\r\r")) }()
	ev, err := NewReader(pr).Next()
	if err != nil || string(ev.Data) != "now" {
		t.Fatalf("got %v %+v", err, ev)
	}
}

func TestEventTooLong(t *testing.T) {
	line := "data: " + strings.Repeat("x", 1<<20) + "\n"
	_, err := all(t, strings.NewReader(strings.Repeat(line, MaxLine>>20+1)+"\n"))
	if !errors.Is(err, ErrEventTooLong) {
		t.Fatalf("want ErrEventTooLong, got %v", err)
	}
}
