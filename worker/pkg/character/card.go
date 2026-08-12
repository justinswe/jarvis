// Package character implements SillyTavern-compatible character cards: parsing,
// PNG embedding, URL import, and content screening.
package character

import (
	"encoding/json"
	"strings"

	"github.com/justinswe/std/errors"
)

// SpecV2 identifies a v2 card envelope.
const SpecV2 = "chara_card_v2"

// CardData is the parsed read model of a card's data block. Export never uses it; the
// imported bytes are the export source of truth.
type CardData struct {
	Name                    string          `json:"name"`
	Description             string          `json:"description"`
	Personality             string          `json:"personality"`
	Scenario                string          `json:"scenario"`
	FirstMes                string          `json:"first_mes"`
	MesExample              string          `json:"mes_example"`
	CreatorNotes            string          `json:"creator_notes,omitempty"`
	SystemPrompt            string          `json:"system_prompt,omitempty"`
	PostHistoryInstructions string          `json:"post_history_instructions,omitempty"`
	AlternateGreetings      []string        `json:"alternate_greetings,omitempty"`
	Tags                    []string        `json:"tags,omitempty"`
	Creator                 string          `json:"creator,omitempty"`
	CharacterVersion        string          `json:"character_version,omitempty"`
	CharacterBook           json.RawMessage `json:"character_book,omitempty"`
}

// Card is one imported character card. Raw keeps the imported JSON verbatim so export is
// lossless by construction; Data is a read model only.
type Card struct {
	Raw  json.RawMessage
	Data CardData
}

// Parse decodes a v1 (flat) or v2 (enveloped) character card.
func Parse(b []byte) (Card, error) {
	var envelope struct {
		Spec string   `json:"spec"`
		Data CardData `json:"data"`
	}
	if err := json.Unmarshal(b, &envelope); err != nil {
		return Card{}, errors.Wrap(err, "decode character card")
	}
	card := Card{Raw: append(json.RawMessage(nil), b...)}
	if envelope.Spec == SpecV2 {
		card.Data = envelope.Data
	} else if err := json.Unmarshal(b, &card.Data); err != nil {
		return Card{}, errors.Wrap(err, "decode v1 character card")
	}
	if strings.TrimSpace(card.Data.Name) == "" {
		return Card{}, errors.New("character card has no name")
	}
	return card, nil
}

// ExportJSON returns the card exactly as it was imported.
func (c Card) ExportJSON() []byte { return c.Raw }

// ReplaceMacros substitutes the SillyTavern card macros for the active pair of names.
func ReplaceMacros(text, charName, userName string) string {
	return strings.NewReplacer(
		"{{char}}", charName, "{{Char}}", charName, "{{CHAR}}", charName,
		"{{user}}", userName, "{{User}}", userName, "{{USER}}", userName,
		"<BOT>", charName, "<USER>", userName,
	).Replace(text)
}

// SetField rewrites one card field, preserving every other field including unknown ones.
// The result is a new authored version of the card, re-parsed by the caller.
func SetField(raw []byte, field, value string) ([]byte, error) {
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, errors.Wrap(err, "decode character card")
	}
	target := doc
	if doc["spec"] == SpecV2 {
		data, ok := doc["data"].(map[string]any)
		if !ok {
			data = map[string]any{}
			doc["data"] = data
		}
		target = data
	}
	target[field] = value
	edited, err := json.Marshal(doc)
	return edited, errors.Wrap(err, "encode character card")
}
