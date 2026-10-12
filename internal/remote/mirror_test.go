package remote

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestIsValidSessionKey(t *testing.T) {
	valid := []string{
		"1760000000-a1b2c3d4e5f6",
		"abc_123-1.2",
		"A.B-c_d-e",
	}
	for _, key := range valid {
		if !IsValidSessionKey(key) {
			t.Fatalf("IsValidSessionKey(%q) = false, want true", key)
		}
	}
	invalid := []string{
		"",
		"   ",
		"a/b",
		"../escape",
		"..",
		"a b",
		"键",
		"with\nnewline",
		strings.Repeat("a", 129), // > 128 chars
	}
	for _, key := range invalid {
		if IsValidSessionKey(key) {
			t.Fatalf("IsValidSessionKey(%q) = true, want false", key)
		}
	}
}

func TestParseRemoteRegistry(t *testing.T) {
	payload := []byte(`{"dirs":[{"id":"proj-a","name":"A","root_path":"/srv/a","meta_location":"project"},{"id":"home-x","name":"X","root_path":"~","meta_location":"home"}],"order":["proj-a","home-x"],"future_field":true}`)
	roots, err := ParseRemoteRegistry(payload)
	if err != nil {
		t.Fatalf("ParseRemoteRegistry: %v", err)
	}
	if len(roots) != 2 {
		t.Fatalf("len(roots) = %d, want 2", len(roots))
	}
	if roots[0].ID != "proj-a" || roots[0].RootPath != "/srv/a" {
		t.Fatalf("roots[0] = %+v", roots[0])
	}
	if roots[1].MetaLocation != "home" {
		t.Fatalf("roots[1].MetaLocation = %q, want home", roots[1].MetaLocation)
	}
}

func TestRemoteConfigDirAndMetaDir(t *testing.T) {
	if got := RemoteConfigDir("/opt/mindfs-data"); got != "/opt/mindfs-data/.config/mindfs" {
		t.Fatalf("RemoteConfigDir = %q", got)
	}
	project := RemoteRootInfo{ID: "proj-a", RootPath: "/srv/a", MetaLocation: "project"}
	if got := RemoteMetaDirPath("/opt/mindfs-data", project); got != "/srv/a/.mindfs" {
		t.Fatalf("project meta dir = %q, want /srv/a/.mindfs", got)
	}
	home := RemoteRootInfo{ID: "home-x", RootPath: "~", MetaLocation: "home"}
	if got := RemoteMetaDirPath("/opt/mindfs-data", home); got != "/opt/mindfs-data/.mindfs/home-x" {
		t.Fatalf("home meta dir = %q, want /opt/mindfs-data/.mindfs/home-x", got)
	}
}

func TestMatchRootByPath(t *testing.T) {
	roots := []RemoteRootInfo{
		{ID: "proj-a", RootPath: "/srv/a"},
		{ID: "proj-b", RootPath: "/srv/b"},
	}
	root, ok := MatchRootByPath(roots, "/srv/b/")
	if !ok || root.ID != "proj-b" {
		t.Fatalf("MatchRootByPath = %+v %v", root, ok)
	}
	if _, ok := MatchRootByPath(roots, "/srv/missing"); ok {
		t.Fatalf("missing path should not match")
	}
}

func TestFetchRemoteMetaEmptyBundle(t *testing.T) {
	fetch := func(ctx context.Context, cmd string) ([]byte, error) {
		return []byte(noSessionsMarker + "\n"), nil
	}
	bundle, err := FetchRemoteMeta(context.Background(), fetch, "/srv/a/.mindfs")
	if err != nil {
		t.Fatalf("FetchRemoteMeta: %v", err)
	}
	defer bundle.Cleanup()
	if !bundle.Empty {
		t.Fatalf("bundle.Empty = false, want true")
	}
}

