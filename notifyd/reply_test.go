// SPDX-License-Identifier: GPL-3.0-or-later

package main

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"encoding/base64"
	"errors"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/godbus/dbus/v5"

	"github.com/joncbenderkh/notifier/notifyd/internal/histfile"
)

// The shared reply vector, asserted by server/test/seal.test.ts and
// notify/ask_test.go too.
const (
	replyVectorKey        = "AAECAwQFBgcICQoLDA0ODxAREhMUFRYXGBkaGxwdHh8="
	replyVectorNonce      = "MDEyMzQ1Njc4OWFi"
	replyVectorCiphertext = "TDMzHSVQ7HW1BXHCzRiocGE39XiLrrhG+LEwUrTqrw8="
)

func TestSealsTheSharedReplyVector(t *testing.T) {
	replyNonce, _ = base64.StdEncoding.DecodeString(replyVectorNonce)
	t.Cleanup(func() { replyNonce = nil })
	n, c, err := sealReply(replyVectorKey, "msg_test", "yes")
	if err != nil || n != replyVectorNonce || c != replyVectorCiphertext {
		t.Fatalf("got %q %q, %v", n, c, err)
	}
}

// openTestReply opens what sealReply produced, as the asker does.
func openTestReply(t *testing.T, key, id, n, c string) string {
	t.Helper()
	k, _ := base64.StdEncoding.DecodeString(key)
	nonce, _ := base64.StdEncoding.DecodeString(n)
	ct, _ := base64.StdEncoding.DecodeString(c)
	block, _ := aes.NewCipher(k)
	gcm, _ := cipher.NewGCM(block)
	plaintext, err := gcm.Open(nil, nonce, ct, []byte("reply:"+id))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	return string(plaintext)
}

var testQuestion = histfile.Reply{Options: []string{"yes", "no"}, Key: replyVectorKey, ExpiresAt: testNow + 600}

func TestAnswersAQuestion(t *testing.T) {
	api := newFakeAPI(t)
	a, _, _ := newTestApp(t)
	st := State{URL: api.URL, DeviceID: "dev_new", DeviceToken: "nd_new", SealKey: testKey}

	api.status["POST /v1/replies/msg_q"] = http.StatusCreated
	if outcome, ok := a.answer(st, "msg_q", testQuestion, "no"); !ok || outcome != "Answered: no" {
		t.Fatalf("%q %v", outcome, ok)
	}
	if api.requests[0] != "POST /v1/replies/msg_q Bearer nd_new" {
		t.Fatalf("requests %q", api.requests)
	}
	if got := openTestReply(t, replyVectorKey, "msg_q", api.bodies[0]["n"], api.bodies[0]["c"]); got != `{"answer":"no"}` {
		t.Fatalf("sealed %s", got)
	}

	for status, want := range map[int]string{
		http.StatusConflict:            "Already answered",
		http.StatusGone:                "The question has expired",
		http.StatusNotFound:            "This desktop may no longer answer",
		http.StatusInternalServerError: "Could not send the answer: ",
	} {
		api.status["POST /v1/replies/msg_q"] = status
		if outcome, ok := a.answer(st, "msg_q", testQuestion, "yes"); ok || !strings.HasPrefix(outcome, want) {
			t.Errorf("%d: %q %v", status, outcome, ok)
		}
	}

	sent := len(api.requests)
	if outcome, ok := a.answer(st, "msg_q", testQuestion, "maybe"); ok || !strings.Contains(outcome, "not one of the answers") {
		t.Errorf("%q %v", outcome, ok)
	}
	expired := testQuestion
	expired.ExpiresAt = testNow
	if outcome, ok := a.answer(st, "msg_q", expired, "yes"); ok || outcome != "The question has expired" {
		t.Errorf("%q %v", outcome, ok)
	}
	if len(api.requests) != sent {
		t.Errorf("sent %q", api.requests[sent:])
	}
}

