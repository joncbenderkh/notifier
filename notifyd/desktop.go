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

// ReceiptFunc reports that the message [id] was shown or opened (see
// [receiptShown], [receiptOpened]). It is called off the signal loop.
type ReceiptFunc func(id, state string)

const (
	receiptShown  = "shown"
	receiptOpened = "opened"
	// openAction is the action a click on the notification invokes.
	openAction = "default"
	// linkAction prefixes the key of a button that opens a link, by index.
	linkAction = "link:"
)

// question is a shown notification with actions: buttons that answer its
// question, if it asks one, or open its links, and a click that opens it,
// if it has a link or its sender asked for receipts.
type question struct {
	id       string
	title    string
	reply    histfile.Reply
	receipts bool
	// url is opened by [openAction]; links by the buttons that open one,
	// by index, "" for one not offered.
	url   string
	links []string
	// markable: it was offered [openAction].
	markable bool
	shown    time.Time
	// owner is the service instance that showed it, which alone numbers
	// it and may answer it; "" when a service was started to show it.
	owner string
	gen   uint64 // the connection it was shown over
	// scope is its key's [keyScope], "" for none.
	scope string
}

// keyedNote is the notification last shown for a key.
type keyedNote struct {
	nid   uint32
	id    string // the message's
	shown time.Time
	owner string // as in [question]
	gen   uint64
}

// keyScope names [p]'s key within its channel, since only a channel's
// owner publishes to it and only the device's own user sends directly;
// "" when it has none.
func keyScope(p Payload) string {
	if p.Key == "" {
		return ""
	}
	channel := p.ChannelID
	if channel == "" {
		channel = "direct"
	}
	return channel + "/" + p.Key
}

// dbusDesktop calls org.freedesktop.Notifications on the session bus, which
// dunst, mako, GNOME, KDE and others implement. A question's answers become
// actions (buttons) when the service supports them.
type dbusDesktop struct {
	answer  AnswerFunc
	receipt ReceiptFunc
	open    OpenFunc
	now     func() time.Time
	// sounds are the sound files by name (see [sound]); none, no sound hint.
	sounds map[string]string

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
	// keyed is what a keyed message replaces, by [keyScope].
	keyed map[string]keyedNote
	// answering holds the notifications whose answer is on its way: by
	// ID, the connection they were shown over. The outcome takes their
	// place, so a keyed message does not.
	answering map[uint32]uint64
	// gen counts connections; signals still queued from an earlier one
	// would name owners and notification IDs a new bus may reuse.
	gen uint64
}

func connectDesktop(ctx context.Context, answer AnswerFunc, receipt ReceiptFunc, open OpenFunc, sounds map[string]string) (Desktop, error) {
	d := &dbusDesktop{answer: answer, receipt: receipt, open: open, now: time.Now, sounds: sounds, pending: map[uint32]question{}, keyed: map[string]keyedNote{}}
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
	// Notification IDs of an earlier service mean nothing to this one.
	clear(d.pending)
	clear(d.keyed)
	clear(d.answering)
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
	now := d.now()
	n := notification(p, markup, actions, now)
	if path, ok := d.sounds[sound(p, now)]; ok {
		n.hints["sound-file"] = dbus.MakeVariant(path)
	}
	scope := keyScope(p)
	var replaces, closing uint32
	d.mu.Lock()
	if k, ok := d.keyed[scope]; ok && scope != "" && k.owner == owner && k.gen == d.gen {
		_, offers := d.pending[k.nid]
		g, answering := d.answering[k.nid]
		switch {
		case answering && g == d.gen: // its outcome takes its place
		case offers || len(n.actions) > 0:
			// Actions are keyed by index alone, so a click on the old one
			// still queued would reach this one in its place, and a close
			// of the old one still queued would take this one's actions:
			// it is closed instead, and this one shown anew.
			closing = k.nid
		default:
			replaces = k.nid
		}
	}
	d.mu.Unlock()
	var id uint32
	err := obj.CallWithContext(ctx, notificationsInterface+".Notify", 0,
		"notifier", replaces, "", n.summary, n.body, n.actions, n.hints, n.timeout).Store(&id)
	if permanent(err) {
		return fmt.Errorf("%w: %v", errRejected, err)
	}
	if err == nil && closing != 0 {
		// Best effort: left showing, it is just one more notification.
		obj.CallWithContext(ctx, notificationsInterface+".CloseNotification", 0, closing)
	}
	if err == nil && scope != "" {
		d.mu.Lock()
		// Not an earlier notification's, whose ID the service reused.
		delete(d.pending, id)
		d.rememberKeyed(scope, keyedNote{nid: id, id: p.ID, shown: now, owner: owner, gen: d.gen})
		d.mu.Unlock()
	}
	if err == nil && len(n.actions) > 0 {
		q := question{
			id: p.ID, title: n.summary, receipts: p.Receipts, markable: slices.Contains(n.actions, openAction),
			shown: now, owner: owner, gen: d.gen, scope: scope,
		}
		if webLink(p.URL) {
			q.url = p.URL
		}
		for _, a := range p.Actions {
			link := ""
			if webLink(a.URL) {
				link = a.URL
			}
			q.links = append(q.links, link)
		}
		if p.Reply != nil {
			q.reply = *p.Reply
		}
		d.mu.Lock()
		d.evictUnanswerable(now)
		d.pending[id] = q
		d.mu.Unlock()
	}
	if err == nil && p.Receipts && d.receipt != nil {
		go d.receipt(p.ID, receiptShown)
	}
	return err
}

