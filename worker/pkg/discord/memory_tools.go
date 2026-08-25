package discord

import (
	"context"
	"strconv"
	"strings"

	"github.com/bwmarrin/discordgo"
	"github.com/justinswe/jarvis/store"
	"github.com/justinswe/jarvis/worker/pkg/genai"
	"github.com/justinswe/jarvis/worker/pkg/llm"
	"github.com/justinswe/std/errors"
)

const (
	listMemoriesToolName = "list_memories"
	pinMemoryToolName    = "pin_memory"
	unpinMemoryToolName  = "unpin_memory"
	editMemoryToolName   = "edit_memory"
	deleteMemoryToolName = "delete_memory"
	addMemoryToolName    = "add_memory"
)

// memoryTools builds the memory book: reads for everyone, mutations for the
// administrative ladder. Available in assistant mode and OOC only — roleplay turns
// offer no tools.
func (p *Processor) memoryTools(m *discordgo.MessageCreate, access string) []genai.FunctionTool {
	if p.memory == nil || m.GuildID == "" || m.Author == nil {
		return nil
	}
	base := memoryTool{p: p, message: m, manage: access != ""}
	tools := []genai.FunctionTool{memoryToolWithAction(base, listMemoriesToolName)}
	if base.manage {
		tools = append(tools,
			memoryToolWithAction(base, pinMemoryToolName),
			memoryToolWithAction(base, unpinMemoryToolName),
			memoryToolWithAction(base, editMemoryToolName),
			memoryToolWithAction(base, deleteMemoryToolName),
			memoryToolWithAction(base, addMemoryToolName),
		)
	}
	return tools
}

type memoryTool struct {
	p       *Processor
	message *discordgo.MessageCreate
	action  string
	manage  bool
}

func memoryToolWithAction(tool memoryTool, action string) memoryTool {
	tool.action = action
	return tool
}

func (t memoryTool) Name() string { return t.action }

func (t memoryTool) Declaration() *llm.ToolDefinition {
	characterArg := stringSchema("Character name owning the memory. Omit for the current channel's character, or the assistant's own memory when none is active.")
	idArg := map[string]any{"id": integerMinimumSchema("The memory record id from list_memories.", 1)}
	switch t.action {
	case listMemoriesToolName:
		return &llm.ToolDefinition{
			Name:        t.action,
			Description: "Read the persistent memory book: what the character remembers long-term, with record ids.",
			InputSchema: objectSchema(map[string]any{"character": characterArg}, nil), Effect: llm.ToolEffectReadOnly,
		}
	case pinMemoryToolName, unpinMemoryToolName:
		verb := "Pin"
		detail := "a pinned memory is permanent and never evicted."
		if t.action == unpinMemoryToolName {
			verb, detail = "Unpin", "the record becomes evictable again."
		}
		return &llm.ToolDefinition{
			Name:        t.action,
			Description: verb + " one memory record: " + detail + " Call only for an explicit request.",
			InputSchema: objectSchema(idArg, []string{"id"}), Effect: llm.ToolEffectMutation,
		}
	case editMemoryToolName:
		return &llm.ToolDefinition{
			Name:        t.action,
			Description: "Rewrite one memory record's content. Call only for an explicit request.",
			InputSchema: objectSchema(map[string]any{
				"id":      idArg["id"],
				"content": stringSchema("The corrected memory text."),
			}, []string{"id", "content"}),
			Effect: llm.ToolEffectMutation,
		}
	case deleteMemoryToolName:
		return &llm.ToolDefinition{
			Name:        t.action,
			Description: "Permanently delete one memory record. Call only for an explicit request.",
			InputSchema: objectSchema(idArg, []string{"id"}), Effect: llm.ToolEffectMutation,
		}
	case addMemoryToolName:
		return &llm.ToolDefinition{
			Name:        t.action,
			Description: "Add one fact to persistent memory. Call only for an explicit request to remember something.",
			InputSchema: objectSchema(map[string]any{
				"character": characterArg,
				"content":   stringSchema("The fact to remember, stated standalone."),
				"pinned":    booleanSchema("Whether the fact is permanent (never evicted)."),
			}, []string{"content"}),
			Effect: llm.ToolEffectMutation,
		}
	default:
		return nil
	}
}

