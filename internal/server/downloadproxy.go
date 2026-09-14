package server

import (
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// 本文件实现下载代理协议的调用方，协议定义见 docs/proxy-protocol.md。

// proxyEndpoint 按协议把代理基地址拼成请求地址：去掉末尾的 "/" 后接 "/proxy"。
// 基地址的路径按转义形式拼接，其中的 %2F 等转义保持原样。
func proxyEndpoint(base *url.URL) *url.URL {
	u := *base
	u.RawPath = strings.TrimSuffix(base.EscapedPath(), "/") + "/proxy"
	u.Path, _ = url.PathUnescape(u.RawPath) // EscapedPath 的结果总是合法的转义
	return &u
}

// proxyURL 返回获取 target 的协议请求地址。target.String() 保留 upstreamURL 生成的严格转义，
// 协议要求代理原样使用解码后的目标 URL。
func (up *upstream) proxyURL(target *url.URL) *url.URL {
	u := *up.proxy
	u.RawQuery = url.Values{"url": {target.String()}}.Encode()
	return &u
}

// proxyError 是下载代理报告的自身错误：响应带有含 error 参数的 Proxy-Status。
// 这时响应状态码描述的是代理与调用方之间的问题，不能按目标服务器的状态码映射。
type proxyError struct {
	status  int    // 代理返回的状态码
	errType string // RFC 9209 的错误类型
}

func (e *proxyError) Error() string {
	return fmt.Sprintf("download proxy error %q (proxy status %d)", e.errType, e.status)
}

// clientStatus 是返回给客户端的状态码：超时类错误为 504，其余为 502。
func (e *proxyError) clientStatus() int {
	switch e.errType {
	case "dns_timeout", "connection_timeout", "connection_read_timeout", "http_response_timeout":
		return http.StatusGatewayTimeout
	}
	return http.StatusBadGateway
}

// proxyStatusError 在 Proxy-Status 头（RFC 9209，structured field 列表）中查找带 error 参数的成员，
// 返回第一个 error 的值。只做这个判断所需的宽松解析：跳过引号字符串里的 ","、";"，
// 不校验其余语法，所以格式有瑕疵的头也不会让代理的错误被误当成目标服务器的响应。
func proxyStatusError(h http.Header) (string, bool) {
	values := h.Values("Proxy-Status")
	if len(values) == 0 {
		return "", false
	}
	for _, member := range splitOutsideQuotes(strings.Join(values, ","), ',') {
		params := splitOutsideQuotes(member, ';')
		for _, p := range params[1:] { // params[0] 是代理的名字
			key, value, _ := strings.Cut(p, "=")
			if strings.TrimSpace(key) != "error" {
				continue
			}
			value = strings.TrimSpace(value)
			if uq, err := strconv.Unquote(value); err == nil && strings.HasPrefix(value, `"`) {
				value = uq
			}
			return value, true
		}
	}
	return "", false
}

// splitOutsideQuotes 按 sep 切分 s，忽略双引号字符串内部（含反斜杠转义）的 sep。
func splitOutsideQuotes(s string, sep byte) []string {
	var parts []string
	start, quoted := 0, false
	for i := 0; i < len(s); i++ {
		switch c := s[i]; {
		case quoted && c == '\\':
			i++
		case c == '"':
			quoted = !quoted
		case !quoted && c == sep:
			parts = append(parts, s[start:i])
			start = i + 1
		}
	}
	return append(parts, s[start:])
}
