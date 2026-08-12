package discord

import (
	"context"
	"encoding/base64"
	"slices"
	"strings"

	"github.com/bwmarrin/discordgo"
	"github.com/justinswe/jarvis/store"
	"github.com/justinswe/jarvis/worker/pkg/character"
	"github.com/justinswe/jarvis/worker/pkg/genai"
	"github.com/justinswe/jarvis/worker/pkg/llm"
	"github.com/justinswe/std/errors"
)

const (
	importCharacterToolName      = "import_character"
	exportCharacterToolName      = "export_character"
	listCharactersToolName       = "list_characters"
	getCharacterToolName         = "get_character"
	updateCharacterFieldToolName = "update_character_field"
	deleteCharacterToolName      = "delete_character"
	activateCharacterToolName    = "activate_character"
	deactivateCharacterToolName  = "deactivate_character"
)

// editableCardFields are the card fields update_character_field may rewrite. Edits are
// re-screened because every one of these is a screened field.
var editableCardFields = []string{
	"description", "personality", "scenario", "first_mes", "mes_example",
	"system_prompt", "post_history_instructions",
}

// CharacterStore persists roleplay characters and their channel bindings.
type CharacterStore interface {
	UpsertCharacter(ctx context.Context, guildID, actorID, name, cardJSON, avatarPNG string) (store.Character, error)
	Character(ctx context.Context, guildID, name string) (store.Character, error)
	Characters(ctx context.Context, guildID string) ([]store.CharacterSummary, error)
	DeleteCharacter(ctx context.Context, guildID, name string) error
	BindChannelCharacter(ctx context.Context, channelID, guildID, name, actorID string) (store.Character, error)
	UnbindChannelCharacter(ctx context.Context, channelID string) error
	ChannelCharacter(ctx context.Context, channelID string) (*store.Character, error)
}

// characterTools builds the character management surface: reads for everyone, mutations
// only for the administrative ladder.
func (p *Processor) characterTools(m *discordgo.MessageCreate, access string) []genai.FunctionTool {
	if p.characters == nil || m.GuildID == "" || m.Author == nil {
		return nil
	}
	base := characterTool{p: p, message: m, manage: access != ""}
	tools := []genai.FunctionTool{
		characterToolWithAction(base, listCharactersToolName),
		characterToolWithAction(base, getCharacterToolName),
		characterToolWithAction(base, exportCharacterToolName),
	}
	if base.manage {
		tools = append(tools,
			characterToolWithAction(base, importCharacterToolName),
			characterToolWithAction(base, updateCharacterFieldToolName),
			characterToolWithAction(base, deleteCharacterToolName),
			characterToolWithAction(base, activateCharacterToolName),
			characterToolWithAction(base, deactivateCharacterToolName),
		)
	}
	return tools
}

type characterTool struct {
	p       *Processor
	message *discordgo.MessageCreate
	action  string
	manage  bool
}

func characterToolWithAction(tool characterTool, action string) characterTool {
	tool.action = action
	return tool
}

func (t characterTool) Name() string { return t.action }

