package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"mindfs/server/internal/preferences"
)

func newPrefsTestStore(t *testing.T) *preferences.Store {
	t.Helper()
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(t.TempDir(), "config"))
	store, err := preferences.NewStore()
	if err != nil {
		t.Fatal(err)
	}
	return store
}

func TestPrefsSyncEndpointGuardRequiresTokenAndLoopback(t *testing.T) {
	handler := &HTTPHandler{LocalCLIToken: "s3cret"}
	probe := handler.prefsSyncEndpoint(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	// No token.
	req := httptest.NewRequest(http.MethodGet, "/api/preferences/export", nil)
	rec := httptest.NewRecorder()
	probe(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("missing token: got %d, want 403", rec.Code)
	}

	// Right token, not loopback.
	req = httptest.NewRequest(http.MethodGet, "/api/preferences/export", nil)
	req.Header.Set(localCLIHeaderName, "s3cret")
	req.RemoteAddr = "100.118.126.86:54321"
	rec = httptest.NewRecorder()
	probe(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("non-loopback: got %d, want 403", rec.Code)
	}

	// Right token, loopback.
	req = httptest.NewRequest(http.MethodGet, "/api/preferences/export", nil)
	req.Header.Set(localCLIHeaderName, "s3cret")
	req.RemoteAddr = "127.0.0.1:54321"
	rec = httptest.NewRecorder()
	probe(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("loopback with token: got %d, want 200", rec.Code)
	}

	// Empty server token must fail closed even on loopback.
	unguarded := (&HTTPHandler{}).prefsSyncEndpoint(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	req = httptest.NewRequest(http.MethodGet, "/api/preferences/export", nil)
	req.Header.Set(localCLIHeaderName, "")
	req.RemoteAddr = "127.0.0.1:1"
	rec = httptest.NewRecorder()
	unguarded(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("empty server token: got %d, want 403", rec.Code)
	}
}

func TestClientUIPreferenceRoundTripAndObjectGate(t *testing.T) {
	store := newPrefsTestStore(t)
	handler := &HTTPHandler{AppContext: &AppContext{Prefs: store}}

	rec := httptest.NewRecorder()
	handler.handleClientUIPreferenceGet(rec, httptest.NewRequest(http.MethodGet, "/api/preferences/client-ui", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("get: %d", rec.Code)
	}
	var fetched map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &fetched); err != nil {
		t.Fatal(err)
	}
	if fetched["configured"] != false || fetched["data"] != nil {
		t.Fatalf("fresh store must be unconfigured: %v", fetched)
	}

	// Arrays and scalars are rejected; only JSON objects pass.
	rec = httptest.NewRecorder()
	body := strings.NewReader(`{"data":[1,2]}`)
	req := httptest.NewRequest(http.MethodPut, "/api/preferences/client-ui", body)
	handler.handleClientUIPreferencePut(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("array data: got %d, want 400", rec.Code)
	}

	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPut, "/api/preferences/client-ui",
		strings.NewReader(`{"data":{"v":1,"appearance":"moss","fontSizes":{"main":1.2}}}`))
	handler.handleClientUIPreferencePut(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("put: %d %s", rec.Code, rec.Body.String())
	}

	// Export carries the blob and a hash; a stale import is rejected 409-equivalent.
	rec = httptest.NewRecorder()
	handler.handlePreferencesExport(rec, httptest.NewRequest(http.MethodGet, "/api/preferences/export", nil))
	var exported preferencesPreferencesExportProbe
	if err := json.Unmarshal(rec.Body.Bytes(), &exported); err != nil {
		t.Fatal(err)
	}
	if exported.Hash == "" || exported.Block.SyncedAtMs == 0 {
		t.Fatalf("export must carry hash and timestamp: %+v", exported)
	}

	rec = httptest.NewRecorder()
	handler.handlePreferencesImport(rec, httptest.NewRequest(http.MethodPut, "/api/preferences/import",
		strings.NewReader(`{"hash":"x","block":{"synced_at_ms":1,"client_ui":{"v":1,"appearance":"dark"}}}`)))
	if rec.Code != http.StatusConflict {
		t.Fatalf("stale import: got %d, want 409", rec.Code)
	}
}

type preferencesPreferencesExportProbe struct {
	Hash  string `json:"hash"`
	Block struct {
		SyncedAtMs int64 `json:"synced_at_ms"`
	} `json:"block"`
}
