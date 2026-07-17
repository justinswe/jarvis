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

const googleGroundingRedirectHost = "vertexaisearch.cloud.google.com"

func appendSources(text string, sources []genai.Source) string {
	links := make([]string, 0, 3)
	for _, source := range sources {
		if len(links) == 3 {
			break
		}
		link, ok := formatSourceLink(source)
		if ok {
			links = append(links, link)
		}
	}
	if len(links) == 0 {
		return text
	}
	return strings.TrimSpace(text) + "\n\n-# Sources: " + strings.Join(links, " · ")
}

// formatSourceLink creates a Discord link labeled with its source domain.
func formatSourceLink(source genai.Source) (string, bool) {
	rawURL := strings.TrimSpace(source.URL)
	parsedURL, err := url.Parse(rawURL)
	if err != nil || parsedURL.User != nil || parsedURL.Host == "" {
		return "", false
	}
	if parsedURL.Scheme != "http" && parsedURL.Scheme != "https" {
		return "", false
	}

	label := sourceLabel(source, parsedURL)
	if label == "" {
		return "", false
	}
	return fmt.Sprintf("[%s](%s)", label, rawURL), true
}

// sourceLabel chooses publisher metadata without exposing redirect infrastructure.
func sourceLabel(source genai.Source, parsedURL *url.URL) string {
	if domain, ok := titleDomain(source.Title); ok {
		return domain
	}
	if !isGoogleGroundingDomain(source.Domain) {
		if domain := baseDomain(source.Domain); domain != "" {
			return domain
		}
	}
	if !isGoogleGroundingRedirect(parsedURL) {
		if domain := baseDomain(parsedURL.Hostname()); domain != "" {
			return domain
		}
	}
	return escapeLinkLabel(source.Title)
}

// titleDomain returns a registrable domain only when the title is a hostname.
func titleDomain(raw string) (string, bool) {
	domain := strings.TrimSuffix(strings.ToLower(strings.TrimSpace(raw)), ".")
	if domain == "" || net.ParseIP(domain) != nil {
		return "", false
	}
	parsedDomain, err := url.Parse("https://" + domain)
	if err != nil || parsedDomain.User != nil || parsedDomain.Port() != "" || parsedDomain.Hostname() != domain {
		return "", false
	}
	registrableDomain, err := publicsuffix.EffectiveTLDPlusOne(domain)
	if err != nil {
		return "", false
	}
	return registrableDomain, true
}

// isGoogleGroundingDomain reports whether a domain identifies redirect infrastructure.
func isGoogleGroundingDomain(raw string) bool {
	domain := strings.TrimSuffix(strings.ToLower(strings.TrimSpace(raw)), ".")
	return domain == googleGroundingRedirectHost
}

// isGoogleGroundingRedirect reports whether a URL is a Google grounding redirect.
func isGoogleGroundingRedirect(parsedURL *url.URL) bool {
	if parsedURL == nil || !strings.EqualFold(parsedURL.Hostname(), googleGroundingRedirectHost) {
		return false
	}
	return parsedURL.Path == "/grounding-api-redirect" || strings.HasPrefix(parsedURL.Path, "/grounding-api-redirect/")
}

// escapeLinkLabel makes a textual source title safe for a Markdown link label.
func escapeLinkLabel(raw string) string {
	label := strings.Join(strings.Fields(raw), " ")
	label = strings.ReplaceAll(label, `\`, `\\`)
	label = strings.ReplaceAll(label, "[", `\[`)
	return strings.ReplaceAll(label, "]", `\]`)
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
		case genai.EvidenceKindCodeExecution:
			label = "code execution"
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
