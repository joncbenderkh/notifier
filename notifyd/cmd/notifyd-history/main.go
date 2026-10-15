// SPDX-License-Identifier: GPL-3.0-or-later

// Command notifyd-history shows what notifyd received in a GTK 4 /
// libadwaita window (issue #64): a list and the selected message, search, a
// channel filter, delete with undo, and clear all. It reads and changes the
// history file under the same lock as notifyd and makes no network access.
package main

import (
	"context"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/diamondburned/gotk4-adwaita/pkg/adw"
	"github.com/diamondburned/gotk4/pkg/gdk/v4"
	"github.com/diamondburned/gotk4/pkg/gio/v2"
	"github.com/diamondburned/gotk4/pkg/glib/v2"
	"github.com/diamondburned/gotk4/pkg/gtk/v4"
	"github.com/diamondburned/gotk4/pkg/pango"

	"github.com/joncbenderkh/notifier/notifyd/internal/histfile"
	"github.com/joncbenderkh/notifier/notifyd/internal/histview"
)

const css = `
.unread-dot { color: @accent_color; font-size: 10px; }
.pill { background: alpha(currentColor, 0.1); border-radius: 999px; padding: 1px 8px; font-size: smaller; }
.badge-high { background: @error_bg_color; color: @error_fg_color; border-radius: 999px; padding: 1px 8px; font-size: smaller; font-weight: bold; }
.history-row { padding: 8px 4px; }
`

func main() {
	app := adw.NewApplication("com.joncbenderkh.notifier.History", gio.ApplicationFlagsNone)
	var w *window
	app.ConnectActivate(func() {
		if w == nil {
			w = newWindow(app)
		}
		w.win.Present()
	})
	os.Exit(app.Run(os.Args))
}

// row is one list row and the labels that search highlights.
type row struct {
	entry          histfile.Entry
	widget         *gtk.ListBoxRow
	dot            *gtk.Label
	title, preview *gtk.Label
}

type window struct {
	win      *adw.ApplicationWindow
	toasts   *adw.ToastOverlay
	split    *adw.NavigationSplitView
	list     *gtk.ListBox
	rows     []*row
	stack    *gtk.Stack // list, empty, none, error
	errPage  *adw.StatusPage
	search   *gtk.SearchEntry
	channels *gtk.DropDown
	names    []string // the dropdown's channels; "" is all

	detail                           *gtk.Stack // message, none
	dTitle, dTiming, dBody, dChannel *gtk.Label
	// dLinks holds a button per link of the message shown.
	dLinks                   *gtk.FlowBox
	dHigh                    *gtk.Label
	historyPath, seenPath    string
	monitor                  gio.FileMonitorrer
	reloadPending            bool
	unread, pending          map[string]bool // pending: deleted, undo still possible
	seen                     map[string]bool // listed, for the i3blocks block
	seenDirty                bool            // the last write failed
	settingChannels          bool            // the dropdown changes are not the user's
	entries                  []histfile.Entry
	selected                 string
	deleteAction, copyAction *gio.SimpleAction
}

func newWindow(app *adw.Application) *window {
	w := &window{unread: map[string]bool{}, pending: map[string]bool{}, seen: map[string]bool{}}
	var err error
	if w.historyPath, err = histfile.Path(); err == nil {
		w.seenPath, err = histview.SeenPath()
	}

	provider := gtk.NewCSSProvider()
	provider.LoadFromString(css)
	gtk.StyleContextAddProviderForDisplay(gdk.DisplayGetDefault(), provider, gtk.STYLE_PROVIDER_PRIORITY_APPLICATION)

	w.win = adw.NewApplicationWindow(&app.Application)
	w.win.SetTitle("Notifier history")
	w.win.SetDefaultSize(1000, 640)
	w.split = adw.NewNavigationSplitView()
	w.split.SetMinSidebarWidth(320)
	w.split.SetMaxSidebarWidth(420)
	w.split.SetSidebar(adw.NewNavigationPage(w.sidebar(), "History"))
	w.split.SetContent(adw.NewNavigationPage(w.content(), "Message"))
	w.toasts = adw.NewToastOverlay()
	w.toasts.SetChild(w.split)
	w.win.SetContent(w.toasts)

	narrow := adw.NewBreakpoint(adw.BreakpointConditionParse("max-width: 600sp"))
	narrow.AddSetter(w.split, "collapsed", true)
	w.win.AddBreakpoint(narrow)

	w.actions(app)
	w.win.ConnectCloseRequest(func() bool {
		w.commitPending()
		return false
	})

	if err != nil {
		w.showError(err)
		return w
	}
	w.reload()
	w.watch()
	return w
}

