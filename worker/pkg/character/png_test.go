package character

import (
	"bytes"
	"image"
	"image/png"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func testPNG(t *testing.T) []byte {
	t.Helper()
	var buffer bytes.Buffer
	require.NoError(t, png.Encode(&buffer, image.NewRGBA(image.Rect(0, 0, 2, 2))))
	return buffer.Bytes()
}

func TestEmbedExtractRoundTrip(t *testing.T) {
	embedded, err := EmbedCard(testPNG(t), []byte(v2Card))
	require.NoError(t, err)
	extracted, err := ExtractCard(embedded)
	require.NoError(t, err)
	assert.Equal(t, v2Card, string(extracted))

	decoded, err := png.Decode(bytes.NewReader(embedded))
	require.NoError(t, err)
	assert.Equal(t, image.Rect(0, 0, 2, 2), decoded.Bounds())
}

func TestEmbedReplacesExistingCard(t *testing.T) {
	first, err := EmbedCard(testPNG(t), []byte(v1Card))
	require.NoError(t, err)
	second, err := EmbedCard(first, []byte(v2Card))
	require.NoError(t, err)
	extracted, err := ExtractCard(second)
	require.NoError(t, err)
	assert.Equal(t, v2Card, string(extracted))
}

func TestExtractErrors(t *testing.T) {
	_, err := ExtractCard(testPNG(t))
	assert.Error(t, err) // no embedded card
	_, err = ExtractCard([]byte("not a png"))
	assert.Error(t, err)
	_, err = ExtractCard(pngSignature)
	assert.Error(t, err) // signature with no chunks
}

func TestDecode(t *testing.T) {
	cardJSON, avatar, err := Decode([]byte(v2Card))
	require.NoError(t, err)
	assert.Nil(t, avatar)
	assert.Equal(t, v2Card, string(cardJSON))

	embedded, err := EmbedCard(testPNG(t), []byte(v2Card))
	require.NoError(t, err)
	cardJSON, avatar, err = Decode(embedded)
	require.NoError(t, err)
	assert.Equal(t, embedded, avatar)
	assert.Equal(t, v2Card, string(cardJSON))
}
