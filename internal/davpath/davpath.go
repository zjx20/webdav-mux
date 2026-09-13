// Package davpath 在“解码后的路径段”这一层处理 URL 路径。
//
// 挂载匹配、前缀替换和重新编码都基于段（[]string）进行，因此不依赖任何一方
// （客户端或上游）的百分号编码风格，也不会把文件名里的 "%2F" 误当成目录分隔符。
package davpath

import (
	"errors"
	"net/url"
	"strings"
	"unicode/utf8"
)

var (
	// ErrInvalid 表示路径不是以 "/" 开头，或者含有非法的百分号编码。
	ErrInvalid = errors.New("davpath: invalid path")
	// ErrUnsafe 表示路径里有某个段可能被上游重新解释成目录穿越，见 checkComponent。
	ErrUnsafe = errors.New("davpath: unsafe path segment")
)

// maxDecodeDepth 是 checkComponent 追加解码的最大层数。超过这个层数仍能继续解码的段
// 直接视为不安全：正常文件名不会被多重编码。
const maxDecodeDepth = 3

// Parse 把请求里的转义路径（如 r.URL.EscapedPath()）拆成解码后的段。
//
//   - 空段（"//"）被忽略；"." 和 ".." 按 RFC 3986 消解，且不会越过根。
//   - 解码后可能被上游当成穿越的段会让整个路径被拒绝（ErrUnsafe）。
//
// dir 表示路径是否指向“目录形式”（以 "/" 结尾或以点段结尾），调用方用它保持尾部斜杠。
func Parse(escaped string) (segs []string, dir bool, err error) {
	if !strings.HasPrefix(escaped, "/") {
		return nil, false, ErrInvalid
	}
	raw := strings.Split(escaped[1:], "/")
	dir = strings.HasSuffix(escaped, "/")
	for i, r := range raw {
		if r == "" {
			continue
		}
		seg, err := url.PathUnescape(r)
		if err != nil {
			return nil, false, ErrInvalid
		}
		if seg == "." || seg == ".." {
			if seg == ".." && len(segs) > 0 {
				segs = segs[:len(segs)-1]
			}
			if i == len(raw)-1 {
				dir = true
			}
			continue
		}
		if err := checkComponent(seg, 0); err != nil {
			return nil, false, err
		}
		segs = append(segs, seg)
	}
	return segs, dir, nil
}

// SplitLiteral 拆分配置文件里书写的未转义路径（例如 "/我的 电影"）。
// 它要求规范形式：以 "/" 开头，不含空段、"." 或 ".."；允许一个尾部斜杠。
// 段的安全规则与 Parse 相同，所以配置里的挂载路径一定能被客户端请求到。
func SplitLiteral(p string) ([]string, error) {
	if !strings.HasPrefix(p, "/") {
		return nil, ErrInvalid
	}
	p = strings.TrimSuffix(p[1:], "/")
	if p == "" {
		return nil, nil
	}
	var segs []string
	for seg := range strings.SplitSeq(p, "/") {
		if seg == "" || seg == "." || seg == ".." {
			return nil, ErrInvalid
		}
		if err := checkComponent(seg, 0); err != nil {
			return nil, err
		}
		segs = append(segs, seg)
	}
	return segs, nil
}

// checkComponent 拒绝这样的段：它在本服务看来只是一个普通文件名，但被实现有缺陷的上游
// 按下面任一方式重新解释后，会出现 "." / ".." 这样的点段或 NUL：
//   - 把 "/" 或 "\" 当作分隔符（Windows 上游会把 "\" 当分隔符）；
//   - 去掉文件名末尾的点和空格（Windows 的行为），所以只由点和空格组成的名字都视为点段；
//   - 对路径多解码一次或几次；
//   - 接受过长编码的 UTF-8（如 C0 AE 表示 "."），或把全角点、全角斜杠、日文代码页的 ¥ 等
//     字符归一化成 ASCII 的 "."、"/"、"\"（NFKC 归一化或 Windows ANSI 代码页的 best-fit 映射），见 fold。
//
// 本服务转发时会重新编码每个段（"/" 变成 %2F），正确实现的上游不会误解；这里防的是
// 有缺陷的上游。上游凭据的权限通常比挂载目标大，一次穿越就能读到挂载目标之外的数据，
// 所以宁可让这类极少见的文件名无法访问。注意只有重新解释后真的出现点段才会被拒绝，
// 像 "作品／第1話.mkv" 这样含全角斜杠的普通文件名不受影响。
func checkComponent(s string, depth int) error {
	for _, v := range []string{s, fold(s)} {
		if strings.IndexByte(v, 0) >= 0 {
			return ErrUnsafe
		}
		for part := range strings.FieldsFuncSeq(v, isSeparator) {
			if strings.Trim(part, ". ") == "" {
				return ErrUnsafe
			}
			if strings.IndexByte(part, '%') < 0 {
				continue
			}
			dec, err := url.PathUnescape(part)
			if err != nil || dec == part {
				continue
			}
			if depth+1 >= maxDecodeDepth {
				return ErrUnsafe
			}
			if err := checkComponent(dec, depth+1); err != nil {
				return err
			}
		}
	}
	return nil
}

