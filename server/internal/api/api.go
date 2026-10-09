// SPDX-License-Identifier: GPL-3.0-or-later

// Package api serves notifier's HTTP API.
//
// Three credentials guard it: the operator's admin token (pairing, key and
// device management), per-sender API keys (sending notifications), and
// per-device tokens (a paired phone managing its own registration).
package api

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/joncbenderkh/notifier/server/internal/push"
	"github.com/joncbenderkh/notifier/server/internal/seal"
	"github.com/joncbenderkh/notifier/server/internal/store"
	"github.com/joncbenderkh/notifier/server/internal/token"
)

const (
	PairingTTL      = 10 * time.Minute
	MessageTTL      = 24 * time.Hour
	maxRequestBytes = 16 << 10
	maxTitleBytes   = 256
	maxBodyBytes    = 2048
	maxNameBytes    = 64
	envelopeVersion = "1"
)

type Server struct {
	Store      *store.Store
	Sender     push.Sender
	AdminToken string
	Logger     *slog.Logger
	Now        func() time.Time
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})

	mux.HandleFunc("POST /v1/pairings", s.admin(s.createPairing))
	mux.HandleFunc("GET /v1/devices", s.admin(s.listDevices))
	mux.HandleFunc("DELETE /v1/devices/{id}", s.admin(s.deleteDevice))
	mux.HandleFunc("POST /v1/keys", s.admin(s.createKey))
	mux.HandleFunc("GET /v1/keys", s.admin(s.listKeys))
	mux.HandleFunc("DELETE /v1/keys/{id}", s.admin(s.deleteKey))

	mux.HandleFunc("POST /v1/devices", s.registerDevice)
	mux.HandleFunc("PUT /v1/device", s.device(s.updateSelf))
	mux.HandleFunc("DELETE /v1/device", s.device(s.unpairSelf))
	mux.HandleFunc("POST /v1/device/test", s.device(s.testSelf))

	mux.HandleFunc("POST /v1/notifications", s.apiKey(s.notify))

	return http.MaxBytesHandler(mux, maxRequestBytes)
}

// --- authentication ---

func bearer(r *http.Request) string {
	scheme, credential, ok := strings.Cut(r.Header.Get("Authorization"), " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") {
		return ""
	}
	return strings.TrimSpace(credential)
}

func (s *Server) admin(next http.HandlerFunc) http.HandlerFunc {
	want := sha256.Sum256([]byte(s.AdminToken))
	return func(w http.ResponseWriter, r *http.Request) {
		got := sha256.Sum256([]byte(bearer(r)))
		if subtle.ConstantTimeCompare(got[:], want[:]) != 1 {
			writeError(w, http.StatusUnauthorized, "invalid admin token")
			return
		}
		next(w, r)
	}
}

func (s *Server) apiKey(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		secret := bearer(r)
		if !strings.HasPrefix(secret, token.APIKeyPrefix) {
			writeError(w, http.StatusUnauthorized, "invalid API key")
			return
		}
		if _, err := s.Store.APIKeyByHash(r.Context(), token.Hash(secret), s.Now()); err != nil {
			s.authFailure(w, err, "invalid API key")
			return
		}
		next(w, r)
	}
}

type deviceKey struct{}

func (s *Server) device(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		secret := bearer(r)
		if !strings.HasPrefix(secret, token.DeviceTokenPrefix) {
			writeError(w, http.StatusUnauthorized, "invalid device token")
			return
		}
		d, err := s.Store.DeviceByTokenHash(r.Context(), token.Hash(secret), s.Now())
		if err != nil {
			s.authFailure(w, err, "invalid device token")
			return
		}
		next(w, r.WithContext(context.WithValue(r.Context(), deviceKey{}, d)))
	}
}

func (s *Server) authFailure(w http.ResponseWriter, err error, message string) {
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusUnauthorized, message)
		return
	}
	s.internalError(w, err)
}

// --- admin ---

type pairingResponse struct {
	Code      string    `json:"code"`
	ExpiresAt time.Time `json:"expires_at"`
}

func (s *Server) createPairing(w http.ResponseWriter, r *http.Request) {
	code := token.NewPairingCode()
	expires := s.Now().Add(PairingTTL).UTC().Truncate(time.Second)
	if err := s.Store.CreatePairing(r.Context(), token.Hash(code), expires); err != nil {
		s.internalError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, pairingResponse{Code: code, ExpiresAt: expires})
}

type deviceResponse struct {
	ID         string    `json:"id"`
	Name       string    `json:"name"`
	Reachable  bool      `json:"reachable"`
	CreatedAt  time.Time `json:"created_at"`
	LastSeenAt time.Time `json:"last_seen_at"`
}

