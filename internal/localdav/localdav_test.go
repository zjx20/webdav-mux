package localdav

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func newTestDir(t *testing.T) (root, dir string) {
	t.Helper()
	root = t.TempDir()
	dir = filepath.Join(root, "share")
	mustMkdir(t, filepath.Join(dir, "sub"))
	mustWrite(t, filepath.Join(dir, "hello.txt"), "hello world")
	mustWrite(t, filepath.Join(dir, "sub", "中文 #1.txt"), "unicode")
	mustWrite(t, filepath.Join(root, "secret.txt"), "top secret")
	// 指向目录外、悬空、绝对路径的符号链接都不能被访问；目录内的相对链接可以。
	mustSymlink(t, "../secret.txt", filepath.Join(dir, "escape.txt"))
	mustSymlink(t, "missing", filepath.Join(dir, "dangling.txt"))
	mustSymlink(t, filepath.Join(dir, "hello.txt"), filepath.Join(dir, "absolute.txt"))
	mustSymlink(t, "hello.txt", filepath.Join(dir, "inside.txt"))
	mustSymlink(t, "sub", filepath.Join(dir, "subdir-link"))
	return root, dir
}

func mustMkdir(t *testing.T, p string) {
	t.Helper()
	if err := os.MkdirAll(p, 0o755); err != nil {
		t.Fatal(err)
	}
}

func mustWrite(t *testing.T, p, content string) {
	t.Helper()
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func mustSymlink(t *testing.T, target, link string) {
	t.Helper()
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
}

func do(t *testing.T, rt http.RoundTripper, method, path string, header http.Header, body string) (*http.Response, string) {
	t.Helper()
	req, err := http.NewRequest(method, "http://local"+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range header {
		req.Header[k] = v
	}
	resp, err := rt.RoundTrip(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("%s %s: reading body: %v", method, path, err)
	}
	return resp, string(b)
}

func TestGetHeadRange(t *testing.T) {
	_, dir := newTestDir(t)
	rt := NewTransport(dir)

	resp, body := do(t, rt, "GET", "/hello.txt", nil, "")
	if resp.StatusCode != 200 || body != "hello world" || resp.ContentLength != 11 || resp.Header.Get("ETag") == "" {
		t.Errorf("GET: %d %q len=%d etag=%q", resp.StatusCode, body, resp.ContentLength, resp.Header.Get("ETag"))
	}

	resp, body = do(t, rt, "HEAD", "/hello.txt", nil, "")
	if resp.StatusCode != 200 || body != "" || resp.ContentLength != 11 {
		t.Errorf("HEAD: %d %q len=%d", resp.StatusCode, body, resp.ContentLength)
	}

	resp, body = do(t, rt, "GET", "/hello.txt", http.Header{"Range": {"bytes=6-"}}, "")
	if resp.StatusCode != 206 || body != "world" || resp.Header.Get("Content-Range") != "bytes 6-10/11" {
		t.Errorf("Range: %d %q %q", resp.StatusCode, body, resp.Header.Get("Content-Range"))
	}

	resp, _ = do(t, rt, "GET", "/sub/%E4%B8%AD%E6%96%87%20%231.txt", nil, "")
	if resp.StatusCode != 200 {
		t.Errorf("unicode name: %d", resp.StatusCode)
	}
}

func TestSymlinksStayInsideRoot(t *testing.T) {
	_, dir := newTestDir(t)
	rt := NewTransport(dir)
	for path, want := range map[string]int{
		"/inside.txt":            200,
		"/subdir-link/中文 #1.txt": 200,
		"/escape.txt":            404,
		"/dangling.txt":          404,
		"/absolute.txt":          404,
		"/../secret.txt":         404,
		"/sub/../../secret.txt":  404,
	} {
		req, _ := http.NewRequest("GET", "http://local/", nil)
		req.URL.Path = path // 绕过客户端的路径清理，直接把 ".." 交给 handler
		resp, err := rt.RoundTrip(req)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != want || bytes.Contains(b, []byte("top secret")) {
			t.Errorf("GET %s: status %d (want %d), body %q", path, resp.StatusCode, want, b)
		}
	}

	resp, body := do(t, rt, "PROPFIND", "/", http.Header{"Depth": {"1"}}, "")
	if resp.StatusCode != 207 {
		t.Fatalf("PROPFIND: %d %s", resp.StatusCode, body)
	}
	for _, name := range []string{"hello.txt", "inside.txt", "subdir-link/", "sub/"} {
		if !strings.Contains(body, "<D:href>/"+name+"</D:href>") {
			t.Errorf("listing lacks %s:\n%s", name, body)
		}
	}
	for _, name := range []string{"escape.txt", "dangling.txt", "absolute.txt"} {
		if strings.Contains(body, name) {
			t.Errorf("listing exposes unresolvable symlink %s:\n%s", name, body)
		}
	}
	// PROPFIND 本身指向不可解析的符号链接时应当是 404，而不是 x/net/webdav 对其它错误返回的 405。
	if resp, _ := do(t, rt, "PROPFIND", "/escape.txt", http.Header{"Depth": {"0"}}, ""); resp.StatusCode != 404 {
		t.Errorf("PROPFIND escaping symlink: %d", resp.StatusCode)
	}
}

func TestReadOnly(t *testing.T) {
	_, dir := newTestDir(t)
	rt := NewTransport(dir)
	for _, r := range []struct{ method, path string }{
		{"PUT", "/new.txt"},
		{"PUT", "/hello.txt"},
		{"DELETE", "/hello.txt"},
		{"MKCOL", "/newdir"},
		{"MOVE", "/hello.txt"},
		{"PROPPATCH", "/hello.txt"},
	} {
		h := http.Header{"Destination": {"http://local/moved.txt"}}
		resp, _ := do(t, rt, r.method, r.path, h, "data")
		if resp.StatusCode < 400 {
			t.Errorf("%s %s: status %d; want an error", r.method, r.path, resp.StatusCode)
		}
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 7 {
		t.Errorf("directory changed: %d entries", len(entries))
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "hello.txt")); string(b) != "hello world" {
		t.Errorf("hello.txt changed: %q", b)
	}
}

// 目录被替换（例如磁盘挂载到了该路径上）之后，新的内容应当立即可见。
func TestDirectoryReplacedAfterStart(t *testing.T) {
	root, dir := newTestDir(t)
	rt := NewTransport(dir)
	if resp, _ := do(t, rt, "GET", "/hello.txt", nil, ""); resp.StatusCode != 200 {
		t.Fatal("setup")
	}
	if err := os.Rename(dir, filepath.Join(root, "old")); err != nil {
		t.Fatal(err)
	}
	mustMkdir(t, dir)
	mustWrite(t, filepath.Join(dir, "new.txt"), "new")
	if resp, body := do(t, rt, "GET", "/new.txt", nil, ""); resp.StatusCode != 200 || body != "new" {
		t.Errorf("new content not visible: %d %q", resp.StatusCode, body)
	}
	if resp, _ := do(t, rt, "GET", "/hello.txt", nil, ""); resp.StatusCode != 404 {
		t.Errorf("old content still visible: %d", resp.StatusCode)
	}
}

// 调用方提前关闭响应体时，handler 必须随之退出，而不是阻塞在管道写入上。
func TestCloseBodyStopsHandler(t *testing.T) {
	done := make(chan error, 1)
	rt := &handlerTransport{handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		buf := make([]byte, 32<<10)
		for {
			if _, err := w.Write(buf); err != nil {
				done <- err
				return
			}
		}
	})}
	req, _ := http.NewRequest("GET", "http://local/", nil)
	resp, err := rt.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.ReadFull(resp.Body, make([]byte, 100<<10)); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	select {
	case err := <-done:
		if !errors.Is(err, io.ErrClosedPipe) {
			t.Errorf("handler write error = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("handler still running after body was closed")
	}
}

func TestContextCancelUnblocksRead(t *testing.T) {
	release := make(chan struct{})
	defer close(release)
	rt := &handlerTransport{handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		<-release // 模拟卡住的磁盘读取：不写也不返回
	})}
	ctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, "GET", "http://local/", nil)
	resp, err := rt.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	errc := make(chan error, 1)
	go func() {
		_, err := resp.Body.Read(make([]byte, 10))
		errc <- err
	}()
	cancel()
	select {
	case err := <-errc:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("read error = %v; want context.Canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("read not unblocked by context cancellation")
	}
}

