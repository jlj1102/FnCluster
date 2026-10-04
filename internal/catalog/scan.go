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
		panels = append(panels,
			cluster.PanelEntry{ID: "fnos.settings", NodeID: nodeID, Kind: "system", Title: "Settings", Native: true},
			cluster.PanelEntry{ID: "fnos.files", NodeID: nodeID, Kind: "system", Title: "Files", Native: true})
	}
	sort.Slice(panels, func(i, j int) bool {
		if panels[i].NodeID != panels[j].NodeID {
			return panels[i].NodeID < panels[j].NodeID
		}
		return panels[i].ID < panels[j].ID
	})
	return panels, nil
}
