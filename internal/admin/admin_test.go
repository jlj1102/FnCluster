package admin

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"fncluster/internal/cluster"
)

func TestPairCodeRoundTrip(t *testing.T) {
	token := strings.Repeat("a1", 32)
	fingerprint := strings.Repeat("b2", 32)
	code := FormatPairCode(token, fingerprint)
	parsedToken, parsedFingerprint, err := ParsePairCode(" \n" + code + "\t")
	if err != nil {
		t.Fatal(err)
	}
	if parsedToken != token || parsedFingerprint != strings.ToUpper(fingerprint) {
		t.Fatalf("round trip mismatch: %q %q", parsedToken, parsedFingerprint)
	}
	if _, _, err := ParsePairCode("not-a-code"); err == nil {
		t.Fatal("malformed code accepted")
	}
	if _, _, err := ParsePairCode(strings.Repeat("zz", 32) + "." + fingerprint); err == nil {
		t.Fatal("non-hex token accepted")
	}
	if _, _, err := ParsePairCode(token); err == nil {
		t.Fatal("missing fingerprint accepted")
	}
}

func TestMeshInitTokenLeave(t *testing.T) {
	dir := t.TempDir()
	a := &Admin{DataDir: dir}
	status, err := a.MeshStatus()
	if err != nil || status.Configured {
		t.Fatalf("expected unconfigured: %+v %v", status, err)
	}
	if err := a.MeshInit("127.0.0.1:38671"); err != nil {
		t.Fatal(err)
	}
	status, err = a.MeshStatus()
	if err != nil || !status.Configured || !status.IsController {
		t.Fatalf("unexpected status: %+v %v", status, err)
	}
	result, err := a.MeshToken()
	if err != nil {
		t.Fatal(err)
	}
	token, fingerprint, err := ParsePairCode(result.Code)
	if err != nil {
		t.Fatal(err)
	}
	if len(token) != 64 || fingerprint != status.Fingerprint {
		t.Fatalf("unexpected pair code: %q %q", token, fingerprint)
	}
	if result.ExpiresInSeconds != 900 {
		t.Fatalf("unexpected expiry: %d", result.ExpiresInSeconds)
	}
	if err := a.MeshLeave(); err == nil {
		t.Fatal("cluster creator was allowed to leave")
	}
	if err := a.MeshRemove("11111111111111111111111111111111"); err == nil {
		t.Fatal("unknown member was removed")
	}
}

func TestLogsTail(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "service.log")
	if err := os.WriteFile(logPath, []byte("one\ntwo\nthree\n"), 0600); err != nil {
		t.Fatal(err)
	}
	a := &Admin{DataDir: dir, LogFile: logPath}
	lines, err := a.Logs("service", 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(lines) != 2 || lines[0] != "two" || lines[1] != "three" {
		t.Fatalf("unexpected tail: %v", lines)
	}
	lines, err = a.Logs("audit", 10)
	if err != nil || len(lines) != 0 {
		t.Fatalf("missing audit log should be empty: %v %v", lines, err)
	}
	if _, err := a.Logs("nope", 10); err == nil {
		t.Fatal("unknown log source accepted")
	}
	a.LogFile = ""
	if _, err := a.Logs("service", 10); err == nil {
		t.Fatal("unconfigured log file accepted")
	}
}

func TestMeshInitSyncsLocalIdentity(t *testing.T) {
	dir := t.TempDir()
	state := cluster.New(cluster.Node{ID: "old-id", Name: "nas", Online: true})
	a := &Admin{DataDir: dir, State: state}
	if err := a.MeshInit("127.0.0.1:38671"); err != nil {
		t.Fatal(err)
	}
	if state.Local().ID == "old-id" || state.Local().Name != "nas" {
		t.Fatalf("local identity was not synced: %+v", state.Local())
	}
	status, err := a.MeshStatus()
	if err != nil || status.NodeID != state.Local().ID {
		t.Fatalf("status does not match local identity: %+v %v", status, err)
	}
}

func TestResetIdentity(t *testing.T) {
	dir := t.TempDir()
	a := &Admin{DataDir: dir}
	if err := a.MeshInit("127.0.0.1:38671"); err != nil {
		t.Fatal(err)
	}
	if err := a.ResetIdentity(); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"mesh.json", "node-id"} {
		if _, err := os.Stat(filepath.Join(dir, name)); !os.IsNotExist(err) {
			t.Fatalf("%s was not removed", name)
		}
	}
	status, err := a.MeshStatus()
	if err != nil || status.Configured {
		t.Fatalf("still configured after reset: %+v %v", status, err)
	}
}
