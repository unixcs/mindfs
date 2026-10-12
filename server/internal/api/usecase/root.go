package usecase

import (
	"errors"

	"mindfs/internal/remote"
	"mindfs/server/internal/agent"
	agenttypes "mindfs/server/internal/agent/types"
	"mindfs/server/internal/fs"
	"mindfs/server/internal/preferences"
	"mindfs/server/internal/session"
)

// DeviceRuntime carries the remote device store and the MindFS config dir
// (which locates devices.json and the ssh mux dir) into usecase services.
type DeviceRuntime struct {
	Store     *remote.Store
	ConfigDir string
}

type Registry interface {
	GetRoot(rootID string) (fs.RootInfo, error)
	GetSessionManager(rootID string) (*session.Manager, error)
	UpsertRoot(path string) (fs.RootInfo, error)
	RemoveRoot(path string) (fs.RootInfo, error)
	RenameRoot(rootID, name, rootPath string) (fs.RootInfo, error)
	ListRoots() []fs.RootInfo
	GetAgentPool() *agent.Pool
	GetPreferences() *preferences.Store
	GetExternalSessionImporter(agentName string) (agenttypes.ExternalSessionImporter, error)
	GetProber() *agent.Prober
	GetCandidateRegistry() *CandidateRegistry
	GetFileWatcher(rootID string, manager *session.Manager) (*fs.SharedFileWatcher, error)
	ReleaseFileWatcher(rootID, sessionKey string)
	GetDeviceRuntime() (DeviceRuntime, bool)
}

type Service struct {
	Registry Registry
}

func (s *Service) ensureRegistry() error {
	if s == nil || s.Registry == nil {
		return errors.New("services not configured")
	}
	return nil
}

// ensureWritableLocalRoot rejects mutations aimed at a device root. A device
// root's RootPath names a directory on the remote device; running local file
// or git mutations against it would silently hit a same-named local directory
// (common on a multi-machine Tailscale network). Device file trees are
// read-only in this phase; edits belong on the device's own MindFS panel.
func (s *Service) ensureWritableLocalRoot(rootID string) (fs.RootInfo, error) {
	root, err := s.Registry.GetRoot(rootID)
	if err != nil {
		return fs.RootInfo{}, err
	}
	if err := rejectDeviceRoot(root); err != nil {
		return fs.RootInfo{}, err
	}
	return root, nil
}