func (s *Server) listDevices(w http.ResponseWriter, r *http.Request) {
	devices, err := s.Store.Devices(r.Context())
	if err != nil {
		s.internalError(w, err)
		return
	}
	out := make([]deviceResponse, 0, len(devices))
	for _, d := range devices {
		out = append(out, deviceResponse{
			ID: d.ID, Name: d.Name, Reachable: d.FCMToken != "",
			CreatedAt: d.CreatedAt, LastSeenAt: d.LastSeenAt,
		})
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) deleteDevice(w http.ResponseWriter, r *http.Request) {
	s.respondDeleted(w, s.Store.DeleteDevice(r.Context(), r.PathValue("id")))
}

type keyRequest struct {
	Name string `json:"name"`
}

type keyResponse struct {
	ID         string     `json:"id"`
	Name       string     `json:"name"`
	Key        string     `json:"key,omitempty"`
	CreatedAt  time.Time  `json:"created_at"`
	LastUsedAt *time.Time `json:"last_used_at,omitempty"`
}

func (s *Server) createKey(w http.ResponseWriter, r *http.Request) {
	var req keyRequest
	if !decode(w, r, &req) {
		return
	}
	name, ok := validName(w, req.Name)
	if !ok {
		return
	}
	k := store.APIKey{ID: token.NewID("key_"), Name: name, CreatedAt: s.Now().UTC().Truncate(time.Second)}
	secret := token.New(token.APIKeyPrefix)
	if err := s.Store.CreateAPIKey(r.Context(), k, token.Hash(secret)); err != nil {
		s.internalError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, keyResponse{ID: k.ID, Name: k.Name, Key: secret, CreatedAt: k.CreatedAt})
}

func (s *Server) listKeys(w http.ResponseWriter, r *http.Request) {
	keys, err := s.Store.APIKeys(r.Context())
	if err != nil {
		s.internalError(w, err)
		return
	}
	out := make([]keyResponse, 0, len(keys))
	for _, k := range keys {
		out = append(out, keyResponse{ID: k.ID, Name: k.Name, CreatedAt: k.CreatedAt, LastUsedAt: k.LastUsedAt})
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) deleteKey(w http.ResponseWriter, r *http.Request) {
	s.respondDeleted(w, s.Store.DeleteAPIKey(r.Context(), r.PathValue("id")))
}

// --- device ---

type registerRequest struct {
	Code     string `json:"code"`
	Name     string `json:"name"`
	FCMToken string `json:"fcm_token"`
}

type registerResponse struct {
	DeviceID    string `json:"device_id"`
	DeviceToken string `json:"device_token"`
	SealKey     string `json:"seal_key"`
}

func (s *Server) registerDevice(w http.ResponseWriter, r *http.Request) {
	var req registerRequest
	if !decode(w, r, &req) {
		return
	}
	name, ok := validName(w, req.Name)
	if !ok {
		return
	}
	if req.FCMToken == "" {
		writeError(w, http.StatusBadRequest, "fcm_token is required")
		return
	}
	key, err := seal.NewKey()
	if err != nil {
		s.internalError(w, err)
		return
	}
	d := store.Device{ID: token.NewID("dev_"), Name: name, SealKey: key, FCMToken: req.FCMToken}
	secret := token.New(token.DeviceTokenPrefix)
	codeHash := token.Hash(token.NormalizePairingCode(req.Code))
	err = s.Store.RedeemPairing(r.Context(), codeHash, s.Now(), d, token.Hash(secret))
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusForbidden, "pairing code is invalid, expired, or already used")
		return
	}
	if err != nil {
		s.internalError(w, err)
		return
	}
	s.Logger.Info("device paired", "device_id", d.ID, "name", d.Name)
	writeJSON(w, http.StatusCreated, registerResponse{
		DeviceID: d.ID, DeviceToken: secret, SealKey: base64.StdEncoding.EncodeToString(key),
	})
}

type updateSelfRequest struct {
	FCMToken string `json:"fcm_token"`
}

