package remote

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// mirrorSessionKeyPattern is the whitelist remote session keys must match
// before they are imported or fetched. Session keys become file names under
// the local meta dir (sessions/<key>.jsonl), so anything outside
// [A-Za-z0-9._-] is rejected outright.
var mirrorSessionKeyPattern = regexp.MustCompile(`^[A-Za-z0-9._-]{1,128}$`)

// IsValidSessionKey reports whether key may be used as a mirrored session
// identifier (db primary key + jsonl file name). Beyond the character
// whitelist it rejects any embedded ".." so a key can never resolve outside
// the sessions directory, locally or remotely.
func IsValidSessionKey(key string) bool {
	key = strings.TrimSpace(key)
	if !mirrorSessionKeyPattern.MatchString(key) {
		return false
	}
	return key != "." && key != ".." && !strings.Contains(key, "..")
}

// mirrorFetcher executes one remote command and returns its stdout. Production
// wires this to SSH via remote.Run; tests inject a fake, which keeps the whole
// mirror pipeline testable without a real device.
type mirrorFetcher func(ctx context.Context, remoteCmd string) ([]byte, error)

// SSHFetcher adapts remote.Run into a mirrorFetcher bound to one device.
func SSHFetcher(device Device, muxDir string, timeout time.Duration) mirrorFetcher {
	if timeout <= 0 {
		timeout = 50 * time.Second
	}
	return func(ctx context.Context, remoteCmd string) ([]byte, error) {
		return Run(ctx, device.Target(), muxDir, remoteCmd, timeout)
	}
}

// RemoteRootInfo is one registry entry of a remote MindFS instance. Field
// names match server/internal/fs.RootInfo's JSON tags so the remote
// registry.json parses directly.
type RemoteRootInfo struct {
	ID           string `json:"id"`
	Name         string `json:"name"`
	RootPath     string `json:"root_path"`
	MetaLocation string `json:"meta_location"`
}

// RemoteConfigDir derives the remote MindFS config dir from the device's
// MindFSHome (mirrors os.UserConfigDir()/mindfs on a POSIX remote).
func RemoteConfigDir(mindFSHome string) string {
	return path.Join(strings.TrimRight(mindFSHome, "/"), ".config", "mindfs")
}

// DiscoverRemoteRoots reads the remote MindFS registry and returns the roots
// the remote instance knows about.
func DiscoverRemoteRoots(ctx context.Context, fetch mirrorFetcher, mindFSHome string) ([]RemoteRootInfo, error) {
	mindFSHome = strings.TrimSpace(mindFSHome)
	if mindFSHome == "" {
		return nil, fmt.Errorf("device mindfsHome 未配置，无法发现远程会话")
	}
	if fetch == nil {
		return nil, fmt.Errorf("mirror fetcher required")
	}
	out, err := fetch(ctx, "cat "+Quote(RemoteConfigDir(mindFSHome)+"/registry.json"))
	if err != nil {
		return nil, fmt.Errorf("读取远程 registry 失败: %w", err)
	}
	return ParseRemoteRegistry(out)
}

// ParseRemoteRegistry parses registry.json content ({dirs: [...], order: [...]}).
func ParseRemoteRegistry(payload []byte) ([]RemoteRootInfo, error) {
	var doc struct {
		Dirs []RemoteRootInfo `json:"dirs"`
	}
	if err := json.Unmarshal(payload, &doc); err != nil {
		return nil, fmt.Errorf("解析远程 registry 失败: %w", err)
	}
	return doc.Dirs, nil
}

// MatchRootByPath finds the remote root whose cleaned RootPath equals the
// given local device-root path. Comparison is POSIX-cleaned on both sides.
func MatchRootByPath(roots []RemoteRootInfo, rootPath string) (RemoteRootInfo, bool) {
	want := path.Clean(strings.ReplaceAll(strings.TrimSpace(rootPath), "\\", "/"))
	for _, root := range roots {
		if path.Clean(strings.TrimSpace(root.RootPath)) == want && want != "" {
			return root, true
		}
	}
	return RemoteRootInfo{}, false
}

// RemoteMetaDirPath derives where the remote MindFS instance stores a root's
// meta data (mirrors fs.RootInfo.MetaDir): project-rooted meta lives at
// <RootPath>/.mindfs, home-rooted meta at <MindFSHome>/.mindfs/<rootID>.
func RemoteMetaDirPath(mindFSHome string, root RemoteRootInfo) string {
	if strings.TrimSpace(root.MetaLocation) == "home" {
		return path.Join(strings.TrimRight(mindFSHome, "/"), ".mindfs", root.ID)
	}
	return path.Join(strings.TrimRight(strings.TrimSpace(root.RootPath), "/"), ".mindfs")
}

