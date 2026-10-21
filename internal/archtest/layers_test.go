package archtest

import (
	"fmt"
	"slices"
	"strings"
)

const module = "github.com/estonshi/aperture-potatos"

// mcpDeps are the core go.mod's only direct requirements, exempt from the standard-library-only rule for ext/mcp alone.
var mcpDeps = []string{"github.com/modelcontextprotocol/go-sdk", "github.com/google/jsonschema-go", "golang.org/x/oauth2"}

// layer names the part of the tree a package path (relative to the module root) belongs to.
type layer int

const (
	lKernel layer = iota
	lMessage
	lEvent
	lStore
	lStoreImpl // store/jsonl and any other reference store
	lProvider
	lAdapter // provider/<api> and provider/providertest
	lProviderInternal
	lInternal
	lExt
	lStd
	lSpec
	lConformance
	lTool
	lExample
	lUnknown
)

func classify(rel string) layer {
	first, rest, _ := strings.Cut(rel, "/")
	switch first {
	case "kernel":
		return lKernel
	case "message":
		return lMessage
	case "event":
		return lEvent
	case "store":
		if rest == "" {
			return lStore
		}
		return lStoreImpl
	case "provider":
		switch {
		case rest == "":
			return lProvider
		case rest == "internal" || strings.HasPrefix(rest, "internal/"):
			return lProviderInternal
		}
		return lAdapter
	case "internal":
		return lInternal
	case "ext":
		return lExt
	case "std":
		return lStd
	case "spec":
		return lSpec
	case "conformance":
		return lConformance
	case "tools":
		return lTool
	case "examples":
		return lExample
	}
	return lUnknown
}

// extension is the unit that must be deletable on its own: ext/<x>, or ext/cmd/<x>cmd for commands.
func extension(rel string) string {
	parts := strings.Split(rel, "/")
	if len(parts) >= 3 && parts[1] == "cmd" {
		return strings.Join(parts[:3], "/")
	}
	if len(parts) >= 2 {
		return strings.Join(parts[:2], "/")
	}
	return rel
}

// adapter is the provider/<api> an adapter package belongs to.
func adapter(rel string) string {
	parts := strings.Split(rel, "/")
	return strings.Join(parts[:min(2, len(parts))], "/")
}

// checkImport reports why the package at from (module-relative) may not import path, or nil when the import is allowed.
func checkImport(from, path string) error {
	if !strings.Contains(strings.SplitN(path, "/", 2)[0], ".") {
		return nil // standard library
	}
	if path != module && !strings.HasPrefix(path, module+"/") {
		if extension(from) == "ext/mcp" && slices.ContainsFunc(mcpDeps, func(m string) bool { return path == m || strings.HasPrefix(path, m+"/") }) {
			return nil
		}
		return fmt.Errorf("third-party import %s: only ext/mcp may import the MCP SDK's modules; everything else uses the standard library", path)
	}
	to := strings.TrimPrefix(strings.TrimPrefix(path, module), "/")
	f, t := classify(from), classify(to)
	deny := func(why string) error { return fmt.Errorf("%s may not import %s: %s", show(from), show(to), why) }

	switch f {
	case lMessage:
		return deny("message depends on the standard library only")
	case lEvent, lStore:
		if t != lMessage {
			return deny("event and store depend on message only")
		}
	case lStoreImpl:
		if t != lStore && t != lMessage {
			return deny("a reference store depends on store and message only")
		}
	case lProvider:
		if t != lMessage {
			return deny("provider depends on message only")
		}
	case lAdapter, lProviderInternal:
		switch {
		case t == lProvider || t == lMessage || t == lProviderInternal || t == lSpec || to == "internal/sse":
		case f == lAdapter && to == "internal/prompt": // reminders are encoded by the same escaper the kernel uses
		case t == lAdapter && adapter(to) == adapter(from):
		case t == lAdapter:
			return deny("adapters never import each other")
		default:
			return deny("the provider layer never depends on the kernel, so it can be used on its own")
		}
	case lKernel, lInternal:
		switch t {
		case lMessage, lEvent, lStore, lProvider, lInternal, lSpec:
		default:
			return deny("the kernel and internal/* depend only on L0, provider, internal/* and spec")
		}
	case lExt:
		switch {
		case t == lKernel || t == lMessage || t == lEvent || t == lStore || t == lProvider || t == lSpec:
		case t == lExt && extension(to) == extension(from) && !(from == "ext/peer" && strings.HasPrefix(to, "ext/peer/a2a")):
		case from == "ext/peer/a2a" || strings.HasPrefix(from, "ext/peer/a2a/"):
			if to != "internal/sse" {
				return deny("ext/peer/a2a may add only internal/sse to the extension rules")
			}
		case t == lExt:
			return deny("extensions never import each other; compose them by tool or task kind name")
		default:
			return deny("extensions use only the exported kernel, L0, L1 and spec")
		}
	case lStd:
		if t != lKernel && t != lExt {
			return deny("std only lists default extensions")
		}
	case lSpec:
		return deny("spec is a data leaf")
	case lConformance:
		if t == lExt || t == lStd {
			return deny("the conformance runner knows no extension")
		}
	case lTool:
		switch t {
		case lKernel, lMessage, lEvent, lStore, lProvider, lAdapter, lSpec, lConformance, lInternal, lProviderInternal:
		default:
			return deny("tools/cmd depend on the kernel, provider and the standard library")
		}
	case lExample:
	default:
		return deny("the package is outside every layer; add it to archtest")
	}
	return nil
}

func show(rel string) string {
	if rel == "kernel" {
		return "the kernel"
	}
	return rel
}
