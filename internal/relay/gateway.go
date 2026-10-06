package relay

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"
	"strings"

	"fncluster/internal/mesh"
)

// CookiePrefix namespaces every cookie the relayed remote desktop touches so
// it can never collide with the local desktop's own cookies on the same host
// (cookies ignore ports).
func CookiePrefix(webOrigin string) string {
	parsed, err := url.Parse(strings.TrimSpace(webOrigin))
	if err != nil || parsed.Hostname() == "" {
		return ""
	}
	host := strings.ToLower(parsed.Hostname())
	var cleaned strings.Builder
	for _, ch := range host {
		switch {
		case ch >= 'a' && ch <= 'z', ch >= '0' && ch <= '9':
			cleaned.WriteRune(ch)
		case ch == '.' || ch == '-':
			cleaned.WriteRune(ch)
		default:
			cleaned.WriteRune('-')
		}
	}
	name := strings.Trim(cleaned.String(), "-.")
	if name == "" {
		return ""
	}
	return "fncluster-" + name + "-"
}

// proxyGateway relays the remote node's whole fnOS gateway. SPA fallback,
// cookie namespacing and the storage shim make the tunneled desktop usable
// inside a same-site iframe.
func (s *Server) proxyGateway(w http.ResponseWriter, r *http.Request, entry session) {
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
	prefix := entry.Prefix
	sessionCookie := cookieName(entry.Port)
	proxy := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.Out.URL.Scheme = "https"
			pr.Out.URL.Host = address
			pr.Out.URL.Path = "/relay/" + GatewayApp + r.URL.Path
			pr.Out.URL.RawQuery = r.URL.RawQuery
			pr.Out.Host = address
			// Identity encoding so HTML can be shim-injected below.
			pr.Out.Header.Del("Accept-Encoding")
			rewriteGatewayRequestCookies(pr.Out.Header, prefix, sessionCookie)
		},
		Transport:     client.Transport,
		FlushInterval: -1,
		ModifyResponse: func(resp *http.Response) error {
			if prefix != "" {
				prefixResponseCookies(resp.Header, prefix)
			}
			if strings.Contains(resp.Header.Get("Content-Type"), "text/html") {
				body, readErr := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
				_ = resp.Body.Close()
				if readErr != nil {
					return readErr
				}
				data := injectShim(body, prefix)
				resp.Body = io.NopCloser(bytes.NewReader(data))
				resp.ContentLength = int64(len(data))
				resp.Header.Set("Content-Length", strconv.Itoa(len(data)))
				resp.Header.Del("Content-Encoding")
			}
			return nil
		},
		ErrorHandler: func(w http.ResponseWriter, _ *http.Request, err error) {
			http.Error(w, "relay gateway: "+err.Error(), http.StatusBadGateway)
		},
	}
	proxy.ServeHTTP(w, r)
}

// rewriteGatewayRequestCookies forwards only the remote namespace to the
// gateway. Cookies on this host may belong to the local desktop (the relay
// origin shares its host), and forwarding the local fnos-token made the remote
// gateway reject every request with errno 65534 (验签失败).
func rewriteGatewayRequestCookies(header http.Header, prefix, sessionCookie string) {
	cookies := headerCookies(header)
	if len(cookies) == 0 {
		return
	}
	header.Del("Cookie")
	if prefix == "" {
		return
	}
	for _, cookie := range cookies {
		if !strings.HasPrefix(cookie.Name, prefix) {
			continue
		}
		cookie.Name = strings.TrimPrefix(cookie.Name, prefix)
		request := &http.Request{Header: http.Header{}}
		request.AddCookie(cookie)
		for _, value := range request.Header.Values("Cookie") {
			header.Add("Cookie", value)
		}
	}
}

// prefixResponseCookies namespaces every Set-Cookie from the remote gateway and
// drops any Domain attribute so the cookie stays host-only on the relay origin.
func prefixResponseCookies(header http.Header, prefix string) {
	values := header.Values("Set-Cookie")
	if len(values) == 0 {
		return
	}
	header.Del("Set-Cookie")
	for _, raw := range values {
		index := strings.Index(raw, "=")
		if index <= 0 {
			header.Add("Set-Cookie", raw)
			continue
		}
		name := strings.TrimSpace(raw[:index])
		if prefix != "" && !strings.HasPrefix(name, prefix) {
			name = prefix + name
		}
		parts := strings.Split(raw[index:], ";")
		cleaned := []string{parts[0]}
		for _, part := range parts[1:] {
			if strings.HasPrefix(strings.ToLower(strings.TrimSpace(part)), "domain=") {
				continue
			}
			cleaned = append(cleaned, part)
		}
		header.Add("Set-Cookie", name+strings.Join(cleaned, ";"))
	}
}

func headerCookies(header http.Header) []*http.Cookie {
	request := &http.Request{Header: header.Clone()}
	return request.Cookies()
}

// injectShim puts the cookie/storage namespace shim before the SPA bundle so
// the remote desktop keeps working while its data stays namespaced.
func injectShim(body []byte, prefix string) []byte {
	if prefix == "" {
		return body
	}
	script := []byte("<script>" + shimScript(prefix) + "</script>")
	lower := bytes.ToLower(body)
	if index := bytes.Index(lower, []byte("<head>")); index >= 0 {
		at := index + len("<head>")
		return append(append(append([]byte{}, body[:at]...), script...), body[at:]...)
	}
	return append(script, body...)
}

func shimScript(prefix string) string {
	quoted, err := json.Marshal(prefix)
	if err != nil {
		quoted = []byte(`""`)
	}
	return fmt.Sprintf(`(function(){
  var prefix=%s;
  try{
    var d=Object.getOwnPropertyDescriptor(Document.prototype,'cookie');
    if(d&&d.get&&d.set){
      Object.defineProperty(document,'cookie',{configurable:true,
        get:function(){
          var out=[];
          d.get.call(document).split('; ').forEach(function(p){ if(p.indexOf(prefix)===0) out.push(p.slice(prefix.length)); });
          return out.join('; ');
        },
        set:function(v){
          var i=v.indexOf('=');
          if(i<=0) return;
          var name=v.slice(0,i).trim();
          if(name.indexOf(prefix)!==0) name=prefix+name;
          d.set.call(document,name+v.slice(i));
        }
      });
    }
  }catch(e){}
  try{
    var proto=Storage.prototype;
    ['getItem','setItem','removeItem'].forEach(function(m){
      var original=proto[m];
      proto[m]=function(key){ var args=[].slice.call(arguments); args[0]=prefix+key; return original.apply(this,args); };
    });
    var key=proto.key;
    proto.key=function(i){ var k=key.call(this,i); return typeof k==='string'&&k.indexOf(prefix)===0?k.slice(prefix.length):null; };
    var clear=proto.clear;
    proto.clear=function(){
      var keys=[]; for(var i=0;i<this.length;i++){ var k=key.call(this,i); if(k&&k.indexOf(prefix)===0) keys.push(k); }
      for(var j=0;j<keys.length;j++) this.removeItem(keys[j]);
    };
  }catch(e){}
})();`, quoted)
}
