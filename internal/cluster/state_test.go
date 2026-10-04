package cluster

import (
	"strings"
	"testing"
	"time"
)

func TestSummaryIsSumOfNodeMounts(t *testing.T) {
	s := New(Node{ID: "node-a", Online: true})
	s.UpdateLocal(Metrics{Filesystems: []Filesystem{{Mount: "/vol1", TotalBytes: 12, UsedBytes: 7}, {Mount: "/vol2", TotalBytes: 8, UsedBytes: 2}}})
	got := s.Summary()
	if got.Storage.TotalBytes != 20 || got.Storage.UsedBytes != 9 || got.Nodes.Online != 1 {
		t.Fatalf("unexpected summary: %+v", got)
	}
}

func TestSetLocalIDMovesLocalNode(t *testing.T) {
	s := New(Node{ID: "old-id", Name: "nas", Hostname: "nas", Online: true})
	s.UpdateLocal(Metrics{CollectedAt: time.Now()})
	s.SetLocalID("new-id")
	local := s.Local()
	if local.ID != "new-id" || local.Name != "nas" || local.Metrics.CollectedAt.IsZero() {
		t.Fatalf("local node not moved: %+v", local)
	}
	if nodes := s.Nodes(); len(nodes) != 1 {
		t.Fatalf("old identity left behind: %+v", nodes)
	}
	s.SetLocalID("")
	if s.Local().ID != "new-id" {
		t.Fatal("empty identity changed the local node")
	}
}

func TestSyntheticPanelsNeverEnterCatalog(t *testing.T) {
	s := New(Node{ID: "node-a", Online: true})
	s.UpdateLocalPanels([]PanelEntry{
		{ID: "fncluster:node-b.entry:1", NodeID: "node-a", Title: "系统设置 · node-b"},
		{ID: "entry:2", NodeID: "node-a", Title: "系统设置"},
	}, "desktop")
	if panels := s.Local().Panels; len(panels) != 1 || panels[0].ID != "entry:2" {
		t.Fatalf("local catalog kept a synthetic panel: %+v", panels)
	}
	s.UpsertRemote(Node{ID: "node-b", Online: true, Panels: []PanelEntry{
		{ID: "fncluster.remote.node-a.entry:1", NodeID: "node-b", Title: "文件管理 · node-a"},
		{ID: "entry:1", NodeID: "node-b", Title: "文件管理"},
	}})
	for _, node := range s.Nodes() {
		if node.ID != "node-b" {
			continue
		}
		if len(node.Panels) != 1 || node.Panels[0].ID != "entry:1" {
			t.Fatalf("remote snapshot kept a synthetic panel: %+v", node.Panels)
		}
	}
	for _, panel := range s.Panels() {
		if strings.HasPrefix(panel.ID, "fncluster") {
			t.Fatalf("aggregate catalog leaked a synthetic panel: %+v", panel)
		}
	}
}

func TestReconcileDropsNonMembers(t *testing.T) {
	s := New(Node{ID: "node-a", Online: true})
	s.UpsertRemote(Node{ID: "node-b", Online: true})
	s.UpsertRemote(Node{ID: "node-c", Online: true})
	s.Reconcile(map[string]string{"node-a": "a:1", "node-c": "c:1"})
	nodes := s.Nodes()
	if len(nodes) != 2 {
		t.Fatalf("reconcile left %d nodes: %+v", len(nodes), nodes)
	}
	for _, node := range nodes {
		if node.ID == "node-b" {
			t.Fatal("non-member node was kept")
		}
	}
}
