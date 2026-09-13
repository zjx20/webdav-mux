// Package server 实现 webdav-mux 的 HTTP 处理逻辑：认证、按用户目录树解析路径、
// 把请求转发给挂载点对应的上游，以及为挂载点之上的目录合成 WebDAV 响应。
package server

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"runtime"
	"slices"
	"strings"
	"sync/atomic"
	"time"

	"webdav-mux/internal/config"
	"webdav-mux/internal/davpath"
	"webdav-mux/internal/davxml"
)

// supportedMethods 是本服务接受的全部方法。服务是只读的，写方法一律返回 405。
var supportedMethods = []string{http.MethodOptions, http.MethodGet, http.MethodHead, "PROPFIND"}

// maxPropfindBody 限制 PROPFIND 请求体的大小；正常的请求体只有几百字节。
const maxPropfindBody = 1 << 20

// defaultRequestReadTimeout 是从开始处理请求到开始转发之间（认证、读取请求体）允许的最长读取时间，
// 见 serve。
const defaultRequestReadTimeout = 30 * time.Second

// Server 是 webdav-mux 的 http.Handler。
type Server struct {
	log   *slog.Logger
	state atomic.Pointer[state]
	// bcryptSlots 限制同时进行的 bcrypt 计算数量，见 authenticate。
	bcryptSlots chan struct{}
	// requestReadTimeout 默认为 defaultRequestReadTimeout；测试中调小。
	requestReadTimeout time.Duration
}

// New 按配置创建 Server。
func New(cfg *config.Config, log *slog.Logger) (*Server, error) {
	s := &Server{
		log:                log,
		bcryptSlots:        make(chan struct{}, max(1, runtime.GOMAXPROCS(0)/2)),
		requestReadTimeout: defaultRequestReadTimeout,
	}
	if err := s.Reload(cfg); err != nil {
		return nil, err
	}
	return s, nil
}

// Reload 原子地切换到新配置。新配置无法生效（例如证书读取失败）时返回错误并保留旧配置。
// 进行中的请求继续使用旧配置直到结束。
func (s *Server) Reload(cfg *config.Config) error {
	st, err := newState(cfg)
	if err != nil {
		return err
	}
	if old := s.state.Swap(st); old != nil {
		old.closeIdleConnections()
	}
	return nil
}

// GetCertificate 返回当前配置的 TLS 证书，供 tls.Config.GetCertificate 使用，
// 这样重载配置即可更换证书，无需重启。
func (s *Server) GetCertificate(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	if cert := s.state.Load().cert; cert != nil {
		return cert, nil
	}
	return nil, errors.New("no TLS certificate configured")
}

// requestLog 收集一次请求的访问日志字段。
type requestLog struct {
	user           string
	upstream       string
	upstreamStatus int
	err            error
}

func (s *Server) ServeHTTP(rw http.ResponseWriter, r *http.Request) {
	start := time.Now()
	w := &statusWriter{ResponseWriter: rw}
	var rl requestLog
	defer func() {
		p := recover()
		if p != nil && p != http.ErrAbortHandler {
			rl.err = fmt.Errorf("panic: %v", p)
		}
		s.logRequest(r, w, &rl, time.Since(start))
		if p != nil {
			panic(p)
		}
	}()
	s.serve(w, r, &rl)
}

func (s *Server) serve(w http.ResponseWriter, r *http.Request, rl *requestLog) {
	// 限制读取请求的时间，防止客户端极慢地发送请求体来长期占住连接：net/http 的
	// ReadHeaderTimeout 只覆盖请求头。不能改用 Server.ReadTimeout：net/http 在处理请求期间
	// 会在后台读连接，读超时会取消请求的 context，从而打断持续数小时的下载。
	// 所以只在转发开始之前设读截止时间，开始转发时清除（clearReadDeadline）；请求被拒绝时
	// 截止时间保留，net/http 在响应之后丢弃未读请求体的过程同样受它限制。
	rc := http.NewResponseController(w)
	rc.SetReadDeadline(time.Now().Add(s.requestReadTimeout))
	clearReadDeadline := func() { rc.SetReadDeadline(time.Time{}) }

	// 同一个 URL 在不同账号下是不同的资源，禁止共享缓存（CDN、缓存代理）存储任何响应。
	w.Header().Set("Cache-Control", "private")

	if r.Method == http.MethodOptions {
		// OPTIONS 不需要认证：它只声明协议能力，不涉及任何内容。
		h := w.Header()
		h.Set("DAV", "1")
		h.Set("Allow", strings.Join(supportedMethods, ", "))
		h.Set("MS-Author-Via", "DAV")
		h.Set("Content-Length", "0")
		w.WriteHeader(http.StatusOK)
		return
	}

	st := s.state.Load()
	u := s.authenticate(r, st)
	if u == nil {
		w.Header().Set("WWW-Authenticate", `Basic realm="webdav-mux", charset="UTF-8"`)
		writeStatus(w, http.StatusUnauthorized)
		return
	}
	rl.user = u.name

	if !slices.Contains(supportedMethods, r.Method) {
		w.Header().Set("Allow", strings.Join(supportedMethods, ", "))
		writeStatus(w, http.StatusMethodNotAllowed)
		return
	}
	segs, dir, err := davpath.Parse(r.URL.EscapedPath())
	if err != nil {
		rl.err = err
		writeStatus(w, http.StatusBadRequest)
		return
	}
	synthetic, m, rest := u.root.resolve(segs)
	if synthetic == nil && m == nil {
		writeStatus(w, http.StatusNotFound)
		return
	}
	if m != nil {
		rl.upstream = m.upstream.name
	}

	if r.Method != "PROPFIND" { // GET、HEAD
		if m == nil {
			w.Header().Set("Allow", "OPTIONS, PROPFIND")
			writeStatus(w, http.StatusMethodNotAllowed)
			return
		}
		clearReadDeadline()
		s.serveGet(w, r, st, rl, m, rest, dir)
		return
	}

	depth, ok := checkDepth(w, r)
	if !ok {
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxPropfindBody))
	if err != nil {
		rl.err = err
		if _, tooLarge := errors.AsType[*http.MaxBytesError](err); tooLarge {
			writeStatus(w, http.StatusRequestEntityTooLarge)
		} else {
			writeStatus(w, http.StatusBadRequest)
		}
		return
	}
	clearReadDeadline()
	if m != nil {
		s.serveUpstreamPropfind(w, r, st, rl, m, rest, dir, depth, body)
	} else {
		serveSyntheticPropfind(w, rl, st, synthetic, segs, depth, body)
	}
}

