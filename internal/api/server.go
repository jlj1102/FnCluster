package api

import (
	"embed"
	"encoding/json"
	"io/fs"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"

	"fncluster/internal/admin"
	"fncluster/internal/cluster"
	"fncluster/internal/mesh"
	"fncluster/internal/relay"
)

//go:embed web/*
var webFiles embed.FS

type server struct {
	state   *cluster.State
	admin   *admin.Admin
	runtime *mesh.Runtime
	relay   *relay.Server
}

func New(state *cluster.State, adminOps *admin.Admin, runtime *mesh.Runtime, relayServer *relay.Server) http.Handler {
	s := &server{state: state, admin: adminOps, runtime: runtime, relay: relayServer}
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		if getOnly(w, r) {
			writeJSON(w, map[string]string{"status": "ok"})
		}
	})
	mux.HandleFunc("/api/v1/cluster", func(w http.ResponseWriter, r *http.Request) {
		if getOnly(w, r) {
			writeJSON(w, state.Summary())
		}
	})
	mux.HandleFunc("/api/v1/storage/summary", func(w http.ResponseWriter, r *http.Request) {
		if getOnly(w, r) {
			writeJSON(w, state.Summary().Storage)
		}
	})
	mux.HandleFunc("/api/v1/nodes", func(w http.ResponseWriter, r *http.Request) {
		if getOnly(w, r) {
			writeJSON(w, nodeViews(state))
		}
	})
	mux.HandleFunc("/api/v1/nodes/", func(w http.ResponseWriter, r *http.Request) {
		if !getOnly(w, r) {
			return
		}
		id := strings.TrimPrefix(r.URL.Path, "/api/v1/nodes/")
		if id == "" || strings.Contains(id, "/") {
			http.NotFound(w, r)
			return
		}
		for _, node := range nodeViews(state) {
			if node.ID == id {
				writeJSON(w, node)
				return
			}
		}
		http.NotFound(w, r)
	})
	mux.HandleFunc("/api/v1/panels", func(w http.ResponseWriter, r *http.Request) {
		if getOnly(w, r) {
			writeJSON(w, state.Panels())
		}
	})
	mux.HandleFunc("/api/v1/panels/report", s.handlePanelsReport)
	mux.HandleFunc("/api/v1/panels/icon/", s.handlePanelIcon)
	mux.HandleFunc("/api/v1/panels/", func(w http.ResponseWriter, r *http.Request) {
		if !getOnly(w, r) {
			return
		}
		id := strings.TrimPrefix(r.URL.Path, "/api/v1/panels/")
		if id == "" || strings.Contains(id, "/") {
			http.NotFound(w, r)
			return
		}
		var panels []cluster.PanelEntry
		for _, panel := range state.Panels() {
			if panel.NodeID == id {
				panels = append(panels, panel)
			}
		}
		writeJSON(w, panels)
	})
	mux.HandleFunc("/api/v1/mesh", s.handleMeshStatus)
	mux.HandleFunc("/api/v1/net/interfaces", s.handleInterfaces)
	mux.HandleFunc("/api/v1/webui", s.handleWebUIStatus)
	mux.HandleFunc("/api/v1/logs", s.handleLogs)
	mux.HandleFunc("/api/v1/mesh/init", s.handleMeshInit)
	mux.HandleFunc("/api/v1/mesh/token", s.handleMeshToken)
	mux.HandleFunc("/api/v1/mesh/join", s.handleMeshJoin)
	mux.HandleFunc("/api/v1/mesh/remove", s.handleMeshRemove)
	mux.HandleFunc("/api/v1/mesh/leave", s.handleMeshLeave)
	mux.HandleFunc("/api/v1/mesh/reset-identity", s.handleMeshResetIdentity)
	mux.HandleFunc("/api/v1/webui/apply", s.handleWebUIApply)
	mux.HandleFunc("/api/v1/webui/restore", s.handleWebUIRestore)
	mux.HandleFunc("/api/v1/webui/enable", s.handleWebUIEnable)
	mux.HandleFunc("/api/v1/relay/tokens", s.handleRelayTokens)
	web, _ := fs.Sub(webFiles, "web")
	mux.Handle("/", http.FileServer(http.FS(web)))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		if r.URL.Path == "/app/fncluster" {
			http.Redirect(w, r, "/app/fncluster/", http.StatusPermanentRedirect)
			return
		}
		if strings.HasPrefix(r.URL.Path, "/app/fncluster/") {
			http.StripPrefix("/app/fncluster", mux).ServeHTTP(w, r)
			return
		}
		mux.ServeHTTP(w, r)
	})
}

