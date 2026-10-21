// Package sse reads text/event-stream bodies; provider adapters and the A2A client share it so every stream is framed the same way.
package sse

import (
	"bufio"
	"bytes"
	"errors"
	"io"
)

// MaxLine bounds one line and the data of one event, so a broken or hostile server cannot make the reader buffer without limit.
const MaxLine = 32 << 20

var (
	// ErrLineTooLong reports a line longer than MaxLine.
	ErrLineTooLong = errors.New("sse: line exceeds 32MB")
	// ErrEventTooLong reports an event whose data lines add up to more than MaxLine.
	ErrEventTooLong = errors.New("sse: event exceeds 32MB")
)

type Event struct {
	Name string // the event field; "" when the server sent none
	Data []byte // data lines joined with "\n"
	ID   string
}

// Done reports the OpenAI-style terminator data: [DONE].
func (e Event) Done() bool { return string(e.Data) == "[DONE]" }

type Reader struct {
	br      *bufio.Reader
	started bool
	afterCR bool // the last line ended with \r, so a \n first in the next one completes that terminator
}

func NewReader(r io.Reader) *Reader {
	return &Reader{br: bufio.NewReaderSize(r, 64<<10)}
}

// Next returns the next event, or io.EOF when the stream ends; an event cut off by the end of the stream is dropped.
func (r *Reader) Next() (Event, error) {
	var ev Event
	var data bytes.Buffer
	hasData := false
	for {
		line, err := r.line()
		if err != nil {
			return Event{}, err
		}
		if len(line) == 0 {
			if !hasData {
				ev = Event{}
				continue
			}
			ev.Data = data.Bytes()
			return ev, nil
		}
		if line[0] == ':' {
			continue
		}
		field, value, _ := bytes.Cut(line, []byte{':'})
		value = bytes.TrimPrefix(value, []byte{' '})
		switch string(field) {
		case "event":
			ev.Name = string(value)
		case "data":
			if data.Len()+len(value)+1 > MaxLine {
				return Event{}, ErrEventTooLong
			}
			if hasData {
				data.WriteByte('\n')
			}
			data.Write(value)
			hasData = true
		case "id":
			ev.ID = string(value)
		}
	}
}

// line returns one line without its terminator, which may be \r\n, \n or \r as the format allows; it returns at the terminator rather than waiting for more bytes.
func (r *Reader) line() ([]byte, error) {
	if r.afterCR {
		r.afterCR = false
		if b, err := r.br.Peek(1); err == nil && b[0] == '\n' {
			_, _ = r.br.Discard(1)
		}
	}
	var acc []byte
	for {
		if r.br.Buffered() == 0 {
			if _, err := r.br.Peek(1); err != nil {
				if errors.Is(err, io.EOF) {
					return nil, io.EOF // a line cut off by the end of the stream is incomplete, like the event it belongs to
				}
				return nil, err
			}
		}
		buf, _ := r.br.Peek(r.br.Buffered())
		i := bytes.IndexAny(buf, "\r\n")
		end := len(buf)
		if i >= 0 {
			end = i
		}
		if len(acc)+end > MaxLine {
			return nil, ErrLineTooLong
		}
		acc = append(acc, buf[:end]...)
		if i < 0 {
			_, _ = r.br.Discard(len(buf))
			continue
		}
		r.afterCR = buf[i] == '\r'
		_, _ = r.br.Discard(i + 1)
		if !r.started {
			r.started = true
			acc = bytes.TrimPrefix(acc, []byte("\xef\xbb\xbf"))
		}
		return acc, nil
	}
}
