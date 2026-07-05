package discord

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/stretchr/testify/assert"
)

func TestStripBotPrefix(t *testing.T) {
	t.Run("removes single prefix", func(t *testing.T) {
		assert.Equal(t, "hello", stripBotPrefix("jarvis: hello"))
	})

	t.Run("removes stacked prefixes", func(t *testing.T) {
		assert.Equal(t, "hi", stripBotPrefix("jarvis: jarvis: hi"))
	})

	t.Run("ignores case and spacing", func(t *testing.T) {
		assert.Equal(t, "hey", stripBotPrefix("  JarvisChat -  hey"))
	})

	t.Run("no prefix leaves text", func(t *testing.T) {
		assert.Equal(t, "hello", stripBotPrefix("hello"))
	})

	t.Run("only prefix results in empty", func(t *testing.T) {
		assert.Equal(t, "", stripBotPrefix("jarvis: "))
	})
}

func TestParseWebInvocation(t *testing.T) {
	t.Run("detects front token", func(t *testing.T) {
		useWeb, query := parseWebInvocation("#web what happened today", "#web")
		assert.True(t, useWeb)
		assert.Equal(t, "what happened today", query)
	})

	t.Run("allows leading whitespace before front token", func(t *testing.T) {
		useWeb, query := parseWebInvocation("   #web   latest golang release", "#web")
		assert.True(t, useWeb)
		assert.Equal(t, "latest golang release", query)
	})

	t.Run("ignores marker in middle of request", func(t *testing.T) {
		useWeb, query := parseWebInvocation("tell me #web updates", "#web")
		assert.False(t, useWeb)
		assert.Equal(t, "tell me #web updates", query)
	})

	t.Run("requires token boundary", func(t *testing.T) {
		useWeb, query := parseWebInvocation("#webbing test", "#web")
		assert.False(t, useWeb)
		assert.Equal(t, "#webbing test", query)
	})
}

func TestMessageTimeoutFor(t *testing.T) {
	bot := &Bot{
		botID: "bot",
		web: webGroundingState{
			enabled:        true,
			indicator:      "#web",
			messageTimeout: 75 * time.Second,
		},
	}

	t.Run("uses longer timeout for sanitized web messages", func(t *testing.T) {
		timeout := bot.messageTimeoutFor(&discordgo.MessageCreate{
			Message: &discordgo.Message{
				Content: "<@bot> #web latest Vertex AI grounding docs",
			},
		})
		assert.Equal(t, 75*time.Second, timeout)
	})

	t.Run("keeps normal timeout for non-web messages", func(t *testing.T) {
		timeout := bot.messageTimeoutFor(&discordgo.MessageCreate{
			Message: &discordgo.Message{
				Content: "<@bot> explain this code",
			},
		})
		assert.Equal(t, 30*time.Second, timeout)
	})
}

func TestPrepareGenerateRequestWebOptions(t *testing.T) {
	bot := &Bot{
		botID: "bot",
		web: normalizeWebGroundingState(WebGroundingConfig{
			Enabled:                 true,
			Indicator:               "#web",
			Timeout:                 45 * time.Second,
			AttemptTimeout:          20 * time.Second,
			AddendumMaxOutputTokens: 512,
			APIVersion:              "v1",
			MaxResults:              2,
			BudgetRatio:             0.25,
			RequireCitations:        true,
			DisableAllowlists:       true,
			UserRPM:                 5,
			GlobalRPM:               5,
		}),
	}
	channel := &discordgo.Channel{
		ID:   "channel-1",
		Type: discordgo.ChannelTypeGuildText,
	}
	message := &discordgo.MessageCreate{
		Message: &discordgo.Message{
			ID:        "msg-1",
			ChannelID: "channel-1",
			Content:   "<@bot> #web latest Gemini grounding status",
			Author:    &discordgo.User{ID: "user-1"},
		},
	}

	req, err := bot.prepareGenerateRequest(channel, message)
	assert.NoError(t, err)
	assert.True(t, req.Options.UseWebGrounding)
	assert.Equal(t, float32(0.25), req.Options.GroundingBudgetRatio)
	assert.Equal(t, 45*time.Second, req.Options.WebGroundingTimeout)
	assert.Equal(t, 20*time.Second, req.Options.WebGroundingAttemptTimeout)
	assert.Equal(t, 512, req.Options.WebGroundingAddendumMaxOutputTokens)
	assert.Equal(t, "v1", req.Options.WebGroundingAPIVersion)
	assert.Equal(t, 2, req.Options.WebGroundingMaxResults)
	assert.True(t, req.Options.RequireCitations)
}

