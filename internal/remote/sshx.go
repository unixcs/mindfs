package remote

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

// maxRemoteOutput caps how much remote stdout is buffered in memory. A
// misbehaving or compromised device could otherwise stream unbounded output
// (tar/cat gone wild) and OOM the panel process.
const maxRemoteOutput = 256 << 20

// Target validation is security-critical: host and user are appended as a
// single ssh argv token, and ssh keeps parsing options until the first
// non-option token. A user value like "-oProxyCommand=..." would be parsed as
// an option whose ProxyCommand executes ON THE PANEL HOST via /bin/sh.
var (
	targetUserPattern = regexp.MustCompile(`^[A-Za-z0-9._][A-Za-z0-9._-]{0,62}$`)
	// Hostname/IPv4/Tailscale-name; bracketed IPv6 literals allowed. A leading
	// "-" is rejected even though User@host concatenation already neutralizes
	// it (defense in depth for future call sites).
	targetHostPattern = regexp.MustCompile(`^(?:[A-Za-z0-9._][A-Za-z0-9._-]{0,252}|\[[0-9a-fA-F:.]{2,45}\])$`)
)

// ValidateTarget rejects Host/User values that could smuggle ssh command-line
// options (see the comment above the patterns).
func ValidateTarget(t Target) error {
	if !targetUserPattern.MatchString(t.User) {
		return errors.New("SSH 用户名包含非法字符")
	}
	if !targetHostPattern.MatchString(t.Host) {
		return errors.New("SSH 主机名包含非法字符")
	}
	return nil
}

// Target is one SSH endpoint.
type Target struct {
	Host         string
	Port         int
	User         string
	IdentityFile string
}

// sshBinaryPath caches the resolved absolute ssh path. Lookup failures are
// NOT cached permanently (an empty PATH at startup or a PATH change later
// must not brick every remote feature until process restart); only a
// successful resolution is sticky.
var (
	sshBinaryMu   sync.Mutex
	sshBinaryPath string
)

// ResolveSSHBinary returns the absolute ssh path, re-resolving on every call
// until one lookup succeeds. Call once at server start to warm it.
func ResolveSSHBinary() string {
	sshBinaryMu.Lock()
	defer sshBinaryMu.Unlock()
	if sshBinaryPath != "" {
		return sshBinaryPath
	}
	if path, err := exec.LookPath("ssh"); err == nil {
		sshBinaryPath = path
		return sshBinaryPath
	}
	for _, candidate := range []string{"/usr/bin/ssh", "/bin/ssh", "/usr/local/bin/ssh"} {
		if info, err := os.Stat(candidate); err == nil && !info.IsDir() {
			sshBinaryPath = candidate
			return sshBinaryPath
		}
	}
	return ""
}

// MuxDir returns (and creates) the directory holding ControlMaster sockets
// and our private known_hosts file.
func MuxDir(configDir string) (string, error) {
	dir := filepath.Join(configDir, "ssh-mux")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	return dir, nil
}

// BaseArgs builds the deterministic ssh argument prefix. -F /dev/null keeps
// behavior independent of the invoking user's ~/.ssh/config.
// BaseArgs builds ssh args that multiplex over one ControlMaster connection
// per host+user. Use for short-lived commands (probes, ls/cat, sync).
func BaseArgs(t Target, muxDir string) []string {
	return sshArgs(t, muxDir, true)
}

// BaseArgsDirect builds ssh args with a dedicated TCP connection (no mux).
// Long-lived agent transports must use this: muxed sessions count against the
// remote sshd MaxSessions, and several agent processes plus short commands
// sharing one master get "session open refused" once the limit is hit.
func BaseArgsDirect(t Target, muxDir string) []string {
	return sshArgs(t, muxDir, false)
}

func sshArgs(t Target, muxDir string, mux bool) []string {
	args := []string{
		"-F", "/dev/null",
		"-o", "BatchMode=yes",
		"-o", "StrictHostKeyChecking=accept-new",
		"-o", "UserKnownHostsFile=" + filepath.Join(muxDir, "known_hosts"),
		"-o", "ServerAliveInterval=15",
		"-o", "ServerAliveCountMax=3",
		"-o", "ConnectTimeout=8",
	}
	if mux {
		args = append(args,
			"-o", "ControlMaster=auto",
			"-o", "ControlPersist=10m",
			"-o", "ControlPath="+filepath.Join(muxDir, "mux-%r@%h-%p"),
		)
	}
	if t.Port > 0 && t.Port != 22 {
		args = append(args, "-p", fmt.Sprintf("%d", t.Port))
	}
	if identity := strings.TrimSpace(t.IdentityFile); identity != "" {
		args = append(args, "-i", identity, "-o", "IdentitiesOnly=yes")
	}
	host := t.Host
	if t.User != "" {
		host = t.User + "@" + t.Host
	}
	return append(args, host)
}

// ValidateLoginArgs is the belt-and-braces check for every code path that
// turns a Target into ssh argv. Store.Save and NewStore already validate, but
// the shim and pool re-load devices.json from disk, so the check is repeated
// at the point of use.
func ValidateLoginArgs(t Target) error {
	if err := ValidateTarget(t); err != nil {
		return err
	}
	if identity := strings.TrimSpace(t.IdentityFile); identity != "" && !filepath.IsAbs(identity) {
		return errors.New("私钥路径必须是绝对路径")
	}
	return nil
}

