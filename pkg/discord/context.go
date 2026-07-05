package discord

import (
	"fmt"
	"slices"
	"strings"

	"github.com/bwmarrin/discordgo"
	"github.com/justinswe/jarvis/pkg/genai"
)

type contextSection struct {
	label    string
	messages []*discordgo.Message
}

func (b *Bot) buildPrompt(channel *discordgo.Channel, m *discordgo.MessageCreate) ([]genai.Message, error) {
	current := sanitizeContent(m.Content, b.botID)
	if current == "" {
		return nil, errEmptyMessageContent
	}
	var sections []contextSection
	if isThreadChannel(channel) {
		sections = append(sections,
			contextSection{"THREAD HISTORY", b.fetchHistory(m.ChannelID, b.threadLimit, m.ID)},
			contextSection{"PARENT CHANNEL", b.fetchHistory(channel.ParentID, b.parentLimit, "")},
		)
	} else {
		sections = append(sections, contextSection{"CHANNEL HISTORY", b.fetchHistory(m.ChannelID, b.channelLimit, m.ID)})
	}
	content := buildContext(sections, current, b.historyRunes)
	return []genai.Message{{Role: "user", Content: content}}, nil
}

func (b *Bot) fetchHistory(channelID string, limit int, before string) []*discordgo.Message {
	if channelID == "" || b.fetchMessages == nil {
		return nil
	}
	messages, err := b.fetchMessages(channelID, limit, before)
	if err != nil {
		return nil
	}
	slices.Reverse(messages)
	return messages
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
