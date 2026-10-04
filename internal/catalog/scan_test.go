package catalog

import (
	"os"
	"path/filepath"
	"testing"
)

func TestScanFPKEntriesAndSystemPanels(t *testing.T) {
	root := t.TempDir()
	app := filepath.Join(root, "photo")
	if err := os.MkdirAll(filepath.Join(app, "target", "ui"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(app, "manifest"), []byte("appname = photo\n"), 0644); err != nil {
		t.Fatal(err)
	}
	config := `{ ".url": { "photo.main": { "title":"Photos", "url":"/app/photo" }, "photo.hidden": { "title":"Hidden", "noDisplay":true } } }`
	if err := os.WriteFile(filepath.Join(app, "target", "ui", "config"), []byte(config), 0644); err != nil {
		t.Fatal(err)
	}
	panels, err := Scan(root, "node-a", "1.2.0701")
	if err != nil {
		t.Fatal(err)
	}
	if len(panels) != 3 || panels[0].ID != "fnos.files" || panels[1].ID != "fnos.settings" || panels[2].ID != "photo.main" || panels[2].DirectURL != "/app/photo" {
		t.Fatalf("unexpected catalog: %+v", panels)
	}
}
