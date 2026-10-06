package catalog

import (
	"bufio"
	"encoding/json"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"fncluster/internal/cluster"
)

type uiURL struct {
	Title         string    `json:"title"`
	Icon          string    `json:"icon"`
	URL           string    `json:"url"`
	NoDisplay     bool      `json:"noDisplay"`
	GatewaySocket string    `json:"gatewaySocket"`
	Protocol      string    `json:"protocol"`
	Port          PortValue `json:"port"`
}

// PortValue accepts a JSON string or number; fnOS ships both shapes, and
// unresolved ${port} placeholders stay empty.
type PortValue string

func (p *PortValue) UnmarshalJSON(data []byte) error {
	raw := strings.Trim(strings.TrimSpace(string(data)), `"`)
	if raw == "" || raw == "null" {
		return nil
	}
	if _, err := strconv.Atoi(raw); err != nil {
		return nil
	}
	*p = PortValue(raw)
	return nil
}

// pathOf returns the URL path without the trailing slash ("" for root).
func pathOf(raw string) string {
	parsed, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	cleaned := strings.TrimSuffix(parsed.Path, "/")
	if cleaned == "/" {
		return ""
	}
	return cleaned
}

// portTarget builds a loopback TCP relay target from the entry's explicit
// port, a loopback absolute URL, or the manifest service_port fallback.
func portTarget(item uiURL, entryURL string, servicePort int, iconPath string) (cluster.RelayTarget, bool) {
	port := strings.TrimSpace(string(item.Port))
	scheme := strings.ToLower(strings.TrimSpace(item.Protocol))
	if port == "" {
		if parsed, err := url.Parse(entryURL); err == nil && cluster.IsLoopbackHost(parsed.Hostname()) {
			port = parsed.Port()
			if scheme == "" {
				scheme = strings.ToLower(parsed.Scheme)
			}
		}
	}
	if port == "" && servicePort > 0 && pathOf(entryURL) == "" {
		port = strconv.Itoa(servicePort)
	}
	if number, err := strconv.Atoi(port); err != nil || number < 1 || number > 65535 {
		return cluster.RelayTarget{}, false
	}
	if scheme != "https" {
		scheme = "http"
	}
	return cluster.RelayTarget{
		Kind:        "port",
		Address:     "127.0.0.1:" + port,
		Scheme:      scheme,
		InsecureTLS: scheme == "https",
		Prefix:      pathOf(entryURL),
		IconPath:    iconPath,
	}, true
}

// relayTargetFor recognizes the two local service shapes the cluster relay can
// reach without the fnOS gateway: a unix socket (`gatewaySocket`) or an app CGI
// binary (`/cgi/ThirdParty/<app>/index.cgi` style). The path is resolved
// locally and never leaves the node.
func relayTargetFor(base, entryURL string, item uiURL, servicePort int) (cluster.RelayTarget, bool) {
	iconPath := iconFile(base, item.Icon)
	if item.GatewaySocket != "" {
		return cluster.RelayTarget{
			Kind:     "socket",
			Socket:   filepath.Join(base, "target", filepath.Base(item.GatewaySocket)),
			Prefix:   strings.TrimSuffix(entryURL, "/"),
			IconPath: iconPath,
		}, true
	}
	// Apps with their own HTTP port (motrix style: protocol+port+relative url)
	// and loopback absolute URLs skip the gateway entirely.
	if strings.TrimSpace(string(item.Port)) != "" || parseLoopback(entryURL) {
		if target, ok := portTarget(item, entryURL, servicePort, iconPath); ok {
			return target, true
		}
	}
	parsed, err := url.Parse(entryURL)
	if err != nil {
		return cluster.RelayTarget{}, false
	}
	if strings.HasSuffix(parsed.Path, ".cgi") {
		cgiPath := filepath.Join(base, "target", "ui", filepath.Base(parsed.Path))
		if info, statErr := os.Stat(cgiPath); statErr == nil && info.Mode().IsRegular() {
			return cluster.RelayTarget{Kind: "cgi", CGIPath: cgiPath, Prefix: path.Dir(parsed.Path), IconPath: iconPath}, true
		}
	}
	// Manifest service_port fallback for apps that only declare the port there.
	if target, ok := portTarget(item, entryURL, servicePort, iconPath); ok {
		return target, true
	}
	return cluster.RelayTarget{}, false
}

