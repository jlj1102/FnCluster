package mesh

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"time"

	"fncluster/internal/cluster"
)

func Sync(ctx context.Context, store *Store, state *cluster.State) {
	poll := func() {
		cfg := store.Snapshot()
		// The controller is the only source of membership and revocation changes.
		if address, ok := cfg.Members[cfg.ControllerID]; ok && cfg.ControllerID != cfg.NodeID {
			node, members, revoked, selfRevoked, err := fetch(cfg, cfg.ControllerID, address)
			if err != nil {
				state.MarkOffline(cfg.ControllerID)
			} else if selfRevoked {
				if markErr := store.MarkLocalRevoked(cfg.ClusterID); markErr != nil {
					log.Printf("mesh revoke self: %v", markErr)
				}
			} else {
				state.UpsertRemote(node)
				if err := store.ApplyControllerSnapshot(cfg.ClusterID, members, revoked); err != nil {
					log.Printf("mesh member sync: %v", err)
				}
			}
		}
		cfg = store.Snapshot()
		if store.RevokedLocal() {
			log.Printf("FnCluster node %s was removed from its cluster; mesh sync stopped", cfg.NodeID)
			return
		}
		state.Reconcile(cfg.Members)
		for id, address := range cfg.Members {
			if id == cfg.NodeID || id == cfg.ControllerID {
				continue
			}
			node, _, _, _, err := fetch(cfg, id, address)
			if err != nil {
				state.MarkOffline(id)
				continue
			}
			state.UpsertRemote(node)
		}
	}
	poll()
	if store.RevokedLocal() {
		return
	}
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			poll()
			if store.RevokedLocal() {
				return
			}
		}
	}
}

func fetch(cfg Config, id, address string) (cluster.Node, map[string]string, map[string]time.Time, bool, error) {
	client, err := Client(cfg, id)
	if err != nil {
		return cluster.Node{}, nil, nil, false, err
	}
	response, err := client.Get("https://" + address + "/v1/snapshot")
	if err != nil {
		return cluster.Node{}, nil, nil, false, err
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusForbidden {
		var removed removedResponse
		if err := json.NewDecoder(io.LimitReader(response.Body, 4<<10)).Decode(&removed); err == nil && removed.Revoked && removed.NodeID == cfg.NodeID && removed.ClusterID == cfg.ClusterID {
			return cluster.Node{}, nil, nil, true, nil
		}
		return cluster.Node{}, nil, nil, false, errors.New("peer rejected snapshot")
	}
	if response.StatusCode != http.StatusOK {
		return cluster.Node{}, nil, nil, false, errors.New("peer rejected snapshot")
	}
	var snapshot PeerSnapshot
	if err := json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&snapshot); err != nil {
		return cluster.Node{}, nil, nil, false, err
	}
	if snapshot.ClusterID != cfg.ClusterID || snapshot.Node.ID != id {
		return cluster.Node{}, nil, nil, false, errors.New("peer identity mismatch")
	}
	return snapshot.Node, snapshot.Members, snapshot.Revoked, false, nil
}
