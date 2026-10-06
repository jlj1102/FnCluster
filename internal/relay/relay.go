// Package relay lets a local, gateway-authenticated desktop open remote fnOS
// app panels without a second login on the remote node. The browser loads a
// per-session relay origin protected by an unguessable token; the listener
// proxies over the mTLS mesh to the owning node, which talks to the app's local
// unix socket or runs its CGI binary.
//
// Each concurrently opened app gets its own origin port so the session cookies
// of two apps can never overwrite each other.
package relay

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"fncluster/internal/mesh"
)

// Cookies are host-scoped, not port-scoped, so every origin port uses its own
// cookie name: two open apps can never overwrite each other's session.
const cookiePrefix = "fncluster_relay_"

func cookieName(port int) string {
	return cookiePrefix + strconv.Itoa(port)
}

// GatewayApp is the reserved target that proxies the remote node's whole fnOS
// gateway (desktop shell, APIs and WebSockets) so native panels open inside a
// same-site iframe without the cross-site cookie wall.
const GatewayApp = "fncluster.gateway"

type session struct {
	NodeID  string
	AppName string
	Prefix  string
	Port    int
	Expires time.Time
}

// Sessions maps unguessable tokens to relay targets with a sliding TTL.
// Origin ports are assigned lazily on the first /open/ visit, so only apps the
// user actually opened occupy a port.
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

// Mint returns a token for the target, reusing an existing session for the same
// node+app so a desktop reload never consumes another port. prefix is the
// remote hostname cookie namespace used by gateway sessions.
func (s *Sessions) Mint(nodeID, appName, prefix string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	s.entries = sweep(s.entries, now)
	for token, entry := range s.entries {
		if entry.NodeID == nodeID && entry.AppName == appName {
			entry.Expires = now.Add(s.ttl)
			if prefix != "" {
				entry.Prefix = prefix
			}
			s.entries[token] = entry
			return token
		}
	}
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		panic(err) // crypto/rand failure is not recoverable
	}
	token := hex.EncodeToString(buf)
	s.entries[token] = session{NodeID: nodeID, AppName: appName, Prefix: prefix, Expires: now.Add(s.ttl)}
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

// Touch slides the session TTL while the app is actively used.
func (s *Sessions) Touch(token string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	entry, ok := s.entries[token]
	if !ok {
		return
	}
	entry.Expires = time.Now().Add(s.ttl)
	s.entries[token] = entry
}

// AssignPort binds the session to one free origin port. An already bound
// session keeps its port; exhausted origins return ok=false.
func (s *Sessions) AssignPort(token string, origins []int) (int, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	entry, ok := s.entries[token]
	if !ok || time.Now().After(entry.Expires) {
		delete(s.entries, token)
		return 0, false
	}
	if entry.Port != 0 {
		return entry.Port, true
	}
	used := map[int]bool{}
	now := time.Now()
	for _, other := range s.entries {
		if other.Port != 0 && now.Before(other.Expires) {
			used[other.Port] = true
		}
	}
	for _, port := range origins {
		if !used[port] {
			entry.Port = port
			s.entries[token] = entry
			return port, true
		}
	}
	return 0, false
}

func sweep(entries map[string]session, now time.Time) map[string]session {
	for token, entry := range entries {
		if now.After(entry.Expires) {
			delete(entries, token)
		}
	}
	return entries
}

// Server runs the portal listener plus a fixed number of origin listeners.
type Server struct {
	DataDir  string
	TTL      time.Duration
	Sessions *Sessions
	Ports    int

	iconMu    sync.Mutex
	iconCache map[string]iconEntry
}

