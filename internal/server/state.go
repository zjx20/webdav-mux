package server

import (
	"crypto/rand"
	"crypto/tls"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"golang.org/x/crypto/bcrypt"

	"webdav-mux/internal/config"
	"webdav-mux/internal/davpath"
	"webdav-mux/internal/davxml"
	"webdav-mux/internal/localdav"
)

// state 是一版配置对应的全部运行时数据。配置重载时整体替换；
// 处理中的请求继续使用它开始时取得的那一份，直到结束。
type state struct {
	users    map[string]*user
	cert     *tls.Certificate // 未启用 TLS 时为 nil
	loadedAt time.Time        // 用作合成目录的修改时间

	// dummyHash 用于不存在的用户名：仍做一次 bcrypt 比较，让响应时间不暴露用户名是否存在。
	// 它的代价取所有账号中最高的，所以只有所有账号代价相同时两者的耗时才一致。
	dummyHash []byte
	// authKey 是认证缓存中 HMAC 的密钥，每次加载配置时随机生成。
	authKey []byte

	// external 用于跟随上游重定向到其他源（scheme、主机、端口任一不同）：
	// 不携带上游凭据，也不沿用上游的 insecure_skip_verify。
	external   *http.Client
	transports []*http.Transport
}

type user struct {
	name  string
	hash  []byte
	root  *node
	cache authCache
}

type upstream struct {
	name     string
	base     url.URL // 只含 scheme 和主机
	origin   string
	basePath []string
	username string
	password string
	hasAuth  bool
	client   *http.Client
}

type mount struct {
	path     []string
	upstream *upstream
	target   []string
}

// node 是用户目录树中的一个节点：要么是挂载点（mount 非 nil，没有子节点），
// 要么是本服务合成的目录（列出 names 中的子节点）。
type node struct {
	children map[string]*node
	names    []string // 排序后的子节点名
	mount    *mount
}

// localUpstreamHost 是本地目录上游在 URL 中使用的主机名。请求不会离开进程，
// 使用保留的 .invalid 顶级域名，确保它不可能被解析到真实主机。
const localUpstreamHost = "local-dir.invalid"

// maxDummyCost 限制 dummyHash 的 bcrypt 代价，避免配置了极高代价时每次加载都要计算很久。
const maxDummyCost = 14

func newState(cfg *config.Config) (*state, error) {
	st := &state{
		users:    make(map[string]*user, len(cfg.Users)),
		loadedAt: time.Now(),
		authKey:  make([]byte, 32),
	}
	rand.Read(st.authKey)
	if cfg.TLS != nil {
		cert, err := tls.LoadX509KeyPair(cfg.TLS.CertFile, cfg.TLS.KeyFile)
		if err != nil {
			return nil, err
		}
		st.cert = &cert
	}

	st.external = st.newClient(nil)
	upstreams := make(map[string]*upstream, len(cfg.Upstreams))
	for name, cu := range cfg.Upstreams {
		up := &upstream{name: name}
		if cu.Dir != "" {
			up.base = url.URL{Scheme: "http", Host: localUpstreamHost}
			up.client = &http.Client{Transport: localdav.NewTransport(cu.Dir), CheckRedirect: noFollow}
		} else {
			up.base = url.URL{Scheme: cu.URL.Scheme, Host: cu.URL.Host}
			up.basePath = cu.BasePath
			up.username, up.password = cu.Username, cu.Password
			up.hasAuth = cu.Username != ""
			var tlsConfig *tls.Config
			if cu.InsecureSkipVerify {
				tlsConfig = &tls.Config{InsecureSkipVerify: true}
			}
			up.client = st.newClient(tlsConfig)
		}
		up.origin = originOf(&up.base)
		upstreams[name] = up
	}

	dummyCost := 0
	for name, cu := range cfg.Users {
		u := &user{name: name, hash: cu.PasswordHash, root: &node{}}
		for _, cm := range cu.Mounts {
			u.root.insert(&mount{path: cm.Path, upstream: upstreams[cm.Upstream], target: cm.Target})
		}
		u.root.sortNames()
		st.users[name] = u
		if cost, err := bcrypt.Cost(cu.PasswordHash); err == nil {
			dummyCost = max(dummyCost, cost)
		}
	}
	if dummyCost == 0 {
		dummyCost = bcrypt.DefaultCost
	}
	var err error
	st.dummyHash, err = bcrypt.GenerateFromPassword([]byte(rand.Text()), min(dummyCost, maxDummyCost))
	if err != nil {
		return nil, err
	}
	return st, nil
}

