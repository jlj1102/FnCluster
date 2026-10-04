package agent

import (
	"context"
	"log"
	"os"
	"runtime"
	"strings"
	"time"

	"fncluster/internal/catalog"
	"fncluster/internal/cluster"
	"fncluster/internal/collector"
	"fncluster/internal/identity"
)

type Agent struct {
	State     *cluster.State
	collector *collector.Collector
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
	return &Agent{State: state, collector: collector.New(mounts)}, nil
}

func (a *Agent) Run(ctx context.Context, interval time.Duration) {
	collect := func() {
		// The desktop report is authoritative: it is complete, localized and
		// includes apps installed on data volumes. Never overwrite it with the
		// English filesystem fallback until the process restarts.
		if a.State.LocalPanelsSource() != "desktop" {
			if panels, err := catalog.Scan("/var/apps", a.State.Local().ID, a.State.Local().FnOSVersion); err == nil {
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
