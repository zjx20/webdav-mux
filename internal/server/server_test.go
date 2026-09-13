package server

import (
	"bytes"
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/xml"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"
	"golang.org/x/net/webdav"

	"webdav-mux/internal/config"
)

// ---- 测试用上游 ----

type seenRequest struct {
	Method string
	Path   string // 解码后的路径
	Auth   string
	Cookie string
	Depth  string
	Header http.Header
	Body   string
}

type fakeUpstream struct {
	*httptest.Server
	fs webdav.FileSystem

	mu   sync.Mutex
	seen []seenRequest
}

func (u *fakeUpstream) record(r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	r.Body = io.NopCloser(bytes.NewReader(body))
	u.mu.Lock()
	defer u.mu.Unlock()
	u.seen = append(u.seen, seenRequest{
		Method: r.Method, Path: r.URL.Path, Auth: r.Header.Get("Authorization"), Cookie: r.Header.Get("Cookie"),
		Depth: r.Header.Get("Depth"), Header: r.Header.Clone(), Body: string(body),
	})
}

func (u *fakeUpstream) requests() []seenRequest {
	u.mu.Lock()
	defer u.mu.Unlock()
	return slices.Clone(u.seen)
}

// newDavUpstream 启动一个在 prefix 下提供 WebDAV 的上游；user 非空时要求对应的 Basic 认证。
func newDavUpstream(t *testing.T, prefix, user, pass string) *fakeUpstream {
	t.Helper()
	u := &fakeUpstream{fs: webdav.NewMemFS()}
	h := &webdav.Handler{Prefix: prefix, FileSystem: u.fs, LockSystem: webdav.NewMemLS()}
	u.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u.record(r)
		if user != "" {
			if gu, gp, ok := r.BasicAuth(); !ok || gu != user || gp != pass {
				w.Header().Set("WWW-Authenticate", `Basic realm="upstream"`)
				http.Error(w, "upstream says no", http.StatusUnauthorized)
				return
			}
		}
		h.ServeHTTP(w, r)
	}))
	t.Cleanup(u.Close)
	return u
}

// newHandlerUpstream 启动一个由测试自定义行为的上游。
func newHandlerUpstream(t *testing.T, h http.HandlerFunc) *fakeUpstream {
	t.Helper()
	u := &fakeUpstream{}
	u.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u.record(r)
		h(w, r)
	}))
	t.Cleanup(u.Close)
	return u
}

func (u *fakeUpstream) put(t *testing.T, name string, content []byte) {
	t.Helper()
	ctx := context.Background()
	cur := ""
	for _, seg := range strings.Split(strings.Trim(path.Dir(name), "/"), "/") {
		if seg != "" {
			cur += "/" + seg
			u.fs.Mkdir(ctx, cur, 0o755)
		}
	}
	f, err := u.fs.OpenFile(ctx, name, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write(content); err != nil {
		t.Fatal(err)
	}
	f.Close()
}

// ---- 被测服务 ----

type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

type testEnv struct {
	t    *testing.T
	srv  *Server
	ts   *httptest.Server
	logs *syncBuffer
}

var hashCache sync.Map

func hashOf(t *testing.T, password string) string {
	if h, ok := hashCache.Load(password); ok {
		return h.(string)
	}
	h, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.MinCost)
	if err != nil {
		t.Fatal(err)
	}
	hashCache.Store(password, string(h))
	return string(h)
}

// newEnv 按 YAML 配置启动被测服务；opts 在服务开始接受请求之前调整 Server。
func newEnv(t *testing.T, yamlText string, opts ...func(*Server)) *testEnv {
	t.Helper()
	cfg, err := config.Parse([]byte(yamlText))
	if err != nil {
		t.Fatalf("config: %v\n%s", err, yamlText)
	}
	logs := &syncBuffer{}
	srv, err := New(cfg, slog.New(slog.NewTextHandler(logs, nil)))
	if err != nil {
		t.Fatal(err)
	}
	for _, opt := range opts {
		opt(srv)
	}
	ts := httptest.NewUnstartedServer(srv)
	ConfigureHTTPServer(ts.Config)
	ts.Start()
	t.Cleanup(ts.Close)
	return &testEnv{t: t, srv: srv, ts: ts, logs: logs}
}

type response struct {
	*http.Response
	body string
}

// do 发出请求；path 按原样（已转义）使用，不做任何清理。
func (e *testEnv) do(method, path, user string, header http.Header, body string) response {
	e.t.Helper()
	req, err := http.NewRequest(method, e.ts.URL, strings.NewReader(body))
	if err != nil {
		e.t.Fatal(err)
	}
	req.URL.Opaque = path // 让 ".."、"%2F" 等原样出现在请求行里
	for k, v := range header {
		req.Header[k] = v
	}
	if user != "" {
		req.SetBasicAuth(user, user+"-pw")
	}
	resp, err := e.ts.Client().Do(req)
	if err != nil {
		e.t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		e.t.Fatalf("%s %s: reading body: %v", method, path, err)
	}
	return response{resp, string(b)}
}

func (e *testEnv) propfind(path, user, depth string) response {
	e.t.Helper()
	return e.do("PROPFIND", path, user, http.Header{"Depth": {depth}}, "")
}

type multistatus struct {
	Responses []struct {
		Href      string `xml:"DAV: href"`
		Propstats []struct {
			Prop struct {
				Any []struct {
					XMLName xml.Name
					Inner   string `xml:",innerxml"`
				} `xml:",any"`
			} `xml:"DAV: prop"`
			Status string `xml:"DAV: status"`
		} `xml:"DAV: propstat"`
	} `xml:"DAV: response"`
}

