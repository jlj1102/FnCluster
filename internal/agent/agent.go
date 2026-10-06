package agent

import (
	"context"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"fncluster/internal/catalog"
	"fncluster/internal/cluster"
	"fncluster/internal/collector"
	"fncluster/internal/identity"
)

// discoveredTTL throttles runtime port discovery; the next scan or report
// renews the entry.
const discoveredTTL = 60 * time.Second

// procRoot is a test seam for the /proc scan.
var procRoot = "/proc"

type Agent struct {
	State     *cluster.State
	collector *collector.Collector

	relayMu      sync.RWMutex
	relayTargets map[string]cluster.RelayTarget
	knownApps    map[string]bool
	appIcons     map[string]string
	discovered   map[string]cluster.RelayTarget
	discoveredAt map[string]time.Time

	discoverMu sync.Mutex
}

// ResolveTarget returns the local relay endpoint for an fnOS app name; only
// targets discovered on this node are ever returned. Apps whose port lives
// only in runtime settings (ui/config "${port}") fall back to process
// discovery, which reads the port fnOS injects into the app's environment.
func (a *Agent) ResolveTarget(appName string) (cluster.RelayTarget, bool) {
	a.relayMu.RLock()
	if target, ok := a.relayTargets[appName]; ok {
		a.relayMu.RUnlock()
		return target, true
	}
	if at, seen := a.discoveredAt[appName]; seen && time.Since(at) < discoveredTTL {
		target, ok := a.discovered[appName]
		a.relayMu.RUnlock()
		return target, ok
	}
	known := a.knownApps[appName]
	a.relayMu.RUnlock()
	if !known {
		return cluster.RelayTarget{}, false
	}
	return a.discoverPort(appName)
}

// discoverPort finds the app's own HTTP port from its process environment:
// fnOS injects TRIM_APPNAME and `port` (TRIM_SERVICE_PORT is usually empty)
// for every running app.
func (a *Agent) discoverPort(appName string) (cluster.RelayTarget, bool) {
	a.discoverMu.Lock()
	defer a.discoverMu.Unlock()
	// Another caller may have finished while we waited for the lock.
	a.relayMu.RLock()
	if target, ok := a.relayTargets[appName]; ok {
		a.relayMu.RUnlock()
		return target, true
	}
	if at, seen := a.discoveredAt[appName]; seen && time.Since(at) < discoveredTTL {
		target, ok := a.discovered[appName]
		a.relayMu.RUnlock()
		return target, ok
	}
	icon := a.appIcons[appName]
	a.relayMu.RUnlock()

	port, scheme := scanAppProcess(appName)
	a.relayMu.Lock()
	a.discoveredAt[appName] = time.Now()
	if port == 0 {
		delete(a.discovered, appName)
		a.relayMu.Unlock()
		return cluster.RelayTarget{}, false
	}
	target := cluster.RelayTarget{
		Kind:        "port",
		Address:     "127.0.0.1:" + strconv.Itoa(port),
		Scheme:      scheme,
		InsecureTLS: scheme == "https",
		IconPath:    icon,
	}
	a.discovered[appName] = target
	a.relayMu.Unlock()
	log.Printf("relay discovery: %s -> 127.0.0.1:%d", appName, port)
	return target, true
}

// AddReportedTarget registers the numeric port the node's own desktop
// reported for an app (raw entries carry the resolved port for "${port}"
// placeholders). It expires like a discovered one and is renewed by reports.
func (a *Agent) AddReportedTarget(appName string, port int, scheme string) bool {
	if appName == "" || port < 1 || port > 65535 {
		return false
	}
	if scheme != "https" {
		scheme = "http"
	}
	a.relayMu.Lock()
	defer a.relayMu.Unlock()
	if _, ok := a.relayTargets[appName]; ok {
		return true
	}
	a.discovered[appName] = cluster.RelayTarget{
		Kind:        "port",
		Address:     "127.0.0.1:" + strconv.Itoa(port),
		Scheme:      scheme,
		InsecureTLS: scheme == "https",
		IconPath:    a.appIcons[appName],
	}
	a.discoveredAt[appName] = time.Now()
	return true
}