func (t characterTool) Declaration() *llm.ToolDefinition {
	nameArg := map[string]any{"name": stringSchema("The character's name.")}
	switch t.action {
	case importCharacterToolName:
		return &llm.ToolDefinition{
			Name: t.action,
			Description: "Import a SillyTavern/TavernAI character card (v1 or v2, PNG or JSON) from a URL " +
				"or from the file attached to the requesting message. Call only for an explicit administrator request.",
			InputSchema: objectSchema(map[string]any{
				"url": stringSchema("Optional https URL of the card. Omit to import the requesting message's attachment."),
			}, nil),
			Effect: llm.ToolEffectMutation,
		}
	case exportCharacterToolName:
		return &llm.ToolDefinition{
			Name:        t.action,
			Description: "Post a character's card into the channel as downloadable JSON (and PNG when the character has one).",
			InputSchema: objectSchema(nameArg, []string{"name"}), Effect: llm.ToolEffectReadOnly,
		}
	case listCharactersToolName:
		return &llm.ToolDefinition{
			Name:        t.action,
			Description: "List this server's roleplay characters.",
			InputSchema: objectSchema(nil, nil), Effect: llm.ToolEffectReadOnly,
		}
	case getCharacterToolName:
		return &llm.ToolDefinition{
			Name:        t.action,
			Description: "Read one character's card fields.",
			InputSchema: objectSchema(nameArg, []string{"name"}), Effect: llm.ToolEffectReadOnly,
		}
	case updateCharacterFieldToolName:
		return &llm.ToolDefinition{
			Name:        t.action,
			Description: "Rewrite one field of a character's card. Call only for an explicit administrator request.",
			InputSchema: objectSchema(map[string]any{
				"name":  stringSchema("The character's name."),
				"field": enumStringSchema("The card field to rewrite.", editableCardFields),
				"value": stringSchema("The new field text."),
			}, []string{"name", "field", "value"}),
			Effect: llm.ToolEffectMutation,
		}
	case deleteCharacterToolName:
		return &llm.ToolDefinition{
			Name:        t.action,
			Description: "Delete a character and its channel activations. Call only for an explicit administrator request.",
			InputSchema: objectSchema(nameArg, []string{"name"}), Effect: llm.ToolEffectMutation,
		}
	case activateCharacterToolName:
		return &llm.ToolDefinition{
			Name: t.action,
			Description: "Activate a character in the current channel or thread: the bot then roleplays as that " +
				"character here. Call only for an explicit administrator request.",
			InputSchema: objectSchema(nameArg, []string{"name"}), Effect: llm.ToolEffectMutation,
		}
	case deactivateCharacterToolName:
		return &llm.ToolDefinition{
			Name:        t.action,
			Description: "Deactivate the current channel's character, returning the channel to assistant mode.",
			InputSchema: objectSchema(nil, nil), Effect: llm.ToolEffectMutation,
		}
	default:
		return nil
	}
}

