package main

import (
	"bytes"
	"encoding/xml"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/jpeg"
	"image/png"
	"io"
	"log"
	"math"
	"net/http"
	"strconv"
	"strings"
	"text/template"
)

const (
	layerName  = "google_satellite"
	layerTitle = "Google Satellite"

	maxImageSize = 8192 // max WIDTH and HEIGHT, in pixels

	v111 = "1.1.1"
	v130 = "1.3.0"

	// Web Mercator world extent, in meters.
	worldMax = 20037508.342789244
	// Latitude limit of Web Mercator, in degrees.
	maxLat = 85.0511287798066
)

// srsAliases are names clients use for the same Web Mercator CRS.
// We only advertise EPSG:3857, but accept these in GetMap.
var srsAliases = map[string]bool{
	"EPSG:3857":   true,
	"EPSG:900913": true,
	"EPSG:102100": true,
	"EPSG:102113": true,
}

// wmsError is a WMS service exception (sent back as XML).
type wmsError struct {
	code string // OGC exception code, may be empty
	msg  string
}

func (e *wmsError) Error() string { return e.code + ": " + e.msg }

func errf(code, format string, args ...any) *wmsError {
	return &wmsError{code: code, msg: fmt.Sprintf(format, args...)}
}

// params holds query parameters with lower-case keys.
// WMS parameter names are case-insensitive.
type params map[string]string

func parseParams(r *http.Request) params {
	p := params{}
	for k, vs := range r.URL.Query() {
		key := strings.ToLower(k)
		if _, seen := p[key]; !seen && len(vs) > 0 {
			p[key] = vs[0]
		}
	}
	return p
}

// negotiateVersion picks 1.1.1 or 1.3.0 from the client's request.
func negotiateVersion(p params) string {
	v := p["version"]
	if v == "" {
		v = p["wmtver"] // very old clients
		if v != "" {
			return v111
		}
	}
	if v == "" {
		return v130
	}
	if compareVersions(v, v130) >= 0 {
		return v130
	}
	return v111
}

// compareVersions compares dotted versions like "1.1.1" and "1.3.0".
func compareVersions(a, b string) int {
	as, bs := strings.Split(a, "."), strings.Split(b, ".")
	for i := 0; i < 3; i++ {
		var x, y int
		if i < len(as) {
			x, _ = strconv.Atoi(as[i])
		}
		if i < len(bs) {
			y, _ = strconv.Atoi(bs[i])
		}
		if x != y {
			if x < y {
				return -1
			}
			return 1
		}
	}
	return 0
}

// handleWMS is the single /wms endpoint.
func handleWMS(w http.ResponseWriter, r *http.Request) {
	p := parseParams(r)
	version := negotiateVersion(p)

	if s := p["service"]; s != "" && !strings.EqualFold(s, "WMS") {
		writeException(w, version, errf("", "unsupported SERVICE %q, only WMS", s))
		return
	}

	switch req := strings.ToLower(p["request"]); req {
	case "getcapabilities", "capabilities":
		writeCapabilities(w, r, version)
	case "getmap", "map":
		gm, err := parseGetMap(p, version)
		if err != nil {
			writeException(w, version, err)
			return
		}
		serveGetMap(w, gm)
	case "":
		writeException(w, version, errf("MissingParameterValue", "missing REQUEST parameter"))
	default:
		writeException(w, version, errf("OperationNotSupported", "unsupported REQUEST %q", p["request"]))
	}
}

// getMapRequest is a parsed and checked GetMap request.
type getMapRequest struct {
	version                string
	minX, minY, maxX, maxY float64 // BBOX in EPSG:3857 meters
	width, height          int
	format                 string // "image/png" or "image/jpeg"
}

