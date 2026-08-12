// Package safety implements the fixed generation-side prohibition on sexualized minors.
// Nothing here is configurable by guilds, cards, or prompts.
package safety

import "regexp"

// sexualContent and minorContext together form the generation-side input screen: a
// message matching both is refused before any model call. Input-side only — the roleplay
// system prompt carries the same prohibition for the generation itself.
var (
	sexualContent = regexp.MustCompile(`(?i)\b(?:sex|sexual|sexually|erotic|nsfw|lewd|nude|naked|fuck)\w*\b`)
	minorContext  = regexp.MustCompile(`(?i)\b(?:` +
		`minor|underage|child|children|preteen|loli|shota|` +
		`(?:[1-9]|1[0-7])[- ]?(?:year[- ]?old|yo)|` +
		`little (?:girl|boy)|school ?girl|school ?boy` +
		`)\b`)
)

// BlocksGeneration reports whether the message asks for prohibited sexualized-minor
// content and must be refused without a model call.
func BlocksGeneration(text string) bool {
	return sexualContent.MatchString(text) && minorContext.MatchString(text)
}