// parseLoopback reports whether the entry URL is an absolute loopback URL.
func parseLoopback(entryURL string) bool {
	parsed, err := url.Parse(entryURL)
	if err != nil || parsed.Host == "" {
		return false
	}
	return parsed.Port() != "" && cluster.IsLoopbackHost(parsed.Hostname())
}

// iconFile resolves the ui/config icon pattern (images/icon_{0}.png) to a
// file inside the app directory; 64 first, then 256 for packages that ship
// only the larger size.
func iconFile(base, pattern string) string {
	if pattern == "" {
		return ""
	}
	candidates := []string{strings.ReplaceAll(pattern, "{0}", "64")}
	if strings.Contains(pattern, "{0}") {
		candidates = append(candidates, strings.ReplaceAll(pattern, "{0}", "256"))
	}
	for _, candidate := range candidates {
		path := filepath.Join(base, "target", "ui", filepath.FromSlash(candidate))
		if info, err := os.Stat(path); err == nil && info.Mode().IsRegular() {
			return path
		}
	}
	return ""
}

// cleanManifestValue trims spaces and matching quotes: some third-party
// manifests ship appname="foo.bar", which must not miss the directory check.
func cleanManifestValue(value string) string {
	value = strings.TrimSpace(value)
	if len(value) >= 2 {
		first, last := value[0], value[len(value)-1]
		if (first == '"' && last == '"') || (first == '\'' && last == '\'') {
			value = strings.TrimSpace(value[1 : len(value)-1])
		}
	}
	return value
}

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

// Scan reads FPK metadata only; it does not call private fnOS APIs or launch
// apps. It returns the local relay target table (appName -> local service)
// plus resolved icon paths for every accepted app, including ones without a
// declared transport (their UI may be gateway-proxied).
func Scan(root, nodeID, fnOSVersion string) ([]cluster.PanelEntry, map[string]cluster.RelayTarget, map[string]string, error) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, nil, nil, err
	}
	var panels []cluster.PanelEntry
	targets := map[string]cluster.RelayTarget{}
	icons := map[string]string{}
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
		servicePort := 0
		scanner := bufio.NewScanner(manifest)
		for scanner.Scan() {
			key, value, ok := strings.Cut(scanner.Text(), "=")
			if !ok {
				continue
			}
			switch strings.TrimSpace(key) {
			case "appname":
				appName = cleanManifestValue(value)
			case "service_port":
				if number, convErr := strconv.Atoi(cleanManifestValue(value)); convErr == nil {
					servicePort = number
				}
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
			URLs map[string]uiURL `json:".url"`
		}
		if json.Unmarshal(data, &ui) != nil {
			continue
		}
		for id, item := range ui.URLs {
			if item.NoDisplay || item.Title == "" {
				continue
			}
			if iconPath := iconFile(base, item.Icon); iconPath != "" {
				icons[appName] = iconPath
			}
			panel := cluster.PanelEntry{ID: id, NodeID: nodeID, Kind: "application", Title: item.Title, Icon: item.Icon, AppName: appName}
			if strings.HasPrefix(item.URL, "/") && !strings.HasPrefix(item.URL, "//") {
				panel.DirectURL = item.URL
			} else if strings.HasPrefix(item.URL, "https://") {
				panel.ExternalURL = item.URL
			}
			if target, ok := relayTargetFor(base, item.URL, item, servicePort); ok {
				panel.Relayable = true
				targets[appName] = target
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
			cluster.PanelEntry{ID: "fnos.settings", NodeID: nodeID, Kind: "system", Title: settings, Native: true, AppName: "trim.setting"},
			cluster.PanelEntry{ID: "fnos.files", NodeID: nodeID, Kind: "system", Title: files, Native: true, AppName: "trim.file-manager"})
	}
	sort.Slice(panels, func(i, j int) bool {
		if panels[i].NodeID != panels[j].NodeID {
			return panels[i].NodeID < panels[j].NodeID
		}
		return panels[i].ID < panels[j].ID
	})
	return panels, targets, icons, nil
}
