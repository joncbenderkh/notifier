// SPDX-License-Identifier: GPL-3.0-or-later

// Command notify sends a notification to paired phones through a notifier
// server.
//
//	notify [-to PHONE]... [-high] TITLE [BODY | -]
//	notify devices
//	notify version
//
// PHONE is a device name (case-insensitive) or ID; without -to the
// notification goes to every paired phone. A BODY of "-" is read from
// standard input. The server and credential come from NOTIFIER_URL and
// NOTIFIER_API_KEY, or from -url and -key.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"text/tabwriter"
	"time"
)

var version = "0.1.0"

const (
	exitError       = 1
	exitUsage       = 2
	exitUndelivered = 3
)

type multiFlag []string

func (m *multiFlag) String() string { return strings.Join(*m, ",") }

func (m *multiFlag) Set(v string) error {
	for _, part := range strings.Split(v, ",") {
		if part = strings.TrimSpace(part); part != "" {
			*m = append(*m, part)
		}
	}
	return nil
}

func main() {
	os.Exit(run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}

func run(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("notify", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var to multiFlag
	fs.Var(&to, "to", "phone `name or ID` to notify; repeatable or comma-separated (default: all paired phones)")
	high := fs.Bool("high", false, "send as high priority (heads-up, may wake the phone)")
	baseURL := fs.String("url", os.Getenv("NOTIFIER_URL"), "notifier server `URL` (default $NOTIFIER_URL)")
	apiKey := fs.String("key", os.Getenv("NOTIFIER_API_KEY"), "API `key` (default $NOTIFIER_API_KEY)")
	timeout := fs.Duration("timeout", 30*time.Second, "request timeout")
	fs.Usage = func() {
		fmt.Fprint(stderr, "Usage:\n  notify [flags] TITLE [BODY | -]\n  notify devices\n  notify version\n\nFlags:\n")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}

	rest := fs.Args()
	if len(rest) == 1 && rest[0] == "version" {
		fmt.Fprintln(stdout, version)
		return 0
	}
	if *baseURL == "" || *apiKey == "" {
		fmt.Fprintln(stderr, "notify: set NOTIFIER_URL and NOTIFIER_API_KEY (or pass -url and -key)")
		return exitUsage
	}

	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	client := &Client{BaseURL: *baseURL, APIKey: *apiKey, HTTP: http.DefaultClient}

	if len(rest) == 1 && rest[0] == "devices" {
		return listDevices(ctx, client, stdout, stderr)
	}
	if len(rest) < 1 || len(rest) > 2 {
		fs.Usage()
		return exitUsage
	}

	n := Notification{Title: rest[0]}
	if *high {
		n.Priority = "high"
	}
	if len(rest) == 2 {
		n.Body = rest[1]
		if n.Body == "-" {
			b, err := io.ReadAll(io.LimitReader(stdin, 64<<10))
			if err != nil {
				return fail(stderr, err)
			}
			n.Body = strings.TrimRight(string(b), "\n")
		}
	}

	if len(to) > 0 {
		devices, err := client.Devices(ctx)
		if err != nil {
			return fail(stderr, err)
		}
		if n.DeviceIDs, err = ResolveDevices(devices, to); err != nil {
			return fail(stderr, err)
		}
	}

	result, err := client.Notify(ctx, n)
	if err != nil {
		return fail(stderr, err)
	}
	code := 0
	for _, d := range result.Deliveries {
		line := fmt.Sprintf("%s: %s", d.DeviceID, d.Status)
		if d.Error != "" {
			line += " (" + d.Error + ")"
		}
		if d.Status != "sent" {
			code = exitUndelivered
			fmt.Fprintln(stderr, line)
		} else {
			fmt.Fprintln(stdout, line)
		}
	}
	return code
}

func listDevices(ctx context.Context, client *Client, stdout, stderr io.Writer) int {
	devices, err := client.Devices(ctx)
	if err != nil {
		return fail(stderr, err)
	}
	w := tabwriter.NewWriter(stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "ID\tNAME\tREACHABLE\tLAST SEEN")
	for _, d := range devices {
		fmt.Fprintf(w, "%s\t%s\t%t\t%s\n", d.ID, d.Name, d.Reachable, d.LastSeenAt.Local().Format(time.DateTime))
	}
	w.Flush()
	return 0
}

func fail(stderr io.Writer, err error) int {
	var apiErr *APIError
	if errors.As(err, &apiErr) && apiErr.Status == http.StatusUnauthorized {
		fmt.Fprintln(stderr, "notify: the server rejected the API key")
		return exitError
	}
	fmt.Fprintln(stderr, "notify:", err)
	return exitError
}