// nodeView marks the local node so the desktop bridge never has to race mesh
// state to tell local apps apart from remote ones.
type nodeView struct {
	cluster.Node
	Local bool `json:"local,omitempty"`
}

func nodeViews(state *cluster.State) []nodeView {
	localID := state.Local().ID
	nodes := state.Nodes()
	out := make([]nodeView, 0, len(nodes))
	for _, node := range nodes {
		out = append(out, nodeView{Node: node, Local: node.ID == localID})
	}
	return out
}

type meshStatusResponse struct {
	admin.MeshStatus
	Running bool   `json:"running"`
	Listen  string `json:"listen,omitempty"`
	Port    string `json:"port,omitempty"`
}

func (s *server) handleMeshStatus(w http.ResponseWriter, r *http.Request) {
	if !getOnly(w, r) {
		return
	}
	status, err := s.admin.MeshStatus()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	running, listen := s.runtime.Status()
	writeJSON(w, meshStatusResponse{MeshStatus: status, Running: running, Listen: listen, Port: s.admin.MeshPort})
}

func (s *server) handleInterfaces(w http.ResponseWriter, r *http.Request) {
	if !getOnly(w, r) {
		return
	}
	writeJSON(w, map[string]any{"port": s.admin.MeshPort, "addresses": s.admin.Addresses()})
}

func (s *server) handleWebUIStatus(w http.ResponseWriter, r *http.Request) {
	if !getOnly(w, r) {
		return
	}
	status, err := s.admin.WebUIStatus()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	compatible, compatErr := s.admin.WebUICompatible()
	if compatErr != nil {
		compatible = false
	}
	writeJSON(w, map[string]any{
		"status":                status,
		"integrationConfigured": s.admin.LoaderFile != "" && s.admin.HealthURL != "",
		"auditLog":              filepath.Join(s.admin.DataDir, "audit.log"),
		"compatible":            compatible,
		"archive":               s.admin.WebUIArchive(),
		"fndesk":                s.admin.FnDesk(),
	})
}

func (s *server) handleLogs(w http.ResponseWriter, r *http.Request) {
	if !getOnly(w, r) {
		return
	}
	lines, err := strconv.Atoi(r.URL.Query().Get("lines"))
	if err != nil {
		lines = 200
	}
	source := r.URL.Query().Get("source")
	tail, err := s.admin.Logs(source, lines)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if source == "" {
		source = "service"
	}
	writeJSON(w, map[string]any{"source": source, "lines": tail})
}

// handlePanelIcon serves a remote panel's original icon composited with the
// FnCluster badge. The desktop appends `?size=256`, which this path-based route
// ignores.
func (s *server) handlePanelIcon(w http.ResponseWriter, r *http.Request) {
	if !getOnly(w, r) {
		return
	}
	rest := strings.TrimPrefix(r.URL.Path, "/api/v1/panels/icon/")
	nodeID, appName, ok := strings.Cut(rest, "/")
	if !ok || nodeID == "" || !relay.ValidAppName(appName) {
		http.NotFound(w, r)
		return
	}
	if s.relay == nil {
		writeError(w, http.StatusServiceUnavailable, "relay is not enabled")
		return
	}
	localID := s.state.Local().ID
	found := false
	for _, node := range s.state.Nodes() {
		if node.ID == nodeID && nodeID != localID {
			found = true
			break
		}
	}
	if !found {
		http.NotFound(w, r)
		return
	}
	data, err := s.relay.Icon(nodeID, appName)
	if err != nil || len(data) == 0 {
		data = relay.DefaultIcon()
	}
	w.Header().Set("Content-Type", "image/png")
	w.Header().Set("Cache-Control", "public, max-age=3600")
	_, _ = w.Write(data)
}

func getOnly(w http.ResponseWriter, r *http.Request) bool {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return false
	}
	return true
}

func writeJSON(w http.ResponseWriter, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(value)
}

func writeError(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": message})
}