// noSessionsMarker is emitted by the fetch command when the remote root has
// no sessions directory yet (a fresh project). Distinguishing it from an
// empty tarball keeps "no sessions yet" and "transfer produced nothing" apart.
const noSessionsMarker = "__MINDFS_NO_SESSIONS__"

// mirrorFetchLimit caps how much uncompressed data one fetch may produce
// (a root's sessions dir is normally a few MB at most).
const mirrorFetchLimit = 512 << 20 // 512MB

// FetchedMirror is a remote root's meta directory materialized locally.
type FetchedMirror struct {
	// Dir is the local temp directory holding the remote meta layout
	// (Dir/sessions/session-list.db, Dir/sessions/<key>.jsonl ...).
	Dir string
	// Empty is true when the remote root has no sessions directory yet.
	Empty bool
	// cleanup removes the temp dir; call it once the bundle is consumed.
	cleanup func()
}

func (f *FetchedMirror) Cleanup() {
	if f != nil && f.cleanup != nil {
		f.cleanup()
	}
}

// FetchRemoteMeta streams the remote meta dir (`tar czf - -C <dir> sessions`)
// in a single SSH round trip and extracts it into a local temp directory.
func FetchRemoteMeta(ctx context.Context, fetch mirrorFetcher, metaDir string) (*FetchedMirror, error) {
	if fetch == nil {
		return nil, fmt.Errorf("mirror fetcher required")
	}
	metaDir = strings.TrimSpace(metaDir)
	if metaDir == "" {
		return nil, fmt.Errorf("remote meta dir required")
	}
	cmd := "if [ -d " + Quote(path.Join(metaDir, "sessions")) + " ]; then tar czf - -C " + Quote(metaDir) + " sessions; else echo " + Quote(noSessionsMarker) + "; fi"
	out, err := fetch(ctx, cmd)
	if err != nil {
		return nil, fmt.Errorf("拉取远程会话数据失败: %w", err)
	}
	if bytes.HasPrefix(bytes.TrimSpace(out), []byte(noSessionsMarker)) {
		dir, err := os.MkdirTemp("", "mindfs-mirror-")
		if err != nil {
			return nil, err
		}
		return &FetchedMirror{Dir: dir, Empty: true, cleanup: func() { os.RemoveAll(dir) }}, nil
	}

	dir, err := os.MkdirTemp("", "mindfs-mirror-")
	if err != nil {
		return nil, err
	}
	result := &FetchedMirror{Dir: dir, cleanup: func() { os.RemoveAll(dir) }}
	if err := extractTarGz(out, dir); err != nil {
		result.Cleanup()
		return nil, fmt.Errorf("解包远程会话数据失败: %w", err)
	}
	return result, nil
}

// extractTarGz unpacks a gzipped tarball into dir, rejecting path traversal
// and enforcing mirrorFetchLimit.
func extractTarGz(payload []byte, dir string) error {
	gz, err := gzip.NewReader(bytes.NewReader(payload))
	if err != nil {
		return fmt.Errorf("gzip: %w", err)
	}
	defer gz.Close()

	reader := tar.NewReader(gz)
	var total int64
	for {
		header, err := reader.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		name := path.Clean(strings.TrimPrefix(filepath.ToSlash(header.Name), "/"))
		if name == "." || name == ".." || strings.HasPrefix(name, "../") || strings.Contains(name, "/../") {
			return fmt.Errorf("unsafe tar entry: %s", header.Name)
		}
		target := filepath.Join(dir, filepath.FromSlash(name))
		switch header.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			file, err := os.OpenFile(target, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, os.FileMode(header.Mode&0o755)|0o600)
			if err != nil {
				return err
			}
			written, err := io.Copy(file, io.LimitReader(reader, mirrorFetchLimit-total+1))
			file.Close()
			if err != nil {
				return err
			}
			total += written
			if total > mirrorFetchLimit {
				return fmt.Errorf("remote session bundle exceeds size limit")
			}
		default:
			// Symlinks/others from a sessions dir are not expected; skip.
		}
	}
}

// FetchRemoteSessionLog reads a single remote session log (jsonl) — used by
// the lazy pull hook when a mirrored session is opened but its local log is
// missing.
func FetchRemoteSessionLog(ctx context.Context, fetch mirrorFetcher, metaDir, key string) ([]byte, error) {
	if fetch == nil {
		return nil, fmt.Errorf("mirror fetcher required")
	}
	if !IsValidSessionKey(key) {
		return nil, fmt.Errorf("invalid session key: %q", key)
	}
	metaDir = strings.TrimSpace(metaDir)
	if metaDir == "" {
		return nil, fmt.Errorf("remote meta dir required")
	}
	out, err := fetch(ctx, "cat "+Quote(path.Join(metaDir, "sessions", key+".jsonl")))
	if err != nil {
		return nil, fmt.Errorf("拉取远程会话日志失败: %w", err)
	}
	return out, nil
}