func TestFetchRemoteMetaExtractsSessions(t *testing.T) {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	files := map[string][]byte{
		"sessions/session-list.db":          []byte("fake-sqlite-bytes"),
		"sessions/1760000000-aaa.jsonl":     []byte("{\"seq\":1,\"role\":\"user\",\"content\":\"hi\"}\n"),
		"sessions/1760000000-aaa.aux.jsonl": []byte("{\"seq\":1,\"line\":1}\n"),
	}
	for name, payload := range files {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(payload))}); err != nil {
			t.Fatalf("tar header: %v", err)
		}
		if _, err := tw.Write(payload); err != nil {
			t.Fatalf("tar write: %v", err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("tar close: %v", err)
	}
	if err := gz.Close(); err != nil {
		t.Fatalf("gzip close: %v", err)
	}

	fetch := func(ctx context.Context, cmd string) ([]byte, error) {
		if !strings.Contains(cmd, "tar czf -") {
			t.Fatalf("unexpected remote command: %s", cmd)
		}
		return buf.Bytes(), nil
	}
	bundle, err := FetchRemoteMeta(context.Background(), fetch, "/srv/a/.mindfs")
	if err != nil {
		t.Fatalf("FetchRemoteMeta: %v", err)
	}
	for name, payload := range files {
		got, err := os.ReadFile(filepath.Join(bundle.Dir, filepath.FromSlash(name)))
		if err != nil {
			t.Fatalf("extracted file %s: %v", name, err)
		}
		if !bytes.Equal(got, payload) {
			t.Fatalf("extracted %s content mismatch", name)
		}
	}
	bundle.Cleanup()
	if _, err := os.Stat(bundle.Dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("cleanup should remove bundle dir, stat err = %v", err)
	}
}

func TestFetchRemoteMetaRejectsTarTraversal(t *testing.T) {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	payload := []byte("evil")
	if err := tw.WriteHeader(&tar.Header{Name: "../evil.txt", Mode: 0o644, Size: int64(len(payload))}); err != nil {
		t.Fatalf("tar header: %v", err)
	}
	if _, err := tw.Write(payload); err != nil {
		t.Fatalf("tar write: %v", err)
	}
	tw.Close()
	gz.Close()

	fetch := func(ctx context.Context, cmd string) ([]byte, error) { return buf.Bytes(), nil }
	bundle, err := FetchRemoteMeta(context.Background(), fetch, "/srv/a/.mindfs")
	if err == nil {
		bundle.Cleanup()
		t.Fatalf("FetchRemoteMeta should reject path traversal")
	}
}

func TestFetchRemoteSessionLog(t *testing.T) {
	fetch := func(ctx context.Context, cmd string) ([]byte, error) {
		if !strings.Contains(cmd, "sessions/1760000000-aaa.jsonl") {
			t.Fatalf("unexpected remote command: %s", cmd)
		}
		return []byte("{\"seq\":1}\n"), nil
	}
	payload, err := FetchRemoteSessionLog(context.Background(), fetch, "/srv/a/.mindfs", "1760000000-aaa")
	if err != nil {
		t.Fatalf("FetchRemoteSessionLog: %v", err)
	}
	if string(payload) != "{\"seq\":1}\n" {
		t.Fatalf("payload = %q", payload)
	}
	if _, err := FetchRemoteSessionLog(context.Background(), fetch, "/srv/a/.mindfs", "../evil"); err == nil {
		t.Fatalf("invalid key should be rejected")
	}
}

func TestDiscoverRemoteRootsRequiresHome(t *testing.T) {
	if _, err := DiscoverRemoteRoots(context.Background(), func(ctx context.Context, cmd string) ([]byte, error) {
		return nil, nil
	}, ""); err == nil {
		t.Fatalf("empty mindfsHome should fail")
	}
}

// TestSSHFetcherRunsRemoteCommand verifies the production fetcher adapts
// remote.Run (it fails to connect to a nonexistent host, which proves the
// command reaches the SSH layer).
func TestSSHFetcherRunsRemoteCommand(t *testing.T) {
	fetch := SSHFetcher(Device{ID: "x", Host: "127.0.0.1", Port: 1, User: "nobody"}, t.TempDir(), time.Second)
	if _, err := fetch(context.Background(), "echo hi"); err == nil {
		t.Fatalf("expected SSH error for unreachable host")
	}
}

