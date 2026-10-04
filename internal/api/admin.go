package api

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
)

// mutationRequest guards browser write endpoints: POST plus a custom header
// that cross-site forms cannot set, and JSON-only bodies. There is no CORS
// support, so cross-origin preflights fail.
func mutationRequest(w http.ResponseWriter, r *http.Request, body any) bool {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		writeError(w, http.StatusMethodNotAllowed, "只支持 POST")
		return false
	}
	if r.Header.Get("X-FnCluster") != "1" {
		writeError(w, http.StatusForbidden, "缺少 FnCluster 请求头")
		return false
	}
	r.Body = http.MaxBytesReader(w, r.Body, 16<<10)
	if err := json.NewDecoder(r.Body).Decode(body); err != nil && !errors.Is(err, io.EOF) {
		writeError(w, http.StatusBadRequest, "请求格式错误")
		return false
	}
	return true
}

func (s *server) handleMeshInit(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Advertise string `json:"advertise"`
	}
	if !mutationRequest(w, r, &body) {
		return
	}
	if err := s.admin.MeshInit(body.Advertise); err != nil {
		s.admin.Audit("mesh.init", body.Advertise, "error: "+err.Error())
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	s.admin.Audit("mesh.init", body.Advertise, "ok")
	writeJSON(w, map[string]bool{"ok": true})
}

func (s *server) handleMeshToken(w http.ResponseWriter, r *http.Request) {
	var body struct{}
	if !mutationRequest(w, r, &body) {
		return
	}
	result, err := s.admin.MeshToken()
	if err != nil {
		s.admin.Audit("mesh.token", "", "error: "+err.Error())
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	s.admin.Audit("mesh.token", "", "ok")
	writeJSON(w, result)
}

func (s *server) handleMeshJoin(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Seed      string `json:"seed"`
		Code      string `json:"code"`
		Advertise string `json:"advertise"`
	}
	if !mutationRequest(w, r, &body) {
		return
	}
	if err := s.admin.MeshJoin(body.Seed, body.Code, body.Advertise); err != nil {
		// Never log the pairing code itself.
		s.admin.Audit("mesh.join", body.Seed, "error: "+err.Error())
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	s.admin.Audit("mesh.join", body.Seed, "ok")
	writeJSON(w, map[string]bool{"ok": true})
}

func (s *server) handleMeshRemove(w http.ResponseWriter, r *http.Request) {
	var body struct {
		NodeID string `json:"nodeId"`
	}
	if !mutationRequest(w, r, &body) {
		return
	}
	if err := s.admin.MeshRemove(body.NodeID); err != nil {
		s.admin.Audit("mesh.remove", body.NodeID, "error: "+err.Error())
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	s.admin.Audit("mesh.remove", body.NodeID, "ok")
	writeJSON(w, map[string]bool{"ok": true})
}

func (s *server) handleMeshLeave(w http.ResponseWriter, r *http.Request) {
	var body struct{}
	if !mutationRequest(w, r, &body) {
		return
	}
	if err := s.admin.MeshLeave(); err != nil {
		s.admin.Audit("mesh.leave", "", "error: "+err.Error())
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	s.admin.Audit("mesh.leave", "", "ok")
	writeJSON(w, map[string]bool{"ok": true})
}

func (s *server) handleMeshResetIdentity(w http.ResponseWriter, r *http.Request) {
	var body struct{}
	if !mutationRequest(w, r, &body) {
		return
	}
	if err := s.admin.ResetIdentity(); err != nil {
		s.admin.Audit("mesh.reset-identity", "", "error: "+err.Error())
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	s.admin.Audit("mesh.reset-identity", "", "ok")
	writeJSON(w, map[string]bool{"ok": true})
}

func (s *server) handleWebUIApply(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Compatible bool `json:"compatible"`
	}
	if !mutationRequest(w, r, &body) {
		return
	}
	target := ""
	if body.Compatible {
		target = "compatible"
	}
	if err := s.admin.WebUIApply(body.Compatible); err != nil {
		s.admin.Audit("webui.apply", target, "error: "+err.Error())
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	s.admin.Audit("webui.apply", target, "ok")
	writeJSON(w, map[string]bool{"ok": true})
}

func (s *server) handleWebUIRestore(w http.ResponseWriter, r *http.Request) {
	var body struct{}
	if !mutationRequest(w, r, &body) {
		return
	}
	if err := s.admin.WebUIRestore(); err != nil {
		s.admin.Audit("webui.restore", "", "error: "+err.Error())
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	s.admin.Audit("webui.restore", "", "ok")
	writeJSON(w, map[string]bool{"ok": true})
}

func (s *server) handleWebUIEnable(w http.ResponseWriter, r *http.Request) {
	var body struct{}
	if !mutationRequest(w, r, &body) {
		return
	}
	if err := s.admin.WebUIEnable(); err != nil {
		s.admin.Audit("webui.enable", "", "error: "+err.Error())
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	s.admin.Audit("webui.enable", "", "ok")
	writeJSON(w, map[string]bool{"ok": true})
}