func parseMultistatus(t *testing.T, r response) multistatus {
	t.Helper()
	if r.StatusCode != http.StatusMultiStatus {
		t.Fatalf("status %d, want 207; body: %s", r.StatusCode, r.body)
	}
	var ms multistatus
	if err := xml.Unmarshal([]byte(r.body), &ms); err != nil {
		t.Fatalf("invalid multistatus: %v\n%s", err, r.body)
	}
	return ms
}

func (ms multistatus) hrefs() []string {
	var out []string
	for _, r := range ms.Responses {
		out = append(out, r.Href)
	}
	slices.Sort(out)
	return out
}

// prop 返回 href 对应条目中状态为 200 的 DAV: 属性。
func (ms multistatus) prop(href, local string) (string, bool) {
	for _, r := range ms.Responses {
		if r.Href != href {
			continue
		}
		for _, ps := range r.Propstats {
			if !strings.Contains(ps.Status, " 200 ") {
				continue
			}
			for _, p := range ps.Prop.Any {
				if p.XMLName == (xml.Name{Space: "DAV:", Local: local}) {
					return p.Inner, true
				}
			}
		}
	}
	return "", false
}

// ---- 标准场景 ----

type standard struct {
	*testEnv
	nas, cloud *fakeUpstream
	localDir   string
	movieA     []byte
}

// newStandard 搭建：
//
//	nas   （需要认证，根路径 /dav）       /media/movies/{a.mkv, sub/b.mkv, 中文 #1.mkv}、/media/secret.txt、/share/team/doc.txt
//	cloud （无认证，Nextcloud 风格根路径） /Documents/report.pdf
//	media （本地目录）                   hello.txt、sub/x.txt、escape.txt -> ../secret.txt
//
//	alice: /movies -> nas:/media/movies, /work/docs -> cloud:/Documents,
//	       /work/share -> nas:/share/team, /local -> media:/
//	bob:   / -> nas:/media/movies/sub
func newStandard(t *testing.T) *standard {
	t.Helper()
	s := &standard{
		nas:    newDavUpstream(t, "/dav", "mux", "nas-secret"),
		cloud:  newDavUpstream(t, "/remote.php/dav/files/bob", "", ""),
		movieA: bytes.Repeat([]byte("0123456789abcdef"), 1<<14), // 256 KiB
	}
	s.nas.put(t, "/media/movies/a.mkv", s.movieA)
	s.nas.put(t, "/media/movies/sub/b.mkv", []byte("movie b"))
	s.nas.put(t, "/media/movies/中文 #1.mkv", []byte("unicode movie"))
	s.nas.put(t, "/media/secret.txt", []byte("nas secret content"))
	s.nas.put(t, "/share/team/doc.txt", []byte("team doc"))
	s.cloud.put(t, "/Documents/report.pdf", []byte("report"))

	root := t.TempDir()
	s.localDir = filepath.Join(root, "share")
	os.MkdirAll(filepath.Join(s.localDir, "sub"), 0o755)
	os.WriteFile(filepath.Join(s.localDir, "hello.txt"), []byte("hello local"), 0o644)
	os.WriteFile(filepath.Join(s.localDir, "sub", "x.txt"), []byte("x"), 0o644)
	os.WriteFile(filepath.Join(root, "secret.txt"), []byte("local secret content"), 0o644)
	os.Symlink("../secret.txt", filepath.Join(s.localDir, "escape.txt"))

	s.testEnv = newEnv(t, fmt.Sprintf(`
upstreams:
  nas:
    url: %s/dav
    username: mux
    password: nas-secret
  cloud:
    url: %s/remote.php/dav/files/bob/
  media:
    dir: %s
users:
  alice:
    password_hash: %q
    mounts:
      /movies: nas:/media/movies
      /work/docs: cloud:/Documents
      /work/share: nas:/share/team
      /local: media:/
  bob:
    password_hash: %q
    mounts:
      /: nas:/media/movies/sub
`, s.nas.URL, s.cloud.URL, s.localDir, hashOf(t, "alice-pw"), hashOf(t, "bob-pw")))
	return s
}

// ---- 测试 ----

func TestOptions(t *testing.T) {
	s := newStandard(t)
	for _, p := range []string{"/", "/movies/a.mkv", "*"} {
		r := s.do("OPTIONS", p, "", nil, "")
		if r.StatusCode != 200 || r.Header.Get("DAV") != "1" || r.Header.Get("Allow") != "OPTIONS, GET, HEAD, PROPFIND" {
			t.Errorf("OPTIONS %s: %d DAV=%q Allow=%q", p, r.StatusCode, r.Header.Get("DAV"), r.Header.Get("Allow"))
		}
	}
	if n := len(s.nas.requests()); n != 0 {
		t.Errorf("OPTIONS reached the upstream %d times", n)
	}
}

func TestAuthentication(t *testing.T) {
	s := newStandard(t)
	check := func(name string, req func(*http.Request)) {
		t.Helper()
		hreq, _ := http.NewRequest("PROPFIND", s.ts.URL+"/", nil)
		hreq.Header.Set("Depth", "0")
		req(hreq)
		resp, err := s.ts.Client().Do(hreq)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != 401 || !strings.HasPrefix(resp.Header.Get("WWW-Authenticate"), "Basic ") {
			t.Errorf("%s: status %d, WWW-Authenticate %q", name, resp.StatusCode, resp.Header.Get("WWW-Authenticate"))
		}
	}
	check("no credentials", func(*http.Request) {})
	check("wrong password", func(r *http.Request) { r.SetBasicAuth("alice", "bob-pw") })
	check("unknown user", func(r *http.Request) { r.SetBasicAuth("mallory", "alice-pw") })
	check("upstream credentials", func(r *http.Request) { r.SetBasicAuth("mux", "nas-secret") })
	check("bearer", func(r *http.Request) { r.Header.Set("Authorization", "Bearer alice-pw") })

	for _, u := range []string{"alice", "bob"} {
		if r := s.propfind("/", u, "0"); r.StatusCode != 207 {
			t.Errorf("%s: %d", u, r.StatusCode)
		}
	}
}