func TestReplyAnswersFromTheHistory(t *testing.T) {
	api := newFakeAPI(t)
	api.status["POST /v1/replies/msg_q"] = http.StatusCreated
	a, stdout, stderr := newTestApp(t)
	if err := saveState(a.statePath, State{URL: api.URL, DeviceID: "dev_new", DeviceToken: "nd_new", SealKey: testKey}); err != nil {
		t.Fatal(err)
	}
	h := newHistory(a.historyPath, a.now)
	q := testQuestion
	for _, p := range []Payload{
		{ID: "msg_q", Title: "Deploy?", Priority: "default", SentAt: testNow, Reply: &q},
		{ID: "msg_plain", Title: "Hi", Priority: "default", SentAt: testNow},
	} {
		if err := h.record(p); err != nil {
			t.Fatal(err)
		}
	}

	if code := a.run([]string{"history"}); code != 0 || !strings.Contains(stdout.String(), "answers: yes | no  (notifyd reply msg_q ANSWER)") {
		t.Fatalf("exit %d: %s%s", code, stdout, stderr)
	}
	stdout.Reset()
	if code := a.run([]string{"reply", "msg_q", "yes"}); code != 0 || stdout.String() != "Answered: yes\n" {
		t.Fatalf("exit %d: %s%s", code, stdout, stderr)
	}
	q.Options = []string{"--help", "-n"}
	if err := h.record(Payload{ID: "msg_dash", Title: "Flags?", Priority: "default", SentAt: testNow, Reply: &q}); err != nil {
		t.Fatal(err)
	}
	api.status["POST /v1/replies/msg_dash"] = http.StatusCreated
	stdout.Reset()
	if code := a.run([]string{"reply", "msg_dash", "--help"}); code != 0 || stdout.String() != "Answered: --help\n" {
		t.Fatalf("exit %d: %s%s", code, stdout, stderr)
	}
	for _, args := range [][]string{{"reply", "msg_plain", "yes"}, {"reply", "msg_missing", "yes"}, {"reply", "msg_q", "maybe"}} {
		if code := a.run(args); code != exitError {
			t.Errorf("%q: exit %d", args, code)
		}
	}
	if len(api.requests) != 2 {
		t.Errorf("requests %q", api.requests)
	}
}

func TestOffersAnswersAsActions(t *testing.T) {
	now := time.Unix(testNow, 0)
	q := testQuestion
	p := Payload{ID: "msg_q", Title: "Deploy?", Body: "v1 < v2", Priority: "default", Reply: &q}

	n := notification(p, true, true, now)
	if !slices.Equal(n.actions, []string{"0", "yes", "1", "no"}) || n.timeout != 600_000 || n.body != "v1 &lt; v2" {
		t.Fatalf("%+v", n)
	}
	n = notification(p, false, false, now)
	if len(n.actions) != 0 || n.body != "v1 < v2\n\nAnswer with: notifyd reply msg_q ANSWER (yes, no)" {
		t.Fatalf("%+v", n)
	}
	if n = notification(p, false, true, now.Add(10*time.Minute)); len(n.actions) != 0 || n.timeout != -1 || n.body != "v1 < v2" {
		t.Fatalf("expired: %+v", n)
	}
}

// fakeBus records Notify calls and numbers the notifications.
type fakeBus struct {
	dbus.BusObject
	mu    sync.Mutex
	calls chan []any
	next  uint32
	caps  []string
	err   error // what Notify fails with, if set
}

func (b *fakeBus) CallWithContext(_ context.Context, method string, _ dbus.Flags, args ...any) *dbus.Call {
	b.mu.Lock()
	defer b.mu.Unlock()
	if method == notificationsInterface+".GetCapabilities" {
		return &dbus.Call{Body: []any{b.caps}}
	}
	b.next++
	b.calls <- args
	if b.err != nil {
		return &dbus.Call{Err: b.err}
	}
	return &dbus.Call{Body: []any{b.next}}
}

func (b *fakeBus) call(t *testing.T) []any {
	t.Helper()
	select {
	case args := <-b.calls:
		return args
	case <-time.After(5 * time.Second):
		t.Fatal("no Notify call")
		return nil
	}
}