func (w *window) sidebar() gtk.Widgetter {
	header := adw.NewHeaderBar()
	searchToggle := gtk.NewToggleButton()
	searchToggle.SetIconName("system-search-symbolic")
	searchToggle.SetTooltipText("Search")
	header.PackStart(searchToggle)
	menu := gio.NewMenu()
	menu.Append("Clear All…", "win.clear")
	menuButton := gtk.NewMenuButton()
	menuButton.SetIconName("open-menu-symbolic")
	menuButton.SetTooltipText("History options")
	menuButton.SetMenuModel(menu)
	header.PackEnd(menuButton)

	w.search = gtk.NewSearchEntry()
	w.search.SetHExpand(true)
	w.search.SetObjectProperty("placeholder-text", "Search messages")
	w.channels = gtk.NewDropDownFromStrings([]string{"All channels"})
	w.names = []string{""}
	box := gtk.NewBox(gtk.OrientationHorizontal, 6)
	box.Append(w.search)
	box.Append(w.channels)
	bar := gtk.NewSearchBar()
	bar.SetChild(box)
	bar.ConnectEntry(w.search)
	bar.SetKeyCaptureWidget(w.win)
	searchToggle.ConnectToggled(func() { bar.SetSearchMode(searchToggle.Active()) })
	bar.NotifyProperty("search-mode-enabled", func() {
		searchToggle.SetActive(bar.SearchMode())
		if !bar.SearchMode() {
			w.search.SetText("")
			w.channels.SetSelected(0)
		}
	})
	w.search.ConnectSearchChanged(w.refilter)
	w.channels.NotifyProperty("selected", func() {
		if !w.settingChannels {
			w.refilter()
		}
	})

	w.list = gtk.NewListBox()
	w.list.AddCSSClass("navigation-sidebar")
	w.list.SetFilterFunc(func(r *gtk.ListBoxRow) bool { return w.visible(w.rows[r.Index()].entry) })
	w.list.ConnectRowSelected(func(r *gtk.ListBoxRow) {
		if r == nil {
			w.show(nil)
			return
		}
		w.show(w.rows[r.Index()])
	})
	w.list.ConnectRowActivated(func(*gtk.ListBoxRow) { w.split.SetShowContent(true) })
	scroll := gtk.NewScrolledWindow()
	scroll.SetPolicy(gtk.PolicyNever, gtk.PolicyAutomatic)
	scroll.SetChild(w.list)
	scroll.SetVExpand(true)

	empty := adw.NewStatusPage()
	empty.SetIconName("preferences-system-notifications-symbolic")
	empty.SetTitle("Nothing Received Yet")
	empty.SetDescription("Messages appear here once notifyd is running and paired.")
	none := adw.NewStatusPage()
	none.SetIconName("edit-find-symbolic")
	none.SetTitle("No Results")
	none.SetDescription("Try a different search or channel.")
	w.errPage = adw.NewStatusPage()
	w.errPage.SetIconName("dialog-error-symbolic")
	w.errPage.SetTitle("Cannot Read the History")
	w.stack = gtk.NewStack()
	w.stack.AddNamed(scroll, "list")
	w.stack.AddNamed(empty, "empty")
	w.stack.AddNamed(none, "none")
	w.stack.AddNamed(w.errPage, "error")

	view := adw.NewToolbarView()
	view.AddTopBar(header)
	view.AddTopBar(bar)
	view.SetContent(w.stack)
	return view
}

