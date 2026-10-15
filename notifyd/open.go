// SPDX-License-Identifier: GPL-3.0-or-later

package main

import (
	"context"
	"errors"
	"fmt"
	"os/exec"

	"github.com/godbus/dbus/v5"

	"github.com/joncbenderkh/notifier/notifyd/internal/histfile"
)

// OpenFunc opens a message's link in the user's browser. It is called off
// the signal loop.
type OpenFunc func(link string)

// webLink reports whether link is one a message may open ([histfile.WebLink]).
func webLink(link string) bool {
	_, ok := histfile.WebLink(link)
	return ok
}

// openLink opens link through the desktop portal's OpenURI, which asks the
// user's chosen browser, or with xdg-open where no portal runs (a plain
// window manager often has none). Any other portal error is final: it may
// have opened the link already, or refused to. logf reports what only
// shows later, xdg-open failing.
func openLink(ctx context.Context, link string, logf func(string, ...any)) error {
	if !webLink(link) {
		return errors.New("not an http or https link")
	}
	err := openWithPortal(ctx, link)
	if !noPortal(err) {
		return err
	}
	cmd := exec.Command("xdg-open", link)
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("no desktop portal, and xdg-open: %w", err)
	}
	go func() {
		if err := cmd.Wait(); err != nil {
			logf("could not open %s with xdg-open: %v", link, err)
		}
	}()
	return nil
}

// openWithPortal asks the portal to open link. The portal answers the
// request later, by a signal to the caller, and may drop it when the
// caller disconnects; the shared session bus connection stays.
func openWithPortal(ctx context.Context, link string) error {
	conn, err := dbus.SessionBus()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, dbusTimeout)
	defer cancel()
	portal := conn.Object("org.freedesktop.portal.Desktop", "/org/freedesktop/portal/desktop")
	return portal.CallWithContext(ctx, "org.freedesktop.portal.OpenURI.OpenURI", 0,
		"", link, map[string]dbus.Variant{}).Err
}

// noPortal tells an error saying no portal (or no OpenURI) runs.
func noPortal(err error) bool {
	var e dbus.Error
	if !errors.As(err, &e) {
		return false
	}
	switch e.Name {
	case "org.freedesktop.DBus.Error.ServiceUnknown", "org.freedesktop.DBus.Error.NameHasNoOwner",
		"org.freedesktop.DBus.Error.UnknownMethod", "org.freedesktop.DBus.Error.UnknownInterface",
		"org.freedesktop.DBus.Error.UnknownObject":
		return true
	}
	return false
}
