package discord

import (
	"context"
	"slices"
	"strings"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/justinswe/jarvis/pkg/genai"
	"github.com/justinswe/std/app"
	"github.com/justinswe/std/errors"
	"go.uber.org/zap"
	googlegenai "google.golang.org/genai"
)

const (
	channelSearchPageSize = 100
	channelSearchResults  = 8
	channelSearchToolName = genai.ChannelSearchFunctionName
)

type channelSearchTool struct {
	processor                    *Processor
	guildID, channelID, beforeID string
}

type channelSearchCriteria struct {
	query, author string
	start, end    *time.Time
}

type channelSearchResponse struct {
	Query            string                 `json:"query,omitempty"`
	Author           string                 `json:"author,omitempty"`
	StartTime        string                 `json:"startTime,omitempty"`
	EndTime          string                 `json:"endTime,omitempty"`
	Results          []channelSearchMessage `json:"results"`
	SearchedMessages int                    `json:"searchedMessages"`
	Truncated        bool                   `json:"truncated"`
	Incomplete       bool                   `json:"incomplete"`
}

type channelSearchMessage struct {
	ID        string `json:"id"`
	AuthorID  string `json:"authorId"`
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
		Name: channelSearchToolName,
		Description: "Search stored, unexpired text messages in the current Discord channel. " +
			"Use this for questions about earlier channel messages, never as a substitute for web search. " +
			"Provide at least one text, author, or time criterion. Results contain the newest eight matches in chronological order.",
		Parameters: &googlegenai.Schema{Type: googlegenai.TypeObject, Properties: map[string]*googlegenai.Schema{
			"query": {
				Type: googlegenai.TypeString, Description: "Optional case-insensitive text contained in the message.",
			},
			"author": {
				Type: googlegenai.TypeString, Description: "Optional exact Discord user ID, mention, username, or display name.",
			},
			"start_time": {
				Type: googlegenai.TypeString, Description: "Optional inclusive RFC3339 message timestamp.",
			},
			"end_time": {
				Type: googlegenai.TypeString, Description: "Optional exclusive RFC3339 message timestamp.",
			},
		}},
	}
}

func (t channelSearchTool) Execute(ctx context.Context, args map[string]any) (any, error) {
	criteria, err := parseChannelSearchCriteria(args)
	if err != nil {
		return nil, genai.NewExecutionError("invalid_search", err.Error(), err)
	}
	return t.processor.searchChannel(ctx, t.guildID, t.channelID, t.beforeID, criteria)
}

// Evidence records that current-channel history was successfully searched.
func (channelSearchTool) Evidence(output any) (genai.Evidence, bool) {
	_, ok := output.(channelSearchResponse)
	return genai.Evidence{Kind: genai.EvidenceKindChannelHistory, Tool: channelSearchToolName}, ok
}

func parseChannelSearchCriteria(args map[string]any) (channelSearchCriteria, error) {
	var criteria channelSearchCriteria
	for name, value := range args {
		var err error
		switch name {
		case "query":
			criteria.query, err = searchStringArgument(value, "query")
		case "author":
			criteria.author, err = searchStringArgument(value, "author")
		case "start_time":
			criteria.start, err = searchTimeArgument(value, "start_time")
		case "end_time":
			criteria.end, err = searchTimeArgument(value, "end_time")
		default:
			return channelSearchCriteria{}, errors.New("search contains an unsupported field")
		}
		if err != nil {
			return channelSearchCriteria{}, err
		}
	}
	if criteria.query == "" && criteria.author == "" && criteria.start == nil && criteria.end == nil {
		return channelSearchCriteria{}, errors.New("at least one search criterion is required")
	}
	if criteria.start != nil && criteria.end != nil && !criteria.start.Before(*criteria.end) {
		return channelSearchCriteria{}, errors.New("start_time must be before end_time")
	}
	return criteria, nil
}

func searchStringArgument(value any, name string) (string, error) {
	result, ok := value.(string)
	if !ok {
		return "", errors.Errorf("%s must be a string", name)
	}
	return strings.TrimSpace(result), nil
}

func searchTimeArgument(value any, name string) (*time.Time, error) {
	raw, err := searchStringArgument(value, name)
	if err != nil {
		return nil, err
	}
	if raw == "" {
		return nil, nil
	}
	parsed, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		return nil, errors.Errorf("%s must be an RFC3339 timestamp", name)
	}
	parsed = parsed.UTC()
	return &parsed, nil
}

