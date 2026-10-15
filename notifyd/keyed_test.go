// SPDX-License-Identifier: GPL-3.0-or-later
package main

import (
	"context"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/godbus/dbus/v5"
	"github.com/joncbenderkh/notifier/notifyd/internal/histfile"
)

// replacingBus is a [fakeBus] that, like a real service, keeps the ID of
// the notification a call replaces, and records the ones closed.
type replacingBus struct {
	fakeBus
	closed []uint32
	// fresh numbers anew a notification that replaces a closed one, as
	// some services do.
	fresh bool
}

func (b *replacingBus) CallWithContext(ctx context.Context, method string, flags dbus.Flags, args ...any) *dbus.Call {
	if method == notificationsInterface+".CloseNotification" {
		b.mu.Lock()
		defer b.mu.Unlock()
		b.closed = append(b.closed, args[0].(uint32))
		return &dbus.Call{}
	}
	call := b.fakeBus.CallWithContext(ctx, method, flags, args...)
	if method == notificationsInterface+".Notify" && call.Err == nil && args[1] != uint32(0) && !b.fresh {
		call.Body = []any{args[1]}
	}
	return call
}

func TestReplacesKeyedNotifications(t *testing.T) {
	bus := &replacingBus{fakeBus: fakeBus{calls: make(chan []any, 10)}}
	d := &dbusDesktop{
		object: func(string) dbus.BusObject { return bus }, actions: true, owner: ":1.5", pending: map[uint32]question{},
		now: func() time.Time { return time.Unix(testNow, 0) },
	}
	show := func(p Payload) uint32 {
		t.Helper()
		if err := d.show(context.Background(), p); err != nil {
			t.Fatal(err)
		}
		return bus.call(t)[1].(uint32)
	}
	closed := func(sender string, nid uint32) {
		d.signal(0, &dbus.Signal{Sender: sender, Name: notificationsInterface + ".NotificationClosed", Body: []any{nid, uint32(2)}})
	}

	if r := show(Payload{ID: "msg_1", Title: "Disk 80%", Key: "disk"}); r != 0 { // 1
		t.Fatalf("first replaces %d", r)
	}
	if r := show(Payload{ID: "msg_2", Title: "Disk 90%", Key: "disk"}); r != 1 {
		t.Fatalf("second replaces %d, want 1", r)
	}
	// Another key, or the same key in another channel, is another notification.
	if r := show(Payload{ID: "msg_3", Title: "Load", Key: "load"}); r != 0 { // 3
		t.Fatalf("other key replaces %d", r)
	}
	if r := show(Payload{ID: "msg_4", Title: "Disk", Key: "disk", ChannelID: "ch_x"}); r != 0 { // 4
		t.Fatalf("other channel replaces %d", r)
	}
	if r := show(Payload{ID: "msg_5", Title: "Unkeyed"}); r != 0 { // 5
		t.Fatalf("unkeyed replaces %d", r)
	}

	// A closed notification's ID may be reused, so it is not replaced;
	// not when someone else says it closed.
	closed(":1.9", 1)
	if r := show(Payload{ID: "msg_6", Title: "Disk 95%", Key: "disk"}); r != 1 {
		t.Fatalf("after a spoofed close replaces %d, want 1", r)
	}
	closed(":1.5", 1)
	if r := show(Payload{ID: "msg_7", Title: "Disk 96%", Key: "disk"}); r != 0 { // 7
		t.Fatalf("after close replaces %d", r)
	}

	// A new service numbers its own.
	d.signal(0, &dbus.Signal{Sender: "org.freedesktop.DBus", Name: "org.freedesktop.DBus.NameOwnerChanged", Body: []any{notificationsInterface, ":1.5", ":1.9"}})
	if r := show(Payload{ID: "msg_8", Title: "Disk 97%", Key: "disk"}); r != 0 { // 8
		t.Fatalf("after a new service replaces %d", r)
	}
}

