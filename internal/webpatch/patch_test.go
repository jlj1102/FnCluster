package webpatch

import (
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const (
	testHTML  = "<html><head><title>fnOS</title></head><body>original</body></html>"
	shellHTML = `<!doctype html><html><head><title>fnOS</title></head><body><div id="root"></div></body></html>`
)

func setup(t *testing.T) (string, string) {
	t.Helper()
	dir := t.TempDir()
	entry := filepath.Join(dir, "index.html")
	if err := os.WriteFile(entry, []byte(testHTML), 0644); err != nil {
		t.Fatal(err)
	}
	return entry, filepath.Join(dir, "state")
}

func markerCheck(entry string) func() error {
	return func() error {
		data, err := os.ReadFile(entry)
		if err != nil || !strings.Contains(string(data), BeginMarker) {
			return errors.New("loader missing")
		}
		return nil
	}
}

func TestInstallFileRetriesRename(t *testing.T) {
	targetDir := t.TempDir()
	staging := t.TempDir()
	target := filepath.Join(targetDir, "index.html")
	original := renameFile
	defer func() { renameFile = original }()
	calls := 0
	renameFile = func(old, new string) error {
		calls++
		if calls < 3 {
			return &os.LinkError{Op: "rename", Old: old, New: new, Err: os.ErrNotExist}
		}
		return os.Rename(old, new)
	}
	if err := installFile(target, []byte("patched"), 0644, staging); err != nil {
		t.Fatal(err)
	}
	if calls != 3 {
		t.Fatalf("rename attempts = %d", calls)
	}
	data, err := os.ReadFile(target)
	if err != nil || string(data) != "patched" {
		t.Fatalf("target not installed: %v %q", err, data)
	}
}

func TestApplyIdempotentAndRestore(t *testing.T) {
	entry, stateDir := setup(t)
	check := markerCheck(entry)
	for range 2 {
		if err := Apply(entry, stateDir, hash([]byte(testHTML)), "/app/fncluster/inject/loader.js?v=1", false, "", check); err != nil {
			t.Fatal(err)
		}
	}
	status, err := Status(entry, stateDir)
	if err != nil || status != "PATCHED" {
		t.Fatalf("status = %s, %v", status, err)
	}
	if err := Restore(entry, stateDir); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(entry)
	if string(data) != testHTML {
		t.Fatalf("original not restored: %q", data)
	}
	status, err = Status(entry, stateDir)
	if err != nil || status != "SAFE_MODE" {
		t.Fatalf("status = %s, %v", status, err)
	}
	if err := Apply(entry, stateDir, hash([]byte(testHTML)), "/app/fncluster/inject/loader.js", false, "", check); err == nil {
		t.Fatal("safe mode allowed patch")
	}
	if err := Enable(entry, stateDir, hash([]byte(testHTML))); err != nil {
		t.Fatal(err)
	}
	if err := Apply(entry, stateDir, hash([]byte(testHTML)), "/app/fncluster/inject/loader.js", false, "", check); err != nil {
		t.Fatalf("explicit re-enable did not permit checked apply: %v", err)
	}
}

func TestHealthFailureRollsBack(t *testing.T) {
	entry, stateDir := setup(t)
	err := Apply(entry, stateDir, hash([]byte(testHTML)), "/app/fncluster/inject/loader.js", false, "", func() error { return errors.New("unhealthy") })
	if err == nil {
		t.Fatal("expected health-check failure")
	}
	data, _ := os.ReadFile(entry)
	if string(data) != testHTML {
		t.Fatal("health-check failure did not restore original")
	}
}

func TestRestorePreservesFnOSUpdate(t *testing.T) {
	entry, stateDir := setup(t)
	if err := Apply(entry, stateDir, hash([]byte(testHTML)), "/app/fncluster/inject/loader.js", false, "", func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(entry)
	updated := strings.Replace(string(data), "original", "updated by fnOS", 1)
	if err := os.WriteFile(entry, []byte(updated), 0644); err != nil {
		t.Fatal(err)
	}
	if err := Restore(entry, stateDir); err != nil {
		t.Fatal(err)
	}
	data, _ = os.ReadFile(entry)
	if strings.Contains(string(data), BeginMarker) || !strings.Contains(string(data), "updated by fnOS") {
		t.Fatalf("fnOS update was lost or loader retained: %q", data)
	}
}

func TestUnsupportedHashDoesNotPatch(t *testing.T) {
	entry, stateDir := setup(t)
	if err := Apply(entry, stateDir, strings.Repeat("0", 64), "/app/fncluster/inject/loader.js", false, "", func() error { return nil }); err == nil {
		t.Fatal("unsupported entry was patched")
	}
	data, _ := os.ReadFile(entry)
	if string(data) != testHTML {
		t.Fatal("unsupported entry changed")
	}
}

func TestApplyCompatibleUnregisteredBuild(t *testing.T) {
	dir := t.TempDir()
	entry := filepath.Join(dir, "index.html")
	if err := os.WriteFile(entry, []byte(shellHTML), 0644); err != nil {
		t.Fatal(err)
	}
	stateDir := filepath.Join(dir, "state")
	check := markerCheck(entry)
	if err := Apply(entry, stateDir, hash([]byte(testHTML)), "/app/fncluster/inject/loader.js", false, "", check); err == nil {
		t.Fatal("unregistered build patched without compatibility confirmation")
	}
	compatible, err := UnregisteredBuild(entry)
	if err != nil || !compatible {
		t.Fatalf("structure not detected: %v %v", compatible, err)
	}
	if err := Apply(entry, stateDir, hash([]byte(testHTML)), "/app/fncluster/inject/loader.js", true, "", check); err != nil {
		t.Fatal(err)
	}
	status, err := Status(entry, stateDir)
	if err != nil || status != "PATCHED" {
		t.Fatalf("status = %s, %v", status, err)
	}
	if err := Restore(entry, stateDir); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(entry)
	if string(data) != shellHTML {
		t.Fatalf("original not restored: %q", data)
	}
}

func TestEnableCompatibleTrackedBuild(t *testing.T) {
	dir := t.TempDir()
	entry := filepath.Join(dir, "index.html")
	if err := os.WriteFile(entry, []byte(shellHTML), 0644); err != nil {
		t.Fatal(err)
	}
	stateDir := filepath.Join(dir, "state")
	if err := Enable(entry, stateDir, hash([]byte(testHTML))); err == nil {
		t.Fatal("compatible entry enabled without a patch record")
	}
	if err := Apply(entry, stateDir, hash([]byte(testHTML)), "/app/fncluster/inject/loader.js", true, "", markerCheck(entry)); err != nil {
		t.Fatal(err)
	}
	if err := Restore(entry, stateDir); err != nil {
		t.Fatal(err)
	}
	status, err := Status(entry, stateDir)
	if err != nil || status != "SAFE_MODE" {
		t.Fatalf("status = %s, %v", status, err)
	}
	if err := Enable(entry, stateDir, hash([]byte(testHTML))); err != nil {
		t.Fatalf("compatible build could not be re-enabled: %v", err)
	}
	if err := Apply(entry, stateDir, hash([]byte(testHTML)), "/app/fncluster/inject/loader.js", true, "", markerCheck(entry)); err != nil {
		t.Fatal(err)
	}
}

func TestApplyCompatibleRejectsBadStructure(t *testing.T) {
	entry, stateDir := setup(t)
	compatible, err := UnregisteredBuild(entry)
	if err != nil || compatible {
		t.Fatalf("structure falsely detected: %v %v", compatible, err)
	}
	if err := Apply(entry, stateDir, strings.Repeat("0", 64), "/app/fncluster/inject/loader.js", true, "", func() error { return nil }); err == nil {
		t.Fatal("bad structure was patched in compatible mode")
	}
	data, _ := os.ReadFile(entry)
	if string(data) != testHTML {
		t.Fatal("bad structure entry changed")
	}
}

func TestUnregisteredBuildRejectsMarkedEntry(t *testing.T) {
	dir := t.TempDir()
	entry := filepath.Join(dir, "index.html")
	marked := BeginMarker + "\n" + shellHTML
	if err := os.WriteFile(entry, []byte(marked), 0644); err != nil {
		t.Fatal(err)
	}
	compatible, err := UnregisteredBuild(entry)
	if err != nil || compatible {
		t.Fatalf("marked entry offered compatible mode: %v %v", compatible, err)
	}
}

func TestHealthCheckRetriesTransientFailure(t *testing.T) {
	const loader = "console.log('fncluster')"
	var indexRequests int32
	webui := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/" {
			if atomic.AddInt32(&indexRequests, 1) <= 2 {
				w.WriteHeader(http.StatusServiceUnavailable)
				return
			}
			_, _ = w.Write([]byte("<head>" + BeginMarker + "</head>"))
			return
		}
		_, _ = w.Write([]byte(loader))
	}))
	defer webui.Close()
	if err := CheckHTTPWithRetry(webui.URL, "/app/fncluster/inject/loader.js", []byte(loader), "/nonexistent.sock", 5, 10*time.Millisecond)(); err != nil {
		t.Fatal(err)
	}
	if atomic.LoadInt32(&indexRequests) < 3 {
		t.Fatal("health check did not retry the transient failure")
	}
}

