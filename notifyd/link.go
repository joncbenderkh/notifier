// SPDX-License-Identifier: GPL-3.0-or-later

package main

import (
	"fmt"
	"net"
	"net/url"
	"strings"
)

// parsePairLink reads a notifier://pair?server=<origin>&code=<code> link,
// as `notify pair` and the server's /pair page show it. Android's
// NotifierLink.kt parses the same links.
func parsePairLink(raw string) (server, code string, err error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || !strings.EqualFold(u.Scheme, "notifier") || u.Host != "pair" {
		return "", "", fmt.Errorf("not a notifier://pair link: %q", raw)
	}
	q := u.Query()
	server, err = origin(q.Get("server"))
	if err != nil {
		return "", "", err
	}
	code = strings.TrimSpace(q.Get("code"))
	if code == "" {
		return "", "", fmt.Errorf("the link has no pairing code")
	}
	return server, code, nil
}

// origin normalizes a server URL to its bare origin: lowercase scheme and
// host, no default port, no trailing slash. It refuses anything else, so
// "https://trusted@evil.example" cannot pose as a trusted server, and allows
// plain http only for this machine.
func origin(raw string) (string, error) {
	u, err := url.Parse(strings.TrimSuffix(strings.TrimSpace(raw), "/"))
	if err != nil || u.Host == "" || u.User != nil || u.Path != "" || u.RawQuery != "" || u.Fragment != "" || u.Opaque != "" {
		return "", fmt.Errorf("invalid server URL %q: expected https://host[:port]", raw)
	}
	scheme, host, port := strings.ToLower(u.Scheme), strings.ToLower(u.Hostname()), u.Port()
	switch {
	case scheme == "https":
	case scheme == "http" && isLoopback(host):
	default:
		return "", fmt.Errorf("server URL must use https (got %q)", raw)
	}
	if port == "443" && scheme == "https" || port == "80" && scheme == "http" {
		port = ""
	}
	if strings.Contains(host, ":") {
		host = "[" + host + "]"
	}
	if port != "" {
		host += ":" + port
	}
	return scheme + "://" + host, nil
}

func isLoopback(host string) bool {
	ip := net.ParseIP(host)
	return host == "localhost" || ip != nil && ip.IsLoopback()
}