func TestClosesAQuestionRatherThanReplaceIt(t *testing.T) {
	bus := &replacingBus{fakeBus: fakeBus{calls: make(chan []any, 10)}}
	answered := make(chan string, 2)
	d := &dbusDesktop{
		object: func(string) dbus.BusObject { return bus }, actions: true, owner: ":1.5", pending: map[uint32]question{},
		now: func() time.Time { return time.Unix(testNow, 0) },
		answer: func(id string, _ histfile.Reply, option string) string {
			answered <- id + "=" + option
			return "Answered: " + option
		},
	}
	q := testQuestion
	if err := d.show(context.Background(), Payload{ID: "msg_a", Title: "Deploy A?", Reply: &q, Key: "deploy"}); err != nil {
		t.Fatal(err)
	}
	bus.call(t) // 1
	other := histfile.Reply{Options: []string{"rollback", "keep"}, Key: q.Key, ExpiresAt: q.ExpiresAt}
	if err := d.show(context.Background(), Payload{ID: "msg_b", Title: "Deploy B?", Reply: &other, Key: "deploy"}); err != nil {
		t.Fatal(err)
	}
	if args := bus.call(t); args[1] != uint32(0) {
		t.Fatalf("replaces %v, want a new notification", args[1])
	}
	if !slices.Equal(bus.closed, []uint32{1}) {
		t.Fatalf("closed %v, want [1]", bus.closed)
	}
	// A click on the first, queued behind the second, still answers the first.
	d.signal(0, &dbus.Signal{Sender: ":1.5", Name: notificationsInterface + ".ActionInvoked", Body: []any{uint32(1), "0"}})
	if a := <-answered; a != "msg_a=yes" {
		t.Fatalf("answered %q", a)
	}
	bus.call(t) // its outcome
	// Answered, the second is replaced by the next.
	d.signal(0, &dbus.Signal{Sender: ":1.5", Name: notificationsInterface + ".ActionInvoked", Body: []any{uint32(2), "1"}})
	if a := <-answered; a != "msg_b=keep" {
		t.Fatalf("answered %q", a)
	}
	bus.call(t)
	if err := d.show(context.Background(), Payload{ID: "msg_c", Title: "Deployed", Key: "deploy"}); err != nil {
		t.Fatal(err)
	}
	if args := bus.call(t); args[1] != uint32(2) {
		t.Fatalf("replaces %v, want 2", args[1])
	}
}

func TestOutcomeKeepsItsPlace(t *testing.T) {
	bus := &replacingBus{fakeBus: fakeBus{calls: make(chan []any, 10)}}
	release := make(chan struct{})
	d := &dbusDesktop{
		object: func(string) dbus.BusObject { return bus }, actions: true, owner: ":1.5", pending: map[uint32]question{},
		now:    func() time.Time { return time.Unix(testNow, 0) },
		answer: func(_ string, _ histfile.Reply, option string) string { <-release; return "Answered: " + option },
	}
	q := testQuestion
	if err := d.show(context.Background(), Payload{ID: "msg_q", Title: "Deploy?", Reply: &q, Key: "deploy"}); err != nil {
		t.Fatal(err)
	}
	bus.call(t) // 1
	d.signal(0, &dbus.Signal{Sender: ":1.5", Name: notificationsInterface + ".ActionInvoked", Body: []any{uint32(1), "0"}})
	// The answer is on its way when the next message with the key comes:
	// it gets a notification of its own, and the outcome the question's.
	if err := d.show(context.Background(), Payload{ID: "msg_r", Title: "Deployed", Key: "deploy"}); err != nil {
		t.Fatal(err)
	}
	if args := bus.call(t); args[1] != uint32(0) {
		t.Fatalf("message replaces %v", args[1])
	}
	close(release)
	if args := bus.call(t); args[1] != uint32(1) || args[3] != "Answered: yes" {
		t.Fatalf("outcome %v", args)
	}
	if len(bus.closed) != 0 {
		t.Fatalf("closed %v", bus.closed)
	}
}

func TestNextMessageReplacesAClosedQuestionsOutcome(t *testing.T) {
	bus := &replacingBus{fakeBus: fakeBus{calls: make(chan []any, 10)}}
	d := &dbusDesktop{
		object: func(string) dbus.BusObject { return bus }, actions: true, owner: ":1.5", pending: map[uint32]question{},
		now:    func() time.Time { return time.Unix(testNow, 0) },
		answer: func(_ string, _ histfile.Reply, option string) string { return "Answered: " + option },
	}
	q := testQuestion
	if err := d.show(context.Background(), Payload{ID: "msg_q", Title: "Deploy?", Reply: &q, Key: "deploy"}); err != nil {
		t.Fatal(err)
	}
	bus.call(t) // 1
	// dunst closes a notification when one of its actions is invoked.
	d.signal(0, &dbus.Signal{Sender: ":1.5", Name: notificationsInterface + ".ActionInvoked", Body: []any{uint32(1), "0"}})
	d.signal(0, &dbus.Signal{Sender: ":1.5", Name: notificationsInterface + ".NotificationClosed", Body: []any{uint32(1), uint32(2)}})
	bus.call(t) // the outcome, as 1 again
	if err := d.show(context.Background(), Payload{ID: "msg_r", Title: "Deployed", Key: "deploy"}); err != nil {
		t.Fatal(err)
	}
	if args := bus.call(t); args[1] != uint32(1) {
		t.Fatalf("replaces %v, want the outcome's 1", args[1])
	}
}

