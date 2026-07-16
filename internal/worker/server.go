// Package worker exposes stateless Discord message processing over HTTP.
package worker

import (
	"context"
	"io"
	"mime"
	"net/http"
	"time"

	"github.com/bwmarrin/discordgo"
	discordv1 "github.com/justinswe/jarvis/api/jarvis/discord/v1"
	"github.com/justinswe/std/app"
	"github.com/justinswe/std/errors"
	"go.uber.org/zap"
	"google.golang.org/protobuf/proto"
)

const (
	gracefulStopTimeout   = 5 * time.Second
	idleConnectionTimeout = 60 * time.Second
	maxRequestSize        = 10 << 20
	processPath           = "/v1/messages:process"
	requestReadTimeout    = 10 * time.Second
	requestHeaderTimeout  = 5 * time.Second
)

// Processor handles a normalized Discord message request.
type Processor interface {
	Process(context.Context, *discordgo.MessageCreate) error
}

// Recorder persists one validated normalized Discord message before processing.
type Recorder interface {
	Record(context.Context, *discordv1.DiscordMessageCreateEvent) error
}

// NewHandler returns the HTTP interface used by direct callers and Pub/Sub push.
func NewHandler(processor Processor, recorders ...Recorder) http.Handler {
	var recorder Recorder
	if len(recorders) > 0 {
		recorder = recorders[0]
	}
	mux := http.NewServeMux()
	mux.HandleFunc(processPath, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", http.MethodPost)
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		handleMessage(w, r, processor, recorder)
	})
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", http.MethodGet)
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", http.MethodGet)
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		w.WriteHeader(http.StatusOK)
	})
	return mux
}

func handleMessage(w http.ResponseWriter, r *http.Request, processor Processor, recorder Recorder) {
	if !supportedContentType(r.Header.Get("Content-Type")) {
		http.Error(w, "unsupported content type", http.StatusUnsupportedMediaType)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxRequestSize)
	body, err := io.ReadAll(r.Body)
	if err != nil {
		var sizeErr *http.MaxBytesError
		if errors.As(err, &sizeErr) {
			http.Error(w, "request body too large", http.StatusRequestEntityTooLarge)
			return
		}
		http.Error(w, "read request body", http.StatusBadRequest)
		return
	}
	request := &discordv1.IngestMessageRequest{}
	if err := proto.Unmarshal(body, request); err != nil {
		http.Error(w, "invalid protobuf message", http.StatusBadRequest)
		return
	}
	message, err := discordMessage(request)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if recorder != nil {
		if err := recorder.Record(r.Context(), request.Event); err != nil {
			app.L().Warn("Discord message recording failed",
				zap.String("guild_id", message.GuildID),
				zap.String("channel_id", message.ChannelID),
				zap.String("message_id", message.ID),
				zap.Error(err),
			)
		}
	}
	if err := processor.Process(r.Context(), message); err != nil {
		app.L().Warn("Discord message processing failed",
			zap.String("guild_id", message.GuildID),
			zap.String("channel_id", message.ChannelID),
			zap.String("message_id", message.ID),
			zap.Error(err),
		)
		http.Error(w, "message processing failed", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func supportedContentType(value string) bool {
	if value == "" {
		return true
	}
	mediaType, _, err := mime.ParseMediaType(value)
	if err != nil {
		return false
	}
	return mediaType == "application/x-protobuf" || mediaType == "application/octet-stream"
}

func discordMessage(req *discordv1.IngestMessageRequest) (*discordgo.MessageCreate, error) {
	if req == nil || req.Event == nil {
		return nil, errors.New("event is required")
	}
	event := req.Event
	if event.MessageId == "" {
		return nil, errors.New("message_id is required")
	}
	if event.ChannelId == "" {
		return nil, errors.New("channel_id is required")
	}
	if event.Author == nil || event.Author.Id == "" {
		return nil, errors.New("author.id is required")
	}

	messageType := discordgo.MessageType(-1)
	switch event.Kind {
	case discordv1.MessageKind_MESSAGE_KIND_DEFAULT:
		messageType = discordgo.MessageTypeDefault
	case discordv1.MessageKind_MESSAGE_KIND_REPLY:
		messageType = discordgo.MessageTypeReply
	}
	message := &discordgo.Message{
		ID:        event.MessageId,
		GuildID:   event.GuildId,
		ChannelID: event.ChannelId,
		Content:   event.Content,
		Type:      messageType,
		Author: &discordgo.User{
			ID:         event.Author.Id,
			Username:   event.Author.Username,
			GlobalName: event.Author.GlobalName,
			Bot:        event.Author.Bot,
		},
	}
	for _, userID := range event.MentionedUserIds {
		if userID != "" {
			message.Mentions = append(message.Mentions, &discordgo.User{ID: userID})
		}
	}
	if event.Reference != nil {
		message.MessageReference = &discordgo.MessageReference{
			MessageID: event.Reference.MessageId,
			ChannelID: event.Reference.ChannelId,
			GuildID:   event.GuildId,
		}
	}
	return &discordgo.MessageCreate{Message: message}, nil
}

// Serve runs the HTTP worker until its context ends or the server fails.
func Serve(ctx context.Context, address string, processor Processor, recorders ...Recorder) error {
	if address == "" {
		return errors.New("worker address is required")
	}
	server := newHTTPServer(address, processor, recorders...)
	done := make(chan error, 1)
	go func() {
		err := server.ListenAndServe()
		if errors.Is(err, http.ErrServerClosed) {
			err = nil
		}
		done <- err
	}()

	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), gracefulStopTimeout)
		defer cancel()
		if err := server.Shutdown(shutdownCtx); err != nil {
			return errors.Wrap(err, "shut down worker HTTP server")
		}
		return <-done
	}
}

func newHTTPServer(address string, processor Processor, recorders ...Recorder) *http.Server {
	return &http.Server{
		Addr:              address,
		Handler:           NewHandler(processor, recorders...),
		ReadHeaderTimeout: requestHeaderTimeout,
		ReadTimeout:       requestReadTimeout,
		IdleTimeout:       idleConnectionTimeout,
	}
}
