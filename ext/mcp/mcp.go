// Package mcp connects a potatOS agent to Model Context Protocol servers and offers their tools to the model.
package mcp

import (
	"context"
	"fmt"
	"net/http"
	"os/exec"
	"regexp"
	"slices"
	"sync"
	"time"

	"github.com/estonshi/aperture-potatos/kernel"
	"github.com/modelcontextprotocol/go-sdk/auth"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// Name is the extension's name, and the source of the reminders it queues.
const Name = "mcp"

// Mode says how the model reaches the servers' tools.
type Mode string

const (
	Auto   Mode = ""       // Direct until the definitions outgrow DiscoveryRatio of the context window, then Search, or Native with NativeSearch
	Direct Mode = "direct" // one tool definition per server tool
	Search Mode = "search" // mcp_search_tools and mcp_call_tool, with the tools listed in the catalog section
	Native Mode = "native" // deferred tool definitions and the provider's own tool search, which loads what it finds
)

const (
	defaultMaxResultChars = 50000
	defaultMaxMediaBytes  = 5 << 20 // Anthropic's limit for one image, the strictest of the APIs
	defaultDiscoveryRatio = 0.02
	defaultCatalogRatio   = 0.05
	defaultContextWindow  = 200000
	defaultConnectTimeout = 30 * time.Second
)

type Options struct {
	Servers        []Server
	MaxResultChars int           // text a result may carry to the model; 0 means 50000
	MaxMediaBytes  int           // one image or PDF a result may carry to the model; 0 means 5 MiB
	ConnectTimeout time.Duration // how long Install waits for the first connection of each server; 0 means 30s
	Discovery      Mode
	NativeSearch   bool       // Auto switches to Native instead of Search; a model that cannot search gets every definition, as in Direct
	DiscoveryRatio float64    // Auto switches above this share of the context window; 0 means 0.02
	CatalogRatio   float64    // the catalog section's budget in Search mode, past which summaries give way to names; 0 means 0.05
	ContextWindow  int        // the window Auto measures against at agent scope; 0 means 200000
	Search         SearchFunc // replaces the built-in keyword search
}

// ToolInfo is one server tool as discovery sees it.
type ToolInfo struct {
	Name        string // the name the model uses, mcp__<server>__<tool>
	Server      string
	Title       string
	Description string
}

// SearchFunc returns the tools of catalog that match query, best first.
type SearchFunc func(ctx context.Context, query string, catalog []ToolInfo) []ToolInfo

type Server struct {
	Name    string           // the host's key, ^[a-z0-9][a-z0-9-]{0,31}$; it goes into tool names
	Command func() *exec.Cmd // stdio; called for each connection, since an exec.Cmd runs once; credentials go in the command's Env
	URL     string           // Streamable HTTP; exactly one of Command and URL

	Headers      map[string]string // sent on every HTTP request
	HTTPClient   *http.Client      // reaches the MCP server
	OAuth        *OAuth
	OAuthHandler auth.OAuthHandler // the SDK's interface, for a host with its own flow; excludes OAuth

	Tools, DenyTools []string      // by MCP name; an empty Tools allows every tool
	Trusted          bool          // trust the server's annotations, which only lets its read-only tools run in parallel
	Timeout          time.Duration // one call; 0 means Limits.ToolTimeout
	Background       bool          // the tools may run as background tasks
	Prompts          bool          // register the server's prompts as slash commands, mcp__<server>__<prompt>

	ProtocolVersion string // one of the SDK's mcp.SupportedProtocolVersions(), offered first; empty is the newest; an older one skips server/discover for servers that mishandle it
}

var serverName = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,31}$`)

// Extension connects its servers wherever it is installed; one value may be installed in several sessions.
type Extension struct {
	o        Options
	mu       sync.Mutex
	installs []*install
}

// New returns the extension; kernel.Use shares its connections across the agent's sessions, kernel.SessionUse gives one session its own.
func New(o Options) *Extension {
	if o.MaxResultChars <= 0 {
		o.MaxResultChars = defaultMaxResultChars
	}
	if o.MaxMediaBytes <= 0 {
		o.MaxMediaBytes = defaultMaxMediaBytes
	}
	if o.ConnectTimeout <= 0 {
		o.ConnectTimeout = defaultConnectTimeout
	}
	if o.DiscoveryRatio <= 0 {
		o.DiscoveryRatio = defaultDiscoveryRatio
	}
	if o.CatalogRatio <= 0 {
		o.CatalogRatio = defaultCatalogRatio
	}
	if o.ContextWindow <= 0 {
		o.ContextWindow = defaultContextWindow
	}
	return &Extension{o: o}
}

func (e *Extension) Name() string { return Name }

func (e *Extension) Install(r kernel.Registrar) error {
	if err := validate(e.o.Servers); err != nil {
		return err
	}
	in := newInstall(e, r)
	if err := in.register(); err != nil {
		return err
	}
	e.mu.Lock()
	e.installs = append(e.installs, in)
	e.mu.Unlock()
	in.start()
	return nil
}

// Close ends every connection this extension opened; session-scope ones also end with their session.
func (e *Extension) Close(ctx context.Context) error {
	e.mu.Lock()
	ins := e.installs
	e.installs = nil
	e.mu.Unlock()
	for _, in := range ins {
		in.close()
	}
	return nil
}

func validate(servers []Server) error {
	seen := map[string]bool{}
	for _, s := range servers {
		switch {
		case !serverName.MatchString(s.Name):
			return fmt.Errorf("%w: mcp server %q", kernel.ErrInvalidName, s.Name)
		case seen[s.Name]:
			return fmt.Errorf("%w: mcp server %q", kernel.ErrDuplicate, s.Name)
		case (s.Command == nil) == (s.URL == ""):
			return fmt.Errorf("mcp server %q: set exactly one of Command and URL", s.Name)
		case s.OAuth != nil && s.OAuthHandler != nil:
			return fmt.Errorf("mcp server %q: set at most one of OAuth and OAuthHandler", s.Name)
		case (s.OAuth != nil || s.OAuthHandler != nil) && s.URL == "":
			return fmt.Errorf("mcp server %q: OAuth needs an HTTP server; a stdio server takes credentials from its command's Env", s.Name)
		case s.ProtocolVersion != "" && !slices.Contains(sdk.SupportedProtocolVersions(), s.ProtocolVersion):
			return fmt.Errorf("mcp server %q: protocol version %q is not one of %v", s.Name, s.ProtocolVersion, sdk.SupportedProtocolVersions())
		}
		if s.OAuth != nil {
			if err := s.OAuth.validate(); err != nil {
				return fmt.Errorf("mcp server %q: %w", s.Name, err)
			}
		}
		seen[s.Name] = true
	}
	return nil
}
