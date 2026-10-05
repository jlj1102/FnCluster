package relay

import (
	"bufio"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"fncluster/internal/cluster"
)

func TestSessionsMintLookupAndExpire(t *testing.T) {
	sessions := NewSessions(20 * time.Millisecond)
	token := sessions.Mint("node-a", "tailscale")
	entry, ok := sessions.Lookup(token)
	if !ok || entry.NodeID != "node-a" || entry.AppName != "tailscale" {
		t.Fatalf("minted session not found: %+v %v", entry, ok)
	}
	time.Sleep(30 * time.Millisecond)
	if _, ok := sessions.Lookup(token); ok {
		t.Fatal("expired session still valid")
	}
}

func TestSessionsReuseAndPortAssignment(t *testing.T) {
	sessions := NewSessions(time.Hour)
	first := sessions.Mint("node-a", "tailscale")
	if again := sessions.Mint("node-a", "tailscale"); again != first {
		t.Fatal("same target minted a new token")
	}
	second := sessions.Mint("node-a", "docker")
	origins := []int{5001, 5002}

	port, ok := sessions.AssignPort(first, origins)
	if !ok || port != 5001 {
		t.Fatalf("first assignment = %d, %v", port, ok)
	}
	if again, ok := sessions.AssignPort(first, origins); !ok || again != port {
		t.Fatalf("same token moved ports: %d, %v", again, ok)
	}
	other, ok := sessions.AssignPort(second, origins)
	if !ok || other == port {
		t.Fatalf("two sessions share a port: %d, %v", other, ok)
	}

	third := sessions.Mint("node-b", "photos")
	if _, ok := sessions.AssignPort(third, origins); ok {
		t.Fatal("exhausted origins still assigned a port")
	}
}

func TestOriginHandlerRejectsForeignPort(t *testing.T) {
	sessions := NewSessions(time.Hour)
	token := sessions.Mint("node-a", "tailscale")
	if _, ok := sessions.AssignPort(token, []int{6001}); !ok {
		t.Fatal("port assignment failed")
	}
	server := &Server{TTL: time.Hour, Sessions: sessions}
	request := httptest.NewRequest(http.MethodGet, "/", nil)
	request.AddCookie(&http.Cookie{Name: cookieName(6002), Value: token})
	response := httptest.NewRecorder()
	server.originHandler(6002).ServeHTTP(response, request)
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("foreign port returned %d", response.Code)
	}
}

func TestParseCGIHeaders(t *testing.T) {
	input := "Status: 201 Created\r\nContent-Type: text/html\r\nSet-Cookie: a=1\r\nSet-Cookie: b=2\r\nConnection: close\r\n\r\nbody"
	status, headers, err := parseCGIHeaders(bufio.NewReader(strings.NewReader(input)))
	if err != nil || status != http.StatusCreated {
		t.Fatalf("status = %d, %v", status, err)
	}
	if headers.Get("Content-Type") != "text/html" || len(headers.Values("Set-Cookie")) != 2 {
		t.Fatalf("headers = %+v", headers)
	}
	if headers.Get("Connection") != "" {
		t.Fatal("hop-by-hop header kept")
	}
}

func TestStripRelayCookieKeepsAppCookies(t *testing.T) {
	request := httptest.NewRequest(http.MethodGet, "/", nil)
	request.AddCookie(&http.Cookie{Name: cookieName(38673), Value: "one"})
	request.AddCookie(&http.Cookie{Name: cookieName(38674), Value: "two"})
	request.AddCookie(&http.Cookie{Name: "ts-session", Value: "app"})
	stripRelayCookie(request)
	cookies := request.Cookies()
	if len(cookies) != 1 || cookies[0].Name != "ts-session" {
		t.Fatalf("unexpected cookies: %+v", cookies)
	}
}

func TestRemoteHandlerRejectsUnknownTarget(t *testing.T) {
	handler := RemoteHandler(func(string) (cluster.RelayTarget, bool) { return cluster.RelayTarget{}, false })
	request := httptest.NewRequest(http.MethodGet, "/relay/tailscale/", nil)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusForbidden {
		t.Fatalf("unknown target returned %d", response.Code)
	}
	request = httptest.NewRequest(http.MethodGet, "/relay/bad%20name/", nil)
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusNotFound {
		t.Fatalf("invalid app name returned %d", response.Code)
	}
}
