package localdav

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"sync"
)

// handlerTransport 是在进程内调用 http.Handler 的 http.RoundTripper。
//
// handler 在独立的 goroutine 中运行，响应体经 io.Pipe 流式传给调用方：大文件不会被
// 整个读进内存，调用方关闭响应体或取消请求的 context 时，handler 的下一次写入会失败返回。
type handlerTransport struct {
	handler http.Handler
}

func (t *handlerTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	ctx := req.Context()
	// handler 看到的是服务端视角的请求。
	sreq := req.Clone(ctx)
	if sreq.Body == nil {
		sreq.Body = http.NoBody
	}
	sreq.RequestURI = req.URL.RequestURI()
	if sreq.Host == "" {
		sreq.Host = req.URL.Host
	}

	pr, pw := io.Pipe()
	w := &pipeResponseWriter{header: make(http.Header), pw: pw, committed: make(chan struct{})}
	go w.serve(t.handler, sreq)

	select {
	case <-w.committed:
	case <-ctx.Done():
		pr.CloseWithError(context.Cause(ctx))
		return nil, context.Cause(ctx)
	}
	if w.failure != nil {
		pr.Close()
		return nil, w.failure
	}

	resp := &http.Response{
		Status:        fmt.Sprintf("%d %s", w.status, http.StatusText(w.status)),
		StatusCode:    w.status,
		Proto:         "HTTP/1.1",
		ProtoMajor:    1,
		ProtoMinor:    1,
		Header:        w.sent,
		ContentLength: -1,
		Request:       req,
	}
	body := &pipeBody{pr: pr, remaining: -1}
	if cl, err := strconv.ParseInt(w.sent.Get("Content-Length"), 10, 64); err == nil && cl >= 0 {
		resp.ContentLength = cl
		// HEAD 响应的 Content-Length 描述的是 GET 的实体长度，响应体本身为空。
		if req.Method != http.MethodHead {
			body.remaining = cl
		}
	}
	// 关闭写端：阻塞中的 Read 立即返回取消原因，handler 的下一次写入返回 io.ErrClosedPipe。
	body.stop = context.AfterFunc(ctx, func() { pw.CloseWithError(context.Cause(ctx)) })
	resp.Body = body
	return resp, nil
}

// pipeResponseWriter 把 handler 的输出写进管道。状态码和响应头在第一次 WriteHeader
// 或 Write 时提交，之后对 Header() 的修改不再生效，与 net/http 的语义一致。
type pipeResponseWriter struct {
	header http.Header
	pw     *io.PipeWriter

	once      sync.Once
	committed chan struct{} // 提交后关闭；之后 status、sent、failure 只读
	status    int
	sent      http.Header
	failure   error // handler 在提交响应前 panic 时非 nil
}

func (w *pipeResponseWriter) serve(h http.Handler, r *http.Request) {
	defer func() {
		// RoundTripper 必须关闭请求体。
		r.Body.Close()
		if p := recover(); p != nil {
			// 这里不能再 panic：这个 goroutine 不在 net/http 的 recover 保护之下。
			err := fmt.Errorf("localdav: handler panic: %v", p)
			w.commit(0, err)
			w.pw.CloseWithError(err)
			return
		}
		w.commit(http.StatusOK, nil)
		w.pw.Close()
	}()
	h.ServeHTTP(w, r)
}

func (w *pipeResponseWriter) commit(status int, failure error) {
	w.once.Do(func() {
		w.status = status
		w.failure = failure
		w.sent = w.header.Clone()
		close(w.committed)
	})
}

func (w *pipeResponseWriter) Header() http.Header { return w.header }

func (w *pipeResponseWriter) WriteHeader(code int) {
	if code < 200 {
		// 1xx 信息性响应不是最终响应，不提交。
		return
	}
	w.commit(code, nil)
}

func (w *pipeResponseWriter) Write(p []byte) (int, error) {
	w.commit(http.StatusOK, nil)
	return w.pw.Write(p)
}

// pipeBody 是响应体。已知 Content-Length 时校验实际长度：handler 提前结束
// （比如文件在读取过程中被截断）必须表现为 io.ErrUnexpectedEOF，而不是一个“正常结束”的短响应。
type pipeBody struct {
	pr        *io.PipeReader
	stop      func() bool
	remaining int64 // -1 表示长度未知
}

func (b *pipeBody) Read(p []byte) (int, error) {
	if b.remaining == 0 {
		return 0, io.EOF
	}
	if b.remaining > 0 && int64(len(p)) > b.remaining {
		p = p[:b.remaining]
	}
	n, err := b.pr.Read(p)
	if b.remaining > 0 {
		b.remaining -= int64(n)
		if err == io.EOF && b.remaining > 0 {
			err = io.ErrUnexpectedEOF
		}
	}
	return n, err
}

// Close 关闭管道读端，让仍在写的 handler 立即出错返回。
func (b *pipeBody) Close() error {
	b.stop()
	return b.pr.Close()
}
