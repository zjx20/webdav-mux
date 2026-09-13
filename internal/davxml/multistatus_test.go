package davxml

import (
	"bytes"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"

	"webdav-mux/internal/davpath"
)

// testMapper 模拟挂载 /movies -> 上游 /dav 下的 /media/movies。
func testMapper(href string) (string, HrefKind) {
	segs, dir, ok := davpath.ParseHref(href, "/dav/media/movies/")
	if !ok {
		return "", HrefOutside
	}
	rest, ok := davpath.CutPrefix(segs, []string{"dav", "media", "movies"})
	if !ok {
		return "", HrefOutside
	}
	kind := HrefInside
	if len(rest) == 0 {
		kind = HrefMountRoot
	}
	return davpath.Encode(append([]string{"movies"}, rest...), dir), kind
}

func rewrite(t *testing.T, in string, mountName string) (string, RewriteStats, error) {
	t.Helper()
	var out bytes.Buffer
	stats, err := RewriteMultistatus(&out, strings.NewReader(in), RewriteOptions{MapHref: testMapper, MountName: mountName})
	return out.String(), stats, err
}

func TestRewriteServerStyles(t *testing.T) {
	tests := []struct {
		name      string
		in, want  string
		responses int
		dropped   int
	}{
		{
			// Apache mod_dav：属性用 lp1/lp2 前缀，命名空间声明在 response 上；多行缩进。
			name: "apache",
			in: `<?xml version="1.0" encoding="utf-8"?>
<D:multistatus xmlns:D="DAV:" xmlns:ns0="DAV:">
<D:response xmlns:lp1="DAV:" xmlns:lp2="http://apache.org/dav/props/">
<D:href>/dav/media/movies/</D:href>
<D:propstat>
<D:prop>
<lp1:resourcetype><D:collection/></lp1:resourcetype>
<lp1:getlastmodified>Mon, 01 Sep 2025 10:00:00 GMT</lp1:getlastmodified>
<lp1:getetag>"1000-5f0a"</lp1:getetag>
</D:prop>
<D:status>HTTP/1.1 200 OK</D:status>
</D:propstat>
</D:response>
<D:response xmlns:lp1="DAV:" xmlns:lp2="http://apache.org/dav/props/">
<D:href>/dav/media/movies/Movie%20(2020).mkv</D:href>
<D:propstat>
<D:prop>
<lp1:resourcetype/>
<lp1:getcontentlength>1234</lp1:getcontentlength>
<lp2:executable>F</lp2:executable>
</D:prop>
<D:status>HTTP/1.1 200 OK</D:status>
</D:propstat>
</D:response>
</D:multistatus>
`,
			want: `<?xml version="1.0" encoding="utf-8"?>
<D:multistatus xmlns:D="DAV:" xmlns:ns0="DAV:">
<D:response xmlns:lp1="DAV:" xmlns:lp2="http://apache.org/dav/props/">
<D:href>/movies/</D:href>
<D:propstat>
<D:prop>
<lp1:resourcetype><D:collection/></lp1:resourcetype>
<lp1:getlastmodified>Mon, 01 Sep 2025 10:00:00 GMT</lp1:getlastmodified>
<lp1:getetag>"1000-5f0a"</lp1:getetag>
</D:prop>
<D:status>HTTP/1.1 200 OK</D:status>
</D:propstat>
</D:response>
<D:response xmlns:lp1="DAV:" xmlns:lp2="http://apache.org/dav/props/">
<D:href>/movies/Movie%20%282020%29.mkv</D:href>
<D:propstat>
<D:prop>
<lp1:resourcetype/>
<lp1:getcontentlength>1234</lp1:getcontentlength>
<lp2:executable>F</lp2:executable>
</D:prop>
<D:status>HTTP/1.1 200 OK</D:status>
</D:propstat>
</D:response>
</D:multistatus>
`,
			responses: 2,
		},
		{
			// SabreDAV / Nextcloud：紧凑输出、小写十六进制编码、实体转义的 ETag、厂商命名空间属性。
			// 第一个 response 指向挂载目标的上级目录，必须被丢弃。
			name: "sabredav",
			in: `<?xml version="1.0"?>
<d:multistatus xmlns:d="DAV:" xmlns:s="http://sabredav.org/ns" xmlns:oc="http://owncloud.org/ns"><d:response><d:href>/dav/media/</d:href><d:propstat><d:prop><d:resourcetype><d:collection/></d:resourcetype></d:prop><d:status>HTTP/1.1 200 OK</d:status></d:propstat></d:response><d:response><d:href>/dav/media/movies/%e4%b8%ad%e6%96%87/</d:href><d:propstat><d:prop><d:getetag>&quot;68b5f1a0c1d2e&quot;</d:getetag><oc:permissions>RDNVCK</oc:permissions><d:resourcetype><d:collection/></d:resourcetype></d:prop><d:status>HTTP/1.1 200 OK</d:status></d:propstat><d:propstat><d:prop><oc:size/></d:prop><d:status>HTTP/1.1 404 Not Found</d:status></d:propstat></d:response></d:multistatus>`,
			want: `<?xml version="1.0"?>
<d:multistatus xmlns:d="DAV:" xmlns:s="http://sabredav.org/ns" xmlns:oc="http://owncloud.org/ns"><d:response><d:href>/movies/%E4%B8%AD%E6%96%87/</d:href><d:propstat><d:prop><d:getetag>&quot;68b5f1a0c1d2e&quot;</d:getetag><oc:permissions>RDNVCK</oc:permissions><d:resourcetype><d:collection/></d:resourcetype></d:prop><d:status>HTTP/1.1 200 OK</d:status></d:propstat><d:propstat><d:prop><oc:size/></d:prop><d:status>HTTP/1.1 404 Not Found</d:status></d:propstat></d:response></d:multistatus>`,
			responses: 1,
			dropped:   1,
		},
		{
			// IIS：a: 前缀、完整 URL 形式的 href、带 b:dt 类型属性；挂载根的 displayname 被替换。
			name:      "iis",
			in:        `<?xml version="1.0" encoding="utf-8"?><a:multistatus xmlns:b="urn:uuid:c2f41010-65b3-11d1-a29f-00aa00c14882/" xmlns:a="DAV:"><a:response><a:href>http://iis.internal/dav/media/movies/</a:href><a:propstat><a:status>HTTP/1.1 200 OK</a:status><a:prop><a:displayname>Movies &amp; TV</a:displayname><a:getcontentlength b:dt="int">0</a:getcontentlength><a:resourcetype><a:collection/></a:resourcetype></a:prop></a:propstat></a:response><a:response><a:href>http://iis.internal/dav/media/movies/a.mkv</a:href><a:propstat><a:status>HTTP/1.1 200 OK</a:status><a:prop><a:displayname>a.mkv</a:displayname><a:resourcetype/></a:prop></a:propstat></a:response></a:multistatus>`,
			want:      `<?xml version="1.0" encoding="utf-8"?><a:multistatus xmlns:b="urn:uuid:c2f41010-65b3-11d1-a29f-00aa00c14882/" xmlns:a="DAV:"><a:response><a:href>/movies/</a:href><a:propstat><a:status>HTTP/1.1 200 OK</a:status><a:prop><a:displayname>films &lt;&amp;&gt;</a:displayname><a:getcontentlength b:dt="int">0</a:getcontentlength><a:resourcetype><a:collection/></a:resourcetype></a:prop></a:propstat></a:response><a:response><a:href>/movies/a.mkv</a:href><a:propstat><a:status>HTTP/1.1 200 OK</a:status><a:prop><a:displayname>a.mkv</a:displayname><a:resourcetype/></a:prop></a:propstat></a:response></a:multistatus>`,
			responses: 2,
		},
		{
			// 默认命名空间、CDATA、注释、属性值里挂载内的 href（改写）、自闭合的 displayname（保持原样）、
			// 一个 response 带多个顶层 href（挂载外的被删除）。
			name: "default namespace and odd constructs",
			in: `<multistatus xmlns="DAV:"><!-- listing --><response><href><![CDATA[/dav/media/movies/]]></href><propstat><prop><displayname/><lockdiscovery><activelock><lockroot><href>/dav/media/movies/</href></lockroot></activelock></lockdiscovery></prop><status>HTTP/1.1 200 OK</status></propstat></response>
<response><href>/dav/media/movies/x&amp;y.txt</href><href>/elsewhere/z</href><status>HTTP/1.1 424 Failed Dependency</status></response></multistatus>`,
			want: `<multistatus xmlns="DAV:"><!-- listing --><response><href>/movies/</href><propstat><prop><displayname/><lockdiscovery><activelock><lockroot><href>/movies/</href></lockroot></activelock></lockdiscovery></prop><status>HTTP/1.1 200 OK</status></propstat></response>
<response><href>/movies/x%26y.txt</href><status>HTTP/1.1 424 Failed Dependency</status></response></multistatus>`,
			responses: 2,
		},
		{
			// 引用挂载目标之外资源的 href 不能出现在输出里，按所在位置删除最小的可选结构。
			name: "hrefs outside the mount inside other elements",
			in: `<D:multistatus xmlns:D="DAV:" xmlns:X="urn:x">` +
				`<D:response><D:href>/dav/media/movies/a</D:href>` +
				`<D:propstat><D:prop>` +
				`<D:getetag>"1"</D:getetag>` +
				`<D:current-user-principal><D:href>/dav/principals/users/admin/</D:href></D:current-user-principal>` +
				`<D:lockdiscovery><D:activelock><D:lockroot><D:href>/dav/media/movies/a</D:href></D:lockroot><D:owner><D:href>mailto:admin@nas.lan</D:href></D:owner></D:activelock></D:lockdiscovery>` +
				`<D:resource-id><D:href>urn:uuid:0e9a5a4e-2a3c-4b1e-9f7a-1d2c3b4a5e6f</D:href></D:resource-id>` +
				`<D:href>/dav/other</D:href>` +
				`<X:links><X:self><D:href>/dav/media/movies/a</D:href></X:self></X:links>` +
				`</D:prop><D:status>HTTP/1.1 200 OK</D:status>` +
				`<D:error><D:need-privileges><D:href>/dav/admin-only</D:href></D:need-privileges></D:error>` +
				`</D:propstat>` +
				`<D:location><D:href>http://nas.lan/elsewhere/</D:href></D:location>` +
				`<D:responsedescription>kept</D:responsedescription>` +
				`</D:response>` +
				`<X:extension><D:href>/dav/secret/</D:href></X:extension>` +
				`<X:extension><D:href>/dav/media/movies/b</D:href></X:extension>` +
				`<D:responsedescription>done</D:responsedescription>` +
				`</D:multistatus>`,
			want: `<D:multistatus xmlns:D="DAV:" xmlns:X="urn:x">` +
				`<D:response><D:href>/movies/a</D:href>` +
				`<D:propstat><D:prop>` +
				`<D:getetag>"1"</D:getetag>` +
				`<X:links><X:self><D:href>/movies/a</D:href></X:self></X:links>` +
				`</D:prop><D:status>HTTP/1.1 200 OK</D:status>` +
				`</D:propstat>` +
				`<D:responsedescription>kept</D:responsedescription>` +
				`</D:response>` +
				`<X:extension><D:href>/movies/b</D:href></X:extension>` +
				`<D:responsedescription>done</D:responsedescription>` +
				`</D:multistatus>`,
			responses: 1,
		},
		{
			// 非 DAV: 命名空间里叫 href / displayname 的元素不能被当成 DAV 元素。
			name:      "foreign elements with DAV local names",
			in:        `<D:multistatus xmlns:D="DAV:" xmlns:X="urn:x"><D:response><D:href>/dav/media/movies/</D:href><X:href>/dav/media/movies/</X:href><D:propstat><D:prop><X:displayname>keep</X:displayname><D:displayname>movies-upstream</D:displayname></D:prop></D:propstat></D:response></D:multistatus>`,
			want:      `<D:multistatus xmlns:D="DAV:" xmlns:X="urn:x"><D:response><D:href>/movies/</D:href><X:href>/dav/media/movies/</X:href><D:propstat><D:prop><X:displayname>keep</X:displayname><D:displayname>films &lt;&amp;&gt;</D:displayname></D:prop></D:propstat></D:response></D:multistatus>`,
			responses: 1,
		},
		{
			// BOM 被去掉；"utf8" 这种非规范写法的编码声明也接受。
			name:      "bom and utf8 label",
			in:        "\xEF\xBB\xBF" + `<?xml version="1.0" encoding="utf8"?><D:multistatus xmlns:D="DAV:"><D:response><D:href>/dav/media/movies/b</D:href></D:response></D:multistatus>`,
			want:      `<?xml version="1.0" encoding="utf8"?><D:multistatus xmlns:D="DAV:"><D:response><D:href>/movies/b</D:href></D:response></D:multistatus>`,
			responses: 1,
		},
		{
			name:    "all responses outside the mount",
			in:      `<D:multistatus xmlns:D="DAV:"><D:response><D:href>/other/</D:href></D:response><D:response><D:href/></D:response></D:multistatus>`,
			want:    `<D:multistatus xmlns:D="DAV:"></D:multistatus>`,
			dropped: 2,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, stats, err := rewrite(t, tt.in, "films <&>")
			if err != nil {
				t.Fatalf("error: %v", err)
			}
			if got != tt.want {
				t.Errorf("output mismatch:\n got: %s\nwant: %s", got, tt.want)
			}
			if stats.Responses != tt.responses || stats.Dropped != tt.dropped {
				t.Errorf("stats = %+v; want %d responses, %d dropped", stats, tt.responses, tt.dropped)
			}
		})
	}
}

