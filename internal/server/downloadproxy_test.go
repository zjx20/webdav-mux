package server

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeProxy 是按 docs/proxy-protocol.md 工作的测试用下载代理：把请求转发给 url 参数指定的目标。
type fakeProxy struct {
	*fakeUpstream

	mu      sync.Mutex
	targets []string
}

func (p *fakeProxy) seenTargets() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return slices.Clone(p.targets)
}

// proxyMarker 由 fakeProxy 加在发往目标的请求上，用来确认请求确实经过了代理。
const proxyMarker = "X-Test-Via-Proxy"

// newFakeProxy 启动一个转发型代理。follow 为 true 时代理自己跟随重定向，否则把 3xx 原样返回。
func newFakeProxy(t *testing.T, follow bool) *fakeProxy {
	t.Helper()
	client := &http.Client{}
	if !follow {
		client.CheckRedirect = noFollow
	}
	p := &fakeProxy{}
	p.fakeUpstream = newHandlerUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		target := r.URL.Query().Get("url")
		p.mu.Lock()
		p.targets = append(p.targets, target)
		p.mu.Unlock()
		req, err := http.NewRequestWithContext(r.Context(), r.Method, target, nil)
		if err != nil {
			w.Header().Set("Proxy-Status", "test-proxy; error=http_request_error")
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		for _, h := range []string{"Range", "If-Match", "If-None-Match", "If-Modified-Since", "If-Unmodified-Since", "If-Range", "Authorization", "Accept-Encoding", "User-Agent"} {
			if v := r.Header.Values(h); len(v) > 0 {
				req.Header[h] = v
			}
		}
		req.Header.Set(proxyMarker, "1")
		resp, err := client.Do(req)
		if err != nil {
			w.Header().Set("Proxy-Status", "test-proxy; error=proxy_internal_error")
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		defer resp.Body.Close()
		for k, v := range resp.Header {
			w.Header()[k] = v
		}
		w.WriteHeader(resp.StatusCode)
		io.Copy(w, resp.Body)
	})
	return p
}

// proxyEnv 启动被测服务：唯一的上游 up 需要 Basic 认证 mux:right，经由 proxyBase 获取文件，
// alice 把它的 /data 挂载在 /m。
func proxyEnv(t *testing.T, upstreamURL, proxyBase string) *testEnv {
	return newEnv(t, fmt.Sprintf(`
upstreams:
  up:
    url: %s
    username: mux
    password: right
    proxy: %s
users:
  alice:
    password_hash: %q
    mounts:
      /m: up:/data
`, upstreamURL, proxyBase, hashOf(t, "alice-pw")))
}

