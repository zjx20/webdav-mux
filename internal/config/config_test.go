package config

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"golang.org/x/crypto/bcrypt"
)

func testHash(t *testing.T) string {
	t.Helper()
	h, err := bcrypt.GenerateFromPassword([]byte("pw"), bcrypt.MinCost)
	if err != nil {
		t.Fatal(err)
	}
	return string(h)
}

func TestParseValid(t *testing.T) {
	dir := t.TempDir()
	hash := testHash(t)
	cfg, err := Parse(fmt.Appendf(nil, `
upstreams:
  nas:
    url: https://nas.lan:5006/dav/%%E4%%B8%%AD/
    username: reader
    password: secret
    insecure_skip_verify: true
    proxy: http://127.0.0.1:8090/profiles/fast/
  plain:
    url: http://10.0.0.2
  media:
    dir: %s
users:
  alice:
    password_hash: %q
    mounts:
      /movies/: nas:/media/movies
      /work/docs: plain:/Documents
      /work/share: nas:/
      /我的 电影: media:/sub dir
  bob:
    password_hash: %q
    mounts:
      /: media:/
`, dir, hash, hash))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Listen != DefaultListen || cfg.TLS != nil {
		t.Errorf("defaults: listen=%q tls=%v", cfg.Listen, cfg.TLS)
	}

	nas := cfg.Upstreams["nas"]
	if nas.URL.String() != "https://nas.lan:5006" || !slices.Equal(nas.BasePath, []string{"dav", "中"}) {
		t.Errorf("nas url=%v base=%q", nas.URL, nas.BasePath)
	}
	if nas.Username != "reader" || nas.Password != "secret" || !nas.InsecureSkipVerify {
		t.Errorf("nas credentials not parsed: %+v", nas)
	}
	if nas.Proxy == nil || nas.Proxy.String() != "http://127.0.0.1:8090/profiles/fast/" {
		t.Errorf("nas proxy = %v", nas.Proxy)
	}
	if plain := cfg.Upstreams["plain"]; plain.BasePath != nil || plain.Username != "" || plain.Proxy != nil {
		t.Errorf("plain: %+v", plain)
	}
	if media := cfg.Upstreams["media"]; media.Dir != dir || media.URL != nil {
		t.Errorf("media: %+v", media)
	}

	var got []string
	for _, m := range cfg.Users["alice"].Mounts {
		got = append(got, m.String())
	}
	want := []string{
		"/movies -> nas:/media/movies",
		"/work/docs -> plain:/Documents",
		"/work/share -> nas:/",
		"/我的 电影 -> media:/sub dir",
	}
	if !slices.Equal(got, want) {
		t.Errorf("alice mounts:\n got %q\nwant %q", got, want)
	}
	if bob := cfg.Users["bob"].Mounts; len(bob) != 1 || len(bob[0].Path) != 0 {
		t.Errorf("bob root mount: %v", bob)
	}
}

func TestParseTLSAndListen(t *testing.T) {
	cfg, err := Parse([]byte("listen: 127.0.0.1:9000\ntls:\n  cert: /c.pem\n  key: /k.pem\n"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Listen != "127.0.0.1:9000" || cfg.TLS == nil || cfg.TLS.CertFile != "/c.pem" || cfg.TLS.KeyFile != "/k.pem" {
		t.Errorf("got %+v tls=%+v", cfg, cfg.TLS)
	}
}

func TestParseErrors(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "f")
	if err := os.WriteFile(file, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	hash := testHash(t)
	user := func(mounts string) string {
		return fmt.Sprintf("upstreams:\n  a: {url: 'http://a'}\n  b: {url: 'http://b'}\nusers:\n  u:\n    password_hash: %q\n    mounts:\n%s", hash, mounts)
	}
	tests := []struct {
		name, yaml, want string
	}{
		{"empty", "", "empty"},
		{"unknown top-level field", "listn: ':1'", "listn"},
		{"typo in user field", fmt.Sprintf("users:\n  u:\n    password: x\n    password_hash: %q", hash), "password"},
		{"multiple documents", "listen: ':1'\n---\nlisten: ':2'", "exactly one"},
		{"tls without key", "tls: {cert: /c}", "cert and key"},
		{"upstream without url or dir", "upstreams:\n  a: {}", "either url or dir"},
		{"upstream with both", fmt.Sprintf("upstreams:\n  a: {url: 'http://a', dir: %q}", dir), "mutually exclusive"},
		{"bad upstream name", "upstreams:\n  'a:b': {url: 'http://a'}", "name may contain"},
		{"ftp scheme", "upstreams:\n  a: {url: 'ftp://a'}", "scheme"},
		{"no host", "upstreams:\n  a: {url: 'http:///x'}", "missing host"},
		{"userinfo in url", "upstreams:\n  a: {url: 'http://u:p@a'}", "credentials"},
		{"query in url", "upstreams:\n  a: {url: 'http://a/?x=1'}", "query"},
		{"password without username", "upstreams:\n  a: {url: 'http://a', password: x}", "username is empty"},
		{"relative dir", "upstreams:\n  a: {dir: 'data'}", "absolute"},
		{"missing dir", fmt.Sprintf("upstreams:\n  a: {dir: %q}", filepath.Join(dir, "nope")), "no such file"},
		{"dir is a file", fmt.Sprintf("upstreams:\n  a: {dir: %q}", file), "not a directory"},
		{"credentials on dir", fmt.Sprintf("upstreams:\n  a: {dir: %q, username: x}", dir), "only to url"},
		{"proxy on dir", fmt.Sprintf("upstreams:\n  a: {dir: %q, proxy: 'http://p'}", dir), "only to url"},
		{"proxy scheme", "upstreams:\n  a: {url: 'http://a', proxy: 'socks5://p:1080'}", "scheme"},
		{"proxy without host", "upstreams:\n  a: {url: 'http://a', proxy: 'http:///x'}", "missing host"},
		{"proxy with userinfo", "upstreams:\n  a: {url: 'http://a', proxy: 'http://u:p@p'}", "credentials"},
		{"proxy with query", "upstreams:\n  a: {url: 'http://a', proxy: 'http://p/?profile=x'}", "query"},
		{"user without hash", "users:\n  u: {}", "password_hash is required"},
		{"user with plaintext hash", "users:\n  u: {password_hash: hunter2}", "bcrypt"},
		{"user with placeholder hash", "users:\n  u: {password_hash: '$2a$10$replace.this.with.a.real.bcrypt.hash.generated.by.the.tool'}", "bcrypt"},
		{"user with truncated hash", fmt.Sprintf("users:\n  u: {password_hash: %q}", hash[:59]), "bcrypt"},
		{"user name with colon", fmt.Sprintf("users:\n  'a:b': {password_hash: %q}", hash), "':'"},
		{"mount without upstream prefix", user("      /x: /media"), "<upstream>:/path"},
		{"mount unknown upstream", user("      /x: c:/media"), "unknown upstream"},
		{"mount relative path", user("      x: a:/media"), "mount path"},
		{"mount dot segment", user("      /x/../y: a:/media"), "mount path"},
		{"target relative path", user("      /x: a:media"), "target path"},
		{"target dot segment", user("      /x: a:/m/../n"), "target path"},
		{"overlapping mounts", user("      /x: a:/m\n      /x/y: b:/n"), "overlaps"},
		{"root mount with others", user("      /: a:/m\n      /y: b:/n"), "overlaps"},
		{"duplicate after normalization", user("      /x: a:/m\n      /x/: b:/n"), "overlaps"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Parse([]byte(tt.yaml))
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("error = %v; want containing %q", err, tt.want)
			}
		})
	}
}
