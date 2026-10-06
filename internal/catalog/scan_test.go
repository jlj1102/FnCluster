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
	panels, _, _, err := Scan(t.TempDir(), "node-a", "1.2.0701")
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

	panels, targets, _, err := Scan(root, "node-a", "1.2.0701")
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

func TestScanMarksPortTargets(t *testing.T) {
	root := t.TempDir()
	writeApp := func(name, manifest, config string) {
		base := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Join(base, "target", "ui", "images"), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(base, "manifest"), []byte(manifest), 0644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(base, "target", "ui", "config"), []byte(config), 0644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(base, "target", "ui", "images", "icon_64.png"), []byte("png"), 0644); err != nil {
			t.Fatal(err)
		}
	}
	writeApp("motrix", "appname = motrix\nservice_port = 17080\n",
		`{ ".url": { "motrix.Application": { "title":"Motrix", "icon":"images/icon_{0}.png", "type":"iframe", "protocol":"http", "port":"17080", "url":"/?v=1.9.7" } } }`)
	writeApp("numeric", "appname = numeric\n",
		`{ ".url": { "numeric.Application": { "title":"Numeric", "icon":"images/icon_{0}.png", "protocol":"http", "port":9000, "url":"/ui" } } }`)
	writeApp("tlsapp", "appname = tlsapp\n",
		`{ ".url": { "tlsapp.Application": { "title":"TLS", "icon":"images/icon_{0}.png", "protocol":"https", "port":"8443", "url":"/" } } }`)
	writeApp("urlport", "appname = urlport\n",
		`{ ".url": { "urlport.Application": { "title":"URLPort", "icon":"images/icon_{0}.png", "url":"http://127.0.0.1:8081/ui" } } }`)
	writeApp("fallback", "appname = fallback\nservice_port = 7000\n",
		`{ ".url": { "fallback.Application": { "title":"Fallback", "icon":"images/icon_{0}.png", "url":"/" } } }`)
	writeApp("bad", "appname = bad\n",
		`{ ".url": { "bad.Application": { "title":"Bad", "icon":"images/icon_{0}.png", "protocol":"http", "port":"99999", "url":"/" } } }`)

	panels, targets, _, err := Scan(root, "node-a", "1.2.0701")
	if err != nil {
		t.Fatal(err)
	}
	cases := map[string]struct {
		address, scheme string
		insecure        bool
		prefix          string
	}{
		"motrix":   {address: "127.0.0.1:17080", scheme: "http"},
		"numeric":  {address: "127.0.0.1:9000", scheme: "http", prefix: "/ui"},
		"tlsapp":   {address: "127.0.0.1:8443", scheme: "https", insecure: true},
		"urlport":  {address: "127.0.0.1:8081", scheme: "http", prefix: "/ui"},
		"fallback": {address: "127.0.0.1:7000", scheme: "http"},
	}
	for name, want := range cases {
		target, ok := targets[name]
		if !ok || target.Kind != "port" {
			t.Fatalf("%s target missing: %+v", name, targets[name])
		}
		if target.Address != want.address || target.Scheme != want.scheme || target.InsecureTLS != want.insecure || target.Prefix != want.prefix {
			t.Fatalf("%s target wrong: %+v", name, target)
		}
		if target.IconPath == "" {
			t.Fatalf("%s icon path missing", name)
		}
	}
	if _, ok := targets["bad"]; ok {
		t.Fatalf("invalid port accepted: %+v", targets["bad"])
	}
	relayable := map[string]bool{}
	for _, panel := range panels {
		if panel.AppName != "" {
			relayable[panel.AppName] = panel.Relayable
		}
	}
	if !relayable["motrix"] {
		t.Fatalf("motrix panel not relayable: %+v", panels)
	}
	if relayable["bad"] {
		t.Fatal("bad panel marked relayable")
	}
}

func TestScanStripsQuotedManifestAndCollectsIcons(t *testing.T) {
	root := t.TempDir()
	app := filepath.Join(root, "baidu.netdisk")
	if err := os.MkdirAll(filepath.Join(app, "target", "ui", "images"), 0755); err != nil {
		t.Fatal(err)
	}
	manifest := "appname=\"baidu.netdisk\"\nservice_port=\"17080\"\n"
	if err := os.WriteFile(filepath.Join(app, "manifest"), []byte(manifest), 0644); err != nil {
		t.Fatal(err)
	}
	config := `{ ".url": { "baidu.netdisk": { "title":"百度网盘", "icon":"images/icon_{0}.png", "type":"iframe", "protocol":"http", "url":"/app-baidu-netdisk?ver=0.0.1" } } }`
	if err := os.WriteFile(filepath.Join(app, "target", "ui", "config"), []byte(config), 0644); err != nil {
		t.Fatal(err)
	}
	// Only the 256px file exists; the 64px candidate must fall through.
	if err := os.WriteFile(filepath.Join(app, "target", "ui", "images", "icon_256.png"), []byte("png"), 0644); err != nil {
		t.Fatal(err)
	}

	panels, _, icons, err := Scan(root, "node-a", "1.2.0701")
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, panel := range panels {
		if panel.AppName == "baidu.netdisk" && panel.Title == "百度网盘" {
			found = true
		}
	}
	if !found {
		t.Fatalf("quoted appname was skipped: %+v", panels)
	}
	if !strings.HasSuffix(icons["baidu.netdisk"], filepath.Join("images", "icon_256.png")) {
		t.Fatalf("icon fallback wrong: %+v", icons)
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
	panels, _, _, err := Scan(root, "node-a", "1.2.0701")
	if err != nil {
		t.Fatal(err)
	}
	if len(panels) != 3 || panels[0].ID != "fnos.files" || panels[1].ID != "fnos.settings" || panels[2].ID != "photo.main" || panels[2].DirectURL != "/app/photo" {
		t.Fatalf("unexpected catalog: %+v", panels)
	}
}
