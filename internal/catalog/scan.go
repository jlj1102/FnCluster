package catalog

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"fncluster/internal/cluster"
)

// localeRoot is a test seam; fnOS ships its UI translations under this tree.
var localeRoot = "/usr/trim/www/locales"

// systemTitles reads the fnOS UI locale files (zh-CN first, then en-US) so the
// filesystem fallback shows 系统设置/文件管理 instead of English. The desktop
// report replaces this fallback with the complete localized entry list; a
// missing or unparsable locale file keeps the English defaults.
func systemTitles() map[string]string {
	for _, lang := range []string{"zh-CN", "en-US"} {
		data, err := os.ReadFile(filepath.Join(localeRoot, lang, "os.json"))
		if err != nil {
			continue
		}
		var locale struct {
			App map[string]string `json:"app"`
		}
		if json.Unmarshal(data, &locale) != nil {
			continue
		}
		titles := map[string]string{}
		if value := strings.TrimSpace(locale.App["setting"]); value != "" {
			titles["setting"] = value
		}
		if value := strings.TrimSpace(locale.App["fileManager"]); value != "" {
			titles["fileManager"] = value
		}
		if len(titles) > 0 {
			return titles
		}
	}
	return nil
}

// Scan reads FPK metadata only; it does not call private fnOS APIs or launch apps.
func Scan(root, nodeID, fnOSVersion string) ([]cluster.PanelEntry, error) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, err
	}
	var panels []cluster.PanelEntry
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		base := filepath.Join(root, entry.Name())
		manifest, err := os.Open(filepath.Join(base, "manifest"))
		if err != nil {
			continue
		}
		appName := ""
		scanner := bufio.NewScanner(manifest)
		for scanner.Scan() {
			key, value, ok := strings.Cut(scanner.Text(), "=")
			if ok && strings.TrimSpace(key) == "appname" {
				appName = strings.TrimSpace(value)
				break
			}
		}
		manifest.Close()
		if appName != entry.Name() {
			continue
		}
		data, err := os.ReadFile(filepath.Join(base, "target", "ui", "config"))
		if err != nil {
			continue
		}
		var ui struct {
			URLs map[string]struct {
				Title     string `json:"title"`
				Icon      string `json:"icon"`
				URL       string `json:"url"`
				NoDisplay bool   `json:"noDisplay"`
			} `json:".url"`
		}
		if json.Unmarshal(data, &ui) != nil {
			continue
		}
		for id, item := range ui.URLs {
			if item.NoDisplay || item.Title == "" {
				continue
			}
			panel := cluster.PanelEntry{ID: id, NodeID: nodeID, Kind: "application", Title: item.Title, Icon: item.Icon}
			if strings.HasPrefix(item.URL, "/") && !strings.HasPrefix(item.URL, "//") {
				panel.DirectURL = item.URL
			} else if strings.HasPrefix(item.URL, "https://") {
				panel.ExternalURL = item.URL
			}
			panels = append(panels, panel)
		}
	}
	// These two native entries are confirmed only for the captured fnOS 1.2.0701 build.
	if strings.Contains(fnOSVersion, "1.2.0701") {
		titles := systemTitles()
		settings, files := titles["setting"], titles["fileManager"]
		if settings == "" {
			settings = "Settings"
		}
		if files == "" {
			files = "Files"
		}
		panels = append(panels,
			cluster.PanelEntry{ID: "fnos.settings", NodeID: nodeID, Kind: "system", Title: settings, Native: true},
			cluster.PanelEntry{ID: "fnos.files", NodeID: nodeID, Kind: "system", Title: files, Native: true})
	}
	sort.Slice(panels, func(i, j int) bool {
		if panels[i].NodeID != panels[j].NodeID {
			return panels[i].NodeID < panels[j].NodeID
		}
		return panels[i].ID < panels[j].ID
	})
	return panels, nil
}
