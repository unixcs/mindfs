package preferences

import (
	"encoding/json"
	"path/filepath"
	"testing"
	"time"
)

func stubSyncedNow(t *testing.T, ms int64) {
	t.Helper()
	prev := syncedNow
	syncedNow = func() time.Time { return time.UnixMilli(ms) }
	t.Cleanup(func() { syncedNow = prev })
}

func newSyncStore(t *testing.T) (*Store, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "preferences.json")
	return &Store{path: path}, path
}

func TestSyncedTimestampBumpedByUserEdits(t *testing.T) {
	s, _ := newSyncStore(t)
	stubSyncedNow(t, 1_700_000_000_000)
	if err := s.UpdateUILayout(true, false); err != nil {
		t.Fatal(err)
	}
	if s.data.SyncedAtMs != 1_700_000_000_000 {
		t.Fatalf("ui layout edit must stamp synced time, got %d", s.data.SyncedAtMs)
	}
	stubSyncedNow(t, 1_700_000_000_500)
	if err := s.UpdateClientUI(json.RawMessage(`{"v":1}`)); err != nil {
		t.Fatal(err)
	}
	if s.data.SyncedAtMs != 1_700_000_000_500 {
		t.Fatalf("client ui edit must stamp synced time, got %d", s.data.SyncedAtMs)
	}
	// Idempotent saves must not stamp.
	stubSyncedNow(t, 1_700_000_001_000)
	if err := s.UpdateClientUI(json.RawMessage(`{"v":1}`)); err != nil {
		t.Fatal(err)
	}
	if s.data.SyncedAtMs != 1_700_000_000_500 {
		t.Fatalf("unchanged client ui must not bump synced time, got %d", s.data.SyncedAtMs)
	}
}

func TestApplySyncedRejectsStaleAndAcceptsNewer(t *testing.T) {
	s, _ := newSyncStore(t)
	stubSyncedNow(t, 2_000)
	if err := s.UpdateClientUI(json.RawMessage(`{"v":1,"appearance":"dark"}`)); err != nil {
		t.Fatal(err)
	}
	stubSyncedNow(t, 3_000)
	if err := s.UpdateForkOwnership(ForkOwnershipDetached); err != nil {
		t.Fatal(err)
	}

	// Older block → stale, nothing changes.
	applied, stale, err := s.ApplySynced(SyncedPreferences{
		SyncedAtMs: 2_500,
		ClientUI:   json.RawMessage(`{"v":1,"appearance":"light"}`),
	})
	if err != nil || !stale || applied {
		t.Fatalf("expected stale reject, got applied=%v stale=%v err=%v", applied, stale, err)
	}
	if s.ForkOwnership() != ForkOwnershipDetached {
		t.Fatal("stale import must not touch local state")
	}

	// Newer block → applied field-wise; unset incoming fields keep local values.
	applied, stale, err = s.ApplySynced(SyncedPreferences{
		SyncedAtMs: 4_000,
		ClientUI:   json.RawMessage(`{"v":1,"appearance":"light"}`),
	})
	if err != nil || stale || !applied {
		t.Fatalf("expected applied, got applied=%v stale=%v err=%v", applied, stale, err)
	}
	if s.data.SyncedAtMs != 4_000 {
		t.Fatalf("import must adopt incoming timestamp, got %d", s.data.SyncedAtMs)
	}
	if s.ForkOwnership() != ForkOwnershipDetached {
		t.Fatal("partial import must not erase fork ownership")
	}
	if raw, _ := s.ClientUI(); string(raw) != `{"v":1,"appearance":"light"}` {
		t.Fatalf("client ui not replicated: %s", raw)
	}
}

func TestApplySyncedNoOpOnIdenticalContent(t *testing.T) {
	s, path := newSyncStore(t)
	stubSyncedNow(t, 5_000)
	if err := s.UpdateUILayout(true, true); err != nil {
		t.Fatal(err)
	}
	block, _ := s.ExportSynced()
	other, _ := newSyncStore(t)
	stubSyncedNow(t, 6_000)
	applied, stale, err := other.ApplySynced(block)
	if err != nil || stale {
		t.Fatal(err)
	}
	if !applied {
		t.Fatal("first import into empty store must apply")
	}
	// Same content again with a different timestamp: content hash says no-op.
	block2, _ := other.ExportSynced()
	block2.SyncedAtMs = 7_000
	applied, stale, err = other.ApplySynced(block2)
	if err != nil || stale || applied {
		t.Fatalf("identical content must be a no-op, got applied=%v stale=%v err=%v", applied, stale, err)
	}
	if other.data.SyncedAtMs != 5_000 {
		t.Fatalf("no-op must not rewrite timestamp, got %d", other.data.SyncedAtMs)
	}
	reloaded := &Store{path: path}
	if err := reloaded.load(); err != nil {
		t.Fatal(err)
	}
	reloadedBlock, reloadedHash := reloaded.ExportSynced()
	_, originalHash := s.ExportSynced()
	if reloadedHash != originalHash {
		t.Fatalf("export hash must survive reload: %s vs %s", reloadedHash, originalHash)
	}
	if reloadedBlock.SyncedAtMs != 5_000 {
		t.Fatalf("unexpected reloaded timestamp %d", reloadedBlock.SyncedAtMs)
	}
}

func TestApplySyncedTieDifferentContentApplies(t *testing.T) {
	s, _ := newSyncStore(t)
	stubSyncedNow(t, 1_000)
	if err := s.UpdateUILayout(false, false); err != nil {
		t.Fatal(err)
	}
	applied, stale, err := s.ApplySynced(SyncedPreferences{
		SyncedAtMs: 1_000,
		UILayout:   func() *storedUILayout { t := true; return &storedUILayout{SidebarsSwapped: &t} }(),
	})
	if err != nil || stale || !applied {
		t.Fatalf("equal timestamp with different content must converge, got applied=%v stale=%v err=%v", applied, stale, err)
	}
	if !s.UILayout().SidebarsSwapped {
		t.Fatal("tie import did not take effect")
	}
}

func TestUpdateClientUIRejectsInvalidJSON(t *testing.T) {
	s, _ := newSyncStore(t)
	if err := s.UpdateClientUI(json.RawMessage(`{"v":`)); err == nil {
		t.Fatal("invalid json must be rejected")
	}
	if _, configured := s.ClientUI(); configured {
		t.Fatal("rejected update must not configure client ui")
	}
}