func TestCanUseWebGrounding(t *testing.T) {
	now := time.Date(2026, 2, 15, 10, 0, 0, 0, time.UTC)
	bot := &Bot{
		web: webGroundingState{
			enabled:          true,
			indicator:        "#web",
			channelAllowlist: map[string]struct{}{"allowed": {}},
			roleAllowlist:    map[string]struct{}{"role-1": {}},
			limiter:          newWebRateLimiter(1, 2, func() time.Time { return now }),
		},
	}

	channel := &discordgo.Channel{ID: "allowed"}
	message := &discordgo.MessageCreate{
		Message: &discordgo.Message{
			Author: &discordgo.User{ID: "user-1"},
			Member: &discordgo.Member{Roles: []string{"role-1"}},
		},
	}

	t.Run("allows first request", func(t *testing.T) {
		allowed, reason := bot.canUseWebGrounding(channel, message)
		assert.True(t, allowed)
		assert.Equal(t, "", reason)
	})

	t.Run("denies second request due to user rate limit", func(t *testing.T) {
		allowed, reason := bot.canUseWebGrounding(channel, message)
		assert.False(t, allowed)
		assert.Contains(t, reason, "limit")
	})

	t.Run("denies non-allowlisted channel", func(t *testing.T) {
		now = now.Add(time.Minute)
		allowed, reason := bot.canUseWebGrounding(&discordgo.Channel{ID: "other"}, message)
		assert.False(t, allowed)
		assert.Contains(t, reason, "channel")
	})

	t.Run("denies when roles are missing", func(t *testing.T) {
		now = now.Add(time.Minute)
		noRoleMessage := &discordgo.MessageCreate{
			Message: &discordgo.Message{
				Author: &discordgo.User{ID: "user-2"},
				Member: &discordgo.Member{Roles: []string{"role-2"}},
			},
		}

		allowed, reason := bot.canUseWebGrounding(channel, noRoleMessage)
		assert.False(t, allowed)
		assert.Contains(t, reason, "role")
	})

	t.Run("bypasses allowlists when disabled", func(t *testing.T) {
		now = now.Add(time.Minute)
		bypass := &Bot{
			web: webGroundingState{
				enabled:           true,
				disableAllowlists: true,
				channelAllowlist:  map[string]struct{}{"allowed": {}},
				roleAllowlist:     map[string]struct{}{"role-1": {}},
				limiter:           newWebRateLimiter(1, 2, func() time.Time { return now }),
			},
		}

		unlistedChannel := &discordgo.Channel{ID: "unlisted"}
		unlistedRoleMessage := &discordgo.MessageCreate{
			Message: &discordgo.Message{
				Author: &discordgo.User{ID: "user-3"},
				Member: &discordgo.Member{Roles: []string{"role-9"}},
			},
		}

		allowed, reason := bypass.canUseWebGrounding(unlistedChannel, unlistedRoleMessage)
		assert.True(t, allowed)
		assert.Equal(t, "", reason)
	})
}

func TestFormatTranscript(t *testing.T) {
	b := &Bot{botID: "bot"}

	t.Run("formats basic conversation", func(t *testing.T) {
		messages := []*discordgo.Message{
			{
				Author:  &discordgo.User{ID: "u1", GlobalName: "Alice"},
				Content: "Hello",
			},
			{
				Author:  &discordgo.User{ID: "bot", Username: "jarvis"},
				Content: "Hi there",
			},
		}

		transcript := b.formatTranscript(messages)
		expected := "Alice: Hello\nJarvis: Hi there\n"
		assert.Equal(t, expected, transcript)
	})

	t.Run("sanitizes content in transcript", func(t *testing.T) {
		messages := []*discordgo.Message{
			{
				Author:  &discordgo.User{ID: "u1", Username: "Bob"},
				Content: "Look at <#123>",
			},
		}

		transcript := b.formatTranscript(messages)
		expected := "Bob: Look at this channel\n"
		assert.Equal(t, expected, transcript)
	})

	t.Run("handles empty or bot-only mentions gracefully", func(t *testing.T) {
		messages := []*discordgo.Message{
			{
				Author:  &discordgo.User{ID: "u1", Username: "Charlie"},
				Content: "<@bot>", // Should be empty after sanitize
			},
			{
				Author:  &discordgo.User{ID: "u1", Username: "Charlie"},
				Content: "Real message",
			},
		}

		transcript := b.formatTranscript(messages)
		expected := "Charlie: Real message\n"
		assert.Equal(t, expected, transcript)
	})
}