func TestAuthCache(t *testing.T) {
	s := newStandard(t)
	if r := s.propfind("/", "alice", "0"); r.StatusCode != 207 {
		t.Fatal(r.StatusCode)
	}
	st := s.srv.state.Load()
	alice := st.users["alice"]
	// 成功校验后改掉存储的哈希：请求仍能通过，说明走的是缓存而不是 bcrypt。
	alice.hash = []byte("$2a$04$invalidinvalidinvalidinvalidinvalidinvalidinvalidinv")
	if r := s.propfind("/", "alice", "0"); r.StatusCode != 207 {
		t.Errorf("cached credentials rejected: %d", r.StatusCode)
	}
	req := func(pass string) int {
		hreq, _ := http.NewRequest("PROPFIND", s.ts.URL+"/", nil)
		hreq.Header.Set("Depth", "0")
		hreq.SetBasicAuth("alice", pass)
		resp, err := s.ts.Client().Do(hreq)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	if code := req("wrong"); code != 401 {
		t.Errorf("wrong password accepted through cache: %d", code)
	}
	if alice.cache.hit(st.passwordDigest("alice-pw"), time.Now().Add(authCacheTTL+time.Second)) {
		t.Error("cache entry did not expire")
	}
}

func TestSyntheticDirectories(t *testing.T) {
	s := newStandard(t)

	ms := parseMultistatus(t, s.propfind("/", "alice", "1"))
	if got, want := ms.hrefs(), []string{"/", "/local/", "/movies/", "/work/"}; !slices.Equal(got, want) {
		t.Errorf("alice / hrefs = %q; want %q", got, want)
	}
	if rt, _ := ms.prop("/movies/", "resourcetype"); !strings.Contains(rt, "collection") {
		t.Errorf("mount point is not a collection: %q", rt)
	}
	if dn, _ := ms.prop("/work/", "displayname"); dn != "work" {
		t.Errorf("displayname = %q", dn)
	}
	if lm, ok := ms.prop("/work/", "getlastmodified"); !ok {
		t.Error("missing getlastmodified")
	} else if _, err := http.ParseTime(lm); err != nil {
		t.Errorf("bad getlastmodified %q: %v", lm, err)
	}

	if got := parseMultistatus(t, s.propfind("/", "alice", "0")).hrefs(); !slices.Equal(got, []string{"/"}) {
		t.Errorf("depth 0 hrefs = %q", got)
	}
	for _, p := range []string{"/work", "/work/"} {
		got := parseMultistatus(t, s.propfind(p, "alice", "1")).hrefs()
		if want := []string{"/work/", "/work/docs/", "/work/share/"}; !slices.Equal(got, want) {
			t.Errorf("%s hrefs = %q; want %q", p, got, want)
		}
	}

	// 合成目录不访问上游：上游都停掉之后仍然可以列出。
	s.nas.Close()
	s.cloud.Close()
	if got := parseMultistatus(t, s.propfind("/", "alice", "1")).hrefs(); len(got) != 4 {
		t.Errorf("listing with upstreams down = %q", got)
	}

	r := s.do("GET", "/work/", "alice", nil, "")
	if r.StatusCode != 405 || r.Header.Get("Allow") != "OPTIONS, PROPFIND" {
		t.Errorf("GET synthetic dir: %d Allow=%q", r.StatusCode, r.Header.Get("Allow"))
	}
}

func TestSyntheticPropRequest(t *testing.T) {
	s := newStandard(t)
	body := `<?xml version="1.0"?><D:propfind xmlns:D="DAV:" xmlns:Z="urn:schemas-microsoft-com:"><D:prop><D:resourcetype/><D:getcontentlength/><Z:Win32FileAttributes/></D:prop></D:propfind>`
	r := s.do("PROPFIND", "/work/", "alice", http.Header{"Depth": {"0"}}, body)
	ms := parseMultistatus(t, r)
	if len(ms.Responses) != 1 || len(ms.Responses[0].Propstats) != 2 {
		t.Fatalf("unexpected response: %s", r.body)
	}
	for _, ps := range ms.Responses[0].Propstats {
		var names []string
		for _, p := range ps.Prop.Any {
			names = append(names, p.XMLName.Local)
		}
		switch ps.Status {
		case "HTTP/1.1 200 OK":
			if !slices.Equal(names, []string{"resourcetype"}) {
				t.Errorf("200 props = %q", names)
			}
		case "HTTP/1.1 404 Not Found":
			if !slices.Equal(names, []string{"getcontentlength", "Win32FileAttributes"}) {
				t.Errorf("404 props = %q", names)
			}
		default:
			t.Errorf("unexpected status %q", ps.Status)
		}
	}
	if r := s.do("PROPFIND", "/", "alice", http.Header{"Depth": {"0"}}, "<not-xml"); r.StatusCode != 400 {
		t.Errorf("malformed body: %d", r.StatusCode)
	}
}

func TestPropfindThroughMount(t *testing.T) {
	s := newStandard(t)

	ms := parseMultistatus(t, s.propfind("/movies/", "alice", "1"))
	want := []string{"/movies/", "/movies/%E4%B8%AD%E6%96%87%20%231.mkv", "/movies/a.mkv", "/movies/sub/"}
	if got := ms.hrefs(); !slices.Equal(got, want) {
		t.Errorf("hrefs = %q; want %q", got, want)
	}
	if cl, _ := ms.prop("/movies/a.mkv", "getcontentlength"); cl != fmt.Sprint(len(s.movieA)) {
		t.Errorf("getcontentlength = %q", cl)
	}
	if etag, _ := ms.prop("/movies/a.mkv", "getetag"); etag == "" {
		t.Error("upstream getetag not passed through")
	}

	// 不带尾部斜杠也可以；上游的根路径不能泄露到 href 里。
	r := s.propfind("/movies", "alice", "0")
	if got := parseMultistatus(t, r).hrefs(); !slices.Equal(got, []string{"/movies/"}) {
		t.Errorf("depth 0 hrefs = %q", got)
	}
	if strings.Contains(r.body, "/dav/") || strings.Contains(r.body, "/media/") {
		t.Errorf("upstream path leaked: %s", r.body)
	}

	// 挂载目标的目录名（team）和挂载点名（share）不同：挂载根的 displayname 应当是挂载点名。
	ms = parseMultistatus(t, s.propfind("/work/share/", "alice", "1"))
	if got := ms.hrefs(); !slices.Equal(got, []string{"/work/share/", "/work/share/doc.txt"}) {
		t.Errorf("share hrefs = %q", got)
	}
	if dn, _ := ms.prop("/work/share/", "displayname"); dn != "share" {
		t.Errorf("mount root displayname = %q; want share", dn)
	}
	if dn, _ := ms.prop("/work/share/doc.txt", "displayname"); dn != "doc.txt" {
		t.Errorf("child displayname = %q", dn)
	}

	// Nextcloud 风格根路径、无认证的上游。
	if got := parseMultistatus(t, s.propfind("/work/docs/", "alice", "1")).hrefs(); !slices.Equal(got, []string{"/work/docs/", "/work/docs/report.pdf"}) {
		t.Errorf("docs hrefs = %q", got)
	}

	// bob 的挂载在根上。
	if got := parseMultistatus(t, s.propfind("/", "bob", "1")).hrefs(); !slices.Equal(got, []string{"/", "/b.mkv"}) {
		t.Errorf("bob hrefs = %q", got)
	}
}

func TestPropfindForwardsBodyAndHeaders(t *testing.T) {
	s := newStandard(t)
	body := `<?xml version="1.0"?><propfind xmlns="DAV:"><prop><getcontentlength/></prop></propfind>`
	r := s.do("PROPFIND", "/movies/a.mkv", "alice", http.Header{
		"Depth": {"0"}, "Content-Type": {"text/xml"}, "Cookie": {"session=alice"}, "X-Custom": {"1"},
	}, body)
	ms := parseMultistatus(t, r)
	if _, ok := ms.prop("/movies/a.mkv", "getcontentlength"); !ok {
		t.Errorf("requested prop missing: %s", r.body)
	}
	if _, ok := ms.prop("/movies/a.mkv", "getetag"); ok {
		t.Errorf("unrequested prop returned, body was not forwarded: %s", r.body)
	}
	reqs := s.nas.requests()
	last := reqs[len(reqs)-1]
	if last.Body != body || last.Header.Get("Content-Type") != "text/xml" || last.Depth != "0" {
		t.Errorf("forwarded request: %+v", last)
	}
	if last.Cookie != "" || last.Header.Get("X-Custom") != "" {
		t.Errorf("client headers leaked to upstream: %v", last.Header)
	}
}

func TestGetThroughMount(t *testing.T) {
	s := newStandard(t)

	r := s.do("GET", "/movies/a.mkv", "alice", nil, "")
	if r.StatusCode != 200 || r.body != string(s.movieA) || r.Header.Get("Content-Length") != fmt.Sprint(len(s.movieA)) {
		t.Fatalf("GET: %d len=%d", r.StatusCode, len(r.body))
	}
	etag := r.Header.Get("ETag")
	if etag == "" || r.Header.Get("Last-Modified") == "" || r.Header.Get("Cache-Control") != "private" {
		t.Errorf("headers: %v", r.Header)
	}

	r = s.do("GET", "/movies/a.mkv", "alice", http.Header{"Range": {"bytes=100-199"}}, "")
	if r.StatusCode != 206 || r.body != string(s.movieA[100:200]) || r.Header.Get("Content-Range") != fmt.Sprintf("bytes 100-199/%d", len(s.movieA)) {
		t.Errorf("Range: %d %q %q", r.StatusCode, r.Header.Get("Content-Range"), r.body[:min(len(r.body), 20)])
	}

	r = s.do("GET", "/movies/a.mkv", "alice", http.Header{"Range": {"bytes=999999999-"}}, "")
	if r.StatusCode != 416 || r.Header.Get("Content-Range") == "" {
		t.Errorf("unsatisfiable Range: %d Content-Range=%q", r.StatusCode, r.Header.Get("Content-Range"))
	}

	if r = s.do("GET", "/movies/a.mkv", "alice", http.Header{"If-None-Match": {etag}}, ""); r.StatusCode != 304 {
		t.Errorf("If-None-Match: %d", r.StatusCode)
	}

	r = s.do("HEAD", "/movies/a.mkv", "alice", nil, "")
	if r.StatusCode != 200 || r.body != "" || r.Header.Get("Content-Length") != fmt.Sprint(len(s.movieA)) {
		t.Errorf("HEAD: %d len=%q", r.StatusCode, r.Header.Get("Content-Length"))
	}

	// 用列表里返回的 href 原样请求，特殊字符必须往返无损。
	r = s.do("GET", "/movies/%E4%B8%AD%E6%96%87%20%231.mkv", "alice", nil, "")
	if r.StatusCode != 200 || r.body != "unicode movie" {
		t.Errorf("unicode GET: %d %q", r.StatusCode, r.body)
	}
	if r = s.do("GET", "/b.mkv", "bob", nil, ""); r.body != "movie b" {
		t.Errorf("bob GET: %d %q", r.StatusCode, r.body)
	}

	// 客户端没发 Accept-Encoding 时显式要求 identity，避免 Go Transport 的透明解压破坏长度语义；
	// 客户端发了就原样转发。Go 的客户端默认会自动加 gzip，这里关掉它来模拟不发的客户端。
	plain := &http.Client{Transport: &http.Transport{DisableCompression: true}}
	for _, ae := range []string{"", "br, gzip"} {
		req, _ := http.NewRequest("GET", s.ts.URL+"/movies/sub/b.mkv", nil)
		req.SetBasicAuth("alice", "alice-pw")
		if ae != "" {
			req.Header.Set("Accept-Encoding", ae)
		}
		resp, err := plain.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		reqs := s.nas.requests()
		want := cmp.Or(ae, "identity")
		if got := reqs[len(reqs)-1].Header.Get("Accept-Encoding"); got != want {
			t.Errorf("client Accept-Encoding %q: upstream got %q; want %q", ae, got, want)
		}
	}
}

func TestLocalDirectoryMount(t *testing.T) {
	s := newStandard(t)
	ms := parseMultistatus(t, s.propfind("/local/", "alice", "1"))
	if got, want := ms.hrefs(), []string{"/local/", "/local/hello.txt", "/local/sub/"}; !slices.Equal(got, want) {
		t.Errorf("hrefs = %q; want %q", got, want)
	}
	if dn, _ := ms.prop("/local/", "displayname"); dn != "local" {
		t.Errorf("mount root displayname = %q", dn)
	}
	if r := s.do("GET", "/local/hello.txt", "alice", http.Header{"Range": {"bytes=6-"}}, ""); r.StatusCode != 206 || r.body != "local" {
		t.Errorf("Range GET: %d %q", r.StatusCode, r.body)
	}
	for _, p := range []string{"/local/escape.txt", "/local/../secret.txt", "/local/%2e%2e/secret.txt"} {
		r := s.do("GET", p, "alice", nil, "")
		if r.StatusCode == 200 || strings.Contains(r.body, "secret content") {
			t.Errorf("GET %s: %d %q", p, r.StatusCode, r.body)
		}
	}
}

// 无论客户端怎样构造路径，上游都不能收到挂载目标之外的请求。
func TestTraversalIsContained(t *testing.T) {
	s := newStandard(t)
	cases := []struct {
		path string
		want int
	}{
		{"/movies/../work/share/doc.txt", 200}, // 在客户端命名空间内消解，仍是 alice 自己的挂载
		{"/movies/%2e%2e/secret.txt", 404},
		{"/movies/..%2Fsecret.txt", 400},
		{"/movies/x%2F..%2F..%2Fsecret.txt", 400},
		{"/movies/..%5Csecret.txt", 400},
		{"/movies/%252e%252e/secret.txt", 400},
		{"/movies/...", 400},
		{"/movies/..%20/secret.txt", 400},
		{"/movies/a%00.mkv", 400},
	}
	for _, c := range cases {
		for _, method := range []string{"GET", "PROPFIND"} {
			r := s.do(method, c.path, "alice", http.Header{"Depth": {"0"}}, "")
			if strings.Contains(r.body, "secret content") {
				t.Errorf("%s %s leaked secret", method, c.path)
			}
			if method == "GET" && r.StatusCode != c.want {
				t.Errorf("GET %s: %d; want %d", c.path, r.StatusCode, c.want)
			}
		}
	}
	for _, req := range s.nas.requests() {
		if !strings.HasPrefix(req.Path, "/dav/media/movies") && !strings.HasPrefix(req.Path, "/dav/share/team") {
			t.Errorf("upstream received out-of-mount request %s %s", req.Method, req.Path)
		}
	}
	// bob 看不到 alice 的挂载。
	if r := s.do("GET", "/work/share/doc.txt", "bob", nil, ""); r.StatusCode != 404 {
		t.Errorf("bob reached alice's mount: %d", r.StatusCode)
	}
}

func TestReadOnly(t *testing.T) {
	s := newStandard(t)
	for _, method := range []string{"PUT", "DELETE", "MKCOL", "COPY", "MOVE", "PROPPATCH", "LOCK", "UNLOCK", "POST", "PATCH", "SEARCH"} {
		for _, p := range []string{"/", "/movies/a.mkv", "/movies/new.mkv", "/local/hello.txt"} {
			r := s.do(method, p, "alice", http.Header{"Destination": {s.ts.URL + "/movies/copy.mkv"}}, "data")
			if r.StatusCode != 405 || r.Header.Get("Allow") != "OPTIONS, GET, HEAD, PROPFIND" {
				t.Errorf("%s %s: %d Allow=%q", method, p, r.StatusCode, r.Header.Get("Allow"))
			}
		}
	}
	for _, req := range s.nas.requests() {
		t.Errorf("write attempt reached upstream: %s %s", req.Method, req.Path)
	}
	if b, _ := os.ReadFile(filepath.Join(s.localDir, "hello.txt")); string(b) != "hello local" {
		t.Error("local file modified")
	}
}

func TestDepthHeader(t *testing.T) {
	s := newStandard(t)
	for _, p := range []string{"/", "/work", "/movies/"} {
		for _, depth := range []string{"infinity", "Infinity", ""} {
			hdr := http.Header{}
			if depth != "" {
				hdr.Set("Depth", depth)
			}
			r := s.do("PROPFIND", p, "alice", hdr, "")
			if r.StatusCode != 403 || !strings.Contains(r.body, "propfind-finite-depth") {
				t.Errorf("PROPFIND %s Depth %q: %d %s", p, depth, r.StatusCode, r.body)
			}
		}
		if r := s.propfind(p, "alice", "2"); r.StatusCode != 400 {
			t.Errorf("PROPFIND %s Depth 2: %d", p, r.StatusCode)
		}
	}
	if n := len(s.nas.requests()); n != 0 {
		t.Errorf("rejected PROPFINDs reached the upstream %d times", n)
	}
}

func TestUpstreamCredentials(t *testing.T) {
	s := newStandard(t)
	s.propfind("/movies/", "alice", "1")
	s.do("GET", "/movies/a.mkv", "alice", http.Header{"Cookie": {"a=b"}}, "")
	s.propfind("/work/docs/", "alice", "1")

	want := "Basic " + basicAuth("mux", "nas-secret")
	for _, req := range s.nas.requests() {
		if req.Auth != want || req.Cookie != "" {
			t.Errorf("nas got Authorization %q Cookie %q", req.Auth, req.Cookie)
		}
	}
	for _, req := range s.cloud.requests() {
		if req.Auth != "" {
			t.Errorf("unauthenticated upstream got Authorization %q", req.Auth)
		}
	}
}

func basicAuth(user, pass string) string {
	r, _ := http.NewRequest("GET", "/", nil)
	r.SetBasicAuth(user, pass)
	return strings.TrimPrefix(r.Header.Get("Authorization"), "Basic ")
}

func simpleEnv(t *testing.T, upstreamURL string, opts ...func(*Server)) *testEnv {
	return newEnv(t, fmt.Sprintf(`
upstreams:
  up:
    url: %s
    username: mux
    password: right
users:
  alice:
    password_hash: %q
    mounts:
      /m: up:/data
`, upstreamURL, hashOf(t, "alice-pw")), opts...)
}

func withReadTimeout(d time.Duration) func(*Server) {
	return func(s *Server) { s.requestReadTimeout = d }
}

// 极慢地发送请求体的连接必须在读截止时间后被断开，无论请求是否通过认证。
func TestSlowRequestBodyIsCutOff(t *testing.T) {
	up := newHandlerUpstream(t, func(http.ResponseWriter, *http.Request) {})
	e := simpleEnv(t, up.URL, withReadTimeout(300*time.Millisecond))
	for _, authorized := range []bool{true, false} {
		conn, err := net.Dial("tcp", e.ts.Listener.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		head := "PROPFIND /m/ HTTP/1.1\r\nHost: mux\r\nDepth: 1\r\nContent-Type: text/xml\r\nContent-Length: 100000\r\n"
		if authorized {
			head += "Authorization: Basic " + basicAuth("alice", "alice-pw") + "\r\n"
		}
		if _, err := io.WriteString(conn, head+"\r\n<"); err != nil {
			t.Fatal(err)
		}
		stop := make(chan struct{})
		go func() {
			for {
				select {
				case <-stop:
					return
				case <-time.After(50 * time.Millisecond):
					if _, err := conn.Write([]byte("a")); err != nil {
						return
					}
				}
			}
		}()
		start := time.Now()
		conn.SetReadDeadline(time.Now().Add(10 * time.Second))
		io.Copy(io.Discard, conn) // 服务端关闭连接时返回
		elapsed := time.Since(start)
		close(stop)
		conn.Close()
		if elapsed > 3*time.Second {
			t.Errorf("authorized=%v: connection held for %v while the body trickled in", authorized, elapsed)
		}
	}
	if n := len(up.requests()); n != 0 {
		t.Errorf("incomplete requests reached the upstream %d times", n)
	}
}

// 读截止时间只覆盖转发之前的阶段：比它长得多的下载和上游响应都不受影响。
func TestReadDeadlineDoesNotCutLongTransfers(t *testing.T) {
	up := newHandlerUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "PROPFIND" {
			time.Sleep(time.Second)
			w.WriteHeader(207)
			io.WriteString(w, `<D:multistatus xmlns:D="DAV:"><D:response><D:href>/data/</D:href></D:response></D:multistatus>`)
			return
		}
		w.WriteHeader(200)
		for range 10 {
			w.Write(bytes.Repeat([]byte("x"), 1000))
			w.(http.Flusher).Flush()
			time.Sleep(100 * time.Millisecond)
		}
	})
	e := simpleEnv(t, up.URL, withReadTimeout(200*time.Millisecond))
	if r := e.do("GET", "/m/slow.bin", "alice", nil, ""); r.StatusCode != 200 || len(r.body) != 10000 {
		t.Errorf("slow GET: %d, %d bytes", r.StatusCode, len(r.body))
	}
	body := `<propfind xmlns="DAV:"><allprop/></propfind>`
	if r := e.do("PROPFIND", "/m/", "alice", http.Header{"Depth": {"0"}}, body); r.StatusCode != 207 {
		t.Errorf("slow PROPFIND: %d %s", r.StatusCode, r.body)
	}
}

