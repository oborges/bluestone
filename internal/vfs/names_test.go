package vfs

import (
	"io"
	"os"
	"testing"
)

func TestWindowsNameMappingRoundTrips(t *testing.T) {
	for _, tc := range []struct {
		key, client string
	}{
		{"plain.txt", "plain.txt"},
		{"a:b", "ab"},
		{`q"*<>?\|`, "q"},
		{"ctl\x01\x1f", "ctl"},
		{"trailing. ", "trailing"},
		{"mid. dle.txt", "mid. dle.txt"},
		{"résumé.txt", "résumé.txt"},
		{".", "."},
		{"..", ".."},
	} {
		if got := windowsName(tc.key); got != tc.client {
			t.Errorf("windowsName(%q) = %q, want %q", tc.key, got, tc.client)
		}
		if got := keyName(tc.client); got != tc.key {
			t.Errorf("keyName(%q) = %q, want %q", tc.client, got, tc.key)
		}
	}
}

func entryNames(t *testing.T, fs *Filesystem, dir string) map[string]bool {
	t.Helper()
	entries, err := fs.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir(%q) error = %v", dir, err)
	}
	names := make(map[string]bool, len(entries))
	for _, entry := range entries {
		names[entry.Name()] = true
	}
	return names
}

func TestWindowsNamesMatchCaseInsensitively(t *testing.T) {
	manager := newTestStagingManager(t)
	store := newFakeObjectStore()
	store.put("Docs/Report.txt", []byte("upper R"))
	store.put("Docs/report.TXT", []byte("lower r"))
	store.put("Docs/Notes.md", []byte("notes"))
	base := newDirtyStagingTestFilesystemWithStore(t, manager, store)
	win := base.WithWindowsNames()

	// No exact match: the first matching key in byte order ("R" < "r").
	if got := readTestFile(t, win, "DOCS/REPORT.TXT"); got != "upper R" {
		t.Fatalf("DOCS/REPORT.TXT = %q, want the byte-order-first key", got)
	}
	// An exact match wins.
	if got := readTestFile(t, win, "docs/report.TXT"); got != "lower r" {
		t.Fatalf("docs/report.TXT = %q, want the exact-case key", got)
	}
	// Listings show every key.
	names := entryNames(t, win, "DOCS")
	for _, want := range []string{"Report.txt", "report.TXT", "Notes.md"} {
		if !names[want] {
			t.Fatalf("listing %v is missing %q", names, want)
		}
	}
	// Views without Windows naming stay case-sensitive.
	if _, err := base.Stat("DOCS/REPORT.TXT"); !os.IsNotExist(err) {
		t.Fatalf("case-sensitive view Stat() error = %v, want not-exist", err)
	}

	// Creating a name that matches an existing key opens that key.
	writeTestFile(t, win, "docs/NOTES.MD", "rewritten")
	if !manager.IsDirty("/Docs/Notes.md") || manager.IsDirty("/docs/NOTES.MD") {
		t.Fatal("create over a case-insensitive match must write the existing key")
	}
	if got := readTestFile(t, win, "Docs/Notes.md"); got != "rewritten" {
		t.Fatalf("Docs/Notes.md = %q, want rewritten", got)
	}
}

func TestWindowsNamesMapReservedCharacters(t *testing.T) {
	manager := newTestStagingManager(t)
	store := newFakeObjectStore()
	store.put("a:b?.txt", []byte("colon"))
	base := newDirtyStagingTestFilesystemWithStore(t, manager, store)
	win := base.WithWindowsNames()

	mapped := "ab.txt"
	if names := entryNames(t, win, "/"); !names[mapped] || names["a:b?.txt"] {
		t.Fatalf("listing %v, want the mapped name %q only", names, mapped)
	}
	if got := readTestFile(t, win, mapped); got != "colon" {
		t.Fatalf("read of mapped name = %q, want colon", got)
	}
	info, err := win.Stat(mapped)
	if err != nil {
		t.Fatalf("Stat(mapped) error = %v", err)
	}
	if info.Name() != mapped {
		t.Fatalf("Stat name = %q, want %q", info.Name(), mapped)
	}
	if attrs := FileAttributes(info); attrs.Mode != info.Mode() {
		t.Fatalf("renamed entry lost its attributes: %+v", attrs)
	}

	// Creating a mapped name stores the real characters.
	writeTestFile(t, win, "xy", "star")
	if !manager.IsDirty("/x*y.") {
		t.Fatal("mapped create must stage the key x*y.")
	}
	if names := entryNames(t, base, "/"); !names["x*y."] {
		t.Fatalf("unmapped view listing %v is missing the stored key x*y.", names)
	}
}

func TestWindowsNamesCaseOnlyRenameChangesKey(t *testing.T) {
	manager := newTestStagingManager(t)
	store := newFakeObjectStore()
	store.put("Notes.md", []byte("notes"))
	win := newDirtyStagingTestFilesystemWithStore(t, manager, store).WithWindowsNames()

	if err := win.Rename("Notes.md", "notes.md"); err != nil {
		t.Fatalf("Rename() error = %v", err)
	}
	if got := string(store.get("notes.md")); got != "notes" {
		t.Fatalf("notes.md = %q, want the renamed object", got)
	}
	if store.get("Notes.md") != nil {
		t.Fatal("the old spelling must be gone after a case-only rename")
	}
}

func TestWindowsNamesResolveStagedOnlyDirectories(t *testing.T) {
	manager := newTestStagingManager(t)
	win := newDirtyStagingTestFilesystemWithStore(t, manager, newFakeObjectStore()).WithWindowsNames()

	writeTestFile(t, win, "NewDir/File.txt", "staged")
	f, err := win.Open("newdir/FILE.TXT")
	if err != nil {
		t.Fatalf("Open(case-insensitive staged path) error = %v", err)
	}
	defer f.Close()
	data, err := io.ReadAll(f)
	if err != nil || string(data) != "staged" {
		t.Fatalf("staged-only file read = %q, %v; want staged", data, err)
	}
}

func TestWindowsNamesViewSurvivesChroot(t *testing.T) {
	manager := newTestStagingManager(t)
	store := newFakeObjectStore()
	store.put("Share/Doc.txt", []byte("doc"))
	win := newDirtyStagingTestFilesystemWithStore(t, manager, store).WithWindowsNames()

	chrooted, err := win.Chroot("SHARE")
	if err != nil {
		t.Fatalf("Chroot() error = %v", err)
	}
	if got := readTestFile(t, chrooted.(*Filesystem), "doc.TXT"); got != "doc" {
		t.Fatalf("chrooted read = %q, want doc", got)
	}
}
