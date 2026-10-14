// SPDX-License-Identifier: GPL-3.0-or-later

package main

import (
	"context"
	"errors"
	"fmt"
	"math"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/godbus/dbus/v5"

	"github.com/joncbenderkh/notifier/notifyd/internal/histfile"
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

const notificationsInterface = "org.freedesktop.Notifications"

// AnswerFunc sends [option] as the answer to the question [id] and returns
// what became of it, to show in place of the question.
type AnswerFunc func(id string, q histfile.Reply, option string) string

// question is a shown notification whose buttons answer a question.
type question struct {
	id    string
	title string
	reply histfile.Reply
	// owner is the service instance that showed it, which alone numbers
	// it and may answer it; "" when a service was started to show it.
	owner string
	gen   uint64 // the connection it was shown over
}

// dbusDesktop calls org.freedesktop.Notifications on the session bus, which
// dunst, mako, GNOME, KDE and others implement. A question's answers become
// actions (buttons) when the service supports them.
type dbusDesktop struct {
	answer AnswerFunc
	now    func() time.Time

	// showMu makes a signal wait for a Notify call in flight, so an action
	// on a question just shown finds it registered.
	showMu sync.Mutex
	mu     sync.Mutex
	conn   *dbus.Conn
	// object addresses the service by a bus name: its current owner's
	// unique name, so a call never reaches a successor that would number
	// the notification differently, or the well-known name when none runs.
	object func(name string) dbus.BusObject
	// nameOwner looks up the service's owner, "" if none.
	nameOwner func(ctx context.Context) (string, error)
	markup    bool
	actions   bool
	// owner is the service's unique bus name: only its signals count, and
	// when it changes, so do notification IDs and capabilities.
	owner     string
	capsStale bool
	pending   map[uint32]question // by notification ID
	// gen counts connections; signals still queued from an earlier one
	// would name owners and notification IDs a new bus may reuse.
	gen uint64
}

func connectDesktop(ctx context.Context, answer AnswerFunc) (Desktop, error) {
	d := &dbusDesktop{answer: answer, now: time.Now, pending: map[uint32]question{}}
	if err := d.connect(ctx); err != nil {
		return nil, err
	}
	return d, nil
}

// connect opens the session bus, again after it went away (a restarted
// bus or session), asks what the service supports, and listens for its
// signals and for another process taking over the service.
func (d *dbusDesktop) connect(ctx context.Context) error {
	// Not dbus.WithContext: that context would bound the connection's
	// whole life, not just the dial. The session bus is a local socket.
	// Signals in order, even when they back up behind a Notify call: an
	// action precedes the close it causes, and owner changes chain.
	conn, err := dbus.ConnectSessionBus(dbus.WithSignalHandler(dbus.NewSequentialSignalHandler()))
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, dbusTimeout)
	defer cancel()
	const path = "/org/freedesktop/Notifications"
	object := func(name string) dbus.BusObject { return conn.Object(name, path) }
	nameOwner := func(ctx context.Context) (string, error) {
		var owner string
		err := conn.BusObject().CallWithContext(ctx, "org.freedesktop.DBus.GetNameOwner", 0, notificationsInterface).Store(&owner)
		var e dbus.Error
		if errors.As(err, &e) && e.Name == "org.freedesktop.DBus.Error.NameHasNoOwner" {
			return "", nil
		}
		return owner, err
	}
	// Subscribed before the owner is looked up, so a change in between is
	// queued and applied after the snapshot below.
	err = conn.AddMatchSignalContext(ctx, dbus.WithMatchObjectPath(path), dbus.WithMatchInterface(notificationsInterface))
	if err == nil {
		err = conn.AddMatchSignalContext(ctx, dbus.WithMatchSender("org.freedesktop.DBus"), dbus.WithMatchMember("NameOwnerChanged"),
			dbus.WithMatchArg(0, notificationsInterface))
	}
	signals := make(chan *dbus.Signal, 64)
	conn.Signal(signals)
	// Through the well-known name first, which starts a service that runs
	// on demand; then it has an owner. The answer may come from an owner
	// that has since left, so the first message asks the one looked up.
	var markup, actions bool
	if err == nil {
		markup, actions, err = capabilities(ctx, object(notificationsInterface))
	}
	var owner string
	if err == nil {
		owner, err = nameOwner(ctx)
	}
	if err != nil {
		conn.Close()
		return err
	}
	d.mu.Lock()
	d.conn, d.object, d.nameOwner = conn, object, nameOwner
	d.markup, d.actions, d.owner, d.capsStale = markup, actions, owner, true
	clear(d.pending) // notification IDs of an earlier service mean nothing to this one
	d.gen++
	gen := d.gen
	d.mu.Unlock()
	// Ends when the connection closes, which closes the channel.
	go func() {
		for sig := range signals {
			d.signal(gen, sig)
		}
	}()
	return nil
}

