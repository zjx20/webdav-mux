// Package localdav 把本地目录包装成一个只读的 WebDAV 上游。
//
// 本地目录由 golang.org/x/net/webdav 提供 WebDAV 协议实现，并通过一个进程内的
// http.RoundTripper 访问。这样本地目录挂载与远程上游走完全相同的挂载、转发和
// href 改写逻辑，而且不需要监听任何端口（本机其他进程无法绕过认证直接访问）。
package localdav

import (
	"context"
	"errors"
	"io/fs"
	"net/http"
	"os"
	"path"
	"path/filepath"

	"golang.org/x/net/webdav"
)

// NewHandler 返回以只读方式提供 dir 目录内容的 WebDAV handler。
//
// 所有路径解析都经过 os.Root，因此请求无法逃出 dir：".." 和指向 dir 之外的符号链接
// 都会被当作不存在（os.Root 也拒绝绝对路径形式的符号链接，即使目标在 dir 之内）。
func NewHandler(dir string) http.Handler {
	return &webdav.Handler{
		FileSystem: readOnlyFS{dir: dir},
		LockSystem: webdav.NewMemLS(),
	}
}

// NewTransport 返回在进程内用 NewHandler(dir) 处理请求的 http.RoundTripper。
func NewTransport(dir string) http.RoundTripper {
	return &handlerTransport{handler: NewHandler(dir)}
}

// readOnlyFS 实现 webdav.FileSystem。每次操作都重新打开 dir 对应的 os.Root，
// 这样在服务启动之后才挂载到 dir 上的文件系统（比如插上的移动硬盘）也能立即看到；
// 长期持有一个 Root 会一直指向挂载之前的那个目录。
type readOnlyFS struct {
	dir string
}

func permissionError(op, name string) error {
	return &fs.PathError{Op: op, Path: name, Err: fs.ErrPermission}
}

func (readOnlyFS) Mkdir(_ context.Context, name string, _ os.FileMode) error {
	return permissionError("mkdir", name)
}

func (readOnlyFS) RemoveAll(_ context.Context, name string) error {
	return permissionError("removeall", name)
}

func (readOnlyFS) Rename(_ context.Context, oldName, _ string) error {
	return permissionError("rename", oldName)
}

const writeFlags = os.O_WRONLY | os.O_RDWR | os.O_APPEND | os.O_CREATE | os.O_TRUNC

func (f readOnlyFS) OpenFile(ctx context.Context, name string, flag int, _ os.FileMode) (webdav.File, error) {
	if flag&writeFlags != 0 {
		return nil, permissionError("open", name)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	root, err := os.OpenRoot(f.dir)
	if err != nil {
		return nil, err
	}
	// 从 Root 打开的文件在 Root 关闭后仍然有效。
	defer root.Close()
	file, err := root.Open(rel(name))
	if err != nil {
		return nil, notFoundIfUnresolvable(root, name, err)
	}
	return readOnlyFile{file}, nil
}

func (f readOnlyFS) Stat(ctx context.Context, name string) (os.FileInfo, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	root, err := os.OpenRoot(f.dir)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	fi, err := root.Stat(rel(name))
	if err != nil {
		return nil, notFoundIfUnresolvable(root, name, err)
	}
	return fi, nil
}

// notFoundIfUnresolvable 把“名字本身存在、但在 Root 内无法解析”（指向外部、悬空或
// 绝对路径的符号链接）的错误转换成 fs.ErrNotExist。对客户端来说，这类条目在这棵目录树里
// 就是不存在的：PROPFIND 返回 404、目录列表里跳过它，而不是 405 或 500。
func notFoundIfUnresolvable(root *os.Root, name string, err error) error {
	if errors.Is(err, fs.ErrNotExist) || errors.Is(err, fs.ErrPermission) {
		return err
	}
	if fi, lerr := root.Lstat(rel(name)); lerr == nil && fi.Mode()&fs.ModeSymlink != 0 {
		return &fs.PathError{Op: "stat", Path: name, Err: fs.ErrNotExist}
	}
	return err
}

// rel 把 webdav 传入的 slash 路径（以 "/" 开头）转换成 os.Root 接受的相对路径。
func rel(name string) string {
	p := path.Clean("/" + name)
	if p == "/" {
		return "."
	}
	return filepath.FromSlash(p[1:])
}

// readOnlyFile 屏蔽写操作。文件本来就是只读打开的，这里让错误语义更明确。
type readOnlyFile struct {
	*os.File
}

func (f readOnlyFile) Write([]byte) (int, error) {
	return 0, permissionError("write", f.Name())
}
