// SPDX-License-Identifier: GPL-3.0-or-later

// Command notifier runs the notifier HTTP API.
//
// Configuration is read from the environment:
//
//	NOTIFIER_ADMIN_TOKEN            required; at least 32 characters
//	NOTIFIER_LISTEN                 listen address (default ":8080")
//	NOTIFIER_DB                     SQLite database path (default "notifier.db")
//	NOTIFIER_PUSH                   "fcm" (default) or "log" for local development
//	GOOGLE_APPLICATION_CREDENTIALS  Firebase service-account JSON (for "fcm")
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/joncbenderkh/notifier/server/internal/api"
	"github.com/joncbenderkh/notifier/server/internal/push"
	"github.com/joncbenderkh/notifier/server/internal/store"
)

var version = "0.1.0"

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	if err := run(logger); err != nil {
		logger.Error("fatal", "err", err)
		os.Exit(1)
	}
}

func run(logger *slog.Logger) error {
	if len(os.Args) > 1 && os.Args[1] == "version" {
		fmt.Println(version)
		return nil
	}

	adminToken := os.Getenv("NOTIFIER_ADMIN_TOKEN")
	if len(adminToken) < 32 {
		return errors.New("NOTIFIER_ADMIN_TOKEN must be set to at least 32 characters")
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	sender, err := newSender(ctx, getenv("NOTIFIER_PUSH", "fcm"), logger)
	if err != nil {
		return err
	}

	st, err := store.Open(ctx, getenv("NOTIFIER_DB", "notifier.db"))
	if err != nil {
		return err
	}
	defer st.Close()

	srv := &http.Server{
		Addr: getenv("NOTIFIER_LISTEN", ":8080"),
		Handler: (&api.Server{
			Store: st, Sender: sender, AdminToken: adminToken, Logger: logger, Now: time.Now,
		}).Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
	}

	errc := make(chan error, 1)
	go func() {
		logger.Info("listening", "addr", srv.Addr, "version", version)
		errc <- srv.ListenAndServe()
	}()

	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return srv.Shutdown(shutdownCtx)
}

func newSender(ctx context.Context, kind string, logger *slog.Logger) (push.Sender, error) {
	switch kind {
	case "fcm":
		return push.NewFCM(ctx)
	case "log":
		logger.Warn("NOTIFIER_PUSH=log: notifications are logged, not delivered")
		return push.Log{Logger: logger}, nil
	default:
		return nil, fmt.Errorf("NOTIFIER_PUSH must be \"fcm\" or \"log\", got %q", kind)
	}
}

func getenv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
