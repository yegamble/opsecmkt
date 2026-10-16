package market

import (
	"bytes"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"math"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"
)

const brandingSVGCap = 256 << 10

var errBrandingSVG = &httpError{Code: 415, Msg: "Logo not saved: use a JPEG, PNG, or a self-contained SVG with simple shapes and text. SVG scripts, styles, links, embedded images, references, animations and document declarations are not accepted."}

var svgNumber = regexp.MustCompile(`^[+-]?(?:[0-9]+(?:\.[0-9]*)?|\.[0-9]+)(?:[eE][+-]?[0-9]+)?$`)
var svgNumberToken = regexp.MustCompile(`[+-]?(?:[0-9]+(?:\.[0-9]*)?|\.[0-9]+)(?:[eE][+-]?[0-9]+)?`)
var svgNumbers = regexp.MustCompile(`^[0-9eE+., \t\r\n-]+$`)
var svgPath = regexp.MustCompile(`^[MmZzLlHhVvCcSsQqTtAa0-9eE+., \t\r\n-]+$`)
var svgTransform = regexp.MustCompile(`^(?:(?:matrix|translate|scale|rotate|skewX|skewY)\([0-9eE+., \t\r\n-]+\)[ \t\r\n]*)+$`)
var svgColor = regexp.MustCompile(`^(?:none|currentColor|transparent|#[0-9a-fA-F]{3}|#[0-9a-fA-F]{4}|#[0-9a-fA-F]{6}|#[0-9a-fA-F]{8}|[a-zA-Z]{1,24}|rgba?\([0-9.,% \t]+\))$`)
var svgFont = regexp.MustCompile(`^[a-zA-Z0-9 ,'-]{1,120}$`)
var svgIdentifier = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_.-]{0,63}$`)

var svgElements = map[string]bool{"svg": true, "g": true, "path": true, "rect": true, "circle": true, "ellipse": true, "line": true, "polyline": true, "polygon": true, "text": true, "tspan": true, "title": true, "desc": true}
var svgNumericAttributes = map[string]bool{"x": true, "y": true, "x1": true, "y1": true, "x2": true, "y2": true, "cx": true, "cy": true, "r": true, "rx": true, "ry": true, "width": true, "height": true, "stroke-width": true, "stroke-miterlimit": true, "stroke-dashoffset": true, "opacity": true, "fill-opacity": true, "stroke-opacity": true, "font-size": true, "letter-spacing": true, "word-spacing": true, "dx": true, "dy": true}

// normalizeBrandingSVG accepts a deliberately small, inert SVG vocabulary and
// emits fresh XML. Unrecognized elements/attributes fail closed; original markup
// is never trusted as HTML, CSS, an external resource, or a template fragment.
func normalizeBrandingSVG(data []byte) ([]byte, int, int, error) {
	if len(data) == 0 || len(data) > brandingSVGCap {
		return nil, 0, 0, errBrandingSVG
	}
	d := xml.NewDecoder(bytes.NewReader(data))
	d.Strict = true
	var out bytes.Buffer
	e := xml.NewEncoder(&out)
	var stack []string
	root, done, count := false, false, 0
	elements, totalNumbers, textRunes, totalCommands := 0, 0, 0, 0
	ids := map[string]bool{}
	width, height := 0, 0
	for {
		tok, err := d.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, 0, 0, errBrandingSVG
		}
		count++
		if count > 10000 || len(stack) > 32 {
			return nil, 0, 0, errBrandingSVG
		}
		switch v := tok.(type) {
		case xml.StartElement:
			elements++
			name := v.Name.Local
			if elements > 1024 || len(v.Attr) > 24 || done || !svgElements[name] || v.Name.Space != "" && v.Name.Space != "http://www.w3.org/2000/svg" || name == "svg" && root || !root && name != "svg" {
				return nil, 0, 0, errBrandingSVG
			}
			attrs := make([]xml.Attr, 0, len(v.Attr)+2)
			seen := map[string]bool{}
			for _, attr := range v.Attr {
				if attr.Name.Space == "" && attr.Name.Local == "xmlns" {
					if root || attr.Value != "http://www.w3.org/2000/svg" || seen["xmlns"] {
						return nil, 0, 0, errBrandingSVG
					}
					seen["xmlns"] = true
					continue
				}
				if attr.Name.Space != "" || seen[attr.Name.Local] || !validBrandingSVGAttribute(name, attr.Name.Local, attr.Value) {
					return nil, 0, 0, errBrandingSVG
				}
				seen[attr.Name.Local] = true
				if attr.Name.Local == "id" {
					if ids[attr.Value] {
						return nil, 0, 0, errBrandingSVG
					}
					ids[attr.Value] = true
				}
				if attr.Name.Local == "d" || attr.Name.Local == "points" || attr.Name.Local == "transform" || attr.Name.Local == "stroke-dasharray" {
					numbers := svgNumberToken.FindAllString(attr.Value, -1)
					totalNumbers += len(numbers)
					if len(numbers) > 2048 || totalNumbers > 10000 {
						return nil, 0, 0, errBrandingSVG
					}
					for _, n := range numbers {
						if !boundedSVGNumber(n) {
							return nil, 0, 0, errBrandingSVG
						}
					}
				}
				if attr.Name.Local == "d" {
					commands := 0
					for _, c := range attr.Value {
						if strings.ContainsRune("MmZzLlHhVvCcSsQqTtAa", c) {
							commands++
						}
					}
					totalCommands += commands
					if commands > 2048 || totalCommands > 10000 {
						return nil, 0, 0, errBrandingSVG
					}
				}
				attrs = append(attrs, xml.Attr{Name: xml.Name{Local: attr.Name.Local}, Value: attr.Value})
			}
			if !root {
				width, height, err = brandingSVGDimensions(attrs)
				if err != nil {
					return nil, 0, 0, errBrandingSVG
				}
				var viewBox string
				for _, attr := range attrs {
					if attr.Name.Local == "viewBox" {
						viewBox = attr.Value
					}
				}
				if viewBox == "" {
					var ow, oh string
					for _, attr := range attrs {
						if attr.Name.Local == "width" {
							ow = strings.TrimSuffix(attr.Value, "px")
						}
						if attr.Name.Local == "height" {
							oh = strings.TrimSuffix(attr.Value, "px")
						}
					}
					attrs = append(attrs, xml.Attr{Name: xml.Name{Local: "viewBox"}, Value: "0 0 " + ow + " " + oh})
				}
				canonical := make([]xml.Attr, 0, len(attrs)+3)
				for _, attr := range attrs {
					if attr.Name.Local != "width" && attr.Name.Local != "height" {
						canonical = append(canonical, attr)
					}
				}
				attrs = append(canonical,
					xml.Attr{Name: xml.Name{Local: "width"}, Value: strconv.Itoa(width)},
					xml.Attr{Name: xml.Name{Local: "height"}, Value: strconv.Itoa(height)},
					xml.Attr{Name: xml.Name{Local: "xmlns"}, Value: "http://www.w3.org/2000/svg"})
				root = true
			}
			stack = append(stack, name)
			if err = e.EncodeToken(xml.StartElement{Name: xml.Name{Local: name}, Attr: attrs}); err != nil {
				return nil, 0, 0, errBrandingSVG
			}
		case xml.EndElement:
			if len(stack) == 0 || stack[len(stack)-1] != v.Name.Local {
				return nil, 0, 0, errBrandingSVG
			}
			stack = stack[:len(stack)-1]
			done = len(stack) == 0
			if err = e.EncodeToken(xml.EndElement{Name: xml.Name{Local: v.Name.Local}}); err != nil {
				return nil, 0, 0, errBrandingSVG
			}
		case xml.CharData:
			textRunes += utf8.RuneCount(v)
			if strings.TrimSpace(string(v)) != "" && textRunes > 4096 {
				return nil, 0, 0, errBrandingSVG
			}
			if len(stack) == 0 || stack[len(stack)-1] != "text" && stack[len(stack)-1] != "tspan" && stack[len(stack)-1] != "title" && stack[len(stack)-1] != "desc" {
				if strings.TrimSpace(string(v)) != "" {
					return nil, 0, 0, errBrandingSVG
				}
			} else if err = e.EncodeToken(v); err != nil {
				return nil, 0, 0, errBrandingSVG
			}
		case xml.Comment:
			// Comments are not presentation and do not survive normalization.
		default:
			// Processing instructions, DOCTYPE and entity declarations are refused.
			return nil, 0, 0, errBrandingSVG
		}
	}
	if !root || !done || len(stack) != 0 || e.Flush() != nil || out.Len() > brandingSVGCap {
		return nil, 0, 0, errBrandingSVG
	}
	return out.Bytes(), width, height, nil
}

func validBrandingSVGAttribute(element, key, value string) bool {
	if value == "" || len(value) > 64<<10 {
		return false
	}
	if svgNumericAttributes[key] {
		if key == "font-size" {
			n, err := strconv.ParseFloat(strings.TrimSuffix(value, "px"), 64)
			if err != nil || n <= 0 || n > 512 {
				return false
			}
		}
		return boundedSVGNumber(strings.TrimSuffix(value, "px"))
	}
	switch key {
	case "viewBox":
		return element == "svg" && svgNumbers.MatchString(value)
	case "d":
		return element == "path" && svgPath.MatchString(value)
	case "points", "stroke-dasharray":
		return svgNumbers.MatchString(value) || key == "stroke-dasharray" && value == "none"
	case "transform":
		return svgTransform.MatchString(value)
	case "fill", "stroke", "color":
		return len(value) <= 128 && svgColor.MatchString(value)
	case "fill-rule", "clip-rule":
		return value == "nonzero" || value == "evenodd"
	case "stroke-linecap":
		return value == "butt" || value == "round" || value == "square"
	case "stroke-linejoin":
		return value == "miter" || value == "round" || value == "bevel"
	case "font-family":
		return svgFont.MatchString(value)
	case "font-weight":
		return value == "normal" || value == "bold" || value == "100" || value == "200" || value == "300" || value == "400" || value == "500" || value == "600" || value == "700" || value == "800" || value == "900"
	case "text-anchor":
		return value == "start" || value == "middle" || value == "end"
	case "dominant-baseline":
		return value == "auto" || value == "middle" || value == "central" || value == "hanging" || value == "alphabetic"
	case "preserveAspectRatio":
		return value == "xMidYMid meet" || value == "xMidYMid" || value == "none"
	case "role":
		return element == "svg" && value == "img"
	case "aria-label":
		return len(value) <= 512
	case "id":
		return svgIdentifier.MatchString(value)
	case "aria-labelledby":
		parts := strings.Fields(value)
		if len(parts) == 0 || len(parts) > 8 {
			return false
		}
		for _, part := range parts {
			if !svgIdentifier.MatchString(part) {
				return false
			}
		}
		return true
	}
	return false
}

func boundedSVGNumber(value string) bool {
	if !svgNumber.MatchString(value) {
		return false
	}
	n, err := strconv.ParseFloat(value, 64)
	return err == nil && !math.IsInf(n, 0) && !math.IsNaN(n) && math.Abs(n) <= 100000
}

func brandingSVGDimensions(attrs []xml.Attr) (int, int, error) {
	var width, height float64
	for _, attr := range attrs {
		if attr.Name.Local == "viewBox" {
			parts := strings.Fields(strings.ReplaceAll(attr.Value, ",", " "))
			if len(parts) != 4 {
				return 0, 0, errors.New("invalid viewBox")
			}
			values := make([]float64, 4)
			for i, part := range parts {
				if !boundedSVGNumber(part) {
					return 0, 0, errors.New("invalid viewBox")
				}
				values[i], _ = strconv.ParseFloat(part, 64)
			}
			width, height = values[2], values[3]
		}
	}
	if width == 0 && height == 0 {
		for _, attr := range attrs {
			switch attr.Name.Local {
			case "width":
				width, _ = strconv.ParseFloat(strings.TrimSuffix(attr.Value, "px"), 64)
			case "height":
				height, _ = strconv.ParseFloat(strings.TrimSuffix(attr.Value, "px"), 64)
			}
		}
	}
	if width <= 0 || height <= 0 || width > 100000 || height > 100000 {
		return 0, 0, fmt.Errorf("invalid logo dimensions")
	}
	factor := math.Min(1, 1200/math.Max(width, height))
	return max(1, int(math.Round(width*factor))), max(1, int(math.Round(height*factor))), nil
}
