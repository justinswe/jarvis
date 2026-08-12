package discord

import (
	"context"
	"strings"
	"testing"

	"github.com/bwmarrin/discordgo"
	"github.com/justinswe/jarvis/store"
	"github.com/justinswe/jarvis/worker/pkg/character"
	"github.com/justinswe/jarvis/worker/pkg/genai"
	"github.com/justinswe/jarvis/worker/pkg/llm"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type fakeCharacterStore struct {
	characters map[string]store.Character
	bindings   map[string]string // channelID -> character name
}

func newFakeCharacterStore() *fakeCharacterStore {
	return &fakeCharacterStore{characters: map[string]store.Character{}, bindings: map[string]string{}}
}

func (f *fakeCharacterStore) UpsertCharacter(_ context.Context, guildID, actorID, name, cardJSON, avatarPNG string) (store.Character, error) {
	stored := store.Character{ID: int64(len(f.characters) + 1), GuildID: guildID, Name: name, CardJSON: cardJSON, AvatarPNG: avatarPNG, CreatedBy: actorID}
	f.characters[strings.ToLower(name)] = stored
	return stored, nil
}

func (f *fakeCharacterStore) Character(_ context.Context, _, name string) (store.Character, error) {
	stored, ok := f.characters[strings.ToLower(name)]
	if !ok {
		return store.Character{}, store.ErrCharacterNotFound
	}
	return stored, nil
}

func (f *fakeCharacterStore) Characters(context.Context, string) ([]store.CharacterSummary, error) {
	var summaries []store.CharacterSummary
	for _, stored := range f.characters {
		summaries = append(summaries, store.CharacterSummary{ID: stored.ID, Name: stored.Name})
	}
	return summaries, nil
}

func (f *fakeCharacterStore) DeleteCharacter(_ context.Context, _, name string) error {
	if _, ok := f.characters[strings.ToLower(name)]; !ok {
		return store.ErrCharacterNotFound
	}
	delete(f.characters, strings.ToLower(name))
	return nil
}

func (f *fakeCharacterStore) BindChannelCharacter(_ context.Context, channelID, guildID, name, _ string) (store.Character, error) {
	stored, ok := f.characters[strings.ToLower(name)]
	if !ok {
		return store.Character{}, store.ErrCharacterNotFound
	}
	f.bindings[channelID] = stored.Name
	return stored, nil
}

func (f *fakeCharacterStore) UnbindChannelCharacter(_ context.Context, channelID string) error {
	delete(f.bindings, channelID)
	return nil
}

func (f *fakeCharacterStore) ChannelCharacter(_ context.Context, channelID string) (*store.Character, error) {
	name, ok := f.bindings[channelID]
	if !ok {
		return nil, nil
	}
	stored := f.characters[strings.ToLower(name)]
	return &stored, nil
}

// fakeScreenHost answers every screening call with one canned verdict.
type fakeScreenHost struct{ verdict string }

func (f fakeScreenHost) Generate(context.Context, llm.Request) (llm.Response, error) {
	return llm.Response{Message: llm.TextMessage(llm.RoleAssistant, f.verdict)}, nil
}

func screeningRegistry(t *testing.T, verdict string) *llm.Registry {
	t.Helper()
	profile := llm.Profile{Name: "primary", Provider: llm.ProviderOpenRouter, ModelID: "test",
		Capabilities: llm.Capabilities{Tools: true, ToolChoice: true}}
	registry, err := llm.NewRegistry([]llm.Profile{profile},
		map[string]llm.Host{"primary": fakeScreenHost{verdict: verdict}}, llm.Selection{Primary: "primary"})
	require.NoError(t, err)
	return registry
}

func characterMessage() *discordgo.MessageCreate {
	return &discordgo.MessageCreate{Message: &discordgo.Message{
		ID: "1", GuildID: "100", ChannelID: "555", Author: &discordgo.User{ID: "200"},
	}}
}

func TestCharacterToolsGating(t *testing.T) {
	m := characterMessage()

	// No store: no tools at all.
	withoutStore := &Processor{botID: "bot"}
	assert.Empty(t, withoutStore.characterTools(m, "root"))

	// Unprivileged: read-only tools.
	p := &Processor{botID: "bot", characters: newFakeCharacterStore()}
	names := toolNames(p.characterTools(m, ""))
	assert.ElementsMatch(t, []string{listCharactersToolName, getCharacterToolName, exportCharacterToolName}, names)

	// Admin ladder: full set.
	names = toolNames(p.characterTools(m, "delegated_admin"))
	assert.Len(t, names, 8)
	assert.Contains(t, names, importCharacterToolName)
	assert.Contains(t, names, activateCharacterToolName)

	// Mutations still refuse when the tool is executed without authorization.
	tool := characterToolWithAction(characterTool{p: p, message: m, manage: false}, deleteCharacterToolName)
	_, err := tool.Execute(context.Background(), map[string]any{"name": "Elyra"})
	requireExecutionErrorCode(t, err, "authorization_denied")
}

func toolNames(tools []genai.FunctionTool) []string {
	var names []string
	for _, tool := range tools {
		names = append(names, tool.Name())
	}
	return names
}

func requireExecutionErrorCode(t *testing.T, err error, code string) {
	t.Helper()
	var execErr *genai.ExecutionError
	require.ErrorAs(t, err, &execErr)
	assert.Equal(t, code, execErr.Code)
}

func TestScreenAndStore(t *testing.T) {
	card, err := character.Parse([]byte(`{"name":"Elyra","description":"A sea captain."}`))
	require.NoError(t, err)

	// Allowed cards are stored.
	characters := newFakeCharacterStore()
	p := &Processor{characters: characters, models: screeningRegistry(t, `{"verdict":"allow","reason":"fine"}`)}
	tool := characterTool{p: p, message: characterMessage(), manage: true}
	result, err := tool.screenAndStore(context.Background(), card, "")
	require.NoError(t, err)
	assert.Equal(t, map[string]any{"status": "stored", "name": "Elyra"}, result)
	assert.Len(t, characters.characters, 1)

	// Rejected cards never reach the store.
	characters = newFakeCharacterStore()
	p = &Processor{characters: characters, models: screeningRegistry(t, `{"verdict":"reject_minor","reason":"prohibited"}`)}
	tool = characterTool{p: p, message: characterMessage(), manage: true}
	_, err = tool.screenAndStore(context.Background(), card, "")
	requireExecutionErrorCode(t, err, "card_rejected")
	assert.Empty(t, characters.characters)

	// A broken screen fails closed.
	characters = newFakeCharacterStore()
	p = &Processor{characters: characters, models: screeningRegistry(t, "no verdict here")}
	tool = characterTool{p: p, message: characterMessage(), manage: true}
	_, err = tool.screenAndStore(context.Background(), card, "")
	requireExecutionErrorCode(t, err, "screening_unavailable")
	assert.Empty(t, characters.characters)
}

func TestActivateAndDeactivate(t *testing.T) {
	characters := newFakeCharacterStore()
	p := &Processor{characters: characters, models: screeningRegistry(t, `{"verdict":"allow","reason":"fine"}`)}
	m := characterMessage()
	_, err := characters.UpsertCharacter(context.Background(), "100", "200", "Elyra", `{"name":"Elyra"}`, "")
	require.NoError(t, err)

	activate := characterToolWithAction(characterTool{p: p, message: m, manage: true}, activateCharacterToolName)
	result, err := activate.Execute(context.Background(), map[string]any{"name": "elyra"})
	require.NoError(t, err)
	assert.Equal(t, "activated", result.(map[string]any)["status"])
	active, err := characters.ChannelCharacter(context.Background(), "555")
	require.NoError(t, err)
	require.NotNil(t, active)
	assert.Equal(t, "Elyra", active.Name)

	deactivate := characterToolWithAction(characterTool{p: p, message: m, manage: true}, deactivateCharacterToolName)
	_, err = deactivate.Execute(context.Background(), nil)
	require.NoError(t, err)
	active, err = characters.ChannelCharacter(context.Background(), "555")
	require.NoError(t, err)
	assert.Nil(t, active)
}

func TestUpdateCharacterField(t *testing.T) {
	characters := newFakeCharacterStore()
	p := &Processor{characters: characters, models: screeningRegistry(t, `{"verdict":"allow","reason":"fine"}`)}
	m := characterMessage()
	_, err := characters.UpsertCharacter(context.Background(), "100", "200", "Elyra",
		`{"spec":"chara_card_v2","data":{"name":"Elyra","description":"old"},"extra":1}`, "")
	require.NoError(t, err)

	update := characterToolWithAction(characterTool{p: p, message: m, manage: true}, updateCharacterFieldToolName)
	_, err = update.Execute(context.Background(), map[string]any{"name": "Elyra", "field": "description", "value": "new"})
	require.NoError(t, err)
	stored, err := characters.Character(context.Background(), "100", "Elyra")
	require.NoError(t, err)
	assert.Contains(t, stored.CardJSON, `"new"`)
	assert.Contains(t, stored.CardJSON, `"extra":1`) // unknown fields survive edits

	_, err = update.Execute(context.Background(), map[string]any{"name": "Elyra", "field": "name", "value": "x"})
	requireExecutionErrorCode(t, err, "invalid_card")
}
