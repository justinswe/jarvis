package character

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"hash/crc32"

	"github.com/justinswe/std/errors"
)

// The card travels in a PNG tEXt chunk keyed "chara" holding base64 JSON — the
// SillyTavern/TavernAI interchange convention.
const charaKeyword = "chara"

var pngSignature = []byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n'}

type chunk struct {
	typ  string
	raw  []byte // complete encoding: length, type, data, crc
	data []byte
}

// ExtractCard returns the character card JSON embedded in a PNG.
func ExtractCard(png []byte) ([]byte, error) {
	chunks, err := parseChunks(png)
	if err != nil {
		return nil, err
	}
	var encoded []byte
	for _, c := range chunks {
		if c.typ == "tEXt" && bytes.HasPrefix(c.data, []byte(charaKeyword+"\x00")) {
			encoded = c.data[len(charaKeyword)+1:] // last chunk wins, matching SillyTavern
		}
	}
	if encoded == nil {
		return nil, errors.New("PNG has no embedded character card")
	}
	card, err := base64.StdEncoding.DecodeString(string(encoded))
	return card, errors.Wrap(err, "decode embedded character card")
}

// EmbedCard returns the PNG carrying cardJSON as its only chara tEXt chunk.
func EmbedCard(png, cardJSON []byte) ([]byte, error) {
	chunks, err := parseChunks(png)
	if err != nil {
		return nil, err
	}
	var out bytes.Buffer
	out.Write(pngSignature)
	for _, c := range chunks {
		if c.typ == "tEXt" && bytes.HasPrefix(c.data, []byte(charaKeyword+"\x00")) {
			continue
		}
		if c.typ == "IEND" {
			out.Write(textChunk(charaKeyword, base64.StdEncoding.EncodeToString(cardJSON)))
		}
		out.Write(c.raw)
	}
	return out.Bytes(), nil
}

func parseChunks(png []byte) ([]chunk, error) {
	if !bytes.HasPrefix(png, pngSignature) {
		return nil, errors.New("not a PNG")
	}
	var chunks []chunk
	rest := png[len(pngSignature):]
	for len(rest) > 0 {
		if len(rest) < 12 {
			return nil, errors.New("truncated PNG chunk")
		}
		length := int(binary.BigEndian.Uint32(rest[:4]))
		if length > len(rest)-12 {
			return nil, errors.New("truncated PNG chunk")
		}
		c := chunk{typ: string(rest[4:8]), raw: rest[:12+length], data: rest[8 : 8+length]}
		chunks = append(chunks, c)
		rest = rest[12+length:]
		if c.typ == "IEND" {
			return chunks, nil
		}
	}
	return nil, errors.New("PNG has no IEND chunk")
}

func textChunk(keyword, text string) []byte {
	data := append([]byte(keyword+"\x00"), text...)
	raw := make([]byte, 8, 12+len(data))
	binary.BigEndian.PutUint32(raw[:4], uint32(len(data)))
	copy(raw[4:8], "tEXt")
	raw = append(raw, data...)
	return binary.BigEndian.AppendUint32(raw, crc32.ChecksumIEEE(raw[4:]))
}
