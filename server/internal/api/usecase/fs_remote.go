package usecase

import (
	"context"
	"errors"
	"strings"

	"mindfs/internal/remote"
	"mindfs/server/internal/fs"
)

// errDeviceReadOnly is returned for every mutating file operation on a
// device-backed root.
var errDeviceReadOnly = errors.New("远程设备目录只读，本期仅支持浏览")

// resolveRemoteDevice looks up the runtime and device backing a device root.
func resolveRemoteDevice(s *Service, root fs.RootInfo) (DeviceRuntime, remote.Device, error) {
	rt, ok := s.Registry.GetDeviceRuntime()
	if !ok {
		return DeviceRuntime{}, remote.Device{}, errors.New("远程设备功能未启用")
	}
	device, ok := rt.Store.Get(root.Device)
	if !ok {
		return DeviceRuntime{}, remote.Device{}, errors.New("设备不存在: " + root.Device)
	}
	return rt, device, nil
}

// normalizeRemoteRelPath mirrors NormalizePath for device roots (slash
// semantics): strips the "#fragment" suffix, a leading slash and a
// root-prefixed absolute path. Escapes are rejected by fs.validateRemoteRelPath.
func normalizeRemoteRelPath(rootPath, input string) string {
	rel := strings.SplitN(strings.TrimSpace(input), "#", 2)[0]
	root := strings.TrimSuffix(strings.TrimSpace(rootPath), "/")
	if root != "" {
		if rel == root {
			return "."
		}
		rel = strings.TrimPrefix(rel, root+"/")
	} else {
		rel = strings.TrimPrefix(rel, "/")
	}
	return rel
}

func (s *Service) listTreeDeviceRoot(ctx context.Context, root fs.RootInfo, dir string) (ListTreeOutput, error) {
	rt, device, err := resolveRemoteDevice(s, root)
	if err != nil {
		return ListTreeOutput{}, err
	}
	// 设备根不建本地 watcher：fsnotify 无法监听远端路径。
	entries, err := fs.RemoteListEntries(ctx, device, rt.ConfigDir, root.RootPath, normalizeRemoteRelPath(root.RootPath, dir))
	if err != nil {
		return ListTreeOutput{}, err
	}
	return ListTreeOutput{Entries: entries}, nil
}

func (s *Service) readFileDeviceRoot(ctx context.Context, root fs.RootInfo, inputPath string, in ReadFileInput) (ReadFileOutput, error) {
	rt, device, err := resolveRemoteDevice(s, root)
	if err != nil {
		return ReadFileOutput{}, err
	}
	result, err := fs.RemoteReadResult(ctx, device, rt.ConfigDir, root.RootPath, normalizeRemoteRelPath(root.RootPath, inputPath), in.MaxBytes, in.Cursor, in.ReadMode)
	if err != nil {
		return ReadFileOutput{}, err
	}
	result.Root = root.ID
	return ReadFileOutput{File: result}, nil
}

func (s *Service) getFileInfoDeviceRoot(ctx context.Context, root fs.RootInfo, inputPath string) (GetFileInfoOutput, error) {
	rt, device, err := resolveRemoteDevice(s, root)
	if err != nil {
		return GetFileInfoOutput{}, err
	}
	rel := normalizeRemoteRelPath(root.RootPath, inputPath)
	size, mtime, err := fs.RemoteStatFile(ctx, device, rt.ConfigDir, root.RootPath, rel)
	if err != nil {
		return GetFileInfoOutput{}, err
	}
	return GetFileInfoOutput{
		Path:  rel,
		Name:  baseName(rel),
		Size:  size,
		MTime: mtime,
	}, nil
}

// rejectDeviceRoot returns errDeviceReadOnly when the root is device-backed.
func rejectDeviceRoot(root fs.RootInfo) error {
	if strings.TrimSpace(root.Device) != "" {
		return errDeviceReadOnly
	}
	return nil
}

// rejectDeviceRootByPath rejects mutations addressed by root path (before the
// registry entry is touched) when the path belongs to a device root.
func rejectDeviceRootByPath(s *Service, path string) error {
	for _, existing := range s.Registry.ListRoots() {
		if existing.Device != "" && sameManagedDirPath(existing.RootPath, path) {
			return errDeviceReadOnly
		}
	}
	return nil
}

// baseName returns the last slash-separated element of a remote rel path.
func baseName(rel string) string {
	if rel == "" || rel == "." {
		return rel
	}
	rel = strings.TrimSuffix(rel, "/")
	if idx := strings.LastIndex(rel, "/"); idx >= 0 {
		return rel[idx+1:]
	}
	return rel
}
