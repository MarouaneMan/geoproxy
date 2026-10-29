package main

import (
	"encoding/xml"
	"image"
	_ "image/jpeg"
	_ "image/png"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// get calls the WMS handler with a raw query string.
func get(t *testing.T, query string) *http.Response {
	t.Helper()
	req := httptest.NewRequest("GET", "http://example.local:8085/wms?"+query, nil)
	rec := httptest.NewRecorder()
	handleWMS(rec, req)
	return rec.Result()
}

func body(t *testing.T, resp *http.Response) string {
	t.Helper()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// wellFormed fails the test if s is not valid XML.
func wellFormed(t *testing.T, s string) {
	t.Helper()
	d := xml.NewDecoder(strings.NewReader(s))
	for {
		_, err := d.Token()
		if err == io.EOF {
			return
		}
		if err != nil {
			t.Fatalf("bad XML: %v\n%s", err, s)
		}
	}
}

func TestNegotiateVersion(t *testing.T) {
	cases := map[string]string{
		"":                "1.3.0",
		"version=1.3.0":   "1.3.0",
		"version=1.4.0":   "1.3.0",
		"version=1.1.1":   "1.1.1",
		"version=1.1.0":   "1.1.1",
		"version=1.0.0":   "1.1.1",
		"wmtver=1.0.0":    "1.1.1",
		"VeRsIoN=1.1.1":   "1.1.1",
		"version=garbage": "1.1.1",
	}
	for q, want := range cases {
		req := httptest.NewRequest("GET", "/wms?"+q, nil)
		if got := negotiateVersion(parseParams(req)); got != want {
			t.Errorf("%q: got %s, want %s", q, got, want)
		}
	}
}

func TestCapabilities(t *testing.T) {
	for _, tc := range []struct{ version, root, ctype string }{
		{"1.1.1", "<WMT_MS_Capabilities", "application/vnd.ogc.wms_xml"},
		{"1.3.0", "<WMS_Capabilities", "text/xml"},
	} {
		resp := get(t, "service=WMS&request=GetCapabilities&version="+tc.version)
		s := body(t, resp)
		wellFormed(t, s)
		if !strings.Contains(s, tc.root) {
			t.Errorf("%s: missing root %s", tc.version, tc.root)
		}
		if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, tc.ctype) {
			t.Errorf("%s: content-type %q", tc.version, ct)
		}
		for _, want := range []string{
			"<Name>google_satellite</Name>",
			"EPSG:3857",
			"http://example.local:8085/wms?",
			`maxx="20037508.342789244"`,
		} {
			if !strings.Contains(s, want) {
				t.Errorf("%s: missing %q", tc.version, want)
			}
		}
		if strings.Contains(s, "e+07") {
			t.Errorf("%s: scientific notation in capabilities", tc.version)
		}
	}
}

const okMap = "LAYERS=google_satellite&STYLES=&BBOX=-1000,-500,1000,500&WIDTH=400&HEIGHT=200&FORMAT=image/png"

func TestGetMapOK(t *testing.T) {
	for _, q := range []string{
		"SERVICE=WMS&REQUEST=GetMap&VERSION=1.1.1&SRS=EPSG:3857&" + okMap,
		"SERVICE=WMS&REQUEST=GetMap&VERSION=1.3.0&CRS=EPSG:3857&" + okMap,
		"service=wms&request=getmap&version=1.3.0&crs=epsg:3857&" + strings.ToLower(okMap),
		"REQUEST=GetMap&VERSION=1.1.1&SRS=EPSG:900913&" + okMap,
		"REQUEST=GetMap&VERSION=1.3.0&SRS=EPSG:3857&" + okMap, // wrong name, still accepted
	} {
		resp := get(t, q)
		if ct := resp.Header.Get("Content-Type"); ct != "image/png" {
			t.Fatalf("%s: content-type %q, body %s", q, ct, body(t, resp))
		}
		img, format, err := image.Decode(resp.Body)
		if err != nil {
			t.Fatalf("%s: decode: %v", q, err)
		}
		if format != "png" || img.Bounds().Dx() != 400 || img.Bounds().Dy() != 200 {
			t.Errorf("%s: got %s %v", q, format, img.Bounds())
		}
	}
}

func TestGetMapJPEG(t *testing.T) {
	resp := get(t, "REQUEST=GetMap&VERSION=1.3.0&CRS=EPSG:3857&"+strings.Replace(okMap, "image/png", "image/jpeg", 1))
	img, format, err := image.Decode(resp.Body)
	if err != nil || format != "jpeg" || img.Bounds().Dx() != 400 {
		t.Fatalf("got %s %v %v", format, img, err)
	}
}

func TestGetMapErrors(t *testing.T) {
	base := "REQUEST=GetMap&VERSION=1.3.0&CRS=EPSG:3857&"
	cases := []struct{ query, code string }{
		{base + strings.Replace(okMap, "google_satellite", "roads", 1), "LayerNotDefined"},
		{"REQUEST=GetMap&VERSION=1.3.0&CRS=EPSG:4326&" + okMap, "InvalidCRS"},
		{"REQUEST=GetMap&VERSION=1.1.1&SRS=EPSG:4326&" + okMap, "InvalidSRS"},
		{"REQUEST=GetMap&VERSION=1.3.0&" + okMap, "InvalidCRS"},
		{base + strings.Replace(okMap, "image/png", "image/gif", 1), "InvalidFormat"},
		{base + strings.Replace(okMap, "WIDTH=400", "WIDTH=9000", 1), "InvalidParameterValue"},
		{base + strings.Replace(okMap, "WIDTH=400", "WIDTH=0", 1), "InvalidParameterValue"},
		{base + strings.Replace(okMap, "HEIGHT=200&", "", 1), "MissingParameterValue"},
		{base + strings.Replace(okMap, "-1000,-500,1000,500", "1000,-500,-1000,500", 1), ""},
		{base + strings.Replace(okMap, "-1000,-500,1000,500", "1,2,3", 1), ""},
		{base + strings.Replace(okMap, "-1000,-500,1000,500", "1,2,NaN,4", 1), ""},
		{"VERSION=1.3.0", "MissingParameterValue"},
		{"REQUEST=GetFeatureInfo", "OperationNotSupported"},
	}
	for _, tc := range cases {
		resp := get(t, tc.query)
		s := body(t, resp)
		wellFormed(t, s)
		if !strings.Contains(s, "<ServiceExceptionReport") {
			t.Errorf("%s: not an exception:\n%s", tc.query, s)
			continue
		}
		if tc.code != "" && !strings.Contains(s, `code="`+tc.code+`"`) {
			t.Errorf("%s: want code %s, got:\n%s", tc.query, tc.code, s)
		}
	}
}

func TestExceptionVersions(t *testing.T) {
	resp := get(t, "REQUEST=GetMap&VERSION=1.1.1&SRS=EPSG:4326&"+okMap)
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/vnd.ogc.se_xml") {
		t.Errorf("1.1.1 exception content-type %q", ct)
	}
	resp = get(t, "REQUEST=GetMap&VERSION=1.3.0&CRS=EPSG:4326&"+okMap)
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/xml") {
		t.Errorf("1.3.0 exception content-type %q", ct)
	}
	if s := body(t, resp); !strings.Contains(s, `xmlns="http://www.opengis.net/ogc"`) {
		t.Errorf("1.3.0 exception missing namespace:\n%s", s)
	}
}

func TestExceptionEscapesXML(t *testing.T) {
	s := body(t, get(t, "REQUEST=GetMap&VERSION=1.3.0&CRS=EPSG:3857&LAYERS=<evil>&"+okMap[len("LAYERS=google_satellite&"):]))
	wellFormed(t, s)
	if strings.Contains(s, "<evil>") {
		t.Errorf("layer name not escaped:\n%s", s)
	}
}