func isSeparator(r rune) bool { return r == '/' || r == '\\' }

// confusables 是可能被有缺陷的上游当成 "."、"/"、"\" 的 Unicode 字符：NFKC 归一化会把
// 全角和小型变体变成对应的 ASCII 字符，Windows 在 ANSI 代码页下的 best-fit 映射会把
// 形似的斜杠变成 "/" 或 "\"（日文代码页 932 和韩文代码页 949 里 0x5C 分别显示为 ¥ 和 ₩）。
var confusables = map[rune]string{
	'．': ".",   // FULLWIDTH FULL STOP
	'﹒': ".",   // SMALL FULL STOP
	'․': ".",   // ONE DOT LEADER
	'‥': "..",  // TWO DOT LEADER
	'…': "...", // HORIZONTAL ELLIPSIS
	'／': "/",   // FULLWIDTH SOLIDUS
	'∕': "/",   // DIVISION SLASH
	'⁄': "/",   // FRACTION SLASH
	'＼': `\`,   // FULLWIDTH REVERSE SOLIDUS
	'﹨': `\`,   // SMALL REVERSE SOLIDUS
	'∖': `\`,   // SET MINUS
	'⧵': `\`,   // REVERSE SOLIDUS OPERATOR
	'¥': `\`,   // YEN SIGN
	'₩': `\`,   // WON SIGN
}

// fold 把 s 中过长编码的 ASCII 字符和 confusables 里的字符替换成对应的 ASCII 字符。
// 其余字节原样保留，包括不是合法 UTF-8 的字节（例如 GBK、Shift_JIS 编码的文件名）。
func fold(s string) string {
	var b strings.Builder
	changed := false
	for i := 0; i < len(s); {
		if c, n := overlongASCII(s[i:]); n > 0 {
			b.WriteByte(c)
			i += n
			changed = true
			continue
		}
		r, n := utf8.DecodeRuneInString(s[i:])
		if rep, ok := confusables[r]; ok {
			b.WriteString(rep)
			changed = true
		} else {
			b.WriteString(s[i : i+n])
		}
		i += n
	}
	if !changed {
		return s
	}
	return b.String()
}

// overlongASCII 识别 s 开头以过长形式编码的 ASCII 字符（2、3、4 字节形式），返回该字符和编码长度；
// 不是时 n 为 0。UTF-8 规范禁止过长编码，但一些老旧的解码器会接受。
func overlongASCII(s string) (c byte, n int) {
	cont := func(i int) bool { return i < len(s) && s[i]&0xC0 == 0x80 }
	switch {
	case len(s) >= 2 && (s[0] == 0xC0 || s[0] == 0xC1) && cont(1):
		return (s[0]&0x1F)<<6 | s[1]&0x3F, 2
	case len(s) >= 3 && s[0] == 0xE0 && (s[1] == 0x80 || s[1] == 0x81) && cont(2):
		return (s[1]&0x3F)<<6 | s[2]&0x3F, 3
	case len(s) >= 4 && s[0] == 0xF0 && s[1] == 0x80 && (s[2] == 0x80 || s[2] == 0x81) && cont(3):
		return (s[2]&0x3F)<<6 | s[3]&0x3F, 4
	}
	return 0, 0
}

// Encode 把段编码成以 "/" 开头的路径；dir 为真或路径为根时以 "/" 结尾。
//
// 除 RFC 3986 的 unreserved 字符（字母、数字、"-._~"）外一律百分号编码，
// 这样 "/"、";"、"%"、"#"、"?"、"+" 和非 ASCII 字符在任何上游或客户端那里都没有歧义，
// 编码结果也是纯 ASCII，可以原样嵌入任何编码的 XML 文档。
func Encode(segs []string, dir bool) string {
	var b strings.Builder
	for _, seg := range segs {
		b.WriteByte('/')
		for i := 0; i < len(seg); i++ {
			c := seg[i]
			if isUnreserved(c) {
				b.WriteByte(c)
			} else {
				b.WriteByte('%')
				b.WriteByte(upperhex[c>>4])
				b.WriteByte(upperhex[c&0xF])
			}
		}
	}
	if dir || len(segs) == 0 {
		b.WriteByte('/')
	}
	return b.String()
}

const upperhex = "0123456789ABCDEF"

func isUnreserved(c byte) bool {
	return 'A' <= c && c <= 'Z' || 'a' <= c && c <= 'z' || '0' <= c && c <= '9' ||
		c == '-' || c == '.' || c == '_' || c == '~'
}

// CutPrefix 在 segs 以 prefix 的各段开头时返回剩余的段。
func CutPrefix(segs, prefix []string) (rest []string, ok bool) {
	if len(segs) < len(prefix) {
		return nil, false
	}
	for i, p := range prefix {
		if segs[i] != p {
			return nil, false
		}
	}
	return segs[len(prefix):], true
}

// ParseHref 从 DAV:href 的值中取出路径并宽松地解码成段。
//
// href 可以是绝对 URL（其 scheme 和主机被忽略：上游可能在反代后面返回内部主机名）、
// 绝对路径，或相对 basePath（上游请求的转义路径）解析的相对引用。
// 编码非法的段按原样当作文件名，因为有些上游不转义文件名里的 "%"。
// 这里不做 Parse 的安全检查：结果只用来计算返回给客户端的 href，
// 客户端再次请求时仍会经过 Parse。
func ParseHref(href, basePath string) (segs []string, dir bool, ok bool) {
	h := strings.TrimSpace(href)
	if i := strings.IndexAny(h, "?#"); i >= 0 {
		h = h[:i]
	}
	switch {
	case strings.HasPrefix(h, "//"):
		h = stripAuthority(h[2:])
	case strings.HasPrefix(h, "/"):
	case hasScheme(h):
		rest := h[strings.IndexByte(h, ':')+1:]
		if !strings.HasPrefix(rest, "//") {
			return nil, false, false
		}
		h = stripAuthority(rest[2:])
	case h == "":
		return nil, false, false
	default:
		h = basePath[:strings.LastIndexByte(basePath, '/')+1] + h
		if !strings.HasPrefix(h, "/") {
			return nil, false, false
		}
	}
	raw := strings.Split(h[1:], "/")
	dir = strings.HasSuffix(h, "/")
	for i, r := range raw {
		if r == "" {
			continue
		}
		seg, err := url.PathUnescape(r)
		if err != nil {
			seg = r
		}
		if seg == "." || seg == ".." {
			if seg == ".." && len(segs) > 0 {
				segs = segs[:len(segs)-1]
			}
			if i == len(raw)-1 {
				dir = true
			}
			continue
		}
		segs = append(segs, seg)
	}
	return segs, dir, true
}

// stripAuthority 去掉 "host[:port]" 部分，返回其后的路径（没有路径时为 "/"）。
func stripAuthority(s string) string {
	if i := strings.IndexByte(s, '/'); i >= 0 {
		return s[i:]
	}
	return "/"
}

// hasScheme 报告 s 是否以 RFC 3986 的 scheme（ALPHA *( ALPHA / DIGIT / "+" / "-" / "." ) ":"）开头。
func hasScheme(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case 'a' <= c && c <= 'z' || 'A' <= c && c <= 'Z':
		case i > 0 && ('0' <= c && c <= '9' || c == '+' || c == '-' || c == '.'):
		case i > 0 && c == ':':
			return true
		default:
			return false
		}
	}
	return false
}