func TestAnswersFromAnInvokedAction(t *testing.T) {
	bus := &fakeBus{calls: make(chan []any, 10)}
	var answered []string
	var mu sync.Mutex
	d := &dbusDesktop{
		object:  func(string) dbus.BusObject { return bus },
		actions: true,
		owner:   ":1.5",
		now:     func() time.Time { return time.Unix(testNow, 0) },
		pending: map[uint32]question{},
		answer: func(id string, q histfile.Reply, option string) string {
			mu.Lock()
			defer mu.Unlock()
			answered = append(answered, id+"="+option)
			return "Answered: " + option
		},
	}
	q := testQuestion
	ask := func(id string) {
		t.Helper()
		if err := d.show(context.Background(), Payload{ID: id, Title: "Deploy?", Reply: &q}); err != nil {
			t.Fatal(err)
		}
		bus.call(t)
	}
	from := func(sender, member string, body ...any) {
		d.signal(0, &dbus.Signal{Sender: sender, Name: notificationsInterface + "." + member, Body: body})
	}
	ask("msg_a") // 1
	ask("msg_b") // 2

	from(":1.5", "NotificationClosed", uint32(2), uint32(2))
	from(":1.5", "ActionInvoked", uint32(2), "0") // closed: gone
	from(":1.9", "ActionInvoked", uint32(1), "0") // not the service
	from(":1.5", "ActionInvoked", uint32(9), "0") // not ours
	from(":1.5", "ActivationToken", uint32(1), "token")
	from(":1.5", "ActionInvoked", uint32(1), "7") // no such answer
	from(":1.5", "ActionInvoked", uint32(1), "1")
	from(":1.5", "ActionInvoked", uint32(1), "0") // answered already
	args := bus.call(t)
	if args[1] != uint32(1) || args[3] != "Answered: no" || args[4] != "Deploy?" {
		t.Fatalf("replacement %v", args)
	}
	mu.Lock()
	if !slices.Equal(answered, []string{"msg_a=no"}) {
		t.Fatalf("answered %q", answered)
	}
	mu.Unlock()

	// A new service: the questions shown are not its own, and what it
	// supports is asked again.
	ask("msg_c") // 4
	bus.caps = []string{"body"}
	d.signal(0, &dbus.Signal{Sender: ":1.9", Name: "org.freedesktop.DBus.NameOwnerChanged", Body: []any{notificationsInterface, ":1.5", ":1.9"}})
	if len(d.pending) != 1 {
		t.Fatal("a spoofed NameOwnerChanged was taken")
	}
	d.signal(0, &dbus.Signal{Sender: "org.freedesktop.DBus", Name: "org.freedesktop.DBus.NameOwnerChanged", Body: []any{notificationsInterface, ":1.5", ":1.9"}})
	if len(d.pending) != 0 || d.owner != ":1.9" {
		t.Fatalf("pending %v, owner %q", d.pending, d.owner)
	}
	if err := d.show(context.Background(), Payload{ID: "msg_d", Title: "Deploy?", Reply: &q}); err != nil {
		t.Fatal(err)
	}
	if args := bus.call(t); len(args[5].([]string)) != 0 || len(d.pending) != 0 {
		t.Fatalf("actions %v without the capability", args[5])
	}
}

// raceBus delivers an action on the question before its Notify call returns.
type raceBus struct {
	fakeBus
	d *dbusDesktop
}

func (b *raceBus) CallWithContext(ctx context.Context, method string, flags dbus.Flags, args ...any) *dbus.Call {
	call := b.fakeBus.CallWithContext(ctx, method, flags, args...)
	if method == notificationsInterface+".Notify" && args[1] == uint32(0) {
		done := make(chan struct{})
		go func() {
			b.d.signal(0, &dbus.Signal{Sender: ":1.5", Name: notificationsInterface + ".ActionInvoked", Body: []any{call.Body[0], "0"}})
			close(done)
		}()
		select {
		case <-done:
		case <-time.After(50 * time.Millisecond): // waiting for show to finish, as it should
		}
	}
	return call
}

