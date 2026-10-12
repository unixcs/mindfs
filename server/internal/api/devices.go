package api

// 多设备 CLI 聚合: HTTP handlers for device management.
//
// All device endpoints are registered in Routes() with protectedEndpoint and
// share one guard: when the devices store is not wired (s.Devices == nil, e.g.
// the server runs without device support) every endpoint answers 501
// 设备功能未启用 instead of silently 404ing.

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"path"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"mindfs/internal/remote"
	"mindfs/server/internal/agent"
	"mindfs/server/internal/fs"
	"mindfs/server/internal/session"
)

const (
	deviceTestTimeout   = 15 * time.Second
	deviceScanTimeout   = 30 * time.Second
	deviceSyncTimeout   = 60 * time.Second
	deviceProbeTimeout  = 12 * time.Second
	deviceFetchTimeout  = 45 * time.Second
	deviceBodyLimit     = 1 << 20
	deviceNotFoundLabel = "device not found"
)

// requireDevices answers 501 when device support is not wired up.
func (h *HTTPHandler) requireDevices(w http.ResponseWriter) (*remote.Store, bool) {
	if h.AppContext == nil || h.AppContext.Devices == nil {
		respondError(w, http.StatusNotImplemented, errServiceUnavailable("设备功能未启用"))
		return nil, false
	}
	return h.AppContext.Devices, true
}

// deviceJSON renders a device as a JSON object (preserving remote.Device's
// exact field names) plus computed fields like root_count.
func deviceJSON(device remote.Device, extra map[string]any) map[string]any {
	payload, err := json.Marshal(device)
	if err != nil {
		return map[string]any{"id": device.ID, "host": device.Host, "user": device.User}
	}
	item := map[string]any{}
	if err := json.Unmarshal(payload, &item); err != nil {
		return map[string]any{"id": device.ID, "host": device.Host, "user": device.User}
	}
	for key, value := range extra {
		item[key] = value
	}
	return item
}

func (h *HTTPHandler) deviceRootCount(deviceID string) int {
	registry := h.AppContext.GetDirRegistry()
	if registry == nil {
		return 0
	}
	return len(registry.ListByDevice(deviceID))
}

func (h *HTTPHandler) handleDevicesList(w http.ResponseWriter, r *http.Request) {
	store, ok := h.requireDevices(w)
	if !ok {
		return
	}
	devices := store.List()
	items := make([]map[string]any, 0, len(devices))
	for _, device := range devices {
		items = append(items, deviceJSON(device, map[string]any{"root_count": h.deviceRootCount(device.ID)}))
	}
	respondJSON(w, http.StatusOK, map[string]any{"devices": items})
}

func (h *HTTPHandler) handleDeviceCreate(w http.ResponseWriter, r *http.Request) {
	store, ok := h.requireDevices(w)
	if !ok {
		return
	}
	var device remote.Device
	if err := json.NewDecoder(io.LimitReader(r.Body, deviceBodyLimit)).Decode(&device); err != nil {
		respondError(w, http.StatusBadRequest, errInvalidRequest("invalid device payload: "+err.Error()))
		return
	}
	if err := store.Save(device); err != nil {
		respondError(w, http.StatusBadRequest, errInvalidRequest(err.Error()))
		return
	}
	saved, _ := store.Get(device.ID)
	respondJSON(w, http.StatusOK, deviceJSON(saved, map[string]any{"root_count": h.deviceRootCount(saved.ID)}))
}

