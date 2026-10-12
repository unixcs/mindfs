package fs

import (
	"bytes"
	"context"
	crand "crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"mime"
	"os"
	"path"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"mindfs/internal/remote"
	"mindfs/server/internal/apperr"
)

// remoteOpTimeout bounds a single SSH file operation (see design: connect 8s
// via ssh args, command 120s).
const remoteOpTimeout = 120 * time.Second

// validateRemoteRelPath joins relPath onto the remote absolute rootPath with
// slash semantics and rejects escapes: absolute inputs, ".." leaving the root
// and empty roots. It returns the cleaned absolute remote path.
func validateRemoteRelPath(rootPath, relPath string) (string, error) {
	if strings.TrimSpace(rootPath) == "" {
		return "", errors.New("root required")
	}
	root := path.Clean("/" + strings.TrimSpace(rootPath))
	rel := strings.TrimSpace(relPath)
	if rel == "" || rel == "." {
		return root, nil
	}
	if strings.HasPrefix(rel, "/") {
		return "", errors.New("absolute path not allowed")
	}
	abs := path.Clean(path.Join(root, rel))
	trimmed := strings.TrimSuffix(root, "/")
	if abs != trimmed && !strings.HasPrefix(abs, trimmed+"/") {
		return "", errors.New("path outside root")
	}
	return abs, nil
}

// remoteRelFromAbs maps an absolute remote path back to the slash path
// relative to root ("." for the root itself).
func remoteRelFromAbs(rootPath, abs string) string {
	trimmed := strings.TrimSuffix(path.Clean("/"+strings.TrimSpace(rootPath)), "/")
	if abs == trimmed {
		return "."
	}
	return strings.TrimPrefix(strings.TrimPrefix(abs, trimmed), "/")
}

// parseRemoteLs parses `LC_ALL=C ls -Alp --time-style=+%s <dir>` output.
// Line layout: perms links owner group size epoch name...; the name is
// re-joined from the 7th field on to tolerate spaces. Locale-dependent
// "total" lines and malformed lines are skipped. Path is filled by the
// caller (it depends on the listed directory); here it defaults to Name.
func parseRemoteLs(output string) []Entry {
	entries := make([]Entry, 0, 32)
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimRight(line, "\r")
		if strings.TrimSpace(line) == "" {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 7 {
			continue
		}
		perms := fields[0]
		if len(perms) < 10 {
			continue
		}
		epoch, err := strconv.ParseInt(fields[5], 10, 64)
		if err != nil {
			continue
		}
		name := strings.Join(fields[6:], " ")
		isDir := perms[0] == 'd'
		isSymlink := perms[0] == 'l'
		if isDir && strings.HasSuffix(name, "/") {
			// -p appends a slash to directories; local entries carry none.
			name = strings.TrimSuffix(name, "/")
		}
		if isSymlink {
			// Keep the link itself (local semantics), not its target.
			if idx := strings.Index(name, " -> "); idx >= 0 {
				name = name[:idx]
			}
		}
		if name == "" || name == "/" {
			continue
		}
		size, _ := strconv.ParseInt(fields[4], 10, 64)
		entries = append(entries, Entry{
			Name:      name,
			Path:      name,
			IsDir:     isDir,
			IsSymlink: isSymlink,
			Size:      size,
			MTime:     time.Unix(epoch, 0).UTC().Format(time.RFC3339Nano),
		})
	}
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].IsDir != entries[j].IsDir {
			return entries[i].IsDir
		}
		return entries[i].Name < entries[j].Name
	})
	return entries
}

// RemoteListEntries lists one remote directory via SSH. Paths are root
// relative and slash style; sorting matches RootInfo.ListEntries (dirs
// first, then by name).
func RemoteListEntries(ctx context.Context, device remote.Device, configDir, rootPath, relPath string) ([]Entry, error) {
	abs, err := validateRemoteRelPath(rootPath, relPath)
	if err != nil {
		return nil, err
	}
	muxDir, err := remote.MuxDir(configDir)
	if err != nil {
		return nil, err
	}
	remoteCmd := "LC_ALL=C ls -Alp --time-style=+%s -- " + remote.Quote(abs)
	out, err := remote.Run(ctx, device.Target(), muxDir, remoteCmd, remoteOpTimeout)
	if err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "no such file or directory") {
			return nil, apperr.Wrap("list", abs, os.ErrNotExist)
		}
		return nil, err
	}
	entries := parseRemoteLs(string(out))
	if base := remoteRelFromAbs(rootPath, abs); base != "." {
		for i := range entries {
			entries[i].Path = base + "/" + entries[i].Name
		}
	}
	return entries, nil
}

type remoteFileHeader struct {
	size  int64
	mtime int64 // epoch seconds
	isDir bool
}

