package agent

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"mindfs/internal/remote"
	"mindfs/server/internal/agent/acp"
	"mindfs/server/internal/agent/claude"
	"mindfs/server/internal/agent/codex"
	agenttypes "mindfs/server/internal/agent/types"
)

// DeviceResolver looks up a remote device config by ID.
type DeviceResolver func(id string) (remote.Device, bool)

// Pool routes agent session creation to protocol-specific runtimes.
type Pool struct {
	cfg             Config
	processCtx      context.Context
	cancel          context.CancelFunc
	mu              sync.Mutex
	sessions        map[string]*sessionEntry
	runtimeEnv      map[string]map[string]string
	closed          bool
	acp             *acp.Runtime
	claude          *claude.Runtime
	codex           *codex.Runtime
	runtimeLocksMu  sync.Mutex
	runtimeLocks    map[runtimeGroup]*sync.Mutex
	memoryMu        sync.RWMutex
	memoryCollectMu sync.Mutex
	memorySnapshot  MemorySnapshot
	deviceMu        sync.RWMutex
	deviceResolver  DeviceResolver
	deviceConfigDir string
}

type sessionEntry struct {
	agentName  string
	sessionKey string
	protocol   Protocol
	session    agenttypes.Session
	lastUsedAt time.Time
	activeUses int
	closing    bool
	closeDone  chan struct{}
}

// NewPool creates a new agent pool.
func NewPool(cfg Config) *Pool {
	processCtx, cancel := context.WithCancel(context.Background())
	return &Pool{
		cfg:          cfg,
		processCtx:   processCtx,
		cancel:       cancel,
		sessions:     make(map[string]*sessionEntry),
		runtimeEnv:   make(map[string]map[string]string),
		acp:          acp.NewRuntime(processCtx),
		claude:       claude.NewRuntime(),
		codex:        codex.NewRuntime(),
		runtimeLocks: make(map[runtimeGroup]*sync.Mutex),
	}
}

