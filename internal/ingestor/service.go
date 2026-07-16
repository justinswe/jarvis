// Package ingestor owns the stateful Discord Gateway connection.
package ingestor

import (
	"context"
	"sync/atomic"

	"github.com/bwmarrin/discordgo"
	"github.com/justinswe/std/app"
	"github.com/justinswe/std/errors"
	"go.uber.org/zap"
)

// Service receives Discord Gateway events and forwards them to a worker.
type Service struct {
	session    *discordgo.Session
	controller *Controller
	ready      atomic.Bool
}

// New creates a Discord Gateway ingestor.
func New(token string, publisher MessagePublisher) (*Service, error) {
	if token == "" {
		return nil, errors.New("discord bot token is required")
	}
	if publisher == nil {
		return nil, errors.New("message publisher is required")
	}
	session, err := discordgo.New("Bot " + token)
	if err != nil {
		return nil, errors.Wrap(err, "create Discord Gateway session")
	}
	return &Service{session: session, controller: NewController(publisher)}, nil
}

// Ready reports whether the Discord Gateway is connected.
func (s *Service) Ready() bool {
	return s.ready.Load()
}

// Start registers Gateway handlers and blocks until shutdown.
func (s *Service) Start(ctx context.Context) error {
	s.session.AddHandler(func(_ *discordgo.Session, event *discordgo.MessageCreate) {
		s.controller.HandleMessage(ctx, event)
	})
	s.session.AddHandler(func(_ *discordgo.Session, _ *discordgo.Ready) { s.ready.Store(true) })
	s.session.AddHandler(func(_ *discordgo.Session, _ *discordgo.Resumed) { s.ready.Store(true) })
	s.session.AddHandler(func(_ *discordgo.Session, _ *discordgo.Disconnect) { s.ready.Store(false) })
	s.session.Identify.Intents = discordgo.IntentsGuildMessages | discordgo.IntentsDirectMessages | discordgo.IntentsMessageContent
	if err := s.session.Open(); err != nil {
		return errors.Wrap(err, "open Discord Gateway connection")
	}
	app.L().Info("Discord ingestor connected")
	<-ctx.Done()
	s.ready.Store(false)
	if err := s.session.Close(); err != nil {
		return errors.Wrap(err, "close Discord Gateway connection")
	}
	app.L().Info("Discord ingestor stopped", zap.Error(ctx.Err()))
	return nil
}