func parseGetMap(p params, version string) (*getMapRequest, *wmsError) {
	gm := &getMapRequest{version: version}

	// LAYERS: must be our single layer.
	layers := p["layers"]
	if layers == "" {
		return nil, errf("LayerNotDefined", "missing LAYERS parameter")
	}
	for _, l := range strings.Split(layers, ",") {
		if strings.TrimSpace(l) != layerName {
			return nil, errf("LayerNotDefined", "unknown layer %q, only %q", l, layerName)
		}
	}
	// STYLES is ignored: we only have the default style.

	// SRS (1.1.1) / CRS (1.3.0). Accept either name, to be lenient.
	srsKey, srsCode := "srs", "InvalidSRS"
	if version == v130 {
		srsKey, srsCode = "crs", "InvalidCRS"
	}
	srs := p[srsKey]
	if srs == "" { // client used the other version's name
		srs = p["srs"]
		if srsKey == "srs" {
			srs = p["crs"]
		}
	}
	if srs == "" {
		return nil, errf(srsCode, "missing %s parameter", strings.ToUpper(srsKey))
	}
	if !srsAliases[strings.ToUpper(srs)] {
		return nil, errf(srsCode, "unsupported %s %q, only EPSG:3857", strings.ToUpper(srsKey), srs)
	}

	// BBOX: minX,minY,maxX,maxY in meters. For EPSG:3857 the axis
	// order is the same in 1.1.1 and 1.3.0.
	parts := strings.Split(p["bbox"], ",")
	if len(parts) != 4 {
		return nil, errf("", "BBOX must be minx,miny,maxx,maxy, got %q", p["bbox"])
	}
	var b [4]float64
	for i, s := range parts {
		f, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
		if err != nil || math.IsNaN(f) || math.IsInf(f, 0) {
			return nil, errf("", "bad BBOX value %q", s)
		}
		b[i] = f
	}
	gm.minX, gm.minY, gm.maxX, gm.maxY = b[0], b[1], b[2], b[3]
	if gm.minX >= gm.maxX || gm.minY >= gm.maxY {
		return nil, errf("", "BBOX min must be smaller than max, got %q", p["bbox"])
	}

	// WIDTH / HEIGHT.
	var err *wmsError
	if gm.width, err = parseSize(p, "width"); err != nil {
		return nil, err
	}
	if gm.height, err = parseSize(p, "height"); err != nil {
		return nil, err
	}

	// FORMAT. Drop extra options like "image/png; mode=8bit".
	f := strings.ToLower(strings.TrimSpace(strings.SplitN(p["format"], ";", 2)[0]))
	switch f {
	case "image/png", "png":
		gm.format = "image/png"
	case "image/jpeg", "image/jpg", "jpeg", "jpg":
		gm.format = "image/jpeg"
	case "":
		return nil, errf("InvalidFormat", "missing FORMAT parameter")
	default:
		return nil, errf("InvalidFormat", "unsupported FORMAT %q, use image/png or image/jpeg", p["format"])
	}

	return gm, nil
}

func parseSize(p params, key string) (int, *wmsError) {
	s := p[key]
	if s == "" {
		return 0, errf("MissingParameterValue", "missing %s parameter", strings.ToUpper(key))
	}
	n, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil || n < 1 {
		return 0, errf("InvalidParameterValue", "bad %s %q", strings.ToUpper(key), s)
	}
	if n > maxImageSize {
		return 0, errf("InvalidParameterValue", "%s %d is too big, max is %d", strings.ToUpper(key), n, maxImageSize)
	}
	return n, nil
}

// serveGetMap renders the map. For now (step 2) it is a plain gray image.
func serveGetMap(w http.ResponseWriter, gm *getMapRequest) {
	img := image.NewRGBA(image.Rect(0, 0, gm.width, gm.height))
	draw.Draw(img, img.Bounds(), &image.Uniform{color.Gray{Y: 128}}, image.Point{}, draw.Src)
	writeImage(w, img, gm.format)
}