// SupportsDeveloperInstructions reports whether the configured transport can
// carry MindFS instructions outside the user-message history.
func (p *Pool) SupportsDeveloperInstructions(agentName string) bool {
	if p == nil {
		return false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	def, ok := p.cfg.GetAgent(strings.TrimSpace(agentName))
	if !ok {
		return false
	}
	protocol := def.Protocol
	if protocol == "" {
		protocol = DefaultProtocol(agentName)
	}
	return protocol == ProtocolCodexSDK || protocol == ProtocolClaudeSDK
}

// SetDeviceSupport wires remote device support: resolver looks up devices
// by ID, configDir locates devices.json and the ssh mux dir.
func (p *Pool) SetDeviceSupport(resolver DeviceResolver, configDir string) {
	p.deviceMu.Lock()
	defer p.deviceMu.Unlock()
	p.deviceResolver = resolver
	p.deviceConfigDir = configDir
}

func (p *Pool) deviceSupport() (DeviceResolver, string) {
	p.deviceMu.RLock()
	defer p.deviceMu.RUnlock()
	return p.deviceResolver, p.deviceConfigDir
}

// resolveDeviceAgent rewrites a device-scoped definition into a local spawn
// of the __remote-agent shim. The shim re-execs ssh carrying the original
// command line (MINDFS_REMOTE_CMDLINE) plus any argv tokens the protocol
// SDK appended around the marker.
func (p *Pool) resolveDeviceAgent(def Definition, in agenttypes.OpenSessionInput) (Definition, agenttypes.OpenSessionInput, error) {
	if strings.TrimSpace(def.Device) == "" {
		return def, in, nil
	}
	resolver, configDir := p.deviceSupport()
	if resolver == nil || configDir == "" {
		return def, in, fmt.Errorf("agent %q targets device %q but device support is not configured", def.Name, def.Device)
	}
	device, ok := resolver(def.Device)
	if !ok {
		return def, in, fmt.Errorf("device %q is not configured", def.Device)
	}
	if !device.Enabled {
		return def, in, fmt.Errorf("device %q is disabled", device.DisplayName())
	}
	selfExe, err := os.Executable()
	if err != nil {
		return def, in, fmt.Errorf("resolve self executable: %w", err)
	}
	remoteCwd := strings.TrimSpace(def.ResolveCwd(in.RootPath))
	if remoteCwd == "" {
		remoteCwd = "~"
	}
	if remoteCwd != "~" && strings.HasPrefix(remoteCwd, "/") {
		// The cwd lives on the device. Some ACP CLIs (qoder) refuse to start
		// in a remote directory that does not exist, so materialize it first;
		// for a registered device root this is an idempotent no-op. Best
		// effort: under transient ssh mux contention a failure must not kill
		// the whole spawn — CLIs that need the dir report it themselves.
		if err := ensureDeviceDir(device, configDir, remoteCwd); err != nil {
			log.Printf("[agent/pool] prepare cwd %s on %s failed (continuing): %v", remoteCwd, device.DisplayName(), err)
		}
	}
	cmdline := append([]string{def.Command}, def.Args...)
	// cloneEnv returns nil for an empty def.Env; the overlay write below
	// would panic on a nil map, so materialize it here.
	env := cloneEnv(def.Env)
	if env == nil {
		env = make(map[string]string, 8)
	}
	overlay, err := remote.DeviceSpawnEnv(configDir, device.ID, remoteCwd, device.PathPrefix, cmdline)
	if err != nil {
		return def, in, err
	}
	for key, value := range overlay {
		env[key] = value
	}
	wrapped := def
	wrapped.Command = selfExe
	wrapped.Args = []string{remote.EnvMarker}
	wrapped.Env = env
	wrapped.CwdTemplate = ""
	// claude SDK stats the local cwd before spawning; the remote cwd is
	// carried by MINDFS_REMOTE_CWD instead.
	if effectiveProtocol(def.Name, def.Protocol) == ProtocolClaudeSDK {
		in.RootPath = ""
	}
	return wrapped, in, nil
}

func effectiveProtocol(name string, protocol Protocol) Protocol {
	if protocol != "" {
		return protocol
	}
	return DefaultProtocol(name)
}

// ensureDeviceDir creates dir on the device (mkdir -p) so remote CLIs that
// validate their working directory can start.
func ensureDeviceDir(device remote.Device, configDir, dir string) error {
	muxDir, err := remote.MuxDir(configDir)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if _, err = remote.Run(ctx, device.Target(), muxDir, "mkdir -p "+remote.Quote(dir), 12*time.Second); err == nil {
		return nil
	}
	time.Sleep(300 * time.Millisecond)
	_, err = remote.Run(ctx, device.Target(), muxDir, "mkdir -p "+remote.Quote(dir), 12*time.Second)
	return err
}

// GetOrCreate returns an existing session handle or creates a new one.
func (p *Pool) GetOrCreate(ctx context.Context, in agenttypes.OpenSessionInput) (agenttypes.Session, error) {
	if in.SessionKey == "" {
		return nil, errors.New("session key required")
	}

	p.mu.Lock()
retryExisting:
	if p.closed {
		p.mu.Unlock()
		return nil, errors.New("agent pool closed")
	}
	if entry, ok := p.sessions[in.SessionKey]; ok {
		if entry.closing {
			done := entry.closeDone
			p.mu.Unlock()
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-done:
			}
			p.mu.Lock()
			goto retryExisting
		}
		entry.lastUsedAt = time.Now()
		p.mu.Unlock()
		return entry.session, nil
	}
	def, ok := p.cfg.GetAgent(in.AgentName)
	if !ok {
		p.mu.Unlock()
		return nil, errors.New("agent not configured: " + in.AgentName)
	}
	p.mu.Unlock()

	// Remote device agents spawn the local __remote-agent shim, which
	// re-execs ssh; rewrite def/in before protocol dispatch.
	def, in, deviceErr := p.resolveDeviceAgent(def, in)
	if deviceErr != nil {
		return nil, deviceErr
	}

	protocol := effectiveProtocol(in.AgentName, def.Protocol)
	unlockRuntime := p.lockRuntimeGroups([]runtimeGroup{{agentName: in.AgentName, protocol: protocol}})
	defer unlockRuntime()

	// A concurrent opener may have created the session while this goroutine
	// waited for the shared runtime lock.
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return nil, errors.New("agent pool closed")
	}
	if entry, ok := p.sessions[in.SessionKey]; ok && entry != nil && entry.session != nil {
		entry.lastUsedAt = time.Now()
		existing := entry.session
		p.mu.Unlock()
		return existing, nil
	}
	p.mu.Unlock()

	// openSession starts subprocesses and can be slow, so keep it outside the pool lock.
	sess, err := p.openSession(ctx, protocol, def, in)
	if err != nil {
		return nil, err
	}

	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		_ = sess.Close()
		return nil, errors.New("agent pool closed")
	}
	// Another goroutine may have created the same session while the lock was released.
	if entry, ok := p.sessions[in.SessionKey]; ok {
		existing := entry.session
		entry.lastUsedAt = time.Now()
		p.mu.Unlock()
		if protocol == ProtocolClaudeSDK {
			_ = sess.Close()
		}
		return existing, nil
	}
	p.sessions[in.SessionKey] = &sessionEntry{
		agentName:  in.AgentName,
		sessionKey: in.SessionKey,
		protocol:   protocol,
		session:    sess,
		lastUsedAt: time.Now(),
	}
	p.mu.Unlock()
	return sess, nil
}