func TestSanitizeContent(t *testing.T) {
	botID := "bot"

	t.Run("removes bot mentions", func(t *testing.T) {
		assert.Equal(t, "hello", sanitizeContent("<@bot> hello", botID))
		assert.Equal(t, "hello", sanitizeContent("<@!bot> hello", botID))
	})

	t.Run("replaces channel mentions", func(t *testing.T) {
		assert.Equal(t, "look at this channel", sanitizeContent("look at <#12345>", botID))
	})

	t.Run("unescapes html", func(t *testing.T) {
		assert.Equal(t, "don't", sanitizeContent("don&#39;t", botID))
	})

	t.Run("trims whitespace", func(t *testing.T) {
		assert.Equal(t, "clean", sanitizeContent("   clean   ", botID))
	})
}

func TestBuildPrompt(t *testing.T) {
	t.Run("returns errEmptyMessageContent for bot-only mention", func(t *testing.T) {
		b := &Bot{botID: "123456789"}
		channel := &discordgo.Channel{
			Type: discordgo.ChannelTypeGuildText,
		}
		m := &discordgo.MessageCreate{
			Message: &discordgo.Message{
				Content: "<@123456789>",
				Author:  &discordgo.User{ID: "user1", Username: "TestUser"},
			},
		}

		prompt, err := b.buildPrompt(channel, m)
		assert.Nil(t, prompt)
		assert.ErrorIs(t, err, errEmptyMessageContent)
	})

	t.Run("returns errEmptyMessageContent for whitespace-only content", func(t *testing.T) {
		b := &Bot{botID: "123456789"}
		channel := &discordgo.Channel{
			Type: discordgo.ChannelTypeGuildText,
		}
		m := &discordgo.MessageCreate{
			Message: &discordgo.Message{
				Content: "   ",
				Author:  &discordgo.User{ID: "user1", Username: "TestUser"},
			},
		}

		prompt, err := b.buildPrompt(channel, m)
		assert.Nil(t, prompt)
		assert.ErrorIs(t, err, errEmptyMessageContent)
	})

	t.Run("returns prompt for valid content", func(t *testing.T) {
		b := &Bot{botID: "123456789"}
		channel := &discordgo.Channel{
			Type: discordgo.ChannelTypeGuildText,
		}
		m := &discordgo.MessageCreate{
			Message: &discordgo.Message{
				Content: "<@123456789> Hello, how are you?",
				Author:  &discordgo.User{ID: "user1", Username: "TestUser"},
			},
		}

		prompt, err := b.buildPrompt(channel, m)
		assert.NoError(t, err)
		assert.Len(t, prompt, 1)
		assert.Equal(t, "user", prompt[0].Role)
		assert.Contains(t, prompt[0].Content, "Hello, how are you?")
	})
}