func TestRewriteFirstDropped(t *testing.T) {
	_, stats, err := rewrite(t, `<D:multistatus xmlns:D="DAV:"><D:response><D:href>/wrong/prefix/a</D:href></D:response><D:response><D:href>/wrong/prefix/b</D:href></D:response></D:multistatus>`, "")
	if err != nil {
		t.Fatal(err)
	}
	if stats.Dropped != 2 || stats.FirstDropped != "/wrong/prefix/a" {
		t.Errorf("stats = %+v", stats)
	}
}

// 挂载在根上时 MountName 为空：挂载根的 displayname 被清空，与合成的根目录一致，也不暴露上游目录名。
func TestRewriteRootMountDisplayname(t *testing.T) {
	in := `<D:multistatus xmlns:D="DAV:"><D:response><D:href>/dav/media/movies/</D:href><D:propstat><D:prop><D:displayname>Upstream</D:displayname></D:prop><D:status>HTTP/1.1 200 OK</D:status></D:propstat></D:response></D:multistatus>`
	got, _, err := rewrite(t, in, "")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "<D:displayname></D:displayname>") {
		t.Errorf("root mount displayname not cleared: %s", got)
	}
}

// 只替换状态为 200 的 propstat 里的 displayname（包括 status 写在 prop 之前的 IIS 风格），
// 404 propstat 里列出的 displayname 保持原样。
func TestRewriteDisplaynameOnlyInOKPropstats(t *testing.T) {
	in := `<D:multistatus xmlns:D="DAV:"><D:response><D:href>/dav/media/movies/</D:href>` +
		`<D:propstat><D:prop><D:displayname></D:displayname></D:prop><D:status>HTTP/1.1 404 Not Found</D:status></D:propstat>` +
		`<D:propstat><D:status>HTTP/1.1 200 OK</D:status><D:prop><D:displayname>a</D:displayname></D:prop></D:propstat>` +
		`<D:propstat><D:prop><D:displayname>b</D:displayname></D:prop><D:status>HTTP/1.1 200 OK</D:status></D:propstat>` +
		`</D:response></D:multistatus>`
	want := `<D:multistatus xmlns:D="DAV:"><D:response><D:href>/movies/</D:href>` +
		`<D:propstat><D:prop><D:displayname></D:displayname></D:prop><D:status>HTTP/1.1 404 Not Found</D:status></D:propstat>` +
		`<D:propstat><D:status>HTTP/1.1 200 OK</D:status><D:prop><D:displayname>films</D:displayname></D:prop></D:propstat>` +
		`<D:propstat><D:prop><D:displayname>films</D:displayname></D:prop><D:status>HTTP/1.1 200 OK</D:status></D:propstat>` +
		`</D:response></D:multistatus>`
	got, _, err := rewrite(t, in, "films")
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Errorf("got:  %s\nwant: %s", got, want)
	}
}