func TestUpstreamErrors(t *testing.T) {
	var status int
	var extra http.Header
	up := newHandlerUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		for k, v := range extra {
			w.Header()[k] = v
		}
		w.Header().Set("WWW-Authenticate", `Basic realm="upstream"`)
		w.Header().Set("Set-Cookie", "upstream=1")
		w.WriteHeader(status)
		io.WriteString(w, "/internal/stack/trace")
	})
	e := simpleEnv(t, up.URL)
	cases := []struct {
		upstream, want int
		extra          http.Header
		check          string
	}{
		{401, 502, nil, ""},
		{407, 502, nil, ""},
		{403, 403, nil, ""},
		{404, 404, nil, ""},
		{405, 405, http.Header{"Allow": {"GET, PUT, PROPFIND"}}, "Allow"},
		{429, 429, http.Header{"Retry-After": {"30"}}, "Retry-After"},
		{500, 502, nil, ""},
		{503, 503, http.Header{"Retry-After": {"5"}}, "Retry-After"},
		{300, 502, nil, ""},
		{201, 502, nil, ""}, // PROPFIND 只接受 207/200
	}
	for _, c := range cases {
		status, extra = c.upstream, c.extra
		for _, method := range []string{"GET", "PROPFIND"} {
			if method == "GET" && c.upstream == 201 {
				continue
			}
			r := e.do(method, "/m/x", "alice", http.Header{"Depth": {"1"}}, "")
			if r.StatusCode != c.want {
				t.Errorf("%s upstream %d: got %d; want %d", method, c.upstream, r.StatusCode, c.want)
			}
			if r.StatusCode != 401 && r.Header.Get("WWW-Authenticate") != "" {
				t.Errorf("%s upstream %d: WWW-Authenticate leaked", method, c.upstream)
			}
			if r.Header.Get("Set-Cookie") != "" || strings.Contains(r.body, "internal") {
				t.Errorf("%s upstream %d: upstream headers/body leaked: %v %q", method, c.upstream, r.Header, r.body)
			}
			if c.check == "Allow" && r.Header.Get("Allow") != "GET, PROPFIND" {
				t.Errorf("405 Allow = %q", r.Header.Get("Allow"))
			}
			if c.check == "Retry-After" && r.Header.Get("Retry-After") == "" {
				t.Errorf("%d: Retry-After not passed", c.upstream)
			}
		}
	}
}

