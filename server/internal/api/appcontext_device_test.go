package api

import (
	"path/filepath"
	"testing"

	"mindfs/internal/remote"
	"mindfs/server/internal/fs"
)

// Device roots live on a remote host; local os.Stat must never delete their
// registration (ListRoots / GetRootContext existence check).
func TestDeviceRootsSurviveExistenceCheck(t *testing.T) {
	registryPath := filepath.Join(t.TempDir(), "registry.json")
	registry := fs.NewRegistry(registryPath)
	device := remote.Device{ID: "nas", Name: "NAS", Host: "100.118.126.86", User: "root", Enabled: true}
	if _, err := registry.UpsertDeviceRoot(device, "/mnt/proj"); err != nil {
		t.Fatalf("UpsertDeviceRoot returned error: %v", err)
	}
	// Contrast: a local root whose directory is missing is cleaned up.
	if _, err := registry.Upsert(filepath.Join(t.TempDir(), "gone")); err != nil {
		t.Fatalf("Upsert returned error: %v", err)
	}

	s := &AppContext{Dirs: registry}
	roots := s.ListRoots()
	if len(roots) != 1 || roots[0].Device != "nas" {
		t.Fatalf("ListRoots = %+v, want only the device root", roots)
	}
	if _, ok := registry.Get("nas-proj"); !ok {
		t.Fatal("device root registration was removed by ListRoots")
	}

	root, err := s.GetRoot("nas-proj")
	if err != nil {
		t.Fatalf("GetRoot(device root) returned error: %v", err)
	}
	if root.Device != "nas" || root.RootPath != "/mnt/proj" {
		t.Fatalf("GetRoot = %+v, want the device root", root)
	}
}