func TestUpsertDeviceAgentsPreservesUnknownFields(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agents.json")
	existing := `{
  "relayBaseURL": "https://relay.example",
  "agents": [
    {
      "name": "qoder",
      "command": "qodercli",
      "protocol": "acp",
      "installCommands": ["npm i -g qoder"],
      "customField": {"nested": true}
    },
    {"name": "codex", "command": "codex", "protocol": "codex-sdk"}
  ]
}`
	if err := os.WriteFile(path, []byte(existing), 0o644); err != nil {
		t.Fatal(err)
	}
	created, skipped, err := UpsertDeviceAgents(path, []AgentEntry{
		{Name: "jp-codex", Command: "codex", Device: "jp", Brief: "JP · codex", Protocol: "codex-sdk"},
		{Name: "jp-qodercli", Command: "qodercli", Device: "jp", Brief: "JP · qodercli", Protocol: "acp"},
		{Name: "codex", Command: "codex", Device: "jp", Brief: "collides with existing"},
	})
	if err != nil {
		t.Fatalf("UpsertDeviceAgents: %v", err)
	}
	if len(created) != 2 || created[0] != "jp-codex" || created[1] != "jp-qodercli" {
		t.Fatalf("created = %v", created)
	}
	if len(skipped) != 1 || skipped[0] != "codex" {
		t.Fatalf("skipped = %v", skipped)
	}

	// Unknown fields of pre-existing entries survive byte-for-byte semantics.
	var doc struct {
		RelayBaseURL string           `json:"relayBaseURL"`
		Agents       []map[string]any `json:"agents"`
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("rewritten file is not valid JSON: %v", err)
	}
	if doc.RelayBaseURL != "https://relay.example" {
		t.Fatalf("top-level unknown field lost: %q", doc.RelayBaseURL)
	}
	byName := map[string]map[string]any{}
	for _, agent := range doc.Agents {
		name, _ := agent["name"].(string)
		byName[name] = agent
	}
	qoder := byName["qoder"]
	if qoder == nil {
		t.Fatalf("existing agent qoder lost")
	}
	if _, ok := qoder["customField"]; !ok {
		t.Fatalf("customField lost on existing entry")
	}
	install, ok := qoder["installCommands"].([]any)
	if !ok || len(install) != 1 || install[0] != "npm i -g qoder" {
		t.Fatalf("installCommands lost: %#v", qoder["installCommands"])
	}
	newEntry := byName["jp-codex"]
	if newEntry == nil || newEntry["device"] != "jp" || newEntry["command"] != "codex" || newEntry["brief"] != "JP · codex" {
		t.Fatalf("new device entry wrong: %#v", newEntry)
	}

	// Re-running is a no-op: the entries now exist.
	created2, skipped2, err := UpsertDeviceAgents(path, []AgentEntry{
		{Name: "jp-codex", Command: "codex", Device: "jp", Brief: "JP · codex"},
	})
	if err != nil || len(created2) != 0 || len(skipped2) != 1 {
		t.Fatalf("second run: created=%v skipped=%v err=%v", created2, skipped2, err)
	}
}

func TestUpsertDeviceAgentsCreatesMissingFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "agents.json")
	created, _, err := UpsertDeviceAgents(path, []AgentEntry{{Name: "jp-opencode", Command: "opencode", Device: "jp", Protocol: "acp"}})
	if err != nil {
		t.Fatalf("UpsertDeviceAgents: %v", err)
	}
	if len(created) != 1 {
		t.Fatalf("created = %v", created)
	}
	payload, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(payload, &doc); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if _, ok := doc["agents"]; !ok {
		t.Fatalf("agents key missing")
	}
}

func TestRemoveDeviceAgents(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agents.json")
	initial := `{"agents":[{"name":"codex","command":"codex"},{"name":"jp-codex","command":"codex","device":"jp"},{"name":"jp-opencode","command":"opencode","device":"jp"},{"name":"sh-codex","command":"codex","device":"sh"}]}`
	if err := os.WriteFile(path, []byte(initial), 0o644); err != nil {
		t.Fatal(err)
	}
	removed, err := RemoveDeviceAgents(path, "jp")
	if err != nil {
		t.Fatalf("RemoveDeviceAgents: %v", err)
	}
	if len(removed) != 2 || removed[0] != "jp-codex" || removed[1] != "jp-opencode" {
		t.Fatalf("removed = %v", removed)
	}
	var doc struct {
		Agents []struct {
			Name   string `json:"name"`
			Device string `json:"device"`
		} `json:"agents"`
	}
	payload, _ := os.ReadFile(path)
	if err := json.Unmarshal(payload, &doc); err != nil {
		t.Fatal(err)
	}
	if len(doc.Agents) != 2 {
		t.Fatalf("agents = %+v", doc.Agents)
	}
	for _, agent := range doc.Agents {
		if agent.Device == "jp" {
			t.Fatalf("device agent survived: %+v", agent)
		}
	}
	// Entries without a device marker must never be touched.
	removed2, err := RemoveDeviceAgents(path, "sh")
	if err != nil || len(removed2) != 1 || removed2[0] != "sh-codex" {
		t.Fatalf("second removal = %v, %v", removed2, err)
	}
	payload, _ = os.ReadFile(path)
	if !strings.Contains(string(payload), `"name": "codex"`) && !strings.Contains(string(payload), `"name":"codex"`) {
		t.Fatalf("marker-less local agent was removed: %s", payload)
	}
}