func TestProxyGet(t *testing.T) {
	nas := newDavUpstream(t, "/dav", "mux", "right")
	movie := bytes.Repeat([]byte("0123456789abcdef"), 1<<14) // 256 KiB
	nas.put(t, "/data/a.mkv", movie)
	nas.put(t, "/data/中文 #1.mkv", []byte("unicode movie"))
	proxy := newFakeProxy(t, false)
	e := proxyEnv(t, nas.URL+"/dav", proxy.URL+"/profiles/fast/")

	r := e.do("GET", "/m/a.mkv", "alice", nil, "")
	if r.StatusCode != 200 || r.body != string(movie) {
		t.Fatalf("GET: %d len=%d", r.StatusCode, len(r.body))
	}
	etag := r.Header.Get("ETag")
	r = e.do("GET", "/m/a.mkv", "alice", http.Header{"Range": {"bytes=100-199"}}, "")
	if r.StatusCode != 206 || r.body != string(movie[100:200]) || r.Header.Get("Content-Range") != fmt.Sprintf("bytes 100-199/%d", len(movie)) {
		t.Errorf("Range: %d %q", r.StatusCode, r.Header.Get("Content-Range"))
	}
	if r = e.do("GET", "/m/a.mkv", "alice", http.Header{"Range": {"bytes=999999999-"}}, ""); r.StatusCode != 416 || r.Header.Get("Content-Range") == "" {
		t.Errorf("unsatisfiable Range: %d Content-Range=%q", r.StatusCode, r.Header.Get("Content-Range"))
	}
	if r = e.do("GET", "/m/a.mkv", "alice", http.Header{"If-None-Match": {etag}}, ""); r.StatusCode != 304 {
		t.Errorf("If-None-Match: %d", r.StatusCode)
	}
	if r = e.do("HEAD", "/m/a.mkv", "alice", nil, ""); r.StatusCode != 200 || r.Header.Get("Content-Length") != fmt.Sprint(len(movie)) {
		t.Errorf("HEAD: %d Content-Length=%q", r.StatusCode, r.Header.Get("Content-Length"))
	}
	if r = e.do("GET", "/m/%E4%B8%AD%E6%96%87%20%231.mkv", "alice", nil, ""); r.StatusCode != 200 || r.body != "unicode movie" {
		t.Errorf("unicode GET: %d %q", r.StatusCode, r.body)
	}
	if r = e.propfind("/m/", "alice", "1"); r.StatusCode != 207 {
		t.Errorf("PROPFIND: %d", r.StatusCode)
	}

	wantAuth := "Basic " + basicAuth("mux", "right")
	proxyReqs := proxy.requests()
	if len(proxyReqs) != 6 {
		t.Errorf("proxy got %d requests; want 6 (the GET/HEAD requests only)", len(proxyReqs))
	}
	for _, req := range proxyReqs {
		// 基地址末尾的 "/" 被去掉后再拼接 "/proxy"。
		if req.Path != "/profiles/fast/proxy" || req.Auth != wantAuth || req.Method == "PROPFIND" {
			t.Errorf("proxy request: %s %s Authorization=%q", req.Method, req.Path, req.Auth)
		}
	}
	// 目标 URL 保留严格转义，代理解码 url 参数后得到的就是直连时会请求的地址。
	if wantTarget := nas.URL + "/dav/data/%E4%B8%AD%E6%96%87%20%231.mkv"; !slices.Contains(proxy.seenTargets(), wantTarget) {
		t.Errorf("targets %q do not contain %q", proxy.seenTargets(), wantTarget)
	}
	for _, req := range nas.requests() {
		if via := req.Header.Get(proxyMarker) != ""; via != (req.Method != "PROPFIND") {
			t.Errorf("upstream %s %s: via proxy = %v", req.Method, req.Path, via)
		}
	}
	if !strings.Contains(e.logs.String(), "via_proxy=true") {
		t.Errorf("proxied request not marked in the access log:\n%s", e.logs.String())
	}
}

