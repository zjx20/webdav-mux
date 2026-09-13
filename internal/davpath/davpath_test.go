package davpath

import (
	"errors"
	"slices"
	"testing"
)

func TestParse(t *testing.T) {
	tests := []struct {
		in   string
		segs []string
		dir  bool
	}{
		{"/", nil, true},
		{"/a", []string{"a"}, false},
		{"/a/b/", []string{"a", "b"}, true},
		{"//a///b", []string{"a", "b"}, false},
		{"/a/./b", []string{"a", "b"}, false},
		{"/a/b/..", []string{"a"}, true},
		{"/a/b/../c", []string{"a", "c"}, false},
		{"/../../a", []string{"a"}, false},
		{"/%2e%2E/a", []string{"a"}, false},
		{"/a/%2e", []string{"a"}, true},
		{"/%E4%B8%AD%E6%96%87/a%20b", []string{"中文", "a b"}, false},
		{"/a+b/c;d", []string{"a+b", "c;d"}, false},
		// 编码的分隔符留在段内；只要拆开后没有点段就是安全的。
		{"/a%2Fb", []string{"a/b"}, false},
		{"/a%5Cb", []string{`a\b`}, false},
		{"/100%25", []string{"100%"}, false},
		{"/..a/a..", []string{"..a", "a.."}, false},
		// 含全角斜杠、¥、省略号等字符的普通文件名：折叠后不构成点段，照常允许。
		{"/%E4%BD%9C%E5%93%81%EF%BC%8F%E7%AC%AC1%E8%A9%B1.mkv", []string{"作品／第1話.mkv"}, false},
		{"/%C2%A5100.txt", []string{"¥100.txt"}, false},
		{"/Movie%E2%80%A6%20(2020)", []string{"Movie… (2020)"}, false},
		{"/a%EF%BC%8Eb", []string{"a．b"}, false},
		// 非 UTF-8 的旧编码文件名（GBK 的“你好”）原样保留。
		{"/%C4%E3%BA%C3", []string{"\xC4\xE3\xBA\xC3"}, false},
	}
	for _, tt := range tests {
		segs, dir, err := Parse(tt.in)
		if err != nil {
			t.Errorf("Parse(%q) error: %v", tt.in, err)
			continue
		}
		if !slices.Equal(segs, tt.segs) || dir != tt.dir {
			t.Errorf("Parse(%q) = %q, %v; want %q, %v", tt.in, segs, dir, tt.segs, tt.dir)
		}
	}
}

func TestParseRejects(t *testing.T) {
	tests := []struct {
		in   string
		want error
	}{
		{"", ErrInvalid},
		{"a/b", ErrInvalid},
		{"/a%zz", ErrInvalid},
		{"/a%00b", ErrUnsafe},
		// 上游把 %2F / %5C 解码成分隔符时会变成穿越。
		{"/m/x%2F..%2F..%2Fsecret", ErrUnsafe},
		{"/m/..%5C..%5Csecret", ErrUnsafe},
		{"/m/%2E%2E%2Fsecret", ErrUnsafe},
		// Windows 去掉末尾的点和空格后会变成 ".."。
		{"/m/...", ErrUnsafe},
		{"/m/..%20", ErrUnsafe},
		{"/m/%20", ErrUnsafe},
		// 多重编码的点段。
		{"/m/%252e%252e", ErrUnsafe},
		{"/m/a%252F%252E%252E", ErrUnsafe},
		{"/m/%25252e%25252e", ErrUnsafe},
		// 过长编码的 UTF-8："."=C0 AE / E0 80 AE / F0 80 80 AE，"/"=C0 AF，"\"=C1 9C，NUL=C0 80。
		{"/m/..%C0%AFsecret", ErrUnsafe},
		{"/m/%C0%AE%C0%AE", ErrUnsafe},
		{"/m/%E0%80%AE%E0%80%AE", ErrUnsafe},
		{"/m/%F0%80%80%AE%F0%80%80%AE", ErrUnsafe},
		{"/m/..%C1%9Csecret", ErrUnsafe},
		{"/m/a%C0%80b", ErrUnsafe},
		// 过长编码再叠加一层百分号编码。
		{"/m/%25C0%25AE%25C0%25AE", ErrUnsafe},
		// 会被 NFKC 或 best-fit 映射成点、斜杠的字符。
		{"/m/%EF%BC%8E%EF%BC%8E", ErrUnsafe},      // ．．
		{"/m/..%EF%BC%8Fsecret", ErrUnsafe},       // ..／secret
		{"/m/..%EF%BC%BCsecret", ErrUnsafe},       // ..＼secret
		{"/m/..%C2%A5secret", ErrUnsafe},          // ..¥secret
		{"/m/%E2%80%A5", ErrUnsafe},               // ‥
		{"/m/%E2%80%A6", ErrUnsafe},               // …
		{"/m/x%E2%88%95..%E2%88%95y", ErrUnsafe}, // x∕..∕y
	}
	for _, tt := range tests {
		if _, _, err := Parse(tt.in); !errors.Is(err, tt.want) {
			t.Errorf("Parse(%q) error = %v; want %v", tt.in, err, tt.want)
		}
	}
}

