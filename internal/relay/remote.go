package relay

import (
	"context"
	"net"
	"net/http"
	"net/http/httputil"
	"os"
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
		target, ok := resolve(appName)
		if !ok {
			http.Error(w, "unknown relay target", http.StatusForbidden)
			return
		}
		data := DefaultIcon()
		if target.IconPath != "" {
			if file, err := os.ReadFile(target.IconPath); err == nil && len(file) > 0 {
				data = file
			}
		}
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write(data)
	})
	return mux
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
			if strings.Contains(r.Header.Get("Accept"), "text/html") {
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