func (w *window) content() gtk.Widgetter {
	header := adw.NewHeaderBar()
	header.SetShowTitle(false)
	del := gtk.NewButtonFromIconName("user-trash-symbolic")
	del.SetTooltipText("Delete")
	del.SetActionName("win.delete")
	copyButton := gtk.NewButtonFromIconName("edit-copy-symbolic")
	copyButton.SetTooltipText("Copy")
	copyButton.SetActionName("win.copy")
	header.PackEnd(del)
	header.PackEnd(copyButton)

	label := func(classes ...string) *gtk.Label {
		l := gtk.NewLabel("")
		l.SetXAlign(0)
		l.SetWrap(true)
		l.SetWrapMode(pango.WrapWordChar)
		l.SetSelectable(true)
		for _, c := range classes {
			l.AddCSSClass(c)
		}
		return l
	}
	w.dTitle = label("title-2")
	w.dTiming = label("dim-label")
	w.dBody = label("body")
	w.dBody.SetMarginTop(12)
	w.dChannel = gtk.NewLabel("")
	w.dChannel.AddCSSClass("pill")
	w.dHigh = gtk.NewLabel("High priority")
	w.dHigh.AddCSSClass("badge-high")
	badges := gtk.NewBox(gtk.OrientationHorizontal, 6)
	badges.Append(w.dChannel)
	badges.Append(w.dHigh)
	box := gtk.NewBox(gtk.OrientationVertical, 8)
	box.SetMarginTop(24)
	box.SetMarginBottom(24)
	box.SetMarginStart(18)
	box.SetMarginEnd(18)
	box.Append(w.dTitle)
	box.Append(badges)
	box.Append(w.dTiming)
	box.Append(gtk.NewSeparator(gtk.OrientationHorizontal))
	box.Append(w.dBody)
	w.dLinks = gtk.NewFlowBox()
	w.dLinks.SetSelectionMode(gtk.SelectionNone)
	box.Append(w.dLinks)
	clamp := adw.NewClamp()
	clamp.SetMaximumSize(820)
	clamp.SetChild(box)
	scroll := gtk.NewScrolledWindow()
	scroll.SetPolicy(gtk.PolicyNever, gtk.PolicyAutomatic)
	scroll.SetChild(clamp)
	scroll.SetVExpand(true)

	nothing := adw.NewStatusPage()
	nothing.SetIconName("mail-unread-symbolic")
	nothing.SetTitle("No Message Selected")
	w.detail = gtk.NewStack()
	w.detail.AddNamed(scroll, "message")
	w.detail.AddNamed(nothing, "none")
	w.detail.SetVisibleChildName("none")

	view := adw.NewToolbarView()
	view.AddTopBar(header)
	view.SetContent(w.detail)
	return view
}

func (w *window) actions(app *adw.Application) {
	add := func(name string, accels []string, fn func()) *gio.SimpleAction {
		a := gio.NewSimpleAction(name, nil)
		a.ConnectActivate(func(*glib.Variant) { fn() })
		w.win.AddAction(a)
		if accels != nil {
			app.SetAccelsForAction("win."+name, accels)
		}
		return a
	}
	add("clear", nil, w.confirmClear)
	w.deleteAction = add("delete", []string{"Delete"}, w.deleteSelected)
	w.copyAction = add("copy", nil, w.copySelected)
	add("close", []string{"<Control>w"}, func() { w.win.Close() })
	w.updateActions()
}

func (w *window) updateActions() {
	w.deleteAction.SetEnabled(w.selected != "")
	w.copyAction.SetEnabled(w.selected != "")
}