func (h *HTTPHandler) handleDeviceDelete(w http.ResponseWriter, r *http.Request) {
	store, ok := h.requireDevices(w)
	if !ok {
		return
	}
	id := strings.TrimSpace(chi.URLParam(r, "id"))
	if _, found := store.Get(id); !found {
		respondError(w, http.StatusNotFound, errInvalidRequest(deviceNotFoundLabel))
		return
	}

	// Release cached per-root resources (session managers, watchers) before
	// the roots are unregistered.
	removedRoots := 0
	if registry := h.AppContext.GetDirRegistry(); registry != nil {
		for _, root := range registry.ListByDevice(id) {
			h.AppContext.ReleaseRootResources(root.ID)
			removedRoots++
		}
		if err := registry.RemoveByDevice(id); err != nil {
			log.Printf("[devices] delete %s: remove roots: %v", id, err)
		}
	}

	// Remove the device's provisioned agents from the user agents.json and
	// from the live pool config.
	removedAgents := 0
	if cfgPath, err := agent.ResolveConfigPath(); err == nil {
		if removed, err := remote.RemoveDeviceAgents(cfgPath, id); err != nil {
			log.Printf("[devices] delete %s: remove agents.json entries: %v", id, err)
		} else {
			removedAgents = len(removed)
		}
	} else {
		log.Printf("[devices] delete %s: resolve agents config: %v", id, err)
	}
	h.removeDeviceAgentsFromPool(r.Context(), id)

	if err := store.Delete(id); err != nil {
		respondError(w, http.StatusInternalServerError, err)
		return
	}
	respondJSON(w, http.StatusOK, map[string]any{
		"removed_agents": removedAgents,
		"removed_roots":  removedRoots,
	})
}

// removeDeviceAgentsFromPool drops every agent whose Device marker matches
// deviceID from the live pool (agents.json was already rewritten). The
// removal is atomic inside the pool so a concurrent hosted-config refresh
// cannot resurrect stale entries.
func (h *HTTPHandler) removeDeviceAgentsFromPool(ctx context.Context, deviceID string) {
	pool := h.AppContext.GetAgentPool()
	if pool == nil {
		return
	}
	effective, changed := pool.RemoveDeviceAgents(deviceID)
	if !changed {
		return
	}
	if prober := h.AppContext.GetProber(); prober != nil {
		prober.UpdateConfig(ctx, &effective)
	}
}

func (h *HTTPHandler) handleDeviceTest(w http.ResponseWriter, r *http.Request) {
	store, ok := h.requireDevices(w)
	if !ok {
		return
	}
	device, found := store.Get(strings.TrimSpace(chi.URLParam(r, "id")))
	if !found {
		respondError(w, http.StatusNotFound, errInvalidRequest(deviceNotFoundLabel))
		return
	}
	runtime, ok := h.AppContext.GetDeviceRuntime()
	if !ok {
		respondError(w, http.StatusNotImplemented, errServiceUnavailable("设备功能未启用"))
		return
	}
	muxDir, err := remote.MuxDir(runtime.ConfigDir)
	if err != nil {
		respondError(w, http.StatusInternalServerError, err)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), deviceTestTimeout)
	defer cancel()
	probe := remote.ProbeDevice(ctx, device, muxDir, nil)

	response := map[string]any{
		"ok":         probe.Ok,
		"home":       probe.Home,
		"latency_ms": probe.LatencyMS,
	}
	if probe.Hostname != "" {
		response["hostname"] = probe.Hostname
	}
	// Missing CLIs render as JSON null so the UI can distinguish "probed and
	// absent" from "present".
	clis := map[string]any{}
	for _, cli := range remote.ProbeCLIs {
		if found := probe.CLIs[cli]; found != "" {
			clis[cli] = found
		} else {
			clis[cli] = nil
		}
	}
	response["clis"] = clis
	if len(probe.Errors) > 0 {
		response["errors"] = probe.Errors
	}

	provisioned := []string{}
	if probe.Ok && device.AutoCreateAgents {
		created, err := h.provisionDeviceAgents(ctx, device, probe)
		if err != nil {
			response["provision_error"] = err.Error()
		} else {
			provisioned = created
		}
	}
	response["provisioned"] = provisioned
	respondJSON(w, http.StatusOK, response)
}

