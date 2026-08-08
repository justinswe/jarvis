package mcpx

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	"github.com/justinswe/jarvis/worker/pkg/genai"
	"github.com/justinswe/jarvis/worker/pkg/llm"
	"github.com/justinswe/std/app"
	"github.com/justinswe/std/errors"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"go.uber.org/zap"
)

// maxResultBytes bounds one MCP tool result before it enters the model context.
const maxResultBytes = 64 << 10

// maxErrorRunes bounds the model-visible text of one failed MCP tool call.
const maxErrorRunes = 1024

// sessionTool adapts one tool of a connected MCP session into a genai.FunctionTool.
// Both remote guild servers and the in-process built-in server go through it, so the
// agent loop sees exactly one kind of tool.
type sessionTool struct {
	session *mcp.ClientSession
	name    string // model-facing name, namespaced for third-party servers
	remote  string // upstream tool name on the session
	decl    *llm.ToolDefinition
	timeout time.Duration
}

func (t *sessionTool) Name() string { return t.name }

func (t *sessionTool) Declaration() *llm.ToolDefinition { return t.decl }

func (t *sessionTool) Execute(ctx context.Context, args map[string]any) (any, error) {
	callCtx, cancel := context.WithTimeout(ctx, t.timeout)
	defer cancel()
	result, err := t.session.CallTool(callCtx, &mcp.CallToolParams{Name: t.remote, Arguments: args})
	if err != nil {
		return nil, genai.NewExecutionError("mcp_call_failed", "The MCP tool call failed.", err)
	}
	if result.IsError {
		// The failure text is the server's guidance for a corrected retry; it is
		// already treated as untrusted data by the system prompt.
		text := truncateRunes(resultText(result), maxErrorRunes)
		if text == "" {
			text = "The MCP tool reported an error."
		}
		return nil, genai.NewExecutionError("mcp_tool_error", text, errors.New("mcp tool returned an error result"))
	}
	return renderResult(result), nil
}

// sessionTools lists a session's tools and adapts each one. An empty namespace keeps
// upstream names (the trusted built-in server); otherwise names become
// mcp_<namespace>_<tool> — the marker the system prompt uses for third-party tools.
func (c *Connector) sessionTools(ctx context.Context, session *mcp.ClientSession, namespace string) ([]genai.FunctionTool, error) {
	listCtx, cancel := context.WithTimeout(ctx, c.timeout())
	defer cancel()
	listed, err := session.ListTools(listCtx, nil)
	if err != nil {
		return nil, errors.Wrap(err, "list MCP tools")
	}
	tools := make([]genai.FunctionTool, 0, len(listed.Tools))
	for _, tool := range listed.Tools {
		schema, ok := SanitizeSchema(schemaObject(tool.InputSchema))
		if !ok {
			app.L().Warn("MCP tool schema is unrepresentable; skipping the tool",
				zap.String("mcp_server", namespace), zap.String("tool_name", tool.Name))
			continue
		}
		name := tool.Name
		if namespace != "" {
			name = modelToolName("mcp_" + namespace + "_" + tool.Name)
		}
		// Unknown effects default to mutation so results are never cached and
		// replayed; a server's own readOnlyHint opts into caching.
		effect := llm.ToolEffectMutation
		if tool.Annotations != nil && tool.Annotations.ReadOnlyHint {
			effect = llm.ToolEffectReadOnly
		}
		tools = append(tools, &sessionTool{
			session: session, name: name, remote: tool.Name, timeout: c.timeout(),
			decl: &llm.ToolDefinition{Name: name, Description: tool.Description, InputSchema: schema, Effect: effect},
		})
	}
	return tools, nil
}

// schemaObject normalizes a listed tool's input schema to a plain JSON object map.
func schemaObject(raw any) any {
	if object, ok := raw.(map[string]any); ok {
		return object
	}
	encoded, err := json.Marshal(raw)
	if err != nil {
		return nil
	}
	var object map[string]any
	if json.Unmarshal(encoded, &object) != nil {
		return nil
	}
	return object
}

// modelToolName forces a name into the charset and length providers accept.
func modelToolName(name string) string {
	var builder strings.Builder
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9',
			r == '_', r == '.', r == ':', r == '-':
			builder.WriteRune(r)
		default:
			builder.WriteByte('_')
		}
	}
	result := builder.String()
	if len(result) > 128 {
		result = result[:128]
	}
	return result
}

// dedupeTools keeps the first tool for each model-facing name; a later duplicate is
// dropped with a warning rather than failing the whole request.
func dedupeTools(tools []genai.FunctionTool) []genai.FunctionTool {
	seen := make(map[string]struct{}, len(tools))
	result := make([]genai.FunctionTool, 0, len(tools))
	for _, tool := range tools {
		if _, duplicate := seen[tool.Name()]; duplicate {
			app.L().Warn("Duplicate MCP tool name; keeping the first", zap.String("tool_name", tool.Name()))
			continue
		}
		seen[tool.Name()] = struct{}{}
		result = append(result, tool)
	}
	return result
}

// renderResult converts one successful MCP result into a tool output value, bounded so
// a misbehaving server cannot flood the model context.
func renderResult(result *mcp.CallToolResult) any {
	if result.StructuredContent != nil {
		if encoded, err := json.Marshal(result.StructuredContent); err == nil && len(encoded) <= maxResultBytes {
			return result.StructuredContent
		}
	}
	text := resultText(result)
	if len(text) > maxResultBytes {
		text = text[:maxResultBytes] + "\n[truncated by the application]"
	}
	return text
}

// resultText joins the text content items of one result.
func resultText(result *mcp.CallToolResult) string {
	var parts []string
	for _, content := range result.Content {
		if text, ok := content.(*mcp.TextContent); ok && text.Text != "" {
			parts = append(parts, text.Text)
		}
	}
	return strings.Join(parts, "\n")
}

func truncateRunes(text string, limit int) string {
	runes := []rune(text)
	if len(runes) <= limit {
		return text
	}
	return string(runes[:limit])
}
