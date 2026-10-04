package api

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"fncluster/internal/admin"
	"fncluster/internal/cluster"
	"fncluster/internal/mesh"
)

func TestReadOnlyAPI(t *testing.T) {
	state := cluster.New(cluster.Node{ID: "stable-id", Online: true})
	state.UpdateLocal(cluster.Metrics{CollectedAt: time.Now()})
	dir := t.TempDir()
	handler := New(state, &admin.Admin{DataDir: dir, Entry: filepath.Join(dir, "index.html")}, mesh.NewRuntime(dir, "127.0.0.1:0", state, time.Hour))
	request := httptest.NewRequest(http.MethodGet, "/api/v1/nodes", nil)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != 200 || !strings.Contains(response.Body.String(), "stable-id") {
		t.Fatalf("unexpected nodes response: %d %q", response.Code, response.Body.String())
	}
	request = httptest.NewRequest(http.MethodPost, "/api/v1/cluster", nil)
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST returned %d", response.Code)
	}
	request = httptest.NewRequest(http.MethodGet, "/app/fncluster/api/v1/cluster", nil)
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != 200 || !strings.Contains(response.Body.String(), "healthy") {
		t.Fatalf("prefixed API returned %d: %q", response.Code, response.Body.String())
	}
	request = httptest.NewRequest(http.MethodGet, "/app/fncluster/inject/loader.js", nil)
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != 200 || !strings.Contains(response.Body.String(), "fncluster-status") {
		t.Fatalf("loader returned %d: %q", response.Code, response.Body.String())
	}
	request = httptest.NewRequest(http.MethodGet, "/app/fncluster/inject/widget.js", nil)
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != 200 || !strings.Contains(response.Body.String(), "fncluster-widget-switch") {
		t.Fatalf("widget bridge returned %d: %q", response.Code, response.Body.String())
	}
	request = httptest.NewRequest(http.MethodGet, "/app/fncluster/api/v1/panels", nil)
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != 200 || !strings.Contains(response.Body.String(), "[") {
		t.Fatalf("panels returned %d: %q", response.Code, response.Body.String())
	}
	request = httptest.NewRequest(http.MethodGet, "/app/fncluster", nil)
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusPermanentRedirect || response.Header().Get("Location") != "/app/fncluster/" {
		t.Fatalf("app entry returned %d, location %q", response.Code, response.Header().Get("Location"))
	}
}
