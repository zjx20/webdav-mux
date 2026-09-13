package server

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"

	"webdav-mux/internal/davpath"
	"webdav-mux/internal/davxml"
)

// maxRedirects 是跟随上游重定向的最大次数。
const maxRedirects = 10

// 请求头和响应头都按白名单转发。客户端的 Authorization、Cookie 绝不能发给上游；
// 上游的 Set-Cookie、WWW-Authenticate、Location 等也不能回给客户端。
var (
	getRequestHeaders = []string{
		"Accept", "Accept-Encoding", "Accept-Language", "If-Match", "If-Modified-Since",
		"If-None-Match", "If-Range", "If-Unmodified-Since", "Range", "User-Agent",
	}
	getResponseHeaders = []string{
		"Accept-Ranges", "Content-Disposition", "Content-Encoding", "Content-Language",
		"Content-Length", "Content-Range", "Content-Type", "ETag", "Last-Modified", "Vary",
	}
	propfindRequestHeaders  = []string{"Accept-Language", "Brief", "Content-Type", "Prefer", "User-Agent"}
	propfindResponseHeaders = []string{"Content-Language", "Preference-Applied", "Vary"}
	// errorResponseHeaders 用于上游返回错误时：429/503 的 Retry-After、416 的 Content-Range。
	errorResponseHeaders = []string{"Content-Range", "Retry-After"}
)

// noFollow 让 http.Client 把重定向响应交还给调用方。跟随逻辑在 roundTrip 里：需要按请求方法
// 决定是否保持方法、按目标源决定是否携带上游凭据，http.Client 的默认行为（比如 301 之后
// 把 PROPFIND 改成 GET、对子域名保留 Authorization）都不符合要求。
func noFollow(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }

type upstreamRequest struct {
	method string
	url    *url.URL
	header http.Header
	body   []byte
}

var errTooManyRedirects = errors.New("upstream: too many redirects")

// roundTrip 向上游发出请求并跟随重定向。上游凭据只附加在发往上游自身源的请求上，
// 所以跟随到 CDN 直链之类的其他源时不会泄露凭据。
func (st *state) roundTrip(ctx context.Context, up *upstream, ur upstreamRequest) (*http.Response, error) {
	target := ur.url
	for hop := 0; ; hop++ {
		req, err := http.NewRequestWithContext(ctx, ur.method, target.String(), bytes.NewReader(ur.body))
		if err != nil {
			return nil, err
		}
		req.Header = ur.header.Clone()
		client := st.external
		if originOf(target) == up.origin {
			client = up.client
			if up.hasAuth {
				req.SetBasicAuth(up.username, up.password)
			}
		}
		resp, err := client.Do(req)
		if err != nil {
			return nil, err
		}
		location := resp.Header.Get("Location")
		if !followable(ur.method, resp.StatusCode) || location == "" {
			return resp, nil
		}
		drainAndClose(resp.Body)
		next, err := target.Parse(location)
		if err != nil {
			return nil, fmt.Errorf("upstream: bad redirect location %q: %w", location, err)
		}
		if next.Scheme != "http" && next.Scheme != "https" {
			return nil, fmt.Errorf("upstream: redirect to unsupported URL %s", next.Redacted())
		}
		if hop == maxRedirects {
			return nil, errTooManyRedirects
		}
		next.Fragment = ""
		target = next
	}
}

// followable 报告是否跟随某个重定向。跟随时总是保持原方法和请求体。
// 303 的语义是“改用 GET 获取另一个资源”，对 PROPFIND 没有意义，不跟随。
func followable(method string, code int) bool {
	switch code {
	case http.StatusMovedPermanently, http.StatusFound, http.StatusTemporaryRedirect, http.StatusPermanentRedirect:
		return true
	case http.StatusSeeOther:
		return method == http.MethodGet || method == http.MethodHead
	}
	return false
}

// drainAndClose 读掉少量剩余响应体后关闭，让连接可以复用。
func drainAndClose(body io.ReadCloser) {
	io.CopyN(io.Discard, body, 64<<10)
	body.Close()
}