// reload rereads the history, keeping the selection, and records what it
// lists as seen for the i3blocks block.
func (w *window) reload() {
	w.reloadPending = false
	entries, err := histfile.Read(w.historyPath, time.Now())
	if err != nil {
		w.showError(err)
		return
	}
	// Another window or show-history may have listed some meanwhile.
	if seen, err := histview.ReadSeen(w.seenPath); err == nil {
		for id := range seen {
			w.seen[id] = true
		}
	}
	for _, e := range entries {
		if !w.seen[e.ID] {
			w.unread[e.ID] = true
		}
	}
	w.entries = entries

	selected := w.selected // RemoveAll unselects
	w.list.RemoveAll()
	w.rows = w.rows[:0]
	now := time.Now()
	var keep *row
	for _, e := range entries {
		r := w.newRow(e, now)
		w.rows = append(w.rows, r)
		w.list.Append(r.widget)
		if e.ID == selected {
			keep = r
		}
	}
	w.setChannels(histview.Channels(entries))
	w.refilter()
	if keep != nil && w.visible(keep.entry) {
		w.list.SelectRow(keep.widget)
	} else {
		w.list.UnselectAll()
		w.show(nil)
	}
}

func (w *window) newRow(e histfile.Entry, now time.Time) *row {
	r := &row{entry: e, widget: gtk.NewListBoxRow()}
	r.dot = gtk.NewLabel("●")
	r.dot.AddCSSClass("unread-dot")
	r.dot.SetVAlign(gtk.AlignCenter)
	r.dot.SetTooltipText("Not seen before")
	r.dot.SetOpacity(0)
	if w.unread[e.ID] {
		r.dot.SetOpacity(1)
	}
	r.title = gtk.NewLabel(histview.Title(e))
	r.title.SetXAlign(0)
	r.title.SetHExpand(true)
	r.title.SetEllipsize(pango.EllipsizeEnd)
	r.title.AddCSSClass("heading")
	when := gtk.NewLabel(histview.When(time.Unix(e.ReceivedAt, 0), now))
	when.AddCSSClass("dim-label")
	when.AddCSSClass("caption")
	when.SetTooltipText(histview.Timing(e))
	top := gtk.NewBox(gtk.OrientationHorizontal, 6)
	top.Append(r.title)
	if e.Priority == "high" {
		high := gtk.NewLabel("High")
		high.AddCSSClass("badge-high")
		top.Append(high)
	}
	top.Append(when)

	r.preview = gtk.NewLabel(histview.Preview(e, 200))
	r.preview.SetXAlign(0)
	r.preview.SetHExpand(true)
	r.preview.SetEllipsize(pango.EllipsizeEnd)
	r.preview.AddCSSClass("dim-label")
	bottom := gtk.NewBox(gtk.OrientationHorizontal, 6)
	if c := histview.Channel(e); c != "" {
		pill := gtk.NewLabel(c)
		pill.AddCSSClass("pill")
		pill.SetEllipsize(pango.EllipsizeEnd)
		pill.SetMaxWidthChars(18)
		bottom.Append(pill)
	}
	bottom.Append(r.preview)

	text := gtk.NewBox(gtk.OrientationVertical, 4)
	text.SetHExpand(true)
	text.Append(top)
	text.Append(bottom)
	box := gtk.NewBox(gtk.OrientationHorizontal, 8)
	box.AddCSSClass("history-row")
	box.Append(r.dot)
	box.Append(text)
	r.widget.SetChild(box)
	return r
}

// setChannels refills the channel filter, keeping the chosen channel while
// it still exists.
func (w *window) setChannels(names []string) {
	names = append([]string{""}, names...)
	if slices.Equal(names, w.names) {
		return
	}
	chosen := w.channel()
	w.names = names
	labels := slices.Clone(names)
	labels[0] = "All channels"
	// A new model selects "All channels" first; listing every channel then
	// would mark hidden messages seen. The caller refilters.
	w.settingChannels = true
	w.channels.SetModel(gtk.NewStringList(labels))
	w.channels.SetSelected(uint(max(slices.Index(names, chosen), 0)))
	w.settingChannels = false
}

