package mcp

import (
	"encoding/json"
	"slices"
	"strconv"
	"strings"

	"github.com/estonshi/aperture-potatos/kernel"
	"github.com/estonshi/aperture-potatos/message"
	"github.com/google/jsonschema-go/jsonschema"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// convert maps a tool result onto what the model sees, by the table in spec/ext/mcp/README.md.
func (in *install) convert(res *sdk.CallToolResult, te *toolEntry) kernel.ToolResult {
	out := kernel.ToolResult{IsError: res.IsError}
	if res.StructuredContent != nil {
		if !res.IsError && !te.outputValid(res.StructuredContent) {
			return errResult(text("bad_output", "tool", te.name))
		}
		out.Structured = res.StructuredContent
	}
	for _, c := range res.Content {
		if b := block(c); b != nil {
			out.Content = append(out.Content, media(b, in.e.o.MaxMediaBytes))
		}
	}
	if len(out.Content) == 0 && res.StructuredContent != nil {
		b, _ := json.Marshal(res.StructuredContent)
		out.Content = message.Content{&message.TextBlock{Text: string(b)}}
	}
	out.Content = truncate(out.Content, in.e.o.MaxResultChars)
	return out
}

// block is one content item for the model, or nil for one meant for the user alone.
func block(c sdk.Content) message.Block {
	switch v := c.(type) {
	case *sdk.TextContent:
		if userOnly(v.Annotations) {
			return nil
		}
		return &message.TextBlock{Text: v.Text}
	case *sdk.ImageContent:
		if userOnly(v.Annotations) {
			return nil
		}
		return &message.ImageBlock{MediaType: v.MIMEType, Data: v.Data}
	case *sdk.AudioContent:
		if userOnly(v.Annotations) {
			return nil
		}
		return &message.TextBlock{Text: text("audio_omitted", "mime", v.MIMEType)}
	case *sdk.ResourceLink:
		if userOnly(v.Annotations) {
			return nil
		}
		t := text("resource_link", "name", v.Name, "uri", v.URI)
		if v.MIMEType != "" {
			t += " (" + v.MIMEType + ")"
		}
		if v.Description != "" {
			t += "\n" + v.Description
		}
		return &message.TextBlock{Text: t}
	case *sdk.EmbeddedResource:
		if userOnly(v.Annotations) || v.Resource == nil {
			return nil
		}
		r := v.Resource
		switch {
		case r.Blob == nil:
			return &message.TextBlock{Text: r.Text}
		case strings.HasPrefix(r.MIMEType, "image/"):
			return &message.ImageBlock{MediaType: r.MIMEType, Data: r.Blob}
		case r.MIMEType == "application/pdf":
			return &message.DocumentBlock{MediaType: r.MIMEType, Data: r.Blob, Filename: lastSegment(r.URI)}
		}
		return &message.TextBlock{Text: text("blob_omitted", "uri", r.URI, "mime", r.MIMEType)}
	}
	return nil
}

// imageTypes are the image types every adapter's API accepts; any other would fail every later request once committed.
var imageTypes = []string{"image/png", "image/jpeg", "image/gif", "image/webp"}

// media keeps an image or document only when the APIs accept its type and size, since one they refuse stays in the history and fails every later request, the compaction summary's too.
func media(b message.Block, max int) message.Block {
	var mime string
	var size int
	switch v := b.(type) {
	case *message.ImageBlock:
		mime, size = v.MediaType, len(v.Data)
		if !slices.Contains(imageTypes, strings.ToLower(mime)) {
			return &message.TextBlock{Text: text("media_unsupported", "mime", mime)}
		}
	case *message.DocumentBlock:
		mime, size = v.MediaType, len(v.Data)
	default:
		return b
	}
	if size > max {
		return &message.TextBlock{Text: text("media_too_large", "mime", mime, "bytes", strconv.Itoa(size), "limit", strconv.Itoa(max))}
	}
	return b
}

func userOnly(a *sdk.Annotations) bool {
	return a != nil && len(a.Audience) > 0 && !slices.Contains(a.Audience, sdk.Role("assistant"))
}

func lastSegment(uri string) string {
	if i := strings.LastIndexByte(uri, '/'); i >= 0 {
		return uri[i+1:]
	}
	return uri
}

// truncate keeps at most max characters of text across the blocks and says how much was cut.
func truncate(content message.Content, max int) message.Content {
	total := 0
	for _, b := range content {
		if t, ok := b.(*message.TextBlock); ok {
			total += len([]rune(t.Text))
		}
	}
	if total <= max {
		return content
	}
	out := make(message.Content, 0, len(content)+1)
	left := max
	for _, b := range content {
		t, ok := b.(*message.TextBlock)
		if !ok {
			out = append(out, b)
			continue
		}
		r := []rune(t.Text)
		if left <= 0 {
			continue
		}
		if len(r) > left {
			r = r[:left]
		}
		left -= len(r)
		out = append(out, &message.TextBlock{Text: string(r)})
	}
	return append(out, &message.TextBlock{Text: text("truncated", "shown", strconv.Itoa(max), "total", strconv.Itoa(total))})
}

// outputValid checks structured content against the tool's output schema; a schema the validator cannot read checks nothing.
func (te *toolEntry) outputValid(v any) bool {
	te.compile()
	if te.outCheck == nil {
		return true
	}
	b, err := json.Marshal(v)
	if err != nil {
		return false
	}
	var inst any
	if json.Unmarshal(b, &inst) != nil {
		return false
	}
	return te.outCheck.Validate(inst) == nil
}

// argsError is why arguments do not fit the tool's input schema, or "" when they do or the schema cannot be read.
func (te *toolEntry) argsError(args json.RawMessage) string {
	te.compile()
	if te.inSchema == nil {
		return ""
	}
	var inst any
	if err := json.Unmarshal(args, &inst); err != nil {
		return err.Error()
	}
	if err := te.inSchema.Validate(inst); err != nil {
		return err.Error()
	}
	return ""
}

func (te *toolEntry) compile() {
	te.once.Do(func() {
		te.inSchema = resolve(te.input)
		te.outCheck = resolve(te.output)
	})
}

func resolve(raw json.RawMessage) *jsonschema.Resolved {
	if len(raw) == 0 {
		return nil
	}
	var s jsonschema.Schema
	if json.Unmarshal(raw, &s) != nil {
		return nil
	}
	r, err := s.Resolve(nil)
	if err != nil {
		return nil
	}
	return r
}