// provisionDeviceAgents writes <deviceID>-<cli> entries into the user
// agents.json (preserving every unknown field — see remote.UpsertDeviceAgents)
// and applies them to the live pool so they are usable immediately.
func (h *HTTPHandler) provisionDeviceAgents(ctx context.Context, device remote.Device, probe *remote.ProbeResult) ([]string, error) {
	cfgPath, err := agent.ResolveConfigPath()
	if err != nil {
		return nil, err
	}
	localCfg, err := agent.LoadConfig("")
	if err != nil {
		return nil, err
	}
	entries := make([]remote.AgentEntry, 0, len(remote.ProbeCLIs))
	defs := make([]agent.Definition, 0, len(remote.ProbeCLIs))
	for _, cli := range remote.ProbeCLIs {
		if probe.CLIs[cli] == "" {
			continue
		}
		entry := remote.AgentEntry{
			Name:    device.ID + "-" + cli,
			Command: cli,
			Device:  device.ID,
			Brief:   device.DisplayName() + " · " + cli,
		}
		// The local definition of the same CLI is the most accurate protocol/
		// args/env template; fall back to the CLI's canonical protocol.
		if base, found := lookupLocalAgentBase(localCfg, cli); found {
			entry.Protocol = string(base.Protocol)
			entry.Args = base.Args
			entry.Env = base.Env
			entry.CwdTemplate = base.CwdTemplate
		} else {
			// No local template (e.g. binary installed without the packaged
			// share/mindfs/agents.json): fall back to canonical protocol and
			// args. Missing ACP args would spawn the CLI in interactive mode
			// on the device, which never speaks ACP and hangs the probe.
			entry.Protocol = defaultDeviceProtocol(cli)
			entry.Args = defaultDeviceArgs(cli, entry.Protocol)
		}
		entries = append(entries, entry)
		defs = append(defs, agent.Definition{
			Name:        entry.Name,
			Brief:       entry.Brief,
			Device:      entry.Device,
			Command:     entry.Command,
			Protocol:    agent.Protocol(entry.Protocol),
			Args:        entry.Args,
			Env:         entry.Env,
			CwdTemplate: entry.CwdTemplate,
		})
	}
	created, _, err := remote.UpsertDeviceAgents(cfgPath, entries)
	if err != nil {
		return nil, err
	}
	pool := h.AppContext.GetAgentPool()
	if pool != nil && len(defs) > 0 {
		effective, changed := pool.UpsertDeviceAgents(defs)
		if changed {
			if prober := h.AppContext.GetProber(); prober != nil {
				prober.UpdateConfig(ctx, &effective)
			}
		}
	}
	return created, nil
}

// lookupLocalAgentBase finds the local (non-device) definition for a CLI,
// matching by Command first (the qoder agent's command is "qodercli"), then
// by name.
func lookupLocalAgentBase(cfg agent.Config, cli string) (agent.Definition, bool) {
	for _, def := range cfg.Agents {
		if def.Device == "" && def.Command == cli {
			return def, true
		}
	}
	for _, def := range cfg.Agents {
		if def.Device == "" && def.Name == cli {
			return def, true
		}
	}
	return agent.Definition{}, false
}

// defaultDeviceProtocol maps a CLI to its canonical MindFS protocol when no
// local definition exists to copy from.
func defaultDeviceProtocol(cli string) string {
	switch cli {
	case "codex":
		return "codex-sdk"
	case "claude":
		return "claude-sdk"
	default:
		return "acp"
	}
}

// defaultDeviceArgs returns the canonical spawn arguments for a CLI when no
// local definition exists to copy from. Mirrors the args in the packaged
// agents.json (agents.json at the repo root).
func defaultDeviceArgs(cli, protocol string) []string {
	if protocol != "acp" {
		return nil
	}
	switch cli {
	case "opencode", "kimi", "kiro-cli", "openclaw", "omp", "hermes", "reasonix":
		return []string{"acp"}
	case "qodercli", "codebuddy", "qwen", "auggie", "cline", "copilot":
		return []string{"--acp"}
	default:
		return nil
	}
}

func (h *HTTPHandler) handleDeviceScan(w http.ResponseWriter, r *http.Request) {
	store, ok := h.requireDevices(w)
	if !ok {
		return
	}
	device, found := store.Get(strings.TrimSpace(chi.URLParam(r, "id")))
	if !found {
		respondError(w, http.StatusNotFound, errInvalidRequest(deviceNotFoundLabel))
		return
	}
	runtime, ok := h.AppContext.GetDeviceRuntime()
	if !ok {
		respondError(w, http.StatusNotImplemented, errServiceUnavailable("设备功能未启用"))
		return
	}
	muxDir, err := remote.MuxDir(runtime.ConfigDir)
	if err != nil {
		respondError(w, http.StatusInternalServerError, err)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), deviceScanTimeout)
	defer cancel()
	roots, err := remote.ScanDeviceRoots(ctx, device, muxDir, 20*time.Second)
	if err != nil {
		respondError(w, http.StatusBadGateway, err)
		return
	}
	if roots == nil {
		roots = []remote.ScanRoot{}
	}
	respondJSON(w, http.StatusOK, map[string]any{"roots": roots})
}

