package catalog

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestScanLocalizesSystemPanels(t *testing.T) {
	original := localeRoot
	localeRoot = t.TempDir()
	defer func() { localeRoot = original }()
	if err := os.MkdirAll(filepath.Join(localeRoot, "zh-CN"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(localeRoot, "zh-CN", "os.json"), []byte(`{"app":{"setting":"系统设置","fileManager":"文件管理"}}`), 0644); err != nil {
		t.Fatal(err)
	}
	panels, _, err := Scan(t.TempDir(), "node-a", "1.2.0701")
	if err != nil {
		t.Fatal(err)
	}
	titles := map[string]string{}
	for _, panel := range panels {
		titles[panel.ID] = panel.Title
	}
	if len(panels) != 2 || titles["fnos.settings"] != "系统设置" || titles["fnos.files"] != "文件管理" {
		t.Fatalf("unexpected localized catalog: %+v", panels)
	}
}

func TestScanMarksRelayTargets(t *testing.T) {
	root := t.TempDir()
	socketApp := filepath.Join(root, "fndesk")
	if err := os.MkdirAll(filepath.Join(socketApp, "target", "ui", "images"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(socketApp, "manifest"), []byte("appname = fndesk\n"), 0644); err != nil {
		t.Fatal(err)
	}
	socketConfig := `{ ".url": { "fndesk.main": { "title":"Fndesk", "icon":"images/icon_{0}.png", "url":"/app/fndesk/", "gatewaySocket":"fndesk.sock" } } }`
	if err := os.WriteFile(filepath.Join(socketApp, "target", "ui", "config"), []byte(socketConfig), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(socketApp, "target", "ui", "images", "icon_64.png"), []byte("png"), 0644); err != nil {
		t.Fatal(err)
	}

	cgiApp := filepath.Join(root, "tailscale")
	if err := os.MkdirAll(filepath.Join(cgiApp, "target", "ui"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cgiApp, "manifest"), []byte("appname = tailscale\n"), 0644); err != nil {
		t.Fatal(err)
	}
	cgiConfig := `{ ".url": { "tailscale.Application": { "title":"Tailscale", "url":"/cgi/ThirdParty/tailscale/index.cgi" } } }`
	if err := os.WriteFile(filepath.Join(cgiApp, "target", "ui", "config"), []byte(cgiConfig), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cgiApp, "target", "ui", "index.cgi"), []byte("#!/bin/sh\n"), 0755); err != nil {
		t.Fatal(err)
	}

	panels, targets, err := Scan(root, "node-a", "1.2.0701")
	if err != nil {
		t.Fatal(err)
	}
	relayable := map[string]bool{}
	for _, panel := range panels {
		if panel.AppName != "" {
			relayable[panel.AppName] = panel.Relayable
		}
	}
	if !relayable["fndesk"] || !relayable["tailscale"] {
		t.Fatalf("relayable apps not marked: %+v", panels)
	}
	if targets["fndesk"].Kind != "socket" || !strings.HasSuffix(targets["fndesk"].Socket, "fndesk.sock") {
		t.Fatalf("socket target wrong: %+v", targets["fndesk"])
	}
	if targets["tailscale"].Kind != "cgi" || targets["tailscale"].Prefix != "/cgi/ThirdParty/tailscale" {
		t.Fatalf("cgi target wrong: %+v", targets["tailscale"])
	}
	if !strings.HasSuffix(targets["fndesk"].IconPath, filepath.Join("images", "icon_64.png")) {
		t.Fatalf("icon path wrong: %+v", targets["fndesk"])
	}
}

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
	panels, _, err := Scan(root, "node-a", "1.2.0701")
	if err != nil {
		t.Fatal(err)
	}
	if len(panels) != 3 || panels[0].ID != "fnos.files" || panels[1].ID != "fnos.settings" || panels[2].ID != "photo.main" || panels[2].DirectURL != "/app/photo" {
		t.Fatalf("unexpected catalog: %+v", panels)
	}
}