// errorStatus 把上游的非成功状态码映射为返回给客户端的状态码。
func errorStatus(upstreamStatus int) int {
	switch {
	case upstreamStatus == http.StatusUnauthorized || upstreamStatus == http.StatusProxyAuthRequired:
		// 上游拒绝的是本服务配置的凭据，与客户端账号无关。原样返回 401 会让客户端以为自己的密码错了。
		return http.StatusBadGateway
	case upstreamStatus >= 400 && upstreamStatus < 500:
		return upstreamStatus
	case upstreamStatus == http.StatusServiceUnavailable || upstreamStatus == http.StatusGatewayTimeout:
		return upstreamStatus
	default:
		// 未能跟随的 3xx、其他 5xx，以及此处不应出现的 1xx/2xx。
		return http.StatusBadGateway
	}
}

// writeUpstreamError 把上游的错误响应转换后写给客户端。上游的错误响应体不转发：
// 其中可能包含上游的内部路径、异常堆栈等信息。
func writeUpstreamError(w http.ResponseWriter, resp *http.Response) {
	copyHeaders(w.Header(), resp.Header, errorResponseHeaders)
	status := errorStatus(resp.StatusCode)
	if status == http.StatusMethodNotAllowed {
		w.Header().Set("Allow", filterAllow(resp.Header.Get("Allow")))
	}
	writeStatus(w, status)
}

// filterAllow 返回上游 Allow 头中本服务也支持的方法。RFC 9110 要求 405 响应带 Allow。
func filterAllow(upstreamAllow string) string {
	var allowed []string
	for m := range strings.SplitSeq(upstreamAllow, ",") {
		m = strings.ToUpper(strings.TrimSpace(m))
		if slices.Contains(supportedMethods, m) && !slices.Contains(allowed, m) {
			allowed = append(allowed, m)
		}
	}
	if len(allowed) == 0 {
		return "OPTIONS, PROPFIND"
	}
	return strings.Join(allowed, ", ")
}

func (s *Server) writeTransportError(w http.ResponseWriter, r *http.Request, rl *requestLog, err error) {
	rl.err = err
	if r.Context().Err() != nil {
		return // 客户端已断开，没有人接收响应
	}
	status := http.StatusBadGateway
	var ne net.Error
	if errors.Is(err, context.DeadlineExceeded) || errors.As(err, &ne) && ne.Timeout() {
		status = http.StatusGatewayTimeout
	}
	writeStatus(w, status)
}

func copyHeaders(dst, src http.Header, names []string) {
	for _, name := range names {
		if values := src.Values(name); len(values) > 0 {
			dst[http.CanonicalHeaderKey(name)] = slices.Clone(values)
		}
	}
}

// serveGet 处理挂载点下的 GET 和 HEAD：流式转发，Range 和条件请求头原样透传。
func (s *Server) serveGet(w http.ResponseWriter, r *http.Request, st *state, rl *requestLog, m *mount, rest []string, dir bool) {
	header := make(http.Header)
	copyHeaders(header, r.Header, getRequestHeaders)
	if header.Get("Accept-Encoding") == "" {
		// 必须显式声明：否则 Go 的 Transport 会自动请求 gzip 并透明解压，
		// 解压后的响应体与上游给出的 Content-Length、Content-Range、ETag 对不上。
		header.Set("Accept-Encoding", "identity")
	}
	resp, err := st.roundTrip(r.Context(), m.upstream, upstreamRequest{method: r.Method, url: m.upstreamURL(rest, dir), header: header})
	if err != nil {
		s.writeTransportError(w, r, rl, err)
		return
	}
	defer resp.Body.Close()
	rl.upstreamStatus = resp.StatusCode

	if resp.StatusCode < 200 || resp.StatusCode >= 300 && resp.StatusCode != http.StatusNotModified {
		writeUpstreamError(w, resp)
		return
	}
	copyHeaders(w.Header(), resp.Header, getResponseHeaders)
	w.WriteHeader(resp.StatusCode)
	if r.Method == http.MethodHead || resp.StatusCode == http.StatusNoContent || resp.StatusCode == http.StatusNotModified {
		return
	}
	s.copyBody(w, r, rl, resp.Body)
}

var copyBufPool = sync.Pool{New: func() any { b := make([]byte, 64<<10); return &b }}