// Serve runs until ctx is done. The portal serves /open/<token>; each origin
// port hosts exactly one open app so cookies stay isolated per app.
func (s *Server) Serve(ctx context.Context, listen string) error {
	host, portalPortText, err := net.SplitHostPort(listen)
	if err != nil {
		return err
	}
	portalPort, err := strconv.Atoi(portalPortText)
	if err != nil {
		return err
	}
	origins := make([]int, 0, s.Ports)
	for i := 1; i <= s.Ports; i++ {
		origins = append(origins, portalPort+i)
	}

	type endpoint struct {
		server   *http.Server
		listener net.Listener
	}
	newEndpoint := func(listener net.Listener, handler http.Handler) endpoint {
		return endpoint{
			server: &http.Server{
				Handler:           handler,
				ReadHeaderTimeout: 10 * time.Second,
				IdleTimeout:       2 * time.Minute,
			},
			listener: listener,
		}
	}

	portal, err := net.Listen("tcp", listen)
	if err != nil {
		return err
	}
	endpoints := []endpoint{newEndpoint(portal, s.portalHandler(origins))}
	for _, port := range origins {
		listener, listenErr := net.Listen("tcp", net.JoinHostPort(host, strconv.Itoa(port)))
		if listenErr != nil {
			log.Printf("relay: origin port %d unavailable: %v", port, listenErr)
			continue
		}
		endpoints = append(endpoints, newEndpoint(listener, s.originHandler(port)))
	}
	log.Printf("relay: portal %s, %d origin ports %d-%d", listen, len(endpoints)-1, portalPort+1, portalPort+s.Ports)

	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		for _, ep := range endpoints {
			_ = ep.server.Shutdown(shutdownCtx)
		}
	}()
	errCh := make(chan error, len(endpoints))
	for _, ep := range endpoints {
		go func(ep endpoint) {
			if serveErr := ep.server.Serve(ep.listener); serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) {
				errCh <- serveErr
			}
		}(ep)
	}
	select {
	case <-ctx.Done():
		return nil
	case serveErr := <-errCh:
		return serveErr
	}
}

func (s *Server) portalHandler(origins []int) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/open/") {
			http.NotFound(w, r)
			return
		}
		token := r.URL.Path[len("/open/"):]
		if _, ok := s.Sessions.Lookup(token); !ok {
			http.Error(w, "relay session not found or expired", http.StatusUnauthorized)
			return
		}
		port, ok := s.Sessions.AssignPort(token, origins)
		if !ok {
			http.Error(w, "relay is at capacity: close another relayed app first", http.StatusServiceUnavailable)
			return
		}
		target := "http://" + net.JoinHostPort(hostOnly(r.Host), strconv.Itoa(port)) + "/open/" + token
		if r.URL.RawQuery != "" {
			target += "?" + r.URL.RawQuery
		}
		http.Redirect(w, r, target, http.StatusFound)
	})
}

func (s *Server) originHandler(port int) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/open/") {
			token := r.URL.Path[len("/open/"):]
			entry, ok := s.Sessions.Lookup(token)
			if !ok || entry.Port != port {
				http.Error(w, "relay session not found or expired", http.StatusUnauthorized)
				return
			}
			http.SetCookie(w, &http.Cookie{
				Name:     cookieName(port),
				Value:    token,
				Path:     "/",
				HttpOnly: true,
				SameSite: http.SameSiteLaxMode,
				MaxAge:   int(s.TTL.Seconds()),
			})
			landing := "/"
			if anchor := r.URL.Query().Get("anchor"); anchor != "" {
				landing = "/appview?anchor=" + url.QueryEscape(anchor)
			}
			http.Redirect(w, r, landing, http.StatusFound)
			return
		}
		cookie, err := r.Cookie(cookieName(port))
		if err != nil {
			http.Error(w, "relay session required", http.StatusUnauthorized)
			return
		}
		entry, ok := s.Sessions.Lookup(cookie.Value)
		if !ok || entry.Port != port {
			http.Error(w, "relay session expired", http.StatusUnauthorized)
			return
		}
		s.Sessions.Touch(cookie.Value)
		if entry.AppName == GatewayApp {
			s.proxyGateway(w, r, entry)
			return
		}
		s.proxy(w, r, entry)
	})
}

func (s *Server) proxy(w http.ResponseWriter, r *http.Request, entry session) {
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

func hostOnly(host string) string {
	name, _, err := net.SplitHostPort(host)
	if err != nil {
		return host
	}
	return name
}

// stripRelayCookie keeps app cookies (which round-trip through the relay origin)
// but never leaks our own session tokens to the remote app. Every origin port's
// cookie shares the prefix, and all of them are dropped.
func stripRelayCookie(req *http.Request) {
	cookies := req.Cookies()
	if len(cookies) == 0 {
		return
	}
	kept := make([]*http.Cookie, 0, len(cookies))
	for _, cookie := range cookies {
		if !strings.HasPrefix(cookie.Name, cookiePrefix) {
			kept = append(kept, cookie)
		}
	}
	req.Header.Del("Cookie")
	for _, cookie := range kept {
		req.AddCookie(cookie)
	}
}