// newClient 创建访问远程上游的 http.Client。它不设整体超时：视频流可能持续数小时，
// 客户端断开时请求的 context 会取消上游请求；ResponseHeaderTimeout 只防止上游在
// 发出响应头之前一直挂起。
func (st *state) newClient(tlsConfig *tls.Config) *http.Client {
	t := &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		ForceAttemptHTTP2:     true,
		TLSClientConfig:       tlsConfig,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: 2 * time.Minute,
		MaxIdleConnsPerHost:   16,
		IdleConnTimeout:       90 * time.Second,
		ExpectContinueTimeout: time.Second,
	}
	st.transports = append(st.transports, t)
	return &http.Client{Transport: t, CheckRedirect: noFollow}
}

// closeIdleConnections 在 state 被替换后释放空闲连接；进行中的请求不受影响。
func (st *state) closeIdleConnections() {
	for _, t := range st.transports {
		t.CloseIdleConnections()
	}
}

func (n *node) insert(m *mount) {
	cur := n
	for _, seg := range m.path {
		child := cur.children[seg]
		if child == nil {
			if cur.children == nil {
				cur.children = make(map[string]*node)
			}
			child = &node{}
			cur.children[seg] = child
			cur.names = append(cur.names, seg)
		}
		cur = child
	}
	cur.mount = m
}

func (n *node) sortNames() {
	slices.Sort(n.names)
	for _, c := range n.children {
		c.sortNames()
	}
}

// resolve 在目录树中查找 segs，结果三选一：
//   - m 非 nil：路径位于挂载点 m 之下，rest 是相对挂载点的剩余段；
//   - dir 非 nil：路径是一个合成目录；
//   - 都为 nil：路径不存在。
func (n *node) resolve(segs []string) (dir *node, m *mount, rest []string) {
	cur := n
	for i, seg := range segs {
		if cur.mount != nil {
			return nil, cur.mount, segs[i:]
		}
		if cur = cur.children[seg]; cur == nil {
			return nil, nil, nil
		}
	}
	if cur.mount != nil {
		return nil, cur.mount, nil
	}
	return cur, nil, nil
}

// name 是挂载点在上级目录中显示的名字；挂载在根上时为空。
func (m *mount) name() string {
	if len(m.path) == 0 {
		return ""
	}
	return m.path[len(m.path)-1]
}

// upstreamURL 返回客户端路径（挂载点下的 rest）对应的上游 URL。
// RawPath 使用 davpath.Encode 的严格编码，文件名中的 "/"、";" 等字符不会被上游误解。
func (m *mount) upstreamURL(rest []string, dir bool) *url.URL {
	segs := slices.Concat(m.upstream.basePath, m.target, rest)
	u := m.upstream.base
	u.Path = "/" + strings.Join(segs, "/")
	if dir && len(segs) > 0 {
		u.Path += "/"
	}
	u.RawPath = davpath.Encode(segs, dir)
	return &u
}

// hrefMapper 返回把上游 href 映射回客户端路径的函数。reqURL 是实际发出的（跟随重定向后的）
// 上游请求 URL，用于解析相对 href。
func (m *mount) hrefMapper(reqURL *url.URL) davxml.HrefMapper {
	prefix := slices.Concat(m.upstream.basePath, m.target)
	base := reqURL.EscapedPath()
	return func(href string) (string, davxml.HrefKind) {
		segs, dir, ok := davpath.ParseHref(href, base)
		if !ok {
			return "", davxml.HrefOutside
		}
		rest, ok := davpath.CutPrefix(segs, prefix)
		if !ok {
			return "", davxml.HrefOutside
		}
		kind := davxml.HrefInside
		if len(rest) == 0 {
			kind = davxml.HrefMountRoot
		}
		return davpath.Encode(slices.Concat(m.path, rest), dir), kind
	}
}

// originOf 返回 URL 的源（scheme://host:port），默认端口补全、大小写归一。
// 上游凭据只发送给与配置的上游同源的地址。
func originOf(u *url.URL) string {
	scheme := strings.ToLower(u.Scheme)
	port := u.Port()
	if port == "" {
		port = map[string]string{"http": "80", "https": "443"}[scheme]
	}
	return scheme + "://" + net.JoinHostPort(strings.ToLower(u.Hostname()), port)
}
