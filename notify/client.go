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
	"net/url"
	"strings"
	"time"
)

// Client calls the notifier API with a bearer token: a user token, or an
// API key (which may only send and list devices). Sign-up sends none.
type Client struct {
	BaseURL string
	Token   string
	HTTP    *http.Client
}

type Device struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Transport string `json:"transport"`
	Reachable bool   `json:"reachable"`
}

type Notification struct {
	Title     string    `json:"title"`
	Body      string    `json:"body,omitempty"`
	Priority  string    `json:"priority,omitempty"`
	Channels  []string  `json:"channels,omitempty"`
	DeviceIDs []string  `json:"device_ids,omitempty"`
	Question  *Question `json:"question,omitempty"`
	Receipts  bool      `json:"receipts,omitempty"`
}

type Delivery struct {
	DeviceID string `json:"device_id"`
	Status   string `json:"status"`
	Error    string `json:"error,omitempty"`
}

// NotifyResult lists deliveries to the sender's own devices; Members counts
// those to other channel members' devices by status.
type NotifyResult struct {
	ID         string         `json:"id"`
	Deliveries []Delivery     `json:"deliveries"`
	Members    map[string]int `json:"members,omitempty"`
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

// Pair creates a single-use pairing code; it needs a user token.
func (c *Client) Pair(ctx context.Context) (Pairing, error) {
	var p Pairing
	return p, c.do(ctx, http.MethodPost, "/v1/pairings", nil, &p)
}

func (c *Client) Notify(ctx context.Context, n Notification) (NotifyResult, error) {
	var result NotifyResult
	return result, c.do(ctx, http.MethodPost, "/v1/notifications", n, &result)
}

// ReplyState is a question's state; N and C hold the sealed answer.
type ReplyState struct {
	Status string `json:"status"`
	By     string `json:"by"`
	N      string `json:"n"`
	C      string `json:"c"`
}

// Reply waits up to [wait] seconds for the answer to the question [id].
func (c *Client) Reply(ctx context.Context, id string, wait int) (ReplyState, error) {
	var r ReplyState
	path := fmt.Sprintf("/v1/replies/%s?wait=%d", url.PathEscape(id), wait)
	return r, c.do(ctx, http.MethodGet, path, nil, &r)
}

// Receipts says which devices showed and opened a message sent with
// receipts; other members' devices only as counts.
type Receipts struct {
	Status  string          `json:"status"`
	Devices []DeviceReceipt `json:"devices"`
	Members struct {
		Shown  int `json:"shown"`
		Opened int `json:"opened"`
	} `json:"members"`
}

// DeviceReceipt holds when one of the sender's devices showed and opened a
// message, in Unix seconds; zero when it has not.
type DeviceReceipt struct {
	DeviceID string `json:"device_id"`
	Name     string `json:"name"`
	ShownAt  int64  `json:"shown_at"`
	OpenedAt int64  `json:"opened_at"`
}

// Receipts waits up to [wait] seconds for a device to open the message [id].
func (c *Client) Receipts(ctx context.Context, id string, wait int) (Receipts, error) {
	var r Receipts
	path := fmt.Sprintf("/v1/notifications/%s?wait=%d", url.PathEscape(id), wait)
	return r, c.do(ctx, http.MethodGet, path, nil, &r)
}

type Signup struct {
	UserID string `json:"user_id"`
	Name   string `json:"name"`
	Token  string `json:"token"`
}

// Signup redeems an invite for a new user; the token is shown only once.
func (c *Client) Signup(ctx context.Context, invite, name string) (Signup, error) {
	var s Signup
	return s, c.do(ctx, http.MethodPost, "/v1/users", map[string]string{"invite": invite, "name": name}, &s)
}

type Invite struct {
	Code      string    `json:"code"`
	ExpiresAt time.Time `json:"expires_at"`
}

func (c *Client) Invite(ctx context.Context) (Invite, error) {
	var i Invite
	return i, c.do(ctx, http.MethodPost, "/v1/invites", nil, &i)
}

type User struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

func (c *Client) Me(ctx context.Context) (User, error) {
	var u User
	return u, c.do(ctx, http.MethodGet, "/v1/user", nil, &u)
}

type APIKey struct {
	ID         string     `json:"id"`
	Name       string     `json:"name"`
	Key        string     `json:"key,omitempty"`
	LastUsedAt *time.Time `json:"last_used_at,omitempty"`
	Channels   []string   `json:"channels"`
}

func (c *Client) Keys(ctx context.Context) ([]APIKey, error) {
	var keys []APIKey
	return keys, c.do(ctx, http.MethodGet, "/v1/keys", nil, &keys)
}

// CreateKey creates an API key, limited to [channels] unless that is empty.
func (c *Client) CreateKey(ctx context.Context, name string, channels []string) (APIKey, error) {
	in := struct {
		Name     string   `json:"name"`
		Channels []string `json:"channels,omitempty"`
	}{name, channels}
	var k APIKey
	return k, c.do(ctx, http.MethodPost, "/v1/keys", in, &k)
}

func (c *Client) DeleteKey(ctx context.Context, id string) error {
	return c.do(ctx, http.MethodDelete, "/v1/keys/"+url.PathEscape(id), nil, nil)
}

// Channel is a channel the user owns or has joined. Name is as the user
// sees it: "homelab" when owned, "alice/homelab" when joined.
type Channel struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Owned   bool   `json:"owned"`
	Members int    `json:"members"`
}