// capabilities asks whether the service renders body markup and shows actions.
func capabilities(ctx context.Context, obj dbus.BusObject) (markup, actions bool, err error) {
	var caps []string
	if err := obj.CallWithContext(ctx, notificationsInterface+".GetCapabilities", 0).Store(&caps); err != nil {
		return false, false, err
	}
	return slices.Contains(caps, "body-markup"), slices.Contains(caps, "actions"), nil
}

func (d *dbusDesktop) Notify(ctx context.Context, p Payload) error {
	d.mu.Lock()
	connected := d.conn.Connected()
	d.mu.Unlock()
	if !connected {
		if err := d.connect(ctx); err != nil {
			return err
		}
	}
	return d.show(ctx, p)
}

// show calls Notify, and remembers a question shown with buttons.
func (d *dbusDesktop) show(ctx context.Context, p Payload) error {
	d.showMu.Lock()
	defer d.showMu.Unlock()
	ctx, cancel := context.WithTimeout(ctx, dbusTimeout)
	defer cancel()
	err := d.showOnce(ctx, p)
	if !departed(err) {
		return err
	}
	// The owner left and the change went unseen (or is still queued): look
	// it up again, so delivery does not stall on a name nobody holds.
	owner, lerr := d.nameOwner(ctx)
	if lerr != nil {
		return err
	}
	d.mu.Lock()
	d.ownerChanged(owner)
	d.mu.Unlock()
	return d.showOnce(ctx, p)
}

// departed reports a call to a unique name whose owner has left the bus.
func departed(err error) bool {
	var e dbus.Error
	return errors.As(err, &e) &&
		(e.Name == "org.freedesktop.DBus.Error.ServiceUnknown" || e.Name == "org.freedesktop.DBus.Error.NameHasNoOwner")
}

// showOnce calls Notify on the current owner; showMu is held.
func (d *dbusDesktop) showOnce(ctx context.Context, p Payload) error {
	d.mu.Lock()
	var starter dbus.BusObject
	if d.owner == "" {
		starter = d.target("")
	}
	d.mu.Unlock()
	if starter != nil {
		// Calling the well-known name starts a service that runs on
		// demand; the question is then tied to that instance by name.
		if _, _, err := capabilities(ctx, starter); err != nil {
			return err
		}
		found, err := d.nameOwner(ctx)
		if err != nil {
			return err
		}
		d.mu.Lock()
		d.ownerChanged(found)
		d.mu.Unlock()
	}
	d.mu.Lock()
	owner, markup, actions, stale := d.owner, d.markup, d.actions, d.capsStale
	obj := d.target(owner)
	d.mu.Unlock()
	if stale {
		var err error
		if markup, actions, err = capabilities(ctx, obj); err != nil {
			return err
		}
		d.mu.Lock()
		d.markup, d.actions, d.capsStale = markup, actions, false
		d.mu.Unlock()
	}
	n := notification(p, markup, actions, d.now())
	var id uint32
	err := obj.CallWithContext(ctx, notificationsInterface+".Notify", 0,
		"notifier", uint32(0), "", n.summary, n.body, n.actions, n.hints, n.timeout).Store(&id)
	if permanent(err) {
		return fmt.Errorf("%w: %v", errRejected, err)
	}
	if err == nil && len(n.actions) > 0 {
		d.mu.Lock()
		d.pending[id] = question{id: p.ID, title: n.summary, reply: *p.Reply, owner: owner, gen: d.gen}
		d.mu.Unlock()
	}
	return err
}

// ownerChanged records that [owner] now holds the service: questions an
// earlier one showed can no longer be answered, and what it supports is
// asked again. d.mu is held.
func (d *dbusDesktop) ownerChanged(owner string) {
	if owner == d.owner && !d.capsStale {
		return
	}
	d.owner, d.capsStale = owner, true
	for nid, q := range d.pending {
		switch q.owner {
		case "": // shown by the service this started
			q.owner = owner
			d.pending[nid] = q
		case owner:
		default:
			delete(d.pending, nid)
		}
	}
}

// target is the service object to call for [owner]; d.mu is held.
func (d *dbusDesktop) target(owner string) dbus.BusObject {
	if owner == "" {
		return d.object(notificationsInterface)
	}
	return d.object(owner)
}