func TestProxyRedirects(t *testing.T) {
	cdn := newHandlerUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		http.ServeContent(w, r, "a.mkv", time.Time{}, strings.NewReader("cdn content"))
	})
	up := newHandlerUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/data/a.mkv":
			http.Redirect(w, r, cdn.URL+"/signed/a.mkv?sig=1", http.StatusFound)
		case "/data/rel.mkv":
			http.Redirect(w, r, "/data/a.mkv", http.StatusMovedPermanently)
		case "/data/loop":
			http.Redirect(w, r, "/data/loop", http.StatusTemporaryRedirect)
		default:
			http.NotFound(w, r)
		}
	})

	t.Run("proxy returns redirects", func(t *testing.T) {
		proxy := newFakeProxy(t, false)
		e := proxyEnv(t, up.URL, proxy.URL)

		r := e.do("GET", "/m/a.mkv", "alice", http.Header{"Range": {"bytes=4-"}}, "")
		if r.StatusCode != 206 || r.body != "content" {
			t.Errorf("GET via cross-origin redirect: %d %q", r.StatusCode, r.body)
		}
		wantTargets := []string{up.URL + "/data/a.mkv", cdn.URL + "/signed/a.mkv?sig=1"}
		if got := proxy.seenTargets(); !slices.Equal(got, wantTargets) {
			t.Errorf("targets = %q; want %q", got, wantTargets)
		}
		// 每一跳都经过调用方，凭据只随目标与上游同源的那一跳发给代理。
		if reqs := proxy.requests(); len(reqs) != 2 || reqs[0].Auth != "Basic "+basicAuth("mux", "right") || reqs[1].Auth != "" {
			t.Errorf("proxy requests: %+v", reqs)
		}
		if reqs := cdn.requests(); len(reqs) != 1 || reqs[0].Auth != "" || reqs[0].Header.Get("Range") != "bytes=4-" {
			t.Errorf("CDN requests: %+v", reqs)
		}

		// 相对 Location 相对目标 URL 解析，而不是相对代理的地址。
		r = e.do("GET", "/m/rel.mkv", "alice", nil, "")
		if r.StatusCode != 200 || r.body != "cdn content" {
			t.Errorf("GET via relative redirect: %d %q", r.StatusCode, r.body)
		}
		if got := proxy.seenTargets()[2:]; !slices.Equal(got, []string{up.URL + "/data/rel.mkv", up.URL + "/data/a.mkv", cdn.URL + "/signed/a.mkv?sig=1"}) {
			t.Errorf("relative redirect targets = %q", got)
		}
		if r := e.do("GET", "/m/loop", "alice", nil, ""); r.StatusCode != 502 {
			t.Errorf("redirect loop: %d", r.StatusCode)
		}
	})

	t.Run("proxy follows redirects", func(t *testing.T) {
		proxy := newFakeProxy(t, true)
		e := proxyEnv(t, up.URL, proxy.URL)
		r := e.do("GET", "/m/rel.mkv", "alice", http.Header{"Range": {"bytes=4-"}}, "")
		if r.StatusCode != 206 || r.body != "content" {
			t.Errorf("GET: %d %q", r.StatusCode, r.body)
		}
		if got := proxy.seenTargets(); !slices.Equal(got, []string{up.URL + "/data/rel.mkv"}) {
			t.Errorf("targets = %q; want the single original target", got)
		}
	})
}

func TestProxyErrors(t *testing.T) {
	var status int
	var proxyStatus string
	proxy := newHandlerUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		if proxyStatus != "" {
			w.Header().Set("Proxy-Status", proxyStatus)
		}
		w.Header().Set("Set-Cookie", "proxy=1")
		w.WriteHeader(status)
		io.WriteString(w, "proxy internals")
	})
	up := newDavUpstream(t, "", "mux", "right")
	e := proxyEnv(t, up.URL, proxy.URL)

	cases := []struct {
		status      int
		proxyStatus string
		want        int
		logged      string
	}{
		// 代理自身的错误：状态码描述的是代理，不能按目标服务器的状态码映射。
		{400, "test-proxy; error=http_request_error", 502, "http_request_error"},
		{504, "test-proxy; error=connection_timeout", 504, "connection_timeout"},
		{504, "test-proxy; error=http_response_timeout", 504, "http_response_timeout"},
		{503, "test-proxy; error=connection_limit_reached", 502, "connection_limit_reached"},
		{502, `"my, proxy"; details="x; error=bogus"; error=dns_error`, 502, "dns_error"},
		// 目标服务器的响应：沿用直连时的映射。
		{404, "", 404, ""},
		{404, "test-proxy; received-status=404", 404, ""},
		{401, "", 502, ""},
		{503, "", 503, ""},
		{500, "", 502, ""},
	}
	for _, c := range cases {
		status, proxyStatus = c.status, c.proxyStatus
		for _, method := range []string{"GET", "HEAD"} {
			r := e.do(method, "/m/x", "alice", nil, "")
			if r.StatusCode != c.want {
				t.Errorf("%s proxy %d %q: got %d; want %d", method, c.status, c.proxyStatus, r.StatusCode, c.want)
			}
			if r.Header.Get("Set-Cookie") != "" || r.Header.Get("Proxy-Status") != "" || strings.Contains(r.body, "internals") {
				t.Errorf("%s proxy %d: proxy headers/body leaked: %v %q", method, c.status, r.Header, r.body)
			}
		}
		if c.logged != "" && !strings.Contains(e.logs.String(), c.logged) {
			t.Errorf("proxy error type %q not logged", c.logged)
		}
	}
	if strings.Contains(e.logs.String(), "bogus") {
		t.Error("error parameter inside a quoted string was taken as the error type")
	}
	if n := len(up.requests()); n != 0 {
		t.Errorf("GET requests bypassed the proxy %d times", n)
	}
}

