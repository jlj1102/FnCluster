package agent

import (
	"context"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"fncluster/internal/catalog"
	"fncluster/internal/cluster"
	"fncluster/internal/collector"
	"fncluster/internal/identity"
)

type Agent struct {
	State     *cluster.State
	collector *collector.Collector

	relayMu      sync.RWMutex
	relayTargets map[string]cluster.RelayTarget
}

// ResolveTarget returns the local relay endpoint for an fnOS app name; only
// targets discovered on this node are ever returned.
func (a *Agent) ResolveTarget(appName string) (cluster.RelayTarget, bool) {
	a.relayMu.RLock()
	defer a.relayMu.RUnlock()
	target, ok := a.relayTargets[appName]
	return target, ok
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

func (a *Agent) setRelayTargets(targets map[string]cluster.RelayTarget) {
	a.relayMu.Lock()
	a.relayTargets = targets
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
	return &Agent{State: state, collector: collector.New(mounts), relayTargets: map[string]cluster.RelayTarget{}}, nil
}

func (a *Agent) Run(ctx context.Context, interval time.Duration) {
	collect := func() {
		// The relay target table must refresh even while a desktop report owns
		// the catalog; the scan then only supplies panels when no report exists.
		if panels, targets, err := catalog.Scan("/var/apps", a.State.Local().ID, a.State.Local().FnOSVersion); err == nil {
			a.setRelayTargets(targets)
			if a.State.LocalPanelsSource() != "desktop" {
				a.State.UpdateLocalPanels(panels, "scan")
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
