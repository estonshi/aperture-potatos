package conformance

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/estonshi/aperture-potatos/conformance/compare"
	"github.com/estonshi/aperture-potatos/provider"
	"github.com/estonshi/aperture-potatos/provider/anthropic"
	"github.com/estonshi/aperture-potatos/provider/openaichat"
	"github.com/estonshi/aperture-potatos/provider/openairesponses"
	"github.com/estonshi/aperture-potatos/provider/providertest"
)

var update = flag.Bool("update", false, "rewrite expected files from the Go reference implementation")

var factories = map[provider.API]Factory{
	provider.APIAnthropicMessages: func(d provider.Doer) provider.Provider {
		return anthropic.New(anthropic.Config{APIKey: "test-key", Doer: d})
	},
	provider.APIOpenAIChat: func(d provider.Doer) provider.Provider {
		return openaichat.New(openaichat.Config{APIKey: "test-key", Doer: d})
	},
	provider.APIOpenAIResponses: func(d provider.Doer) provider.Provider {
		return openairesponses.New(openairesponses.Config{APIKey: "test-key", Doer: d})
	},
}

// chunkSeeds re-run every decode case with random read sizes; the result must not depend on how bytes arrive.
var chunkSeeds = []uint64{1, 2, 3, 5, 8, 13, 21, 34, 55, 89}

func apiDirs(t *testing.T, layer string) map[provider.API]string {
	t.Helper()
	out := map[provider.API]string{}
	for api := range factories {
		dir := filepath.Join(layer, string(api))
		if _, err := os.Stat(dir); err == nil {
			out[api] = dir
		}
	}
	if len(out) == 0 {
		t.Fatalf("no %s fixtures", layer)
	}
	return out
}

func TestProviderDecode(t *testing.T) {
	for api, dir := range apiDirs(t, "provider") {
		cases, err := Cases(dir)
		if err != nil {
			t.Fatal(err)
		}
		for _, c := range cases {
			t.Run(string(api)+"/"+filepath.Base(c), func(t *testing.T) {
				res, err := RunDecode(context.Background(), c, factories[api], 0)
				if err != nil {
					t.Fatal(err)
				}
				stream, err := compare.JSONL(res.Stream)
				if err != nil {
					t.Fatal(err)
				}
				err = Verify(c, *update, func(out string) error {
					if err := os.WriteFile(filepath.Join(out, "actual.stream.jsonl"), stream, 0o644); err != nil || res.Message == nil {
						return err
					}
					return os.WriteFile(filepath.Join(out, "actual.message.json"), res.Message, 0o644)
				})
				if err != nil {
					t.Fatal(err)
				}
				for _, seed := range chunkSeeds {
					again, err := RunDecode(context.Background(), c, factories[api], seed)
					if err != nil {
						t.Fatal(err)
					}
					b, _ := compare.JSONL(again.Stream)
					if !bytes.Equal(b, stream) {
						t.Fatalf("seed %d: chunking changed the stream\n got %s\nwant %s", seed, b, stream)
					}
				}
			})
		}
	}
}

// okReply is the canned stream render cases answer with; its content is irrelevant to them.
var okReply = map[provider.API]providertest.Response{
	provider.APIAnthropicMessages: {Body: []byte("event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_r\",\"usage\":{\"input_tokens\":1,\"output_tokens\":1}}}\n\nevent: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":1}}\n\nevent: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")},
	provider.APIOpenAIChat:        {Body: []byte("data: {\"id\":\"chatcmpl-r\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")},
	provider.APIOpenAIResponses:   {Body: []byte("data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_r\"}}\n\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_r\",\"usage\":{\"input_tokens\":1,\"output_tokens\":1}}}\n\n")},
}

func TestProviderRender(t *testing.T) {
	for api, dir := range apiDirs(t, "render") {
		cases, err := Cases(dir)
		if err != nil {
			t.Fatal(err)
		}
		for _, c := range cases {
			t.Run(string(api)+"/"+filepath.Base(c), func(t *testing.T) {
				res, err := RunRender(context.Background(), c, factories[api], okReply[api])
				if err != nil {
					t.Fatal(err)
				}
				err = Verify(c, *update, func(out string) error {
					notices, err := compare.JSONL(res.Notices)
					if err != nil {
						return err
					}
					files := map[string][]byte{"actual.body.json": res.Body, "actual.error.json": res.Error, "actual.notices.jsonl": notices}
					if res.Body != nil {
						files["actual.headers.json"], _ = json.Marshal(res.Headers)
					}
					for name, b := range files {
						if b == nil {
							continue
						}
						if err := os.WriteFile(filepath.Join(out, name), b, 0o644); err != nil {
							return err
						}
					}
					return nil
				})
				if err != nil {
					t.Fatal(err)
				}
			})
		}
	}
}

func TestProviderErrors(t *testing.T) {
	for api, dir := range apiDirs(t, "errors") {
		files, err := filepath.Glob(filepath.Join(dir, "*.json"))
		if err != nil || len(files) == 0 {
			t.Fatalf("no error cases in %s", dir)
		}
		for _, file := range files {
			t.Run(string(api)+"/"+strings.TrimSuffix(filepath.Base(file), ".json"), func(t *testing.T) {
				var c ErrorCase
				if err := readJSON(file, &c); err != nil {
					t.Fatal(err)
				}
				got, err := RunError(context.Background(), c, "claude-test", factories[api])
				if err != nil {
					t.Fatal(err)
				}
				err = Verify(file, *update, func(out string) error {
					return os.WriteFile(filepath.Join(out, "actual.json"), got, 0o644)
				})
				if err != nil {
					t.Fatal(err)
				}
			})
		}
	}
}
