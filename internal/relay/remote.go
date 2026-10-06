package relay

import (
	"context"
	"crypto/tls"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"fncluster/internal/cluster"
)

var appNamePattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]{0,63}$`)

// RemoteHandler serves /relay/<appName>/<path...> on the mTLS mesh listener.
// The caller must already have verified the peer certificate; resolve only
// returns targets that were discovered on this node.
func RemoteHandler(resolve func(string) (cluster.RelayTarget, bool)) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/relay/", func(w http.ResponseWriter, r *http.Request) {
		rest := strings.TrimPrefix(r.URL.Path, "/relay/")
		appName, tail, _ := strings.Cut(rest, "/")
		if !appNamePattern.MatchString(appName) {
			http.NotFound(w, r)
			return
		}
		if appName == GatewayApp {
			tailPath := "/" + tail
			if tail == "" {
				tailPath = "/"
			}
			serveRemoteGateway(w, r, tailPath)
			return
		}
		target, ok := resolve(appName)
		if !ok {
			http.Error(w, "unknown relay target", http.StatusForbidden)
			return
		}
		tailPath := "/" + tail
		if tailPath == "/" {
			tailPath = "/"
		}
		switch target.Kind {
		case "socket":
			proxySocket(w, r, target, tailPath)
		case "cgi":
			serveCGI(w, r, target, tailPath)
		case "port":
			proxyPort(w, r, target, tailPath)
		default:
			http.Error(w, "unsupported relay target", http.StatusBadGateway)
		}
	})
	mux.HandleFunc("/icon/", func(w http.ResponseWriter, r *http.Request) {
		appName := strings.TrimPrefix(r.URL.Path, "/icon/")
		if !ValidAppName(appName) {
			http.NotFound(w, r)
			return
		}
		var data []byte
		if target, ok := resolve(appName); ok && target.IconPath != "" {
			if file, err := os.ReadFile(target.IconPath); err == nil && len(file) > 0 {
				data = file
			}
		}
		if data == nil {
			if file, err := os.ReadFile(filepath.Join(systemIconRoot, appName, "icon.png")); err == nil && len(file) > 0 {
				data = file
			}
		}
		if data == nil {
			data = DefaultIcon()
		}
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write(data)
	})
	return mux
}

// systemIconRoot is a test seam; fnOS serves builtin app icons from here.
var systemIconRoot = "/usr/trim/www/static/app/icons"

// spaRoute lists the SPA-only paths the fnOS gateway itself 404s (its own
// html_nocache fallback covers /, /login, /signin, /system, /oauth and /biz/).
// Everything else - /apps/*, /app/*, /cgi/*, assets - must pass through, or a
// nested iframe app would receive the desktop shell instead.
func spaRoute(path string) bool {
	for _, prefix := range []string{"/appview", "/reset-password", "/debug", "/404"} {
		if path == prefix || strings.HasPrefix(path, prefix+"/") {
			return true
		}
	}
	return false
}

// ValidAppName reports whether a relay app name is safe to route.
func ValidAppName(name string) bool {
	return appNamePattern.MatchString(name)
}

// serveRemoteGateway proxies to the node's own fnOS gateway. Document
// navigations always fetch "/" so the SPA handles client-side routes; the
// gateway itself only serves index.html for a few paths.
func serveRemoteGateway(w http.ResponseWriter, r *http.Request, tailPath string) {
	transport := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{Timeout: 5 * time.Second}).DialContext(ctx, "tcp", "127.0.0.1:5666")
		},
		MaxIdleConns:    8,
		IdleConnTimeout: 90 * time.Second,
	}
	proxy := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.Out.URL.Scheme = "http"
			pr.Out.URL.Host = "127.0.0.1:5666"
			pr.Out.Host = "127.0.0.1:5666"
			pr.Out.URL.Path = tailPath
			if strings.Contains(r.Header.Get("Accept"), "text/html") && spaRoute(tailPath) {
				pr.Out.URL.Path = "/"
			}
			pr.Out.URL.RawQuery = r.URL.RawQuery
			pr.Out.Header.Del("Accept-Encoding")
		},
		Transport:     transport,
		FlushInterval: -1,
		ErrorHandler: func(w http.ResponseWriter, _ *http.Request, err error) {
			http.Error(w, "relay gateway upstream: "+err.Error(), http.StatusBadGateway)
		},
	}
	proxy.ServeHTTP(w, r)
}

// proxyPort forwards to the app's own loopback HTTP port (motrix style). The
// Host header and any loopback absolute redirects are normalized so the app
// sees itself at 127.0.0.1 while the browser stays on the relay origin.
func proxyPort(w http.ResponseWriter, r *http.Request, target cluster.RelayTarget, tailPath string) {
	scheme := target.Scheme
	if scheme != "https" {
		scheme = "http"
	}
	transport := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{Timeout: 5 * time.Second}).DialContext(ctx, "tcp", target.Address)
		},
	}
	if scheme == "https" {
		// Loopback app ports use self-signed certificates by convention.
		transport.TLSClientConfig = &tls.Config{InsecureSkipVerify: target.InsecureTLS} // #nosec G402 - loopback only
	}
	proxy := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.Out.URL.Scheme = scheme
			pr.Out.URL.Host = target.Address
			pr.Out.URL.Path = tailPath
			pr.Out.URL.RawQuery = r.URL.RawQuery
			pr.Out.Host = target.Address
		},
		Transport:     transport,
		FlushInterval: -1,
		ModifyResponse: func(resp *http.Response) error {
			rewriteLoopbackLocation(resp.Header, target.Address)
			return nil
		},
		ErrorHandler: func(w http.ResponseWriter, _ *http.Request, err error) {
			http.Error(w, "relay port: "+err.Error(), http.StatusBadGateway)
		},
	}
	proxy.ServeHTTP(w, r)
}

// rewriteLoopbackLocation turns absolute redirects that point at the app's own
// loopback address into root-relative paths the relay origin can serve.
func rewriteLoopbackLocation(header http.Header, address string) {
	values := header.Values("Location")
	if len(values) == 0 {
		return
	}
	header.Del("Location")
	for _, raw := range values {
		parsed, err := url.Parse(raw)
		if err != nil || parsed.Host == "" {
			header.Add("Location", raw)
			continue
		}
		if parsed.Host == address || cluster.IsLoopbackHost(parsed.Hostname()) {
			parsed.Scheme = ""
			parsed.Host = ""
			header.Add("Location", parsed.String())
			continue
		}
		header.Add("Location", raw)
	}
}

// proxySocket forwards to the app's local unix socket. The path is passed
// through as the browser sent it; socket apps serve both prefixed and
// root-relative paths (fnOS-style gateways forward the original path).
func proxySocket(w http.ResponseWriter, r *http.Request, target cluster.RelayTarget, tailPath string) {
	proxy := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.Out.URL.Scheme = "http"
			pr.Out.URL.Host = "fncluster-relay"
			pr.Out.URL.Path = tailPath
			pr.Out.URL.RawQuery = r.URL.RawQuery
			pr.Out.Host = "fncluster-relay"
		},
		Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{Timeout: 5 * time.Second}).DialContext(ctx, "unix", target.Socket)
		}},
		FlushInterval: -1,
		ErrorHandler: func(w http.ResponseWriter, _ *http.Request, err error) {
			http.Error(w, "relay socket: "+err.Error(), http.StatusBadGateway)
		},
	}
	proxy.ServeHTTP(w, r)
}
