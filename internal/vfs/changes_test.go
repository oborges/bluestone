package vfs

import "testing"

// Clients are told about a change under the name they use: relative to the
// view's root, with characters Windows cannot use mapped for Windows views.
func TestClientPath(t *testing.T) {
	plain := &Filesystem{root: "/"}
	windows := &Filesystem{root: "/", windowsNames: true}
	chrooted := &Filesystem{root: "/exports/team", windowsNames: true}

	for _, tc := range []struct {
		fs   *Filesystem
		key  string
		want string
		ok   bool
	}{
		{plain, "/docs/a:b.txt", "docs/a:b.txt", true},
		{windows, "/docs/a:b.txt", "docs/ab.txt", true},
		{chrooted, "/exports/team/plan.txt", "plan.txt", true},
		{chrooted, "/exports/teammate/plan.txt", "", false},
		{chrooted, "/other/plan.txt", "", false},
	} {
		got, ok := tc.fs.ClientPath(tc.key)
		if got != tc.want || ok != tc.ok {
			t.Errorf("ClientPath(%q) with root %q = %q, %v; want %q, %v", tc.key, tc.fs.root, got, ok, tc.want, tc.ok)
		}
	}
}

// Publishing with nobody subscribed, or on a filesystem built without a
// feed, does nothing.
func TestChangeFeedWithoutSubscribers(t *testing.T) {
	var nilFeed *changeFeed
	nilFeed.publish(Change{Action: ChangeAdded, Path: "/x"})
	if nilFeed.active() {
		t.Fatal("a nil feed reports subscribers")
	}
	feed := &changeFeed{}
	fs := &Filesystem{changes: feed}
	var got []Change
	cancel := fs.SubscribeChanges(func(c Change) { got = append(got, c) })
	if !feed.active() {
		t.Fatal("feed with a subscriber reports none")
	}
	fs.changed(Change{Action: ChangeRemoved, Path: "/x"})
	cancel()
	fs.changed(Change{Action: ChangeRemoved, Path: "/y"})
	if len(got) != 1 || got[0].Path != "/x" {
		t.Fatalf("subscriber saw %v, want only the change made while subscribed", got)
	}
}
