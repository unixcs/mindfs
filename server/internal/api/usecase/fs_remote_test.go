package usecase

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"mindfs/internal/remote"
	"mindfs/server/internal/agent"
	agenttypes "mindfs/server/internal/agent/types"
	rootfs "mindfs/server/internal/fs"
	"mindfs/server/internal/preferences"
	"mindfs/server/internal/session"
)

type deviceTestRegistry struct {
	deviceRoot rootfs.RootInfo
	localRoot  rootfs.RootInfo
	runtime    DeviceRuntime
	hasRuntime bool
}

func (r *deviceTestRegistry) GetDeviceRuntime() (DeviceRuntime, bool) {
	return r.runtime, r.hasRuntime
}

func (r *deviceTestRegistry) GetRoot(rootID string) (rootfs.RootInfo, error) {
	if rootID == r.deviceRoot.ID && r.deviceRoot.ID != "" {
		return r.deviceRoot, nil
	}
	if rootID == r.localRoot.ID && r.localRoot.ID != "" {
		return r.localRoot, nil
	}
	return rootfs.RootInfo{}, errors.New("root not found")
}

func (r *deviceTestRegistry) ListRoots() []rootfs.RootInfo {
	roots := make([]rootfs.RootInfo, 0, 2)
	if r.deviceRoot.ID != "" {
		roots = append(roots, r.deviceRoot)
	}
	if r.localRoot.ID != "" {
		roots = append(roots, r.localRoot)
	}
	return roots
}

func (r *deviceTestRegistry) GetSessionManager(string) (*session.Manager, error) { return nil, nil }
func (r *deviceTestRegistry) UpsertRoot(string) (rootfs.RootInfo, error) {
	return rootfs.RootInfo{}, nil
}
func (r *deviceTestRegistry) RemoveRoot(string) (rootfs.RootInfo, error) {
	return rootfs.RootInfo{}, nil
}
func (r *deviceTestRegistry) RenameRoot(string, string, string) (rootfs.RootInfo, error) {
	return rootfs.RootInfo{}, nil
}
func (r *deviceTestRegistry) GetAgentPool() *agent.Pool          { return nil }
func (r *deviceTestRegistry) GetPreferences() *preferences.Store { return nil }
func (r *deviceTestRegistry) GetExternalSessionImporter(string) (agenttypes.ExternalSessionImporter, error) {
	return nil, errors.New("not implemented")
}
func (r *deviceTestRegistry) GetProber() *agent.Prober                 { return nil }
func (r *deviceTestRegistry) GetCandidateRegistry() *CandidateRegistry { return nil }
func (r *deviceTestRegistry) GetFileWatcher(string, *session.Manager) (*rootfs.SharedFileWatcher, error) {
	return nil, nil
}
func (r *deviceTestRegistry) ReleaseFileWatcher(string, string) {}

func deviceRootFixture() rootfs.RootInfo {
	return rootfs.RootInfo{
		ID:           "nas-0",
		Name:         "nas-0",
		RootPath:     "/mnt/0",
		Device:       "nas",
		MetaLocation: rootfs.MetaLocationHome,
	}
}

func deviceTestRegistryWithoutRuntime(t *testing.T) *deviceTestRegistry {
	t.Helper()
	return &deviceTestRegistry{deviceRoot: deviceRootFixture()}
}

func deviceTestRegistryWithEmptyStore(t *testing.T) *deviceTestRegistry {
	t.Helper()
	store, err := remote.NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewStore returned error: %v", err)
	}
	return &deviceTestRegistry{
		deviceRoot: deviceRootFixture(),
		runtime:    DeviceRuntime{Store: store, ConfigDir: t.TempDir()},
		hasRuntime: true,
	}
}

func TestListTreeDeviceRootRequiresRuntime(t *testing.T) {
	s := &Service{Registry: deviceTestRegistryWithoutRuntime(t)}
	_, err := s.ListTree(context.Background(), ListTreeInput{RootID: "nas-0", Dir: "sub"})
	if err == nil || !strings.Contains(err.Error(), "远程设备功能未启用") {
		t.Fatalf("ListTree error = %v, want 远程设备功能未启用", err)
	}
}

func TestListTreeDeviceRootRequiresKnownDevice(t *testing.T) {
	s := &Service{Registry: deviceTestRegistryWithEmptyStore(t)}
	_, err := s.ListTree(context.Background(), ListTreeInput{RootID: "nas-0"})
	if err == nil || !strings.Contains(err.Error(), "设备不存在") {
		t.Fatalf("ListTree error = %v, want 设备不存在", err)
	}
}

