package discord

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestActiveRoleplay(t *testing.T) {
	characters := newFakeCharacterStore()
	_, err := characters.UpsertCharacter(context.Background(), "100", "200", "Elyra",
		`{"spec":"chara_card_v2","data":{"name":"Elyra","description":"{{char}} sails with {{user}}."}}`, "")
	require.NoError(t, err)
	p := &Processor{botID: "bot", characters: characters}
	m := characterMessage()
	m.Author.Username = "justin"

	// Unbound channel: assistant mode.
	assert.Nil(t, p.activeRoleplay(context.Background(), m))
	assert.False(t, p.hasActiveCharacter(context.Background(), "555"))

	_, err = characters.BindChannelCharacter(context.Background(), "555", "100", "Elyra", "200")
	require.NoError(t, err)

	roleplay := p.activeRoleplay(context.Background(), m)
	require.NotNil(t, roleplay)
	assert.Equal(t, "Elyra", roleplay.CharacterName)
	assert.Equal(t, "Elyra sails with justin.", roleplay.Description, "macros are substituted")
	assert.True(t, p.hasActiveCharacter(context.Background(), "555"), "bound channel targets every message")

	// OOC prefixes escape to assistant mode; ordinary prose does not.
	for _, content := range []string{"ooc: list characters", "(ooc) hello", "// deactivate please", "OOC, who am I"} {
		m.Content = content
		assert.Nil(t, p.activeRoleplay(context.Background(), m), content)
	}
	for _, content := range []string{"hello there", "the doocot is empty", "look // behind you"} {
		m.Content = content
		assert.NotNil(t, p.activeRoleplay(context.Background(), m), content)
	}

	// No character store: everything stays assistant mode.
	bare := &Processor{botID: "bot"}
	assert.Nil(t, bare.activeRoleplay(context.Background(), m))
}