func TestSplitLiteral(t *testing.T) {
	good := map[string][]string{
		"/":             nil,
		"/movies":       {"movies"},
		"/movies/":      {"movies"},
		"/我的 电影/2024":   {"我的 电影", "2024"},
		"/100%/a%2Fb":   {"100%", "a%2Fb"},
		"/work/docs(1)": {"work", "docs(1)"},
	}
	for in, want := range good {
		got, err := SplitLiteral(in)
		if err != nil || !slices.Equal(got, want) {
			t.Errorf("SplitLiteral(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, in := range []string{"", "movies", "//a", "/a//b", "/a/./b", "/a/../b", "/..", "/a/...", `/a\..\b`} {
		if _, err := SplitLiteral(in); err == nil {
			t.Errorf("SplitLiteral(%q) succeeded; want error", in)
		}
	}
}

func TestEncode(t *testing.T) {
	tests := []struct {
		segs []string
		dir  bool
		want string
	}{
		{nil, false, "/"},
		{nil, true, "/"},
		{[]string{"a"}, false, "/a"},
		{[]string{"a", "b"}, true, "/a/b/"},
		{[]string{"a b", "中"}, false, "/a%20b/%E4%B8%AD"},
		{[]string{"a/b", "c;d", "100%", "x#?+&=()"}, false, "/a%2Fb/c%3Bd/100%25/x%23%3F%2B%26%3D%28%29"},
		{[]string{"A-Z_a.z~0"}, false, "/A-Z_a.z~0"},
	}
	for _, tt := range tests {
		if got := Encode(tt.segs, tt.dir); got != tt.want {
			t.Errorf("Encode(%q, %v) = %q; want %q", tt.segs, tt.dir, got, tt.want)
		}
	}
}

// Encode 的输出必须能被 Parse 无损还原，否则客户端拿返回的 href 再请求时会找错文件。
func TestEncodeParseRoundTrip(t *testing.T) {
	names := []string{"a b", "中文", "a/b", `a\b`, "100%", "%41", "c;d", "x#?+&=", "日本語のファイル.mkv", "..a", "a..", "~"}
	for _, n := range names {
		segs := []string{"dir", n}
		got, dir, err := Parse(Encode(segs, true))
		if err != nil || !dir || !slices.Equal(got, segs) {
			t.Errorf("round trip of %q: got %q, %v, %v", n, got, dir, err)
		}
	}
}

func TestCutPrefix(t *testing.T) {
	rest, ok := CutPrefix([]string{"a", "b", "c"}, []string{"a", "b"})
	if !ok || !slices.Equal(rest, []string{"c"}) {
		t.Errorf("got %q, %v", rest, ok)
	}
	if rest, ok := CutPrefix([]string{"a"}, nil); !ok || !slices.Equal(rest, []string{"a"}) {
		t.Errorf("empty prefix: got %q, %v", rest, ok)
	}
	if _, ok := CutPrefix([]string{"ab"}, []string{"a"}); ok {
		t.Error("segment-wise prefix must not match partial segments")
	}
	if _, ok := CutPrefix([]string{"a"}, []string{"a", "b"}); ok {
		t.Error("longer prefix must not match")
	}
}

func TestParseHref(t *testing.T) {
	const base = "/dav/media/movies/"
	tests := []struct {
		href string
		segs []string
		dir  bool
		ok   bool
	}{
		{"/dav/media/movies/", []string{"dav", "media", "movies"}, true, true},
		{"  /dav/a%20b.mkv\n", []string{"dav", "a b.mkv"}, false, true},
		{"http://internal:8080/dav/x/", []string{"dav", "x"}, true, true},
		{"https://h", nil, true, true},
		{"//host/dav/x", []string{"dav", "x"}, false, true},
		{"/dav/%e4%b8%ad", []string{"dav", "中"}, false, true},
		{"/dav/100%.txt", []string{"dav", "100%.txt"}, false, true},
		{"/dav/a%2Fb", []string{"dav", "a/b"}, false, true},
		{"/dav/x?query#frag", []string{"dav", "x"}, false, true},
		{"/dav/a/../b", []string{"dav", "b"}, false, true},
		{"child.mkv", []string{"dav", "media", "movies", "child.mkv"}, false, true},
		{"sub/", []string{"dav", "media", "movies", "sub"}, true, true},
		{"", nil, false, false},
		{"mailto:x@example.com", nil, false, false},
	}
	for _, tt := range tests {
		segs, dir, ok := ParseHref(tt.href, base)
		if ok != tt.ok || !slices.Equal(segs, tt.segs) || (ok && dir != tt.dir) {
			t.Errorf("ParseHref(%q) = %q, %v, %v; want %q, %v, %v", tt.href, segs, dir, ok, tt.segs, tt.dir, tt.ok)
		}
	}
}