// scanAppProcess walks /proc for the app's process and returns its HTTP port.
func scanAppProcess(appName string) (int, string) {
	entries, err := os.ReadDir(procRoot)
	if err != nil {
		return 0, ""
	}
	for _, entry := range entries {
		name := entry.Name()
		if !entry.IsDir() || name == "" || name[0] < '0' || name[0] > '9' {
			continue
		}
		data, err := os.ReadFile(filepath.Join(procRoot, name, "environ"))
		if err != nil {
			continue
		}
		env := parseEnv(data)
		if env["TRIM_APPNAME"] != appName {
			continue
		}
		if port := envPort(env["port"]); port > 0 {
			return port, "http"
		}
		if port := envPort(env["TRIM_SERVICE_PORT"]); port > 0 {
			return port, "http"
		}
	}
	return 0, ""
}

func parseEnv(data []byte) map[string]string {
	env := map[string]string{}
	for _, pair := range strings.Split(string(data), "\x00") {
		key, value, ok := strings.Cut(pair, "=")
		if ok && key != "" {
			env[key] = value
		}
	}
	return env
}

func envPort(value string) int {
	port, err := strconv.Atoi(strings.TrimSpace(value))
	if err != nil || port < 1 || port > 65535 {
		return 0
	}
	return port
}

func (a *Agent) Relayable(appName string) bool {
	_, ok := a.ResolveTarget(appName)
	return ok
}

// LocalIcon returns this node's own icon for an app name: the FPK scan icon or
// the builtin www icon. The relay uses it before falling back to its logo.
func (a *Agent) LocalIcon(appName string) ([]byte, bool) {
	if target, ok := a.ResolveTarget(appName); ok && target.IconPath != "" {
		if data, err := os.ReadFile(target.IconPath); err == nil && len(data) > 0 {
			return data, true
		}
	}
	data, err := os.ReadFile(filepath.Join("/usr/trim/www/static/app/icons", appName, "icon.png"))
	if err == nil && len(data) > 0 {
		return data, true
	}
	return nil, false
}

// setCatalog refreshes the scan-derived tables: relay targets plus the app
// name / icon metadata runtime discovery needs for apps without a declared
// transport (ui/config "${port}" placeholders).
func (a *Agent) setCatalog(targets map[string]cluster.RelayTarget, panels []cluster.PanelEntry) {
	known := make(map[string]bool, len(panels))
	icons := map[string]string{}
	for _, panel := range panels {
		if panel.AppName == "" {
			continue
		}
		known[panel.AppName] = true
		if icon := catalog.IconPath(filepath.Join("/var/apps", panel.AppName), panel.Icon); icon != "" {
			icons[panel.AppName] = icon
		}
	}
	a.relayMu.Lock()
	a.relayTargets = targets
	a.knownApps = known
	a.appIcons = icons
	a.relayMu.Unlock()
}

func New(dataDir string, mounts []string) (*Agent, error) {
	id, err := identity.LoadOrCreate(dataDir)
	if err != nil {
		return nil, err
	}
	hostname, err := os.Hostname()
	if err != nil {
		return nil, err
	}
	version := "unknown"
	if data, err := os.ReadFile("/usr/trim/etc/version"); err == nil {
		version = strings.TrimSpace(string(data))
	}
	state := cluster.New(cluster.Node{
		ID: id, Name: hostname, Hostname: hostname,
		FnOSVersion: version, Architecture: runtime.GOARCH, Online: true,
	})
	return &Agent{
		State:        state,
		collector:    collector.New(mounts),
		relayTargets: map[string]cluster.RelayTarget{},
		knownApps:    map[string]bool{},
		appIcons:     map[string]string{},
		discovered:   map[string]cluster.RelayTarget{},
		discoveredAt: map[string]time.Time{},
	}, nil
}

func (a *Agent) Run(ctx context.Context, interval time.Duration) {
	collect := func() {
		// The relay target table must refresh even while a desktop report owns
		// the catalog; the scan then only supplies panels when no report exists.
		if panels, targets, err := catalog.Scan("/var/apps", a.State.Local().ID, a.State.Local().FnOSVersion); err == nil {
			a.setCatalog(targets, panels)
			if a.State.LocalPanelsSource() != "desktop" {
				a.State.UpdateLocalPanels(panels, "scan")
			} else {
				// Apps installed after the last desktop report would otherwise
				// stay invisible for up to 30 minutes (or until a restart).
				a.State.MergeLocalPanels(panels)
			}
		}
		metrics, err := a.collector.Collect()
		if err != nil {
			log.Printf("metrics: %v", err)
			return
		}
		a.State.UpdateLocal(metrics)
	}
	collect()
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			collect()
		}
	}
}
