// Package remote implements SSH remote device support: device registry,
// SSH execution, the __remote-agent spawn shim, device probing and
// session mirroring from remote MindFS instances.
package remote

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
)

// Device describes one SSH-reachable machine whose CLI agents are
// aggregated into this MindFS instance.
type Device struct {
	ID string `json:"id"`
	// Name is a human label shown in the UI (defaults to ID).
	Name string `json:"name,omitempty"`
	Host string `json:"host"`
	Port int    `json:"port,omitempty"`
	User string `json:"user"`
	// IdentityFile is an absolute path to a private key. Empty = ssh
	// default identities (~/.ssh/id_*).
	IdentityFile string   `json:"identityFile,omitempty"`
	WatchRoots   []string `json:"watchRoots,omitempty"`
	// MindFSHome is the remote MindFS service HOME (for session mirror
	// discovery), e.g. /opt/mindfs-data. Empty disables mirroring.
	MindFSHome string `json:"mindfsHome,omitempty"`
	// PathPrefix is injected before the remote command, e.g.
	// "export PATH=$HOME/.opencode/bin:$PATH" (PATH escape hatch).
	PathPrefix string `json:"pathPrefix,omitempty"`
	// AutoCreateAgents derives <device>-<cli> agent entries on test.
	AutoCreateAgents bool `json:"autoCreateAgents"`
	Enabled          bool `json:"enabled"`
}

func (d Device) DisplayName() string {
	if name := strings.TrimSpace(d.Name); name != "" {
		return name
	}
	return d.ID
}

func (d Device) Target() Target {
	return Target{Host: d.Host, Port: d.Port, User: d.User, IdentityFile: d.IdentityFile}
}

var deviceIDPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,63}$`)

// Store persists devices.json under the MindFS config dir (0600).
type Store struct {
	mu      sync.RWMutex
	path    string
	devices []Device
}

func NewStore(configDir string) (*Store, error) {
	s := &Store{path: filepath.Join(configDir, "devices.json")}
	payload, err := os.ReadFile(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return nil, err
	}
	if len(strings.TrimSpace(string(payload))) == 0 {
		return s, nil
	}
	if err := json.Unmarshal(payload, &s.devices); err != nil {
		return nil, fmt.Errorf("parse devices.json: %w", err)
	}
	// Drop entries that would smuggle ssh argv options (the file is 0600 and
	// operator-owned, but validation at every load keeps the invariant that
	// no stored device can produce an option injection).
	kept := s.devices[:0]
	for _, d := range s.devices {
		if ValidateLoginArgs(d.Target()) == nil {
			kept = append(kept, d)
			continue
		}
		log.Printf("[remote/devices] dropped invalid device entry %q from %s (host/user/identity validation failed)", d.ID, s.path)
	}
	s.devices = kept
	return s, nil
}

func (s *Store) List() []Device {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]Device, len(s.devices))
	copy(out, s.devices)
	sort.SliceStable(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

func (s *Store) Get(id string) (Device, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, d := range s.devices {
		if d.ID == id {
			return d, true
		}
	}
	return Device{}, false
}

// Resolver returns the device lookup function consumed by the agent pool.
func (s *Store) Resolver() func(id string) (Device, bool) {
	return s.Get
}

func (s *Store) Save(d Device) error {
	d.ID = strings.TrimSpace(d.ID)
	d.Host = strings.TrimSpace(d.Host)
	d.User = strings.TrimSpace(d.User)
	d.IdentityFile = strings.TrimSpace(d.IdentityFile)
	// Accept the common "~/.ssh/id_ed25519" spelling; ssh needs an absolute
	// path because the shim runs with a minimal environment.
	if strings.HasPrefix(d.IdentityFile, "~/") || d.IdentityFile == "~" {
		if home, err := os.UserHomeDir(); err == nil && home != "" {
			d.IdentityFile = filepath.Join(home, strings.TrimPrefix(d.IdentityFile, "~"))
		}
	}
	if !deviceIDPattern.MatchString(d.ID) {
		return errors.New("设备 ID 只允许小写字母/数字/-/_，且以字母或数字开头")
	}
	if d.Host == "" || d.User == "" {
		return errors.New("host 和 user 必填")
	}
	if d.Port == 0 {
		d.Port = 22
	}
	if d.Port < 1 || d.Port > 65535 {
		return errors.New("端口非法")
	}
	// Rejects option-injection values in Host/User and relative identity
	// paths (see ValidateTarget: a user like "-oProxyCommand=..." would run
	// on the panel host).
	if err := ValidateLoginArgs(d.Target()); err != nil {
		return err
	}
	d.Name = strings.TrimSpace(d.Name)
	s.mu.Lock()
	defer s.mu.Unlock()
	replaced := false
	for i := range s.devices {
		if s.devices[i].ID == d.ID {
			s.devices[i] = d
			replaced = true
			break
		}
	}
	if !replaced {
		s.devices = append(s.devices, d)
	}
	return s.saveLocked()
}

func (s *Store) Delete(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	kept := s.devices[:0]
	found := false
	for _, d := range s.devices {
		if d.ID == id {
			found = true
			continue
		}
		kept = append(kept, d)
	}
	if !found {
		return fmt.Errorf("device %q not found", id)
	}
	s.devices = kept
	return s.saveLocked()
}

func (s *Store) saveLocked() error {
	payload, err := json.MarshalIndent(s.devices, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, append(payload, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}
