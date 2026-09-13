// Package davxml 处理 WebDAV 的 XML 报文：流式改写上游返回的 multistatus、
// 解析 PROPFIND 请求体、为本服务合成的目录生成 multistatus。
package davxml

import (
	"bufio"
	"bytes"
	"cmp"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
)

// HrefKind 描述一个上游 href 映射到客户端命名空间的结果。
type HrefKind int

const (
	// HrefOutside 表示 href 不在挂载目标之内，不能暴露给客户端。
	HrefOutside HrefKind = iota
	// HrefInside 表示 href 位于挂载目标之内。
	HrefInside
	// HrefMountRoot 表示 href 恰好是挂载目标本身。
	HrefMountRoot
)

// HrefMapper 把上游 href 的文本映射为客户端 href。
// 返回的 href 必须已经百分号编码；它会被 XML 转义后写入文档。
type HrefMapper func(href string) (string, HrefKind)

// RewriteOptions 控制 RewriteMultistatus 的改写行为。
type RewriteOptions struct {
	MapHref HrefMapper
	// MountName 替换挂载根那条 response 中 DAV:displayname 的值（可以为空，对应挂载在根上的情况），
	// 使客户端看到的名字与目录树一致，也不暴露上游目录的真实名字。
	MountName string
}

// RewriteStats 汇总一次改写的结果。
type RewriteStats struct {
	Responses int // 写出的 response 数
	Dropped   int // 因顶层 href 不在挂载目标内而丢弃的 response 数
	// FirstDropped 是第一个被丢弃的 response 的 href，便于排查上游 URL 前缀配置错误。
	FirstDropped string
}

// ErrNotMultistatus 表示文档的根元素不是 DAV:multistatus。
var ErrNotMultistatus = errors.New("davxml: document root is not DAV:multistatus")

// maxBufferedBytes 限制尚未写出的原始字节数，也就是根元素下单个子元素（比如一个 DAV:response，
// 它必须完整缓冲，读完才能决定保留、删减还是丢弃）或其之外单个 XML token 的大小。
// 限制在读取层执行，异常上游在耗尽内存之前就会被拒绝，而不是等一个巨大的 token 读完才检查。
const maxBufferedBytes = 8 << 20

var errTooLarge = fmt.Errorf("davxml: DAV:response element or XML token exceeds %d bytes", maxBufferedBytes)

var (
	nameMultistatus = xml.Name{Space: "DAV:", Local: "multistatus"}
	nameResponse    = xml.Name{Space: "DAV:", Local: "response"}
	nameHref        = xml.Name{Space: "DAV:", Local: "href"}
	namePropstat    = xml.Name{Space: "DAV:", Local: "propstat"}
	nameProp        = xml.Name{Space: "DAV:", Local: "prop"}
	nameStatus      = xml.Name{Space: "DAV:", Local: "status"}
	nameDisplayname = xml.Name{Space: "DAV:", Local: "displayname"}
)

// 元素在文档中的深度（根元素为 1）：
//
//	multistatus(1) / response(2) / href(3)
//	multistatus(1) / response(2) / propstat(3) / status(4)
//	multistatus(1) / response(2) / propstat(3) / prop(4) / 某个属性(5)
const (
	depthUnit     = 2
	depthTopHref  = 3
	depthPropstat = 3
	depthStatus   = 4
	depthProperty = 5
)