func (w *window) channel() string {
	if i := int(w.channels.Selected()); i < len(w.names) {
		return w.names[i]
	}
	return ""
}

func (w *window) visible(e histfile.Entry) bool {
	return !w.pending[e.ID] && histview.Matches(e, w.search.Text(), w.channel())
}

// refilter applies the search and channel, highlights matches, and picks
// what the sidebar shows.
func (w *window) refilter() {
	query := strings.TrimSpace(w.search.Text())
	shown, kept := 0, 0
	for _, r := range w.rows {
		if w.pending[r.entry.ID] {
			continue
		}
		kept++
		if !w.visible(r.entry) {
			continue
		}
		shown++
		r.title.SetMarkup(histview.Highlight(histview.Title(r.entry), query))
		r.preview.SetMarkup(histview.Highlight(histview.Preview(r.entry, 200), query))
	}
	w.list.InvalidateFilter()
	w.markListed()
	switch {
	case kept == 0:
		w.stack.SetVisibleChildName("empty")
	case shown == 0:
		w.stack.SetVisibleChildName("none")
	default:
		w.stack.SetVisibleChildName("list")
	}
	if sel := w.list.SelectedRow(); sel != nil && !w.visible(w.rows[sel.Index()].entry) {
		w.list.UnselectAll()
	}
}

// markListed records the rows the list shows as seen for the i3blocks
// block; rows hidden by a search or filter stay unseen.
func (w *window) markListed() {
	changed := w.seenDirty
	for _, r := range w.rows {
		if w.visible(r.entry) && !w.seen[r.entry.ID] {
			w.seen[r.entry.ID] = true
			changed = true
		}
	}
	if !changed {
		return
	}
	// Merge what other viewers recorded since the last reload, and prune
	// against the current file, not this window's possibly older copy.
	if seen, err := histview.ReadSeen(w.seenPath); err == nil {
		for id := range seen {
			w.seen[id] = true
		}
	}
	entries, err := histfile.Read(w.historyPath, time.Now())
	if err == nil {
		err = histview.WriteSeen(w.seenPath, w.seen, entries)
	}
	if err != nil && !w.seenDirty {
		w.toast("Could not mark messages as seen: " + err.Error())
	}
	w.seenDirty = err != nil // retried on the next change or reload
}

// show puts r in the content pane (nil: nothing selected).
func (w *window) show(r *row) {
	w.updateActionsFor(r)
	if r == nil {
		w.detail.SetVisibleChildName("none")
		return
	}
	e := r.entry
	delete(w.unread, e.ID)
	r.dot.SetOpacity(0)
	w.dTitle.SetText(histview.Title(e))
	w.dTiming.SetText(histview.Timing(e))
	body := histview.Body(e)
	w.dBody.SetText(body)
	w.dBody.SetVisible(body != "")
	if strings.Contains(body, "\n") {
		w.dBody.AddCSSClass("monospace")
	} else {
		w.dBody.RemoveCSSClass("monospace")
	}
	for child := w.dLinks.FirstChild(); child != nil; child = w.dLinks.FirstChild() {
		w.dLinks.Remove(child)
	}
	links := histview.Links(e)
	for _, l := range links {
		// Opens through the desktop's launcher (the portal where one runs).
		w.dLinks.Append(gtk.NewLinkButtonWithLabel(l.URL, l.Label))
	}
	w.dLinks.SetVisible(len(links) > 0)
	c := histview.Channel(e)
	w.dChannel.SetText(c)
	w.dChannel.SetVisible(c != "")
	w.dHigh.SetVisible(e.Priority == "high")
	w.detail.SetVisibleChildName("message")
}

