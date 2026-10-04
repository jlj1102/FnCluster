// Package relay lets a local, gateway-authenticated desktop open remote fnOS
// app panels without a second login on the remote node. The browser loads the
// local relay listener with a short unguessable session token; the listener
// proxies over the mTLS mesh to the owning node, which talks to the app's local
// unix socket or runs its CGI binary.
package relay

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"sync"
	"time"

	"fncluster/internal/mesh"
)

// CookieName carries the relay session for the root-path proxy.
const CookieName = "fncluster_relay"

type session struct {
	NodeID  string
	AppName string
	Expires time.Time
}

// Sessions maps unguessable tokens to relay targets with a fixed TTL.
type Sessions struct {
	ttl time.Duration

	mu      sync.Mutex
	entries map[string]session
}

func NewSessions(ttl time.Duration) *Sessions {
	if ttl <= 0 {
		ttl = 12 * time.Hour
	}
	return &Sessions{ttl: ttl, entries: map[string]session{}}
}

func (s *Sessions) Mint(nodeID, appName string) string {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		panic(err) // crypto/rand failure is not recoverable
	}
	token := hex.EncodeToString(buf)
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	for key, entry := range s.entries {
		if now.After(entry.Expires) {
			delete(s.entries, key)
		}
	}
	s.entries[token] = session{NodeID: nodeID, AppName: appName, Expires: now.Add(s.ttl)}
	return token
}

func (s *Sessions) Lookup(token string) (session, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	entry, ok := s.entries[token]
	if !ok || time.Now().After(entry.Expires) {
		if ok {
			delete(s.entries, token)
		}
		return session{}, false
	}
	return entry, true
}

// Server is the browser-facing relay listener (plain HTTP on the LAN,
// protected by the session cookie).
type Server struct {
	DataDir  string
	TTL      time.Duration
	Sessions *Sessions
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/open/", s.handleOpen)
	mux.HandleFunc("/", s.handleProxy)
	return mux
}

// Serve runs until ctx is done. The HTTP server has no write timeout so CGI
// downloads and WebSocket upgrades are not cut off.
func (s *Server) Serve(ctx context.Context, listen string) error {
	server := &http.Server{
		Handler:           s.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       2 * time.Minute,
	}
	listener, err := net.Listen("tcp", listen)
	if err != nil {
		return err
	}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdownCtx)
	}()
	err = server.Serve(listener)
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

func (s *Server) handleOpen(w http.ResponseWriter, r *http.Request) {
	token := r.URL.Path[len("/open/"):]
	if _, ok := s.Sessions.Lookup(token); !ok {
		http.Error(w, "relay session not found or expired", http.StatusUnauthorized)
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name:     CookieName,
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   int(s.TTL.Seconds()),
	})
	http.Redirect(w, r, "/", http.StatusFound)
}

func (s *Server) handleProxy(w http.ResponseWriter, r *http.Request) {
	cookie, err := r.Cookie(CookieName)
	if err != nil {
		http.Error(w, "relay session required", http.StatusUnauthorized)
		return
	}
	entry, ok := s.Sessions.Lookup(cookie.Value)
	if !ok {
		http.Error(w, "relay session expired", http.StatusUnauthorized)
		return
	}
	store, err := mesh.Open(s.DataDir)
	if err != nil {
		http.Error(w, "mesh is not configured", http.StatusServiceUnavailable)
		return
	}
	cfg := store.Snapshot()
	address, ok := cfg.Members[entry.NodeID]
	if !ok {
		http.Error(w, "node is not a cluster member", http.StatusForbidden)
		return
	}
	client, err := mesh.RelayClient(cfg, entry.NodeID)
	if err != nil {
		http.Error(w, "relay client: "+err.Error(), http.StatusBadGateway)
		return
	}
	proxy := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.Out.URL.Scheme = "https"
			pr.Out.URL.Host = address
			pr.Out.URL.Path = "/relay/" + url.PathEscape(entry.AppName) + r.URL.Path
			pr.Out.URL.RawQuery = r.URL.RawQuery
			pr.Out.Host = address
			stripRelayCookie(pr.Out)
		},
		Transport:     client.Transport,
		FlushInterval: -1,
		ErrorHandler: func(w http.ResponseWriter, _ *http.Request, err error) {
			http.Error(w, "relay upstream: "+err.Error(), http.StatusBadGateway)
		},
	}
	proxy.ServeHTTP(w, r)
}

// stripRelayCookie keeps app cookies (which round-trip through the relay origin)
// but never leaks our own session token to the remote app.
func stripRelayCookie(req *http.Request) {
	cookies := req.Cookies()
	if len(cookies) == 0 {
		return
	}
	kept := make([]*http.Cookie, 0, len(cookies))
	for _, cookie := range cookies {
		if cookie.Name != CookieName {
			kept = append(kept, cookie)
		}
	}
	req.Header.Del("Cookie")
	for _, cookie := range kept {
		req.AddCookie(cookie)
	}
}
