package discord

import (
	"context"

	"github.com/bwmarrin/discordgo"
	"github.com/justinswe/jarvis/worker/pkg/config"
	"github.com/justinswe/jarvis/worker/pkg/genai"
	"github.com/justinswe/std/app"
	"go.uber.org/zap"
)

// discordMCPToolNames is the discordmcp subset offered to the model: live reads of the
// current channel only.
//
// Posting stays with Jarvis's reply pipeline (claims, rate limits, recording), so no
// send, edit, or delete tool is ever registered, and the native pre-bound reaction tool
// covers reactions. list_channels and search_messages are withheld because they are
// guild-wide and scoped to the bot's permissions rather than the requesting user's: a
// member who cannot see a private channel could otherwise have Jarvis read it back to
// them. That keeps this surface inside what channel_search_enabled already consents to.
var discordMCPToolNames = []string{"read_messages", "get_message"}

// mcpTools assembles the tool surface for one message: Jarvis's native tools unchanged,
// the Discord subset pinned to this guild and channel, and the guild's remote MCP
// servers. The returned release closes every session at message end. Failures degrade to
// the native tools — MCP never costs a reply.
func (p *Processor) mcpTools(ctx context.Context, m *discordgo.MessageCreate, guildConfig config.GuildConfig, native []genai.FunctionTool) ([]genai.FunctionTool, func()) {
	if p.mcp == nil {
		return native, func() {}
	}
	// Live current-channel reads are the same privacy surface as stored-channel search,
	// so the existing consent flag gates them; DMs have no guild to pin a view to.
	var subset []string
	if m.GuildID != "" && guildConfig.Settings.ChannelSearchEnabled {
		subset = discordMCPToolNames
	}
	tools := append([]genai.FunctionTool(nil), native...)
	discordTools, releaseDiscord, err := p.mcp.ServeDiscord(ctx, p.dm, m.GuildID, m.ChannelID, subset)
	if err != nil {
		app.L().Warn("Discord MCP tools unavailable; continuing with the native tools",
			zap.String("guild_id", m.GuildID), zap.Error(err))
		releaseDiscord = func() {}
	}
	tools = appendNewTools(tools, discordTools)
	remote, releaseRemote := p.mcp.GuildTools(ctx, m.GuildID, p.remoteMCPServers(m.GuildID, guildConfig))
	tools = appendNewTools(tools, remote)
	return tools, func() {
		releaseRemote()
		releaseDiscord()
	}
}

// appendNewTools adds tools whose names are not taken yet.
//
// The generator rejects a duplicate function name by failing the whole request, so a name
// collision must not be able to cost a reply. Native tools are assembled first and
// therefore win: Jarvis's own abilities outrank anything a tool server offers.
func appendNewTools(tools []genai.FunctionTool, added []genai.FunctionTool) []genai.FunctionTool {
	taken := make(map[string]struct{}, len(tools))
	for _, tool := range tools {
		taken[tool.Name()] = struct{}{}
	}
	for _, tool := range added {
		if _, exists := taken[tool.Name()]; exists {
			app.L().Warn("Ignoring an MCP tool whose name is already taken", zap.String("tool_name", tool.Name()))
			continue
		}
		taken[tool.Name()] = struct{}{}
		tools = append(tools, tool)
	}
	return tools
}

// remoteMCPServers merges the deployment defaults with the guild's attachments; a
// guild row overrides a default of the same name. DMs get none — remote MCP access is
// a per-guild grant, and there is no guild to scope a DM to.
func (p *Processor) remoteMCPServers(guildID string, guildConfig config.GuildConfig) []config.MCPServer {
	if guildID == "" {
		return nil
	}
	overridden := make(map[string]struct{}, len(guildConfig.MCPServers))
	for _, server := range guildConfig.MCPServers {
		overridden[server.Name] = struct{}{}
	}
	servers := make([]config.MCPServer, 0, len(p.defaultMCPServers)+len(guildConfig.MCPServers))
	for _, server := range p.defaultMCPServers {
		if _, replaced := overridden[server.Name]; !replaced {
			servers = append(servers, server)
		}
	}
	return append(servers, guildConfig.MCPServers...)
}
