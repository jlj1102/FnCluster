package cluster

import (
	"sort"
	"strings"
	"sync"
	"time"
)

type Filesystem struct {
	Mount      string `json:"mount"`
	TotalBytes uint64 `json:"totalBytes"`
	UsedBytes  uint64 `json:"usedBytes"`
}

type NetworkCounter struct {
	ReceiveBytes  uint64 `json:"receiveBytes"`
	TransmitBytes uint64 `json:"transmitBytes"`
}

type PanelEntry struct {
	ID          string `json:"id"`
	NodeID      string `json:"nodeId"`
	Kind        string `json:"kind"`
	Title       string `json:"title"`
	Icon        string `json:"icon,omitempty"`
	DirectURL   string `json:"directUrl,omitempty"`
	ExternalURL string `json:"externalUrl,omitempty"`
	Native      bool   `json:"native"`
	// AppName is the fnOS application name (e.g. tailscale); AppName+Relayable
	// let a remote browser request this panel through the cluster relay.
	AppName   string `json:"appName,omitempty"`
	Relayable bool   `json:"relayable,omitempty"`
}

// RelayTarget is resolved on the owning node only and never crosses the mesh:
// a local unix socket or an app CGI binary the relay may proxy to.
type RelayTarget struct {
	Kind     string `json:"kind"` // "socket" or "cgi"
	Socket   string `json:"socket,omitempty"`
	CGIPath  string `json:"cgiPath,omitempty"`
	Prefix   string `json:"prefix,omitempty"`
	IconPath string `json:"iconPath,omitempty"`
}

type Rates struct {
	NetworkReceiveBytesPerSecond  float64 `json:"networkReceiveBytesPerSecond"`
	NetworkTransmitBytesPerSecond float64 `json:"networkTransmitBytesPerSecond"`
	DiskReadBytesPerSecond        float64 `json:"diskReadBytesPerSecond"`
	DiskWriteBytesPerSecond       float64 `json:"diskWriteBytesPerSecond"`
}

type Metrics struct {
	CPUPercent    *float64                  `json:"cpuPercent,omitempty"`
	Load          [3]float64                `json:"load"`
	MemoryTotal   uint64                    `json:"memoryTotalBytes"`
	MemoryUsed    uint64                    `json:"memoryUsedBytes"`
	UptimeSeconds float64                   `json:"uptimeSeconds"`
	Network       map[string]NetworkCounter `json:"network"`
	Rates         *Rates                    `json:"rates,omitempty"`
	Filesystems   []Filesystem              `json:"filesystems"`
	CollectedAt   time.Time                 `json:"collectedAt"`
}

type Node struct {
	ID           string       `json:"id"`
	Name         string       `json:"name"`
	Hostname     string       `json:"hostname"`
	FnOSVersion  string       `json:"fnosVersion"`
	Architecture string       `json:"architecture"`
	LastSeen     time.Time    `json:"lastSeen"`
	Online       bool         `json:"online"`
	Metrics      Metrics      `json:"metrics"`
	Panels       []PanelEntry `json:"panels,omitempty"`
	// PanelsSource is "desktop" for the localized list reported by the fnOS
	// desktop page and "scan" for the filesystem fallback.
	PanelsSource string `json:"panelsSource,omitempty"`
	// WebOrigin is the browser origin the node's desktop was opened with
	// (scheme://host:port). Remote panels use it for DIRECT links.
	WebOrigin string `json:"webOrigin,omitempty"`
}

type Summary struct {
	Status string `json:"status"`
	Nodes  struct {
		Online int `json:"online"`
		Total  int `json:"total"`
	} `json:"nodes"`
	Storage struct {
		TotalBytes uint64 `json:"totalBytes"`
		UsedBytes  uint64 `json:"usedBytes"`
	} `json:"storage"`
}

type State struct {
	mu      sync.RWMutex
	localID string
	nodes   map[string]Node
}

func New(local Node) *State {
	return &State{localID: local.ID, nodes: map[string]Node{local.ID: local}}
}