// signal handles signals: from the service, an invoked action answers its
// question, and a closed notification can no longer be answered; from the
// bus, a new owner of the service makes the questions an earlier one
// showed unanswerable (its notification IDs are its own) and may support
// other things. Any other client could send the same signals, so the
// sender must be the service instance that showed the question, over
// connection [gen].
func (d *dbusDesktop) signal(gen uint64, sig *dbus.Signal) {
	d.showMu.Lock()
	defer d.showMu.Unlock()
	if sig.Name == "org.freedesktop.DBus.NameOwnerChanged" {
		if sig.Sender != "org.freedesktop.DBus" || len(sig.Body) != 3 {
			return
		}
		name, _ := sig.Body[0].(string)
		from, _ := sig.Body[1].(string)
		owner, _ := sig.Body[2].(string)
		d.mu.Lock()
		// A change from another owner predates the one show looked up.
		if gen == d.gen && name == notificationsInterface && from == d.owner {
			d.ownerChanged(owner)
		}
		d.mu.Unlock()
		return
	}
	invoked := sig.Name == notificationsInterface+".ActionInvoked"
	if !invoked && sig.Name != notificationsInterface+".NotificationClosed" || len(sig.Body) < 2 {
		return
	}
	nid, ok := sig.Body[0].(uint32)
	if !ok {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	q, found := d.pending[nid]
	if gen != d.gen || !found || sig.Sender != q.owner {
		return
	}
	if !invoked {
		delete(d.pending, nid)
		return
	}
	key, _ := sig.Body[1].(string)
	i, err := strconv.Atoi(key)
	if err != nil || i < 0 || i >= len(q.reply.Options) {
		return
	}
	delete(d.pending, nid)
	// Off the signal loop: answering is a network call.
	go func() { d.showOutcome(nid, q, d.answer(q.id, q.reply, q.reply.Options[i])) }()
}

// showOutcome shows how answering went, looking the owner up again if it
// left unseen, as show does.
func (d *dbusDesktop) showOutcome(nid uint32, q question, outcome string) {
	d.showMu.Lock()
	defer d.showMu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), dbusTimeout)
	defer cancel()
	if err := d.outcomeOnce(ctx, nid, q, outcome); !departed(err) {
		return
	}
	owner, err := d.nameOwner(ctx)
	if err != nil {
		return
	}
	d.mu.Lock()
	d.ownerChanged(owner)
	d.mu.Unlock()
	d.outcomeOnce(ctx, nid, q, outcome)
}

// outcomeOnce calls Notify with the outcome on the current owner; showMu
// is held.
func (d *dbusDesktop) outcomeOnce(ctx context.Context, nid uint32, q question, outcome string) error {
	d.mu.Lock()
	owner, markup, stale, gen := d.owner, d.markup, d.capsStale, d.gen
	obj := d.target(owner)
	d.mu.Unlock()
	if stale { // another service took over: ask it, and escape if it cannot say
		m, _, err := capabilities(ctx, obj)
		markup = m || err != nil
	}
	// Shown in place of the question, which most services have closed
	// by now; a service that took over since numbers its own.
	replaces := nid
	if owner != q.owner || gen != q.gen {
		replaces = 0
	}
	body := q.title
	if markup {
		body = escapeMarkup(body)
	}
	hints := map[string]dbus.Variant{"urgency": dbus.MakeVariant(urgencyNormal)}
	return obj.CallWithContext(ctx, notificationsInterface+".Notify", 0,
		"notifier", replaces, "", dropNUL(outcome), body, []string{}, hints, int32(-1)).Err
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

// notifyArgs are the message-specific arguments of Notify.
type notifyArgs struct {
	summary, body string
	actions       []string
	hints         map[string]dbus.Variant
	timeout       int32
}

// notification maps a message onto Notify's arguments: high priority is
// critical urgency, and a channel becomes the category "notifier.<channel>",
// so notification daemon rules can match it. An open question's answers
// become actions, keyed by index, and it stays until the question closes; without
// actions, the body says how to answer from a terminal.
func notification(p Payload, markup, actions bool, now time.Time) notifyArgs {
	urgency := urgencyNormal
	if p.Priority == "high" {
		urgency = urgencyCritical
	}
	n := notifyArgs{
		summary: dropNUL(p.Title),
		body:    dropNUL(p.Body),
		actions: []string{},
		hints:   map[string]dbus.Variant{"urgency": dbus.MakeVariant(urgency)},
		timeout: -1,
	}
	if p.Channel != "" {
		n.hints["category"] = dbus.MakeVariant("notifier." + dropNUL(p.Channel))
	}
	if q := p.Reply; q != nil && len(q.Options) > 0 && now.Unix() < q.ExpiresAt {
		if actions {
			for i, o := range q.Options {
				n.actions = append(n.actions, strconv.Itoa(i), dropNUL(o))
			}
			n.timeout = int32(min(q.ExpiresAt-now.Unix(), math.MaxInt32/1000) * 1000)
		} else {
			if n.body != "" {
				n.body += "\n\n"
			}
			n.body += fmt.Sprintf("Answer with: notifyd reply %s ANSWER (%s)", p.ID, dropNUL(strings.Join(q.Options, ", ")))
		}
	}
	if markup {
		n.body = escapeMarkup(n.body)
	}
	return n
}

// dropNUL removes NUL characters, which JSON allows but D-Bus strings
// cannot carry.
func dropNUL(s string) string { return strings.ReplaceAll(s, "\x00", "") }

// escapeMarkup keeps a body from being read as markup by daemons that
// support it; the summary is plain text by specification.
var escapeMarkup = strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;").Replace