func (s *Server) updateSelf(w http.ResponseWriter, r *http.Request) {
	var req updateSelfRequest
	if !decode(w, r, &req) {
		return
	}
	if req.FCMToken == "" {
		writeError(w, http.StatusBadRequest, "fcm_token is required")
		return
	}
	d := r.Context().Value(deviceKey{}).(store.Device)
	if err := s.Store.SetFCMToken(r.Context(), d.ID, req.FCMToken); err != nil {
		s.internalError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) unpairSelf(w http.ResponseWriter, r *http.Request) {
	d := r.Context().Value(deviceKey{}).(store.Device)
	s.respondDeleted(w, s.Store.DeleteDevice(r.Context(), d.ID))
}

func (s *Server) testSelf(w http.ResponseWriter, r *http.Request) {
	d := r.Context().Value(deviceKey{}).(store.Device)
	p := Payload{
		ID: token.NewID("msg_"), Title: "Test notification",
		Body: "Delivered and decrypted end to end.", Priority: "default", SentAt: s.Now().Unix(),
	}
	writeJSON(w, http.StatusOK, notifyResponse{ID: p.ID, Deliveries: []Delivery{s.deliver(r.Context(), d, p)}})
}

// --- notifications ---

// Payload is the plaintext sealed inside each push message. The Android app
// decodes the same JSON shape.
type Payload struct {
	ID       string `json:"id"`
	Title    string `json:"title"`
	Body     string `json:"body,omitempty"`
	Priority string `json:"priority"`
	SentAt   int64  `json:"sent_at"`
}

type notifyRequest struct {
	Title     string   `json:"title"`
	Body      string   `json:"body"`
	Priority  string   `json:"priority"`
	DeviceIDs []string `json:"device_ids"`
}

type Delivery struct {
	DeviceID string `json:"device_id"`
	Status   string `json:"status"`
	Error    string `json:"error,omitempty"`
}

type notifyResponse struct {
	ID         string     `json:"id"`
	Deliveries []Delivery `json:"deliveries"`
}

func (s *Server) notify(w http.ResponseWriter, r *http.Request) {
	var req notifyRequest
	if !decode(w, r, &req) {
		return
	}
	switch {
	case strings.TrimSpace(req.Title) == "":
		writeError(w, http.StatusBadRequest, "title is required")
		return
	case len(req.Title) > maxTitleBytes:
		writeError(w, http.StatusBadRequest, "title is too long")
		return
	case len(req.Body) > maxBodyBytes:
		writeError(w, http.StatusBadRequest, "body is too long")
		return
	case !utf8.ValidString(req.Title) || !utf8.ValidString(req.Body):
		writeError(w, http.StatusBadRequest, "title and body must be valid UTF-8")
		return
	}
	switch req.Priority {
	case "":
		req.Priority = "default"
	case "default", "high":
	default:
		writeError(w, http.StatusBadRequest, `priority must be "default" or "high"`)
		return
	}

	targets, err := s.targets(r.Context(), req.DeviceIDs)
	if err != nil {
		s.internalError(w, err)
		return
	}
	if len(targets) == 0 {
		writeError(w, http.StatusNotFound, "no matching paired devices")
		return
	}

	p := Payload{
		ID: token.NewID("msg_"), Title: req.Title, Body: req.Body,
		Priority: req.Priority, SentAt: s.Now().Unix(),
	}
	resp := notifyResponse{ID: p.ID, Deliveries: make([]Delivery, 0, len(targets))}
	for _, d := range targets {
		resp.Deliveries = append(resp.Deliveries, s.deliver(r.Context(), d, p))
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) targets(ctx context.Context, ids []string) ([]store.Device, error) {
	devices, err := s.Store.Devices(ctx)
	if err != nil || len(ids) == 0 {
		return devices, err
	}
	wanted := make(map[string]bool, len(ids))
	for _, id := range ids {
		wanted[id] = true
	}
	var out []store.Device
	for _, d := range devices {
		if wanted[d.ID] {
			out = append(out, d)
		}
	}
	return out, nil
}

func (s *Server) deliver(ctx context.Context, d store.Device, p Payload) Delivery {
	result := Delivery{DeviceID: d.ID}
	if d.FCMToken == "" {
		result.Status = "unreachable"
		return result
	}
	plaintext, err := json.Marshal(p)
	if err != nil {
		result.Status, result.Error = "failed", err.Error()
		return result
	}
	env, err := seal.Seal(d.SealKey, d.ID, plaintext)
	if err != nil {
		result.Status, result.Error = "failed", err.Error()
		return result
	}
	err = s.Sender.Send(ctx, push.Message{
		Token: d.FCMToken,
		High:  p.Priority == "high",
		TTL:   MessageTTL,
		Data: map[string]string{
			"v": envelopeVersion,
			"n": base64.StdEncoding.EncodeToString(env.Nonce),
			"c": base64.StdEncoding.EncodeToString(env.Ciphertext),
		},
	})
	switch {
	case errors.Is(err, push.ErrUnregistered):
		s.Logger.Warn("push token unregistered; clearing", "device_id", d.ID)
		if err := s.Store.SetFCMToken(ctx, d.ID, ""); err != nil {
			s.Logger.Error("clear push token", "device_id", d.ID, "err", err)
		}
		result.Status = "unreachable"
	case err != nil:
		s.Logger.Error("push send failed", "device_id", d.ID, "err", err)
		result.Status, result.Error = "failed", "push transport error"
	default:
		result.Status = "sent"
	}
	return result
}

// --- helpers ---

func validName(w http.ResponseWriter, raw string) (string, bool) {
	name := strings.TrimSpace(raw)
	if name == "" || len(name) > maxNameBytes || !utf8.ValidString(name) {
		writeError(w, http.StatusBadRequest, "name is required and must be at most 64 bytes")
		return "", false
	}
	return name, true
}

func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
		return false
	}
	return true
}

func (s *Server) respondDeleted(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeError(w, http.StatusNotFound, "not found")
	case err != nil:
		s.internalError(w, err)
	default:
		w.WriteHeader(http.StatusNoContent)
	}
}

func (s *Server) internalError(w http.ResponseWriter, err error) {
	s.Logger.Error("internal error", "err", err)
	writeError(w, http.StatusInternalServerError, "internal error")
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"error": message})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}
