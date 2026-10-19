package api

import (
	"bytes"
	"crypto/subtle"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"strings"

	"mindfs/server/internal/preferences"
)

const maxClientUIBodyBytes = 1 << 17 // 128 KiB

type clientUIPreferenceRequest struct {
	Data json.RawMessage `json:"data"`
}

func clientUIPreferenceResponse(store *preferences.Store) map[string]any {
	raw, configured := store.ClientUI()
	var data any
	if configured {
		data = json.RawMessage(raw)
	}
	return map[string]any{"configured": configured, "data": data}
}

func (h *HTTPHandler) handleClientUIPreferenceGet(w http.ResponseWriter, _ *http.Request) {
	if h.AppContext == nil || h.AppContext.GetPreferences() == nil {
		respondError(w, http.StatusServiceUnavailable, errInvalidRequest("preferences not configured"))
		return
	}
	respondJSON(w, http.StatusOK, clientUIPreferenceResponse(h.AppContext.GetPreferences()))
}

func (h *HTTPHandler) handleClientUIPreferencePut(w http.ResponseWriter, r *http.Request) {
	if h.AppContext == nil || h.AppContext.GetPreferences() == nil {
		respondError(w, http.StatusServiceUnavailable, errInvalidRequest("preferences not configured"))
		return
	}
	var req clientUIPreferenceRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, maxClientUIBodyBytes)).Decode(&req); err != nil {
		respondError(w, http.StatusRequestEntityTooLarge, errInvalidRequest("invalid client ui payload"))
		return
	}
	trimmed := bytes.TrimSpace(req.Data)
	if len(trimmed) == 0 || trimmed[0] != '{' || !json.Valid(trimmed) {
		respondError(w, http.StatusBadRequest, errInvalidRequest("data must be a JSON object"))
		return
	}
	if err := h.AppContext.GetPreferences().UpdateClientUI(trimmed); err != nil {
		respondError(w, http.StatusBadRequest, errInvalidRequest(err.Error()))
		return
	}
	respondJSON(w, http.StatusOK, clientUIPreferenceResponse(h.AppContext.GetPreferences()))
}

type preferencesExportResponse struct {
	Hash  string                        `json:"hash"`
	Block preferences.SyncedPreferences `json:"block"`
}

type preferencesImportRequest struct {
	Hash  string                        `json:"hash"`
	Block preferences.SyncedPreferences `json:"block"`
}

func (h *HTTPHandler) handlePreferencesExport(w http.ResponseWriter, _ *http.Request) {
	if h.AppContext == nil || h.AppContext.GetPreferences() == nil {
		respondError(w, http.StatusServiceUnavailable, errInvalidRequest("preferences not configured"))
		return
	}
	block, hash := h.AppContext.GetPreferences().ExportSynced()
	respondJSON(w, http.StatusOK, preferencesExportResponse{Hash: hash, Block: block})
}

func (h *HTTPHandler) handlePreferencesImport(w http.ResponseWriter, r *http.Request) {
	if h.AppContext == nil || h.AppContext.GetPreferences() == nil {
		respondError(w, http.StatusServiceUnavailable, errInvalidRequest("preferences not configured"))
		return
	}
	var req preferencesImportRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil {
		respondError(w, http.StatusBadRequest, errInvalidRequest("invalid preferences import"))
		return
	}
	applied, stale, err := h.AppContext.GetPreferences().ApplySynced(req.Block)
	if err != nil {
		respondError(w, http.StatusBadRequest, errInvalidRequest(err.Error()))
		return
	}
	if stale {
		respondError(w, http.StatusConflict, errInvalidRequest("incoming preferences are older than local"))
		return
	}
	respondJSON(w, http.StatusOK, map[string]any{"applied": applied})
}

// prefsSyncEndpoint guards the whole-preference export/import pair. These are
// intentionally NOT browser endpoints: they are reachable only over loopback
// with the local CLI token, which the reconciler script reads on each machine.
// The anonymous protectedEndpoint relaxation (e2ee disabled) must not extend
// to bulk preference overwrites.
func (h *HTTPHandler) prefsSyncEndpoint(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		token := strings.TrimSpace(h.LocalCLIToken)
		if token == "" || subtle.ConstantTimeCompare([]byte(strings.TrimSpace(r.Header.Get(localCLIHeaderName))), []byte(token)) != 1 {
			respondError(w, http.StatusForbidden, errInvalidRequest("local cli token required"))
			return
		}
		host, _, err := net.SplitHostPort(strings.TrimSpace(r.RemoteAddr))
		if err != nil {
			host = strings.TrimSpace(r.RemoteAddr)
		}
		ip := net.ParseIP(host)
		if ip == nil || !ip.IsLoopback() {
			respondError(w, http.StatusForbidden, errInvalidRequest("loopback only"))
			return
		}
		next(w, r)
	}
}