func (t characterTool) Execute(ctx context.Context, args map[string]any) (any, error) {
	p, m := t.p, t.message
	if p.characters == nil || m.GuildID == "" {
		return nil, genai.NewExecutionError("characters_unavailable", "Character storage is not configured.", nil)
	}
	switch t.action {
	case listCharactersToolName:
		summaries, err := p.characters.Characters(ctx, m.GuildID)
		if err != nil {
			return nil, characterFailure(err)
		}
		names := make([]string, 0, len(summaries))
		for _, summary := range summaries {
			names = append(names, summary.Name)
		}
		return map[string]any{"characters": names}, nil
	case getCharacterToolName:
		stored, card, err := t.loadCard(ctx, args)
		if err != nil {
			return nil, err
		}
		return map[string]any{
			"name":          stored.Name,
			"description":   truncateRunes(card.Data.Description, 1500),
			"personality":   truncateRunes(card.Data.Personality, 1500),
			"scenario":      truncateRunes(card.Data.Scenario, 1500),
			"first_message": truncateRunes(card.Data.FirstMes, 1500),
			"has_avatar":    stored.AvatarPNG != "",
		}, nil
	case exportCharacterToolName:
		stored, card, err := t.loadCard(ctx, args)
		if err != nil {
			return nil, err
		}
		files := []*discordgo.File{{
			Name: stored.Name + ".json", ContentType: "application/json",
			Reader: strings.NewReader(string(card.ExportJSON())),
		}}
		if stored.AvatarPNG != "" {
			avatar, err := base64.StdEncoding.DecodeString(stored.AvatarPNG)
			if err == nil {
				if embedded, err := character.EmbedCard(avatar, card.ExportJSON()); err == nil {
					files = append(files, &discordgo.File{
						Name: stored.Name + ".png", ContentType: "image/png",
						Reader: strings.NewReader(string(embedded)),
					})
				}
			}
		}
		if _, err := p.client.SendFile(ctx, m.ChannelID, "Character export: "+stored.Name, files); err != nil {
			return nil, genai.NewExecutionError("export_failed", "Posting the export failed.", err)
		}
		return map[string]any{"status": "exported", "name": stored.Name}, nil
	case importCharacterToolName:
		return t.importCharacter(ctx, args)
	case updateCharacterFieldToolName:
		return t.updateCharacterField(ctx, args)
	case deleteCharacterToolName:
		if err := t.requireManage(); err != nil {
			return nil, err
		}
		name, err := nameArgument(args)
		if err != nil {
			return nil, err
		}
		if err := p.characters.DeleteCharacter(ctx, m.GuildID, name); err != nil {
			return nil, characterFailure(err)
		}
		return map[string]any{"status": "deleted", "name": name}, nil
	case activateCharacterToolName:
		if err := t.requireManage(); err != nil {
			return nil, err
		}
		name, err := nameArgument(args)
		if err != nil {
			return nil, err
		}
		bound, err := p.characters.BindChannelCharacter(ctx, m.ChannelID, m.GuildID, name, m.Author.ID)
		if err != nil {
			return nil, characterFailure(err)
		}
		// The character opens the scene with its card greeting, when it has one.
		if card, err := character.Parse([]byte(bound.CardJSON)); err == nil {
			if greeting := strings.TrimSpace(character.ReplaceMacros(card.Data.FirstMes, bound.Name, displayName(m.Author))); greeting != "" {
				_, _ = p.sendMessageChunks(ctx, m.ChannelID, greeting)
			}
		}
		return map[string]any{"status": "activated", "name": bound.Name, "channel_id": m.ChannelID}, nil
	case deactivateCharacterToolName:
		if err := t.requireManage(); err != nil {
			return nil, err
		}
		if err := p.characters.UnbindChannelCharacter(ctx, m.ChannelID); err != nil {
			return nil, characterFailure(err)
		}
		return map[string]any{"status": "deactivated", "channel_id": m.ChannelID}, nil
	default:
		return nil, genai.NewExecutionError("unsupported_function", "Unknown character action.", nil)
	}
}

func (t characterTool) importCharacter(ctx context.Context, args map[string]any) (any, error) {
	if err := t.requireManage(); err != nil {
		return nil, err
	}
	body, err := t.cardBody(ctx, args)
	if err != nil {
		return nil, err
	}
	cardJSON, avatar, err := character.Decode(body)
	if err != nil {
		return nil, genai.NewExecutionError("invalid_card", err.Error(), err)
	}
	card, err := character.Parse(cardJSON)
	if err != nil {
		return nil, genai.NewExecutionError("invalid_card", err.Error(), err)
	}
	return t.screenAndStore(ctx, card, base64.StdEncoding.EncodeToString(avatar))
}

func (t characterTool) updateCharacterField(ctx context.Context, args map[string]any) (any, error) {
	if err := t.requireManage(); err != nil {
		return nil, err
	}
	stored, card, err := t.loadCard(ctx, args)
	if err != nil {
		return nil, err
	}
	field, _ := args["field"].(string)
	value, _ := args["value"].(string)
	if !slices.Contains(editableCardFields, field) {
		return nil, genai.NewExecutionError("invalid_card", "field is not editable", nil)
	}
	edited, err := character.SetField(card.Raw, field, value)
	if err != nil {
		return nil, genai.NewExecutionError("invalid_card", err.Error(), err)
	}
	editedCard, err := character.Parse(edited)
	if err != nil {
		return nil, genai.NewExecutionError("invalid_card", err.Error(), err)
	}
	return t.screenAndStore(ctx, editedCard, stored.AvatarPNG)
}

