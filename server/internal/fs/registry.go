package fs

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"mindfs/internal/remote"
	"mindfs/server/internal/apperr"
	configpkg "mindfs/server/internal/config"
)

var ErrRootNameConflict = errors.New("root name already exists")

type Registry struct {
	mu    sync.Mutex
	path  string
	dirs  map[string]RootInfo
	order []string
}

func NewRegistry(path string) *Registry {
	return &Registry{path: path, dirs: make(map[string]RootInfo)}
}

func NewDefaultRegistry() (*Registry, error) {
	configDir, err := configpkg.MindFSConfigDir()
	if err != nil {
		return nil, err
	}
	path := filepath.Join(configDir, "registry.json")
	return NewRegistry(path), nil
}

func (r *Registry) Load() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	payload, err := os.ReadFile(r.path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return apperr.Wrap("read", r.path, err)
	}
	var stored struct {
		Dirs  []RootInfo `json:"dirs"`
		Order []string   `json:"order"`
	}
	if err := json.Unmarshal(payload, &stored); err != nil {
		return err
	}
	r.dirs = make(map[string]RootInfo)
	r.order = nil
	seen := make(map[string]struct{})
	for _, info := range stored.Dirs {
		name := info.Name
		if name == "" {
			name = filepath.Base(info.RootPath)
		}
		id := info.ID
		if id == "" {
			id = name
		}
		if id == "" || id == "." || id == string(filepath.Separator) {
			continue
		}
		if _, exists := seen[id]; exists {
			continue
		}
		seen[id] = struct{}{}
		info.Name = name
		info.ID = id
		r.dirs[id] = info
		r.order = append(r.order, id)
	}
	return nil
}

func (r *Registry) Save() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.saveLocked()
}

func (r *Registry) saveLocked() error {
	if r.path == "" {
		return errors.New("registry path required")
	}
	if err := os.MkdirAll(filepath.Dir(r.path), 0o755); err != nil {
		return apperr.Wrap("mkdir", filepath.Dir(r.path), err)
	}
	recs := make([]RootInfo, 0, len(r.dirs))
	for _, id := range r.order {
		if dir, ok := r.dirs[id]; ok {
			recs = append(recs, dir)
		}
	}
	payload, err := json.MarshalIndent(map[string]any{"dirs": recs, "order": r.order}, "", "  ")
	if err != nil {
		return err
	}
	return apperr.Wrap("write", r.path, os.WriteFile(r.path, payload, 0o644))
}

func (r *Registry) List() []RootInfo {
	r.mu.Lock()
	defer r.mu.Unlock()
	result := make([]RootInfo, 0, len(r.order))
	for _, id := range r.order {
		if dir, ok := r.dirs[id]; ok {
			result = append(result, dir)
		}
	}
	return result
}

func (r *Registry) Get(id string) (RootInfo, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	dir, ok := r.dirs[id]
	return dir, ok
}

func (r *Registry) Upsert(root string) (RootInfo, error) {
	return r.UpsertWithMetaLocation(root, MetaLocationProject)
}

