package mcpx

import (
	"context"

	discordmcp "github.com/justinswe/discord-mcp"
	"github.com/justinswe/jarvis/worker/pkg/genai"
	"github.com/justinswe/std/errors"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// ServeDiscord builds one per-message in-process MCP server carrying the Discord tool
// subset, scoped to the guild and channel the message arrived in, and returns them as
// FunctionTools.
//
// Only the discordmcp tools traverse this path. Jarvis's own tools stay native: routing
// them through MCP would serialize their outputs to generic JSON and erase the optional
// interfaces the orchestrator depends on (genai.EvidenceProducer above all), which costs
// evidence and answer quality for nothing — they are already the same FunctionTool the
// agent loop consumes.
//
// The guild and channel pins are discordmcp API guarantees, never model-supplied
// arguments: a scoped view ignores those input fields and refuses a channel outside its
// guild even when addressed by raw ID.
func (c *Connector) ServeDiscord(ctx context.Context, dm *discordmcp.Client, guildID, channelID string, names []string) ([]genai.FunctionTool, func(), error) {
	if dm == nil || guildID == "" || len(names) == 0 {
		return nil, func() {}, nil
	}
	server := mcp.NewServer(&mcp.Implementation{Name: "jarvis-discord", Version: "1"}, nil)
	if err := dm.Guild(guildID).Channel(channelID).RegisterTools(server, names...); err != nil {
		return nil, nil, errors.Wrap(err, "register Discord MCP tools")
	}
	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	serverSession, err := server.Connect(ctx, serverTransport, nil)
	if err != nil {
		return nil, nil, errors.Wrap(err, "serve Discord MCP tools")
	}
	clientSession, err := mcp.NewClient(&mcp.Implementation{Name: "jarvis-worker", Version: "1"}, nil).
		Connect(ctx, clientTransport, nil)
	if err != nil {
		_ = serverSession.Close()
		return nil, nil, errors.Wrap(err, "connect Discord MCP tools")
	}
	release := func() {
		_ = clientSession.Close()
		_ = serverSession.Close()
	}
	// Built-in names stay bare: the mcp_ prefix marks third-party servers only.
	tools, err := c.sessionTools(ctx, clientSession, "")
	if err != nil {
		release()
		return nil, nil, err
	}
	return tools, release, nil
}
