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
	request.AddCookie(&http.Cookie{Name: CookieName, Value: "secret"})
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
