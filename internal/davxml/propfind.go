package davxml

import (
	"bytes"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// PropfindKind 是 PROPFIND 请求体的三种形式（RFC 4918 §14.20）。
type PropfindKind int

const (
	AllProp PropfindKind = iota
	PropName
	Prop
)

// Propfind 是解析后的 PROPFIND 请求体。
type Propfind struct {
	Kind  PropfindKind
	Names []xml.Name // Kind 为 Prop 时请求的属性，已去重
}

// ErrBadPropfind 表示 PROPFIND 请求体不是合法的 DAV:propfind 文档。
var ErrBadPropfind = errors.New("davxml: malformed PROPFIND request body")

// ParsePropfind 解析 PROPFIND 请求体。按 RFC 4918 §9.1，空请求体等价于 allprop。
// allprop 附带的 DAV:include 被忽略：合成目录没有 allprop 之外的属性。
func ParsePropfind(body []byte) (Propfind, error) {
	if len(bytes.TrimSpace(body)) == 0 {
		return Propfind{Kind: AllProp}, nil
	}
	dec := xml.NewDecoder(bytes.NewReader(body))
	dec.CharsetReader = charsetReader

	root, err := nextStartElement(dec)
	if err != nil || root.Name != (xml.Name{Space: "DAV:", Local: "propfind"}) {
		return Propfind{}, ErrBadPropfind
	}
	var pf Propfind
	forms := 0
	for {
		tok, err := dec.Token()
		if err != nil {
			return Propfind{}, ErrBadPropfind
		}
		switch t := tok.(type) {
		case xml.StartElement:
			switch t.Name {
			case xml.Name{Space: "DAV:", Local: "allprop"}:
				pf.Kind, forms = AllProp, forms+1
			case xml.Name{Space: "DAV:", Local: "propname"}:
				pf.Kind, forms = PropName, forms+1
			case xml.Name{Space: "DAV:", Local: "prop"}:
				pf.Kind, forms = Prop, forms+1
				if pf.Names, err = childNames(dec); err != nil {
					return Propfind{}, ErrBadPropfind
				}
				continue
			}
			if err := dec.Skip(); err != nil {
				return Propfind{}, ErrBadPropfind
			}
		case xml.EndElement:
			// </propfind>：三种形式必须恰好出现一种。
			if forms != 1 {
				return Propfind{}, ErrBadPropfind
			}
			return pf, nil
		}
	}
}

func nextStartElement(dec *xml.Decoder) (xml.StartElement, error) {
	for {
		tok, err := dec.Token()
		if err != nil {
			return xml.StartElement{}, err
		}
		switch t := tok.(type) {
		case xml.StartElement:
			return t, nil
		case xml.CharData:
			if len(bytes.TrimSpace(t)) > 0 {
				return xml.StartElement{}, ErrBadPropfind
			}
		}
	}
}

// childNames 读取当前元素的直接子元素名，直到当前元素结束。
func childNames(dec *xml.Decoder) ([]xml.Name, error) {
	var names []xml.Name
	seen := make(map[xml.Name]bool)
	for {
		tok, err := dec.Token()
		if err != nil {
			return nil, err
		}
		switch t := tok.(type) {
		case xml.StartElement:
			if !seen[t.Name] {
				seen[t.Name] = true
				names = append(names, t.Name)
			}
			if err := dec.Skip(); err != nil {
				return nil, err
			}
		case xml.EndElement:
			return names, nil
		}
	}
}

// Collection 是本服务合成的一个目录：虚拟根目录，或挂载点所在的中间目录。
type Collection struct {
	Href        string // 已百分号编码
	DisplayName string
	ModTime     time.Time
}

// 合成目录支持的属性，也是 allprop 和 propname 返回的属性，顺序固定。
var collectionProps = []xml.Name{
	{Space: "DAV:", Local: "resourcetype"},
	{Space: "DAV:", Local: "displayname"},
	{Space: "DAV:", Local: "getlastmodified"},
	{Space: "DAV:", Local: "creationdate"},
}

func (c Collection) propValue(name xml.Name) (string, bool) {
	if name.Space != "DAV:" {
		return "", false
	}
	switch name.Local {
	case "resourcetype":
		return "<D:collection/>", true
	case "displayname":
		return escapeText(c.DisplayName), true
	case "getlastmodified":
		return c.ModTime.UTC().Format(http.TimeFormat), true
	case "creationdate":
		return c.ModTime.UTC().Format(time.RFC3339), true
	}
	return "", false
}

// WriteCollections 按 PROPFIND 请求 pf 为合成目录输出 multistatus 文档。
func WriteCollections(w io.Writer, cols []Collection, pf Propfind) error {
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?>` + "\n" + `<D:multistatus xmlns:D="DAV:">`)
	for _, c := range cols {
		b.WriteString("<D:response><D:href>")
		b.WriteString(escapeText(c.Href))
		b.WriteString("</D:href>")
		switch pf.Kind {
		case AllProp:
			writePropstat(&b, collectionProps, http.StatusOK, c.propValue)
		case PropName:
			writePropstat(&b, collectionProps, http.StatusOK, nil)
		case Prop:
			var found, missing []xml.Name
			for _, n := range pf.Names {
				if _, ok := c.propValue(n); ok {
					found = append(found, n)
				} else {
					missing = append(missing, n)
				}
			}
			writePropstat(&b, found, http.StatusOK, c.propValue)
			writePropstat(&b, missing, http.StatusNotFound, nil)
		}
		b.WriteString("</D:response>")
	}
	b.WriteString("</D:multistatus>\n")
	_, err := io.WriteString(w, b.String())
	return err
}

// writePropstat 写出一个 propstat；value 为 nil 时只写属性名（空元素）。
func writePropstat(b *strings.Builder, names []xml.Name, status int, value func(xml.Name) (string, bool)) {
	if len(names) == 0 {
		return
	}
	b.WriteString("<D:propstat><D:prop>")
	for _, n := range names {
		v := ""
		if value != nil {
			v, _ = value(n)
		}
		writeElement(b, n, v)
	}
	fmt.Fprintf(b, "</D:prop><D:status>HTTP/1.1 %d %s</D:status></D:propstat>", status, http.StatusText(status))
}

// writeElement 写出属性元素 name，内容为已转义的 inner。
// 非 DAV: 命名空间的元素各自声明前缀：请求里的属性可以来自任意命名空间。
func writeElement(b *strings.Builder, name xml.Name, inner string) {
	var qname string
	switch name.Space {
	case "DAV:":
		qname = "D:" + name.Local
		b.WriteString("<" + qname)
	case "":
		qname = name.Local
		b.WriteString("<" + qname)
	default:
		qname = "ns0:" + name.Local
		b.WriteString("<" + qname + ` xmlns:ns0="`)
		b.WriteString(escapeText(name.Space))
		b.WriteString(`"`)
	}
	if inner == "" {
		b.WriteString("/>")
		return
	}
	b.WriteString(">" + inner + "</" + qname + ">")
}

// FiniteDepthError 是拒绝 "Depth: infinity" 的 PROPFIND 时返回的响应体（RFC 4918 §9.1、§16）。
const FiniteDepthError = `<?xml version="1.0" encoding="UTF-8"?>` + "\n" +
	`<D:error xmlns:D="DAV:"><D:propfind-finite-depth/></D:error>` + "\n"
