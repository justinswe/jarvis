package store

import (
	"context"
	"database/sql"
	"strconv"
	"strings"
	"time"

	"github.com/justinswe/std/errors"
)

// ErrCharacterNotFound reports a lookup for a character the guild does not have.
var ErrCharacterNotFound = errors.New("character not found")

// Character is one stored roleplay character. CardJSON is the imported card verbatim.
type Character struct {
	ID        int64
	GuildID   string
	Name      string
	CardJSON  string
	AvatarPNG string // base64 source PNG, empty for JSON imports
	CreatedBy string
	CreatedAt time.Time
	UpdatedAt time.Time
}

// CharacterSummary lists a character without its card body.
type CharacterSummary struct {
	ID        int64
	Name      string
	CreatedBy string
	CreatedAt time.Time
}

const characterColumns = `id, guild_id, name, card_json, avatar_png, created_by, created_at, updated_at`

// UpsertCharacter creates or replaces a guild's character by case-insensitive name.
func (s *Store) UpsertCharacter(ctx context.Context, guildID, actorID, name, cardJSON, avatarPNG string) (Character, error) {
	gid, err := snowflake(guildID)
	if err != nil {
		return Character{}, err
	}
	actor, err := snowflake(actorID)
	if err != nil {
		return Character{}, err
	}
	name = strings.TrimSpace(name)
	if name == "" {
		return Character{}, errors.New("character name is required")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Character{}, errors.Wrap(err, "begin character transaction")
	}
	defer func() { _ = tx.Rollback() }()
	now := s.now().UTC()
	var id int64
	row := tx.QueryRowContext(ctx, s.q(
		`SELECT id FROM characters WHERE guild_id = ? AND lower(name) = lower(?)`+s.d.forUpdate), gid, name)
	switch err := row.Scan(&id); {
	case err == nil:
		if _, err := tx.ExecContext(ctx, s.q(`
			UPDATE characters SET name = ?, card_json = ?, avatar_png = ?, updated_at = ? WHERE id = ?`),
			name, cardJSON, avatarPNG, now.Unix(), id); err != nil {
			return Character{}, errors.Wrap(err, "update character")
		}
	case errors.Is(err, sql.ErrNoRows):
		id = now.UnixNano()
		if _, err := tx.ExecContext(ctx, s.q(`
			INSERT INTO characters (id, guild_id, name, card_json, avatar_png, created_by, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?)`),
			id, gid, name, cardJSON, avatarPNG, actor, now.Unix(), now.Unix()); err != nil {
			return Character{}, errors.Wrap(err, "insert character")
		}
	default:
		return Character{}, errors.Wrap(err, "look up character")
	}
	if err := tx.Commit(); err != nil {
		return Character{}, errors.Wrap(err, "commit character")
	}
	return s.Character(ctx, guildID, name)
}

// Character loads one character by case-insensitive name.
func (s *Store) Character(ctx context.Context, guildID, name string) (Character, error) {
	gid, err := snowflake(guildID)
	if err != nil {
		return Character{}, err
	}
	row := s.db.QueryRowContext(ctx, s.q(`SELECT `+characterColumns+`
		FROM characters WHERE guild_id = ? AND lower(name) = lower(?)`), gid, strings.TrimSpace(name))
	return scanCharacter(row)
}

// Characters lists a guild's characters, newest first, without card bodies.
func (s *Store) Characters(ctx context.Context, guildID string) ([]CharacterSummary, error) {
	gid, err := snowflake(guildID)
	if err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, s.q(`
		SELECT id, name, created_by, created_at FROM characters
		WHERE guild_id = ? ORDER BY created_at DESC`), gid)
	if err != nil {
		return nil, errors.Wrap(err, "query characters")
	}
	defer rows.Close()
	var characters []CharacterSummary
	for rows.Next() {
		var summary CharacterSummary
		var createdBy, createdAt int64
		if err := rows.Scan(&summary.ID, &summary.Name, &createdBy, &createdAt); err != nil {
			return nil, errors.Wrap(err, "decode character row")
		}
		summary.CreatedBy = strconv.FormatInt(createdBy, 10)
		summary.CreatedAt = time.Unix(createdAt, 0).UTC()
		characters = append(characters, summary)
	}
	return characters, errors.Wrap(rows.Err(), "read characters")
}