// RewriteMultistatus 把上游的 multistatus 文档从 src 流式复制到 dst，
// 只改写 DAV:href 的文本（以及挂载根的 displayname），其余字节原样保留。
//
// 之所以在原始字节上拼接、而不是解析后重新序列化：上游的属性（ETag、各厂商的
// 自定义命名空间属性等）需要无损透传，而 encoding/xml 不能保真地重新输出命名空间声明。
//
// 映射到挂载目标之外的 href 不会出现在输出里，按所在位置删除包含它的最小的可选结构：
//   - response 的顶层 href：删除该 href；顶层 href 全部被删除的 response 整个丢弃；
//   - 属性值里的 href（如 DAV:current-user-principal、DAV:lockdiscovery 里的 owner）：
//     删除整个属性，客户端看到的效果是上游没有报告这个属性；
//   - response 或 propstat 下其他元素里的 href（如 DAV:location、DAV:error）：删除该元素；
//   - 根元素下 response 之外的元素里的 href：删除该元素。
//
// 输出以根元素的子元素为单位流式写出。返回错误时 dst 可能已经收到部分内容，
// 调用方应根据是否已向客户端写出数据决定返回错误状态还是中断连接。
func RewriteMultistatus(dst io.Writer, src io.Reader, opts RewriteOptions) (RewriteStats, error) {
	br := bufio.NewReaderSize(src, 32<<10)
	// encoding/xml 会把 BOM 当成根元素之外的文本；UTF-8 BOM 没有意义，直接丢掉。
	if b, err := br.Peek(3); err == nil && string(b) == "\xEF\xBB\xBF" {
		br.Discard(3)
	}
	rw := &rewriter{dst: dst, in: &captureReader{r: br}, opts: opts}
	dec := xml.NewDecoder(rw.in)
	dec.CharsetReader = charsetReader
	err := rw.run(dec)
	return rw.stats, err
}

type rewriter struct {
	dst   io.Writer
	in    *captureReader
	opts  RewriteOptions
	stats RewriteStats

	stack      []openElement
	rootClosed bool
	unit       *unitState // 正在缓冲的根元素子元素；不在其中时为 nil
}

// openElement 是一个尚未结束的元素。
type openElement struct {
	name   xml.Name
	start  int64 // 起始标签 "<" 的偏移
	remove bool  // 结束时整体删除
	// 进入元素时 unit.edits 和 unit.displayNames 的长度。元素整体删除时，
	// 在它内部记录的修改一并作废。
	editMark, nameMark int
}

// unitState 是根元素的一个子元素（通常是 DAV:response）的改写状态。
type unitState struct {
	isResponse bool
	start      int64
	edits      []edit
	topMapped  bool // 至少一个顶层 href 在挂载目标内
	mountRoot  bool // 顶层 href 指向挂载根
	firstHref  string

	href *hrefState // 正在读取的 href

	// displayNames 是 propstat/prop/displayname 的内容区间；ok 在所属 propstat 的状态读完后确定。
	displayNames  []*span
	openName      *span
	propstatNames int              // 当前 propstat 的第一个 displayname 在 displayNames 中的下标
	statusText    *strings.Builder // 正在读取的 propstat/status
	propstatOK    bool
}

type hrefState struct {
	contentStart int64 // 起始标签之后的偏移
	depth        int
	text         strings.Builder
}

type span struct {
	from, to  int64
	selfClose bool
	ok        bool // 所属 propstat 的状态是 200
}

// edit 表示把原始字节 [from, to) 替换为 repl。
type edit struct {
	from, to int64
	repl     string
}

func (rw *rewriter) run(dec *xml.Decoder) error {
	var prev int64 // 当前 token 的起始偏移 = 上一个 token 的结束偏移
	for {
		tok, err := dec.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		off := dec.InputOffset()
		if err := rw.token(tok, prev, off); err != nil {
			return err
		}
		prev = off
	}
	if !rw.rootClosed {
		if len(rw.stack) == 0 {
			return ErrNotMultistatus
		}
		return io.ErrUnexpectedEOF
	}
	return rw.flush(prev)
}

// token 处理原始字节区间 [start, end) 上的一个 token。
func (rw *rewriter) token(tok xml.Token, start, end int64) error {
	switch t := tok.(type) {
	case xml.StartElement:
		if err := rw.startElement(t, start, end); err != nil {
			return err
		}
	case xml.EndElement:
		if err := rw.endElement(start, end); err != nil {
			return err
		}
	case xml.CharData:
		if u := rw.unit; u != nil {
			if u.href != nil {
				u.href.text.Write(t)
			}
			if u.statusText != nil {
				u.statusText.Write(t)
			}
		} else if len(rw.stack) == 0 && len(bytes.TrimSpace(t)) > 0 {
			// 根元素之外出现文本，通常是上游在输出到一半时出错、又追加了错误信息。
			return fmt.Errorf("davxml: unexpected text outside the root element: %.40q", t)
		}
	}
	if rw.unit == nil {
		return rw.flush(end)
	}
	return nil
}

