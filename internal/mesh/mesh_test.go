package mesh

import (
	"context"
	"crypto/tls"
	"net"
	"net/http"
	"testing"
	"time"

	"fncluster/internal/cluster"
)

func TestJoinAndMemberAuthorization(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	seedID := "11111111111111111111111111111111"
	joinID := "22222222222222222222222222222222"
	seedDir, joinDir := t.TempDir(), t.TempDir()
	if _, err := Init(seedDir, seedID, listener.Addr().String()); err != nil {
		t.Fatal(err)
	}
	seed, err := Open(seedDir)
	if err != nil {
		t.Fatal(err)
	}
	tlsConfig, err := seed.TLSConfig()
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{Handler: seed.Handler(func() cluster.Node {
		return cluster.Node{ID: seedID, Online: true, Metrics: cluster.Metrics{CollectedAt: time.Now(), Filesystems: []cluster.Filesystem{{Mount: "/vol1", TotalBytes: 12, UsedBytes: 7}}}}
	}), ReadHeaderTimeout: time.Second}
	go server.Serve(tls.NewListener(listener, tlsConfig))
	defer server.Close()
	// The CLI opens a separate Store while the server is running.
	cli, err := Open(seedDir)
	if err != nil {
		t.Fatal(err)
	}
	token, err := cli.NewToken()
	if err != nil {
		t.Fatal(err)
	}

	if err := JoinCluster(joinDir, joinID, "127.0.0.1:40002", listener.Addr().String(), token, seed.Fingerprint()); err != nil {
		t.Fatalf("join: %v", err)
	}
	if seed.Snapshot().Members[joinID] != "127.0.0.1:40002" {
		t.Fatal("joined member was not persisted")
	}
	joined, err := Open(joinDir)
	if err != nil {
		t.Fatal(err)
	}
	client, err := Client(joined.Snapshot(), seedID)
	if err != nil {
		t.Fatal(err)
	}
	response, err := client.Get("https://" + listener.Addr().String() + "/v1/snapshot")
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("joined member got %d", response.StatusCode)
	}
	state := cluster.New(cluster.Node{ID: joinID, Online: true})
	state.UpdateLocal(cluster.Metrics{CollectedAt: time.Now(), Filesystems: []cluster.Filesystem{{Mount: "/vol1", TotalBytes: 8, UsedBytes: 2}}})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go Sync(ctx, joined, state)
	deadline := time.Now().Add(time.Second)
	for state.Summary().Nodes.Total != 2 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if got := state.Summary(); got.Nodes.Online != 2 || got.Storage.TotalBytes != 20 || got.Storage.UsedBytes != 9 {
		t.Fatalf("remote storage was not aggregated: %+v", got)
	}
	unauthenticated := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}}
	response, err = unauthenticated.Get("https://" + listener.Addr().String() + "/v1/snapshot")
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusForbidden {
		t.Fatalf("unauthenticated client got %d", response.StatusCode)
	}
	if err := JoinCluster(t.TempDir(), "33333333333333333333333333333333", "127.0.0.1:40003", listener.Addr().String(), token, seed.Fingerprint()); err == nil {
		t.Fatal("one-time token was reused")
	}
}

func TestMemberRemovalRevokesCertificate(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	seedID := "44444444444444444444444444444444"
	joinID := "55555555555555555555555555555555"
	seedDir, joinDir := t.TempDir(), t.TempDir()
	if _, err := Init(seedDir, seedID, listener.Addr().String()); err != nil {
		t.Fatal(err)
	}
	seed, err := Open(seedDir)
	if err != nil {
		t.Fatal(err)
	}
	tlsConfig, err := seed.TLSConfig()
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{Handler: seed.Handler(func() cluster.Node {
		return cluster.Node{ID: seedID, Online: true}
	}), ReadHeaderTimeout: time.Second}
	go server.Serve(tls.NewListener(listener, tlsConfig))
	defer server.Close()
	cli, err := Open(seedDir)
	if err != nil {
		t.Fatal(err)
	}
	token, err := cli.NewToken()
	if err != nil {
		t.Fatal(err)
	}
	if err := JoinCluster(joinDir, joinID, "127.0.0.1:40012", listener.Addr().String(), token, seed.Fingerprint()); err != nil {
		t.Fatal(err)
	}
	joined, err := Open(joinDir)
	if err != nil {
		t.Fatal(err)
	}
	client, err := Client(joined.Snapshot(), seedID)
	if err != nil {
		t.Fatal(err)
	}
	response, err := client.Get("https://" + listener.Addr().String() + "/v1/snapshot")
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("member before removal got %d", response.StatusCode)
	}
	if err := cli.Remove(joinID); err != nil {
		t.Fatal(err)
	}
	response, err = client.Get("https://" + listener.Addr().String() + "/v1/snapshot")
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusForbidden {
		t.Fatalf("revoked member got %d", response.StatusCode)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go Sync(ctx, joined, cluster.New(cluster.Node{ID: joinID, Online: true}))
	deadline := time.Now().Add(3 * time.Second)
	for !joined.RevokedLocal() {
		if time.Now().After(deadline) {
			t.Fatal("removed node did not learn its revocation")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if _, still := joined.Snapshot().Members[joinID]; still {
		t.Fatal("revoked member remained in the member table")
	}
	if _, err := Open(joinDir); err != nil {
		t.Fatalf("revoked node state should stay readable: %v", err)
	}
	newToken, err := cli.NewToken()
	if err != nil {
		t.Fatal(err)
	}
	if err := JoinCluster(t.TempDir(), joinID, "127.0.0.1:40013", listener.Addr().String(), newToken, seed.Fingerprint()); err == nil {
		t.Fatal("revoked node ID was allowed to rejoin")
	}
}