// screenAndStore is the single write funnel: nothing reaches the store unscreened.
func (t characterTool) screenAndStore(ctx context.Context, card character.Card, avatarPNG string) (any, error) {
	verdict, err := t.p.screenCard(ctx, card)
	if err != nil {
		// Fail closed: an unavailable screen refuses the import.
		return nil, genai.NewExecutionError("screening_unavailable", "Card screening is unavailable; the card was not imported.", err)
	}
	if !verdict.Allowed() {
		return nil, genai.NewExecutionError("card_rejected", "The card was rejected by content screening: "+verdict.Reason, nil)
	}
	stored, err := t.p.characters.UpsertCharacter(ctx, t.message.GuildID, t.message.Author.ID,
		card.Data.Name, string(card.Raw), avatarPNG)
	if err != nil {
		return nil, characterFailure(err)
	}
	return map[string]any{"status": "stored", "name": stored.Name}, nil
}

// cardBody resolves the import source: an explicit URL, or the requesting message's
// attachment. Both routes go through the same https-only, size-bounded fetcher.
func (t characterTool) cardBody(ctx context.Context, args map[string]any) ([]byte, error) {
	rawURL, _ := args["url"].(string)
	if strings.TrimSpace(rawURL) == "" {
		attachment := firstCardAttachment(t.message.Attachments)
		if attachment == nil {
			return nil, genai.NewExecutionError("no_card", "Provide a card URL or attach a .png or .json card file.", nil)
		}
		rawURL = attachment.URL
	}
	body, err := t.p.downloadCard(ctx, rawURL)
	if err != nil {
		return nil, genai.NewExecutionError("fetch_failed", err.Error(), err)
	}
	return body, nil
}

func (p *Processor) downloadCard(ctx context.Context, url string) ([]byte, error) {
	if p.fetchCard != nil {
		return p.fetchCard(ctx, url)
	}
	return character.Fetch(ctx, url)
}

func firstCardAttachment(attachments []*discordgo.MessageAttachment) *discordgo.MessageAttachment {
	for _, attachment := range attachments {
		if attachment == nil {
			continue
		}
		name := strings.ToLower(attachment.Filename)
		if strings.HasSuffix(name, ".png") || strings.HasSuffix(name, ".json") {
			return attachment
		}
	}
	return nil
}

func (t characterTool) loadCard(ctx context.Context, args map[string]any) (store.Character, character.Card, error) {
	name, err := nameArgument(args)
	if err != nil {
		return store.Character{}, character.Card{}, err
	}
	stored, err := t.p.characters.Character(ctx, t.message.GuildID, name)
	if err != nil {
		return store.Character{}, character.Card{}, characterFailure(err)
	}
	card, err := character.Parse([]byte(stored.CardJSON))
	if err != nil {
		return store.Character{}, character.Card{}, genai.NewExecutionError("invalid_card", err.Error(), err)
	}
	return stored, card, nil
}

func (t characterTool) requireManage() error {
	if !t.manage {
		return genai.NewExecutionError("authorization_denied", "Only a server administrator may manage characters.", nil)
	}
	return nil
}

func nameArgument(args map[string]any) (string, error) {
	name, _ := args["name"].(string)
	if strings.TrimSpace(name) == "" {
		return "", genai.NewExecutionError("invalid_card", "name must be a non-empty string", nil)
	}
	return strings.TrimSpace(name), nil
}

func characterFailure(err error) error {
	if errors.Is(err, store.ErrCharacterNotFound) {
		return genai.NewExecutionError("character_not_found", "This server has no character by that name.", err)
	}
	return genai.NewExecutionError("character_storage_failed", "Character storage failed.", err)
}

// screenCard screens one card against the deployment's primary model profile.
func (p *Processor) screenCard(ctx context.Context, card character.Card) (character.Verdict, error) {
	if p.models == nil {
		return character.Verdict{}, errors.New("model registry unavailable")
	}
	name := p.models.Selection().Primary
	host, hostOK := p.models.Host(name)
	profile, profileOK := p.models.Profile(name)
	if !hostOK || !profileOK {
		return character.Verdict{}, errors.Errorf("screening profile %q unavailable", name)
	}
	return character.Screen(ctx, host, profile, card)
}

func truncateRunes(text string, limit int) string {
	runes := []rune(text)
	if len(runes) <= limit {
		return text
	}
	return string(runes[:limit]) + "…"
}