// BeginSessionUse prevents idle cleanup while a turn is using the session.
// The returned function must be called when that use finishes.
func (p *Pool) BeginSessionUse(sessionKey string) func() {
	p.mu.Lock()
	entry := p.sessions[sessionKey]
	if entry == nil || entry.closing {
		p.mu.Unlock()
		return func() {}
	}
	entry.activeUses++
	entry.lastUsedAt = time.Now()
	p.mu.Unlock()

	var once sync.Once
	return func() {
		once.Do(func() {
			p.mu.Lock()
			if current := p.sessions[sessionKey]; current == entry {
				if current.activeUses > 0 {
					current.activeUses--
				}
				current.lastUsedAt = time.Now()
			}
			p.mu.Unlock()
		})
	}
}

// ReleaseIdleSessions closes inactive runtime sessions without changing the
// persisted MindFS session or its closed_at metadata.
type IdleReleaseResult struct {
	ReleasedSessions int `json:"released_sessions"`
	FailedSessions   int `json:"failed_sessions"`
}

func (p *Pool) ReleaseIdleSessions(idleFor time.Duration, now time.Time) int {
	return p.ReleaseIdleSessionsDetailed(idleFor, now).ReleasedSessions
}

func (p *Pool) ReleaseIdleSessionsDetailed(idleFor time.Duration, now time.Time) IdleReleaseResult {
	if idleFor <= 0 {
		return IdleReleaseResult{}
	}
	cutoff := now.Add(-idleFor)
	return p.releaseSessionsDetailed(now, "idle_for="+idleFor.String(), func(entry *sessionEntry) bool {
		return !entry.lastUsedAt.After(cutoff)
	})
}

// ReleaseInactiveSessionsDetailed immediately releases every session that is
// not currently in use. It is intended for an explicit user action; automatic
// cleanup should continue to use ReleaseIdleSessionsDetailed with a threshold.
func (p *Pool) ReleaseInactiveSessionsDetailed(now time.Time) IdleReleaseResult {
	return p.releaseSessionsDetailed(now, "manual", func(_ *sessionEntry) bool { return true })
}

