// SPDX-License-Identifier: GPL-3.0-or-later

package api

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/joncbenderkh/notifier/server/internal/push"
	"github.com/joncbenderkh/notifier/server/internal/seal"
	"github.com/joncbenderkh/notifier/server/internal/store"
)

const adminToken = "admin-token-admin-token-admin-token"

type fakeSender struct {
	mu           sync.Mutex
	sent         []push.Message
	unregistered map[string]bool
}

func (f *fakeSender) Send(_ context.Context, m push.Message) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.unregistered[m.Token] {
		return push.ErrUnregistered
	}
	f.sent = append(f.sent, m)
	return nil
}

type harness struct {
	t      *testing.T
	srv    *httptest.Server
	sender *fakeSender
	now    time.Time
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	st, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	h := &harness{t: t, sender: &fakeSender{unregistered: map[string]bool{}}, now: time.Unix(1_790_000_000, 0)}
	s := &Server{
		Store: st, Sender: h.sender, AdminToken: adminToken,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Now:    func() time.Time { return h.now },
	}
	h.srv = httptest.NewServer(s.Handler())
	t.Cleanup(h.srv.Close)
	return h
}

func (h *harness) do(method, path, bearer string, body any, wantStatus int, out any) {
	h.t.Helper()
	var reader io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		reader = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, h.srv.URL+path, reader)
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		h.t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != wantStatus {
		h.t.Fatalf("%s %s: status %d, want %d: %s", method, path, resp.StatusCode, wantStatus, raw)
	}
	if out != nil {
		if err := json.Unmarshal(raw, out); err != nil {
			h.t.Fatalf("decode %s: %v", raw, err)
		}
	}
}

func (h *harness) pair(name, fcmToken string) registerResponse {
	h.t.Helper()
	var p pairingResponse
	h.do("POST", "/v1/pairings", adminToken, nil, http.StatusCreated, &p)
	var reg registerResponse
	h.do("POST", "/v1/devices", "", registerRequest{Code: p.Code, Name: name, FCMToken: fcmToken}, http.StatusCreated, &reg)
	return reg
}

func (h *harness) apiKey() string {
	h.t.Helper()
	var k keyResponse
	h.do("POST", "/v1/keys", adminToken, keyRequest{Name: "ci"}, http.StatusCreated, &k)
	return k.Key
}

func TestNotifySealsPayloadForEachDevice(t *testing.T) {
	h := newHarness(t)
	phone := h.pair("phone", "fcm-phone")
	tablet := h.pair("tablet", "fcm-tablet")
	key := h.apiKey()

	var resp notifyResponse
	h.do("POST", "/v1/notifications", key,
		notifyRequest{Title: "Build done", Body: "main is green", Priority: "high"}, http.StatusOK, &resp)

	if len(resp.Deliveries) != 2 || len(h.sender.sent) != 2 {
		t.Fatalf("deliveries %+v, sent %d", resp.Deliveries, len(h.sender.sent))
	}
	for i, reg := range []registerResponse{phone, tablet} {
		m := h.sender.sent[i]
		if !m.High {
			t.Error("high priority not propagated")
		}
		sealKey, _ := base64.StdEncoding.DecodeString(reg.SealKey)
		nonce, _ := base64.StdEncoding.DecodeString(m.Data["n"])
		ct, _ := base64.StdEncoding.DecodeString(m.Data["c"])
		plaintext, err := seal.Open(sealKey, reg.DeviceID, seal.Envelope{Nonce: nonce, Ciphertext: ct})
		if err != nil {
			t.Fatalf("device %s cannot open its payload: %v", reg.DeviceID, err)
		}
		var p Payload
		json.Unmarshal(plaintext, &p)
		if p.Title != "Build done" || p.Body != "main is green" || p.ID != resp.ID {
			t.Errorf("payload %+v", p)
		}
	}
}

func TestNotifyTargetsSelectedDevices(t *testing.T) {
	h := newHarness(t)
	h.pair("phone", "fcm-phone")
	tablet := h.pair("tablet", "fcm-tablet")

	var resp notifyResponse
	h.do("POST", "/v1/notifications", h.apiKey(),
		notifyRequest{Title: "hi", DeviceIDs: []string{tablet.DeviceID}}, http.StatusOK, &resp)
	if len(h.sender.sent) != 1 || h.sender.sent[0].Token != "fcm-tablet" {
		t.Fatalf("sent %+v", h.sender.sent)
	}
}

