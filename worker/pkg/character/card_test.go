package character

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const v2Card = `{"spec":"chara_card_v2","spec_version":"2.0","data":{"name":"Elyra","description":"A sea captain.","personality":"gruff","scenario":"harbor","first_mes":"Hello.","mes_example":"<START>...","alternate_greetings":["Ahoy."],"character_book":{"entries":[{"keys":["harbor"],"content":"The harbor is old."}]},"unknown_field":{"kept":true}}}`

const v1Card = `{"name":"Elyra","description":"A sea captain.","personality":"gruff","scenario":"harbor","first_mes":"Hello.","mes_example":"<START>..."}`

func TestParseV2(t *testing.T) {
	card, err := Parse([]byte(v2Card))
	require.NoError(t, err)
	assert.Equal(t, "Elyra", card.Data.Name)
	assert.Equal(t, "A sea captain.", card.Data.Description)
	assert.Equal(t, []string{"Ahoy."}, card.Data.AlternateGreetings)
	assert.NotEmpty(t, card.Data.CharacterBook)
}

func TestParseV1(t *testing.T) {
	card, err := Parse([]byte(v1Card))
	require.NoError(t, err)
	assert.Equal(t, "Elyra", card.Data.Name)
	assert.Equal(t, "gruff", card.Data.Personality)
}

func TestExportIsVerbatim(t *testing.T) {
	for _, raw := range []string{v2Card, v1Card} {
		card, err := Parse([]byte(raw))
		require.NoError(t, err)
		assert.Equal(t, raw, string(card.ExportJSON()))
	}
}

func TestParseRejectsNameless(t *testing.T) {
	_, err := Parse([]byte(`{"description":"no name"}`))
	assert.Error(t, err)
	_, err = Parse([]byte(`not json`))
	assert.Error(t, err)
}

func TestParseVerdict(t *testing.T) {
	verdict, err := parseVerdict("Sure, here's the verdict: {\"verdict\":\"allow\",\"reason\":\"fine\"} hope that helps")
	require.NoError(t, err)
	assert.True(t, verdict.Allowed())

	verdict, err = parseVerdict(`{"verdict":"reject_minor","reason":"underage"}`)
	require.NoError(t, err)
	assert.False(t, verdict.Allowed())

	_, err = parseVerdict(`{"verdict":"maybe"}`)
	assert.Error(t, err)
	_, err = parseVerdict("no json at all")
	assert.Error(t, err)
}

func TestMinorPrefilter(t *testing.T) {
	assert.True(t, minorPrefilter.MatchString("a loli character"))
	assert.False(t, minorPrefilter.MatchString("lollipop trolley"))
}