func (p *Pool) releaseSessionsDetailed(now time.Time, reason string, eligible func(*sessionEntry) bool) IdleReleaseResult {
	result := IdleReleaseResult{}
	p.mu.Lock()
	groups := make(map[runtimeGroup]struct{})
	for _, entry := range p.sessions {
		if entry == nil || entry.closing || entry.activeUses > 0 || !eligible(entry) {
			continue
		}
		groups[runtimeGroup{agentName: entry.agentName, protocol: entry.protocol}] = struct{}{}
	}
	p.mu.Unlock()
	if len(groups) == 0 {
		return result
	}
	groupList := make([]runtimeGroup, 0, len(groups))
	for group := range groups {
		groupList = append(groupList, group)
	}
	unlockRuntimes := p.lockRuntimeGroups(groupList)
	defer unlockRuntimes()

	p.mu.Lock()
	entries := make([]*sessionEntry, 0)
	for _, entry := range p.sessions {
		group := runtimeGroup{}
		if entry != nil {
			group = runtimeGroup{agentName: entry.agentName, protocol: entry.protocol}
		}
		if _, selected := groups[group]; !selected || entry == nil || entry.closing || entry.activeUses > 0 || !eligible(entry) {
			continue
		}
		entry.closing = true
		entry.closeDone = make(chan struct{})
		entries = append(entries, entry)
	}
	p.mu.Unlock()

	for _, entry := range entries {
		err := entry.session.Close()
		p.mu.Lock()
		current := p.sessions[entry.sessionKey]
		if current == entry {
			if err == nil {
				delete(p.sessions, entry.sessionKey)
				result.ReleasedSessions++
			} else {
				entry.closing = false
				entry.lastUsedAt = now
				result.FailedSessions++
			}
		}
		close(entry.closeDone)
		p.mu.Unlock()
		if err != nil {
			log.Printf("[agent/pool] idle_release.error session=%s agent=%s protocol=%s err=%v", entry.sessionKey, entry.agentName, entry.protocol, err)
		} else {
			log.Printf("[agent/pool] idle_release.done session=%s agent=%s protocol=%s reason=%s", entry.sessionKey, entry.agentName, entry.protocol, reason)
		}
	}
	return result
}

type runtimeGroup struct {
	agentName string
	protocol  Protocol
}

func (p *Pool) lockRuntimeGroups(groups []runtimeGroup) func() {
	if len(groups) == 0 {
		return func() {}
	}
	unique := make(map[runtimeGroup]struct{}, len(groups))
	ordered := make([]runtimeGroup, 0, len(groups))
	for _, group := range groups {
		if _, ok := unique[group]; ok {
			continue
		}
		unique[group] = struct{}{}
		ordered = append(ordered, group)
	}
	sort.Slice(ordered, func(i, j int) bool {
		if ordered[i].protocol != ordered[j].protocol {
			return ordered[i].protocol < ordered[j].protocol
		}
		return ordered[i].agentName < ordered[j].agentName
	})
	locks := make([]*sync.Mutex, 0, len(ordered))
	p.runtimeLocksMu.Lock()
	for _, group := range ordered {
		lock := p.runtimeLocks[group]
		if lock == nil {
			lock = &sync.Mutex{}
			p.runtimeLocks[group] = lock
		}
		locks = append(locks, lock)
	}
	p.runtimeLocksMu.Unlock()
	for _, lock := range locks {
		lock.Lock()
	}
	return func() {
		for index := len(locks) - 1; index >= 0; index-- {
			locks[index].Unlock()
		}
	}
}

func (p *Pool) StartIdleReleaseLoop(ctx context.Context, idleFor func() time.Duration) {
	if p == nil || idleFor == nil {
		return
	}
	go func() {
		ticker := time.NewTicker(time.Minute)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case now := <-ticker.C:
				p.ReleaseIdleSessions(idleFor(), now)
			}
		}
	}()
}

