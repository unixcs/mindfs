package fs

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"mindfs/internal/remote"
)

func TestParseRemoteLs(t *testing.T) {
	// Captured from: LC_ALL=C ls -Alp --time-style=+%s (dirs, files, dotfiles,
	// names with spaces, symlinks whose names/targets contain spaces/arrows).
	output := strings.Join([]string{
		"总计 8",
		"-rw-r--r-- 1 root root  6 1791753410 a.txt",
		"-rw-r--r-- 1 root root  1 1791753410 b.big",
		"lrwxrwxrwx 1 root root 24 1791753410 dirlink -> /tmp/mfs-ls-test/sub dir",
		"drwxr-xr-x 2 root root 40 1791753410 .hidden/",
		"lrwxrwxrwx 1 root root  5 1791753410 link-> with spaces -> a.txt",
		"drwxr-xr-x 2 root root 40 1791753410 sub dir/",
		"",
	}, "\n")

	entries := parseRemoteLs(output)
	wantNames := []string{".hidden", "sub dir", "a.txt", "b.big", "dirlink", "link-> with spaces"}
	if len(entries) != len(wantNames) {
		t.Fatalf("parseRemoteLs returned %d entries (%v), want %d", len(entries), entries, len(wantNames))
	}
	wantMTime := time.Unix(1791753410, 0).UTC().Format(time.RFC3339Nano)
	for i, entry := range entries {
		if entry.Name != wantNames[i] {
			t.Errorf("entry[%d].Name = %q, want %q", i, entry.Name, wantNames[i])
		}
		if entry.Path != entry.Name {
			t.Errorf("entry[%d].Path = %q, want %q", i, entry.Path, entry.Name)
		}
		if entry.MTime != wantMTime {
			t.Errorf("entry[%d].MTime = %q, want %q", i, entry.MTime, wantMTime)
		}
	}
	if !entries[0].IsDir || entries[0].Name != ".hidden" {
		t.Errorf("entries[0] = %+v, want dir .hidden first", entries[0])
	}
	if !entries[1].IsDir || entries[1].Name != "sub dir" {
		t.Errorf("entries[1] = %+v, want dir sub dir second", entries[1])
	}
	if entries[2].IsDir || entries[2].Size != 6 {
		t.Errorf("entries[2] = %+v, want file a.txt with size 6", entries[2])
	}
	if entries[4].IsDir || !entries[4].IsSymlink {
		t.Errorf("entries[4] = %+v, want symlink dirlink", entries[4])
	}
	if entries[5].IsDir || !entries[5].IsSymlink || entries[5].Name != "link-> with spaces" {
		t.Errorf("entries[5] = %+v, want symlink link-> with spaces (target stripped)", entries[5])
	}
}

func TestParseRemoteLsSkipsHeaderAndMalformedLines(t *testing.T) {
	output := strings.Join([]string{
		"total 12",
		"总计 0",
		"",
		"2026-10-12 not an ls entry",
		"",
	}, "\n")
	if entries := parseRemoteLs(output); len(entries) != 0 {
		t.Fatalf("parseRemoteLs returned %v, want no entries", entries)
	}
}

