package preferences

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"time"
)

// syncedNow is a seam for tests to produce deterministic timestamps.
var syncedNow = time.Now

// SyncedPreferences is the cross-instance subset of user preferences that may
// follow the user between MindFS servers. Machine-local data (pinned models,
// agent defaults, launcher nodes, session naming) is deliberately excluded:
// those reference per-host agents and paths and would dangle if replicated.
type SyncedPreferences struct {
	SyncedAtMs    int64           `json:"synced_at_ms"`
	UILayout      *storedUILayout `json:"ui_layout,omitempty"`
	ForkOwnership string          `json:"fork_ownership,omitempty"`
	ClientUI      json.RawMessage `json:"client_ui,omitempty"`
}

// ExportSynced returns the syncable block plus its content hash. The hash
// covers everything except SyncedAtMs, so identical content seeded at
// different times compares equal and imports stay idempotent.
func (s *Store) ExportSynced() (SyncedPreferences, string) {
	block := SyncedPreferences{}
	if s != nil {
		s.mu.RLock()
		block.SyncedAtMs = s.data.SyncedAtMs
		block.UILayout = cloneStoredUILayout(s.data.UILayout)
		block.ForkOwnership = s.data.ForkOwnership
		if len(s.data.ClientUI) > 0 {
			block.ClientUI = append(json.RawMessage{}, s.data.ClientUI...)
		}
		s.mu.RUnlock()
	}
	return block, block.contentHash()
}

func (b SyncedPreferences) contentHash() string {
	b.SyncedAtMs = 0
	payload, err := json.Marshal(b)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(payload)
	return hex.EncodeToString(sum[:])
}

// ApplySynced installs a block replicated from another instance. Rules:
//   - older SyncedAtMs than local → stale, rejected (caller re-exports);
//   - equal-or-newer with identical content hash → no-op;
//   - equal ms with different content → applied: the reconciler picks one
//     winner and pushes outward, so accepting ties converges without ping-pong.
//
// Unset fields in the incoming block keep the local value (a partial block
// must not erase a setting the peer never had).
func (s *Store) ApplySynced(incoming SyncedPreferences) (applied bool, stale bool, err error) {
	if s == nil {
		return false, false, errors.New("preferences store not configured")
	}
	if incoming.ClientUI != nil && !json.Valid(incoming.ClientUI) {
		return false, false, errors.New("incoming client_ui is not valid json")
	}
	if incoming.ForkOwnership != "" &&
		incoming.ForkOwnership != ForkOwnershipNested && incoming.ForkOwnership != ForkOwnershipDetached {
		return false, false, errors.New("incoming fork_ownership is invalid")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if incoming.SyncedAtMs < s.data.SyncedAtMs {
		return false, true, nil
	}
	if incoming.contentHash() == (SyncedPreferences{
		SyncedAtMs:    s.data.SyncedAtMs,
		UILayout:      cloneStoredUILayout(s.data.UILayout),
		ForkOwnership: s.data.ForkOwnership,
		ClientUI:      append(json.RawMessage{}, s.data.ClientUI...),
	}).contentHash() {
		return false, false, nil
	}
	previous := s.data
	s.data.SyncedAtMs = incoming.SyncedAtMs
	if incoming.UILayout != nil {
		s.data.UILayout = cloneStoredUILayout(incoming.UILayout)
	}
	if incoming.ForkOwnership != "" {
		s.data.ForkOwnership = incoming.ForkOwnership
	}
	if len(incoming.ClientUI) > 0 {
		s.data.ClientUI = append(json.RawMessage{}, incoming.ClientUI...)
	}
	if err := s.saveLocked(); err != nil {
		s.data = previous
		return false, false, err
	}
	return true, false, nil
}

func cloneStoredUILayout(in *storedUILayout) *storedUILayout {
	if in == nil {
		return nil
	}
	out := &storedUILayout{}
	if in.SidebarsSwapped != nil {
		v := *in.SidebarsSwapped
		out.SidebarsSwapped = &v
	}
	if in.GitDiffSideBySide != nil {
		v := *in.GitDiffSideBySide
		out.GitDiffSideBySide = &v
	}
	return out
}