func (p *Pool) openSession(ctx context.Context, protocol Protocol, def Definition, in agenttypes.OpenSessionInput) (agenttypes.Session, error) {
	switch protocol {
	case ProtocolClaudeSDK:
		return p.claude.OpenSession(ctx, claude.OpenOptions{
			AgentName:             in.AgentName,
			SessionKey:            in.SessionKey,
			Model:                 in.Model,
			Effort:                in.Effort,
			PlanMode:              in.PlanMode,
			RootPath:              in.RootPath,
			Command:               def.Command,
			Args:                  append([]string{}, def.Args...),
			Env:                   cloneEnv(def.Env),
			DeveloperInstructions: in.DeveloperInstructions,
			ResumeSessionID:       in.AgentSessionID,
			ForkSessionID:         in.ForkPoint.AgentSessionID,
			ResumeMessageID:       in.ForkPoint.ClaudeMessageUUID,
		})
	case ProtocolCodexSDK:
		var codexUserOrdinal *int
		if in.ForkPoint.Kind == agenttypes.ForkPointCodexUserOrdinal {
			value := in.ForkPoint.CodexUserOrdinal
			codexUserOrdinal = &value
		}
		return p.codex.OpenSession(ctx, codex.OpenOptions{
			AgentName:             in.AgentName,
			SessionKey:            in.SessionKey,
			Model:                 in.Model,
			Effort:                in.Effort,
			FastService:           in.FastService,
			PlanMode:              in.PlanMode,
			Probe:                 in.Probe,
			RootPath:              in.RootPath,
			Command:               def.Command,
			Args:                  append([]string{}, def.Args...),
			Env:                   cloneEnv(def.Env),
			DeveloperInstructions: in.DeveloperInstructions,
			ResumeSessionID:       in.AgentSessionID,
			ForkSessionID:         in.ForkPoint.AgentSessionID,
			CodexUserOrdinal:      codexUserOrdinal,
		})
	case ProtocolACP:
		fallthrough
	default:
		cwd := def.ResolveCwd(in.RootPath)
		if strings.TrimSpace(def.Device) != "" {
			// The cwd is a remote path; acp.Start must not set the
			// local cmd.Dir to it. The shim cds remotely instead.
			cwd = ""
		}
		return p.acp.OpenSession(ctx, acp.OpenOptions{
			AgentName:       in.AgentName,
			SessionKey:      in.SessionKey,
			Model:           in.Model,
			Mode:            in.Mode,
			Effort:          in.Effort,
			RootPath:        in.RootPath,
			Command:         def.Command,
			Args:            def.BuildArgs(in.RootPath),
			Env:             cloneEnv(def.Env),
			Cwd:             cwd,
			ResumeSessionID: in.AgentSessionID,
		})
	}
}

func (p *Pool) KillAgentProcess(agentName string, wait time.Duration) (string, bool) {
	_ = wait
	def, ok := p.cfg.GetAgent(agentName)
	if !ok {
		return "", false
	}

	protocol := def.Protocol
	if protocol == "" {
		protocol = DefaultProtocol(agentName)
	}
	unlockRuntime := p.lockRuntimeGroups([]runtimeGroup{{agentName: agentName, protocol: protocol}})
	defer unlockRuntime()
	switch protocol {
	case ProtocolClaudeSDK:
		p.closeSessionsForAgent(agentName, ProtocolClaudeSDK)
		log.Printf("[agent/pool] kill_agent_process.claude_closed agent=%s", agentName)
		return "", true
	case ProtocolCodexSDK:
		p.closeSessionsForAgent(agentName, ProtocolCodexSDK)
		_ = p.codex.Close(agentName)
		log.Printf("[agent/pool] kill_agent_process.codex_closed agent=%s", agentName)
		return "", true
	case ProtocolACP:
		p.closeSessionsForAgent(agentName, ProtocolACP)
		_ = p.acp.Close(agentName)
		if hint, ok := p.acp.RecentCloseHint(agentName); ok {
			log.Printf("[agent/pool] kill_agent_process.hint agent=%s hint=%q", agentName, hint)
			return hint, true
		}
		log.Printf("[agent/pool] kill_agent_process.no_hint agent=%s", agentName)
		return "", false
	default:
		return "", false
	}
}

func (p *Pool) closeSessionsForAgent(agentName string, protocol Protocol) {
	p.closeSessions(
		p.takeSessions(func(entry *sessionEntry) bool {
			return entry.agentName == agentName && entry.protocol == protocol
		}),
	)
}

func cloneEnv(env map[string]string) map[string]string {
	if len(env) == 0 {
		return nil
	}
	out := make(map[string]string, len(env))
	for key, value := range env {
		out[key] = value
	}
	return out
}

// Close closes a session (not the underlying runtime pool).
func (p *Pool) Close(sessionKey string) {
	p.mu.Lock()
	groups := make([]runtimeGroup, 0, 1)
	if entry := p.sessions[sessionKey]; entry != nil {
		groups = append(groups, runtimeGroup{agentName: entry.agentName, protocol: entry.protocol})
	}
	p.mu.Unlock()
	unlockRuntimes := p.lockRuntimeGroups(groups)
	defer unlockRuntimes()
	entries := p.takeSessions(func(entry *sessionEntry) bool {
		return entry.sessionKey == sessionKey
	})
	if len(entries) == 0 {
		return
	}
	p.closeSessions(entries)
}