// Quote single-quotes one token for POSIX shell consumption.
func Quote(arg string) string {
	return "'" + strings.ReplaceAll(arg, "'", `'\''`) + "'"
}

// RemoteCommand builds the remote bash -lc payload:
//
//	cd <cwd> 2>/dev/null || true;  <pathPrefix>;  export K=V...;  exec <cmd...>
//
// The tolerant cd lets probes fall back to $HOME when the local probe dir
// does not exist remotely.
func RemoteCommand(cwd, pathPrefix string, cmdline []string, env map[string]string) string {
	var b strings.Builder
	if cwd = strings.TrimSpace(cwd); cwd != "" && cwd != "~" {
		b.WriteString("cd " + Quote(cwd) + " 2>/dev/null || true; ")
	}
	if prefix := strings.TrimSpace(pathPrefix); prefix != "" {
		b.WriteString(prefix + "; ")
	}
	for key, value := range env {
		if strings.HasPrefix(key, "MINDFS_REMOTE_") || key == "" {
			continue
		}
		// The key is spliced into the shell string unquoted, so only accept
		// strict POSIX identifier keys. Values go through Quote.
		if !envKeyPattern.MatchString(key) {
			continue
		}
		b.WriteString("export " + key + "=" + Quote(value) + "; ")
	}
	b.WriteString("exec")
	for _, part := range cmdline {
		b.WriteString(" " + Quote(part))
	}
	return b.String()
}

// LoginCommand wraps a ready-made remote command string as ssh argv tokens
// ("bash" "-lc" <quoted-remote-cmd>). ssh joins argv with spaces and hands
// the result to the remote user's shell, so the payload must be shell-quoted.
func LoginCommand(remoteCmd string) []string {
	return []string{"bash", "-lc", Quote(remoteCmd)}
}

var (
	semMu       sync.Mutex
	semaphores  = map[string]chan struct{}{}
	semCapacity = 4
)

var envKeyPattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

func semaphore(key string) chan struct{} {
	semMu.Lock()
	defer semMu.Unlock()
	ch, ok := semaphores[key]
	if !ok {
		ch = make(chan struct{}, semCapacity)
		semaphores[key] = ch
	}
	return ch
}

// Run executes a short-lived remote command and returns stdout. Stderr is
// folded into the error for diagnosability. Concurrency per host is capped.
func Run(ctx context.Context, t Target, muxDir, remoteCmd string, timeout time.Duration) ([]byte, error) {
	sshPath := ResolveSSHBinary()
	if sshPath == "" {
		return nil, errors.New("ssh 客户端不可用（未找到 ssh 可执行文件）")
	}
	if err := ValidateLoginArgs(t); err != nil {
		return nil, err
	}
	if timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	sem := semaphore(t.Host + ":" + t.User)
	select {
	case sem <- struct{}{}:
		defer func() { <-sem }()
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	args := append(BaseArgs(t, muxDir), LoginCommand(remoteCmd)...)
	cmd := exec.CommandContext(ctx, sshPath, args...)
	cmd.Env = processEnv()
	var stderr limitedBuffer
	cmd.Stderr = &stderr
	// WaitDelay is load-bearing: in mux mode the ControlMaster keeps the
	// stdout pipe's write end alive after the client process is killed, so
	// without it Output()/Wait() blocks until the remote command finishes on
	// its own and the per-host semaphore slot is held the whole time.
	cmd.WaitDelay = 3 * time.Second
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	var out bytes.Buffer
	copyDone := make(chan struct{})
	go func() {
		defer close(copyDone)
		_, _ = io.Copy(&out, io.LimitReader(stdout, maxRemoteOutput+1))
	}()
	waitErr := cmd.Wait()
	// Wait closes the pipe after the process exits (WaitDelay bounds how long
	// a lingering master can hold it), which unblocks the copy goroutine.
	<-copyDone
	if out.Len() > maxRemoteOutput {
		return out.Bytes()[:maxRemoteOutput], errors.New("远程命令输出超过 256MB，已截断")
	}
	if waitErr != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = waitErr.Error()
		}
		if ctx.Err() != nil {
			return out.Bytes(), fmt.Errorf("ssh 命令超时或被取消: %w", ctx.Err())
		}
		return out.Bytes(), errors.New("ssh: " + msg)
	}
	return out.Bytes(), nil
}

// limitedBuffer caps how much stderr is buffered: a compromised device could
// otherwise stream unbounded diagnostics (profile noise, cat /dev/zero) and
// OOM the panel process.
type limitedBuffer struct {
	buf     bytes.Buffer
	max     int
	dropped bool
}

const maxRemoteStderr = 1 << 20

func (b *limitedBuffer) Write(p []byte) (int, error) {
	if b.buf.Len() < b.max {
		room := b.max - b.buf.Len()
		if room < len(p) {
			b.buf.Write(p[:room])
			b.dropped = true
			return len(p), nil
		}
		return b.buf.Write(p)
	}
	b.dropped = true
	return len(p), nil
}

func (b *limitedBuffer) String() string {
	if b.dropped {
		return b.buf.String() + "\n…(stderr 超过 1MB，已截断)"
	}
	return b.buf.String()
}

func processEnv() []string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		home = "/root"
	}
	return []string{
		"HOME=" + home,
		"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
	}
}