func (w *window) updateActionsFor(r *row) {
	if r == nil {
		w.selected = ""
	} else {
		w.selected = r.entry.ID
	}
	w.updateActions()
}

func (w *window) selectedEntry() (histfile.Entry, bool) {
	for _, r := range w.rows {
		if r.entry.ID == w.selected {
			return r.entry, true
		}
	}
	return histfile.Entry{}, false
}

func (w *window) copySelected() {
	e, ok := w.selectedEntry()
	if !ok {
		return
	}
	text := histview.Title(e)
	if body := histview.Body(e); body != "" {
		text += "\n\n" + body
	}
	w.win.Clipboard().SetText(text)
	w.toast("Copied")
}

// deleteSelected hides the message at once and deletes it from the file
// when the toast goes away without Undo.
func (w *window) deleteSelected() {
	e, ok := w.selectedEntry()
	if !ok {
		return
	}
	id := e.ID
	w.pending[id] = true
	w.list.UnselectAll()
	w.show(nil)
	w.split.SetShowContent(false)
	w.refilter()

	t := adw.NewToast("Message deleted")
	t.SetUseMarkup(false)
	t.SetButtonLabel("Undo")
	t.ConnectButtonClicked(func() {
		delete(w.pending, id)
		w.refilter()
	})
	t.ConnectDismissed(func() {
		if w.pending[id] {
			w.commit(id)
		}
	})
	w.toasts.AddToast(t)
}

func (w *window) commit(ids ...string) {
	for _, id := range ids {
		delete(w.pending, id)
	}
	err := histfile.WithLock(w.historyPath, func() error {
		_, err := histfile.Delete(w.historyPath, time.Now(), ids...)
		return err
	})
	if err != nil {
		w.toast("Could not delete: " + err.Error())
		w.scheduleReload()
	}
}

func (w *window) commitPending() {
	var ids []string
	for id := range w.pending {
		ids = append(ids, id)
	}
	if len(ids) > 0 {
		w.commit(ids...)
	}
}

func (w *window) confirmClear() {
	d := adw.NewAlertDialog("Clear the history?",
		"This removes every notification kept on this computer. The server keeps no copy.")
	d.AddResponse("cancel", "Cancel")
	d.AddResponse("clear", "Clear")
	d.SetResponseAppearance("clear", adw.ResponseDestructive)
	d.SetDefaultResponse("cancel")
	d.SetCloseResponse("cancel")
	d.ConnectResponse(func(response string) {
		if response != "clear" {
			return
		}
		clear(w.pending)
		err := histfile.WithLock(w.historyPath, func() error { return histfile.Clear(w.historyPath) })
		if err != nil {
			w.toast("Could not clear the history: " + err.Error())
		}
		w.reload()
	})
	d.Present(w.win)
}

// watch reloads when notifyd records a message or another process changes
// the history.
func (w *window) watch() {
	m, err := gio.NewFileForPath(w.historyPath).MonitorFile(context.Background(), gio.FileMonitorWatchMoves)
	if err != nil {
		w.toast("New messages will not appear until reopened: " + err.Error())
		return
	}
	w.monitor = m
	gio.BaseFileMonitor(m).ConnectChanged(func(_, _ gio.Filer, _ gio.FileMonitorEvent) { w.scheduleReload() })
}

func (w *window) scheduleReload() {
	if w.reloadPending {
		return
	}
	w.reloadPending = true
	glib.TimeoutAdd(300, func() bool {
		w.reload()
		return false
	})
}

func (w *window) showError(err error) {
	w.errPage.SetDescription(histfile.Printable(err.Error(), false))
	w.stack.SetVisibleChildName("error")
}

func (w *window) toast(text string) {
	t := adw.NewToast(histfile.Printable(text, false))
	t.SetUseMarkup(false) // errors can hold file names
	w.toasts.AddToast(t)
}
