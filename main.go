// webdav-mux 是一个只读的 WebDAV 聚合服务：把多个上游 WebDAV 服务和本地目录的子目录
// 按账号拼成各自的目录树，对客户端提供标准的 WebDAV 访问，所有流量都经由本服务转发。
package main

import (
	"bufio"
	"context"
	"crypto/tls"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"slices"
	"strings"
	"syscall"
	"time"

	"golang.org/x/crypto/bcrypt"
	"golang.org/x/term"

	"webdav-mux/internal/config"
	"webdav-mux/internal/server"
)

const usage = `usage:
  webdav-mux serve [-config config.yaml]   start the server (SIGHUP reloads the config)
  webdav-mux check [-config config.yaml]   validate the config and print the mounts
  webdav-mux hash-password [-cost N]       read a password and print its bcrypt hash
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	var err error
	switch cmd, args := os.Args[1], os.Args[2:]; cmd {
	case "serve":
		err = runServe(args)
	case "check":
		err = runCheck(args)
	case "hash-password":
		err = runHashPassword(args)
	case "help", "-h", "-help", "--help":
		fmt.Print(usage)
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n%s", cmd, usage)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "webdav-mux:", err)
		os.Exit(1)
	}
}

func configFlag(name string, args []string) (string, error) {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	path := fs.String("config", "config.yaml", "path to the YAML config file")
	if err := fs.Parse(args); err != nil {
		return "", err
	}
	if fs.NArg() > 0 {
		return "", fmt.Errorf("unexpected arguments: %q", fs.Args())
	}
	return *path, nil
}

func runServe(args []string) error {
	path, err := configFlag("serve", args)
	if err != nil {
		return err
	}
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	startup, err := config.Load(path)
	if err != nil {
		return err
	}
	srv, err := server.New(startup, logger)
	if err != nil {
		return err
	}

	hs := &http.Server{
		Handler:  srv,
		ErrorLog: slog.NewLogLogger(logger.Handler(), slog.LevelWarn),
	}
	server.ConfigureHTTPServer(hs)
	ln, err := net.Listen("tcp", startup.Listen)
	if err != nil {
		return err
	}
	serveErr := make(chan error, 1)
	if startup.TLS != nil {
		hs.TLSConfig = &tls.Config{MinVersion: tls.VersionTLS12, GetCertificate: srv.GetCertificate}
		go func() { serveErr <- hs.ServeTLS(ln, "", "") }()
	} else {
		go func() { serveErr <- hs.Serve(ln) }()
	}
	logger.Info("listening", "addr", ln.Addr().String(), "tls", startup.TLS != nil, "users", len(startup.Users), "upstreams", len(startup.Upstreams))

	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGHUP, syscall.SIGINT, syscall.SIGTERM)
	for {
		select {
		case err := <-serveErr:
			return err
		case sig := <-signals:
			if sig == syscall.SIGHUP {
				reload(path, startup, srv, logger)
				continue
			}
			logger.Info("shutting down", "signal", sig.String())
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			if err := hs.Shutdown(ctx); err != nil {
				// 超时之后仍在进行的通常是长时间的下载，直接断开。
				hs.Close()
			}
			return nil
		}
	}
}

// reload 重新读取配置并切换。监听地址和是否启用 TLS 在启动时就确定了，
// 重载时沿用启动时的值；证书文件会重新读取，便于更换证书。
func reload(path string, startup *config.Config, srv *server.Server, logger *slog.Logger) {
	cfg, err := config.Load(path)
	if err != nil {
		logger.Error("reload failed; keeping the previous config", "error", err)
		return
	}
	if cfg.Listen != startup.Listen || (cfg.TLS == nil) != (startup.TLS == nil) {
		logger.Warn("changes to listen or to enabling/disabling tls take effect only after a restart")
		cfg.Listen = startup.Listen
		if (cfg.TLS == nil) != (startup.TLS == nil) {
			cfg.TLS = startup.TLS
		}
	}
	if err := srv.Reload(cfg); err != nil {
		logger.Error("reload failed; keeping the previous config", "error", err)
		return
	}
	logger.Info("config reloaded", "users", len(cfg.Users), "upstreams", len(cfg.Upstreams))
}

func runCheck(args []string) error {
	path, err := configFlag("check", args)
	if err != nil {
		return err
	}
	cfg, err := config.Load(path)
	if err != nil {
		return err
	}
	fmt.Printf("config OK: listen %s, tls %v, %d upstreams, %d users\n", cfg.Listen, cfg.TLS != nil, len(cfg.Upstreams), len(cfg.Users))
	for _, name := range sortedKeys(cfg.Upstreams) {
		up := cfg.Upstreams[name]
		if up.Dir != "" {
			fmt.Printf("  upstream %s: dir %s\n", name, up.Dir)
		} else {
			proxy := ""
			if up.Proxy != nil {
				proxy = ", download proxy: " + up.Proxy.String()
			}
			fmt.Printf("  upstream %s: %s/%s (auth: %v%s)\n", name, up.URL, strings.Join(up.BasePath, "/"), up.Username != "", proxy)
		}
	}
	for _, name := range sortedKeys(cfg.Users) {
		fmt.Printf("  user %s:\n", name)
		for _, m := range cfg.Users[name].Mounts {
			fmt.Printf("    %s\n", m)
		}
	}
	return nil
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}

func runHashPassword(args []string) error {
	fs := flag.NewFlagSet("hash-password", flag.ContinueOnError)
	cost := fs.Int("cost", bcrypt.DefaultCost, "bcrypt cost")
	if err := fs.Parse(args); err != nil {
		return err
	}
	password, err := readPassword()
	if err != nil {
		return err
	}
	if password == "" {
		return errors.New("empty password")
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(password), *cost)
	if err != nil {
		return err
	}
	fmt.Println(string(hash))
	return nil
}

// readPassword 在终端上无回显地读取两次密码；非终端（管道）时读取第一行，便于脚本调用。
func readPassword() (string, error) {
	fd := int(os.Stdin.Fd())
	if !term.IsTerminal(fd) {
		line, err := bufio.NewReader(os.Stdin).ReadString('\n')
		if err != nil && !(errors.Is(err, io.EOF) && line != "") {
			return "", err
		}
		return strings.TrimRight(line, "\r\n"), nil
	}
	fmt.Fprint(os.Stderr, "Password: ")
	first, err := term.ReadPassword(fd)
	fmt.Fprintln(os.Stderr)
	if err != nil {
		return "", err
	}
	fmt.Fprint(os.Stderr, "Confirm password: ")
	second, err := term.ReadPassword(fd)
	fmt.Fprintln(os.Stderr)
	if err != nil {
		return "", err
	}
	if string(first) != string(second) {
		return "", errors.New("passwords do not match")
	}
	return string(first), nil
}