func (p *Pool) takeSessions(match func(*sessionEntry) bool) []*sessionEntry {
	p.mu.Lock()
	defer p.mu.Unlock()

	var entries []*sessionEntry
	for key, entry := range p.sessions {
		if entry == nil || !match(entry) {
			continue
		}
		entries = append(entries, entry)
		delete(p.sessions, key)
	}
	return entries
}

func (p *Pool) closeSessions(entries []*sessionEntry) {
	for _, entry := range entries {
		if entry == nil || entry.session == nil {
			continue
		}
		_ = entry.session.Close()
	}
}

// Config returns the pool configuration.
func (p *Pool) Config() Config {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.cfg
}

func (p *Pool) CodexRateLimits(ctx context.Context, agentName string) (codex.RateLimitStatus, error) {
	opts, err := p.codexRuntimeOptions(agentName)
	if err != nil {
		return codex.RateLimitStatus{}, err
	}
	return p.codex.ReadRateLimits(ctx, opts)
}

func (p *Pool) ConsumeCodexRateLimitReset(ctx context.Context, agentName, idempotencyKey, creditID string) (codex.ConsumeRateLimitResetResult, error) {
	opts, err := p.codexRuntimeOptions(agentName)
	if err != nil {
		return codex.ConsumeRateLimitResetResult{}, err
	}
	return p.codex.ConsumeRateLimitReset(ctx, opts, idempotencyKey, creditID)
}

func (p *Pool) codexRuntimeOptions(agentName string) (codex.OpenOptions, error) {
	agentName = strings.TrimSpace(agentName)
	if agentName == "" {
		return codex.OpenOptions{}, errors.New("agent required")
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return codex.OpenOptions{}, errors.New("agent pool closed")
	}
	def, ok := p.cfg.GetAgent(agentName)
	if !ok {
		return codex.OpenOptions{}, errors.New("agent not configured: " + agentName)
	}
	protocol := def.Protocol
	if protocol == "" {
		protocol = DefaultProtocol(agentName)
	}
	if protocol != ProtocolCodexSDK {
		return codex.OpenOptions{}, errors.New("agent does not use codex-sdk: " + agentName)
	}
	return codex.OpenOptions{
		AgentName: agentName,
		Command:   def.Command,
		Args:      append([]string(nil), def.Args...),
		Env:       cloneEnv(def.Env),
	}, nil
}

func (p *Pool) UpdateConfig(cfg Config) Config {
	p.mu.Lock()
	defer p.mu.Unlock()
	cfg = p.applyRuntimeEnvOverridesLocked(cfg)
	p.cfg = cfg
	return p.cfg
}

// UpsertDeviceAgents atomically adds device agent definitions to the live
// pool config (existing names are left untouched). Doing the read-modify-write
// under the pool lock prevents a concurrent hosted-config refresh from
// rolling the additions back with a stale snapshot.
func (p *Pool) UpsertDeviceAgents(defs []Definition) (Config, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	changed := false
	for _, def := range defs {
		if strings.TrimSpace(def.Device) == "" {
			continue
		}
		if _, exists := p.cfg.GetAgent(def.Name); exists {
			continue
		}
		p.cfg.Agents = append(p.cfg.Agents, def)
		changed = true
	}
	if changed {
		p.cfg = p.applyRuntimeEnvOverridesLocked(p.cfg)
	}
	return p.cfg, changed
}

// RemoveDeviceAgents drops every agent whose Device marker matches deviceID
// from the live pool config, atomically.
func (p *Pool) RemoveDeviceAgents(deviceID string) (Config, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	kept := make([]Definition, 0, len(p.cfg.Agents))
	changed := false
	for _, def := range p.cfg.Agents {
		if strings.TrimSpace(def.Device) == deviceID {
			changed = true
			continue
		}
		kept = append(kept, def)
	}
	if !changed {
		return p.cfg, false
	}
	p.cfg.Agents = kept
	return p.cfg, true
}

