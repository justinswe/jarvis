// Package mcpx connects Jarvis to MCP tool servers: guild-configured remote servers
// over Streamable HTTP, and the in-process built-in server carrying the Discord tools,
// so both traverse one protocol path into the agent loop.
package mcpx

import (
	"context"
	"net/http"
	"net/url"
	"time"

	"github.com/justinswe/jarvis/worker/pkg/config"
	"github.com/justinswe/jarvis/worker/pkg/genai"
	"github.com/justinswe/std/app"
	"github.com/justinswe/std/errors"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"go.uber.org/zap"
)

// defaultCallTimeout bounds each MCP connect, tool listing, and tool call.
const defaultCallTimeout = 15 * time.Second

// Config controls remote MCP access for one deployment.
type Config struct {
	// CallTimeout bounds each connect, tool listing, and tool call. Zero uses 15s.
	// It must stay well under the per-message timeout.
	CallTimeout time.Duration
	// AllowPrivateNetworks permits MCP URLs resolving to loopback, private, or
	// link-local addresses, and plain http — the self-hosted escape hatch. Leave it
	// off on shared deployments: the combined image runs unauthenticated Valkey and
	// NATS on loopback.
	AllowPrivateNetworks bool
}

// AuthSource resolves a decrypted bearer token for one guild's MCP server at dial
// time. Implemented by *store.Store; the token never travels anywhere else.
type AuthSource interface {
	MCPServerAuth(ctx context.Context, guildID, name string) (string, error)
}

// Connector dials remote MCP servers on behalf of guilds.
type Connector struct {
	cfg    Config
	auth   AuthSource
	client *http.Client
}

// New creates a Connector whose outbound HTTP client enforces the network policy.
func New(cfg Config, auth AuthSource) *Connector {
	return &Connector{cfg: cfg, auth: auth, client: newHTTPClient(cfg.AllowPrivateNetworks)}
}

func (c *Connector) timeout() time.Duration {
	if c.cfg.CallTimeout > 0 {
		return c.cfg.CallTimeout
	}
	return defaultCallTimeout
}

// GuildTools connects to every enabled MCP server attached to one guild, lists their
// tools, and returns namespaced FunctionTool adapters plus a release func closing all
// sessions. A failed or misbehaving server is logged and skipped — it never fails the
// message. Sessions live for one message only, so no cross-request or cross-guild
// state exists to leak.
//
// ponytail: per-message connect+list; add a per-guild pooled session with idle
// eviction when MCP latency measurably matters.
func (c *Connector) GuildTools(ctx context.Context, guildID string, servers []config.MCPServer) ([]genai.FunctionTool, func()) {
	var tools []genai.FunctionTool
	var sessions []*mcp.ClientSession
	for _, server := range servers {
		if !server.Enabled {
			continue
		}
		session, serverTools, err := c.connect(ctx, guildID, server)
		if err != nil {
			app.L().Warn("MCP server unavailable; skipping its tools",
				zap.String("guild_id", guildID), zap.String("mcp_server", server.Name), zap.Error(err))
			continue
		}
		sessions = append(sessions, session)
		tools = append(tools, serverTools...)
	}
	release := func() {
		for _, session := range sessions {
			_ = session.Close()
		}
	}
	return dedupeTools(tools), release
}

// connect dials one server, verifies the endpoint policy, and adapts its tools.
func (c *Connector) connect(ctx context.Context, guildID string, server config.MCPServer) (*mcp.ClientSession, []genai.FunctionTool, error) {
	if err := c.allowedEndpoint(server.URL); err != nil {
		return nil, nil, err
	}
	client := c.client
	if server.HasAuth {
		if c.auth == nil {
			return nil, nil, errors.New("no credential source is configured for an authenticated MCP server")
		}
		token, err := c.auth.MCPServerAuth(ctx, guildID, server.Name)
		if err != nil {
			return nil, nil, err
		}
		if token != "" {
			client = &http.Client{
				Transport: bearerTransport{base: c.client.Transport, token: token},
				// The token is attached per request inside the RoundTripper, which is
				// below net/http's cross-host redirect stripping — so without this the
				// guild's credential would follow a 302 to any host the server names.
				CheckRedirect: sameHostRedirect,
			}
		}
	}
	connectCtx, cancel := context.WithTimeout(ctx, c.timeout())
	defer cancel()
	session, err := mcp.NewClient(&mcp.Implementation{Name: "jarvis", Version: "1"}, nil).
		Connect(connectCtx, &mcp.StreamableClientTransport{Endpoint: server.URL, HTTPClient: client}, nil)
	if err != nil {
		return nil, nil, errors.Wrap(err, "connect MCP server")
	}
	tools, err := c.sessionTools(connectCtx, session, server.Name)
	if err != nil {
		_ = session.Close()
		return nil, nil, err
	}
	return session, tools, nil
}

// allowedEndpoint enforces the structural half of the outbound policy for every dial,
// covering rows written by the external accounts API as well as Jarvis's own tools.
// The resolved-IP half lives in the HTTP client's DialContext (see dial.go).
func (c *Connector) allowedEndpoint(raw string) error {
	value, err := url.Parse(raw)
	if err != nil {
		return errors.Wrap(err, "parse MCP server URL")
	}
	if value.Host == "" || value.User != nil {
		return errors.New("MCP server URL must name a host and carry no credentials")
	}
	if value.Scheme != "https" && !c.cfg.AllowPrivateNetworks {
		return errors.New("MCP server URL must be https")
	}
	if value.Scheme != "https" && value.Scheme != "http" {
		return errors.New("MCP server URL must be http(s)")
	}
	return nil
}

// sameHostRedirect refuses a redirect that would carry the credential to another host.
func sameHostRedirect(request *http.Request, via []*http.Request) error {
	if len(via) > 0 && request.URL.Host != via[0].URL.Host {
		return errors.Errorf("MCP server redirected to %q; refusing to forward the credential across hosts", request.URL.Host)
	}
	if len(via) >= 10 {
		return errors.New("MCP server redirected too many times")
	}
	return nil
}

// bearerTransport attaches the guild's decrypted token to every request of one session.
type bearerTransport struct {
	base  http.RoundTripper
	token string
}

func (t bearerTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	clone := request.Clone(request.Context())
	clone.Header.Set("Authorization", "Bearer "+t.token)
	return t.base.RoundTrip(clone)
}