// The read protocol prefixes the file bytes with `MFS_SZ:<nonce>:<size>|<mtime>|<type>`.
// The nonce is per-read random: a device's login profile (motd, shell rc) that
// happens to print an MFS_SZ-looking line can no longer desync every remote
// read; only the echo our own command produced matches.
func newRemoteSizeMarker() []byte {
	var b [8]byte
	if _, err := crand.Read(b[:]); err != nil {
		binary.LittleEndian.PutUint64(b[:], uint64(time.Now().UnixNano()))
	}
	return []byte(fmt.Sprintf("MFS_SZ:%s:", hex.EncodeToString(b[:])))
}

// splitRemoteReadOutput splits `echo "MFS_SZ:<nonce>:<size>|<mtime>|<type>"; tail|head`
// output into the header and the raw file bytes. Profile noise before the
// marker line is tolerated; the marker line must carry the expected nonce.
func splitRemoteReadOutput(output, marker []byte) (remoteFileHeader, []byte, error) {
	for start := 0; start < len(output); {
		line := output[start:]
		if nl := bytes.IndexByte(line, '\n'); nl >= 0 {
			line = line[:nl]
			if bytes.HasPrefix(line, marker) {
				header, err := parseRemoteFileHeader(string(line[len(marker):]))
				if err != nil {
					return remoteFileHeader{}, nil, err
				}
				return header, output[start+nl+1:], nil
			}
			start += nl + 1
			continue
		}
		if bytes.HasPrefix(line, marker) {
			header, err := parseRemoteFileHeader(string(line[len(marker):]))
			if err != nil {
				return remoteFileHeader{}, nil, err
			}
			return header, nil, nil
		}
		break
	}
	return remoteFileHeader{}, nil, errors.New("远程读取失败：输出缺少 MFS_SZ 标记")
}

func parseRemoteFileHeader(spec string) (remoteFileHeader, error) {
	parts := strings.SplitN(spec, "|", 3)
	if len(parts) < 3 {
		return remoteFileHeader{}, fmt.Errorf("远程读取失败：无法解析文件头 %q", spec)
	}
	size, err := strconv.ParseInt(strings.TrimSpace(parts[0]), 10, 64)
	if err != nil {
		size = -1
	}
	mtime, err := strconv.ParseInt(strings.TrimSpace(parts[1]), 10, 64)
	if err != nil {
		mtime = 0
	}
	return remoteFileHeader{size: size, mtime: mtime, isDir: strings.EqualFold(strings.TrimSpace(parts[2]), "directory")}, nil
}

// runRemoteRead streams one remote file slice in a single SSH round trip.
// size < 0 means the file does not exist remotely (os.ErrNotExist).
func runRemoteRead(ctx context.Context, device remote.Device, muxDir, abs string, offset, maxBytes int64) (remoteFileHeader, []byte, error) {
	marker := newRemoteSizeMarker()
	remoteCmd := "f=" + remote.Quote(abs) +
		"; s=$(stat -c %s \"$f\" 2>/dev/null || echo -1)" +
		"; m=$(stat -c %Y \"$f\" 2>/dev/null || echo 0)" +
		"; t=$(stat -c %F \"$f\" 2>/dev/null || echo missing)" +
		"; echo \"" + string(marker) + "$s|$m|$t\"" +
		"; if [ \"$s\" -ge 0 ]; then tail -c +" + strconv.FormatInt(offset+1, 10) +
		" -- \"$f\" 2>/dev/null | head -c " + strconv.FormatInt(maxBytes, 10) + "; fi"
	out, err := remote.Run(ctx, device.Target(), muxDir, remoteCmd, remoteOpTimeout)
	if err != nil {
		return remoteFileHeader{}, nil, err
	}
	header, content, err := splitRemoteReadOutput(out, marker)
	if err != nil {
		return header, nil, err
	}
	if header.size < 0 {
		return header, nil, apperr.Wrap("stat", abs, os.ErrNotExist)
	}
	if header.isDir {
		return header, nil, errors.New("path is a directory")
	}
	if maxBytes > 0 && int64(len(content)) > maxBytes {
		content = content[:maxBytes]
	}
	return header, content, nil
}

// RemoteReadFile reads up to maxBytes of a remote file starting at byte
// offset. It is the transport-only primitive: use RemoteReadResult for the
// full ReadResult semantics (decoding, truncation, cursor).
func RemoteReadFile(ctx context.Context, device remote.Device, configDir, rootPath, relPath string, offset, maxBytes int64) ([]byte, int64, error) {
	abs, err := validateRemoteRelPath(rootPath, relPath)
	if err != nil {
		return nil, 0, err
	}
	if offset < 0 {
		return nil, 0, errors.New("cursor must be non-negative")
	}
	if maxBytes < 0 {
		maxBytes = 0
	}
	muxDir, err := remote.MuxDir(configDir)
	if err != nil {
		return nil, 0, err
	}
	header, content, err := runRemoteRead(ctx, device, muxDir, abs, offset, maxBytes)
	if err != nil {
		return nil, 0, err
	}
	return content, header.size, nil
}

// bomFromHead mirrors readFileBOM for an in-memory head of the file.
func bomFromHead(content []byte) string {
	if len(content) >= 2 && content[0] == 0xFF && content[1] == 0xFE {
		return "utf16le"
	}
	if len(content) >= 2 && content[0] == 0xFE && content[1] == 0xFF {
		return "utf16be"
	}
	if len(content) >= 3 && content[0] == 0xEF && content[1] == 0xBB && content[2] == 0xBF {
		return "utf8bom"
	}
	return ""
}

