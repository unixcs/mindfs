package remote

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestShimExtraArgsCodexPrepend(t *testing.T) {
	// codex SDK builds argv as ["app-server"] + def.Args, so the marker lands
	// after the SDK's own token.
	got := ShimExtraArgs([]string{"app-server", EnvMarker})
	want := []string{"app-server"}
	if len(got) != len(want) || got[0] != want[0] {
		t.Fatalf("ShimExtraArgs = %v, want %v", got, want)
	}
}

func TestShimExtraArgsACPBare(t *testing.T) {
	got := ShimExtraArgs([]string{EnvMarker})
	if len(got) != 0 {
		t.Fatalf("ShimExtraArgs = %v, want empty", got)
	}
}

func TestShimExtraArgsClaudeAppends(t *testing.T) {
	// claude SDK appends its stream-json flags after def.Args.
	got := ShimExtraArgs([]string{EnvMarker, "--output-format", "stream-json", "--verbose"})
	want := []string{"--output-format", "stream-json", "--verbose"}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Fatalf("ShimExtraArgs = %v, want %v", got, want)
	}
}

func TestShimExtraArgsMarkerInMiddle(t *testing.T) {
	got := ShimExtraArgs([]string{"--foo", EnvMarker, "--bar"})
	if strings.Join(got, " ") != "--foo --bar" {
		t.Fatalf("ShimExtraArgs = %v, want [--foo --bar]", got)
	}
}

func TestIsShimArgv(t *testing.T) {
	if !IsShimArgv([]string{EnvMarker}) {
		t.Fatal("marker argv should match")
	}
	if IsShimArgv([]string{"serve"}) {
		t.Fatal("plain argv should not match")
	}
	if IsShimArgv(nil) {
		t.Fatal("empty argv should not match")
	}
}

func TestRemoteCommandQuoting(t *testing.T) {
	cmd := RemoteCommand("/data/pro ject", "export PATH=$HOME/.opencode/bin:$PATH",
		[]string{"codex", "app-server"}, map[string]string{
			EnvDevice:      "jp",
			"MINDFS_OTHER": "x",
			"API_KEY":      "abc'123",
		})
	if !strings.HasPrefix(cmd, "cd '/data/pro ject' 2>/dev/null || true; ") {
		t.Fatalf("missing tolerant cd: %q", cmd)
	}
	if !strings.Contains(cmd, "export PATH=$HOME/.opencode/bin:$PATH; ") {
		t.Fatalf("missing path prefix: %q", cmd)
	}
	if strings.Contains(cmd, EnvDevice) {
		t.Fatalf("MINDFS_REMOTE_* env must not leak into remote command: %q", cmd)
	}
	if !strings.Contains(cmd, `export API_KEY='abc'\''123'`) {
		t.Fatalf("single quote not escaped: %q", cmd)
	}
	if !strings.HasSuffix(cmd, "exec 'codex' 'app-server'") {
		t.Fatalf("cmdline not exec'd: %q", cmd)
	}
}

func TestRemoteCommandHomeCwdOmitted(t *testing.T) {
	cmd := RemoteCommand("~", "", []string{"opencode", "--acp"}, nil)
	if strings.Contains(cmd, "cd ") {
		t.Fatalf("~ cwd should skip cd: %q", cmd)
	}
	if !strings.HasSuffix(cmd, "exec 'opencode' '--acp'") {
		t.Fatalf("cmdline wrong: %q", cmd)
	}
}

func TestDeviceSpawnEnvRoundTrip(t *testing.T) {
	env, err := DeviceSpawnEnv("/tmp/cfg", "jp", "/data/w", "", []string{"qodercli", "--acp"})
	if err != nil {
		t.Fatalf("DeviceSpawnEnv: %v", err)
	}
	if env[EnvDevice] != "jp" || env[EnvCwd] != "/data/w" {
		t.Fatalf("env mismatch: %v", env)
	}
	var cmdline []string
	if err := json.Unmarshal([]byte(env[EnvCmdline]), &cmdline); err != nil {
		t.Fatalf("cmdline not json: %v", err)
	}
	if strings.Join(cmdline, " ") != "qodercli --acp" {
		t.Fatalf("cmdline = %v", cmdline)
	}
	if _, ok := env[EnvPathPrefix]; ok {
		t.Fatal("empty path prefix should be omitted")
	}
}

func TestLoginCommandShape(t *testing.T) {
	got := LoginCommand("echo hi")
	if len(got) != 3 || got[0] != "bash" || got[1] != "-lc" {
		t.Fatalf("LoginCommand = %v", got)
	}
	if !strings.HasPrefix(got[2], "'echo hi'") {
		t.Fatalf("payload not quoted: %q", got[2])
	}
}