func TestUpstreamUnreachable(t *testing.T) {
	up := newHandlerUpstream(t, func(http.ResponseWriter, *http.Request) {})
	url := up.URL
	up.Close()
	e := simpleEnv(t, url)
	for _, method := range []string{"GET", "PROPFIND"} {
		if r := e.do(method, "/m/x", "alice", http.Header{"Depth": {"0"}}, ""); r.StatusCode != 502 {
			t.Errorf("%s: %d", method, r.StatusCode)
		}
	}
	if !strings.Contains(e.logs.String(), "connection refused") {
		t.Errorf("transport error not logged:\n%s", e.logs.String())
	}
}

func TestResponseHeadersFiltered(t *testing.T) {
	up := newHandlerUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Set-Cookie", "sid=upstream")
		h.Set("X-Internal-Host", "nas.lan")
		h.Set("Cache-Control", "public, max-age=86400")
		h.Set("Location", "http://nas.lan/elsewhere")
		h.Set("Content-Type", "video/x-matroska")
		h.Set("ETag", `"v1"`)
		io.WriteString(w, "content")
	})
	e := simpleEnv(t, up.URL)
	r := e.do("GET", "/m/a.mkv", "alice", nil, "")
	if r.StatusCode != 200 || r.body != "content" {
		t.Fatalf("%d %q", r.StatusCode, r.body)
	}
	for _, h := range []string{"Set-Cookie", "X-Internal-Host", "Location"} {
		if r.Header.Get(h) != "" {
			t.Errorf("%s leaked: %q", h, r.Header.Get(h))
		}
	}
	if r.Header.Get("Cache-Control") != "private" || r.Header.Get("ETag") != `"v1"` || r.Header.Get("Content-Type") != "video/x-matroska" {
		t.Errorf("headers: %v", r.Header)
	}
}