func TestAnswersAnActionThatOutrunsItsNotifyCall(t *testing.T) {
	bus := &raceBus{fakeBus: fakeBus{calls: make(chan []any, 10)}}
	answered := make(chan string, 1)
	d := &dbusDesktop{
		object: func(string) dbus.BusObject { return bus }, actions: true, owner: ":1.5", pending: map[uint32]question{},
		now:    func() time.Time { return time.Unix(testNow, 0) },
		answer: func(_ string, _ histfile.Reply, option string) string { answered <- option; return "ok" },
	}
	bus.d = d
	q := testQuestion
	if err := d.show(context.Background(), Payload{ID: "msg_a", Title: "Deploy?", Reply: &q}); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-answered:
		if got != "yes" {
			t.Fatalf("answered %q", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the action was lost")
	}
}

// TestTiesQuestionsToTheServiceThatShowedThem checks that calls go to the
// owner's unique name, which a successor never answers; that a call to an
// owner that left looks the owner up again rather than stalling; and that
// each question stays answerable only by the instance that showed it.
func TestTiesQuestionsToTheServiceThatShowedThem(t *testing.T) {
	bus := &fakeBus{calls: make(chan []any, 10), caps: []string{"actions"}}
	var mu sync.Mutex
	var names []string
	lookup := ""
	answered := make(chan string, 4)
	d := &dbusDesktop{
		object: func(name string) dbus.BusObject {
			mu.Lock()
			defer mu.Unlock()
			names = append(names, name)
			if name == ":1.5" || name == ":1.8" {
				return goneBus{} // exited, but notifyd has not heard yet
			}
			return bus
		},
		nameOwner: func(context.Context) (string, error) { return lookup, nil },
		actions:   true, owner: ":1.5", pending: map[uint32]question{},
		now:    func() time.Time { return time.Unix(testNow, 0) },
		answer: func(id string, _ histfile.Reply, option string) string { answered <- id + "=" + option; return "ok" },
	}
	q := testQuestion
	show := func(id string) {
		t.Helper()
		if err := d.show(context.Background(), Payload{ID: id, Title: "Deploy?", Reply: &q}); err != nil {
			t.Fatal(err)
		}
		bus.call(t)
	}
	owner := func(old, new string) {
		d.signal(0, &dbus.Signal{Sender: "org.freedesktop.DBus", Name: "org.freedesktop.DBus.NameOwnerChanged",
			Body: []any{notificationsInterface, old, new}})
	}

	// The owner left unseen: looked up again (none), the well-known name
	// starts a service, whose question it is once that shows up.
	show("msg_a")
	owner(":1.5", "") // the change that was missed, arriving late
	owner("", ":1.7")
	d.signal(0, &dbus.Signal{Sender: ":1.7", Name: notificationsInterface + ".ActionInvoked", Body: []any{uint32(1), "0"}})
	if got := <-answered; got != "msg_a=yes" {
		t.Fatalf("answered %q", got)
	}
	bus.call(t) // the outcome

	show("msg_c")
	owner(":1.7", ":1.8")
	if len(d.pending) != 0 {
		t.Fatalf("kept %v from a service that exited", d.pending)
	}

	// The next owner left and another took over, both unseen: the lookup
	// finds the new one, and the late changes leave its question alone.
	lookup = ":1.9"
	show("msg_d")
	owner(":1.8", "")
	owner("", ":1.9")
	if _, ok := d.pending[4]; !ok {
		t.Fatal("dropped the question the new owner showed")
	}
	d.signal(0, &dbus.Signal{Sender: ":1.9", Name: notificationsInterface + ".ActionInvoked", Body: []any{uint32(4), "1"}})
	if got := <-answered; got != "msg_d=no" {
		t.Fatalf("answered %q", got)
	}
	bus.call(t)
	mu.Lock()
	defer mu.Unlock()
	if !slices.Equal(names, []string{":1.5", notificationsInterface, notificationsInterface, ":1.7", ":1.7", ":1.8", ":1.9", ":1.9"}) {
		t.Fatalf("called %q", names)
	}
}

// goneBus is a unique name whose owner has left the bus.
type goneBus struct{ dbus.BusObject }

func (goneBus) CallWithContext(context.Context, string, dbus.Flags, ...any) *dbus.Call {
	return &dbus.Call{Err: dbus.Error{Name: "org.freedesktop.DBus.Error.ServiceUnknown"}}
}

// What the service supports is asked of the owner looked up, since the
// one that answered while connecting may have left.
func TestAsksTheOwnerWhatItSupports(t *testing.T) {
	old := &fakeBus{calls: make(chan []any, 1)}
	bus := &fakeBus{calls: make(chan []any, 1), caps: []string{"actions"}}
	d := &dbusDesktop{
		object: func(name string) dbus.BusObject {
			if name == ":1.7" {
				return bus
			}
			return old
		},
		owner: ":1.7", capsStale: true, pending: map[uint32]question{},
		now: func() time.Time { return time.Unix(testNow, 0) },
	}
	q := testQuestion
	if err := d.show(context.Background(), Payload{ID: "msg_a", Title: "Deploy?", Reply: &q}); err != nil {
		t.Fatal(err)
	}
	if args := bus.call(t); len(args[5].([]string)) == 0 || len(d.pending) != 1 {
		t.Fatalf("shown without buttons: %v", args)
	}
}

// A service started on demand is looked up before Notify, so changes
// queued meanwhile (one instance started and left) leave its question.
func TestPinsAServiceStartedOnDemand(t *testing.T) {
	bus := &fakeBus{calls: make(chan []any, 2), caps: []string{"actions"}}
	answered := make(chan string, 1)
	d := &dbusDesktop{
		object:    func(string) dbus.BusObject { return bus },
		nameOwner: func(context.Context) (string, error) { return ":1.8", nil },
		capsStale: true, pending: map[uint32]question{},
		now:    func() time.Time { return time.Unix(testNow, 0) },
		answer: func(id string, _ histfile.Reply, option string) string { answered <- id + "=" + option; return "ok" },
	}
	q := testQuestion
	if err := d.show(context.Background(), Payload{ID: "msg_a", Title: "Deploy?", Reply: &q}); err != nil {
		t.Fatal(err)
	}
	bus.call(t)
	for _, c := range [][2]string{{"", ":1.7"}, {":1.7", ""}, {"", ":1.8"}} {
		d.signal(0, &dbus.Signal{Sender: "org.freedesktop.DBus", Name: "org.freedesktop.DBus.NameOwnerChanged",
			Body: []any{notificationsInterface, c[0], c[1]}})
	}
	d.signal(0, &dbus.Signal{Sender: ":1.8", Name: notificationsInterface + ".ActionInvoked", Body: []any{uint32(1), "0"}})
	select {
	case got := <-answered:
		if got != "msg_a=yes" {
			t.Fatalf("answered %q", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("not answered; pending %v", d.pending)
	}
}

// An outcome shown by a service that took over while the answer was on
// its way follows what that service supports.
func TestOutcomeAsksANewServiceForMarkup(t *testing.T) {
	bus := &fakeBus{calls: make(chan []any, 2), caps: []string{"body-markup"}}
	release := make(chan struct{})
	d := &dbusDesktop{
		object: func(string) dbus.BusObject { return bus },
		owner:  ":1.5", pending: map[uint32]question{7: {id: "msg_a", title: "<b>Deploy?</b>", reply: testQuestion, owner: ":1.5"}},
		answer: func(string, histfile.Reply, string) string { <-release; return "Answered: yes" },
	}
	d.signal(0, &dbus.Signal{Sender: ":1.5", Name: notificationsInterface + ".ActionInvoked", Body: []any{uint32(7), "0"}})
	d.signal(0, &dbus.Signal{Sender: "org.freedesktop.DBus", Name: "org.freedesktop.DBus.NameOwnerChanged",
		Body: []any{notificationsInterface, ":1.5", ":1.6"}})
	close(release)
	if args := bus.call(t); args[1] != uint32(0) || args[4] != "&lt;b&gt;Deploy?&lt;/b&gt;" {
		t.Fatalf("outcome %v", args)
	}
}

// An outcome for a service that left unseen while the answer was on its
// way goes to the owner looked up again, as a new notification.
func TestOutcomeFollowsAServiceThatLeftUnseen(t *testing.T) {
	bus := &fakeBus{calls: make(chan []any, 1), caps: []string{"actions"}}
	d := &dbusDesktop{
		object: func(name string) dbus.BusObject {
			if name == ":1.5" {
				return goneBus{}
			}
			return bus
		},
		nameOwner: func(context.Context) (string, error) { return ":1.6", nil },
		owner:     ":1.5", pending: map[uint32]question{7: {id: "msg_a", title: "Deploy?", reply: testQuestion, owner: ":1.5"}},
		answer: func(string, histfile.Reply, string) string { return "Answered: yes" },
	}
	d.signal(0, &dbus.Signal{Sender: ":1.5", Name: notificationsInterface + ".ActionInvoked", Body: []any{uint32(7), "0"}})
	if args := bus.call(t); args[1] != uint32(0) || args[3] != "Answered: yes" {
		t.Fatalf("outcome %v", args)
	}
}

// Signals left queued from a connection that closed are ignored: a
// restarted bus can give the service the same unique name, and the
// service the same notification IDs.
func TestIgnoresSignalsFromAnEarlierConnection(t *testing.T) {
	answered := make(chan string, 1)
	d := &dbusDesktop{
		object: func(string) dbus.BusObject { return &fakeBus{calls: make(chan []any, 1)} },
		owner:  ":1.5", gen: 1, pending: map[uint32]question{1: {id: "msg_new", reply: testQuestion, owner: ":1.5"}},
		answer: func(id string, _ histfile.Reply, option string) string { answered <- id + "=" + option; return "ok" },
	}
	d.signal(0, &dbus.Signal{Sender: "org.freedesktop.DBus", Name: "org.freedesktop.DBus.NameOwnerChanged",
		Body: []any{notificationsInterface, ":1.5", ":1.6"}})
	d.signal(0, &dbus.Signal{Sender: ":1.5", Name: notificationsInterface + ".ActionInvoked", Body: []any{uint32(1), "1"}})
	if d.owner != ":1.5" || len(d.pending) != 1 {
		t.Fatalf("owner %q, pending %v", d.owner, d.pending)
	}
	d.signal(1, &dbus.Signal{Sender: ":1.5", Name: notificationsInterface + ".ActionInvoked", Body: []any{uint32(1), "0"}})
	if got := <-answered; got != "msg_new=yes" {
		t.Fatalf("answered %q", got)
	}
}

// An outcome arriving after a reconnect is a new notification: the new
// bus may give the service the same name, and it may reuse the ID.
func TestOutcomeAfterAReconnectReplacesNothing(t *testing.T) {
	bus := &fakeBus{calls: make(chan []any, 1)}
	release := make(chan struct{})
	d := &dbusDesktop{
		object: func(string) dbus.BusObject { return bus },
		owner:  ":1.5", pending: map[uint32]question{7: {id: "msg_a", title: "Deploy?", reply: testQuestion, owner: ":1.5"}},
		answer: func(string, histfile.Reply, string) string { <-release; return "Answered: yes" },
	}
	d.signal(0, &dbus.Signal{Sender: ":1.5", Name: notificationsInterface + ".ActionInvoked", Body: []any{uint32(7), "0"}})
	d.mu.Lock()
	d.gen++ // reconnected; the service came back as :1.5
	d.mu.Unlock()
	close(release)
	if args := bus.call(t); args[1] != uint32(0) {
		t.Fatalf("outcome %v", args)
	}
}

func TestPicksASoundAndKeepsOutcomesQuiet(t *testing.T) {
	bus := &fakeBus{calls: make(chan []any, 10)}
	d := &dbusDesktop{
		object:  func(string) dbus.BusObject { return bus },
		actions: true,
		owner:   ":1.5",
		now:     func() time.Time { return time.Unix(testNow, 0) },
		pending: map[uint32]question{},
		sounds:  map[string]string{"message": "/m.wav", "urgent": "/u.wav", "question": "/q.wav"},
		answer:  func(string, histfile.Reply, string) string { return "Answered: yes" },
	}
	q := testQuestion
	closed := q
	closed.ExpiresAt = testNow
	for _, c := range []struct {
		p    Payload
		want string
	}{
		{Payload{ID: "a", Title: "Hi"}, "/m.wav"},
		{Payload{ID: "b", Title: "Disk", Priority: "high"}, "/u.wav"},
		{Payload{ID: "c", Title: "Deploy?", Priority: "high", Reply: &q}, "/q.wav"},
		{Payload{ID: "d", Title: "Deploy?", Reply: &closed}, "/m.wav"},
	} {
		if err := d.show(context.Background(), c.p); err != nil {
			t.Fatal(err)
		}
		hints := bus.call(t)[6].(map[string]dbus.Variant)
		if hints["sound-file"] != dbus.MakeVariant(c.want) {
			t.Errorf("%s: sound %v, want %s", c.p.ID, hints["sound-file"], c.want)
		}
	}
	d.signal(0, &dbus.Signal{Sender: ":1.5", Name: notificationsInterface + ".ActionInvoked", Body: []any{uint32(3), "0"}})
	hints := bus.call(t)[6].(map[string]dbus.Variant)
	if _, ok := hints["sound-file"]; ok || hints["suppress-sound"] != dbus.MakeVariant(true) {
		t.Fatalf("outcome hints %v", hints)
	}

	d.sounds = nil // --no-sound, or the files could not be written
	if err := d.show(context.Background(), Payload{ID: "e", Title: "Hi"}); err != nil {
		t.Fatal(err)
	}
	if hints := bus.call(t)[6].(map[string]dbus.Variant); hints["sound-file"].Value() != nil {
		t.Fatalf("a sound without sound files: %v", hints)
	}
}

func TestReportsReceiptsWhenAsked(t *testing.T) {
	bus := &fakeBus{calls: make(chan []any, 10)}
	receipts := make(chan string, 10)
	d := &dbusDesktop{
		object:  func(string) dbus.BusObject { return bus },
		actions: true,
		owner:   ":1.5",
		now:     func() time.Time { return time.Unix(testNow, 0) },
		pending: map[uint32]question{},
		receipt: func(id, state string) { receipts <- id + "=" + state },
		answer:  func(string, histfile.Reply, string) string { return "The question has expired" },
	}
	show := func(p Payload) []any {
		t.Helper()
		if err := d.show(context.Background(), p); err != nil {
			t.Fatal(err)
		}
		return bus.call(t)
	}
	click := func(nid uint32) {
		d.signal(0, &dbus.Signal{Sender: ":1.5", Name: notificationsInterface + ".ActionInvoked", Body: []any{nid, openAction}})
	}
	next := func() string {
		t.Helper()
		select {
		case r := <-receipts:
			return r
		case <-time.After(5 * time.Second):
			t.Fatal("no receipt")
			return ""
		}
	}

	if args := show(Payload{ID: "msg_plain", Title: "hi"}); len(args[5].([]string)) != 0 {
		t.Fatalf("actions %v without receipts", args[5])
	}
	click(1)
	if args := show(Payload{ID: "msg_r", Title: "Disk full", Receipts: true}); !slices.Equal(args[5].([]string), []string{openAction, "Mark seen"}) {
		t.Fatalf("actions %v", args[5])
	}
	if r := next(); r != "msg_r=shown" {
		t.Fatalf("receipt %q", r)
	}
	click(2)
	click(2) // once is enough
	if r := next(); r != "msg_r=opened" {
		t.Fatalf("receipt %q", r)
	}
	q := testQuestion
	args := show(Payload{ID: "msg_q", Title: "Deploy?", Reply: &q, Receipts: true})
	// Its answers report the opening; an Open action would replace dunst's menu of them.
	if !slices.Equal(args[5].([]string), []string{"0", "yes", "1", "no"}) {
		t.Fatalf("actions %v", args[5])
	}
	next() // shown
	// A click the service sends unoffered neither reports nor drops the question.
	click(3)
	// Expired, the answer never reaches the server, which would count it.
	d.signal(0, &dbus.Signal{Sender: ":1.5", Name: notificationsInterface + ".ActionInvoked", Body: []any{uint32(3), "0"}})
	if r := next(); r != "msg_q=opened" {
		t.Fatalf("receipt %q", r)
	}
	bus.call(t) // the outcome
	select {
	case r := <-receipts:
		t.Fatalf("extra receipt %q", r)
	case <-time.After(50 * time.Millisecond):
	}
}

func TestReportsShownWithoutActionsButNotWhenRefused(t *testing.T) {
	bus := &fakeBus{calls: make(chan []any, 10)}
	receipts := make(chan string, 10)
	d := &dbusDesktop{
		object:  func(string) dbus.BusObject { return bus },
		owner:   ":1.5",
		now:     func() time.Time { return time.Unix(testNow, 0) },
		pending: map[uint32]question{},
		receipt: func(id, state string) { receipts <- id + "=" + state },
	}
	if err := d.show(context.Background(), Payload{ID: "msg_r", Title: "Disk full", Receipts: true}); err != nil {
		t.Fatal(err)
	}
	if args := bus.call(t); len(args[5].([]string)) != 0 {
		t.Fatalf("actions %v without the actions capability", args[5])
	}
	select {
	case r := <-receipts:
		if r != "msg_r=shown" {
			t.Fatalf("receipt %q", r)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no shown receipt")
	}
	bus.err = dbus.Error{Name: "org.freedesktop.DBus.Error.InvalidArgs"}
	if err := d.show(context.Background(), Payload{ID: "msg_x", Title: "Refused", Receipts: true}); !errors.Is(err, errRejected) {
		t.Fatalf("err %v", err)
	}
	bus.call(t)
	select {
	case r := <-receipts:
		t.Fatalf("receipt %q for a refused message", r)
	case <-time.After(50 * time.Millisecond):
	}
}

func TestKeepsBoundedNotificationsForActions(t *testing.T) {
	now := time.Unix(testNow, 0)
	d := &dbusDesktop{pending: map[uint32]question{}}
	open := histfile.Reply{Options: []string{"yes"}, ExpiresAt: testNow + 60}
	d.pending[1] = question{id: "msg_q", reply: open, shown: now.Add(-time.Hour)}
	for i := range maxPending - 1 {
		d.pending[uint32(i+2)] = question{id: "msg_r", receipts: true, shown: now.Add(time.Duration(i) * time.Second)}
	}
	d.evictUnanswerable(now)
	if _, ok := d.pending[1]; !ok || len(d.pending) != maxPending-1 {
		t.Fatalf("kept %d, the open question: %v", len(d.pending), ok)
	}
	if _, ok := d.pending[2]; ok {
		t.Fatal("the oldest message without answers stayed")
	}
}

func TestPostsAReceipt(t *testing.T) {
	api := newFakeAPI(t)
	a, _, stderr := newTestApp(t)
	st := State{URL: api.URL, DeviceID: "dev_new", DeviceToken: "nd_new", SealKey: testKey}
	api.status["POST /v1/device/receipts"] = http.StatusNoContent
	a.receipt(st, "msg_r", receiptOpened)
	if api.requests[0] != "POST /v1/device/receipts Bearer nd_new" || api.bodies[0]["message_id"] != "msg_r" || api.bodies[0]["state"] != "opened" {
		t.Fatalf("requests %q, bodies %v", api.requests, api.bodies)
	}
	api.status["POST /v1/device/receipts"] = http.StatusNotFound
	a.receipt(st, "msg_r", receiptShown)
	if !strings.Contains(stderr.String(), "could not report message msg_r as shown") {
		t.Fatalf("stderr %q", stderr.String())
	}
}