func (rw *rewriter) startElement(t xml.StartElement, start, end int64) error {
	rw.stack = append(rw.stack, openElement{name: t.Name, start: start})
	depth := len(rw.stack)
	switch depth {
	case 1:
		if rw.rootClosed || t.Name != nameMultistatus {
			return ErrNotMultistatus
		}
		return nil
	case depthUnit:
		if err := rw.flush(start); err != nil {
			return err
		}
		rw.unit = &unitState{isResponse: t.Name == nameResponse, start: start}
	}
	u := rw.unit
	e := &rw.stack[depth-1]
	e.editMark, e.nameMark = len(u.edits), len(u.displayNames)

	switch {
	case t.Name == nameHref && u.href == nil:
		u.href = &hrefState{contentStart: end, depth: depth}
	case !u.isResponse:
	case depth == depthPropstat && t.Name == namePropstat:
		u.propstatNames = len(u.displayNames)
		// 缺少 status 的 propstat 不合规范，按 200 处理：宁可替换 displayname，也不泄漏上游目录名。
		u.propstatOK = true
	case depth == depthStatus && t.Name == nameStatus && rw.stack[depthPropstat-1].name == namePropstat:
		u.statusText = &strings.Builder{}
	case depth == depthProperty && t.Name == nameDisplayname && rw.inPropstatProp():
		u.openName = &span{from: end, selfClose: bytes.HasSuffix(rw.in.bytes(start, end), []byte("/>"))}
	}
	return nil
}

func (rw *rewriter) endElement(start, end int64) error {
	depth := len(rw.stack)
	defer func() { rw.stack = rw.stack[:depth-1] }()
	if depth == 1 {
		rw.rootClosed = true
		return nil
	}
	u := rw.unit
	switch {
	case u.href != nil && depth == u.href.depth:
		rw.endHref(start)
	case u.statusText != nil && depth == depthStatus:
		fields := strings.Fields(u.statusText.String())
		u.propstatOK = len(fields) >= 2 && fields[1] == "200"
		u.statusText = nil
	case u.openName != nil && depth == depthProperty:
		u.openName.to = start
		u.displayNames = append(u.displayNames, u.openName)
		u.openName = nil
	case u.isResponse && depth == depthPropstat && rw.stack[depth-1].name == namePropstat:
		for _, s := range u.displayNames[min(u.propstatNames, len(u.displayNames)):] {
			s.ok = u.propstatOK
		}
	}

	e := &rw.stack[depth-1]
	if depth == depthUnit {
		return rw.endUnit(end, e.remove)
	}
	if e.remove {
		u.edits = append(u.edits[:e.editMark], edit{from: e.start, to: end})
		u.displayNames = u.displayNames[:min(e.nameMark, len(u.displayNames))]
	}
	return nil
}

// inPropstatProp 报告当前元素（深度 5）是否位于 response/propstat/prop 之下。
func (rw *rewriter) inPropstatProp() bool {
	return rw.stack[depthPropstat-1].name == namePropstat && rw.stack[depthProperty-2].name == nameProp
}

func (rw *rewriter) endHref(start int64) {
	u := rw.unit
	h := u.href
	u.href = nil
	text := h.text.String()
	top := u.isResponse && h.depth == depthTopHref
	if top && u.firstHref == "" {
		u.firstHref = text
	}
	mapped, kind := rw.opts.MapHref(text)
	if kind != HrefOutside {
		if top {
			u.topMapped = true
			u.mountRoot = u.mountRoot || kind == HrefMountRoot
		}
		u.edits = append(u.edits, edit{from: h.contentStart, to: start, repl: escapeText(mapped)})
		return
	}
	rw.stack[rw.removalDepth(h.depth)-1].remove = true
}

