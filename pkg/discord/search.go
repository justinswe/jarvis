package discord

import (
	"context"
	"slices"
	"strings"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/justinswe/jarvis/pkg/genai"
	"github.com/justinswe/std/errors"
	googlegenai "google.golang.org/genai"
)

const (
	discordMessagePageLimit = 100
	channelSearchToolName   = "search_current_channel"
	channelSearchMessages   = 200
	channelSearchResults    = 8
)

type channelSearchTool struct {
	processor                    *Processor
	guildID, channelID, beforeID string
}

type channelSearchResponse struct {
	Query            string                 `json:"query"`
	Results          []channelSearchMessage `json:"results"`
	SearchedMessages int                    `json:"searchedMessages"`
	Truncated        bool                   `json:"truncated"`
}

type channelSearchMessage struct {
	ID        string `json:"id"`
	Author    string `json:"author"`
	Content   string `json:"content"`
	Timestamp string `json:"timestamp,omitempty"`
	URL       string `json:"url"`
}

func (p *Processor) searchCurrentChannel(guildID, channelID, beforeID string) genai.FunctionTool {
	return channelSearchTool{processor: p, guildID: guildID, channelID: channelID, beforeID: beforeID}
}

func (channelSearchTool) Name() string { return channelSearchToolName }

func (channelSearchTool) Declaration() *googlegenai.FunctionDeclaration {
	return &googlegenai.FunctionDeclaration{
		Name:        channelSearchToolName,
		Description: "Search recent messages in the current Discord channel. Use this for questions about what was said earlier in this channel.",
		Parameters: &googlegenai.Schema{Type: googlegenai.TypeObject, Properties: map[string]*googlegenai.Schema{
			"query": {Type: googlegenai.TypeString, Description: "Case-insensitive text to find in recent channel messages."},
		}, Required: []string{"query"}},
	}
}

func (t channelSearchTool) Execute(ctx context.Context, args map[string]any) (any, error) {
	query, ok := args["query"].(string)
	if !ok {
		return nil, errors.New("query must be a string")
	}
	return t.processor.searchChannel(ctx, t.guildID, t.channelID, t.beforeID, query)
}

// Evidence records that current-channel history was successfully searched.
func (channelSearchTool) Evidence(any) (genai.Evidence, bool) {
	return genai.Evidence{Kind: genai.EvidenceKindChannelHistory, Tool: channelSearchToolName}, true
}

func (p *Processor) searchChannel(ctx context.Context, guildID, channelID, beforeID, query string) (channelSearchResponse, error) {
	query = strings.TrimSpace(query)
	if query == "" {
		return channelSearchResponse{}, errors.New("query is required")
	}
	if channelID == "" {
		return channelSearchResponse{}, errors.New("channel search is unavailable")
	}

	needle := strings.ToLower(query)
	resp := channelSearchResponse{Query: query}
	for beforeID != "" && resp.SearchedMessages < channelSearchMessages && len(resp.Results) < channelSearchResults {
		select {
		case <-ctx.Done():
			return channelSearchResponse{}, ctx.Err()
		default:
		}
		pageLimit := min(discordMessagePageLimit, channelSearchMessages-resp.SearchedMessages)
		messages, err := p.client.Messages(ctx, channelID, pageLimit, beforeID)
		if err != nil {
			return channelSearchResponse{}, errors.Wrap(err, "fetch channel messages")
		}
		if len(messages) == 0 {
			break
		}
		for _, message := range messages {
			resp.SearchedMessages++
			if match := searchResult(guildID, channelID, p.botID, message, needle); match != nil {
				resp.Results = append(resp.Results, *match)
				if len(resp.Results) == channelSearchResults {
					break
				}
			}
		}
		beforeID = messages[len(messages)-1].ID
	}
	resp.Truncated = resp.SearchedMessages >= channelSearchMessages || len(resp.Results) >= channelSearchResults
	slices.Reverse(resp.Results)
	return resp, nil
}

func searchResult(guildID, channelID, botID string, message *discordgo.Message, needle string) *channelSearchMessage {
	if message == nil || message.Author == nil {
		return nil
	}
	content := sanitizeContent(message.Content, botID)
	if content == "" || !strings.Contains(strings.ToLower(content), needle) {
		return nil
	}
	result := channelSearchMessage{
		ID:      message.ID,
		Author:  displayName(message.Author),
		Content: content,
		URL:     discordMessageURL(guildID, channelID, message.ID),
	}
	if !message.Timestamp.IsZero() {
		result.Timestamp = message.Timestamp.UTC().Format(time.RFC3339)
	}
	return &result
}

func discordMessageURL(guildID, channelID, messageID string) string {
	if guildID == "" {
		guildID = "@me"
	}
	return "https://discord.com/channels/" + guildID + "/" + channelID + "/" + messageID
}
