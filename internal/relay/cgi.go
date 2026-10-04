package relay

import (
	"bufio"
	"bytes"
	"context"
	"io"
	"log"
	"net"
	"net/http"
	"net/textproto"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"fncluster/internal/cluster"
)

// cgiSem caps concurrent CGI processes per node; each request spawns one.
var cgiSem = make(chan struct{}, 4)

const cgiTimeout = 60 * time.Second

// serveCGI runs the app's CGI binary with a standard CGI environment and
// streams its response back. This is how fnOS itself serves apps whose
// ui/config points at /cgi/ThirdParty/<app>/index.cgi.
func serveCGI(w http.ResponseWriter, r *http.Request, target cluster.RelayTarget, tailPath string) {
	ctx, cancel := context.WithTimeout(r.Context(), cgiTimeout)
	defer cancel()
	select {
	case cgiSem <- struct{}{}:
		defer func() { <-cgiSem }()
	case <-ctx.Done():
		http.Error(w, "relay is busy", http.StatusServiceUnavailable)
		return
	}

	cmd := exec.CommandContext(ctx, target.CGIPath)
	cmd.Dir = filepath.Dir(target.CGIPath)
	cmd.Env = cgiEnv(r, target, tailPath)
	cmd.Stdin = r.Body
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		http.Error(w, "relay cgi: "+err.Error(), http.StatusInternalServerError)
		return
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		http.Error(w, "relay cgi start: "+err.Error(), http.StatusBadGateway)
		return
	}
	reader := bufio.NewReader(stdout)
	status, headers, err := parseCGIHeaders(reader)
	if err != nil {
		_ = cmd.Wait()
		log.Printf("relay cgi %s: %v (stderr: %s)", target.CGIPath, err, strings.TrimSpace(stderr.String()))
		http.Error(w, "relay cgi headers: "+err.Error(), http.StatusBadGateway)
		return
	}
	for name, values := range headers {
		for _, value := range values {
			w.Header().Add(name, value)
		}
	}
	w.WriteHeader(status)
	if _, err := io.Copy(w, reader); err != nil {
		log.Printf("relay cgi %s: body: %v", target.CGIPath, err)
	}
	if err := cmd.Wait(); err != nil && ctx.Err() == nil {
		log.Printf("relay cgi %s: exit: %v (stderr: %s)", target.CGIPath, err, strings.TrimSpace(stderr.String()))
	}
}

func parseCGIHeaders(reader *bufio.Reader) (int, http.Header, error) {
	headers := http.Header{}
	status := http.StatusOK
	tp := textproto.NewReader(reader)
	for {
		line, err := tp.ReadLine()
		if err != nil {
			return 0, nil, err
		}
		if line == "" {
			break
		}
		name, value, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		name = textproto.CanonicalMIMEHeaderKey(strings.TrimSpace(name))
		value = strings.TrimSpace(value)
		if name == "Status" {
			if fields := strings.Fields(value); len(fields) > 0 {
				if code, convErr := strconv.Atoi(fields[0]); convErr == nil {
					status = code
				}
			}
			continue
		}
		switch name {
		case "Connection", "Keep-Alive", "Transfer-Encoding", "Upgrade", "Te", "Trailer", "Proxy-Authenticate", "Proxy-Authorization":
			continue
		}
		headers.Add(name, value)
	}
	return status, headers, nil
}

func cgiEnv(r *http.Request, target cluster.RelayTarget, tailPath string) []string {
	scriptName := strings.TrimSuffix(target.Prefix, "/") + "/" + filepath.Base(target.CGIPath)
	requestURI := scriptName + tailPath
	if r.URL.RawQuery != "" {
		requestURI += "?" + r.URL.RawQuery
	}
	contentLength := ""
	if r.ContentLength >= 0 {
		contentLength = strconv.FormatInt(r.ContentLength, 10)
	}
	host, port := splitHostPort(r.Host)
	env := []string{
		"GATEWAY_INTERFACE=CGI/1.1",
		"SERVER_SOFTWARE=fncluster-relay",
		"SERVER_PROTOCOL=" + r.Proto,
		"REQUEST_METHOD=" + r.Method,
		"SCRIPT_NAME=" + scriptName,
		"SCRIPT_FILENAME=" + target.CGIPath,
		"PATH_INFO=" + tailPath,
		"REQUEST_URI=" + requestURI,
		"QUERY_STRING=" + r.URL.RawQuery,
		"REMOTE_ADDR=" + remoteHost(r.RemoteAddr),
		"SERVER_NAME=" + host,
		"SERVER_PORT=" + port,
		"CONTENT_TYPE=" + r.Header.Get("Content-Type"),
		"CONTENT_LENGTH=" + contentLength,
		"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
	}
	for name, values := range r.Header {
		if name == "Content-Type" || name == "Content-Length" {
			continue
		}
		key := "HTTP_" + strings.ToUpper(strings.ReplaceAll(name, "-", "_"))
		env = append(env, key+"="+strings.Join(values, ", "))
	}
	return env
}

func splitHostPort(host string) (string, string) {
	name, port, err := net.SplitHostPort(host)
	if err != nil {
		return host, ""
	}
	return name, port
}

func remoteHost(remoteAddr string) string {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		return remoteAddr
	}
	return host
}