func TestRedirects(t *testing.T) {
	cdn := newHandlerUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		http.ServeContent(w, r, "a.mkv", time.Time{}, strings.NewReader("cdn content"))
	})
	up := newHandlerUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/data/a.mkv":
			http.Redirect(w, r, cdn.URL+"/signed/a.mkv?sig=1", http.StatusFound)
		case r.URL.Path == "/data/rel.mkv":
			http.Redirect(w, r, "/data/a.mkv", http.StatusMovedPermanently)
		case r.URL.Path == "/data/loop":
			http.Redirect(w, r, "/data/loop", http.StatusTemporaryRedirect)
		case r.URL.Path == "/data/see-other":
			http.Redirect(w, r, "/data/", http.StatusSeeOther)
		case r.URL.Path == "/data/dir" && r.Method == "PROPFIND":
			http.Redirect(w, r, "/data/dir/", http.StatusMovedPermanently)
		case r.URL.Path == "/data/dir/" && r.Method == "PROPFIND":
			w.WriteHeader(207)
			io.WriteString(w, `<D:multistatus xmlns:D="DAV:"><D:response><D:href>/data/dir/</D:href></D:response><D:response><D:href>child.mkv</D:href></D:response></D:multistatus>`)
		default:
			http.NotFound(w, r)
		}
	})
	e := simpleEnv(t, up.URL)

	r := e.do("GET", "/m/a.mkv", "alice", http.Header{"Range": {"bytes=4-"}}, "")
	if r.StatusCode != 206 || r.body != "content" {
		t.Errorf("GET via cross-origin redirect: %d %q", r.StatusCode, r.body)
	}
	cdnReqs := cdn.requests()
	if len(cdnReqs) != 1 || cdnReqs[0].Auth != "" || cdnReqs[0].Header.Get("Range") != "bytes=4-" {
		t.Errorf("CDN requests: %+v", cdnReqs)
	}

	if r := e.do("GET", "/m/rel.mkv", "alice", nil, ""); r.StatusCode != 200 || r.body != "cdn content" {
		t.Errorf("GET via relative redirect chain: %d %q", r.StatusCode, r.body)
	}
	if r := e.do("GET", "/m/loop", "alice", nil, ""); r.StatusCode != 502 {
		t.Errorf("redirect loop: %d", r.StatusCode)
	}

	body := `<propfind xmlns="DAV:"><allprop/></propfind>`
	r = e.do("PROPFIND", "/m/dir", "alice", http.Header{"Depth": {"1"}}, body)
	if got := parseMultistatus(t, r).hrefs(); !slices.Equal(got, []string{"/m/dir/", "/m/dir/child.mkv"}) {
		t.Errorf("PROPFIND after redirect: %q", got)
	}
	if r := e.do("PROPFIND", "/m/see-other", "alice", http.Header{"Depth": {"0"}}, ""); r.StatusCode != 502 {
		t.Errorf("PROPFIND 303: %d", r.StatusCode)
	}
	for _, req := range up.requests() {
		if req.Path == "/data/dir/" && (req.Method != "PROPFIND" || req.Body != body || req.Depth != "1") {
			t.Errorf("redirected PROPFIND lost method, body or depth: %+v", req)
		}
		if req.Auth != "Basic "+basicAuth("mux", "right") {
			t.Errorf("same-origin request lacks upstream credentials: %s %s", req.Method, req.Path)
		}
	}
}