// 非挂载根条目的 displayname 不能被替换。
func TestRewriteDisplaynameOnlyOnMountRoot(t *testing.T) {
	in := `<D:multistatus xmlns:D="DAV:"><D:response><D:href>/dav/media/movies/sub/</D:href><D:propstat><D:prop><D:displayname>sub</D:displayname></D:prop></D:propstat></D:response></D:multistatus>`
	got, _, err := rewrite(t, in, "films")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "<D:displayname>sub</D:displayname>") {
		t.Errorf("displayname of a non-root entry was changed: %s", got)
	}
}

func TestRewriteErrors(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want error // nil 表示只要求返回错误
	}{
		{"html page", `<!DOCTYPE html><html><body>login</body></html>`, ErrNotMultistatus},
		{"wrong namespace", `<multistatus><response/></multistatus>`, ErrNotMultistatus},
		{"empty", ``, ErrNotMultistatus},
		{"truncated", `<D:multistatus xmlns:D="DAV:"><D:response><D:href>/dav/media/movies/</D:href>`, nil},
		{"trailing error text", `<D:multistatus xmlns:D="DAV:"></D:multistatus>Internal Server Error`, nil},
		{"second root", `<D:multistatus xmlns:D="DAV:"></D:multistatus><D:multistatus xmlns:D="DAV:"></D:multistatus>`, ErrNotMultistatus},
		{"latin1", `<?xml version="1.0" encoding="ISO-8859-1"?><D:multistatus xmlns:D="DAV:"></D:multistatus>`, nil},
		{"undeclared entity", `<D:multistatus xmlns:D="DAV:"><D:response><D:href>/dav/media/movies/&nbsp;</D:href></D:response></D:multistatus>`, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, _, err := rewrite(t, tt.in, "")
			if err == nil || (tt.want != nil && !errors.Is(err, tt.want)) {
				t.Errorf("error = %v; want %v", err, tt.want)
			}
		})
	}
}

