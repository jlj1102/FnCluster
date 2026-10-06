package agent

import (
	"os"
	"path/filepath"
	"testing"
)

func TestScanAppProcessFindsInjectedPort(t *testing.T) {
	original := procRoot
	procRoot = t.TempDir()
	defer func() { procRoot = original }()

	openlist := filepath.Join(procRoot, "17262")
	if err := os.MkdirAll(openlist, 0755); err != nil {
		t.Fatal(err)
	}
	environ := "TRIM_APPNAME=OpenList\x00port=5244\x00TRIM_SERVICE_PORT=\x00"
	if err := os.WriteFile(filepath.Join(openlist, "environ"), []byte(environ), 0644); err != nil {
		t.Fatal(err)
	}
	port, scheme := scanAppProcess("OpenList")
	if port != 5244 || scheme != "http" {
		t.Fatalf("port = %d, scheme = %q", port, scheme)
	}
	if port, _ := scanAppProcess("other"); port != 0 {
		t.Fatalf("matched an unrelated app: %d", port)
	}

	legacy := filepath.Join(procRoot, "99")
	if err := os.MkdirAll(legacy, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(legacy, "environ"), []byte("TRIM_APPNAME=legacy\x00TRIM_SERVICE_PORT=8088\x00"), 0644); err != nil {
		t.Fatal(err)
	}
	if port, _ := scanAppProcess("legacy"); port != 8088 {
		t.Fatalf("TRIM_SERVICE_PORT fallback = %d", port)
	}
}