// SetLocalID moves the local node to a freshly generated identity after this
// node left and rejoined; metrics and panels are preserved.
func (s *State) SetLocalID(id string) {
	if id == "" || id == s.localID {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	local := s.nodes[s.localID]
	delete(s.nodes, s.localID)
	local.ID = id
	s.nodes[id] = local
	s.localID = id
}

func (s *State) UpdateLocal(metrics Metrics) {
	s.mu.Lock()
	defer s.mu.Unlock()
	node := s.nodes[s.localID]
	node.Metrics = metrics
	node.LastSeen = time.Now().UTC()
	node.Online = true
	s.nodes[s.localID] = node
}

// isSyntheticPanel recognizes the desktop bridge's own all-apps entries. They
// must never enter a catalog: the old bug fed them back through the report and
// composed titles like "系统设置 · FN02 · FN01" on every sync round.
func isSyntheticPanel(id string) bool {
	return strings.HasPrefix(id, "fncluster:") || strings.HasPrefix(id, "fncluster.remote.")
}

func sanitizePanels(panels []PanelEntry) []PanelEntry {
	if len(panels) == 0 {
		return panels
	}
	out := make([]PanelEntry, 0, len(panels))
	for _, panel := range panels {
		if isSyntheticPanel(panel.ID) {
			continue
		}
		out = append(out, panel)
	}
	return out
}

func (s *State) UpdateLocalPanels(panels []PanelEntry, source string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	node := s.nodes[s.localID]
	node.Panels = append([]PanelEntry(nil), sanitizePanels(panels)...)
	node.PanelsSource = source
	s.nodes[s.localID] = node
}

func (s *State) LocalPanelsSource() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.nodes[s.localID].PanelsSource
}

func (s *State) SetLocalWebOrigin(origin string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	node := s.nodes[s.localID]
	node.WebOrigin = origin
	s.nodes[s.localID] = node
}

func (s *State) Panels() []PanelEntry {
	s.mu.RLock()
	defer s.mu.RUnlock()
	panels := make([]PanelEntry, 0)
	for _, node := range s.nodes {
		panels = append(panels, sanitizePanels(node.Panels)...)
	}
	sort.Slice(panels, func(i, j int) bool {
		if panels[i].NodeID != panels[j].NodeID {
			return panels[i].NodeID < panels[j].NodeID
		}
		return panels[i].ID < panels[j].ID
	})
	return panels
}

func (s *State) Nodes() []Node {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]Node, 0, len(s.nodes))
	for _, node := range s.nodes {
		node.Panels = sanitizePanels(node.Panels)
		out = append(out, node)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

func (s *State) Local() Node {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.nodes[s.localID]
}

func (s *State) UpsertRemote(node Node) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if node.ID == "" || node.ID == s.localID {
		return
	}
	node.Panels = sanitizePanels(node.Panels)
	node.Online = true
	node.LastSeen = time.Now().UTC()
	s.nodes[node.ID] = node
}

// Reconcile drops remote nodes that are no longer cluster members, including revoked ones.
func (s *State) Reconcile(members map[string]string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for id := range s.nodes {
		if id == s.localID {
			continue
		}
		if _, ok := members[id]; !ok {
			delete(s.nodes, id)
		}
	}
}

func (s *State) MarkOffline(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if id == s.localID {
		return
	}
	node, ok := s.nodes[id]
	if !ok {
		node = Node{ID: id, Name: id}
	}
	node.Online = false
	s.nodes[id] = node
}

// Summary adds independent filesystem capacities; it does not describe a pool.
func (s *State) Summary() Summary {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var result Summary
	healthy := true
	result.Nodes.Total = len(s.nodes)
	for _, node := range s.nodes {
		if !node.Online {
			healthy = false
			continue
		}
		if node.Metrics.CollectedAt.IsZero() || time.Since(node.Metrics.CollectedAt) > 20*time.Second {
			healthy = false
		}
		result.Nodes.Online++
		for _, fs := range node.Metrics.Filesystems {
			result.Storage.TotalBytes += fs.TotalBytes
			result.Storage.UsedBytes += fs.UsedBytes
		}
	}
	if healthy {
		result.Status = "healthy"
	} else {
		result.Status = "degraded"
	}
	return result
}
