// SPDX-License-Identifier: GPL-3.0-or-later

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Client calls the notifier API with a bearer token: an API key for
// sending, or the admin token for pairing.
type Client struct {
	BaseURL string
	Token   string
	HTTP    *http.Client
}

type Device struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Reachable bool   `json:"reachable"`
}

type Notification struct {
	Title     string   `json:"title"`
	Body      string   `json:"body,omitempty"`
	Priority  string   `json:"priority,omitempty"`
	DeviceIDs []string `json:"device_ids,omitempty"`
}

type Delivery struct {
	DeviceID string `json:"device_id"`
	Status   string `json:"status"`
	Error    string `json:"error,omitempty"`
}

type NotifyResult struct {
	ID         string     `json:"id"`
	Deliveries []Delivery `json:"deliveries"`
}

// APIError is a non-2xx response, carrying the server's error message.
type APIError struct {
	Status  int
	Message string
}

func (e *APIError) Error() string { return fmt.Sprintf("server returned %d: %s", e.Status, e.Message) }

func (c *Client) Devices(ctx context.Context) ([]Device, error) {
	var devices []Device
	return devices, c.do(ctx, http.MethodGet, "/v1/devices", nil, &devices)
}

type Pairing struct {
	Code      string    `json:"code"`
	ExpiresAt time.Time `json:"expires_at"`
	URI       string    `json:"uri"`
}

// Pair creates a single-use pairing code; it needs the admin token.
func (c *Client) Pair(ctx context.Context) (Pairing, error) {
	var p Pairing
	return p, c.do(ctx, http.MethodPost, "/v1/pairings", nil, &p)
}

func (c *Client) Notify(ctx context.Context, n Notification) (NotifyResult, error) {
	var result NotifyResult
	return result, c.do(ctx, http.MethodPost, "/v1/notifications", n, &result)
}

// ErrNoSuchDevice means a selector matched no paired phone.
var ErrNoSuchDevice = errors.New("no paired phone matches")

// ResolveDevices maps each selector (a device ID, or a name compared
// case-insensitively) to exactly one device ID.
func ResolveDevices(devices []Device, selectors []string) ([]string, error) {
	ids := make([]string, 0, len(selectors))
	for _, sel := range selectors {
		var matches []Device
		for _, d := range devices {
			if d.ID == sel {
				matches = []Device{d}
				break
			}
			if strings.EqualFold(d.Name, sel) {
				matches = append(matches, d)
			}
		}
		switch len(matches) {
		case 0:
			return nil, fmt.Errorf("%w %q (see `notify devices`)", ErrNoSuchDevice, sel)
		case 1:
			ids = append(ids, matches[0].ID)
		default:
			return nil, fmt.Errorf("%d devices are named %q; use a device ID instead", len(matches), sel)
		}
	}
	return ids, nil
}

func (c *Client) do(ctx context.Context, method, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(c.BaseURL, "/")+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode/100 != 2 {
		var e struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(raw, &e) != nil || e.Error == "" {
			e.Error = strings.TrimSpace(string(raw))
		}
		return &APIError{Status: resp.StatusCode, Message: e.Error}
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return errors.New("unexpected response from server: " + err.Error())
	}
	return nil
}