// RemoteReadResult maps a remote read onto the exact ReadResult semantics of
// RootInfo.ReadFile: same cursor rules (NextCursor = cursor + returned
// bytes), same text/binary/legacy encoding chain, same truncation rule.
// Revision stays empty, matching the local plain read (only the editable
// flow assigns a hash revision, which device roots reject anyway).
func RemoteReadResult(ctx context.Context, device remote.Device, configDir, rootPath, relPath string, maxBytes, cursor int64, readMode string) (ReadResult, error) {
	if cursor < 0 {
		return ReadResult{}, errors.New("cursor must be non-negative")
	}
	if readMode == "" {
		readMode = "incremental"
	}
	if readMode != "full" && readMode != "incremental" {
		return ReadResult{}, errors.New("invalid read mode")
	}
	if readMode == "incremental" && maxBytes <= 0 {
		maxBytes = defaultMaxReadBytes
	}
	abs, err := validateRemoteRelPath(rootPath, relPath)
	if err != nil {
		return ReadResult{}, err
	}
	readBound := maxBytes
	if readMode == "full" {
		readBound = maxFullReadBytes + 1
	}
	muxDir, err := remote.MuxDir(configDir)
	if err != nil {
		return ReadResult{}, err
	}
	header, content, err := runRemoteRead(ctx, device, muxDir, abs, cursor, readBound)
	if err != nil {
		return ReadResult{}, err
	}
	if readMode == "full" && (header.size > maxFullReadBytes || int64(len(content)) > maxFullReadBytes) {
		return ReadResult{}, errors.New("file is too large for full read")
	}
	ext := path.Ext(abs)
	bom := ""
	if cursor == 0 {
		bom = bomFromHead(content)
	}
	content = trimTailForDecoding(content, ext, bom)
	encoding := "utf-8"
	text := string(content)
	if !utf8.Valid(content) {
		if decoded, detected, ok := TryDecodeText(content, ext); ok {
			encoding = detected
			text = decoded
		} else {
			encoding = "binary"
			text = ""
		}
	}
	return ReadResult{
		Path:       remoteRelFromAbs(rootPath, abs),
		Name:       path.Base(abs),
		Content:    text,
		Encoding:   encoding,
		Truncated:  readMode != "full" && cursor+int64(len(content)) < header.size,
		NextCursor: cursor + int64(len(content)),
		Size:       header.size,
		Ext:        ext,
		Mime:       mime.TypeByExtension(ext),
		MTime:      time.Unix(header.mtime, 0).UTC(),
	}, nil
}

// RemoteStatFile stats one remote file (follows symlinks, like os.Stat).
func RemoteStatFile(ctx context.Context, device remote.Device, configDir, rootPath, relPath string) (int64, time.Time, error) {
	abs, err := validateRemoteRelPath(rootPath, relPath)
	if err != nil {
		return 0, time.Time{}, err
	}
	muxDir, err := remote.MuxDir(configDir)
	if err != nil {
		return 0, time.Time{}, err
	}
	remoteCmd := "LC_ALL=C stat -c '%s|%Y' -- " + remote.Quote(abs)
	out, err := remote.Run(ctx, device.Target(), muxDir, remoteCmd, remoteOpTimeout)
	if err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "no such file or directory") {
			return 0, time.Time{}, apperr.Wrap("stat", abs, os.ErrNotExist)
		}
		return 0, time.Time{}, err
	}
	line := strings.TrimSpace(strings.SplitN(string(out), "\n", 2)[0])
	parts := strings.Split(line, "|")
	if len(parts) != 2 {
		return 0, time.Time{}, fmt.Errorf("远程 stat %s 失败：无法解析输出 %q", abs, line)
	}
	size, err1 := strconv.ParseInt(parts[0], 10, 64)
	mtime, err2 := strconv.ParseInt(parts[1], 10, 64)
	if err1 != nil || err2 != nil {
		return 0, time.Time{}, fmt.Errorf("远程 stat %s 失败：无法解析输出 %q", abs, line)
	}
	return size, time.Unix(mtime, 0).UTC(), nil
}

// RemoteStatRoot verifies the device root directory exists remotely.
func RemoteStatRoot(ctx context.Context, device remote.Device, configDir, rootPath string) error {
	if strings.TrimSpace(rootPath) == "" {
		return errors.New("root required")
	}
	muxDir, err := remote.MuxDir(configDir)
	if err != nil {
		return err
	}
	remoteCmd := "test -d " + remote.Quote(path.Clean(strings.TrimSpace(rootPath)))
	if _, err := remote.Run(ctx, device.Target(), muxDir, remoteCmd, remoteOpTimeout); err != nil {
		return fmt.Errorf("远程目录不存在或不可访问: %s@%s:%s (%v)", device.User, device.Host, rootPath, err)
	}
	return nil
}
