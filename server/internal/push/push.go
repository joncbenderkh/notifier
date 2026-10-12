// SPDX-License-Identifier: GPL-3.0-or-later

// Package push hands sealed payloads to a delivery transport.
package push

import (
	"context"
	"errors"
	"log/slog"
	"time"

	firebase "firebase.google.com/go/v4"
	"firebase.google.com/go/v4/messaging"
)

// ErrUnregistered means the transport no longer recognizes the device's push
// token (app uninstalled, data cleared); the token should be discarded.
var ErrUnregistered = errors.New("push: device token is no longer registered")

type Message struct {
	Token string
	Data  map[string]string
	High  bool
	TTL   time.Duration
}

type Sender interface {
	Send(ctx context.Context, m Message) error
}

type FCM struct {
	client *messaging.Client
}

// NewFCM authenticates with Application Default Credentials, normally a
// service-account JSON file named by GOOGLE_APPLICATION_CREDENTIALS.
func NewFCM(ctx context.Context) (*FCM, error) {
	app, err := firebase.NewApp(ctx, nil)
	if err != nil {
		return nil, err
	}
	client, err := app.Messaging(ctx)
	if err != nil {
		return nil, err
	}
	return &FCM{client: client}, nil
}

func (f *FCM) Send(ctx context.Context, m Message) error {
	priority := "normal"
	if m.High {
		priority = "high"
	}
	ttl := m.TTL
	// Data-only on purpose: a notification block would be rendered by the
	// system tray directly, bypassing the app's decryption.
	_, err := f.client.Send(ctx, &messaging.Message{
		Token:   m.Token,
		Data:    m.Data,
		Android: &messaging.AndroidConfig{Priority: priority, TTL: &ttl},
	})
	if messaging.IsUnregistered(err) {
		return ErrUnregistered
	}
	return err
}

// Log is a Sender for local development without Firebase credentials.
type Log struct {
	Logger *slog.Logger
}

func (l Log) Send(_ context.Context, m Message) error {
	l.Logger.Info("push (log transport)", "token", m.Token, "high", m.High, "data", m.Data)
	return nil
}