// MergeHostedConfig replaces the pool config with a hosted+local merge while
// preserving the pool's CURRENT device agents. Device agents are managed
// exclusively by UpsertDeviceAgents/RemoveDeviceAgents; the hosted refresh's
// disk snapshot may be arbitrarily stale relative to those mutations, so
// device entries from the merged snapshot are ignored (manual agents.json
// edits of device entries take effect after restart).
func (p *Pool) MergeHostedConfig(cfg Config) Config {
	p.mu.Lock()
	defer p.mu.Unlock()
	kept := make([]Definition, 0, len(cfg.Agents)+len(p.cfg.Agents))
	for _, def := range cfg.Agents {
		if strings.TrimSpace(def.Device) != "" {
			continue
		}
		kept = append(kept, def)
	}
	for _, def := range p.cfg.Agents {
		if strings.TrimSpace(def.Device) == "" {
			continue
		}
		kept = append(kept, def)
	}
	cfg.Agents = kept
	cfg = p.applyRuntimeEnvOverridesLocked(cfg)
	p.cfg = cfg
	return p.cfg
}

func (p *Pool) SetAgentEnv(agentName string, env map[string]string) error {
	if agentName == "" {
		return errors.New("agent required")
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	for i := range p.cfg.Agents {
		if p.cfg.Agents[i].Name != agentName {
			continue
		}
		p.runtimeEnv[agentName] = cloneEnv(env)
		p.cfg.Agents[i].Env = cloneEnv(env)
		return nil
	}
	return errors.New("agent not configured: " + agentName)
}

func (p *Pool) applyRuntimeEnvOverridesLocked(cfg Config) Config {
	if len(p.runtimeEnv) == 0 {
		return cfg
	}
	for i := range cfg.Agents {
		env, ok := p.runtimeEnv[cfg.Agents[i].Name]
		if !ok {
			continue
		}
		cfg.Agents[i].Env = cloneEnv(env)
	}
	return cfg
}

// Get returns an existing session handle if present.
func (p *Pool) Get(sessionKey string) (agenttypes.Session, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	entry, ok := p.sessions[sessionKey]
	if !ok || entry == nil || entry.session == nil {
		return nil, false
	}
	return entry.session, true
}

// Context returns the pool lifecycle context (read-only).
func (p *Pool) Context() context.Context {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.processCtx != nil {
		return p.processCtx
	}
	return context.Background()
}

// CloseAll closes all runtime resources.
func (p *Pool) CloseAll() {
	p.mu.Lock()
	p.closed = true
	cancel := p.cancel
	p.cancel = nil
	groups := make([]runtimeGroup, 0, len(p.cfg.Agents))
	for _, def := range p.cfg.Agents {
		protocol := def.Protocol
		if protocol == "" {
			protocol = DefaultProtocol(def.Name)
		}
		groups = append(groups, runtimeGroup{agentName: def.Name, protocol: protocol})
	}
	p.mu.Unlock()

	// Cancel the process lifecycle before waiting for runtime locks. An ACP
	// process may still be initializing while holding one of those locks; its
	// command and initialize RPC both use this context.
	if cancel != nil {
		cancel()
	}

	unlockRuntimes := p.lockRuntimeGroups(groups)
	defer unlockRuntimes()

	p.mu.Lock()
	sessions := p.sessions
	p.sessions = make(map[string]*sessionEntry)
	acpRuntime := p.acp
	claudeRuntime := p.claude
	codexRuntime := p.codex
	p.mu.Unlock()

	// Claude SDK sessions own independent CLI processes rather than a shared
	// runtime, so each resident session must be closed explicitly. Close them
	// concurrently so shutdown is bounded by one SDK grace period.
	var claudeCloseWG sync.WaitGroup
	for _, entry := range sessions {
		if entry == nil || entry.protocol != ProtocolClaudeSDK || entry.session == nil {
			continue
		}
		claudeCloseWG.Add(1)
		go func(entry *sessionEntry) {
			defer claudeCloseWG.Done()
			if err := entry.session.Close(); err != nil {
				log.Printf("[agent/pool] close_all.claude_error session=%s agent=%s err=%v", entry.sessionKey, entry.agentName, err)
			}
		}(entry)
	}
	claudeCloseWG.Wait()

	if acpRuntime != nil {
		acpRuntime.CloseAll()
	}
	if claudeRuntime != nil {
		claudeRuntime.CloseAll()
	}
	if codexRuntime != nil {
		codexRuntime.CloseAll()
	}
}