// writeImage encodes img in memory first, so an encode error never
// leaves a half-written response.
func writeImage(w http.ResponseWriter, img image.Image, format string) {
	var buf bytes.Buffer
	var err error
	if format == "image/jpeg" {
		err = jpeg.Encode(&buf, img, &jpeg.Options{Quality: 90})
	} else {
		err = (&png.Encoder{CompressionLevel: png.BestSpeed}).Encode(&buf, img)
	}
	if err != nil {
		log.Printf("encode %s: %v", format, err)
		http.Error(w, "image encode failed", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", format)
	w.Header().Set("Content-Length", strconv.Itoa(buf.Len()))
	w.Write(buf.Bytes())
}

// writeException sends a WMS ServiceExceptionReport. We use HTTP 200,
// like most WMS servers, because some clients ignore the body otherwise.
func writeException(w http.ResponseWriter, version string, e *wmsError) {
	log.Printf("WMS exception: %v", e)

	var esc bytes.Buffer
	xml.EscapeText(&esc, []byte(e.msg))
	codeAttr := ""
	if e.code != "" {
		codeAttr = fmt.Sprintf(` code="%s"`, e.code)
	}

	var body string
	if version == v130 {
		w.Header().Set("Content-Type", "text/xml; charset=utf-8")
		body = fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<ServiceExceptionReport version="1.3.0" xmlns="http://www.opengis.net/ogc">
  <ServiceException%s>%s</ServiceException>
</ServiceExceptionReport>
`, codeAttr, esc.String())
	} else {
		w.Header().Set("Content-Type", "application/vnd.ogc.se_xml; charset=utf-8")
		body = fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<ServiceExceptionReport version="1.1.1">
  <ServiceException%s>%s</ServiceException>
</ServiceExceptionReport>
`, codeAttr, esc.String())
	}
	io.WriteString(w, body)
}

// serviceURL builds the URL clients should call back, from the Host
// header. This works for 127.0.0.1 and for a VM address alike.
func serviceURL(r *http.Request) string {
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	return scheme + "://" + r.Host + "/wms?"
}

// capsData numbers are strings in plain decimal form: the template
// would print floats like 2.0037508342789244e+07.
type capsData struct {
	URL                   string
	MaxSize               int
	LayerName, LayerTitle string
	WorldMin, WorldMax    string
	MaxLat                string
}

func fmtFloat(f float64) string { return strconv.FormatFloat(f, 'f', -1, 64) }

func writeCapabilities(w http.ResponseWriter, r *http.Request, version string) {
	data := capsData{
		URL:        serviceURL(r),
		MaxSize:    maxImageSize,
		LayerName:  layerName,
		LayerTitle: layerTitle,
		WorldMin:   fmtFloat(-worldMax),
		WorldMax:   fmtFloat(worldMax),
		MaxLat:     fmtFloat(maxLat),
	}
	tmpl, ctype := caps130, "text/xml; charset=utf-8"
	if version == v111 {
		tmpl, ctype = caps111, "application/vnd.ogc.wms_xml; charset=utf-8"
	}
	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, data); err != nil {
		log.Printf("capabilities template: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", ctype)
	w.Write(buf.Bytes())
}

var tmplFuncs = template.FuncMap{
	"x": func(s string) string { // XML-escape
		var b bytes.Buffer
		xml.EscapeText(&b, []byte(s))
		return b.String()
	},
}

// No DOCTYPE on purpose: some XML parsers try to download the DTD.
var caps111 = template.Must(template.New("caps111").Funcs(tmplFuncs).Parse(`<?xml version="1.0" encoding="UTF-8"?>
<WMT_MS_Capabilities version="1.1.1">
  <Service>
    <Name>OGC:WMS</Name>
    <Title>geoproxy</Title>
    <Abstract>Google satellite tiles served as WMS</Abstract>
    <OnlineResource xmlns:xlink="http://www.w3.org/1999/xlink" xlink:type="simple" xlink:href="{{x .URL}}"/>
    <Fees>none</Fees>
    <AccessConstraints>none</AccessConstraints>
  </Service>
  <Capability>
    <Request>
      <GetCapabilities>
        <Format>application/vnd.ogc.wms_xml</Format>
        <DCPType><HTTP><Get><OnlineResource xmlns:xlink="http://www.w3.org/1999/xlink" xlink:type="simple" xlink:href="{{x .URL}}"/></Get></HTTP></DCPType>
      </GetCapabilities>
      <GetMap>
        <Format>image/png</Format>
        <Format>image/jpeg</Format>
        <DCPType><HTTP><Get><OnlineResource xmlns:xlink="http://www.w3.org/1999/xlink" xlink:type="simple" xlink:href="{{x .URL}}"/></Get></HTTP></DCPType>
      </GetMap>
    </Request>
    <Exception>
      <Format>application/vnd.ogc.se_xml</Format>
    </Exception>
    <Layer>
      <Title>geoproxy</Title>
      <SRS>EPSG:3857</SRS>
      <LatLonBoundingBox minx="-180" miny="-{{.MaxLat}}" maxx="180" maxy="{{.MaxLat}}"/>
      <BoundingBox SRS="EPSG:3857" minx="{{.WorldMin}}" miny="{{.WorldMin}}" maxx="{{.WorldMax}}" maxy="{{.WorldMax}}"/>
      <Layer queryable="0" opaque="1">
        <Name>{{.LayerName}}</Name>
        <Title>{{.LayerTitle}}</Title>
        <SRS>EPSG:3857</SRS>
        <LatLonBoundingBox minx="-180" miny="-{{.MaxLat}}" maxx="180" maxy="{{.MaxLat}}"/>
        <BoundingBox SRS="EPSG:3857" minx="{{.WorldMin}}" miny="{{.WorldMin}}" maxx="{{.WorldMax}}" maxy="{{.WorldMax}}"/>
      </Layer>
    </Layer>
  </Capability>
</WMT_MS_Capabilities>
`))

var caps130 = template.Must(template.New("caps130").Funcs(tmplFuncs).Parse(`<?xml version="1.0" encoding="UTF-8"?>
<WMS_Capabilities version="1.3.0" xmlns="http://www.opengis.net/wms" xmlns:xlink="http://www.w3.org/1999/xlink">
  <Service>
    <Name>WMS</Name>
    <Title>geoproxy</Title>
    <Abstract>Google satellite tiles served as WMS</Abstract>
    <OnlineResource xlink:type="simple" xlink:href="{{x .URL}}"/>
    <Fees>none</Fees>
    <AccessConstraints>none</AccessConstraints>
    <LayerLimit>1</LayerLimit>
    <MaxWidth>{{.MaxSize}}</MaxWidth>
    <MaxHeight>{{.MaxSize}}</MaxHeight>
  </Service>
  <Capability>
    <Request>
      <GetCapabilities>
        <Format>text/xml</Format>
        <DCPType><HTTP><Get><OnlineResource xlink:type="simple" xlink:href="{{x .URL}}"/></Get></HTTP></DCPType>
      </GetCapabilities>
      <GetMap>
        <Format>image/png</Format>
        <Format>image/jpeg</Format>
        <DCPType><HTTP><Get><OnlineResource xlink:type="simple" xlink:href="{{x .URL}}"/></Get></HTTP></DCPType>
      </GetMap>
    </Request>
    <Exception>
      <Format>XML</Format>
    </Exception>
    <Layer>
      <Title>geoproxy</Title>
      <CRS>EPSG:3857</CRS>
      <EX_GeographicBoundingBox>
        <westBoundLongitude>-180</westBoundLongitude>
        <eastBoundLongitude>180</eastBoundLongitude>
        <southBoundLatitude>-{{.MaxLat}}</southBoundLatitude>
        <northBoundLatitude>{{.MaxLat}}</northBoundLatitude>
      </EX_GeographicBoundingBox>
      <BoundingBox CRS="EPSG:3857" minx="{{.WorldMin}}" miny="{{.WorldMin}}" maxx="{{.WorldMax}}" maxy="{{.WorldMax}}"/>
      <Layer queryable="0" opaque="1">
        <Name>{{.LayerName}}</Name>
        <Title>{{.LayerTitle}}</Title>
        <CRS>EPSG:3857</CRS>
        <EX_GeographicBoundingBox>
          <westBoundLongitude>-180</westBoundLongitude>
          <eastBoundLongitude>180</eastBoundLongitude>
          <southBoundLatitude>-{{.MaxLat}}</southBoundLatitude>
          <northBoundLatitude>{{.MaxLat}}</northBoundLatitude>
        </EX_GeographicBoundingBox>
        <BoundingBox CRS="EPSG:3857" minx="{{.WorldMin}}" miny="{{.WorldMin}}" maxx="{{.WorldMax}}" maxy="{{.WorldMax}}"/>
      </Layer>
    </Layer>
  </Capability>
</WMS_Capabilities>
`))
