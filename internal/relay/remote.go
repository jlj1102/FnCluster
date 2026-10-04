package relay

import (
	"context"
	"net"
	"net/http"
	"net/http/httputil"
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
	return mux
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
