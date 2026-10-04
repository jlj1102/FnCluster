package mesh

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"fncluster/internal/cluster"
)

func TestRuntimeTracksMeshStateFile(t *testing.T) {
	dir := t.TempDir()
	state := cluster.New(cluster.Node{ID: "99999999999999999999999999999999", Online: true})
	runtime := NewRuntime(dir, "127.0.0.1:0", state, 50*time.Millisecond)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go runtime.Run(ctx)
	if running, _ := runtime.Status(); running {
		t.Fatal("runtime started without mesh state")
	}
	if _, err := Init(dir, "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "127.0.0.1:41001"); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		if running, _ := runtime.Status(); running {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("runtime did not start after mesh state appeared")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err := os.Remove(filepath.Join(dir, "mesh.json")); err != nil {
		t.Fatal(err)
	}
	deadline = time.Now().Add(3 * time.Second)
	for {
		if running, _ := runtime.Status(); !running {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("runtime did not stop after mesh state disappeared")
		}
		time.Sleep(20 * time.Millisecond)
	}
}