func (p *Processor) searchChannel(ctx context.Context, guildID, channelID, beforeID string, criteria channelSearchCriteria) (channelSearchResponse, error) {
	if channelID == "" || p.history == nil {
		return channelSearchResponse{}, genai.NewExecutionError(
			"channel_search_unavailable", "Stored channel history could not be searched.", nil,
		)
	}
	response := responseForCriteria(criteria)
	started := time.Now()
	for {
		select {
		case <-ctx.Done():
			return channelSearchResponse{}, ctx.Err()
		default:
		}

		messages, readErr := p.history.Messages(ctx, guildID, channelID, channelSearchPageSize, beforeID)
		if ctx.Err() != nil {
			return channelSearchResponse{}, ctx.Err()
		}
		if len(messages) == 0 {
			if readErr != nil {
				return channelSearchResponse{}, genai.NewExecutionError(
					"channel_search_unavailable", "Stored channel history could not be searched.", readErr,
				)
			}
			break
		}
		if readErr != nil {
			response.Incomplete = true
		}

		reachedStart := false
		for _, message := range messages {
			if message == nil {
				continue
			}
			response.SearchedMessages++
			if criteria.start != nil && !message.Timestamp.IsZero() && message.Timestamp.Before(*criteria.start) {
				reachedStart = true
				break
			}
			if result := searchResult(guildID, channelID, p.botID, message, criteria); result != nil {
				response.Results = append(response.Results, *result)
				if len(response.Results) == channelSearchResults {
					response.Truncated = true
					break
				}
			}
		}
		if response.Truncated || response.Incomplete || reachedStart {
			break
		}

		nextBeforeID := oldestMessageID(messages)
		if nextBeforeID == "" || nextBeforeID == beforeID {
			if response.SearchedMessages == 0 {
				return channelSearchResponse{}, genai.NewExecutionError(
					"channel_search_unavailable", "Stored channel history could not be searched.", nil,
				)
			}
			response.Incomplete = true
			break
		}
		beforeID = nextBeforeID
	}
	slices.Reverse(response.Results)
	app.L().Info("Stored channel search completed",
		zap.String("guild_id", guildID),
		zap.String("channel_id", channelID),
		zap.Duration("duration", time.Since(started)),
		zap.Bool("query_filter", criteria.query != ""),
		zap.Bool("author_filter", criteria.author != ""),
		zap.Bool("start_time_filter", criteria.start != nil),
		zap.Bool("end_time_filter", criteria.end != nil),
		zap.Int("searched_message_count", response.SearchedMessages),
		zap.Int("result_count", len(response.Results)),
		zap.Bool("truncated", response.Truncated),
		zap.Bool("incomplete", response.Incomplete),
	)
	return response, nil
}

func responseForCriteria(criteria channelSearchCriteria) channelSearchResponse {
	response := channelSearchResponse{Query: criteria.query, Author: criteria.author}
	if criteria.start != nil {
		response.StartTime = criteria.start.Format(time.RFC3339)
	}
	if criteria.end != nil {
		response.EndTime = criteria.end.Format(time.RFC3339)
	}
	return response
}

func oldestMessageID(messages []*discordgo.Message) string {
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i] != nil && messages[i].ID != "" {
			return messages[i].ID
		}
	}
	return ""
}

func searchResult(guildID, channelID, botID string, message *discordgo.Message, criteria channelSearchCriteria) *channelSearchMessage {
	if message == nil || message.ID == "" || message.Author == nil || !searchAuthorMatches(message.Author, criteria.author) {
		return nil
	}
	if criteria.start != nil || criteria.end != nil {
		if message.Timestamp.IsZero() || (criteria.start != nil && message.Timestamp.Before(*criteria.start)) ||
			(criteria.end != nil && !message.Timestamp.Before(*criteria.end)) {
			return nil
		}
	}
	content := sanitizeContent(message.Content, botID)
	if content == "" || (criteria.query != "" && !strings.Contains(strings.ToLower(content), strings.ToLower(criteria.query))) {
		return nil
	}
	result := channelSearchMessage{
		ID:       message.ID,
		AuthorID: message.Author.ID,
		Author:   displayName(message.Author),
		Content:  content,
		URL:      discordMessageURL(guildID, channelID, message.ID),
	}
	if !message.Timestamp.IsZero() {
		result.Timestamp = message.Timestamp.UTC().Format(time.RFC3339)
	}
	return &result
}

func searchAuthorMatches(author *discordgo.User, criterion string) bool {
	if criterion == "" {
		return true
	}
	criterion = strings.TrimSpace(criterion)
	if strings.HasPrefix(criterion, "<@") && strings.HasSuffix(criterion, ">") {
		criterion = strings.TrimSuffix(strings.TrimPrefix(criterion, "<@"), ">")
		criterion = strings.TrimPrefix(criterion, "!")
	}
	return author.ID == criterion || strings.EqualFold(author.Username, criterion) ||
		strings.EqualFold(author.GlobalName, criterion) || strings.EqualFold(displayName(author), criterion)
}

func discordMessageURL(guildID, channelID, messageID string) string {
	if guildID == "" {
		guildID = "@me"
	}
	return "https://discord.com/channels/" + guildID + "/" + channelID + "/" + messageID
}
