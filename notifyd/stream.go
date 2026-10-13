// SPDX-License-Identifier: GPL-3.0-or-later

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand/v2"
	"net/http"
	"strings"
	"time"

	"github.com/coder/websocket"
)

// Close codes the server sends (server/src/stream.ts).
const (
	closeReplaced websocket.StatusCode = 4000
	closeUnpaired websocket.StatusCode = 4001
)

var (
	errNewerEnvelope = errors.New("update notifyd to read envelope version")
	errUnpaired      = errors.New("the server no longer knows this device; pair it again")
	errReplaced      = errors.New("another notifyd connected as this device")
)

// frame is one message from the server: envelope v1 plus the message ID,
// which is acked once the message has been shown.
type frame struct {
	Type string `json:"type"`
	ID   string `json:"id"`
	V    string `json:"v"`
	N    string `json:"n"`
	C    string `json:"c"`
}

// streamer holds the device's connection to the server, reconnecting with
// backoff, and shows each message it receives.
type streamer struct {
	state   State
	desktop Desktop
	http    *http.Client
	logf    func(format string, args ...any)
	seen    *recentIDs

	pingInterval time.Duration
	readTimeout  time.Duration
	dialTimeout  time.Duration
	minBackoff   time.Duration
	maxBackoff   time.Duration
}

func newStreamer(st State, d Desktop, client *http.Client, logf func(string, ...any)) *streamer {
	return &streamer{
		state:        st,
		desktop:      d,
		http:         client,
		logf:         logf,
		seen:         newRecentIDs(500),
		pingInterval: 30 * time.Second,
		readTimeout:  75 * time.Second,
		dialTimeout:  30 * time.Second,
		minBackoff:   time.Second,
		maxBackoff:   5 * time.Minute,
	}
}

// run keeps the stream open until ctx ends (nil) or the server ends this
// pairing (errUnpaired, errReplaced), which reconnecting cannot fix.
func (s *streamer) run(ctx context.Context) error {
	backoff := s.minBackoff
	for {
		lasted, err := s.session(ctx)
		if ctx.Err() != nil {
			return nil
		}
		if errors.Is(err, errUnpaired) || errors.Is(err, errReplaced) {
			return err
		}
		// A connection that held for a while was healthy: start over.
		if lasted > time.Minute {
			backoff = s.minBackoff
		}
		wait := backoff/2 + rand.N(backoff/2+1)
		s.logf("disconnected (%v); reconnecting in %s", err, wait.Round(time.Millisecond))
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(wait):
		}
		backoff = min(backoff*2, s.maxBackoff)
	}
}

// session connects once and serves the connection until it ends, reporting
// how long it was up.
func (s *streamer) session(ctx context.Context) (time.Duration, error) {
	// A server that accepts the connection but never answers the upgrade
	// must not hang the daemon.
	dialCtx, cancelDial := context.WithTimeout(ctx, s.dialTimeout)
	conn, resp, err := websocket.Dial(dialCtx, streamURL(s.state.URL), &websocket.DialOptions{
		HTTPClient: s.http,
		HTTPHeader: http.Header{"Authorization": {"Bearer " + s.state.DeviceToken}},
	})
	cancelDial() // bounds the handshake only, not the connection
	if err != nil {
		if resp != nil && (resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusNotFound) {
			return 0, errUnpaired
		}
		return 0, err
	}
	defer conn.CloseNow()
	conn.SetReadLimit(64 << 10)
	start := time.Now()
	s.logf("connected to %s", s.state.URL)

	sessionCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	go s.ping(sessionCtx, conn)
	err = s.receive(sessionCtx, conn)
	switch websocket.CloseStatus(err) {
	case closeUnpaired:
		err = errUnpaired
	case closeReplaced:
		err = errReplaced
	}
	if ctx.Err() != nil {
		conn.Close(websocket.StatusNormalClosure, "shutting down")
	}
	return time.Since(start), err
}

// ping keeps the connection alive; the server answers without waking up.
func (s *streamer) ping(ctx context.Context, conn *websocket.Conn) {
	t := time.NewTicker(s.pingInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := conn.Write(ctx, websocket.MessageText, []byte("ping")); err != nil {
				return
			}
		}
	}
}

// receive handles frames until the connection fails or goes silent for
// longer than a ping round trip should take.
func (s *streamer) receive(ctx context.Context, conn *websocket.Conn) error {
	for {
		readCtx, cancel := context.WithTimeout(ctx, s.readTimeout)
		typ, data, err := conn.Read(readCtx)
		cancel()
		if err != nil {
			return err
		}
		if typ != websocket.MessageText || string(data) == "pong" {
			continue
		}
		if err := s.handle(ctx, conn, data); err != nil {
			return err
		}
	}
}

// handle shows a message, then acks it so the server drops it from the
// queue. A message the desktop could not show is not acked, so the server
// replays it on the next connection; one that can never be opened is acked
// and dropped.
func (s *streamer) handle(ctx context.Context, conn *websocket.Conn, data []byte) error {
	var f frame
	if err := json.Unmarshal(data, &f); err != nil || f.Type != "message" || f.ID == "" {
		s.logf("ignoring an unexpected frame")
		return nil
	}
	if !s.seen.has(f.ID) {
		p, err := s.open(f)
		switch {
		case errors.Is(err, errNewerEnvelope):
			// Left in the server's queue for an updated notifyd, not lost.
			s.logf("message %s: %v", f.ID, err)
			return nil
		case err != nil:
			s.logf("dropping message %s: %v", f.ID, err)
		default:
			if err := s.desktop.Notify(ctx, p); errors.Is(err, errRejected) {
				// Retrying cannot help, and would hold up the messages queued behind it.
				s.logf("dropping message %s: %v", f.ID, err)
			} else if err != nil {
				return fmt.Errorf("show notification: %w", err)
			}
		}
		s.seen.add(f.ID)
	}
	ack, err := json.Marshal(map[string]string{"ack": f.ID})
	if err != nil {
		return err
	}
	return conn.Write(ctx, websocket.MessageText, ack)
}

func (s *streamer) open(f frame) (Payload, error) {
	if f.V != envelopeVersion {
		return Payload{}, fmt.Errorf("%w %q", errNewerEnvelope, f.V)
	}
	p, err := openEnvelope(s.state.SealKey, s.state.DeviceID, f.N, f.C)
	if err != nil {
		return p, err
	}
	if p.ID != f.ID {
		return p, fmt.Errorf("sealed ID %q does not match", p.ID)
	}
	return p, nil
}

// streamURL is the stream endpoint of a server origin: https becomes wss.
func streamURL(server string) string {
	if rest, ok := strings.CutPrefix(server, "http"); ok {
		server = "ws" + rest
	}
	return server + "/v1/device/stream"
}

// recentIDs remembers the last few message IDs shown, since delivery is at
// least once: the server replays a message whose ack it did not get.
type recentIDs struct {
	order []string
	set   map[string]struct{}
	max   int
}

func newRecentIDs(max int) *recentIDs {
	return &recentIDs{set: make(map[string]struct{}, max), max: max}
}

func (r *recentIDs) has(id string) bool {
	_, ok := r.set[id]
	return ok
}

func (r *recentIDs) add(id string) {
	if r.has(id) {
		return
	}
	if len(r.order) == r.max {
		delete(r.set, r.order[0])
		r.order = r.order[1:]
	}
	r.order = append(r.order, id)
	r.set[id] = struct{}{}
}
