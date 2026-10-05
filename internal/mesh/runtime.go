package mesh

import (
	"context"
	"crypto/tls"
	"errors"
	"log"
	"net"
	"net/http"
	"os"
	"sync"
	"time"

	"fncluster/internal/cluster"
)

// Runtime starts and stops the mTLS listener and peer sync as mesh.json
// appears or disappears, so UI/CLI init and join need no service restart.
type Runtime struct {
	dataDir  string
	listen   string
	interval time.Duration
	state    *cluster.State

	mu      sync.Mutex
	current *running
	relay   http.Handler
}

// SetRelay installs the /relay/ handler on the mTLS listener. It is only
// reachable by verified cluster members.
func (r *Runtime) SetRelay(handler http.Handler) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.relay = handler
}

type running struct {
	clusterID string
	nodeID    string
	listener  net.Listener
	server    *http.Server
	cancel    context.CancelFunc
	addr      string
}

func NewRuntime(dataDir, listen string, state *cluster.State, interval time.Duration) *Runtime {
	if interval <= 0 {
		interval = 2 * time.Second
	}
	return &Runtime{dataDir: dataDir, listen: listen, interval: interval, state: state}
}

func (r *Runtime) Run(ctx context.Context) {
	r.tick(ctx)
	ticker := time.NewTicker(r.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			r.Stop()
			return
		case <-ticker.C:
			r.tick(ctx)
		}
	}
}

func (r *Runtime) Status() (bool, string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.current == nil {
		return false, ""
	}
	return true, r.current.addr
}

func (r *Runtime) tick(ctx context.Context) {
	store, err := Open(r.dataDir)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			log.Printf("mesh runtime: %v", err)
		}
		r.Stop()
		return
	}
	if store.RevokedLocal() {
		r.Stop()
		return
	}
	cfg := store.Snapshot()
	r.mu.Lock()
	same := r.current != nil && r.current.clusterID == cfg.ClusterID && r.current.nodeID == cfg.NodeID
	r.mu.Unlock()
	if same {
		return
	}
	r.Stop()
	if err := r.start(ctx, store, cfg); err != nil {
		log.Printf("mesh runtime: %v", err)
	}
}

func (r *Runtime) start(parent context.Context, store *Store, cfg Config) error {
	tlsConfig, err := store.TLSConfig()
	if err != nil {
		return err
	}
	listener, err := net.Listen("tcp", r.listen)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithCancel(parent)
	r.mu.Lock()
	relayHandler := r.relay
	r.mu.Unlock()
	handler := store.Handler(r.state.Local)
	if relayHandler != nil {
		guarded := http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			if !store.authorized(req) {
				http.Error(w, "mTLS member required", http.StatusForbidden)
				return
			}
			relayHandler.ServeHTTP(w, req)
		})
		outer := http.NewServeMux()
		outer.Handle("/relay/", guarded)
		outer.Handle("/icon/", guarded)
		outer.Handle("/", handler)
		handler = outer
	}
	// No read/write deadlines: relay responses (CGI, WebSocket) may stay open.
	server := &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       2 * time.Minute,
	}
	go func() {
		if err := server.Serve(tls.NewListener(listener, tlsConfig)); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Printf("mesh listener: %v", err)
		}
	}()
	go Sync(ctx, store, r.state)
	r.mu.Lock()
	r.current = &running{clusterID: cfg.ClusterID, nodeID: cfg.NodeID, listener: listener, server: server, cancel: cancel, addr: listener.Addr().String()}
	r.mu.Unlock()
	log.Printf("FnCluster mTLS mesh listening on %s", listener.Addr())
	return nil
}

func (r *Runtime) Stop() {
	r.mu.Lock()
	current := r.current
	r.current = nil
	r.mu.Unlock()
	if current == nil {
		return
	}
	current.cancel()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = current.server.Shutdown(shutdownCtx)
	r.state.Reconcile(map[string]string{})
	log.Printf("FnCluster mTLS mesh stopped")
}