func TestPropfindRewriteFailures(t *testing.T) {
	var body string
	up := newHandlerUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(207)
		io.WriteString(w, body)
	})
	e := simpleEnv(t, up.URL)

	body = `<html><body>Login required</body></html>`
	if r := e.propfind("/m/", "alice", "1"); r.StatusCode != 502 || strings.Contains(r.body, "Login") {
		t.Errorf("HTML body: %d %q", r.StatusCode, r.body)
	}

	body = `<D:multistatus xmlns:D="DAV:"><D:response><D:href>/data/</D:href></D:response><D:response><D:href>/other/leak/</D:href></D:response></D:multistatus>`
	r := e.propfind("/m/", "alice", "1")
	if got := parseMultistatus(t, r).hrefs(); !slices.Equal(got, []string{"/m/"}) || strings.Contains(r.body, "leak") {
		t.Errorf("out-of-mount entry not dropped: %s", r.body)
	}
	if logs := e.logs.String(); !strings.Contains(logs, "dropped PROPFIND entries") || !strings.Contains(logs, "/other/leak/") {
		t.Errorf("dropped entries not logged:\n%s", logs)
	}
}

// 上游在响应体发送到一半时断开，客户端必须看到传输错误，而不是一个“正常结束”的截断文件。
func TestUpstreamFailureMidBodyAbortsClient(t *testing.T) {
	up := newHandlerUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "PROPFIND" {
			w.WriteHeader(207)
			io.WriteString(w, `<D:multistatus xmlns:D="DAV:">`)
			for i := range 5000 {
				fmt.Fprintf(w, `<D:response><D:href>/data/f%d</D:href></D:response>`, i)
			}
		} else {
			w.WriteHeader(200)
			w.Write(bytes.Repeat([]byte("x"), 200<<10))
		}
		w.(http.Flusher).Flush()
		panic(http.ErrAbortHandler)
	})
	e := simpleEnv(t, up.URL)
	for _, method := range []string{"GET", "PROPFIND"} {
		req, _ := http.NewRequest(method, e.ts.URL+"/m/f", nil)
		req.Header.Set("Depth", "1")
		req.SetBasicAuth("alice", "alice-pw")
		resp, err := e.ts.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_, err = io.ReadAll(resp.Body)
		resp.Body.Close()
		if err == nil {
			t.Errorf("%s: truncated response delivered as complete (status %d)", method, resp.StatusCode)
		}
	}
}