func TestReadFileDeviceRootRequiresRuntime(t *testing.T) {
	s := &Service{Registry: deviceTestRegistryWithoutRuntime(t)}
	_, err := s.ReadFile(context.Background(), ReadFileInput{RootID: "nas-0", Path: "a.txt"})
	if err == nil || !strings.Contains(err.Error(), "远程设备功能未启用") {
		t.Fatalf("ReadFile error = %v, want 远程设备功能未启用", err)
	}
}

func TestGetFileInfoDeviceRootRequiresRuntime(t *testing.T) {
	s := &Service{Registry: deviceTestRegistryWithoutRuntime(t)}
	_, err := s.GetFileInfo(context.Background(), GetFileInfoInput{RootID: "nas-0", Path: "a.txt"})
	if err == nil || !strings.Contains(err.Error(), "远程设备功能未启用") {
		t.Fatalf("GetFileInfo error = %v, want 远程设备功能未启用", err)
	}
}

func TestOpenFileRawDeviceRootRejected(t *testing.T) {
	s := &Service{Registry: deviceTestRegistryWithoutRuntime(t)}
	_, err := s.OpenFileRaw(context.Background(), OpenFileRawInput{RootID: "nas-0", Path: "a.txt"})
	if err == nil || !strings.Contains(err.Error(), "raw") {
		t.Fatalf("OpenFileRaw error = %v, want raw 限制说明", err)
	}
}

func TestSaveUploadedFilesDeviceRootRejected(t *testing.T) {
	s := &Service{Registry: deviceTestRegistryWithoutRuntime(t)}
	_, err := s.SaveUploadedFiles(context.Background(), SaveUploadedFilesInput{
		RootID: "nas-0",
		Dir:    "docs",
		Files:  []UploadFile{{Name: "x.txt", Reader: strings.NewReader("hello")}},
	})
	if err == nil || !strings.Contains(err.Error(), "远程设备目录只读") {
		t.Fatalf("SaveUploadedFiles error = %v, want 远程设备目录只读", err)
	}
}

func TestRenameManagedDirDeviceRootRejected(t *testing.T) {
	s := &Service{Registry: deviceTestRegistryWithoutRuntime(t)}
	_, err := s.RenameManagedDir(context.Background(), RenameManagedDirInput{RootID: "nas-0", Name: "renamed"})
	if err == nil || !strings.Contains(err.Error(), "远程设备目录只读") {
		t.Fatalf("RenameManagedDir error = %v, want 远程设备目录只读", err)
	}
}

func TestRemoveManagedDirDeviceRootRejected(t *testing.T) {
	registry := deviceTestRegistryWithoutRuntime(t)
	removed := false
	// Wrap RemoveRoot to prove the rejection happens before unregistration.
	orig := registry.RemoveRoot
	_ = orig
	s := &Service{Registry: &removeGuardRegistry{deviceTestRegistry: registry, onRemove: func() { removed = true }}}
	_, err := s.RemoveManagedDir(context.Background(), RemoveManagedDirInput{Path: "/mnt/0"})
	if err == nil || !strings.Contains(err.Error(), "远程设备目录只读") {
		t.Fatalf("RemoveManagedDir error = %v, want 远程设备目录只读", err)
	}
	if removed {
		t.Fatal("RemoveRoot must not be called for device roots")
	}
}

type removeGuardRegistry struct {
	*deviceTestRegistry
	onRemove func()
}

func (r *removeGuardRegistry) RemoveRoot(path string) (rootfs.RootInfo, error) {
	r.onRemove()
	return r.deviceTestRegistry.RemoveRoot(path)
}

func TestListTreeLocalRootUnaffectedWithoutRuntime(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	s := &Service{Registry: &deviceTestRegistry{
		localRoot: rootfs.RootInfo{ID: "proj", Name: "proj", RootPath: dir, MetaLocation: rootfs.MetaLocationHome},
	}}
	out, err := s.ListTree(context.Background(), ListTreeInput{RootID: "proj"})
	if err != nil {
		t.Fatalf("ListTree local root returned error: %v", err)
	}
	found := map[string]bool{}
	for _, entry := range out.Entries {
		found[entry.Name] = true
	}
	if !found["sub"] || !found["a.txt"] {
		t.Fatalf("ListTree entries missing expected items: %+v", out.Entries)
	}
}