func TestBuildPromptContextSections(t *testing.T) {
	t.Run("thread prompt separates thread and parent contexts", func(t *testing.T) {
		b := &Bot{
			botID: "bot",
			fetchMessages: func(channelID string, limit int, beforeID string) ([]*discordgo.Message, error) {
				switch channelID {
				case "thread-1":
					return []*discordgo.Message{
						{Author: &discordgo.User{ID: "u1", Username: "alice"}, Content: "thread context message"},
					}, nil
				case "parent-1":
					return []*discordgo.Message{
						{Author: &discordgo.User{ID: "u2", Username: "bob"}, Content: "parent context message"},
					}, nil
				default:
					return nil, nil
				}
			},
		}

		channel := &discordgo.Channel{
			ID:       "thread-1",
			ParentID: "parent-1",
			Type:     discordgo.ChannelTypeGuildPublicThread,
		}
		m := &discordgo.MessageCreate{
			Message: &discordgo.Message{
				ID:        "msg-1",
				ChannelID: "thread-1",
				Author:    &discordgo.User{ID: "u3", Username: "requester"},
			},
		}

		prompt, err := b.buildPromptFromCurrent(channel, m, "what is next?")
		assert.NoError(t, err)
		assert.Len(t, prompt, 1)
		assert.Contains(t, prompt[0].Content, "THREAD HISTORY CONTEXT:")
		assert.Contains(t, prompt[0].Content, "PARENT CHANNEL CONTEXT:")
		assert.Contains(t, prompt[0].Content, "CURRENT REQUEST (PRIMARY TASK):")
		assert.Contains(t, prompt[0].Content, "alice: thread context message")
		assert.Contains(t, prompt[0].Content, "bob: parent context message")
	})

	t.Run("channel prompt uses channel history context", func(t *testing.T) {
		b := &Bot{
			botID: "bot",
			fetchMessages: func(channelID string, limit int, beforeID string) ([]*discordgo.Message, error) {
				return []*discordgo.Message{
					{Author: &discordgo.User{ID: "u1", Username: "alice"}, Content: "channel history message"},
				}, nil
			},
		}

		channel := &discordgo.Channel{
			ID:   "channel-1",
			Type: discordgo.ChannelTypeGuildText,
		}
		m := &discordgo.MessageCreate{
			Message: &discordgo.Message{
				ID:        "msg-2",
				ChannelID: "channel-1",
				Author:    &discordgo.User{ID: "u2", Username: "requester"},
			},
		}

		prompt, err := b.buildPromptFromCurrent(channel, m, "summarize please")
		assert.NoError(t, err)
		assert.Len(t, prompt, 1)
		assert.Contains(t, prompt[0].Content, "CHANNEL HISTORY CONTEXT:")
		assert.Contains(t, prompt[0].Content, "CURRENT REQUEST (PRIMARY TASK):")
		assert.Contains(t, prompt[0].Content, "alice: channel history message")
	})
}

func TestSplitMessageForDiscord(t *testing.T) {
	t.Run("returns single chunk when content is short", func(t *testing.T) {
		chunks := splitMessageForDiscord("hello", discordMessageMaxLength)
		assert.Equal(t, []string{"hello"}, chunks)
	})

	t.Run("splits long content into valid chunks", func(t *testing.T) {
		content := strings.Repeat("a", 4500)
		chunks := splitMessageForDiscord(content, discordMessageMaxLength)
		assert.Len(t, chunks, 3)
		assert.Equal(t, content, strings.Join(chunks, ""))
		for _, chunk := range chunks {
			assert.LessOrEqual(t, len([]rune(chunk)), discordMessageMaxLength)
		}
	})
}

func TestSendMessageChunks(t *testing.T) {
	t.Run("sends all chunks when content exceeds limit", func(t *testing.T) {
		var sent []string
		bot := &Bot{
			sendMessage: func(channelID, content string) (*discordgo.Message, error) {
				sent = append(sent, content)
				return &discordgo.Message{ChannelID: channelID, Content: content}, nil
			},
		}

		content := strings.Repeat("b", 4100)
		err := bot.sendMessageChunks("channel-1", content)
		assert.NoError(t, err)
		assert.Len(t, sent, 3)
		assert.Equal(t, content, strings.Join(sent, ""))
	})

	t.Run("returns error when send fails on a chunk", func(t *testing.T) {
		bot := &Bot{
			sendMessage: func(channelID, content string) (*discordgo.Message, error) {
				return nil, errors.New("send failed")
			},
		}

		err := bot.sendMessageChunks("channel-1", strings.Repeat("x", 2100))
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "failed to send chunk")
	})
}

func TestSendReplyFallback(t *testing.T) {
	t.Run("falls back to channel when thread creation fails", func(t *testing.T) {
		var sentChannelIDs []string
		bot := &Bot{
			sendMessage: func(channelID, content string) (*discordgo.Message, error) {
				sentChannelIDs = append(sentChannelIDs, channelID)
				return &discordgo.Message{ChannelID: channelID, Content: content}, nil
			},
			startThread: func(channelID, messageID, name string, autoArchiveDuration int) (*discordgo.Channel, error) {
				return nil, errors.New("thread create failed")
			},
		}

		channel := &discordgo.Channel{
			ID:   "parent-1",
			Type: discordgo.ChannelTypeGuildText,
		}
		msg := &discordgo.MessageCreate{
			Message: &discordgo.Message{
				ID:        "msg-1",
				ChannelID: "parent-1",
				Author:    &discordgo.User{Username: "alice"},
			},
		}

		threadID, err := bot.sendReply(nil, channel, msg, "hello from bot")
		assert.NoError(t, err)
		assert.Equal(t, "", threadID)
		assert.Equal(t, []string{"parent-1"}, sentChannelIDs)
	})
}