func TestProxyUnreachable(t *testing.T) {
	proxy := newHandlerUpstream(t, func(http.ResponseWriter, *http.Request) {})
	proxyURL := proxy.URL
	proxy.Close()
	up := newDavUpstream(t, "", "mux", "right")
	up.put(t, "/data/x", []byte("x"))
	e := proxyEnv(t, up.URL, proxyURL)
	if r := e.do("GET", "/m/x", "alice", nil, ""); r.StatusCode != 502 {
		t.Errorf("GET: %d", r.StatusCode)
	}
	if r := e.propfind("/m/", "alice", "0"); r.StatusCode != 207 {
		t.Errorf("PROPFIND must not depend on the proxy: %d", r.StatusCode)
	}
	if !strings.Contains(e.logs.String(), "connection refused") {
		t.Errorf("transport error not logged:\n%s", e.logs.String())
	}
}

func TestProxyStatusError(t *testing.T) {
	tests := []struct {
		values   []string
		wantType string
		wantOK   bool
	}{
		{nil, "", false},
		{[]string{"p; error=dns_error"}, "dns_error", true},
		{[]string{"p;error=dns_error;details=\"x\""}, "dns_error", true},
		{[]string{"p; received-status=404"}, "", false},
		{[]string{"cdn; received-status=502", `helper; error="connection_refused"`}, "connection_refused", true},
		{[]string{"cdn; received-status=502, helper; error=proxy_internal_error"}, "proxy_internal_error", true},
		{[]string{`"x;y,z"; error=proxy_internal_error`}, "proxy_internal_error", true},
		{[]string{`p; details="error=fake"`}, "", false},
		{[]string{`p; details="said \"; error=fake\""`}, "", false},
		{[]string{"p; error"}, "", true},
		{[]string{"error=looks_like_a_param_but_is_a_name"}, "", false},
	}
	for _, tt := range tests {
		h := http.Header{"Proxy-Status": tt.values}
		if got, ok := proxyStatusError(h); got != tt.wantType || ok != tt.wantOK {
			t.Errorf("%q: got (%q, %v); want (%q, %v)", tt.values, got, ok, tt.wantType, tt.wantOK)
		}
	}
}

func TestProxyEndpoint(t *testing.T) {
	tests := []struct{ base, want string }{
		{"http://p:8090", "http://p:8090/proxy"},
		{"http://p:8090/", "http://p:8090/proxy"},
		{"https://p/profiles/fast/", "https://p/profiles/fast/proxy"},
		{"http://p/a%2Fb", "http://p/a%2Fb/proxy"},
	}
	for _, tt := range tests {
		base, err := url.Parse(tt.base)
		if err != nil {
			t.Fatal(err)
		}
		if got := proxyEndpoint(base).String(); got != tt.want {
			t.Errorf("proxyEndpoint(%q) = %q; want %q", tt.base, got, tt.want)
		}
	}

	up := &upstream{proxy: proxyEndpoint(&url.URL{Scheme: "http", Host: "p", Path: "/x"})}
	target := &url.URL{Scheme: "https", Host: "nas", Path: "/a b/c&d=e?.mkv", RawPath: "/a%20b/c%26d%3De%3F.mkv"}
	got := up.proxyURL(target)
	if got.Query().Get("url") != "https://nas/a%20b/c%26d%3De%3F.mkv" || len(got.Query()) != 1 {
		t.Errorf("proxyURL = %s", got)
	}
}