func TestRemembersAtMostMaxPendingKeys(t *testing.T) {
	d := &dbusDesktop{}
	start := time.Unix(testNow, 0)
	for i := range maxPending + 1 {
		d.rememberKeyed(fmt.Sprintf("direct/k%d", i), keyedNote{nid: uint32(i + 1), shown: start.Add(time.Duration(i) * time.Second)})
	}
	if len(d.keyed) != maxPending {
		t.Fatalf("%d keys", len(d.keyed))
	}
	if _, ok := d.keyed["direct/k0"]; ok {
		t.Fatal("the oldest was kept")
	}
	if _, ok := d.keyed[fmt.Sprintf("direct/k%d", maxPending)]; !ok {
		t.Fatal("the newest was dropped")
	}
}

func TestShowsAMessageWithActionsAnew(t *testing.T) {
	bus := &replacingBus{fakeBus: fakeBus{calls: make(chan []any, 10)}}
	answered := make(chan string, 1)
	d := &dbusDesktop{
		object: func(string) dbus.BusObject { return bus }, actions: true, owner: ":1.5", pending: map[uint32]question{},
		now:    func() time.Time { return time.Unix(testNow, 0) },
		answer: func(id string, _ histfile.Reply, option string) string { answered <- id; return "ok" },
	}
	if err := d.show(context.Background(), Payload{ID: "msg_1", Title: "Status", Key: "k"}); err != nil {
		t.Fatal(err)
	}
	bus.call(t) // 1, which times out; its close is still queued
	q := testQuestion
	if err := d.show(context.Background(), Payload{ID: "msg_2", Title: "Deploy?", Reply: &q, Key: "k"}); err != nil {
		t.Fatal(err)
	}
	if args := bus.call(t); args[1] != uint32(0) {
		t.Fatalf("replaces %v, want a new notification", args[1])
	}
	d.signal(0, &dbus.Signal{Sender: ":1.5", Name: notificationsInterface + ".NotificationClosed", Body: []any{uint32(1), uint32(1)}})
	d.signal(0, &dbus.Signal{Sender: ":1.5", Name: notificationsInterface + ".ActionInvoked", Body: []any{uint32(2), "0"}})
	select {
	case id := <-answered:
		if id != "msg_2" {
			t.Fatalf("answered %s", id)
		}
	case <-time.After(time.Second):
		t.Fatal("the question's click was dropped after the close of the one it replaced")
	}
}

func TestOutcomeTakesTheKeyWhenTheCloseComesLate(t *testing.T) {
	bus := &replacingBus{fakeBus: fakeBus{calls: make(chan []any, 10)}, fresh: true}
	d := &dbusDesktop{
		object: func(string) dbus.BusObject { return bus }, actions: true, owner: ":1.5", pending: map[uint32]question{},
		now:    func() time.Time { return time.Unix(testNow, 0) },
		answer: func(_ string, _ histfile.Reply, option string) string { return "Answered: " + option },
	}
	q := testQuestion
	if err := d.show(context.Background(), Payload{ID: "msg_q", Title: "Deploy?", Reply: &q, Key: "deploy"}); err != nil {
		t.Fatal(err)
	}
	bus.call(t) // 1
	d.signal(0, &dbus.Signal{Sender: ":1.5", Name: notificationsInterface + ".ActionInvoked", Body: []any{uint32(1), "0"}})
	bus.call(t) // the outcome, numbered 2 by a service that closed 1
	d.signal(0, &dbus.Signal{Sender: ":1.5", Name: notificationsInterface + ".NotificationClosed", Body: []any{uint32(1), uint32(2)}})
	if err := d.show(context.Background(), Payload{ID: "msg_r", Title: "Deployed", Key: "deploy"}); err != nil {
		t.Fatal(err)
	}
	if args := bus.call(t); args[1] != uint32(2) {
		t.Fatalf("replaces %v, want the outcome's 2", args[1])
	}
}