func TestShortBodyIsUnexpectedEOF(t *testing.T) {
	rt := &handlerTransport{handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "10")
		w.Write([]byte("short"))
	})}
	req, _ := http.NewRequest("GET", "http://local/", nil)
	resp, err := rt.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if _, err := io.ReadAll(resp.Body); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Errorf("error = %v; want io.ErrUnexpectedEOF", err)
	}
}

func TestHandlerPanic(t *testing.T) {
	rt := &handlerTransport{handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		panic("boom")
	})}
	req, _ := http.NewRequest("GET", "http://local/", nil)
	if _, err := rt.RoundTrip(req); err == nil || !strings.Contains(err.Error(), "boom") {
		t.Errorf("panic before headers: err = %v", err)
	}

	rt = &handlerTransport{handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("partial"))
		panic(http.ErrAbortHandler)
	})}
	resp, err := rt.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if _, err := io.ReadAll(resp.Body); err == nil {
		t.Error("panic after headers: body read succeeded; want error")
	}
}

func TestHeaderSnapshot(t *testing.T) {
	rt := &handlerTransport{handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Before", "1")
		w.WriteHeader(http.StatusTeapot)
		w.Header().Set("X-After", "1")
		w.WriteHeader(http.StatusOK)
	})}
	req, _ := http.NewRequest("GET", "http://local/", nil)
	resp, body := do(t, rt, req.Method, "/", nil, "")
	if resp.StatusCode != http.StatusTeapot || resp.Header.Get("X-Before") != "1" || resp.Header.Get("X-After") != "" || body != "" {
		t.Errorf("status %d headers %v body %q", resp.StatusCode, resp.Header, body)
	}
}