func (r *Registry) UpsertWithMetaLocation(root, metaLocation string) (RootInfo, error) {
	if root == "" {
		return RootInfo{}, errors.New("root required")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	now := time.Now().UTC()
	name := filepath.Base(root)
	if name == "" || name == "." || name == string(filepath.Separator) {
		return RootInfo{}, errors.New("invalid directory name")
	}
	dir, ok := r.dirs[name]
	if !ok {
		metaLocation, err := NormalizeMetaLocation(metaLocation)
		if err != nil {
			return RootInfo{}, err
		}
		dir = NewRootInfo(name, name, root)
		dir.MetaLocation = metaLocation
		dir.CreatedAt = now
		r.order = append(r.order, name)
	} else if !sameRegistryPath(dir.RootPath, root) {
		return RootInfo{}, fmt.Errorf("%w: %q is already managed at %s; rename the directory before adding %s", ErrRootNameConflict, name, dir.RootPath, root)
	}
	dir.UpdatedAt = now
	r.dirs[name] = dir
	return dir, r.saveLocked()
}

func sameRegistryPath(a, b string) bool {
	left := cleanRegistryPath(a)
	right := cleanRegistryPath(b)
	if runtime.GOOS == "windows" {
		left = strings.ToLower(left)
		right = strings.ToLower(right)
	}
	return left == right
}

func cleanRegistryPath(path string) string {
	cleaned := filepath.Clean(strings.TrimSpace(path))
	if abs, err := filepath.Abs(cleaned); err == nil {
		return abs
	}
	return cleaned
}

func (r *Registry) Remove(root string) (RootInfo, error) {
	if root == "" {
		return RootInfo{}, errors.New("root required")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	cleaned := filepath.Clean(root)
	name := filepath.Base(cleaned)
	if name == "" || name == "." || name == string(filepath.Separator) {
		return RootInfo{}, errors.New("invalid directory name")
	}
	dir, ok := r.dirs[name]
	if !ok {
		return RootInfo{}, errors.New("root not found")
	}
	if filepath.Clean(dir.RootPath) != cleaned {
		return RootInfo{}, errors.New("root not found")
	}
	delete(r.dirs, name)
	nextOrder := make([]string, 0, len(r.order))
	for _, id := range r.order {
		if id != name {
			nextOrder = append(nextOrder, id)
		}
	}
	r.order = nextOrder
	if err := r.saveLocked(); err != nil {
		return RootInfo{}, err
	}
	return dir, nil
}

func (r *Registry) Rename(id, name, rootPath string) (RootInfo, error) {
	id = strings.TrimSpace(id)
	name = strings.TrimSpace(name)
	if id == "" {
		return RootInfo{}, errors.New("root id required")
	}
	if name == "" {
		return RootInfo{}, errors.New("root name required")
	}
	if rootPath == "" {
		return RootInfo{}, errors.New("root required")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	dir, ok := r.dirs[id]
	if !ok {
		return RootInfo{}, errors.New("root not found")
	}
	if existing, exists := r.dirs[name]; exists && existing.ID != id {
		return RootInfo{}, fmt.Errorf("%w: %q is already managed at %s", ErrRootNameConflict, name, existing.RootPath)
	}

	previousDirs := make(map[string]RootInfo, len(r.dirs))
	for key, value := range r.dirs {
		previousDirs[key] = value
	}
	previousOrder := append([]string(nil), r.order...)
	rollbackMeta, err := renameHomeMeta(dir, name, filepath.Clean(rootPath))
	if err != nil {
		return RootInfo{}, err
	}

	dir.ID = name
	dir.Name = name
	dir.RootPath = filepath.Clean(rootPath)
	dir.UpdatedAt = time.Now().UTC()
	delete(r.dirs, id)
	r.dirs[name] = dir
	for i, item := range r.order {
		if item == id {
			r.order[i] = name
			break
		}
	}
	if err := r.saveLocked(); err != nil {
		r.dirs = previousDirs
		r.order = previousOrder
		rollbackMeta()
		return RootInfo{}, err
	}
	return dir, nil
}

// sanitizeDeviceBase lowercases the last path segment and maps every rune
// outside [a-z0-9_-] to '-', yielding an empty result only when the segment
// is empty (mapped to "root" by the caller).
func sanitizeDeviceBase(remotePath string) string {
	base := path.Base(path.Clean(strings.TrimSpace(remotePath)))
	var b strings.Builder
	for _, char := range base {
		switch {
		case char >= 'a' && char <= 'z', char >= '0' && char <= '9', char == '_', char == '-':
			b.WriteRune(char)
		case char >= 'A' && char <= 'Z':
			b.WriteRune(char + ('a' - 'A'))
		default:
			b.WriteRune('-')
		}
	}
	return b.String()
}

func deviceRootID(deviceID, remotePath string) string {
	base := sanitizeDeviceBase(remotePath)
	if base == "" || base == "-" {
		base = "root"
	}
	return deviceID + "-" + base
}

func sameRemotePath(a, b string) bool {
	return path.Clean(strings.TrimSpace(a)) == path.Clean(strings.TrimSpace(b))
}

// UpsertDeviceRoot registers a read-only root backed by a remote device
// directory. The id is derived as <deviceID>-<base>; an existing id bound to
// another path or device is rejected so local roots can never be hijacked.
func (r *Registry) UpsertDeviceRoot(dev remote.Device, remotePath string) (RootInfo, error) {
	dev.ID = strings.TrimSpace(dev.ID)
	remotePath = path.Clean(strings.TrimSpace(remotePath))
	if dev.ID == "" {
		return RootInfo{}, errors.New("device id required")
	}
	if !path.IsAbs(remotePath) {
		return RootInfo{}, errors.New("device root path must be absolute")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	now := time.Now().UTC()
	id := deviceRootID(dev.ID, remotePath)
	if !validRootID(id) {
		return RootInfo{}, errors.New("invalid device root id")
	}
	if existing, ok := r.dirs[id]; ok {
		if existing.Device != dev.ID || !sameRemotePath(existing.RootPath, remotePath) {
			return RootInfo{}, fmt.Errorf("%w: root %q already exists (device=%q path=%s)", ErrRootNameConflict, id, existing.Device, existing.RootPath)
		}
		existing.UpdatedAt = now
		r.dirs[id] = existing
		return existing, r.saveLocked()
	}
	root := RootInfo{
		ID:           id,
		Name:         id,
		RootPath:     remotePath,
		Device:       dev.ID,
		MetaLocation: MetaLocationHome,
		CreatedAt:    now,
		UpdatedAt:    now,
	}
	r.dirs[id] = root
	r.order = append(r.order, id)
	return root, r.saveLocked()
}

// RemoveByDevice unregisters every root derived from the given device.
func (r *Registry) RemoveByDevice(deviceID string) error {
	deviceID = strings.TrimSpace(deviceID)
	if deviceID == "" {
		return errors.New("device id required")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	kept := make([]string, 0, len(r.order))
	changed := false
	for _, id := range r.order {
		if dir, ok := r.dirs[id]; ok && dir.Device == deviceID {
			delete(r.dirs, id)
			changed = true
			continue
		}
		kept = append(kept, id)
	}
	if !changed {
		return nil
	}
	r.order = kept
	return r.saveLocked()
}

// ListByDevice returns the registered roots derived from the given device.
func (r *Registry) ListByDevice(deviceID string) []RootInfo {
	deviceID = strings.TrimSpace(deviceID)
	r.mu.Lock()
	defer r.mu.Unlock()
	result := make([]RootInfo, 0, len(r.order))
	for _, id := range r.order {
		if dir, ok := r.dirs[id]; ok && dir.Device == deviceID {
			result = append(result, dir)
		}
	}
	return result
}
