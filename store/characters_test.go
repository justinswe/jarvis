package store

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const testCardJSON = `{"spec":"chara_card_v2","data":{"name":"Elyra","description":"A sea captain."}}`

func TestCharacterLifecycle(t *testing.T) {
	s := memoryStore(t)
	ctx := context.Background()

	created, err := s.UpsertCharacter(ctx, "100", "200", "Elyra", testCardJSON, "cGln")
	require.NoError(t, err)
	assert.Equal(t, "Elyra", created.Name)
	assert.Equal(t, testCardJSON, created.CardJSON)
	assert.Equal(t, "cGln", created.AvatarPNG)
	assert.Equal(t, "200", created.CreatedBy)

	// Case-insensitive lookup and upsert-by-name replace.
	loaded, err := s.Character(ctx, "100", "elyra")
	require.NoError(t, err)
	assert.Equal(t, created.ID, loaded.ID)
	replaced, err := s.UpsertCharacter(ctx, "100", "300", "ELYRA", `{"name":"Elyra"}`, "")
	require.NoError(t, err)
	assert.Equal(t, created.ID, replaced.ID)
	assert.Equal(t, `{"name":"Elyra"}`, replaced.CardJSON)
	assert.Equal(t, "200", replaced.CreatedBy) // creator survives replacement

	summaries, err := s.Characters(ctx, "100")
	require.NoError(t, err)
	require.Len(t, summaries, 1)
	assert.Equal(t, "ELYRA", summaries[0].Name)

	// Other guilds see nothing.
	_, err = s.Character(ctx, "999", "Elyra")
	assert.ErrorIs(t, err, ErrCharacterNotFound)

	require.NoError(t, s.DeleteCharacter(ctx, "100", "elyra"))
	assert.ErrorIs(t, s.DeleteCharacter(ctx, "100", "elyra"), ErrCharacterNotFound)
}

func TestChannelCharacterBinding(t *testing.T) {
	s := memoryStore(t)
	ctx := context.Background()

	_, err := s.UpsertCharacter(ctx, "100", "200", "Elyra", testCardJSON, "")
	require.NoError(t, err)
	second, err := s.UpsertCharacter(ctx, "100", "200", "Bram", `{"name":"Bram"}`, "")
	require.NoError(t, err)

	// Unbound channel is assistant mode.
	active, err := s.ChannelCharacter(ctx, "555")
	require.NoError(t, err)
	assert.Nil(t, active)

	bound, err := s.BindChannelCharacter(ctx, "555", "100", "elyra", "200")
	require.NoError(t, err)
	assert.Equal(t, "Elyra", bound.Name)
	active, err = s.ChannelCharacter(ctx, "555")
	require.NoError(t, err)
	require.NotNil(t, active)
	assert.Equal(t, "Elyra", active.Name)
	assert.Equal(t, "100", active.GuildID)

	// Rebinding replaces; binding an unknown character fails.
	_, err = s.BindChannelCharacter(ctx, "555", "100", "Bram", "200")
	require.NoError(t, err)
	active, err = s.ChannelCharacter(ctx, "555")
	require.NoError(t, err)
	assert.Equal(t, second.ID, active.ID)
	_, err = s.BindChannelCharacter(ctx, "555", "100", "nobody", "200")
	assert.ErrorIs(t, err, ErrCharacterNotFound)

	// Deleting the character cascades the binding away.
	require.NoError(t, s.DeleteCharacter(ctx, "100", "Bram"))
	active, err = s.ChannelCharacter(ctx, "555")
	require.NoError(t, err)
	assert.Nil(t, active)

	require.NoError(t, s.UnbindChannelCharacter(ctx, "555"))
}