func (t memoryTool) Execute(ctx context.Context, args map[string]any) (any, error) {
	p, m := t.p, t.message
	if p.memory == nil || m.GuildID == "" {
		return nil, genai.NewExecutionError("memory_unavailable", "Persistent memory is not configured.", nil)
	}
	switch t.action {
	case listMemoriesToolName:
		scope, err := t.scope(ctx, args)
		if err != nil {
			return nil, err
		}
		records, err := p.memory.List(ctx, m.GuildID, m.ChannelID, scope, 100)
		if err != nil {
			return nil, memoryFailure(err)
		}
		listed := make([]map[string]any, 0, len(records))
		for _, record := range records {
			listed = append(listed, map[string]any{
				"id": record.ID, "kind": record.Kind, "entity_key": record.EntityKey,
				"content": record.Content, "pinned": record.Pinned,
			})
		}
		return map[string]any{"memories": listed}, nil
	case pinMemoryToolName, unpinMemoryToolName:
		if err := t.requireManage(); err != nil {
			return nil, err
		}
		id, err := memoryIDArgument(args)
		if err != nil {
			return nil, err
		}
		if err := p.memory.Pin(ctx, m.GuildID, m.ChannelID, id, t.action == pinMemoryToolName); err != nil {
			return nil, memoryFailure(err)
		}
		return map[string]any{"status": "updated", "id": id}, nil
	case editMemoryToolName:
		if err := t.requireManage(); err != nil {
			return nil, err
		}
		id, err := memoryIDArgument(args)
		if err != nil {
			return nil, err
		}
		content, _ := args["content"].(string)
		if strings.TrimSpace(content) == "" {
			return nil, genai.NewExecutionError("invalid_memory", "content must be a non-empty string", nil)
		}
		if err := p.memory.Edit(ctx, m.GuildID, m.ChannelID, id, content); err != nil {
			return nil, memoryFailure(err)
		}
		return map[string]any{"status": "updated", "id": id}, nil
	case deleteMemoryToolName:
		if err := t.requireManage(); err != nil {
			return nil, err
		}
		id, err := memoryIDArgument(args)
		if err != nil {
			return nil, err
		}
		if err := p.memory.Delete(ctx, m.GuildID, m.ChannelID, id); err != nil {
			return nil, memoryFailure(err)
		}
		return map[string]any{"status": "deleted", "id": id}, nil
	case addMemoryToolName:
		if err := t.requireManage(); err != nil {
			return nil, err
		}
		scope, err := t.scope(ctx, args)
		if err != nil {
			return nil, err
		}
		content, _ := args["content"].(string)
		if strings.TrimSpace(content) == "" {
			return nil, genai.NewExecutionError("invalid_memory", "content must be a non-empty string", nil)
		}
		pinned, _ := args["pinned"].(bool)
		if err := p.memory.Add(ctx, m.GuildID, m.ChannelID, m.Author.ID, scope, content, pinned); err != nil {
			return nil, memoryFailure(err)
		}
		return map[string]any{"status": "stored"}, nil
	default:
		return nil, genai.NewExecutionError("unsupported_function", "Unknown memory action.", nil)
	}
}

// scope resolves the memory owner: an explicitly named character, else the channel's
// bound character, else the assistant persona.
func (t memoryTool) scope(ctx context.Context, args map[string]any) (int64, error) {
	if name, _ := args["character"].(string); strings.TrimSpace(name) != "" {
		if t.p.characters == nil {
			return 0, genai.NewExecutionError("characters_unavailable", "Character storage is not configured.", nil)
		}
		stored, err := t.p.characters.Character(ctx, t.message.GuildID, strings.TrimSpace(name))
		if err != nil {
			return 0, characterFailure(err)
		}
		return stored.ID, nil
	}
	return memoryScope(t.p.activeCharacter(ctx, t.message)), nil
}

func (t memoryTool) requireManage() error {
	if !t.manage {
		return genai.NewExecutionError("authorization_denied", "Only a server administrator may change memories.", nil)
	}
	return nil
}

func memoryIDArgument(args map[string]any) (int64, error) {
	switch value := args["id"].(type) {
	case float64:
		return int64(value), nil
	case string:
		id, err := strconv.ParseInt(value, 10, 64)
		if err == nil {
			return id, nil
		}
	}
	return 0, genai.NewExecutionError("invalid_memory", "id must be an integer", nil)
}

func memoryFailure(err error) error {
	switch {
	case errors.Is(err, store.ErrMemoryNotFound):
		return genai.NewExecutionError("memory_not_found", "No memory record has that id.", err)
	case errors.Is(err, store.ErrMemoryUnavailable):
		return genai.NewExecutionError("memory_unavailable", "Persistent memory requires the PostgreSQL store.", err)
	default:
		return genai.NewExecutionError("memory_storage_failed", "Memory storage failed.", err)
	}
}
