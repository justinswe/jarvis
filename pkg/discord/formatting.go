package discord

import (
	"context"
	"fmt"
	"html"
	"regexp"
	"strings"

	"github.com/bwmarrin/discordgo"
	"github.com/justinswe/jarvis/pkg/genai"
	"github.com/justinswe/std/errors"
)

var botPrefixPattern = regexp.MustCompile(`(?i)^(?:\s*(?:jarvis|jarvischat)\s*[:\-]\s*)+`)
var channelMentionPattern = regexp.MustCompile(`<#[0-9]+>`)

func appendSources(text string, sources []genai.Source) string {
	links := make([]string, 0, 3)
	for _, source := range sources {
		if len(links) == 3 {
			break
		}
		url := strings.TrimSpace(source.URL)
		if url != "" {
			links = append(links, fmt.Sprintf("[%d](%s)", len(links)+1, url))
		}
	}
	if len(links) == 0 {
		return text
	}
	return strings.TrimSpace(text) + "\n\n-# Sources: " + strings.Join(links, " · ")
}

func sanitizeContent(content, botID string) string {
	if botID != "" {
		content = strings.ReplaceAll(content, fmt.Sprintf("<@%s>", botID), "")
		content = strings.ReplaceAll(content, fmt.Sprintf("<@!%s>", botID), "")
	}
	content = channelMentionPattern.ReplaceAllString(content, "this channel")
	return strings.Join(strings.Fields(html.UnescapeString(content)), " ")
}

func stripBotPrefix(text string) string {
	return strings.TrimSpace(botPrefixPattern.ReplaceAllString(strings.TrimSpace(text), ""))
}
func mentionsBot(users []*discordgo.User, id string) bool {
	for _, u := range users {
		if u != nil && u.ID == id {
			return true
		}
	}
	return false
}
func isThreadChannel(ch *discordgo.Channel) bool {
	return ch != nil && (ch.Type == discordgo.ChannelTypeGuildPublicThread || ch.Type == discordgo.ChannelTypeGuildPrivateThread || ch.Type == discordgo.ChannelTypeGuildNewsThread)
}
func displayName(u *discordgo.User) string {
	if u == nil {
		return ""
	}
	if u.GlobalName != "" {
		return u.GlobalName
	}
	return u.Username
}
func safeThreadName(username, globalName string) string {
	name := strings.TrimSpace(globalName)
	if name == "" {
		name = strings.TrimSpace(username)
	}
	if name == "" {
		return "AI Thread"
	}
	r := []rune(name)
	if len(r) > 64 {
		name = string(r[:64])
	}
	return name
}

func (p *Processor) sendMessageChunks(ctx context.Context, channelID, content string) error {
	if strings.TrimSpace(content) == "" {
		return errors.New("cannot send empty message content")
	}
	for i, chunk := range splitMessageForDiscord(content, discordMessageMaxLength) {
		if _, err := p.client.SendMessage(ctx, channelID, chunk); err != nil {
			return errors.Wrapf(err, "send chunk %d", i+1)
		}
	}
	return nil
}

func splitMessageForDiscord(content string, max int) []string {
	if content == "" {
		return nil
	}
	if max <= 0 {
		max = discordMessageMaxLength
	}
	runes := []rune(content)
	var chunks []string
	for len(runes) > max {
		at := max
		for i := max; i > max/2; i-- {
			if runes[i-1] == '\n' {
				at = i
				break
			}
		}
		chunks = append(chunks, string(runes[:at]))
		runes = runes[at:]
	}
	if len(runes) > 0 {
		chunks = append(chunks, string(runes))
	}
	return chunks
}