func (c *Client) Channels(ctx context.Context) ([]Channel, error) {
	var channels []Channel
	return channels, c.do(ctx, http.MethodGet, "/v1/channels", nil, &channels)
}

func (c *Client) CreateChannel(ctx context.Context, name string) (Channel, error) {
	var ch Channel
	return ch, c.do(ctx, http.MethodPost, "/v1/channels", map[string]string{"name": name}, &ch)
}

func (c *Client) DeleteChannel(ctx context.Context, id string) error {
	return c.do(ctx, http.MethodDelete, "/v1/channels/"+url.PathEscape(id), nil, nil)
}

type ChannelInvite struct {
	Code      string    `json:"code"`
	Channel   string    `json:"channel"`
	ExpiresAt time.Time `json:"expires_at"`
	URI       string    `json:"uri"`
}

func (c *Client) InviteToChannel(ctx context.Context, id string) (ChannelInvite, error) {
	var i ChannelInvite
	return i, c.do(ctx, http.MethodPost, "/v1/channels/"+url.PathEscape(id)+"/invites", nil, &i)
}

// Join redeems a channel invite for the user.
func (c *Client) Join(ctx context.Context, code string) (Channel, error) {
	var ch Channel
	return ch, c.do(ctx, http.MethodPost, "/v1/memberships", map[string]string{"invite": code}, &ch)
}

type Member struct {
	ID       string    `json:"id"`
	Name     string    `json:"name"`
	JoinedAt time.Time `json:"joined_at"`
}

func (c *Client) Members(ctx context.Context, channelID string) ([]Member, error) {
	var members []Member
	return members, c.do(ctx, http.MethodGet, "/v1/channels/"+url.PathEscape(channelID)+"/members", nil, &members)
}

// RemoveMember removes a member (the owner's right), or the user themselves.
func (c *Client) RemoveMember(ctx context.Context, channelID, userID string) error {
	path := "/v1/channels/" + url.PathEscape(channelID) + "/members/" + url.PathEscape(userID)
	return c.do(ctx, http.MethodDelete, path, nil, nil)
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
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
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
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return errors.New("unexpected response from server: " + err.Error())
	}
	return nil
}

// Heartbeat is a dead-man's switch: the server alerts its channel when no
// ping arrives within Interval plus Grace (seconds).
type Heartbeat struct {
	ID         string     `json:"id"`
	Name       string     `json:"name"`
	Channel    string     `json:"channel"`
	Interval   int        `json:"interval"`
	Grace      int        `json:"grace"`
	State      string     `json:"state"`
	PingURL    string     `json:"ping_url,omitempty"`
	LastPingAt *time.Time `json:"last_ping_at,omitempty"`
	DueAt      *time.Time `json:"due_at,omitempty"`
}

func (c *Client) Heartbeats(ctx context.Context) ([]Heartbeat, error) {
	var hbs []Heartbeat
	return hbs, c.do(ctx, http.MethodGet, "/v1/heartbeats", nil, &hbs)
}

// CreateHeartbeat creates a heartbeat on [channel], or default when empty;
// a zero grace takes the server's default.
func (c *Client) CreateHeartbeat(ctx context.Context, name string, interval, grace time.Duration, channel string) (Heartbeat, error) {
	in := struct {
		Name     string `json:"name"`
		Interval int    `json:"interval"`
		Grace    int    `json:"grace,omitempty"`
		Channel  string `json:"channel,omitempty"`
	}{name, int(interval.Seconds()), int(grace.Seconds()), channel}
	var hb Heartbeat
	return hb, c.do(ctx, http.MethodPost, "/v1/heartbeats", in, &hb)
}

func (c *Client) DeleteHeartbeat(ctx context.Context, id string) error {
	return c.do(ctx, http.MethodDelete, "/v1/heartbeats/"+url.PathEscape(id), nil, nil)
}

type Hook struct {
	ID         string     `json:"id"`
	Name       string     `json:"name"`
	Format     string     `json:"format"`
	Channel    string     `json:"channel"`
	URL        string     `json:"url,omitempty"`
	LastUsedAt *time.Time `json:"last_used_at,omitempty"`
}

func (c *Client) Hooks(ctx context.Context) ([]Hook, error) {
	var hooks []Hook
	return hooks, c.do(ctx, http.MethodGet, "/v1/hooks", nil, &hooks)
}

// CreateHook creates a hook reading [format] that publishes to [channel],
// or default when empty.
func (c *Client) CreateHook(ctx context.Context, name, format, channel string) (Hook, error) {
	in := struct {
		Name    string `json:"name"`
		Format  string `json:"format"`
		Channel string `json:"channel,omitempty"`
	}{name, format, channel}
	var h Hook
	return h, c.do(ctx, http.MethodPost, "/v1/hooks", in, &h)
}

func (c *Client) DeleteHook(ctx context.Context, id string) error {
	return c.do(ctx, http.MethodDelete, "/v1/hooks/"+url.PathEscape(id), nil, nil)
}
