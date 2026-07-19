package discord

import (
	"context"
	"fmt"
	"html"
	"net"
	"net/url"
	"regexp"
	"strings"

	"github.com/bwmarrin/discordgo"
	"github.com/justinswe/jarvis/pkg/genai"
	"github.com/justinswe/std/errors"
	"golang.org/x/net/publicsuffix"
)

var botPrefixPattern = regexp.MustCompile(`(?i)^(?:\s*(?:jarvis|jarvischat)\s*[:\-]\s*)+`)
var channelMentionPattern = regexp.MustCompile(`<#[0-9]+>`)

const (
	webUnconfirmedEvidenceStatusSentence = "Current details could not be confirmed from usable web sources."
)

var evidenceStatusText = map[genai.EvidenceStatus]string{
	genai.EvidenceStatusWebUnconfirmed:     webUnconfirmedEvidenceStatusSentence,
	genai.EvidenceStatusRuntimeUnconfirmed: "Current runtime details could not be confirmed.",
	genai.EvidenceStatusChannelUnconfirmed: "Requested channel history could not be confirmed.",
	genai.EvidenceStatusGeneralUnconfirmed: "Some details could not be confirmed with available evidence.",
}

func appendSources(text string, sources []genai.Source) string {
	links := make([]string, 0, 3)
	for _, source := range sources {
		if len(links) == 3 {
			break
		}
		link, ok := formatSourceLink(source)
		if ok {
			links = append(links, fmt.Sprintf("[%d · %s", len(links)+1, strings.TrimPrefix(link, "[")))
		}
	}
	if len(links) == 0 {
		return text
	}
	return strings.TrimSpace(text) + "\n\n-# Sources consulted: " + strings.Join(links, " · ")
}

// formatSourceLink creates a Discord link labeled with its source domain.
func formatSourceLink(source genai.Source) (string, bool) {
	rawURL := strings.TrimSpace(source.URL)
	if strings.ContainsAny(rawURL, "()") {
		return "", false
	}
	parsedURL, err := url.Parse(rawURL)
	if err != nil || parsedURL.User != nil || parsedURL.Host == "" {
		return "", false
	}
	if parsedURL.Scheme != "http" && parsedURL.Scheme != "https" {
		return "", false
	}

	label := baseDomain(parsedURL.Hostname())
	if label == "" {
		return "", false
	}
	return fmt.Sprintf("[%s](%s)", label, rawURL), true
}

// baseDomain reduces a hostname to its registrable domain when possible.
func baseDomain(raw string) string {
	domain := strings.TrimSuffix(strings.ToLower(strings.TrimSpace(raw)), ".")
	if domain == "" || net.ParseIP(domain) != nil {
		return domain
	}
	registrableDomain, err := publicsuffix.EffectiveTLDPlusOne(domain)
	if err != nil {
		return domain
	}
	return registrableDomain
}

func appendEvidence(text string, evidence []genai.Evidence) string {
	labels := make([]string, 0, 3)
	seen := make(map[string]struct{}, 3)
	for _, item := range evidence {
		label := ""
		switch item.Kind {
		case genai.EvidenceKindRuntimeContext:
			label = "runtime context"
		case genai.EvidenceKindChannelHistory:
			label = "channel history"
		}
		if label == "" {
			continue
		}
		if _, ok := seen[label]; ok {
			continue
		}
		seen[label] = struct{}{}
		labels = append(labels, label)
	}
	if len(labels) == 0 {
		return text
	}
	return strings.TrimSpace(text) + "\n\n-# Evidence used: " + strings.Join(labels, " · ")
}

// appendEvidenceStatus renders a recognized status after all provenance footers.
func appendEvidenceStatus(text string, status genai.EvidenceStatus, sources []genai.Source) string {
	return appendEvidenceStatuses(text, []genai.EvidenceStatus{status}, sources)
}

// appendEvidenceStatuses renders recognized statuses after all provenance footers.
func appendEvidenceStatuses(text string, statuses []genai.EvidenceStatus, sources []genai.Source) string {
	text = stripEvidenceStatusFooters(text)
	seen := make(map[genai.EvidenceStatus]struct{}, len(statuses))
	labels := make([]string, 0, len(statuses))
	for _, status := range statuses {
		if status == genai.EvidenceStatusWebUnconfirmed && hasRenderableSource(sources) {
			continue
		}
		label, ok := evidenceStatusText[status]
		if !ok {
			continue
		}
		if _, ok := seen[status]; ok {
			continue
		}
		seen[status] = struct{}{}
		labels = append(labels, label)
	}
	if len(labels) == 0 {
		return text
	}
	return strings.TrimSpace(text) + "\n\n-# Evidence status: " + strings.Join(labels, " ")
}

// stripEvidenceStatusFooters removes model-provided text from the reserved status line.
func stripEvidenceStatusFooters(text string) string {
	lines := strings.Split(text, "\n")
	kept := lines[:0]
	for _, line := range lines {
		if strings.HasPrefix(strings.ToLower(strings.TrimSpace(line)), "-# evidence status:") {
			continue
		}
		kept = append(kept, line)
	}
	return strings.TrimSpace(strings.Join(kept, "\n"))
}

// hasRenderableSource reports whether Discord can display a normalized source.
func hasRenderableSource(sources []genai.Source) bool {
	for _, source := range sources {
		if _, ok := formatSourceLink(source); ok {
			return true
		}
	}
	return false
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
