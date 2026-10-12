package remote

import (
	"bytes"
	"context"
	"fmt"
	"path"
	"sort"
	"strings"
	"sync"
	"time"
)

// ProbeCLIs is the default CLI inventory probed by ProbeDevice. The device
// management API exposes these as the aggregatable remote CLIs.
var ProbeCLIs = []string{"opencode", "codex", "qodercli", "codebuddy", "claude"}

// ScanRoot is one candidate project directory discovered on a device.
type ScanRoot struct {
	Path string `json:"path"`
	Name string `json:"name"`
}

// ProbeResult reports the outcome of probing a remote device.
//
// CLIs maps a CLI name to its resolved absolute path; a missing entry or an
// empty string means "not found on the device". The HTTP layer turns empty
// values into JSON null for the frontend.
type ProbeResult struct {
	Ok        bool              `json:"ok"`
	Hostname  string            `json:"hostname,omitempty"`
	Home      string            `json:"home,omitempty"`
	LatencyMS int64             `json:"latency_ms"`
	CLIs      map[string]string `json:"clis"`
	Errors    []string          `json:"errors,omitempty"`
}

// probeCommandTimeout bounds each individual SSH round trip during probing.
const probeCommandTimeout = 10 * time.Second

// withPathPrefix prepends the device PATH prefix when set; with an empty
// prefix it must return the bare statement, because a leading "; " is a bash
// syntax error on the remote side.
func withPathPrefix(prefix, stmt string) string {
	prefix = strings.TrimSpace(prefix)
	if prefix == "" {
		return stmt
	}
	return prefix + "; " + stmt
}

// ProbeDevice SSHes into the device and checks, for every CLI in clis
// (default ProbeCLIs), whether it exists via `command -v <cli>`; it also
// resolves the remote $HOME. Each check runs concurrently through the shared
// SSH mux. Connectivity/lookup failures degrade into result.Errors instead of
// aborting the whole probe; the caller decides what to do with result.Ok.
func ProbeDevice(ctx context.Context, device Device, muxDir string, clis []string) *ProbeResult {
	if len(clis) == 0 {
		clis = ProbeCLIs
	}
	result := &ProbeResult{CLIs: make(map[string]string, len(clis))}
	start := time.Now()

	var mu sync.Mutex
	var wg sync.WaitGroup

	// Connectivity + $HOME. This doubles as the reachability check.
	wg.Add(1)
	go func() {
		defer wg.Done()
		out, err := Run(ctx, device.Target(), muxDir, withPathPrefix(device.PathPrefix, "printenv HOME"), probeCommandTimeout)
		mu.Lock()
		defer mu.Unlock()
		if err != nil {
			result.Errors = append(result.Errors, fmt.Sprintf("resolve $HOME: %v", err))
			return
		}
		home := strings.TrimSpace(string(out))
		if home == "" {
			result.Errors = append(result.Errors, "resolve $HOME: empty response")
			return
		}
		result.Home = home
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		out, err := Run(ctx, device.Target(), muxDir, withPathPrefix(device.PathPrefix, "hostname 2>/dev/null || true"), probeCommandTimeout)
		mu.Lock()
		defer mu.Unlock()
		if err == nil {
			result.Hostname = strings.TrimSpace(string(out))
		}
	}()

	for _, cli := range clis {
		wg.Add(1)
		go func(cli string) {
			defer wg.Done()
			found, err := probeRemoteCLI(ctx, device, muxDir, cli)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				result.Errors = append(result.Errors, fmt.Sprintf("%s: %v", cli, err))
				return
			}
			result.CLIs[cli] = found
		}(cli)
	}

	wg.Wait()
	result.LatencyMS = time.Since(start).Milliseconds()
	// Ok means: reachable enough that agent aggregation can work — $HOME
	// resolved (proving bash -lc + PATH work). A reachable device with zero
	// supported CLIs is still "ok"; provisioning just yields nothing.
	result.Ok = result.Home != ""
	return result
}

// probeRemoteCLI runs `command -v <cli>` remotely. remote.Run folds stderr and
// treats non-zero exits as errors, so the command captures its own exit code
// (`; echo rc=$?`) and we parse it out instead of relying on the error path.
func probeRemoteCLI(ctx context.Context, device Device, muxDir, cli string) (string, error) {
	cmd := withPathPrefix(device.PathPrefix, "command -v "+Quote(cli)+" 2>/dev/null; echo rc=$?")
	out, err := Run(ctx, device.Target(), muxDir, cmd, probeCommandTimeout)
	if err != nil {
		return "", err
	}
	var found string
	rc := -1
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(line, "rc="):
			fmt.Sscanf(line, "rc=%d", &rc)
		case line != "" && found == "":
			found = line
		}
	}
	if rc != 0 {
		return "", nil
	}
	return found, nil
}

// ScanDeviceRoots lists first-level subdirectories of the device's watch
// roots (default: the remote $HOME). Output is NUL-delimited so names with
// spaces or newlines survive intact.
func ScanDeviceRoots(ctx context.Context, device Device, muxDir string, timeout time.Duration) ([]ScanRoot, error) {
	if timeout <= 0 {
		timeout = 20 * time.Second
	}
	watchRoots := device.WatchRoots
	if len(watchRoots) == 0 {
		out, err := Run(ctx, device.Target(), muxDir, withPathPrefix(device.PathPrefix, "printenv HOME"), probeCommandTimeout)
		if err != nil {
			return nil, fmt.Errorf("resolve $HOME: %w", err)
		}
		home := strings.TrimSpace(string(out))
		if home == "" {
			return nil, fmt.Errorf("resolve $HOME: empty response")
		}
		watchRoots = []string{home}
	}

	seen := make(map[string]bool)
	roots := make([]ScanRoot, 0, 16)
	for _, watchRoot := range watchRoots {
		watchRoot = strings.TrimSpace(watchRoot)
		if watchRoot == "" {
			continue
		}
		cmd := withPathPrefix(device.PathPrefix, "find "+Quote(watchRoot)+" -mindepth 1 -maxdepth 1 -type d -print0 2>/dev/null")
		out, err := Run(ctx, device.Target(), muxDir, cmd, timeout)
		if err != nil {
			return nil, fmt.Errorf("scan %s: %w", watchRoot, err)
		}
		for _, entry := range bytes.Split(out, []byte{0}) {
			dirPath := strings.TrimSpace(string(entry))
			if dirPath == "" || seen[dirPath] {
				continue
			}
			seen[dirPath] = true
			roots = append(roots, ScanRoot{Path: dirPath, Name: path.Base(dirPath)})
		}
	}
	sort.Slice(roots, func(i, j int) bool { return roots[i].Path < roots[j].Path })
	return roots, nil
}