func TestLargeFileStreaming(t *testing.T) {
	dir := t.TempDir()
	content := make([]byte, 48<<20)
	for i := range content {
		content[i] = byte(i * 7 >> 3)
	}
	if err := os.WriteFile(filepath.Join(dir, "big.bin"), content, 0o644); err != nil {
		t.Fatal(err)
	}
	e := newEnv(t, fmt.Sprintf("upstreams:\n  d: {dir: %q}\nusers:\n  alice:\n    password_hash: %q\n    mounts:\n      /d: d:/\n", dir, hashOf(t, "alice-pw")))
	req, _ := http.NewRequest("GET", e.ts.URL+"/d/big.bin", nil)
	req.SetBasicAuth("alice", "alice-pw")
	resp, err := e.ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	h := sha256.New()
	n, err := io.Copy(h, resp.Body)
	if err != nil || n != int64(len(content)) || !bytes.Equal(h.Sum(nil), sha256Sum(content)) {
		t.Errorf("streamed %d bytes, err %v, checksum match %v", n, err, bytes.Equal(h.Sum(nil), sha256Sum(content)))
	}
}

func sha256Sum(b []byte) []byte {
	s := sha256.Sum256(b)
	return s[:]
}

func TestReload(t *testing.T) {
	s := newStandard(t)
	cfg, err := config.Parse([]byte(fmt.Sprintf(`
upstreams:
  cloud:
    url: %s/remote.php/dav/files/bob
users:
  alice:
    password_hash: %q
    mounts:
      /docs: cloud:/Documents
  carol:
    password_hash: %q
    mounts: {}
`, s.cloud.URL, hashOf(t, "alice-pw"), hashOf(t, "carol-pw"))))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.srv.Reload(cfg); err != nil {
		t.Fatal(err)
	}
	if got := parseMultistatus(t, s.propfind("/", "alice", "1")).hrefs(); !slices.Equal(got, []string{"/", "/docs/"}) {
		t.Errorf("alice after reload: %q", got)
	}
	if got := parseMultistatus(t, s.propfind("/", "carol", "1")).hrefs(); !slices.Equal(got, []string{"/"}) {
		t.Errorf("carol after reload: %q", got)
	}
	if r := s.propfind("/", "bob", "0"); r.StatusCode != 401 {
		t.Errorf("removed user still accepted: %d", r.StatusCode)
	}

	// 无法生效的配置（证书不存在）被拒绝，旧配置保留。
	cfg.TLS = &config.TLS{CertFile: "/nonexistent.pem", KeyFile: "/nonexistent.key"}
	if err := s.srv.Reload(cfg); err == nil {
		t.Error("reload with missing certificate succeeded")
	}
	if r := s.propfind("/docs/", "alice", "0"); r.StatusCode != 207 {
		t.Errorf("previous config lost after failed reload: %d", r.StatusCode)
	}
}