// sizeLimitReader 在读出 limit 字节后报错，用来确认改写器没有在读完整个巨大 token 之后才检查大小。
type sizeLimitReader struct {
	r     io.Reader
	n     int
	limit int
}

func (s *sizeLimitReader) Read(p []byte) (int, error) {
	if s.n >= s.limit {
		return 0, fmt.Errorf("read past %d bytes", s.limit)
	}
	n, err := s.r.Read(p[:min(len(p), s.limit-s.n)])
	s.n += n
	return n, err
}

func TestRewriteSizeLimit(t *testing.T) {
	big := strings.Repeat("x", 4*maxBufferedBytes)
	for name, in := range map[string]string{
		"large response element": `<D:multistatus xmlns:D="DAV:"><D:response><D:href>/dav/media/movies/a</D:href><D:propstat><D:prop><D:comment>` + big + `</D:comment></D:prop></D:propstat></D:response></D:multistatus>`,
		"large token outside responses": `<D:multistatus xmlns:D="DAV:"><D:responsedescription>` + big + `</D:responsedescription></D:multistatus>`,
	} {
		t.Run(name, func(t *testing.T) {
			src := &sizeLimitReader{r: strings.NewReader(in), limit: 2 * maxBufferedBytes}
			_, err := RewriteMultistatus(io.Discard, src, RewriteOptions{MapHref: testMapper})
			if !errors.Is(err, errTooLarge) {
				t.Errorf("error = %v; want errTooLarge before reading the whole token", err)
			}
		})
	}
}