func TestValidateRemoteRelPath(t *testing.T) {
	cases := []struct {
		rootPath string
		relPath  string
		want     string
		wantErr  bool
	}{
		{"/mnt/0", "", "/mnt/0", false},
		{"/mnt/0", ".", "/mnt/0", false},
		{"/mnt/0", "sub", "/mnt/0/sub", false},
		{"/mnt/0", "sub/x.txt", "/mnt/0/sub/x.txt", false},
		{"/mnt/0", "a/../b", "/mnt/0/b", false},
		{"/mnt/0", "sub/x/", "/mnt/0/sub/x", false},
		{"/mnt/0", " ", "/mnt/0", false},
		{"/", "x", "/x", false},
		{"/mnt/0", "..", "", true},
		{"/mnt/0", "../etc", "", true},
		{"/mnt/0", "a/../../etc", "", true},
		{"/mnt/0", "sub/../../etc", "", true},
		{"/mnt/0", "/etc/passwd", "", true},
		{"/", "/x", "", true},
		{"", "x", "", true},
		{"  ", "x", "", true},
	}
	for _, tc := range cases {
		got, err := validateRemoteRelPath(tc.rootPath, tc.relPath)
		if tc.wantErr {
			if err == nil {
				t.Errorf("validateRemoteRelPath(%q, %q) = %q, want error", tc.rootPath, tc.relPath, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("validateRemoteRelPath(%q, %q) returned error: %v", tc.rootPath, tc.relPath, err)
			continue
		}
		if got != tc.want {
			t.Errorf("validateRemoteRelPath(%q, %q) = %q, want %q", tc.rootPath, tc.relPath, got, tc.want)
		}
	}
}

func TestSplitRemoteReadOutput(t *testing.T) {
	marker := newRemoteSizeMarker()
	if !bytes.HasPrefix(marker, []byte("MFS_SZ:")) || !bytes.HasSuffix(marker, []byte(":")) {
		t.Fatalf("marker %q must look like MFS_SZ:<nonce>:", marker)
	}
	mk := func(spec string) []byte {
		return append(append([]byte{}, marker...), []byte(spec)...)
	}
	header, content, err := splitRemoteReadOutput(mk("11|1718000000|regular file\nhello\nworld"), marker)
	if err != nil {
		t.Fatalf("splitRemoteReadOutput returned error: %v", err)
	}
	if header.size != 11 || header.mtime != 1718000000 || header.isDir {
		t.Fatalf("header = %+v, want size=11 mtime=1718000000 regular", header)
	}
	if string(content) != "hello\nworld" {
		t.Fatalf("content = %q, want %q", content, "hello\nworld")
	}

	// Profile noise before the marker line is tolerated.
	header, content, err = splitRemoteReadOutput(append([]byte("welcome banner\n"), mk("2|100|regular file\nhi")...), marker)
	if err != nil || header.size != 2 || header.mtime != 100 || string(content) != "hi" {
		t.Fatalf("noisy output: header=%+v content=%q err=%v", header, content, err)
	}

	// A stale/foreign marker line (profile noise from an earlier read) never
	// satisfies the current nonce.
	stale := []byte("MFS_SZ:deadbeef:2|100|regular file\nhi")
	if _, _, err := splitRemoteReadOutput(stale, marker); err == nil {
		t.Fatal("foreign marker should not be accepted")
	}

	// Missing file: size -1.
	header, _, err = splitRemoteReadOutput(mk("-1|-1|missing\n"), marker)
	if err != nil || header.size != -1 {
		t.Fatalf("missing file: header=%+v err=%v, want size=-1 without error", header, err)
	}

	// Directory payload.
	header, _, err = splitRemoteReadOutput(mk("4096|123|directory\n"), marker)
	if err != nil || !header.isDir {
		t.Fatalf("directory: header=%+v err=%v, want isDir", header, err)
	}

	// Marker without trailing newline yields empty content.
	header, content, err = splitRemoteReadOutput(mk("0|0|regular file"), marker)
	if err != nil || header.size != 0 || len(content) != 0 {
		t.Fatalf("marker only: header=%+v content=%q err=%v", header, content, err)
	}

	if _, _, err := splitRemoteReadOutput([]byte("no marker here"), marker); err == nil {
		t.Fatal("output without marker should fail")
	}
}

func TestRegistryUpsertDeviceRoot(t *testing.T) {
	registry := NewRegistry(filepath.Join(t.TempDir(), "registry.json"))
	device := remote.Device{ID: "nas", Name: "NAS", Host: "100.118.126.86", User: "root", Enabled: true}

	root, err := registry.UpsertDeviceRoot(device, "/mnt/0/")
	if err != nil {
		t.Fatalf("UpsertDeviceRoot returned error: %v", err)
	}
	if root.ID != "nas-0" || root.Device != "nas" || root.RootPath != "/mnt/0" {
		t.Fatalf("root = %+v, want id nas-0 device nas path /mnt/0", root)
	}
	if root.MetaLocation != MetaLocationHome {
		t.Fatalf("MetaLocation = %q, want home", root.MetaLocation)
	}
	if root.Name == "" {
		t.Fatal("device root name must not be empty")
	}

	again, err := registry.UpsertDeviceRoot(device, "/mnt/0")
	if err != nil {
		t.Fatalf("re-UpsertDeviceRoot returned error: %v", err)
	}
	if again.ID != root.ID {
		t.Fatalf("re-Upsert id = %q, want %q", again.ID, root.ID)
	}
	if got := registry.List(); len(got) != 1 {
		t.Fatalf("List after re-Upsert = %d entries, want 1", len(got))
	}

	if list := registry.ListByDevice("nas"); len(list) != 1 || list[0].ID != "nas-0" {
		t.Fatalf("ListByDevice(nas) = %+v, want [nas-0]", list)
	}
	if list := registry.ListByDevice("tx"); len(list) != 0 {
		t.Fatalf("ListByDevice(tx) = %+v, want empty", list)
	}

	// Other base names derive their own ids.
	other, err := registry.UpsertDeviceRoot(device, "/mnt/Projects/MindFS")
	if err != nil {
		t.Fatalf("UpsertDeviceRoot second path returned error: %v", err)
	}
	if other.ID != "nas-mindfs" {
		t.Fatalf("second root id = %q, want nas-mindfs", other.ID)
	}
	if filepath.Base(other.ID) != other.ID || strings.ContainsAny(other.ID, `/\\`) {
		t.Fatalf("device root id %q must pass validRootID shape", other.ID)
	}

	// A same-id entry bound to another device or path is rejected.
	// Another device id derives its own id.
	if _, err := registry.UpsertDeviceRoot(remote.Device{ID: "tx"}, "/somewhere-else"); err != nil {
		t.Fatalf("unexpected error for new device id: %v", err)
	}
	// Same derived id (base "mindfs"), different path → conflict.
	if _, err := registry.UpsertDeviceRoot(remote.Device{ID: "nas"}, "/different/MindFS"); err == nil {
		t.Fatal("conflicting path for derived base should fail; got nil")
	} else if !errors.Is(err, ErrRootNameConflict) {
		t.Fatalf("conflict error = %v, want ErrRootNameConflict", err)
	}
	// A local (non-device) root holding the derived id is never hijacked.
	localDir := filepath.Join(t.TempDir(), "nas-local")
	if err := os.Mkdir(localDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := registry.Upsert(localDir); err != nil {
		t.Fatalf("Upsert local root returned error: %v", err)
	}
	if _, err := registry.UpsertDeviceRoot(device, "/mnt/remote/local"); err == nil {
		t.Fatal("device root colliding with a local root id should fail")
	} else if !errors.Is(err, ErrRootNameConflict) {
		t.Fatalf("local-root collision error = %v, want ErrRootNameConflict", err)
	}

	// Empty/relative remote paths are rejected.
	if _, err := registry.UpsertDeviceRoot(device, "relative/path"); err == nil {
		t.Fatal("relative device path should fail")
	}
	if _, err := registry.UpsertDeviceRoot(remote.Device{}, "/mnt/0"); err == nil {
		t.Fatal("missing device id should fail")
	}

	// Persistence round trip keeps the device binding.
	reloaded := NewRegistry(registry.path)
	if err := reloaded.Load(); err != nil {
		t.Fatalf("Load returned error: %v", err)
	}
	stored, ok := reloaded.Get("nas-0")
	if !ok || stored.Device != "nas" || stored.RootPath != "/mnt/0" {
		t.Fatalf("persisted root = %+v, want device nas path /mnt/0", stored)
	}

	if err := reloaded.RemoveByDevice("nas"); err != nil {
		t.Fatalf("RemoveByDevice returned error: %v", err)
	}
	if list := reloaded.ListByDevice("nas"); len(list) != 0 {
		t.Fatalf("ListByDevice after removal = %+v, want empty", list)
	}
	if list := reloaded.ListByDevice("tx"); len(list) != 1 {
		t.Fatalf("tx roots after nas removal = %+v, want 1", list)
	}
	// No-op removal for unknown devices.
	if err := reloaded.RemoveByDevice("ghost"); err != nil {
		t.Fatalf("RemoveByDevice(ghost) returned error: %v", err)
	}
}
