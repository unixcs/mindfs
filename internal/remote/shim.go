package remote

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// Shim env channel. These ride inside the agent Definition Env map, which is
// the only env path that reaches every protocol's spawn (codex SDK replaces
// the process env with OpenOptions.Env entirely).
const (
	EnvMarker     = "__remote-agent"
	EnvDevice     = "MINDFS_REMOTE_DEVICE"
	EnvCmdline    = "MINDFS_REMOTE_CMDLINE"
	EnvCwd        = "MINDFS_REMOTE_CWD"
	EnvSSHPath    = "MINDFS_REMOTE_SSH"
	EnvConfigDir  = "MINDFS_REMOTE_CONFIG_DIR"
	EnvPathPrefix = "MINDFS_REMOTE_PATH_PREFIX"
)

// RunShim implements `mindfs __remote-agent`: it re-execs ssh with the
// original command line plus whatever argv the caller's SDK appended (for
// codex the SDK prepends "app-server" before our marker, so the rule is to
// forward every argv token except the marker itself, order preserved).
// stdin/stdout/stderr pass straight through so stdio protocols are unaware
// of the SSH hop. Returns the process exit code.
func RunShim(argv []string) int {
	deviceID := os.Getenv(EnvDevice)
	configDir := os.Getenv(EnvConfigDir)
	cmdlineRaw := os.Getenv(EnvCmdline)
	remoteCwd := os.Getenv(EnvCwd)
	pathPrefix := os.Getenv(EnvPathPrefix)

	fail := func(msg string) int {
		fmt.Fprintln(os.Stderr, "[mindfs __remote-agent] "+msg)
		return 127
	}
	if deviceID == "" || configDir == "" || cmdlineRaw == "" {
		return fail("missing remote env (device/config/cmdline)")
	}
	var cmdline []string
	if err := json.Unmarshal([]byte(cmdlineRaw), &cmdline); err != nil || len(cmdline) == 0 {
		return fail("bad MINDFS_REMOTE_CMDLINE")
	}
	extra := ShimExtraArgs(argv)
	full := append(append([]string{}, cmdline...), extra...)

	store, err := NewStore(configDir)
	if err != nil {
		return fail("load devices: " + err.Error())
	}
	device, ok := store.Get(deviceID)
	if !ok {
		return fail(fmt.Sprintf("device %q not found", deviceID))
	}
	if !device.Enabled {
		return fail(fmt.Sprintf("device %q is disabled", deviceID))
	}
	if err := ValidateLoginArgs(device.Target()); err != nil {
		return fail(err.Error())
	}
	sshPath := os.Getenv(EnvSSHPath)
	if sshPath == "" {
		sshPath = ResolveSSHBinary()
	}
	if sshPath == "" {
		return fail("ssh binary not available")
	}
	muxDir, err := MuxDir(configDir)
	if err != nil {
		return fail("prepare ssh mux dir: " + err.Error())
	}
	remoteCmd := RemoteCommand(remoteCwd, pathPrefix, full, nil)
	args := append(BaseArgsDirect(device.Target(), muxDir), LoginCommand(remoteCmd)...)
	cmd := exec.Command(sshPath, args...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Env = processEnv()
	if err := cmd.Run(); err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return exitErr.ExitCode()
		}
		fmt.Fprintln(os.Stderr, "[mindfs __remote-agent] ssh exec failed: "+err.Error())
		return 255
	}
	return 0
}

// IsShimArgv reports whether the process was invoked as the remote shim.
func IsShimArgv(args []string) bool {
	return len(args) > 0 && strings.TrimSpace(args[0]) == EnvMarker
}

// ShimExtraArgs strips the marker from the shim argv, preserving the order of
// everything else. Protocol SDKs add their own tokens around the marker (the
// codex SDK prepends "app-server" before it), and those tokens must reach the
// remote command line unchanged.
func ShimExtraArgs(argv []string) []string {
	extra := make([]string, 0, len(argv))
	for _, token := range argv {
		if token == EnvMarker {
			continue
		}
		extra = append(extra, token)
	}
	return extra
}

// DeviceSpawnEnv builds the env overlay that turns a Definition spawn into a
// remote one. Values travel through the Definition Env channel.
func DeviceSpawnEnv(configDir, deviceID, remoteCwd, pathPrefix string, cmdline []string) (map[string]string, error) {
	payload, err := json.Marshal(cmdline)
	if err != nil {
		return nil, err
	}
	sshPath := ResolveSSHBinary()
	if sshPath == "" {
		return nil, errors.New("ssh 客户端不可用（未找到 ssh 可执行文件）")
	}
	env := map[string]string{
		EnvDevice:    deviceID,
		EnvCmdline:   string(payload),
		EnvCwd:       remoteCwd,
		EnvSSHPath:   sshPath,
		EnvConfigDir: configDir,
	}
	if prefix := strings.TrimSpace(pathPrefix); prefix != "" {
		env[EnvPathPrefix] = prefix
	}
	return env, nil
}