// FuzzRewriteMultistatus 检查改写器对任意输入都满足：不 panic；成功时输出是格式良好的 XML；
// 并且输出中每个 DAV:href 都指向挂载内（/movies），即挂载目标之外的路径不会泄漏给客户端。
func FuzzRewriteMultistatus(f *testing.F) {
	for _, seed := range []string{
		`<D:multistatus xmlns:D="DAV:"><D:response><D:href>/dav/media/movies/</D:href><D:propstat><D:prop><D:displayname>x</D:displayname><D:current-user-principal><D:href>/dav/p/</D:href></D:current-user-principal></D:prop><D:status>HTTP/1.1 200 OK</D:status></D:propstat></D:response></D:multistatus>`,
		`<multistatus xmlns="DAV:"><response><href>/dav/media/movies/a</href><href>/x</href><status>HTTP/1.1 424 Failed Dependency</status><location><href>/y</href></location></response><z xmlns="urn:z"><href xmlns="DAV:">/q</href></z></multistatus>`,
		`<?xml version="1.0"?><a:multistatus xmlns:a="DAV:"><a:response><a:href><![CDATA[/dav/media/movies/b]]></a:href><a:propstat><a:status>HTTP/1.1 404 Not Found</a:status><a:prop><a:displayname/></a:prop><a:error><a:x><a:href>/e</a:href></a:x></a:error></a:propstat></a:response></a:multistatus>`,
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, in string) {
		var out bytes.Buffer
		_, err := RewriteMultistatus(&out, strings.NewReader(in), RewriteOptions{MapHref: testMapper, MountName: "m&<n>"})
		if err != nil {
			return
		}
		dec := xml.NewDecoder(bytes.NewReader(out.Bytes()))
		dec.CharsetReader = charsetReader
		var hrefDepth, depth int
		var text strings.Builder
		for {
			tok, err := dec.Token()
			if err == io.EOF {
				break
			}
			if err != nil {
				t.Fatalf("output is not well-formed XML: %v\ninput:  %q\noutput: %q", err, in, out.String())
			}
			switch tok := tok.(type) {
			case xml.StartElement:
				depth++
				if tok.Name == nameHref && hrefDepth == 0 {
					hrefDepth = depth
					text.Reset()
				}
			case xml.CharData:
				if hrefDepth > 0 {
					text.Write(tok)
				}
			case xml.EndElement:
				if depth == hrefDepth {
					hrefDepth = 0
					if h := strings.TrimSpace(text.String()); h != "/movies" && !strings.HasPrefix(h, "/movies/") {
						t.Fatalf("href outside the mount leaked: %q\ninput:  %q\noutput: %q", h, in, out.String())
					}
				}
				depth--
			}
		}
	})
}