func TestHealthCheckRetriesMissingMarker(t *testing.T) {
	const loader = "console.log('fncluster')"
	var indexRequests int32
	webui := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/" {
			if atomic.AddInt32(&indexRequests, 1) <= 2 {
				_, _ = w.Write([]byte("<html></html>"))
				return
			}
			_, _ = w.Write([]byte("<head>" + BeginMarker + "</head>"))
			return
		}
		_, _ = w.Write([]byte(loader))
	}))
	defer webui.Close()
	if err := CheckHTTPWithRetry(webui.URL, "/app/fncluster/inject/loader.js", []byte(loader), "/nonexistent.sock", 5, 10*time.Millisecond)(); err != nil {
		t.Fatal(err)
	}
}

func TestHealthCheckExhaustsRetries(t *testing.T) {
	webui := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer webui.Close()
	if err := CheckHTTPWithRetry(webui.URL, "/app/fncluster/inject/loader.js", []byte("x"), "/nonexistent.sock", 3, 5*time.Millisecond)(); err == nil {
		t.Fatal("persistent failure passed the health check")
	}
}

func TestHealthCheckAcceptsAuthWalledIndex(t *testing.T) {
	const loader = "console.log('fncluster')"
	dir, err := os.MkdirTemp(os.TempDir(), "fcs-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	socket := filepath.Join(dir, "s")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	local := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/app/fncluster/inject/loader.js" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(loader))
	})}
	defer local.Close()
	go local.Serve(listener)
	webui := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/" {
			http.Redirect(w, r, "/login", http.StatusFound)
			return
		}
		_, _ = w.Write([]byte("invalid token"))
	}))
	defer webui.Close()
	if err := CheckHTTP(webui.URL, "/app/fncluster/inject/loader.js?v=1", []byte(loader), socket)(); err != nil {
		t.Fatal(err)
	}
}

