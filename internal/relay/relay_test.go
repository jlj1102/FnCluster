package relay

import (
	"bufio"
	"bytes"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"fncluster/internal/cluster"
)

func TestSessionsMintLookupAndExpire(t *testing.T) {
	sessions := NewSessions(20 * time.Millisecond)
	token := sessions.Mint("node-a", "tailscale", "")
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
	first := sessions.Mint("node-a", "tailscale", "")
	if again := sessions.Mint("node-a", "tailscale", ""); again != first {
		t.Fatal("same target minted a new token")
	}
	second := sessions.Mint("node-a", "docker", "")
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

	third := sessions.Mint("node-b", "photos", "")
	if _, ok := sessions.AssignPort(third, origins); ok {
		t.Fatal("exhausted origins still assigned a port")
	}
}

func TestOriginLandingRedirectsToDesktopRoot(t *testing.T) {
	sessions := NewSessions(time.Hour)
	token := sessions.Mint("node-a", GatewayApp, "")
	if _, ok := sessions.AssignPort(token, []int{6001}); !ok {
		t.Fatal("port assignment failed")
	}
	server := &Server{TTL: time.Hour, Sessions: sessions}
	request := httptest.NewRequest(http.MethodGet, "/open/"+token+"?anchor=trim.download-center", nil)
	response := httptest.NewRecorder()
	server.originHandler(6001).ServeHTTP(response, request)
	if response.Code != http.StatusFound {
		t.Fatalf("landing status = %d", response.Code)
	}
	if got := response.Header().Get("Location"); got != "/?fncluster_relay_anchor=trim.download-center" {
		t.Fatalf("landing = %q", got)
	}
}

func TestOriginLandingKeepsTheme(t *testing.T) {
	sessions := NewSessions(time.Hour)
	token := sessions.Mint("node-a", GatewayApp, "")
	if _, ok := sessions.AssignPort(token, []int{6001}); !ok {
		t.Fatal("port assignment failed")
	}
	server := &Server{TTL: time.Hour, Sessions: sessions}
	request := httptest.NewRequest(http.MethodGet, "/open/"+token+"?anchor=trim.download-center&fncluster_theme=dark", nil)
	response := httptest.NewRecorder()
	server.originHandler(6001).ServeHTTP(response, request)
	if got := response.Header().Get("Location"); got != "/?fncluster_relay_anchor=trim.download-center&fncluster_theme=dark" {
		t.Fatalf("landing = %q", got)
	}
	request = httptest.NewRequest(http.MethodGet, "/open/"+token+"?anchor=trim.download-center&fncluster_theme=evil", nil)
	response = httptest.NewRecorder()
	server.originHandler(6001).ServeHTTP(response, request)
	if got := response.Header().Get("Location"); got != "/?fncluster_relay_anchor=trim.download-center" {
		t.Fatalf("invalid theme not dropped: %q", got)
	}
}

