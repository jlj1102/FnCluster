package api

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"fncluster/internal/admin"
	"fncluster/internal/cluster"
	"fncluster/internal/mesh"
)

func testHandler(t *testing.T) (http.Handler, *admin.Admin) {
	t.Helper()
	dir := t.TempDir()
	state := cluster.New(cluster.Node{ID: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", Online: true})
	adminOps := &admin.Admin{DataDir: dir, Entry: filepath.Join(dir, "index.html"), MeshPort: "38671"}
	if err := os.WriteFile(adminOps.Entry, []byte("<html></html>"), 0644); err != nil {
		t.Fatal(err)
	}
	return New(state, adminOps, mesh.NewRuntime(dir, "127.0.0.1:0", state, time.Hour)), adminOps
}

func TestMutationRequiresGuardHeader(t *testing.T) {
	handler, adminOps := testHandler(t)
	request := httptest.NewRequest(http.MethodPost, "/api/v1/mesh/init", strings.NewReader(`{"advertise":"127.0.0.1:38671"}`))
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d: %s", response.Code, response.Body.String())
	}
	if _, err := os.Stat(filepath.Join(adminOps.DataDir, "mesh.json")); !os.IsNotExist(err) {
		t.Fatal("mesh state was written without the guard header")
	}
}

func TestMutationInitStatusAndAudit(t *testing.T) {
	handler, adminOps := testHandler(t)
	request := httptest.NewRequest(http.MethodPost, "/api/v1/mesh/init", strings.NewReader(`{"advertise":"127.0.0.1:38671"}`))
	request.Header.Set("X-FnCluster", "1")
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("init returned %d: %s", response.Code, response.Body.String())
	}
	request = httptest.NewRequest(http.MethodGet, "/api/v1/mesh", nil)
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"configured":true`) {
		t.Fatalf("mesh status: %d %s", response.Code, response.Body.String())
	}
	if _, err := os.Stat(filepath.Join(adminOps.DataDir, "audit.log")); err != nil {
		t.Fatalf("audit log missing: %v", err)
	}
	request = httptest.NewRequest(http.MethodGet, "/api/v1/webui", nil)
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), "UNSUPPORTED") {
		t.Fatalf("webui status: %d %s", response.Code, response.Body.String())
	}
	request = httptest.NewRequest(http.MethodGet, "/api/v1/net/interfaces", nil)
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"38671"`) {
		t.Fatalf("interfaces: %d %s", response.Code, response.Body.String())
	}
	if !strings.Contains(response.Body.String(), `"addresses"`) {
		t.Fatalf("interfaces missing addresses: %s", response.Body.String())
	}
}

func TestPanelsReportMapsLocalizedEntries(t *testing.T) {
	handler, _ := testHandler(t)
	body := `{
	  "language": "zh-CN",
	  "entries": [
	    {"entryKey":"app:trim.setting","appName":"trim.setting","title":"系统设置","type":"inbuilt","nativeApp":true,"isDisplay":true},
	    {"entryKey":"app:trim.photos","appName":"trim.photos","title":"相册","type":"inbuilt","nativeApp":true,"isDisplay":true},
	    {"entryKey":"entry:hidden","appName":"trim.preview","title":"Preview","type":"inbuilt","nativeApp":true,"isDisplay":false},
	    {"entryKey":"entry:fncluster","appName":"fncluster","title":"FnCluster","type":"iframe","fullUrl":"/app/fncluster/","isDisplay":true},
	    {"entryKey":"entry:external","appName":"someapp","title":"External","type":"url","fullUrl":"https://example.com/app","isDisplay":true},
	    {"entryKey":"fncluster:node.panel","appName":"fncluster.remote.node.panel","title":"系统设置 · FN02","type":"iframe","fullUrl":"/app/fncluster/?node=node","isDisplay":true,"source":"fncluster"}
	  ]
	}`
	request := httptest.NewRequest(http.MethodPost, "/api/v1/panels/report", strings.NewReader(body))
	request.Header.Set("X-FnCluster", "1")
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("report returned %d: %s", response.Code, response.Body.String())
	}
	if !strings.Contains(response.Body.String(), `"count":4`) {
		t.Fatalf("synthetic entry was stored: %s", response.Body.String())
	}
	request = httptest.NewRequest(http.MethodGet, "/api/v1/panels", nil)
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	out := response.Body.String()
	for _, want := range []string{
		`"title":"系统设置"`, `"kind":"system"`,
		`"title":"FnCluster"`, `"kind":"application"`, `"directUrl":"/app/fncluster/"`,
		`"kind":"external"`, `"externalUrl":"https://example.com/app"`,
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("panels missing %q: %s", want, out)
		}
	}
	if strings.Contains(out, "Preview") {
		t.Fatal("hidden entry was stored in the catalog")
	}
	if strings.Contains(out, "fncluster.remote") || strings.Contains(out, "系统设置 · FN02") {
		t.Fatalf("synthetic all-apps entry leaked into the catalog: %s", out)
	}

	request = httptest.NewRequest(http.MethodPost, "/api/v1/panels/report", strings.NewReader(`{"entries":[{"entryKey":"x","title":"X"}]}`))
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusForbidden {
		t.Fatalf("unguarded report returned %d", response.Code)
	}
}

func TestMutationResetIdentity(t *testing.T) {
	handler, adminOps := testHandler(t)
	request := httptest.NewRequest(http.MethodPost, "/api/v1/mesh/init", strings.NewReader(`{"advertise":"127.0.0.1:38671"}`))
	request.Header.Set("X-FnCluster", "1")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("init: %d %s", response.Code, response.Body.String())
	}
	request = httptest.NewRequest(http.MethodPost, "/api/v1/mesh/reset-identity", strings.NewReader(`{}`))
	request.Header.Set("X-FnCluster", "1")
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("reset: %d %s", response.Code, response.Body.String())
	}
	if _, err := os.Stat(filepath.Join(adminOps.DataDir, "mesh.json")); !os.IsNotExist(err) {
		t.Fatal("mesh.json survived reset")
	}
	request = httptest.NewRequest(http.MethodGet, "/api/v1/webui", nil)
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if !strings.Contains(response.Body.String(), `"auditLog"`) {
		t.Fatalf("webui status misses auditLog: %s", response.Body.String())
	}
}