func TestAuthentication(t *testing.T) {
	h := newHarness(t)
	reg := h.pair("phone", "fcm-phone")
	key := h.apiKey()
	note := notifyRequest{Title: "hi"}

	h.do("POST", "/v1/pairings", "wrong", nil, http.StatusUnauthorized, nil)
	h.do("POST", "/v1/pairings", key, nil, http.StatusUnauthorized, nil)
	h.do("POST", "/v1/notifications", "", note, http.StatusUnauthorized, nil)
	h.do("POST", "/v1/notifications", adminToken, note, http.StatusUnauthorized, nil)
	h.do("POST", "/v1/notifications", reg.DeviceToken, note, http.StatusUnauthorized, nil)
	h.do("POST", "/v1/notifications", "nk_unknown", note, http.StatusUnauthorized, nil)
	h.do("PUT", "/v1/device", key, updateSelfRequest{FCMToken: "x"}, http.StatusUnauthorized, nil)
}

func TestRevokedKeyIsRejected(t *testing.T) {
	h := newHarness(t)
	h.pair("phone", "fcm-phone")
	var k keyResponse
	h.do("POST", "/v1/keys", adminToken, keyRequest{Name: "ci"}, http.StatusCreated, &k)
	h.do("DELETE", "/v1/keys/"+k.ID, adminToken, nil, http.StatusNoContent, nil)
	h.do("POST", "/v1/notifications", k.Key, notifyRequest{Title: "hi"}, http.StatusUnauthorized, nil)
}

func TestPairingCodeIsSingleUseAndExpires(t *testing.T) {
	h := newHarness(t)
	var p pairingResponse
	h.do("POST", "/v1/pairings", adminToken, nil, http.StatusCreated, &p)
	req := registerRequest{Code: p.Code, Name: "phone", FCMToken: "fcm"}
	h.do("POST", "/v1/devices", "", req, http.StatusCreated, nil)
	h.do("POST", "/v1/devices", "", req, http.StatusForbidden, nil)

	h.do("POST", "/v1/pairings", adminToken, nil, http.StatusCreated, &p)
	h.now = h.now.Add(PairingTTL)
	req.Code = p.Code
	h.do("POST", "/v1/devices", "", req, http.StatusForbidden, nil)
}

func TestDeviceSelfManagement(t *testing.T) {
	h := newHarness(t)
	reg := h.pair("phone", "fcm-old")
	h.do("PUT", "/v1/device", reg.DeviceToken, updateSelfRequest{FCMToken: "fcm-new"}, http.StatusNoContent, nil)

	var resp notifyResponse
	h.do("POST", "/v1/device/test", reg.DeviceToken, nil, http.StatusOK, &resp)
	if h.sender.sent[0].Token != "fcm-new" {
		t.Fatalf("test notification went to %q", h.sender.sent[0].Token)
	}

	h.do("DELETE", "/v1/device", reg.DeviceToken, nil, http.StatusNoContent, nil)
	h.do("POST", "/v1/device/test", reg.DeviceToken, nil, http.StatusUnauthorized, nil)
}

func TestUnregisteredTokenIsCleared(t *testing.T) {
	h := newHarness(t)
	h.pair("phone", "fcm-gone")
	h.sender.unregistered["fcm-gone"] = true
	key := h.apiKey()

	var resp notifyResponse
	h.do("POST", "/v1/notifications", key, notifyRequest{Title: "hi"}, http.StatusOK, &resp)
	if resp.Deliveries[0].Status != "unreachable" {
		t.Fatalf("status %q", resp.Deliveries[0].Status)
	}
	var devices []deviceResponse
	h.do("GET", "/v1/devices", adminToken, nil, http.StatusOK, &devices)
	if devices[0].Reachable {
		t.Fatal("device still reachable after unregistered token")
	}
}

func TestNotifyValidation(t *testing.T) {
	h := newHarness(t)
	h.pair("phone", "fcm")
	key := h.apiKey()
	for _, req := range []any{
		notifyRequest{Title: " "},
		notifyRequest{Title: "hi", Priority: "urgent"},
		notifyRequest{Title: string(make([]byte, maxTitleBytes+1))},
		map[string]string{"title": "hi", "unexpected": "field"},
	} {
		h.do("POST", "/v1/notifications", key, req, http.StatusBadRequest, nil)
	}
}