// maxPending bounds the notifications kept for their actions: a service
// may keep a message without answers in a tray, never closing it.
const maxPending = 500

// evictUnanswerable makes room for one more notification, dropping the
// oldest whose question is closed or that has none. d.mu is held.
func (d *dbusDesktop) evictUnanswerable(now time.Time) {
	if len(d.pending) < maxPending {
		return
	}
	var oldest uint32
	found := false
	for nid, q := range d.pending {
		if len(q.reply.Options) > 0 && now.Unix() < q.reply.ExpiresAt {
			continue
		}
		if !found || q.shown.Before(d.pending[oldest].shown) {
			oldest, found = nid, true
		}
	}
	if found {
		delete(d.pending, oldest)
	}
}

// rememberKeyed records [k] as what [scope]'s next message replaces,
// keeping at most [maxPending]. d.mu is held.
func (d *dbusDesktop) rememberKeyed(scope string, k keyedNote) {
	if _, ok := d.keyed[scope]; !ok && len(d.keyed) >= maxPending {
		oldest := ""
		for s, o := range d.keyed {
			if oldest == "" || o.shown.Before(d.keyed[oldest].shown) {
				oldest = s
			}
		}
		delete(d.keyed, oldest)
	}
	if d.keyed == nil {
		d.keyed = map[string]keyedNote{}
	}
	d.keyed[scope] = k
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
	for scope, k := range d.keyed {
		switch k.owner {
		case "":
			k.owner = owner
			d.keyed[scope] = k
		case owner:
		default:
			delete(d.keyed, scope)
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
// question or, for a click, reports the message opened, and a closed
// notification can no longer be answered; from the
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
	if !invoked && gen == d.gen {
		// A closed ID may be reused for another notification, perhaps
		// another program's, which a keyed message must not replace.
		for scope, k := range d.keyed {
			if k.nid == nid && k.owner == sig.Sender {
				delete(d.keyed, scope)
			}
		}
	}
	q, found := d.pending[nid]
	if gen != d.gen || !found || sig.Sender != q.owner {
		return
	}
	if !invoked {
		delete(d.pending, nid)
		return
	}
	key, _ := sig.Body[1].(string)
	if key == openAction {
		// Some services send it on a click whether offered or not; an
		// open question, which is not offered it, still awaits its answer.
		if !q.markable {
			return
		}
		// Offered only without answers; its link buttons, if any, stay
		// clickable where the service keeps it showing.
		if !slices.ContainsFunc(q.links, func(l string) bool { return l != "" }) {
			delete(d.pending, nid)
		}
		d.opened(q, q.url)
		return
	}
	if l, ok := strings.CutPrefix(key, linkAction); ok {
		i, err := strconv.Atoi(l)
		if err != nil || i < 0 || i >= len(q.links) || q.links[i] == "" {
			return
		}
		// Kept: a service that leaves it showing may click another.
		d.opened(q, q.links[i])
		return
	}
	i, err := strconv.Atoi(key)
	if err != nil || i < 0 || i >= len(q.reply.Options) {
		return
	}
	delete(d.pending, nid)
	if d.answering == nil {
		d.answering = map[uint32]uint64{}
	}
	d.answering[nid] = q.gen
	// The server counts an answer it takes as opening the message, but one
	// that expired here never reaches it; reporting every answer is simpler
	// than telling them apart, and the server ignores a repeat.
	if q.receipts && d.receipt != nil {
		go d.receipt(q.id, receiptOpened)
	}
	// Off the signal loop: answering is a network call.
	go func() { d.showOutcome(nid, q, d.answer(q.id, q.reply, q.reply.Options[i])) }()
}

// opened reports q opened, if its sender asked, and opens link, if any;
// both off the signal loop. d.mu is held.
func (d *dbusDesktop) opened(q question, link string) {
	if q.receipts && d.receipt != nil {
		go d.receipt(q.id, receiptOpened)
	}
	if link != "" && webLink(link) && d.open != nil {
		go d.open(link)
	}
}

// showOutcome shows how answering went, looking the owner up again if it
// left unseen, as show does.
func (d *dbusDesktop) showOutcome(nid uint32, q question, outcome string) {
	d.showMu.Lock()
	defer d.showMu.Unlock()
	defer func() {
		d.mu.Lock()
		if g, ok := d.answering[nid]; ok && g == q.gen {
			delete(d.answering, nid)
		}
		d.mu.Unlock()
	}()
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
	// Quiet: it answers a click.
	hints := map[string]dbus.Variant{"urgency": dbus.MakeVariant(urgencyNormal), "suppress-sound": dbus.MakeVariant(true)}
	var id uint32
	err := obj.CallWithContext(ctx, notificationsInterface+".Notify", 0,
		"notifier", replaces, "", dropNUL(outcome), body, []string{}, hints, int32(-1)).Store(&id)
	d.mu.Lock()
	defer d.mu.Unlock()
	// The next message with its key replaces the outcome, unless one came
	// since the question.
	if k, ok := d.keyed[q.scope]; err == nil && q.scope != "" && (!ok || k.id == q.id) && gen == d.gen {
		d.rememberKeyed(q.scope, keyedNote{nid: id, id: q.id, shown: d.now(), owner: owner, gen: gen})
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
	q := p.Reply
	open := q != nil && len(q.Options) > 0 && now.Unix() < q.ExpiresAt
	// An open question's answers report the opening (the server records
	// it), and a default action would take the place of dunst's menu of them.
	switch {
	case !actions || open:
	case webLink(p.URL):
		n.actions = append(n.actions, openAction, "Open")
	case p.Receipts:
		// Clicking it is what counts; it opens nothing.
		n.actions = append(n.actions, openAction, "Mark seen")
	}
	// The server sends no buttons with a question: its answers are the buttons.
	if actions && !open {
		for i, a := range p.Actions {
			if webLink(a.URL) {
				n.actions = append(n.actions, linkAction+strconv.Itoa(i), dropNUL(a.Label))
			}
		}
	}
	if !actions || open {
		n.body = withLinks(n.body, p)
	}
	if open {
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

// withLinks adds a message's links to body, for a service that shows no
// actions, or an open question, whose actions are its answers.
func withLinks(body string, p Payload) string {
	var lines []string
	if webLink(p.URL) {
		lines = append(lines, dropNUL(p.URL))
	}
	for _, a := range p.Actions {
		if webLink(a.URL) {
			lines = append(lines, dropNUL(a.Label)+": "+dropNUL(a.URL))
		}
	}
	if len(lines) == 0 {
		return body
	}
	if body != "" {
		body += "\n\n"
	}
	return body + strings.Join(lines, "\n")
}

// sound names the sound for a message: a question still open, then high
// priority, then any other.
func sound(p Payload, now time.Time) string {
	switch {
	case p.Reply != nil && len(p.Reply.Options) > 0 && now.Unix() < p.Reply.ExpiresAt:
		return "question"
	case p.Priority == "high":
		return "urgent"
	default:
		return "message"
	}
}

// dropNUL removes NUL characters, which JSON allows but D-Bus strings
// cannot carry.
func dropNUL(s string) string { return strings.ReplaceAll(s, "\x00", "") }

// escapeMarkup keeps a body from being read as markup by daemons that
// support it; the summary is plain text by specification.
var escapeMarkup = strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;").Replace
