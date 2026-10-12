package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"mindfs/internal/remote"
	"mindfs/server/internal/fs"
)

// withDeviceID injects the chi route param the URL-param handlers read; the
// tests call handlers directly, outside the router.
func withDeviceID(r *http.Request, id string) *http.Request {
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("id", id)
	return r.WithContext(context.WithValue(r.Context(), chi.RouteCtxKey, rctx))
}

// Device endpoints must answer 501 设备功能未启用 when the devices store is
// not wired (s.Devices == nil), on every route — never a silent 404/500.
func TestDeviceEndpointsWithoutStoreReturn501(t *testing.T) {
	handler := &HTTPHandler{AppContext: &AppContext{}}

	cases := []struct {
		name    string
		method  string
		path    string
		body    string
		handler func(w http.ResponseWriter, r *http.Request)
	}{
		{"list", http.MethodGet, "/api/devices", "", handler.handleDevicesList},
		{"create", http.MethodPost, "/api/devices", `{}`, handler.handleDeviceCreate},
		{"delete", http.MethodDelete, "/api/devices/jp", "", func(w http.ResponseWriter, r *http.Request) { handler.handleDeviceDelete(w, withDeviceID(r, "jp")) }},
		{"test", http.MethodPost, "/api/devices/jp/test", `{}`, func(w http.ResponseWriter, r *http.Request) { handler.handleDeviceTest(w, withDeviceID(r, "jp")) }},
		{"scan", http.MethodPost, "/api/devices/jp/scan", `{}`, func(w http.ResponseWriter, r *http.Request) { handler.handleDeviceScan(w, withDeviceID(r, "jp")) }},
		{"sync", http.MethodPost, "/api/devices/jp/sync", `{}`, func(w http.ResponseWriter, r *http.Request) { handler.handleDeviceSync(w, withDeviceID(r, "jp")) }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(tc.method, tc.path, strings.NewReader(tc.body))
			resp := httptest.NewRecorder()
			tc.handler(resp, req)
			if resp.Code != http.StatusNotImplemented {
				t.Fatalf("status = %d, want 501; body=%s", resp.Code, resp.Body.String())
			}
			if !strings.Contains(resp.Body.String(), "设备功能未启用") {
				t.Fatalf("body = %s, want 设备功能未启用", resp.Body.String())
			}
		})
	}
}

func newDeviceTestContext(t *testing.T) (*HTTPHandler, *fs.Registry, string) {
	t.Helper()
	configDir := t.TempDir()
	store, err := remote.NewStore(configDir)
	if err != nil {
		t.Fatal(err)
	}
	registry := fs.NewRegistry(filepath.Join(t.TempDir(), "registry.json"))
	return &HTTPHandler{AppContext: &AppContext{Devices: store, Dirs: registry}}, registry, configDir
}

func TestDeviceListShowsRootCount(t *testing.T) {
	handler, registry, _ := newDeviceTestContext(t)
	device := remote.Device{ID: "jp", Name: "JP Box", Host: "203.0.113.10", User: "dev", Enabled: true, AutoCreateAgents: true}
	if err := handler.AppContext.Devices.Save(device); err != nil {
		t.Fatal(err)
	}
	if _, err := registry.UpsertDeviceRoot(device, "/srv/projects/alpha"); err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/devices", nil)
	resp := httptest.NewRecorder()
	handler.handleDevicesList(resp, req)
	if resp.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", resp.Code, resp.Body.String())
	}
	var payload struct {
		Devices []struct {
			ID        string `json:"id"`
			Host      string `json:"host"`
			RootCount int    `json:"root_count"`
		} `json:"devices"`
	}
	if err := json.Unmarshal(resp.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if len(payload.Devices) != 1 {
		t.Fatalf("devices = %+v", payload.Devices)
	}
	if payload.Devices[0].ID != "jp" || payload.Devices[0].Host != "203.0.113.10" || payload.Devices[0].RootCount != 1 {
		t.Fatalf("device = %+v", payload.Devices[0])
	}
}

func TestDeviceCreateValidatesAndPersists(t *testing.T) {
	handler, _, _ := newDeviceTestContext(t)

	// Missing host/user is rejected by the store's validation.
	req := httptest.NewRequest(http.MethodPost, "/api/devices", strings.NewReader(`{"id":"jp"}`))
	resp := httptest.NewRecorder()
	handler.handleDeviceCreate(resp, req)
	if resp.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", resp.Code, resp.Body.String())
	}

	// A valid device is saved and echoed back with defaults applied.
	req = httptest.NewRequest(http.MethodPost, "/api/devices", strings.NewReader(`{"id":"jp","host":"203.0.113.10","user":"dev"}`))
	resp = httptest.NewRecorder()
	handler.handleDeviceCreate(resp, req)
	if resp.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", resp.Code, resp.Body.String())
	}
	var device map[string]any
	if err := json.Unmarshal(resp.Body.Bytes(), &device); err != nil {
		t.Fatal(err)
	}
	if device["id"] != "jp" || device["port"] != float64(22) {
		t.Fatalf("saved device = %+v", device)
	}
	if _, ok := handler.AppContext.Devices.Get("jp"); !ok {
		t.Fatal("device not persisted")
	}
}

func TestDeviceDeleteMissingReturns404(t *testing.T) {
	handler, _, _ := newDeviceTestContext(t)
	req := withDeviceID(httptest.NewRequest(http.MethodDelete, "/api/devices/nope", nil), "nope")
	resp := httptest.NewRecorder()
	handler.handleDeviceDelete(resp, req)
	if resp.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.Code)
	}
}

func TestDeviceSyncRejectsRootFromOtherDevice(t *testing.T) {
	handler, registry, _ := newDeviceTestContext(t)
	device := remote.Device{ID: "jp", Host: "203.0.113.10", User: "dev", MindFSHome: "/opt/mindfs-data", Enabled: true}
	if err := handler.AppContext.Devices.Save(device); err != nil {
		t.Fatal(err)
	}
	if _, err := registry.UpsertDeviceRoot(device, "/srv/projects/alpha"); err != nil {
		t.Fatal(err)
	}

	req := withDeviceID(httptest.NewRequest(http.MethodPost, "/api/devices/jp/sync", strings.NewReader(`{"root_id":"somewhere-else"}`)), "jp")
	resp := httptest.NewRecorder()
	handler.handleDeviceSync(resp, req)
	if resp.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", resp.Code, resp.Body.String())
	}

	// A root that belongs to the device passes the guard and fails later at
	// SSH discovery (unreachable host), which must surface as 502, not panic.
	req = withDeviceID(httptest.NewRequest(http.MethodPost, "/api/devices/jp/sync", strings.NewReader(`{"root_id":"jp-alpha"}`)), "jp")
	resp = httptest.NewRecorder()
	handler.handleDeviceSync(resp, req)
	if resp.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502; body=%s", resp.Code, resp.Body.String())
	}
}

func TestDeviceSyncWithoutMindFSHomeIs400(t *testing.T) {
	handler, _, _ := newDeviceTestContext(t)
	if err := handler.AppContext.Devices.Save(remote.Device{ID: "jp", Host: "203.0.113.10", User: "dev", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	req := withDeviceID(httptest.NewRequest(http.MethodPost, "/api/devices/jp/sync", strings.NewReader(`{}`)), "jp")
	resp := httptest.NewRecorder()
	handler.handleDeviceSync(resp, req)
	if resp.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", resp.Code, resp.Body.String())
	}
	if !strings.Contains(resp.Body.String(), "mindfsHome") {
		t.Fatalf("body = %s, want mindfsHome hint", resp.Body.String())
	}
}