// handleDeviceRootCreate registers a remote directory as a device root.
// Body: {"path": "<absolute remote path>"}. The directory is verified over
// SSH before registration so the panel can surface a precise error instead
// of failing later on first browse.
func (h *HTTPHandler) handleDeviceRootCreate(w http.ResponseWriter, r *http.Request) {
	store, ok := h.requireDevices(w)
	if !ok {
		return
	}
	device, found := store.Get(strings.TrimSpace(chi.URLParam(r, "id")))
	if !found {
		respondError(w, http.StatusNotFound, errInvalidRequest(deviceNotFoundLabel))
		return
	}
	runtime, ok := h.AppContext.GetDeviceRuntime()
	if !ok {
		respondError(w, http.StatusNotImplemented, errServiceUnavailable("设备功能未启用"))
		return
	}
	var body struct {
		Path string `json:"path"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, deviceBodyLimit)).Decode(&body); err != nil {
		respondError(w, http.StatusBadRequest, errInvalidRequest("invalid payload: "+err.Error()))
		return
	}
	remotePath := strings.TrimSpace(body.Path)
	if remotePath == "" {
		respondError(w, http.StatusBadRequest, errInvalidRequest("path 必填"))
		return
	}
	cleaned := path.Clean(remotePath)
	if !strings.HasPrefix(cleaned, "/") || cleaned == "/" {
		respondError(w, http.StatusBadRequest, errInvalidRequest("path 必须是设备上的绝对目录"))
		return
	}
	if fs.RemoteStatRoot(r.Context(), device, runtime.ConfigDir, cleaned) != nil {
		respondError(w, http.StatusBadGateway, errInvalidRequest("设备上不存在该目录: "+cleaned))
		return
	}
	registry := h.AppContext.GetDirRegistry()
	if registry == nil {
		respondError(w, http.StatusInternalServerError, errServiceUnavailable("目录注册表未就绪"))
		return
	}
	root, err := registry.UpsertDeviceRoot(device, cleaned)
	if err != nil {
		respondError(w, http.StatusBadRequest, errInvalidRequest(err.Error()))
		return
	}
	respondJSON(w, http.StatusOK, map[string]any{"root": root, "root_count": h.deviceRootCount(device.ID)})
}

func (h *HTTPHandler) handleDeviceSync(w http.ResponseWriter, r *http.Request) {
	store, ok := h.requireDevices(w)
	if !ok {
		return
	}
	device, found := store.Get(strings.TrimSpace(chi.URLParam(r, "id")))
	if !found {
		respondError(w, http.StatusNotFound, errInvalidRequest(deviceNotFoundLabel))
		return
	}
	if strings.TrimSpace(device.MindFSHome) == "" {
		respondError(w, http.StatusBadRequest, errInvalidRequest("设备未配置 mindfsHome，无法镜像会话"))
		return
	}
	var body struct {
		RootID string `json:"root_id"`
	}
	if r.Body != nil {
		if err := json.NewDecoder(io.LimitReader(r.Body, deviceBodyLimit)).Decode(&body); err != nil && err != io.EOF {
			respondError(w, http.StatusBadRequest, errInvalidRequest("invalid sync payload: "+err.Error()))
			return
		}
	}

	var deviceRoots []fs.RootInfo
	if registry := h.AppContext.GetDirRegistry(); registry != nil {
		deviceRoots = registry.ListByDevice(device.ID)
	}
	if wanted := strings.TrimSpace(body.RootID); wanted != "" {
		filtered := make([]fs.RootInfo, 0, len(deviceRoots))
		matched := false
		for _, root := range deviceRoots {
			if root.ID == wanted {
				filtered = append(filtered, root)
				matched = true
			}
		}
		if !matched {
			respondError(w, http.StatusBadRequest, errInvalidRequest("root 不属于该设备: "+wanted))
			return
		}
		deviceRoots = filtered
	}

	result := map[string]any{
		"imported": 0,
		"updated":  0,
		"skipped":  0,
		"errors":   []string{},
	}
	if len(deviceRoots) == 0 {
		errors := result["errors"].([]string)
		result["errors"] = append(errors, "该设备没有已注册的设备根；请先在文件树中添加设备目录后重试")
		respondJSON(w, http.StatusOK, result)
		return
	}

	runtime, ok := h.AppContext.GetDeviceRuntime()
	if !ok {
		respondError(w, http.StatusNotImplemented, errServiceUnavailable("设备功能未启用"))
		return
	}
	muxDir, err := remote.MuxDir(runtime.ConfigDir)
	if err != nil {
		respondError(w, http.StatusInternalServerError, err)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), deviceSyncTimeout)
	defer cancel()
	fetch := remote.SSHFetcher(device, muxDir, deviceFetchTimeout)
	remoteRoots, err := remote.DiscoverRemoteRoots(ctx, fetch, device.MindFSHome)
	if err != nil {
		respondError(w, http.StatusBadGateway, err)
		return
	}

	mapAgent := h.deviceAgentMapper(device)
	var imported, updated, skipped int
	var errorsList []string
	var skippedAgents []string
	for _, root := range deviceRoots {
		remoteRoot, matched := remote.MatchRootByPath(remoteRoots, root.RootPath)
		if !matched {
			skipped++
			errorsList = append(errorsList, "root "+root.ID+": 远程 registry 未找到 RootPath="+root.RootPath+" 的对应项目")
			continue
		}
		metaDir := remote.RemoteMetaDirPath(device.MindFSHome, remoteRoot)
		bundle, err := remote.FetchRemoteMeta(ctx, fetch, metaDir)
		if err != nil {
			errorsList = append(errorsList, "root "+root.ID+": "+err.Error())
			continue
		}
		manager, err := h.AppContext.GetSessionManager(root.ID)
		if err != nil {
			bundle.Cleanup()
			errorsList = append(errorsList, "root "+root.ID+": "+err.Error())
			continue
		}
		res, err := manager.ImportMirrorBundle(ctx, session.MirrorImportInput{
			DeviceID:          device.ID,
			SourceDir:         bundle.Dir,
			RemoteSessionsDir: metaDir + "/sessions",
			MapAgent:          mapAgent,
		})
		bundle.Cleanup()
		if err != nil {
			errorsList = append(errorsList, "root "+root.ID+": "+err.Error())
			continue
		}
		imported += res.Imported
		updated += res.Updated
		skipped += res.Skipped
		for _, agentName := range res.SkippedAgents {
			if !containsString(skippedAgents, agentName) {
				skippedAgents = append(skippedAgents, agentName)
			}
		}
		for _, message := range res.Errors {
			errorsList = append(errorsList, "root "+root.ID+": "+message)
		}
	}
	if errorsList == nil {
		errorsList = []string{}
	}
	respondJSON(w, http.StatusOK, map[string]any{
		"imported":       imported,
		"updated":        updated,
		"skipped":        skipped,
		"errors":         errorsList,
		"skipped_agents": skippedAgents,
	})
}

// deviceAgentMapper builds the remote→local binding agent rewrite used during
// mirror import: "codex" maps to the provisioned "jp-codex" when it exists in
// the pool config; as a fallback the remote agent's own Command is tried (the
// qoder agent's command is "qodercli"). Unmapped names keep their original
// value and are reported via skipped_agents.
func (h *HTTPHandler) deviceAgentMapper(device remote.Device) func(agent string) (string, bool) {
	pool := h.AppContext.GetAgentPool()
	var poolCfg agent.Config
	if pool != nil {
		poolCfg = pool.Config()
	}
	return func(agentName string) (string, bool) {
		agentName = strings.TrimSpace(agentName)
		if agentName == "" {
			return "", false
		}
		candidate := device.ID + "-" + agentName
		if _, exists := poolCfg.GetAgent(candidate); exists {
			return candidate, true
		}
		if base, exists := poolCfg.GetAgent(agentName); exists {
			alternative := device.ID + "-" + strings.TrimSpace(base.Command)
			if alternative != candidate && strings.TrimSpace(base.Command) != "" {
				if _, exists := poolCfg.GetAgent(alternative); exists {
					return alternative, true
				}
			}
		}
		return "", false
	}
}