// chunkRecorder 记录每次 Write，用来确认输出是逐个 response 流式写出的。
type chunkRecorder struct {
	bytes.Buffer
	writes int
}

func (c *chunkRecorder) Write(p []byte) (int, error) {
	c.writes++
	return c.Buffer.Write(p)
}

func TestRewriteLargeListingStreams(t *testing.T) {
	const n = 20000
	var in, want strings.Builder
	in.WriteString(`<?xml version="1.0"?><D:multistatus xmlns:D="DAV:">`)
	want.WriteString(`<?xml version="1.0"?><D:multistatus xmlns:D="DAV:">`)
	for i := range n {
		fmt.Fprintf(&in, "\n<D:response><D:href>/dav/media/movies/file%%20%d.mkv</D:href><D:propstat><D:prop><D:getcontentlength>%d</D:getcontentlength></D:prop><D:status>HTTP/1.1 200 OK</D:status></D:propstat></D:response>", i, i)
		fmt.Fprintf(&want, "\n<D:response><D:href>/movies/file%%20%d.mkv</D:href><D:propstat><D:prop><D:getcontentlength>%d</D:getcontentlength></D:prop><D:status>HTTP/1.1 200 OK</D:status></D:propstat></D:response>", i, i)
	}
	in.WriteString("\n</D:multistatus>\n")
	want.WriteString("\n</D:multistatus>\n")

	var out chunkRecorder
	stats, err := RewriteMultistatus(&out, io.MultiReader(strings.NewReader(in.String())), RewriteOptions{MapHref: testMapper})
	if err != nil {
		t.Fatal(err)
	}
	if out.String() != want.String() {
		t.Fatal("large listing output mismatch")
	}
	if stats.Responses != n || out.writes < n {
		t.Errorf("stats = %+v, writes = %d; want %d responses written one by one", stats, out.writes, n)
	}
}