func TestOriginHandlerRejectsForeignPort(t *testing.T) {
	sessions := NewSessions(time.Hour)
	token := sessions.Mint("node-a", "tailscale", "")
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

func TestGatewayCookieNamespace(t *testing.T) {
	if prefix := CookiePrefix("http://192.168.1.27:5666/"); prefix != "fncluster-192.168.1.27-" {
		t.Fatalf("prefix = %q", prefix)
	}
	if prefix := CookiePrefix("not a url"); prefix != "" {
		t.Fatalf("garbage prefix = %q", prefix)
	}

	header := http.Header{}
	header.Add("Set-Cookie", "fnos-token=abc; Path=/; SameSite=Strict")
	header.Add("Set-Cookie", "other=1; Domain=192.168.1.27; Path=/")
	prefixResponseCookies(header, "fncluster-192.168.1.27-")
	values := header.Values("Set-Cookie")
	if len(values) != 2 || !strings.HasPrefix(values[0], "fncluster-192.168.1.27-fnos-token=") {
		t.Fatalf("Set-Cookie rewrite = %v", values)
	}
	for _, value := range values {
		if strings.Contains(strings.ToLower(value), "domain=") {
			t.Fatalf("domain attribute kept: %q", value)
		}
	}

	request := http.Header{}
	request.Add("Cookie", "fncluster-192.168.1.27-fnos-token=abc; "+cookieName(38673)+"=session; fnos-token=local; other=1")
	rewriteGatewayRequestCookies(request, "fncluster-192.168.1.27-", cookieName(38673))
	got := strings.Join(request.Values("Cookie"), " ")
	if !strings.Contains(got, "fnos-token=abc") {
		t.Fatalf("remote cookie not forwarded: %q", got)
	}
	if strings.Contains(got, "fncluster-") || strings.Contains(got, "local") || strings.Contains(got, "other=1") {
		t.Fatalf("local or session cookie leaked upstream: %q", got)
	}
}

func TestSpaRouteWhitelist(t *testing.T) {
	for _, path := range []string{"/appview", "/appview/", "/reset-password", "/debug", "/404"} {
		if !spaRoute(path) {
			t.Fatalf("%s should fall back to the SPA shell", path)
		}
	}
	for _, path := range []string{"/", "/login", "/apps/download/index.html", "/app/fncluster/", "/assets/index.js", "/cgi/ThirdParty/tailscale/index.cgi"} {
		if spaRoute(path) {
			t.Fatalf("%s must pass through untouched", path)
		}
	}
}

func TestRemoteIconServesSystemIcon(t *testing.T) {
	original := systemIconRoot
	systemIconRoot = t.TempDir()
	defer func() { systemIconRoot = original }()
	dir := filepath.Join(systemIconRoot, "trim.download-center")
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "icon.png"), []byte("pngbytes"), 0644); err != nil {
		t.Fatal(err)
	}
	handler := RemoteHandler(func(string) (cluster.RelayTarget, bool) { return cluster.RelayTarget{}, false })
	request := httptest.NewRequest(http.MethodGet, "/icon/trim.download-center", nil)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK || response.Body.String() != "pngbytes" {
		t.Fatalf("system icon = %d %q", response.Code, response.Body.String())
	}
}

func TestInjectShim(t *testing.T) {
	body := []byte("<html><head><title>x</title></head><body></body></html>")
	out := string(injectShim(body, "fncluster-192.168.1.27-"))
	if !strings.Contains(out, "<head><style>") || !strings.Contains(out, "fncluster-192.168.1.27-") {
		t.Fatalf("shim not injected: %s", out)
	}
	if !strings.Contains(out, "fncluster_relay_anchor") || !strings.Contains(out, "mod.wa") {
		t.Fatalf("anchor bootstrap missing: %s", out)
	}
	if !strings.Contains(out, "markAppview") || !strings.Contains(out, "fncluster-appview-window") || !strings.Contains(out, "visibility") {
		t.Fatalf("appview header marker missing: %s", out)
	}
	if !strings.Contains(out, "resizable-handler") {
		t.Fatalf("body marker must skip resize handles: %s", out)
	}
	if !strings.Contains(out, "fncluster:theme") || !strings.Contains(out, "os-theme-mode") {
		t.Fatalf("theme bridge missing: %s", out)
	}
	if string(injectShim(body, "")) != string(body) {
		t.Fatal("empty prefix must not change the body")
	}
}

func TestCompositeIconAddsBadge(t *testing.T) {
	base := image.NewRGBA(image.Rect(0, 0, 32, 32))
	draw.Draw(base, base.Bounds(), image.NewUniform(color.RGBA{R: 10, G: 200, B: 10, A: 255}), image.Point{}, draw.Src)
	var buffer bytes.Buffer
	if err := png.Encode(&buffer, base); err != nil {
		t.Fatal(err)
	}
	out, err := compositeIcon(buffer.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	img, err := png.Decode(bytes.NewReader(out))
	if err != nil {
		t.Fatal(err)
	}
	if img.Bounds().Dx() != iconSize || img.Bounds().Dy() != iconSize {
		t.Fatalf("composited size = %v", img.Bounds())
	}
	r, _, _, _ := img.At(iconSize-2, iconSize-2).RGBA()
	if r < 0xf000 {
		t.Fatalf("badge corner not drawn: %v", r)
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