// DeleteCharacter removes a character and, through cascade, its channel bindings.
func (s *Store) DeleteCharacter(ctx context.Context, guildID, name string) error {
	gid, err := snowflake(guildID)
	if err != nil {
		return err
	}
	result, err := s.db.ExecContext(ctx, s.q(`
		DELETE FROM characters WHERE guild_id = ? AND lower(name) = lower(?)`), gid, strings.TrimSpace(name))
	if err != nil {
		return errors.Wrap(err, "delete character")
	}
	deleted, err := result.RowsAffected()
	if err != nil {
		return errors.Wrap(err, "count deleted characters")
	}
	if deleted == 0 {
		return ErrCharacterNotFound
	}
	return nil
}

// BindChannelCharacter activates a character in one channel or thread.
func (s *Store) BindChannelCharacter(ctx context.Context, channelID, guildID, name, actorID string) (Character, error) {
	character, err := s.Character(ctx, guildID, name)
	if err != nil {
		return Character{}, err
	}
	cid, err := snowflake(channelID)
	if err != nil {
		return Character{}, err
	}
	gid, err := snowflake(guildID)
	if err != nil {
		return Character{}, err
	}
	actor, err := snowflake(actorID)
	if err != nil {
		return Character{}, err
	}
	_, err = s.db.ExecContext(ctx, s.q(`
		INSERT INTO channel_characters (channel_id, guild_id, character_id, activated_by, created_at)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT (channel_id) DO UPDATE SET
			guild_id = excluded.guild_id, character_id = excluded.character_id,
			activated_by = excluded.activated_by, created_at = excluded.created_at`),
		cid, gid, character.ID, actor, s.now().UTC().Unix())
	return character, errors.Wrap(err, "bind channel character")
}

// UnbindChannelCharacter deactivates whatever character the channel has.
func (s *Store) UnbindChannelCharacter(ctx context.Context, channelID string) error {
	cid, err := snowflake(channelID)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, s.q(`DELETE FROM channel_characters WHERE channel_id = ?`), cid)
	return errors.Wrap(err, "unbind channel character")
}

// ChannelCharacter returns the channel's active character, or nil when the channel has
// none and should run in assistant mode.
func (s *Store) ChannelCharacter(ctx context.Context, channelID string) (*Character, error) {
	cid, err := snowflake(channelID)
	if err != nil {
		return nil, err
	}
	row := s.db.QueryRowContext(ctx, s.q(`SELECT `+prefixedColumns("c", characterColumns)+`
		FROM channel_characters b JOIN characters c ON c.id = b.character_id
		WHERE b.channel_id = ?`), cid)
	character, err := scanCharacter(row)
	if errors.Is(err, ErrCharacterNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &character, nil
}

func scanCharacter(row *sql.Row) (Character, error) {
	var character Character
	var gid, createdBy, createdAt, updatedAt int64
	err := row.Scan(&character.ID, &gid, &character.Name, &character.CardJSON,
		&character.AvatarPNG, &createdBy, &createdAt, &updatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Character{}, ErrCharacterNotFound
	}
	if err != nil {
		return Character{}, errors.Wrap(err, "decode character")
	}
	character.GuildID = strconv.FormatInt(gid, 10)
	character.CreatedBy = strconv.FormatInt(createdBy, 10)
	character.CreatedAt = time.Unix(createdAt, 0).UTC()
	character.UpdatedAt = time.Unix(updatedAt, 0).UTC()
	return character, nil
}

// prefixedColumns qualifies a comma-separated column list with a table alias.
func prefixedColumns(alias, columns string) string {
	parts := strings.Split(columns, ",")
	for i, part := range parts {
		parts[i] = alias + "." + strings.TrimSpace(part)
	}
	return strings.Join(parts, ", ")
}