// copyBody 把上游响应体复制给客户端。上游中途出错时中断客户端连接：响应头已经发出，
// 只有中断连接才能让客户端知道内容不完整（分块传输时尤其如此）。
func (s *Server) copyBody(w http.ResponseWriter, r *http.Request, rl *requestLog, body io.Reader) {
	bufp := copyBufPool.Get().(*[]byte)
	defer copyBufPool.Put(bufp)
	buf := *bufp
	for {
		n, rerr := body.Read(buf)
		if n > 0 {
			if _, werr := w.Write(buf[:n]); werr != nil {
				rl.err = fmt.Errorf("writing to client: %w", werr)
				return
			}
		}
		switch {
		case rerr == io.EOF:
			return
		case rerr != nil && r.Context().Err() != nil:
			rl.err = fmt.Errorf("client went away: %w", r.Context().Err())
			return
		case rerr != nil:
			rl.err = fmt.Errorf("reading from upstream: %w", rerr)
			panic(http.ErrAbortHandler)
		}
	}
}

// serveUpstreamPropfind 把 PROPFIND 转发给上游，并把响应里的 href 改写回客户端路径。
func (s *Server) serveUpstreamPropfind(w http.ResponseWriter, r *http.Request, st *state, rl *requestLog, m *mount, rest []string, dir bool, depth string, body []byte) {
	header := make(http.Header)
	copyHeaders(header, r.Header, propfindRequestHeaders)
	header.Set("Depth", depth)
	if len(body) > 0 && header.Get("Content-Type") == "" {
		header.Set("Content-Type", "application/xml; charset=utf-8")
	}
	resp, err := st.roundTrip(r.Context(), m.upstream, upstreamRequest{method: "PROPFIND", url: m.upstreamURL(rest, dir), header: header, body: body})
	if err != nil {
		s.writeTransportError(w, r, rl, err)
		return
	}
	defer resp.Body.Close()
	rl.upstreamStatus = resp.StatusCode

	// 个别上游对 PROPFIND 回 200 而不是 207；只要响应体确实是 multistatus 就照常处理。
	if resp.StatusCode != http.StatusMultiStatus && resp.StatusCode != http.StatusOK {
		writeUpstreamError(w, resp)
		return
	}
	copyHeaders(w.Header(), resp.Header, propfindResponseHeaders)
	w.Header().Set("Content-Type", "application/xml; charset=utf-8")

	lw := &lazyWriter{w: w, status: http.StatusMultiStatus}
	bw := bufio.NewWriterSize(lw, 64<<10)
	stats, err := davxml.RewriteMultistatus(bw, resp.Body, davxml.RewriteOptions{
		MapHref:   m.hrefMapper(resp.Request.URL),
		MountName: m.name(),
	})
	if err == nil {
		err = bw.Flush()
	}
	if stats.Dropped > 0 {
		s.log.Warn("dropped PROPFIND entries outside the mount target; check that the upstream url matches the hrefs it returns",
			"upstream", m.upstream.name,
			"dropped", stats.Dropped,
			"example_href", stats.FirstDropped,
			"expected_prefix", davpath.Encode(slices.Concat(m.upstream.basePath, m.target), true))
	}
	if err == nil {
		return
	}
	rl.err = fmt.Errorf("rewriting upstream multistatus: %w", err)
	switch {
	case r.Context().Err() != nil:
	case !lw.started:
		w.Header().Del("Content-Type")
		writeStatus(w, http.StatusBadGateway)
	default:
		// 已经向客户端写出了部分 XML，只能中断连接，避免客户端把截断的列表当成完整的。
		panic(http.ErrAbortHandler)
	}
}

// lazyWriter 在第一次真正写出数据时才发送状态码。改写器前面有 64KB 缓冲，
// 缓冲写满之前出错时客户端还没收到任何字节，仍然可以返回干净的 502。
type lazyWriter struct {
	w       http.ResponseWriter
	status  int
	started bool
}

func (l *lazyWriter) Write(p []byte) (int, error) {
	if !l.started {
		l.started = true
		l.w.WriteHeader(l.status)
	}
	return l.w.Write(p)
}