// removalDepth 返回删除一个挂载目标之外的 href 时需要整体删除的元素的深度，规则见 RewriteMultistatus。
func (rw *rewriter) removalDepth(hrefDepth int) int {
	switch {
	case !rw.unit.isResponse:
		return depthUnit
	case hrefDepth == depthTopHref:
		return depthTopHref
	case rw.stack[depthPropstat-1].name != namePropstat:
		return depthPropstat // response 下的 location、error 等
	case hrefDepth >= depthProperty && rw.stack[depthProperty-2].name == nameProp:
		return depthProperty
	default:
		return depthProperty - 1 // propstat 下的 error、responsedescription 等
	}
}

func (rw *rewriter) endUnit(end int64, removed bool) error {
	u := rw.unit
	rw.unit = nil
	if u.isResponse && !u.topMapped {
		rw.stats.Dropped++
		if rw.stats.FirstDropped == "" {
			rw.stats.FirstDropped = u.firstHref
		}
		removed = true
	}
	if removed {
		rw.in.discard(end)
		return nil
	}
	if u.mountRoot {
		// 自闭合的 <displayname/> 没有内容区间可替换，它本来就是空的，保持原样即可。
		for _, s := range u.displayNames {
			if s.ok && !s.selfClose {
				u.edits = append(u.edits, edit{from: s.from, to: s.to, repl: escapeText(rw.opts.MountName)})
			}
		}
	}
	slices.SortFunc(u.edits, func(a, b edit) int { return cmp.Compare(a.from, b.from) })

	var out bytes.Buffer
	pos := u.start
	for _, e := range u.edits {
		out.Write(rw.in.bytes(pos, e.from))
		out.WriteString(e.repl)
		pos = e.to
	}
	out.Write(rw.in.bytes(pos, end))
	rw.in.discard(end)
	if u.isResponse {
		rw.stats.Responses++
	}
	_, err := rw.dst.Write(out.Bytes())
	return err
}

// flush 原样写出 off 之前尚未处理的字节。
func (rw *rewriter) flush(off int64) error {
	b := rw.in.bytes(rw.in.base, off)
	if len(b) == 0 {
		return nil
	}
	_, err := rw.dst.Write(b)
	rw.in.discard(off)
	return err
}

// captureReader 记录 xml.Decoder 读过但尚未写出或丢弃的原始字节。
// xml.Decoder 发现 io.ByteReader 后只调用 ReadByte，InputOffset 与这里的偏移一一对应；
// 解码器可能多读一个字节再回退，所以 buf 可能比 InputOffset 多出几个字节。
type captureReader struct {
	r    *bufio.Reader
	buf  []byte
	base int64 // buf[0] 的偏移
}

func (c *captureReader) ReadByte() (byte, error) {
	if len(c.buf) >= maxBufferedBytes {
		return 0, errTooLarge
	}
	b, err := c.r.ReadByte()
	if err == nil {
		c.buf = append(c.buf, b)
	}
	return b, err
}

func (c *captureReader) Read(p []byte) (int, error) {
	if len(c.buf) >= maxBufferedBytes {
		return 0, errTooLarge
	}
	n, err := c.r.Read(p[:min(len(p), maxBufferedBytes-len(c.buf))])
	c.buf = append(c.buf, p[:n]...)
	return n, err
}

func (c *captureReader) bytes(from, to int64) []byte {
	return c.buf[from-c.base : to-c.base]
}

func (c *captureReader) discard(off int64) {
	n := copy(c.buf, c.buf[off-c.base:])
	c.buf = c.buf[:n]
	c.base = off
}

// charsetReader 只接受与 UTF-8 兼容的编码声明。改写是在原始字节上进行的，
// 转码会让偏移失效；主流 WebDAV 服务器都输出 UTF-8。
func charsetReader(label string, input io.Reader) (io.Reader, error) {
	switch strings.ToLower(label) {
	case "utf-8", "utf8", "us-ascii", "ascii":
		return input, nil
	}
	return nil, fmt.Errorf("davxml: unsupported XML encoding %q", label)
}

func escapeText(s string) string {
	var b strings.Builder
	xml.EscapeText(&b, []byte(s))
	return b.String()
}