// checkDepth 校验 PROPFIND 的 Depth 头，返回 "0" 或 "1"。
// 按 RFC 4918 §9.1，缺省的 Depth 等同于 infinity；本服务拒绝 infinity（RFC 允许这样做），
// 以免一个请求就让上游遍历整棵目录树。
func checkDepth(w http.ResponseWriter, r *http.Request) (string, bool) {
	switch depth := strings.ToLower(strings.TrimSpace(r.Header.Get("Depth"))); depth {
	case "0", "1":
		return depth, true
	case "", "infinity":
		w.Header().Set("Content-Type", "application/xml; charset=utf-8")
		w.WriteHeader(http.StatusForbidden)
		io.WriteString(w, davxml.FiniteDepthError)
	default:
		writeStatus(w, http.StatusBadRequest)
	}
	return "", false
}

// serveSyntheticPropfind 为合成目录（根目录或挂载点的上级目录）生成 PROPFIND 响应。
// 属性是固定值，不访问上游：某个上游不可用时，目录树本身仍然可以浏览。
func serveSyntheticPropfind(w http.ResponseWriter, rl *requestLog, st *state, n *node, segs []string, depth string, body []byte) {
	pf, err := davxml.ParsePropfind(body)
	if err != nil {
		rl.err = err
		writeStatus(w, http.StatusBadRequest)
		return
	}
	self := davxml.Collection{Href: davpath.Encode(segs, true), ModTime: st.loadedAt}
	if len(segs) > 0 {
		self.DisplayName = segs[len(segs)-1]
	}
	cols := []davxml.Collection{self}
	if depth == "1" {
		for _, name := range n.names {
			cols = append(cols, davxml.Collection{
				Href:        davpath.Encode(append(slices.Clip(segs), name), true),
				DisplayName: name,
				ModTime:     st.loadedAt,
			})
		}
	}
	w.Header().Set("Content-Type", "application/xml; charset=utf-8")
	w.WriteHeader(http.StatusMultiStatus)
	if err := davxml.WriteCollections(w, cols, pf); err != nil {
		rl.err = err
	}
}

// ConfigureHTTPServer 设置对外 http.Server 的超时和选项。服务直接暴露在公网时，
// ReadHeaderTimeout 和 MaxHeaderBytes 防止慢速或超大请求头占住连接。
// 不设 WriteTimeout 和 ReadTimeout：视频流响应可以持续数小时，而 ReadTimeout 会在处理期间
// 取消请求的 context；请求体的读取时限由 Server.serve 按请求设置。
func ConfigureHTTPServer(hs *http.Server) {
	hs.ReadHeaderTimeout = 10 * time.Second
	hs.IdleTimeout = 2 * time.Minute
	hs.MaxHeaderBytes = 64 << 10
	// 让 "OPTIONS *" 也由 Server 回答，带上 DAV 能力声明，而不是 net/http 内置的空响应。
	hs.DisableGeneralOptionsHandler = true
}

func writeStatus(w http.ResponseWriter, code int) {
	http.Error(w, http.StatusText(code), code)
}

func (s *Server) logRequest(r *http.Request, w *statusWriter, rl *requestLog, elapsed time.Duration) {
	attrs := []slog.Attr{
		slog.String("method", r.Method),
		slog.String("path", r.URL.EscapedPath()),
		slog.Int("status", w.status),
		slog.Int64("bytes", w.bytes),
		slog.Duration("duration", elapsed),
		slog.String("remote", r.RemoteAddr),
	}
	if rl.user != "" {
		attrs = append(attrs, slog.String("user", rl.user))
	}
	if rl.upstream != "" {
		attrs = append(attrs, slog.String("upstream", rl.upstream), slog.Int("upstream_status", rl.upstreamStatus))
	}
	level := slog.LevelInfo
	if rl.err != nil {
		level = slog.LevelWarn
		attrs = append(attrs, slog.String("error", rl.err.Error()))
	}
	s.log.LogAttrs(context.Background(), level, "request", attrs...)
}

// statusWriter 记录响应状态码和写出的字节数，用于访问日志。
type statusWriter struct {
	http.ResponseWriter
	status int
	bytes  int64
}

func (w *statusWriter) WriteHeader(code int) {
	if w.status == 0 && code >= 200 {
		w.status = code
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Write(p []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	n, err := w.ResponseWriter.Write(p)
	w.bytes += int64(n)
	return n, err
}

// Unwrap 让 http.ResponseController 能找到底层的 ResponseWriter。
func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }
