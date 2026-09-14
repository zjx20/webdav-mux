// Package config 读取并校验 webdav-mux 的 YAML 配置文件。
//
// 校验在加载时一次做完：Parse 返回的 Config 里所有路径都已拆成段、所有引用都已解析，
// 运行时代码不再需要处理非法配置。
package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"unicode"

	"go.yaml.in/yaml/v3"
	"golang.org/x/crypto/bcrypt"

	"webdav-mux/internal/davpath"
)

// DefaultListen 是未配置 listen 时的监听地址。
const DefaultListen = ":8080"

// bcryptHashLen 是 bcrypt 哈希字符串（$2a$、$2b$、$2y$ 格式）的固定长度。
const bcryptHashLen = 60

// Config 是校验过的配置。
type Config struct {
	Listen    string
	TLS       *TLS // nil 表示不启用 TLS
	Upstreams map[string]*Upstream
	Users     map[string]*User
}

// TLS 是内置 HTTPS 的证书配置。
type TLS struct {
	CertFile string
	KeyFile  string
}

// Upstream 是一个 WebDAV 上游：远程 WebDAV 服务（URL 非 nil）或本地目录（Dir 非空），二者恰好其一。
type Upstream struct {
	Name string

	// URL 只含 scheme 和主机（含端口）；上游根路径拆成段放在 BasePath。
	URL                *url.URL
	BasePath           []string
	Username           string
	Password           string
	InsecureSkipVerify bool
	// Proxy 是下载代理的基地址（docs/proxy-protocol.md），nil 表示直连。
	// 只用于获取文件内容；PROPFIND 始终直连上游。
	Proxy *url.URL

	// Dir 是本地目录的绝对路径。
	Dir string
}

// User 是一个客户端账号及其目录树。
type User struct {
	Name         string
	PasswordHash []byte
	Mounts       []*Mount // 按路径排序
}

// Mount 把客户端路径 Path 映射到上游 Upstream 的 Target 目录。
// Target 相对上游根（远程上游的 BasePath 或本地目录）。
type Mount struct {
	Path     []string
	Upstream string
	Target   []string
}

var upstreamNameRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]*$`)

type rawConfig struct {
	Listen    string                 `yaml:"listen"`
	TLS       *rawTLS                `yaml:"tls"`
	Upstreams map[string]rawUpstream `yaml:"upstreams"`
	Users     map[string]rawUser     `yaml:"users"`
}

type rawTLS struct {
	Cert string `yaml:"cert"`
	Key  string `yaml:"key"`
}

type rawUpstream struct {
	URL                string `yaml:"url"`
	Dir                string `yaml:"dir"`
	Username           string `yaml:"username"`
	Password           string `yaml:"password"`
	InsecureSkipVerify bool   `yaml:"insecure_skip_verify"`
	Proxy              string `yaml:"proxy"`
}

type rawUser struct {
	PasswordHash string            `yaml:"password_hash"`
	Mounts       map[string]string `yaml:"mounts"`
}

// Load 读取并校验 path 处的配置文件。
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	cfg, err := Parse(data)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return cfg, nil
}

// Parse 解析并校验 YAML 配置。未知字段视为错误：配置里写错字段名（比如把 password_hash
// 拼错）应该在启动时暴露，而不是悄悄变成一个没有密码的账号。
func Parse(data []byte) (*Config, error) {
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	var raw rawConfig
	if err := dec.Decode(&raw); err != nil {
		if errors.Is(err, io.EOF) {
			return nil, errors.New("config is empty")
		}
		return nil, err
	}
	var extra any
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		return nil, errors.New("config must contain exactly one YAML document")
	}

	cfg := &Config{
		Listen:    raw.Listen,
		Upstreams: make(map[string]*Upstream, len(raw.Upstreams)),
		Users:     make(map[string]*User, len(raw.Users)),
	}
	if cfg.Listen == "" {
		cfg.Listen = DefaultListen
	}
	if raw.TLS != nil {
		if raw.TLS.Cert == "" || raw.TLS.Key == "" {
			return nil, errors.New("tls: both cert and key are required")
		}
		cfg.TLS = &TLS{CertFile: raw.TLS.Cert, KeyFile: raw.TLS.Key}
	}

	for _, name := range sortedKeys(raw.Upstreams) {
		up, err := parseUpstream(name, raw.Upstreams[name])
		if err != nil {
			return nil, fmt.Errorf("upstreams.%s: %w", name, err)
		}
		cfg.Upstreams[name] = up
	}
	for _, name := range sortedKeys(raw.Users) {
		u, err := parseUser(name, raw.Users[name], cfg.Upstreams)
		if err != nil {
			return nil, fmt.Errorf("users.%s: %w", name, err)
		}
		cfg.Users[name] = u
	}
	return cfg, nil
}

func parseUpstream(name string, raw rawUpstream) (*Upstream, error) {
	if !upstreamNameRE.MatchString(name) {
		return nil, errors.New("name may contain only letters, digits, '_', '.' and '-', and must not start with a symbol")
	}
	up := &Upstream{Name: name}
	switch {
	case raw.URL != "" && raw.Dir != "":
		return nil, errors.New("url and dir are mutually exclusive")
	case raw.Dir != "":
		// 代理只能按 URL 获取内容，而本地目录上游只存在于本进程内。
		if raw.Username != "" || raw.Password != "" || raw.InsecureSkipVerify || raw.Proxy != "" {
			return nil, errors.New("username, password, insecure_skip_verify and proxy apply only to url upstreams")
		}
		if !filepath.IsAbs(raw.Dir) {
			return nil, fmt.Errorf("dir %q must be an absolute path", raw.Dir)
		}
		dir := filepath.Clean(raw.Dir)
		fi, err := os.Stat(dir)
		if err != nil {
			return nil, err
		}
		if !fi.IsDir() {
			return nil, fmt.Errorf("dir %q is not a directory", dir)
		}
		up.Dir = dir
	case raw.URL != "":
		u, err := url.Parse(raw.URL)
		if err != nil {
			return nil, err
		}
		if u.Scheme != "http" && u.Scheme != "https" {
			return nil, fmt.Errorf("url %q: scheme must be http or https", raw.URL)
		}
		if u.Host == "" {
			return nil, fmt.Errorf("url %q: missing host", raw.URL)
		}
		if u.User != nil {
			return nil, fmt.Errorf("url %q: put credentials in username/password instead of the URL", raw.URL)
		}
		if u.RawQuery != "" || u.ForceQuery || u.Fragment != "" {
			return nil, fmt.Errorf("url %q: query and fragment are not allowed", raw.URL)
		}
		base, _, err := davpath.Parse("/" + strings.TrimPrefix(u.EscapedPath(), "/"))
		if err != nil {
			return nil, fmt.Errorf("url %q: %w", raw.URL, err)
		}
		if raw.Password != "" && raw.Username == "" {
			return nil, errors.New("password is set but username is empty")
		}
		up.URL = &url.URL{Scheme: u.Scheme, Host: u.Host}
		up.BasePath = base
		up.Username = raw.Username
		up.Password = raw.Password
		up.InsecureSkipVerify = raw.InsecureSkipVerify
		if raw.Proxy != "" {
			if up.Proxy, err = parseProxy(raw.Proxy); err != nil {
				return nil, err
			}
		}
	default:
		return nil, errors.New("either url or dir is required")
	}
	return up, nil
}

// parseProxy 校验下载代理的基地址。协议规定基地址不带查询参数和片段；
// 代理自身的认证不在协议之内，所以也不接受 URL 里的用户名密码。
func parseProxy(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("proxy: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, fmt.Errorf("proxy %q: scheme must be http or https", raw)
	}
	if u.Host == "" {
		return nil, fmt.Errorf("proxy %q: missing host", raw)
	}
	if u.User != nil {
		return nil, fmt.Errorf("proxy %q: credentials in the URL are not supported", raw)
	}
	if u.RawQuery != "" || u.ForceQuery || u.Fragment != "" {
		return nil, fmt.Errorf("proxy %q: query and fragment are not allowed", raw)
	}
	return u, nil
}

func parseUser(name string, raw rawUser, upstreams map[string]*Upstream) (*User, error) {
	// RFC 7617：Basic 认证的用户名不能含冒号。
	if strings.ContainsRune(name, ':') || strings.IndexFunc(name, unicode.IsControl) >= 0 {
		return nil, errors.New("user name must not contain ':' or control characters")
	}
	if raw.PasswordHash == "" {
		return nil, errors.New("password_hash is required (generate one with `webdav-mux hash-password`)")
	}
	// bcrypt.Cost 只解析前缀，长度不对的占位字符串也能通过；那样的配置能加载，但谁都登录不了。
	if _, err := bcrypt.Cost([]byte(raw.PasswordHash)); err != nil || len(raw.PasswordHash) != bcryptHashLen {
		return nil, errors.New("password_hash is not a valid bcrypt hash (generate one with `webdav-mux hash-password`)")
	}
	u := &User{Name: name, PasswordHash: []byte(raw.PasswordHash)}

	for _, p := range sortedKeys(raw.Mounts) {
		m, err := parseMount(p, raw.Mounts[p], upstreams)
		if err != nil {
			return nil, fmt.Errorf("mounts[%q]: %w", p, err)
		}
		u.Mounts = append(u.Mounts, m)
	}
	slices.SortFunc(u.Mounts, func(a, b *Mount) int { return slices.Compare(a.Path, b.Path) })
	// 挂载路径之间不能互为前缀：否则同一个客户端路径同时属于两个上游，
	// 目录列表需要合并两边的内容，这不在本服务的语义之内。
	for i, a := range u.Mounts {
		for _, b := range u.Mounts[i+1:] {
			if _, ok := davpath.CutPrefix(b.Path, a.Path); ok {
				return nil, fmt.Errorf("mount %q overlaps mount %q", literal(a.Path), literal(b.Path))
			}
		}
	}
	return u, nil
}

func parseMount(p, target string, upstreams map[string]*Upstream) (*Mount, error) {
	path, err := davpath.SplitLiteral(p)
	if err != nil {
		return nil, fmt.Errorf("mount path must look like /a/b without empty, '.', '..' or dot/space-only segments: %w", err)
	}
	upName, targetPath, ok := strings.Cut(target, ":")
	if !ok {
		return nil, fmt.Errorf("target %q must look like <upstream>:/path", target)
	}
	if _, exists := upstreams[upName]; !exists {
		return nil, fmt.Errorf("unknown upstream %q", upName)
	}
	t, err := davpath.SplitLiteral(targetPath)
	if err != nil {
		return nil, fmt.Errorf("target path %q must look like /a/b without empty, '.', '..' or dot/space-only segments: %w", targetPath, err)
	}
	return &Mount{Path: path, Upstream: upName, Target: t}, nil
}

// literal 把段拼回配置文件里的书写形式，用于错误信息和日志。
func literal(segs []string) string {
	return "/" + strings.Join(segs, "/")
}

// String 返回挂载的可读形式，例如 "/movies -> nas:/media/movies"。
func (m *Mount) String() string {
	return literal(m.Path) + " -> " + m.Upstream + ":" + literal(m.Target)
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
