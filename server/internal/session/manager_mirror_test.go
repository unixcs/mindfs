package session

import (
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	rootfs "mindfs/server/internal/fs"
)

// mirrorFixtureKey is a valid session key matching the whitelist.
const mirrorFixtureKey = "1760000000-a1b2c3d4e5f6"

// buildMirrorBundle materializes a fake "fetched remote meta" directory with
// the same layout remote.FetchRemoteMeta produces: <dir>/sessions/session-list.db
// plus per-key jsonl logs.
func buildMirrorBundle(t *testing.T, sessions []struct {
	Key       string
	Name      string
	UpdatedAt time.Time
	Agent     string
	AgentID   string
}) string {
	t.Helper()
	dir := t.TempDir()
	sessionsDir := filepath.Join(dir, "sessions")
	if err := os.MkdirAll(sessionsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", filepath.Join(sessionsDir, "session-list.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TABLE sessions (
		key TEXT PRIMARY KEY, type TEXT NOT NULL DEFAULT 'chat', name TEXT NOT NULL DEFAULT '',
		created_at TEXT NOT NULL DEFAULT '', updated_at TEXT NOT NULL DEFAULT ''
	)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE session_agent_bindings (
		session_key TEXT NOT NULL, agent TEXT NOT NULL, agent_session_id TEXT NOT NULL DEFAULT '',
		agent_ctx_seq INTEGER NOT NULL DEFAULT 0,
		external_source_path TEXT NOT NULL DEFAULT '', external_source_offset INTEGER NOT NULL DEFAULT 0,
		external_source_mtime_ns INTEGER NOT NULL DEFAULT 0, PRIMARY KEY (session_key, agent)
	)`); err != nil {
		t.Fatal(err)
	}
	for _, s := range sessions {
		if _, err := db.Exec(`INSERT INTO sessions (key, type, name, created_at, updated_at) VALUES (?, 'chat', ?, ?, ?)`,
			s.Key, s.Name, s.UpdatedAt.UTC().Format(time.RFC3339Nano), s.UpdatedAt.UTC().Format(time.RFC3339Nano)); err != nil {
			t.Fatal(err)
		}
		if s.Agent != "" {
			if _, err := db.Exec(`INSERT INTO session_agent_bindings (session_key, agent, agent_session_id, agent_ctx_seq) VALUES (?, ?, ?, 2)`,
				s.Key, s.Agent, s.AgentID); err != nil {
				t.Fatal(err)
			}
		}
		exchanges := []Exchange{
			{Seq: 1, Role: "user", Content: "hello from remote"},
			{Seq: 2, Role: "assistant", Agent: s.Agent, Content: "remote reply"},
		}
		var log strings.Builder
		for _, exchange := range exchanges {
			payload, err := json.Marshal(exchange)
			if err != nil {
				t.Fatal(err)
			}
			log.Write(payload)
			log.WriteByte('\n')
		}
		if err := os.WriteFile(filepath.Join(sessionsDir, s.Key+".jsonl"), []byte(log.String()), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func TestImportMirrorBundleImportsAndRewritesBindings(t *testing.T) {
	root := rootfs.NewRootInfo("mindfs", "mindfs", t.TempDir())
	manager := NewManager(root)
	remoteUpdated := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	bundleDir := buildMirrorBundle(t, []struct {
		Key       string
		Name      string
		UpdatedAt time.Time
		Agent     string
		AgentID   string
	}{
		{Key: mirrorFixtureKey, Name: "Remote Session", UpdatedAt: remoteUpdated, Agent: "codex", AgentID: "remote-codex-1"},
		{Key: "1760000001-bb", Name: "Remote Session 2", UpdatedAt: remoteUpdated, Agent: "qoder", AgentID: "remote-qoder-1"},
	})

	result, err := manager.ImportMirrorBundle(context.Background(), MirrorImportInput{
		DeviceID:          "jp",
		SourceDir:         bundleDir,
		RemoteSessionsDir: "/opt/mindfs-data/.mindfs/jp-proj/sessions",
		// Real-world mapping shape: provisioned device agents exist for codex
		// (jp-codex); "qoder" has no device agent → keep original name.
		MapAgent: func(agent string) (string, bool) {
			if agent == "codex" {
				return "jp-codex", true
			}
			return "", false
		},
	})
	if err != nil {
		t.Fatalf("ImportMirrorBundle: %v", err)
	}
	if result.Imported != 2 || result.Updated != 0 || result.Skipped != 0 {
		t.Fatalf("result = %+v, want imported=2", result)
	}
	if len(result.SkippedAgents) != 1 || result.SkippedAgents[0] != "qoder" {
		t.Fatalf("SkippedAgents = %v, want [qoder]", result.SkippedAgents)
	}

	imported, err := manager.Get(context.Background(), mirrorFixtureKey, 0)
	if err != nil {
		t.Fatalf("get imported session: %v", err)
	}
	if imported.Source != "remote:jp" {
		t.Fatalf("Source = %q, want remote:jp", imported.Source)
	}
	if imported.Name != "Remote Session" {
		t.Fatalf("Name = %q", imported.Name)
	}
	if !imported.UpdatedAt.Equal(remoteUpdated) {
		t.Fatalf("UpdatedAt = %v, want %v", imported.UpdatedAt, remoteUpdated)
	}
	if len(imported.Exchanges) != 2 || imported.Exchanges[0].Content != "hello from remote" {
		t.Fatalf("exchanges = %+v", imported.Exchanges)
	}

	// Binding rewrite: remote "codex" binding now points at the device agent.
	binding, err := manager.GetAgentBinding(context.Background(), mirrorFixtureKey, "jp-codex")
	if err != nil {
		t.Fatalf("get rewritten binding: %v", err)
	}
	if binding.AgentSessionID != "remote-codex-1" {
		t.Fatalf("AgentSessionID = %q, want remote-codex-1", binding.AgentSessionID)
	}
	if binding.ExternalSourcePath != "/opt/mindfs-data/.mindfs/jp-proj/sessions/codex.jsonl" {
		t.Fatalf("ExternalSourcePath = %q", binding.ExternalSourcePath)
	}
	// Unmapped agent keeps its original name so the session still renders.
	if _, err := manager.GetAgentBinding(context.Background(), "1760000001-bb", "qoder"); err != nil {
		t.Fatalf("original binding for unmapped agent should survive: %v", err)
	}
	// The remote binding name itself must not linger under its old key.
	if _, err := manager.GetAgentBinding(context.Background(), mirrorFixtureKey, "codex"); err == nil {
		t.Fatalf("remote binding 'codex' should have been rewritten to 'jp-codex'")
	}
}

func TestImportMirrorBundleLocalNewerWins(t *testing.T) {
	root := rootfs.NewRootInfo("mindfs", "mindfs", t.TempDir())
	manager := NewManager(root)

	older := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	newer := time.Date(2026, 1, 9, 0, 0, 0, 0, time.UTC)

	// Local session exists and is newer than the remote copy. The manager
	// clock is pinned so its UpdatedAt is deterministic.
	manager = NewManager(root, WithClock(func() time.Time { return newer }))
	if _, err := manager.Create(context.Background(), CreateInput{Type: TypeChat, Key: mirrorFixtureKey, Name: "Local Wins"}); err != nil {
		t.Fatal(err)
	}
	currentSession, err := manager.Get(context.Background(), mirrorFixtureKey, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.AddExchangeForAgentAt(context.Background(), currentSession, "assistant", "local reply", "codex", "", "", "", newer); err != nil {
		t.Fatal(err)
	}

	bundleDir := buildMirrorBundle(t, []struct {
		Key       string
		Name      string
		UpdatedAt time.Time
		Agent     string
		AgentID   string
	}{
		{Key: mirrorFixtureKey, Name: "Remote Older", UpdatedAt: older, Agent: "codex", AgentID: "remote-1"},
	})
	result, err := manager.ImportMirrorBundle(context.Background(), MirrorImportInput{
		DeviceID:  "jp",
		SourceDir: bundleDir,
		MapAgent:  func(agent string) (string, bool) { return "jp-" + agent, true },
	})
	if err != nil {
		t.Fatalf("ImportMirrorBundle: %v", err)
	}
	if result.Imported != 0 || result.Updated != 0 || result.Skipped != 1 {
		t.Fatalf("result = %+v, want skipped=1", result)
	}
	current, err := manager.Get(context.Background(), mirrorFixtureKey, 0)
	if err != nil {
		t.Fatal(err)
	}
	if current.Name != "Local Wins" || len(current.Exchanges) != 1 || current.Exchanges[0].Content != "local reply" {
		t.Fatalf("local session was overwritten: %+v", current)
	}

	// A NEWER remote copy overwrites the local meta (and log).
	bundleDir2 := buildMirrorBundle(t, []struct {
		Key       string
		Name      string
		UpdatedAt time.Time
		Agent     string
		AgentID   string
	}{
		{Key: mirrorFixtureKey, Name: "Remote Newer", UpdatedAt: newer.Add(time.Hour), Agent: "codex", AgentID: "remote-2"},
	})
	result2, err := manager.ImportMirrorBundle(context.Background(), MirrorImportInput{
		DeviceID:  "jp",
		SourceDir: bundleDir2,
		MapAgent:  func(agent string) (string, bool) { return "jp-" + agent, true },
	})
	if err != nil {
		t.Fatalf("ImportMirrorBundle #2: %v", err)
	}
	if result2.Updated != 1 || result2.Imported != 0 {
		t.Fatalf("result2 = %+v, want updated=1", result2)
	}
	current, err = manager.Get(context.Background(), mirrorFixtureKey, 0)
	if err != nil {
		t.Fatal(err)
	}
	if current.Name != "Remote Newer" || len(current.Exchanges) != 2 || current.Exchanges[0].Content != "hello from remote" {
		t.Fatalf("remote newer copy not applied: %+v", current)
	}
}

func TestImportMirrorBundleRejectsInvalidKeys(t *testing.T) {
	root := rootfs.NewRootInfo("mindfs", "mindfs", t.TempDir())
	manager := NewManager(root)
	bundleDir := buildMirrorBundle(t, []struct {
		Key       string
		Name      string
		UpdatedAt time.Time
		Agent     string
		AgentID   string
	}{
		// Slash-free but rejected by the whitelist (embedded ".."), so the
		// fixture never writes outside the bundle dir.
		{Key: "with..dots", Name: "Evil", UpdatedAt: time.Now().UTC(), Agent: "", AgentID: ""},
	})

	result, err := manager.ImportMirrorBundle(context.Background(), MirrorImportInput{
		DeviceID:  "jp",
		SourceDir: bundleDir,
	})
	if err != nil {
		t.Fatalf("ImportMirrorBundle: %v", err)
	}
	if result.Imported != 0 || result.Skipped != 1 {
		t.Fatalf("result = %+v, want skipped=1", result)
	}
	if len(result.Errors) == 0 || !strings.Contains(strings.Join(result.Errors, " "), "invalid key") {
		t.Fatalf("errors = %v, want invalid-key message", result.Errors)
	}
	// Nothing may be written under the local meta dir.
	sessionsDir := filepath.Join(root.MetaDir(), "sessions")
	entries, _ := os.ReadDir(sessionsDir)
	for _, entry := range entries {
		if strings.HasSuffix(entry.Name(), ".jsonl") {
			t.Fatalf("unexpected log written for invalid key: %s", entry.Name())
		}
	}
}

func TestImportMirrorBundleCorruptDBCopiesLogs(t *testing.T) {
	root := rootfs.NewRootInfo("mindfs", "mindfs", t.TempDir())
	manager := NewManager(root)
	bundleDir := t.TempDir()
	sessionsDir := filepath.Join(bundleDir, "sessions")
	if err := os.MkdirAll(sessionsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sessionsDir, "session-list.db"), []byte("this is not sqlite"), 0o644); err != nil {
		t.Fatal(err)
	}
	logPayload := []byte("{\"seq\":1,\"role\":\"user\",\"content\":\"orphan log\"}\n")
	if err := os.WriteFile(filepath.Join(sessionsDir, mirrorFixtureKey+".jsonl"), logPayload, 0o644); err != nil {
		t.Fatal(err)
	}

	result, err := manager.ImportMirrorBundle(context.Background(), MirrorImportInput{DeviceID: "jp", SourceDir: bundleDir})
	if err != nil {
		t.Fatalf("ImportMirrorBundle: %v", err)
	}
	if len(result.Errors) == 0 || !strings.Contains(strings.Join(result.Errors, " "), "integrity_check") {
		t.Fatalf("errors = %v, want integrity_check failure", result.Errors)
	}
	// The plain log is still copied even though the db was unreadable.
	payload, err := os.ReadFile(filepath.Join(root.MetaDir(), "sessions", mirrorFixtureKey+".jsonl"))
	if err != nil {
		t.Fatalf("orphan log not copied: %v", err)
	}
	if string(payload) != string(logPayload) {
		t.Fatalf("orphan log content mismatch: %q", payload)
	}
}

func TestImportMirrorBundleEmptyBundleIsNoop(t *testing.T) {
	root := rootfs.NewRootInfo("mindfs", "mindfs", t.TempDir())
	manager := NewManager(root)
	result, err := manager.ImportMirrorBundle(context.Background(), MirrorImportInput{DeviceID: "jp", SourceDir: t.TempDir()})
	if err != nil {
		t.Fatalf("ImportMirrorBundle: %v", err)
	}
	if result.Imported != 0 || result.Updated != 0 || result.Skipped != 0 || len(result.Errors) != 0 {
		t.Fatalf("result = %+v, want zeros", result)
	}
}

func TestWriteExchangeLogIfMissing(t *testing.T) {
	root := rootfs.NewRootInfo("mindfs", "mindfs", t.TempDir())
	manager := NewManager(root)
	created, err := manager.Create(context.Background(), CreateInput{Type: TypeChat, Name: "Lazy"})
	if err != nil {
		t.Fatal(err)
	}
	key := created.Key
	payload := []byte("{\"seq\":1,\"role\":\"user\",\"content\":\"lazy pull\"}\n")

	// Simulate a mirrored meta row whose log is missing: drop the log file.
	logPath := filepath.Join(root.MetaDir(), "sessions", key+".jsonl")
	if err := os.Remove(logPath); err != nil {
		t.Fatal(err)
	}
	if manager.ExchangeLogExists(key) {
		t.Fatalf("ExchangeLogExists should be false after removal")
	}
	wrote, err := manager.WriteExchangeLogIfMissing(key, payload)
	if err != nil || !wrote {
		t.Fatalf("WriteExchangeLogIfMissing = %v, %v", wrote, err)
	}
	// Second call is a no-op (log exists).
	wrote, err = manager.WriteExchangeLogIfMissing(key, []byte("different"))
	if err != nil || wrote {
		t.Fatalf("second WriteExchangeLogIfMissing = %v, %v, want false", wrote, err)
	}
	// The cache was invalidated: Get re-reads the log from disk.
	current, err := manager.Get(context.Background(), key, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(current.Exchanges) != 1 || current.Exchanges[0].Content != "lazy pull" {
		t.Fatalf("exchanges after lazy pull = %+v", current.Exchanges)
	}
	// Invalid keys are rejected before any disk access.
	if manager.ExchangeLogExists("../evil") {
		t.Fatalf("invalid key should not resolve to an existing log")
	}
}
