package discord

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/bwmarrin/discordgo"
	"github.com/justinswe/jarvis/internal/config"
	"github.com/justinswe/jarvis/pkg/genai"
)

type contextSection struct {
	label    string
	messages []*discordgo.Message
}

const incompleteContextNotice = "CONTEXT NOTICE: Stored conversation history could not be fully loaded; the supplied history may be incomplete."

func (p *Processor) buildPrompt(ctx context.Context, channel *discordgo.Channel, m *discordgo.MessageCreate, settings config.ServerSettings) ([]genai.Message, error) {
	current := sanitizeContent(m.Content, p.botID)
	image, imageAttachmentNotice := p.currentImage(ctx, m.Attachments)
	if current == "" && image == nil && imageAttachmentNotice == "" {
		return nil, errEmptyMessageContent
	}
	if current == "" {
		current = "Please respond to the attached image."
	}
	if imageAttachmentNotice != "" {
		current += "\n\n" + imageAttachmentNotice
	}
	var sections []contextSection
	incomplete := false
	if isThreadChannel(channel) {
		threadMessages, threadErr := p.fetchHistory(ctx, m.GuildID, m.ChannelID, settings.ThreadMessages, m.ID)
		parentMessages, parentErr := p.fetchHistory(ctx, m.GuildID, channel.ParentID, settings.ParentMessages, "")
		incomplete = threadErr != nil || parentErr != nil
		sections = append(sections,
			contextSection{"THREAD HISTORY", threadMessages},
			contextSection{"PARENT CHANNEL", parentMessages},
		)
	} else {
		channelMessages, channelErr := p.fetchHistory(ctx, m.GuildID, m.ChannelID, settings.ChannelMessages, m.ID)
		incomplete = channelErr != nil
		sections = append(sections, contextSection{"CHANNEL HISTORY", channelMessages})
	}
	content := buildContext(sections, current, settings.HistoryRunes)
	if incomplete {
		content = incompleteContextNotice + "\n\n" + content
	}
	return []genai.Message{{Role: "user", Content: content, Image: image}}, nil
}

func (p *Processor) fetchHistory(ctx context.Context, guildID, channelID string, limit int, before string) ([]*discordgo.Message, error) {
	if channelID == "" {
		return nil, nil
	}
	var messages []*discordgo.Message
	var err error
	if p.history != nil {
		messages, err = p.history.Messages(ctx, guildID, channelID, limit, before)
	} else {
		messages, err = p.client.Messages(ctx, channelID, limit, before)
	}
	if err != nil {
		if p.history == nil {
			return nil, nil
		}
		slices.Reverse(messages)
		return messages, err
	}
	slices.Reverse(messages)
	return messages, nil
}

func buildContext(sections []contextSection, current string, budget int) string {
	currentSection := "CURRENT REQUEST:\n" + strings.TrimSpace(current)
	if budget <= 0 {
		budget = defaultHistoryRunes
	}
	remaining := budget

	// Parent context is lowest priority, followed by the oldest conversation turns.
	for totalHistoryRunes(sections) > remaining {
		removed := false
		for i := range sections {
			if sections[i].label == "PARENT CHANNEL" && len(sections[i].messages) > 0 {
				sections[i].messages = sections[i].messages[1:]
				removed = true
				break
			}
		}
		if removed {
			continue
		}
		for i := range sections {
			if len(sections[i].messages) > 0 {
				sections[i].messages = sections[i].messages[1:]
				removed = true
				break
			}
		}
		if !removed {
			break
		}
	}
	var parts []string
	for _, section := range sections {
		if transcript := formatTranscript(section.messages); transcript != "" {
			parts = append(parts, section.label+":\n"+transcript)
		}
	}
	parts = append(parts, currentSection)
	return strings.Join(parts, "\n\n")
}

func totalHistoryRunes(sections []contextSection) int {
	total := 0
	for _, section := range sections {
		total += len([]rune(section.label + ":\n" + formatTranscript(section.messages) + "\n\n"))
	}
	return total
}

func formatTranscript(messages []*discordgo.Message) string {
	var lines []string
	for _, message := range messages {
		if message == nil || message.Author == nil {
			continue
		}
		content := sanitizeContent(message.Content, "")
		if content != "" {
			lines = append(lines, fmt.Sprintf("%s: %s", displayName(message.Author), content))
		}
	}
	return strings.Join(lines, "\n")
}
