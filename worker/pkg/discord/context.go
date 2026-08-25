package discord

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/justinswe/jarvis/worker/pkg/config"
	"github.com/justinswe/jarvis/worker/pkg/genai"
)

type contextSection struct {
	label    string
	messages []*discordgo.Message
}

type builtPrompt struct {
	messages []genai.Message
	intent   genai.IntentContext
}

func promptHasImage(messages []genai.Message) bool {
	for _, message := range messages {
		if message.Image != nil {
			return true
		}
	}
	return false
}

const incompleteContextNotice = "CONTEXT NOTICE: Stored conversation history could not be fully loaded; the supplied history may be incomplete."

func (p *Processor) buildPrompt(ctx context.Context, channel *discordgo.Channel, m *discordgo.MessageCreate, settings config.ServerSettings) ([]genai.Message, error) {
	built, err := p.buildPromptWithIntent(ctx, channel, m, settings)
	return built.messages, err
}

func (p *Processor) buildPromptWithIntent(ctx context.Context, channel *discordgo.Channel, m *discordgo.MessageCreate, settings config.ServerSettings) (builtPrompt, error) {
	current := sanitizeContent(m.Content, p.botID)
	image, imageAttachmentNotice := p.currentImage(ctx, m.Attachments)
	if current == "" && image == nil && imageAttachmentNotice == "" {
		return builtPrompt{}, errEmptyMessageContent
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
	if image == nil && len(m.Attachments) == 0 && visualFollowup(current) {
		image, imageAttachmentNotice = p.previousImage(ctx, m.Message, sections)
		if imageAttachmentNotice != "" {
			current += "\n\n" + imageAttachmentNotice
		}
	}
	messages := buildStructuredContext(sections, current, settings.HistoryRunes, image, incomplete)
	return builtPrompt{
		messages: messages,
		intent: genai.IntentContext{
			CurrentRequest:      current,
			PreviousUserRequest: previousUserRequest(sections, m.Message),
		},
	}, nil
}

func previousSameAuthorRequest(sections []contextSection, authorID string) string {
	for _, section := range sections {
		if section.label == "PARENT CHANNEL" {
			continue
		}
		for i := len(section.messages) - 1; i >= 0; i-- {
			message := section.messages[i]
			if message == nil || message.Author == nil || message.Author.Bot {
				continue
			}
			if message.Author.ID == authorID {
				return sanitizeContent(message.Content, "")
			}
		}
	}
	return ""
}

func previousUserRequest(sections []contextSection, current *discordgo.Message) string {
	if current != nil && current.MessageReference != nil {
		if referenced := referencedUserRequest(sections, current.MessageReference.MessageID); referenced != "" {
			return referenced
		}
	}
	if current == nil || current.Author == nil {
		return ""
	}
	return previousSameAuthorRequest(sections, current.Author.ID)
}

func referencedUserRequest(sections []contextSection, messageID string) string {
	if messageID == "" {
		return ""
	}
	for _, section := range sections {
		for _, message := range section.messages {
			if message == nil || message.ID != messageID || message.Author == nil {
				continue
			}
			if !message.Author.Bot {
				return sanitizeContent(message.Content, "")
			}
			if message.MessageReference != nil {
				return referencedUserRequest(sections, message.MessageReference.MessageID)
			}
		}
	}
	return ""
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
	sections = pruneContextSections(sections, budget)
	var parts []string
	for _, section := range sections {
		if transcript := formatTranscript(section.messages); transcript != "" {
			parts = append(parts, section.label+":\n"+transcript)
		}
	}
	parts = append(parts, currentSection)
	return strings.Join(parts, "\n\n")
}

func buildStructuredContext(sections []contextSection, current string, budget int, image *genai.Image, incomplete bool) []genai.Message {
	sections = pruneContextSections(sections, budget)
	var result []genai.Message
	for _, section := range sections {
		for _, message := range section.messages {
			if message == nil || message.Author == nil {
				continue
			}
			content := sanitizeContent(message.Content, "")
			if content == "" {
				continue
			}
			role := "user"
			if message.Author.Bot {
				role = "assistant"
			}
			result = append(result, genai.Message{Role: role, Content: formatContextMessage(section.label, message, content)})
		}
	}
	content := "CURRENT REQUEST:\n" + strings.TrimSpace(current)
	if incomplete {
		content = incompleteContextNotice + "\n\n" + content
	}
	return append(result, genai.Message{Role: "user", Content: content, Image: image})
}

func formatContextMessage(section string, message *discordgo.Message, content string) string {
	timestamp := "timestamp unavailable"
	if !message.Timestamp.IsZero() {
		timestamp = message.Timestamp.UTC().Format(time.RFC3339)
	}
	return fmt.Sprintf("CONTEXT [%s; %s; author=%s; author_id=%s]:\n%s",
		section, timestamp, displayName(message.Author), message.Author.ID, content)
}

func pruneContextSections(sections []contextSection, budget int) []contextSection {
	if budget <= 0 {
		budget = defaultHistoryRunes
	}
	sections = cloneContextSections(sections)
	for totalHistoryRunes(sections) > budget {
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
	return sections
}

func cloneContextSections(sections []contextSection) []contextSection {
	result := make([]contextSection, len(sections))
	for i, section := range sections {
		result[i] = contextSection{label: section.label, messages: append([]*discordgo.Message(nil), section.messages...)}
	}
	return result
}

func totalHistoryRunes(sections []contextSection) int {
	total := 0
	for _, section := range sections {
		for _, message := range section.messages {
			if message == nil || message.Author == nil {
				continue
			}
			content := sanitizeContent(message.Content, "")
			if content == "" {
				continue
			}
			total += len([]rune(formatContextMessage(section.label, message, content)))
		}
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
			timestamp := "timestamp unavailable"
			if !message.Timestamp.IsZero() {
				timestamp = message.Timestamp.UTC().Format(time.RFC3339)
			}
			author := displayName(message.Author)
			if message.Author.Bot {
				author += " [bot]"
			}
			lines = append(lines, fmt.Sprintf("[%s] %s: %s", timestamp, author, content))
		}
	}
	return strings.Join(lines, "\n")
}