func TestThreadGroundingMemoryFlow(t *testing.T) {
	t.Run("blocks second #web call in same thread", func(t *testing.T) {
		bot := &Bot{
			botID: "bot",
			web: webGroundingState{
				enabled:   true,
				indicator: "#web",
			},
			grounding: map[string]threadGroundingMemory{
				"thread-1": {
					content:   "Web-grounded addendum (30% budget):\nFact [1]\n\nSources:\n[1] https://example.com",
					updatedAt: time.Now(),
				},
			},
		}

		channel := &discordgo.Channel{
			ID:   "thread-1",
			Type: discordgo.ChannelTypeGuildPublicThread,
		}
		m := &discordgo.MessageCreate{
			Message: &discordgo.Message{
				ChannelID: "thread-1",
				Content:   "#web another lookup",
				Author:    &discordgo.User{ID: "user-1"},
			},
		}

		_, err := bot.prepareGenerateRequest(channel, m)
		assert.Error(t, err)
		var deniedErr webGroundingDeniedError
		assert.ErrorAs(t, err, &deniedErr)
		assert.Contains(t, deniedErr.reason, "already used")
	})

	t.Run("injects stored web grounding context into prompt", func(t *testing.T) {
		bot := &Bot{
			botID: "bot",
			grounding: map[string]threadGroundingMemory{
				"thread-2": {
					content:   "Web-grounded addendum (30% budget):\nImportant grounded fact [1]\n\nSources:\n[1] https://example.com",
					updatedAt: time.Now(),
				},
			},
		}
		channel := &discordgo.Channel{
			ID:   "thread-2",
			Type: discordgo.ChannelTypeGuildPublicThread,
		}
		m := &discordgo.MessageCreate{
			Message: &discordgo.Message{
				ChannelID: "thread-2",
				Author:    &discordgo.User{ID: "user-1"},
			},
		}

		prompt, err := bot.buildPromptFromCurrent(channel, m, "follow-up question")
		assert.NoError(t, err)
		assert.Len(t, prompt, 1)
		assert.Contains(t, prompt[0].Content, "THREAD WEB GROUNDING CONTEXT:")
		assert.Contains(t, prompt[0].Content, "Important grounded fact")
	})

	t.Run("blocks #web when prior grounded response is found in thread history", func(t *testing.T) {
		bot := &Bot{
			botID: "bot",
			web: webGroundingState{
				enabled:   true,
				indicator: "#web",
			},
			fetchMessages: func(channelID string, limit int, beforeID string) ([]*discordgo.Message, error) {
				return []*discordgo.Message{
					{
						ID:      "older-1",
						Author:  &discordgo.User{ID: "bot"},
						Content: "Web-grounded addendum (30% budget): fact [1]",
					},
				}, nil
			},
		}

		channel := &discordgo.Channel{
			ID:   "thread-3",
			Type: discordgo.ChannelTypeGuildPublicThread,
		}
		m := &discordgo.MessageCreate{
			Message: &discordgo.Message{
				ID:        "new-msg",
				ChannelID: "thread-3",
				Content:   "#web another lookup",
				Author:    &discordgo.User{ID: "user-1"},
			},
		}

		_, err := bot.prepareGenerateRequest(channel, m)
		assert.Error(t, err)
		var deniedErr webGroundingDeniedError
		assert.ErrorAs(t, err, &deniedErr)
		assert.Contains(t, deniedErr.reason, "already used")
	})
}

func TestExtractThreadGroundingMemory(t *testing.T) {
	t.Run("stores addendum section when marker exists", func(t *testing.T) {
		reply := "Base response text\n\nWeb-grounded addendum (30% budget):\nGrounded details [1]\n\nSources:\n[1] https://example.com"
		memory := extractThreadGroundingMemory(reply)
		assert.True(t, strings.HasPrefix(memory, "Web-grounded addendum (30% budget):"))
		assert.Contains(t, memory, "Sources:")
	})
}
