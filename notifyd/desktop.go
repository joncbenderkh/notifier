// SPDX-License-Identifier: GPL-3.0-or-later

package main

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/godbus/dbus/v5"
)

// Desktop shows a message to the user. An error wrapping errRejected means
// this message can never be shown; any other error is worth retrying.
type Desktop interface {
	Notify(ctx context.Context, p Payload) error
}

var errRejected = errors.New("the notification service rejected the message")

// dbusTimeout bounds each call, so a notification service that stops
// answering cannot stall delivery or shutdown.
const dbusTimeout = 10 * time.Second

// freedesktop urgency levels.
const (
	urgencyNormal   byte = 1
	urgencyCritical byte = 2
)

// dbusDesktop calls org.freedesktop.Notifications on the session bus, which
// dunst, mako, GNOME, KDE and others implement.
type dbusDesktop struct {
	conn   *dbus.Conn
	obj    dbus.BusObject
	markup bool
}

func connectDesktop(ctx context.Context) (Desktop, error) {
	d := &dbusDesktop{}
	if err := d.connect(ctx); err != nil {
		return nil, err
	}
	return d, nil
}

// connect opens the session bus, again after it went away (a restarted
// bus or session), and asks whether the service renders body markup.
func (d *dbusDesktop) connect(ctx context.Context) error {
	// Not dbus.WithContext: that context would bound the connection's
	// whole life, not just the dial. The session bus is a local socket.
	conn, err := dbus.ConnectSessionBus()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, dbusTimeout)
	defer cancel()
	obj := conn.Object("org.freedesktop.Notifications", "/org/freedesktop/Notifications")
	var caps []string
	if err := obj.CallWithContext(ctx, "org.freedesktop.Notifications.GetCapabilities", 0).Store(&caps); err != nil {
		conn.Close()
		return err
	}
	d.conn, d.obj, d.markup = conn, obj, slices.Contains(caps, "body-markup")
	return nil
}

func (d *dbusDesktop) Notify(ctx context.Context, p Payload) error {
	if !d.conn.Connected() {
		if err := d.connect(ctx); err != nil {
			return err
		}
	}
	ctx, cancel := context.WithTimeout(ctx, dbusTimeout)
	defer cancel()
	summary, body, hints := notification(p, d.markup)
	err := d.obj.CallWithContext(ctx, "org.freedesktop.Notifications.Notify", 0,
		"notifier", uint32(0), "", summary, body, []string{}, hints, int32(-1)).Err
	if permanent(err) {
		return fmt.Errorf("%w: %v", errRejected, err)
	}
	return err
}

// permanent tells a message the service refused (or that cannot be
// encoded) from a service that is missing or not answering, which may
// recover.
func permanent(err error) bool {
	var format dbus.FormatError
	if errors.As(err, &format) {
		return true
	}
	var reply dbus.Error
	if !errors.As(err, &reply) {
		return false
	}
	switch reply.Name {
	case "org.freedesktop.DBus.Error.ServiceUnknown", "org.freedesktop.DBus.Error.NameHasNoOwner",
		"org.freedesktop.DBus.Error.NoReply", "org.freedesktop.DBus.Error.Timeout",
		"org.freedesktop.DBus.Error.Disconnected", "org.freedesktop.DBus.Error.Spawn.ChildExited":
		return false
	}
	return true
}

// notification maps a message onto Notify's arguments: high priority is
// critical urgency, and a channel becomes the category "notifier.<channel>",
// so notification daemon rules can match it.
func notification(p Payload, markup bool) (summary, body string, hints map[string]dbus.Variant) {
	urgency := urgencyNormal
	if p.Priority == "high" {
		urgency = urgencyCritical
	}
	hints = map[string]dbus.Variant{"urgency": dbus.MakeVariant(urgency)}
	if p.Channel != "" {
		hints["category"] = dbus.MakeVariant("notifier." + dropNUL(p.Channel))
	}
	body = dropNUL(p.Body)
	if markup {
		body = escapeMarkup(body)
	}
	return dropNUL(p.Title), body, hints
}

// dropNUL removes NUL characters, which JSON allows but D-Bus strings
// cannot carry.
func dropNUL(s string) string { return strings.ReplaceAll(s, "\x00", "") }

// escapeMarkup keeps a body from being read as markup by daemons that
// support it; the summary is plain text by specification.
var escapeMarkup = strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;").Replace
