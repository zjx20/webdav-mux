package davxml

import (
	"encoding/xml"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestParsePropfind(t *testing.T) {
	tests := []struct {
		name string
		body string
		want Propfind
	}{
		{"empty body", "", Propfind{Kind: AllProp}},
		{"whitespace body", " \r\n", Propfind{Kind: AllProp}},
		{"allprop", `<?xml version="1.0"?><D:propfind xmlns:D="DAV:"><D:allprop/></D:propfind>`, Propfind{Kind: AllProp}},
		{"allprop with include", `<propfind xmlns="DAV:"><allprop/><include><supported-report-set/></include></propfind>`, Propfind{Kind: AllProp}},
		{"propname", `<a:propfind xmlns:a="DAV:"><a:propname/></a:propfind>`, Propfind{Kind: PropName}},
		{
			"prop with foreign namespaces and duplicates",
			`<propfind xmlns="DAV:" xmlns:z="urn:schemas-microsoft-com:"><prop><resourcetype/><z:Win32FileAttributes/><getcontentlength/><resourcetype/></prop></propfind>`,
			Propfind{Kind: Prop, Names: []xml.Name{
				{Space: "DAV:", Local: "resourcetype"},
				{Space: "urn:schemas-microsoft-com:", Local: "Win32FileAttributes"},
				{Space: "DAV:", Local: "getcontentlength"},
			}},
		},
		{"empty prop", `<propfind xmlns="DAV:"><prop></prop></propfind>`, Propfind{Kind: Prop}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParsePropfind([]byte(tt.body))
			if err != nil {
				t.Fatal(err)
			}
			if got.Kind != tt.want.Kind || !slices.Equal(got.Names, tt.want.Names) {
				t.Errorf("got %+v; want %+v", got, tt.want)
			}
		})
	}
}

func TestParsePropfindErrors(t *testing.T) {
	for _, body := range []string{
		`<propfind/>`, // 不在 DAV: 命名空间
		`<propfind xmlns="DAV:"></propfind>`,
		`<propfind xmlns="DAV:"><allprop/><prop><getetag/></prop></propfind>`,
		`<propfind xmlns="DAV:"><allprop/>`,
		`<D:propfind xmlns:D="DAV:"><D:allprop/></D:propfin>`,
		`text<propfind xmlns="DAV:"><allprop/></propfind>`,
		`<lockinfo xmlns="DAV:"/>`,
	} {
		if _, err := ParsePropfind([]byte(body)); err == nil {
			t.Errorf("ParsePropfind(%q) succeeded; want error", body)
		}
	}
}

var testTime = time.Date(2025, 9, 1, 10, 0, 0, 0, time.UTC)

func TestWriteCollectionsAllProp(t *testing.T) {
	var b strings.Builder
	cols := []Collection{
		{Href: "/", ModTime: testTime},
		{Href: "/a%20%26b/", DisplayName: "a &b", ModTime: testTime},
	}
	if err := WriteCollections(&b, cols, Propfind{Kind: AllProp}); err != nil {
		t.Fatal(err)
	}
	const props = `<D:getlastmodified>Mon, 01 Sep 2025 10:00:00 GMT</D:getlastmodified><D:creationdate>2025-09-01T10:00:00Z</D:creationdate></D:prop><D:status>HTTP/1.1 200 OK</D:status></D:propstat></D:response>`
	want := `<?xml version="1.0" encoding="UTF-8"?>` + "\n" + `<D:multistatus xmlns:D="DAV:">` +
		`<D:response><D:href>/</D:href><D:propstat><D:prop><D:resourcetype><D:collection/></D:resourcetype><D:displayname/>` + props +
		`<D:response><D:href>/a%20%26b/</D:href><D:propstat><D:prop><D:resourcetype><D:collection/></D:resourcetype><D:displayname>a &amp;b</D:displayname>` + props +
		"</D:multistatus>\n"
	if b.String() != want {
		t.Errorf("got:\n%s\nwant:\n%s", b.String(), want)
	}
}

// parsedResponse 是测试用的 multistatus 结构，按命名空间解析，验证输出是合法且语义正确的 XML。
type parsedMultistatus struct {
	XMLName   xml.Name `xml:"DAV: multistatus"`
	Responses []struct {
		Href      string `xml:"DAV: href"`
		Propstats []struct {
			Prop struct {
				Inner []struct {
					XMLName xml.Name
					Value   string `xml:",innerxml"`
				} `xml:",any"`
			} `xml:"DAV: prop"`
			Status string `xml:"DAV: status"`
		} `xml:"DAV: propstat"`
	} `xml:"DAV: response"`
}

func TestWriteCollectionsProp(t *testing.T) {
	var b strings.Builder
	pf := Propfind{Kind: Prop, Names: []xml.Name{
		{Space: "DAV:", Local: "resourcetype"},
		{Space: `urn:"odd"&ns`, Local: "Win32FileAttributes"},
		{Space: "", Local: "bare"},
		{Space: "DAV:", Local: "getcontentlength"},
		{Space: "DAV:", Local: "displayname"},
	}}
	if err := WriteCollections(&b, []Collection{{Href: "/work/", DisplayName: "work", ModTime: testTime}}, pf); err != nil {
		t.Fatal(err)
	}
	var ms parsedMultistatus
	if err := xml.Unmarshal([]byte(b.String()), &ms); err != nil {
		t.Fatalf("output is not valid multistatus XML: %v\n%s", err, b.String())
	}
	if len(ms.Responses) != 1 || ms.Responses[0].Href != "/work/" {
		t.Fatalf("responses: %+v", ms.Responses)
	}
	got := map[string][]xml.Name{}
	for _, ps := range ms.Responses[0].Propstats {
		for _, p := range ps.Prop.Inner {
			got[ps.Status] = append(got[ps.Status], p.XMLName)
		}
	}
	wantOK := []xml.Name{{Space: "DAV:", Local: "resourcetype"}, {Space: "DAV:", Local: "displayname"}}
	wantMissing := []xml.Name{{Space: `urn:"odd"&ns`, Local: "Win32FileAttributes"}, {Local: "bare"}, {Space: "DAV:", Local: "getcontentlength"}}
	if !slices.Equal(got["HTTP/1.1 200 OK"], wantOK) || !slices.Equal(got["HTTP/1.1 404 Not Found"], wantMissing) {
		t.Errorf("propstats = %v\n%s", got, b.String())
	}
}

func TestWriteCollectionsPropName(t *testing.T) {
	var b strings.Builder
	if err := WriteCollections(&b, []Collection{{Href: "/"}}, Propfind{Kind: PropName}); err != nil {
		t.Fatal(err)
	}
	want := `<D:prop><D:resourcetype/><D:displayname/><D:getlastmodified/><D:creationdate/></D:prop>`
	if !strings.Contains(b.String(), want) {
		t.Errorf("got %s", b.String())
	}
}