func TestHealthCheckRejectsBrokenIndex(t *testing.T) {
	const loader = "console.log('fncluster')"
	webui := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	defer webui.Close()
	if err := CheckHTTP(webui.URL, "/app/fncluster/inject/loader.js?v=1", []byte(loader), "/nonexistent.sock")(); err == nil {
		t.Fatal("404 index passed the health check")
	}
}

func TestHealthCheckUsesLocalSocketWhenGatewayNeedsLogin(t *testing.T) {
	const loader = "console.log('fncluster')"
	dir, err := os.MkdirTemp(os.TempDir(), "fcs-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	socket := filepath.Join(dir, "s")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	local := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/app/fncluster/inject/loader.js" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(loader))
	})}
	defer local.Close()
	go local.Serve(listener)
	webui := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/" {
			_, _ = w.Write([]byte("<head>" + BeginMarker + "</head>"))
			return
		}
		_, _ = w.Write([]byte("invalid token"))
	}))
	defer webui.Close()
	if err := CheckHTTP(webui.URL, "/app/fncluster/inject/loader.js?v=1", []byte(loader), socket)(); err != nil {
		t.Fatal(err)
	}
	wrongRoute := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/" {
			_, _ = w.Write([]byte("<head>" + BeginMarker + "</head>"))
			return
		}
		http.NotFound(w, r)
	}))
	defer wrongRoute.Close()
	if err := CheckHTTP(wrongRoute.URL, "/app/fncluster/inject/loader.js?v=1", []byte(loader), socket)(); err == nil {
		t.Fatal("missing public loader route passed health check")
	}
}
