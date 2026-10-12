package remote

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// AgentEntry is the device-provisioning view of one entry in the user-layer
// agents.json file. Its JSON field names MUST stay in sync with
// server/internal/agent.Definition (that package imports this package, so it
// cannot be referenced here without an import cycle).
//
// Only the subset the device provisioning needs to control is modelled as
// typed fields; every other field of an existing entry survives untouched
// because entries are rewritten as raw JSON (see UpsertDeviceAgents).
type AgentEntry struct {
	Name        string            `json:"name"`
	Brief       string            `json:"brief,omitempty"`
	Device      string            `json:"device,omitempty"`
	Command     string            `json:"command"`
	Protocol    string            `json:"protocol,omitempty"`
	Args        []string          `json:"args,omitempty"`
	Env         map[string]string `json:"env,omitempty"`
	CwdTemplate string            `json:"cwdTemplate,omitempty"`
}

// agentsJSONMu serializes mutations of the user agents.json within this
// process. Cross-process safety follows the agent package's existing
// write model (plain os.WriteFile, no file lock), so a process-wide mutex is
// the strongest guarantee we can add without touching frozen code.
var agentsJSONMu sync.Mutex

// UpsertDeviceAgents merges the given device agent entries into the agents
// JSON file at path (the same user-layer file agent.ResolveConfigPath()
// returns). Semantics:
//
//   - The document is parsed as raw JSON; unknown top-level keys and unknown
//     per-entry fields (installCommands, configBackup, ...) are preserved
//     byte-for-byte. This file is never rewritten from a typed struct, so a
//     provision run can never drop fields this package does not know about.
//   - Entries whose "name" already exists are left untouched (skip).
//   - Returns the names of newly created entries and the names that were
//     skipped because they already existed.
func UpsertDeviceAgents(path string, entries []AgentEntry) (created []string, skipped []string, err error) {
	if len(entries) == 0 {
		return nil, nil, nil
	}
	agentsJSONMu.Lock()
	defer agentsJSONMu.Unlock()

	doc, err := readAgentsDocument(path)
	if err != nil {
		return nil, nil, err
	}

	byName := make(map[string]int, len(doc.Agents))
	for i, entry := range doc.Agents {
		if name := rawEntryName(entry); name != "" {
			byName[name] = i
		}
	}
	for _, entry := range entries {
		if strings.TrimSpace(entry.Name) == "" || strings.TrimSpace(entry.Command) == "" {
			return nil, nil, fmt.Errorf("device agent entry requires name and command: %+v", entry)
		}
		if _, exists := byName[entry.Name]; exists {
			skipped = append(skipped, entry.Name)
			continue
		}
		payload, err := json.Marshal(entry)
		if err != nil {
			return nil, nil, err
		}
		doc.Agents = append(doc.Agents, nil)
		idx := len(doc.Agents) - 1
		if err := json.Unmarshal(payload, &doc.Agents[idx]); err != nil {
			return nil, nil, err
		}
		byName[entry.Name] = idx
		created = append(created, entry.Name)
	}
	if len(created) == 0 {
		return created, skipped, nil
	}
	if err := writeAgentsDocument(path, doc); err != nil {
		return nil, nil, err
	}
	return created, skipped, nil
}

// RemoveDeviceAgents deletes every agents.json entry whose "device" field
// equals deviceID and returns the removed agent names. Entries without a
// device marker are never touched.
func RemoveDeviceAgents(path, deviceID string) (removed []string, err error) {
	deviceID = strings.TrimSpace(deviceID)
	if deviceID == "" {
		return nil, fmt.Errorf("device id required")
	}
	agentsJSONMu.Lock()
	defer agentsJSONMu.Unlock()

	doc, err := readAgentsDocument(path)
	if err != nil {
		return nil, err
	}
	kept := make([]map[string]json.RawMessage, 0, len(doc.Agents))
	changed := false
	for _, entry := range doc.Agents {
		entryDevice := strings.TrimSpace(rawEntryString(entry, "device"))
		if entryDevice == deviceID {
			removed = append(removed, rawEntryName(entry))
			changed = true
			continue
		}
		kept = append(kept, entry)
	}
	if !changed {
		return nil, nil
	}
	doc.Agents = kept
	if err := writeAgentsDocument(path, doc); err != nil {
		return nil, err
	}
	return removed, nil
}

// agentsDocument keeps the raw JSON of every top-level key so unrelated
// fields survive a rewrite.
type agentsDocument struct {
	// Agents holds the raw JSON of each entry under "agents".
	Agents []map[string]json.RawMessage
	// Extra holds every other top-level key verbatim.
	Extra map[string]json.RawMessage
}

func readAgentsDocument(path string) (*agentsDocument, error) {
	payload, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return &agentsDocument{Extra: map[string]json.RawMessage{}}, nil
		}
		return nil, fmt.Errorf("read agents config: %w", err)
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal(payload, &top); err != nil {
		return nil, fmt.Errorf("parse agents config %s: %w", path, err)
	}
	doc := &agentsDocument{Extra: make(map[string]json.RawMessage, len(top))}
	for key, raw := range top {
		if key == "agents" {
			continue
		}
		doc.Extra[key] = raw
	}
	var agents []map[string]json.RawMessage
	if raw, ok := top["agents"]; ok && len(raw) > 0 {
		if err := json.Unmarshal(raw, &agents); err != nil {
			return nil, fmt.Errorf("parse agents config %s: agents: %w", path, err)
		}
	}
	doc.Agents = agents
	return doc, nil
}

func writeAgentsDocument(path string, doc *agentsDocument) error {
	top := make(map[string]json.RawMessage, len(doc.Extra)+1)
	for key, raw := range doc.Extra {
		top[key] = raw
	}
	if doc.Agents == nil {
		doc.Agents = []map[string]json.RawMessage{}
	}
	agents, err := json.Marshal(doc.Agents)
	if err != nil {
		return err
	}
	top["agents"] = agents
	payload, err := json.MarshalIndent(top, "", "  ")
	if err != nil {
		return err
	}
	payload = append(payload, '\n')
	if dir := filepath.Dir(path); dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	// Atomic tmp+rename so a crash mid-write cannot truncate agents.json.
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, payload, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func rawEntryName(entry map[string]json.RawMessage) string {
	return rawEntryString(entry, "name")
}

func rawEntryString(entry map[string]json.RawMessage, field string) string {
	if entry == nil {
		return ""
	}
	raw, ok := entry[field]
	if !ok {
		return ""
	}
	var value string
	if err := json.Unmarshal(raw, &value); err != nil {
		return ""
	}
	return strings.TrimSpace(value)
}
