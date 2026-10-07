// Copyright 2026 肖其顿 (XIAO QI DUN)
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package pdfgo

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"unicode/utf8"

	"golang.org/x/text/encoding/charmap"
)

// Font 保存PDF字体程序、字符映射及字宽，不依赖渲染后端
// Vertical为true时使用竖排度量，字形本身仍以横排原点描述
// BoundingBox保留非Type3字体描述符边界，未提供有效边界时为空
// FallbackBoundingBox保留未内嵌标准字体的AFM保守边界，不用于排版，边界单位为千分之一文字空间
type Font struct {
	Name                string
	Subtype             Name
	Program             []byte
	ProgramType         Name
	Dictionary          Dictionary
	BoundingBox         *Rectangle
	FallbackBoundingBox *Rectangle
	Unicode             UnicodeMap
	Vertical            bool
	widths              map[uint32]float64
	defaultWidth        float64
	cidWidths           cidMetrics[float64]
	verticals           cidMetrics[VerticalMetrics]
	defaultVertical     [2]float64
	composite           bool
	encoding            Name
	cidMap              *cidCMap
	cidUnicode          UnicodeMap
	glyphMap            []byte
	glyphCount          uint32
	simpleCmap          []byte
	cmapEncoding        Name
	symbolic            bool
	post                *fontPostMapping
	cffGlyphs           map[uint32]uint16
	cffNames            map[uint32]string
	differences         map[uint32]string
	type3Matrix         Matrix
	type3Bounds         *Rectangle
	type3Procs          Dictionary
	type3Resources      Dictionary
	reader              *Reader
}

// Glyph 保存原始字符码、Unicode文本、字形名称、编号和千分之一字宽
// 缺少Unicode映射但具有字形编号或未定义字形时，Text为空，不推测字符含义
// CID保留复合字体字符标识，只有可确定内嵌字形编号时HasID才为true
// Vertical仅在竖排字体中有效，Width始终保留横排字宽
type Glyph struct {
	Name      string
	Code      uint32
	Text      string
	CID       uint16
	ID        uint16
	HasID     bool
	Width     float64
	Vertical  VerticalMetrics
	WordSpace bool
}

// ReadFont 读取字体资源及嵌入程序
// 入参: object 字体字典或引用
// 返回: *Font 字体资源, error 错误信息
func (r *Reader) ReadFont(object Object) (*Font, error) {
	return r.ReadFontContext(context.Background(), object)
}

// ReadFontContext 读取字体及映射，解码和度量展开使用本次取消上下文
// 入参: ctx 取消上下文, object 字体字典或引用
// 返回: *Font 完整字体资源, error 读取或取消错误
func (r *Reader) ReadFontContext(ctx context.Context, object Object) (*Font, error) {
	if ctx == nil {
		return nil, fmt.Errorf("invalid font context")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if r.closed {
		return nil, os.ErrClosed
	}
	ref, indirect := object.(Reference)
	if indirect && r.fonts[ref] != nil {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		return r.fonts[ref], nil
	}
	object, err := r.Resolve(object)
	if err != nil {
		return nil, err
	}
	dict, ok := object.(Dictionary)
	if !ok {
		return nil, fmt.Errorf("invalid font dictionary")
	}
	value, err := r.Resolve(dict["Subtype"])
	if err != nil {
		return nil, err
	}
	subtype, ok := value.(Name)
	if !ok {
		return nil, fmt.Errorf("missing font subtype")
	}
	font := &Font{Dictionary: dict, Subtype: subtype, widths: map[uint32]float64{}, reader: r}
	value, err = r.Resolve(dict["BaseFont"])
	if err != nil {
		return nil, err
	}
	if value != nil {
		name, ok := value.(Name)
		if !ok {
			return nil, fmt.Errorf("invalid base font name")
		}
		font.Name = string(name)
	}
	var descriptor Object
	boundsDeclared := false
	ignoreEncoding := false
	if subtype == "TrueType" {
		descriptor, err = r.Resolve(dict["FontDescriptor"])
		if err != nil {
			return nil, err
		}
		if descriptor != nil {
			d, ok := descriptor.(Dictionary)
			if !ok {
				return nil, fmt.Errorf("invalid font descriptor")
			}
			flags, err := r.fontFlags(d)
			if err != nil {
				return nil, err
			}
			font.symbolic = flags&4 != 0
			if font.symbolic {
				for _, key := range []Name{"FontFile2", "FontFile3", "FontFile"} {
					value, err := r.Resolve(d[key])
					if err != nil {
						return nil, err
					}
					ignoreEncoding = ignoreEncoding || value != nil
				}
			}
		}
	}
	metrics := dict
	var cidSubtype Name
	noEncoding := false
	if subtype == Name("Type0") {
		font.composite = true
		encoding, err := r.Resolve(dict["Encoding"])
		if err != nil {
			return nil, err
		}
		font.encoding, _ = encoding.(Name)
		font.cidMap, err = r.readCIDCMapContext(ctx, encoding, nil)
		if err != nil {
			return nil, err
		}
		font.Vertical = font.cidMap.vertical
		descendants, err := r.Resolve(dict["DescendantFonts"])
		if err != nil {
			return nil, err
		}
		array, ok := descendants.(Array)
		if !ok || len(array) != 1 {
			return nil, fmt.Errorf("invalid descendant fonts")
		}
		child, err := r.Resolve(array[0])
		if err != nil {
			return nil, err
		}
		metrics, ok = child.(Dictionary)
		if !ok {
			return nil, fmt.Errorf("invalid descendant font")
		}
		value, err := r.Resolve(metrics["Subtype"])
		if err != nil {
			return nil, err
		}
		cidSubtype, _ = value.(Name)
		if cidSubtype != Name("CIDFontType2") && cidSubtype != Name("CIDFontType0") {
			return nil, &UnsupportedError{Feature: "CID font subtype"}
		}
		font.defaultWidth = 1000
		value, err = r.Resolve(metrics["DW"])
		if err != nil {
			return nil, err
		}
		if value != nil {
			font.defaultWidth, err = r.number(value)
			if err != nil {
				return nil, err
			}
		}
		widths, err := r.Resolve(metrics["W"])
		if err != nil {
			return nil, err
		}
		if widths != nil {
			array, ok := widths.(Array)
			if !ok {
				return nil, fmt.Errorf("invalid CID widths")
			}
			for n := 0; n < len(array); {
				if err := ctx.Err(); err != nil {
					return nil, err
				}
				value, err := r.Resolve(array[n])
				if err != nil {
					return nil, err
				}
				start, ok := value.(Integer)
				if !ok || start < 0 || start > 65535 || n+1 >= len(array) {
					return nil, fmt.Errorf("invalid CID width range")
				}
				n++
				values, err := r.Resolve(array[n])
				if err != nil {
					return nil, err
				}
				n++
				if widths, ok := values.(Array); ok {
					if int64(len(widths))+int64(start) > 65536 {
						return nil, fmt.Errorf("excessive CID width range")
					}
					for j, value := range widths {
						if j&255 == 0 {
							if err := ctx.Err(); err != nil {
								return nil, err
							}
						}
						width, err := r.number(value)
						if err != nil {
							return nil, err
						}
						font.cidWidths.set(uint32(start)+uint32(j), uint32(start)+uint32(j), width)
					}
				} else {
					end, ok := values.(Integer)
					if !ok || end < start || end > 65535 || n >= len(array) {
						return nil, fmt.Errorf("invalid CID width range")
					}
					width, err := r.number(array[n])
					if err != nil {
						return nil, err
					}
					n++
					font.cidWidths.set(uint32(start), uint32(end), width)
				}
			}
		}
		if font.Vertical {
			if err := r.readVerticalMetricsContext(ctx, font, metrics); err != nil {
				return nil, err
			}
		}
		gidMap, err := r.Resolve(metrics["CIDToGIDMap"])
		if err != nil {
			return nil, err
		}
		if gidMap != nil && gidMap != Name("Identity") {
			stream, ok := gidMap.(*Stream)
			if !ok || stream == nil {
				return nil, fmt.Errorf("invalid CIDToGIDMap")
			}
			font.glyphMap, err = stream.DecodeContext(ctx)
			if err != nil {
				return nil, err
			}
			if len(font.glyphMap)%2 != 0 {
				return nil, fmt.Errorf("invalid CIDToGIDMap length")
			}
		}
	} else if subtype == Name("TrueType") || (subtype == Name("Type1") || subtype == Name("MMType1")) || subtype == Name("Type3") {
		var encoding Object
		if !ignoreEncoding {
			encoding, err = r.Resolve(dict["Encoding"])
			if err != nil {
				return nil, err
			}
		}
		noEncoding = encoding == nil
		if encoding != nil {
			switch value := encoding.(type) {
			case Name:
				font.encoding = value
			case Dictionary:
				base, err := r.Resolve(value["BaseEncoding"])
				if err != nil {
					return nil, err
				}
				if base != nil {
					name, ok := base.(Name)
					if !ok {
						return nil, fmt.Errorf("invalid font base encoding")
					}
					font.encoding = name
				}
				array, err := r.Resolve(value["Differences"])
				if err != nil {
					return nil, err
				}
				entries, ok := array.(Array)
				if array != nil && !ok {
					return nil, fmt.Errorf("invalid font encoding differences")
				}
				font.differences = map[uint32]string{}
				code := 256
				for _, entry := range entries {
					entry, err := r.Resolve(entry)
					if err != nil {
						return nil, err
					}
					switch v := entry.(type) {
					case Integer:
						if v < 0 || v > 255 {
							return nil, fmt.Errorf("invalid font difference code")
						}
						code = int(v)
					case Name:
						if code > 255 {
							return nil, fmt.Errorf("invalid font difference code")
						}
						font.differences[uint32(code)] = string(v)
						code++
					default:
						return nil, fmt.Errorf("invalid font encoding differences")
					}
				}
			default:
				return nil, fmt.Errorf("invalid font encoding")
			}
		}
		widths, err := r.Resolve(dict["Widths"])
		if err != nil {
			return nil, err
		}
		if widths != nil {
			array, ok := widths.(Array)
			if !ok {
				return nil, fmt.Errorf("invalid font widths")
			}
			first, err := r.number(dict["FirstChar"])
			if err != nil {
				return nil, err
			}
			if first < 0 || first > 255 || first != float64(int(first)) || int(first)+len(array) > 256 {
				return nil, fmt.Errorf("invalid simple font width range")
			}
			for n, value := range array {
				width, err := r.number(value)
				if err != nil {
					return nil, err
				}
				font.widths[uint32(first)+uint32(n)] = width
			}
		}
		if subtype == Name("Type3") {
			matrix, err := r.Resolve(dict["FontMatrix"])
			if err != nil {
				return nil, err
			}
			values, ok := matrix.(Array)
			if !ok || len(values) != 6 {
				return nil, fmt.Errorf("invalid Type3 font matrix")
			}
			for index, value := range values {
				font.type3Matrix[index], err = r.number(value)
				if err != nil {
					return nil, err
				}
			}
			if dict["FontBBox"] != nil {
				box, err := r.rectangle(dict["FontBBox"])
				if err != nil {
					return nil, err
				}
				font.type3Bounds = &box
			}
			procs, err := r.Resolve(dict["CharProcs"])
			if err != nil {
				return nil, err
			}
			font.type3Procs, ok = procs.(Dictionary)
			if !ok {
				return nil, fmt.Errorf("invalid Type3 character procedures")
			}
			resources, err := r.Resolve(dict["Resources"])
			if err != nil {
				return nil, err
			}
			if resources != nil {
				font.type3Resources, ok = resources.(Dictionary)
				if !ok {
					return nil, fmt.Errorf("invalid Type3 resources")
				}
			}
		}
	} else {
		return nil, &UnsupportedError{Feature: "font subtype " + string(subtype)}
	}
	if dict["ToUnicode"] != nil {
		font.Unicode, err = r.readUnicodeCMapContext(ctx, dict["ToUnicode"], nil)
		if err != nil {
			if cause := ctx.Err(); cause != nil {
				return nil, cause
			}
			if r.warning == nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) || errors.Is(err, os.ErrClosed) || errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
				return nil, err
			}
			font.Unicode = nil
			r.warning(Diagnostic{Message: fmt.Sprintf("PDF font %q ToUnicode unavailable: %v; glyph mapping retained", font.Name, err)})
		}
	}
	if subtype != "TrueType" {
		descriptor, err = r.Resolve(metrics["FontDescriptor"])
		if err != nil {
			return nil, err
		}
	}
	if descriptor != nil {
		d, ok := descriptor.(Dictionary)
		if !ok {
			return nil, fmt.Errorf("invalid font descriptor")
		}
		if subtype != "TrueType" {
			flags, err := r.fontFlags(d)
			if err != nil {
				return nil, err
			}
			font.symbolic = flags&4 != 0
		}
		boxObject := d["FontBBox"]
		if subtype == "Type3" {
			boxObject = nil
		}
		value, err := r.Resolve(boxObject)
		if err != nil {
			return nil, err
		}
		if subtype != "Type3" && value != nil {
			boundsDeclared = true
			array, ok := value.(Array)
			if !ok || len(array) != 4 {
				return nil, fmt.Errorf("invalid font bounding box")
			}
			var values [4]float64
			for i, value := range array {
				values[i], err = r.number(value)
				if err != nil {
					return nil, fmt.Errorf("font bounding box: %w", err)
				}
			}
			bounds := Rectangle{min(values[0], values[2]), min(values[1], values[3]), max(values[0], values[2]), max(values[1], values[3])}
			if bounds.XMin < bounds.XMax && bounds.YMin < bounds.YMax {
				font.BoundingBox = &bounds
			}
		}
		missingWidth := d["MissingWidth"]
		if font.composite {
			missingWidth = nil
		}
		value, err = r.Resolve(missingWidth)
		if err != nil {
			return nil, err
		}
		if !font.composite && value != nil {
			font.defaultWidth, err = r.number(value)
			if err != nil {
				return nil, err
			}
		}
		for _, key := range []Name{"FontFile2", "FontFile3", "FontFile"} {
			value, err := r.Resolve(d[key])
			if err != nil {
				return nil, err
			}
			if value == nil {
				continue
			}
			stream, ok := value.(*Stream)
			if !ok || stream == nil {
				return nil, fmt.Errorf("invalid font program")
			}
			font.ProgramType = key
			if key == Name("FontFile3") {
				value, err := r.Resolve(stream.Dictionary["Subtype"])
				if err != nil {
					return nil, err
				}
				font.ProgramType, ok = value.(Name)
				if !ok {
					return nil, fmt.Errorf("invalid font program subtype")
				}
			}
			font.Program, err = stream.DecodeContext(ctx)
			if err != nil {
				return nil, err
			}
			break
		}
	}
	if len(font.Program) >= 4 && font.Program[0] == 1 && font.Program[1] == 0 && font.Program[2] >= 4 && font.Program[3] >= 1 && font.Program[3] <= 4 {
		font.ProgramType = "Type1C"
		if font.composite {
			font.ProgramType = "CIDFontType0C"
		}
	}
	if (subtype == Name("Type1") || subtype == Name("MMType1")) && font.ProgramType == "FontFile" && font.encoding == "" {
		builtIn, err := type1BuiltInEncoding(font.Program)
		if err != nil {
			return nil, err
		}
		for code, name := range font.differences {
			builtIn[code] = name
		}
		font.differences = builtIn
	}
	var cffProgram []byte
	if font.ProgramType == "Type1C" || font.ProgramType == "CIDFontType0C" {
		cffProgram = font.Program
	} else if font.ProgramType == "OpenType" && len(font.Program) >= 4 && string(font.Program[:4]) == "OTTO" {
		cffProgram, err = fontTableContext(ctx, font.Program, "CFF ")
		if err != nil {
			return nil, err
		}
		if len(cffProgram) == 0 {
			return nil, &UnsupportedError{Feature: "OpenType font without CFF table"}
		}
	}
	if !font.composite && font.Subtype == Name("TrueType") && len(font.Program) > 0 && cffProgram == nil {
		font.symbolic = font.symbolic || noEncoding
		font.simpleCmap, font.cmapEncoding, err = fontCmapContext(ctx, font.Program, font.symbolic)
		if errors.Is(err, errFontCmapUnavailable) && !font.symbolic {
			glyphs, readErr := fontPostGlyphsContext(ctx, font.Program)
			if readErr != nil {
				return nil, readErr
			}
			if len(glyphs) != 0 {
				font.post = &fontPostMapping{program: font.Program, glyphs: glyphs, ready: true}
				err = nil
			}
		}
		if err != nil {
			return nil, err
		}
		if !font.symbolic && font.post == nil {
			font.post = &fontPostMapping{program: font.Program}
		}
	}
	if cffProgram != nil {
		if !font.composite && font.encoding != "" && font.encoding != "WinAnsiEncoding" && font.encoding != "MacRomanEncoding" && font.encoding != "MacExpertEncoding" && font.encoding != "StandardEncoding" {
			return nil, &UnsupportedError{Feature: "external CFF encoding"}
		}
		identity := font.composite && (cidSubtype == Name("CIDFontType0") || cidSubtype == Name("CIDFontType2") && font.glyphMap == nil)
		font.cffGlyphs, font.cffNames, err = cffFontMapping(cffProgram, font.composite, font.encoding, font.differences, identity, font.ProgramType)
		if err != nil {
			return nil, err
		}
	}
	if (font.composite && cidSubtype == "CIDFontType2" || font.simpleCmap != nil || font.post != nil) && len(font.Program) != 0 && cffProgram == nil {
		maxp, err := fontTableContext(ctx, font.Program, "maxp")
		if err != nil {
			return nil, err
		}
		if maxp != nil {
			if len(maxp) < 6 {
				return nil, fmt.Errorf("truncated TrueType maximum profile")
			}
			font.glyphCount = uint32(binary.BigEndian.Uint16(maxp[4:6]))
			if font.glyphCount == 0 {
				return nil, fmt.Errorf("invalid TrueType glyph count")
			}
		}
	}
	if font.composite && len(font.Program) != 0 && cidSubtype == Name("CIDFontType0") && font.cffGlyphs == nil {
		return nil, &UnsupportedError{Feature: "CID CFF glyph mapping without CFF program"}
	}
	if font.composite && metrics["CIDSystemInfo"] != nil {
		value, err := r.Resolve(metrics["CIDSystemInfo"])
		if err != nil {
			return nil, err
		}
		info, ok := value.(Dictionary)
		if !ok {
			return nil, fmt.Errorf("invalid CID system information")
		}
		registry, err := r.Resolve(info["Registry"])
		if err != nil {
			return nil, err
		}
		ordering, err := r.Resolve(info["Ordering"])
		if err != nil {
			return nil, err
		}
		reg, regOK := registry.(String)
		ord, ordOK := ordering.(String)
		if !regOK || !ordOK {
			return nil, fmt.Errorf("invalid CID character collection")
		}
		if font.cidMap.ordering != "" && font.cidMap.ordering != "Identity" && (font.cidMap.ordering != string(ord) || font.cidMap.registry != string(reg)) {
			return nil, fmt.Errorf("CMap and font character collections differ")
		}
		if string(reg) == "Adobe" {
			index, err := cmapResourceIndex()
			if err != nil {
				return nil, err
			}
			name := Name("Adobe-" + string(ord) + "-UCS2")
			if _, exists := index[name]; exists {
				font.cidUnicode, err = loadUnicodeCMapContext(ctx, name, nil)
				if err != nil {
					return nil, err
				}
			}
		}
	}
	if font.Subtype == "Type1" && len(font.Program) == 0 && !boundsDeclared {
		fonts, err := coreFonts()
		if err != nil {
			return nil, err
		}
		if metrics := fonts[font.Name]; metrics != nil && metrics.bounds != nil {
			bounds := *metrics.bounds
			font.FallbackBoundingBox = &bounds
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if indirect {
		if r.fonts == nil {
			r.fonts = map[Reference]*Font{}
		}
		r.fonts[ref] = font
	}
	return font, nil
}

// fontFlags 读取字体描述符的整数标志，支持间接对象
// 入参: descriptor 字体描述符
// 返回: int64 标志值, error 引用或类型错误
func (r *Reader) fontFlags(descriptor Dictionary) (int64, error) {
	value, err := r.Resolve(descriptor["Flags"])
	if err != nil || value == nil {
		return 0, err
	}
	flags, ok := value.(Integer)
	if !ok {
		return 0, fmt.Errorf("Flags is not an integer")
	}
	return int64(flags), nil
}

// Decode 解码文字串，未知Unicode映射不会以猜测文本替代
// 入参: data 原始文字字节
// 返回: []Glyph 字符信息, error 错误信息
func (f *Font) Decode(data []byte) ([]Glyph, error) {
	return f.DecodeContext(context.Background(), data)
}

// DecodeContext 解码文字串并检查取消，不返回不完整的字形列表
// 入参: ctx 取消上下文, data 原始文字字节
// 返回: []Glyph 字符信息, error 解码或取消错误
func (f *Font) DecodeContext(ctx context.Context, data []byte) ([]Glyph, error) {
	if ctx == nil {
		return nil, fmt.Errorf("invalid font context")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	var core *coreFontMetrics
	if f.Subtype == "Type1" && len(f.Program) == 0 {
		fonts, err := coreFonts()
		if err != nil {
			return nil, err
		}
		core = fonts[f.Name]
	}
	step := 1
	if f.composite {
		step = 2
	}
	if f.cidMap == nil && len(data)%step != 0 {
		return nil, fmt.Errorf("incomplete font character code")
	}
	glyphs := make([]Glyph, 0, len(data)/step)
	for n := 0; n < len(data); n += step {
		if len(glyphs)&255 == 0 {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
		}
		valid := true
		if f.cidMap != nil {
			step, valid = f.cidMap.next(data[n:])
		}
		raw := data[n : n+step]
		code := uint32(codeNumber(raw))
		cid := code
		if f.cidMap != nil {
			cid = uint32(f.cidMap.lookup(raw, valid))
		}
		if f.composite && len(f.Program) != 0 && !f.hasCIDGlyph(cid) {
			if f.cidMap != nil {
				cid = uint32(f.cidMap.undefined(raw))
			} else {
				cid = 0
			}
			if !f.hasCIDGlyph(cid) {
				cid = 0
			}
		}
		text, ok := f.Unicode[string(raw)]
		if !ok && f.cidUnicode != nil {
			text, ok = f.cidUnicode[string([]byte{byte(cid >> 8), byte(cid)})]
		}
		name := ""
		if !f.composite && !(f.simpleCmap != nil && f.symbolic) {
			var mapped bool
			name, mapped = f.differences[code]
			if f.cffNames != nil {
				name, mapped = f.cffNames[code]
			}
			if !mapped && f.encoding == "" && (f.cffNames != nil || f.Subtype == "Type3") {
				name, mapped = ".notdef", true
			}
			if !mapped {
				encoding := pdfStandardNames
				if core != nil && f.encoding == "" {
					encoding = core.names[:]
				}
				switch f.encoding {
				case Name("WinAnsiEncoding"):
					encoding = pdfWinAnsiNames
				case Name("MacRomanEncoding"):
					encoding = pdfMacRomanNames
				case Name("MacExpertEncoding"):
					encoding = pdfMacExpertNames[:]
				case "", Name("StandardEncoding"):
				default:
					encoding = nil
				}
				if int(code) < len(encoding) {
					name = encoding[code]
				}
				if name == "" {
					name = ".notdef"
				}
			}
		}
		if !ok && valid && f.cidMap != nil {
			text, ok = predefinedCodeUnicode(f.encoding, raw)
		}
		if !ok && !f.composite {
			if name != "" && name != ".notdef" {
				if f.Name == "ZapfDingbats" {
					text, ok = glyphNameUnicodeMap(name, dingbatGlyphNames())
				} else {
					text, ok = glyphNameUnicode(name)
				}
			}
		}
		if core != nil {
			if _, exists := core.widths[name]; !exists {
				name = ".notdef"
			}
		}
		if name == ".notdef" && (f.Subtype == "Type1" || f.Subtype == "MMType1") {
			ok = true
		}
		if !ok && f.cffGlyphs != nil {
			ok = true
		}
		if !ok && (f.Subtype == "Type1" || f.Subtype == "MMType1") && len(f.Program) != 0 {
			ok = true
		}
		if !ok && (f.simpleCmap != nil || f.post != nil) {
			ok = true
		}
		if !ok && f.composite && len(f.Program) != 0 && f.cffGlyphs == nil {
			ok = true
		}
		if !ok {
			if f.composite {
				return nil, &UnsupportedError{Feature: "CID without Unicode mapping"}
			}
			if f.Subtype == Name("Type3") {
				text = ""
			} else if f.encoding == Name("WinAnsiEncoding") {
				text = string(charmap.Windows1252.DecodeByte(raw[0]))
			} else if core == nil && raw[0] >= 32 && raw[0] <= 126 && (f.encoding == "" || f.encoding == Name("StandardEncoding")) {
				text = string(rune(raw[0]))
			} else {
				return nil, &UnsupportedError{Feature: "unmapped simple font character"}
			}
		}
		width, ok := f.widths[cid]
		if f.composite {
			width, ok = f.cidWidths.get(cid)
		}
		if !ok {
			width = f.defaultWidth
			if len(f.widths) == 0 && core != nil {
				if standardWidth, found := core.widths[name]; found {
					width = standardWidth
					ok = true
				}
			}
		}
		if f.Subtype == Name("Type3") {
			width *= f.type3Matrix[0] * 1000
		}
		if !ok && width == 0 && len(f.widths) == 0 && !f.composite && (core == nil || name != ".notdef") {
			return nil, &UnsupportedError{Feature: "unembedded standard font metrics"}
		}
		glyph := Glyph{Code: code, Text: text, Width: width, WordSpace: step == 1 && code == 32}
		if f.Vertical {
			var ok bool
			glyph.Vertical, ok = f.verticals.get(cid)
			if !ok {
				glyph.Vertical = VerticalMetrics{Advance: f.defaultVertical[1], Origin: Point{width / 2, f.defaultVertical[0]}}
			}
		}
		if f.composite {
			glyph.CID = uint16(cid)
		}
		if f.Subtype == Name("Type1") || f.Subtype == Name("MMType1") {
			glyph.Name = name
		}
		if f.Subtype == Name("Type3") {
			glyph.Name = name
		}
		if f.simpleCmap != nil || f.post != nil {
			id, err := f.simpleGlyphContext(ctx, code, name)
			if err != nil {
				return nil, err
			}
			if f.glyphCount != 0 && uint32(id) >= f.glyphCount {
				id = 0
			}
			glyph.ID = id
			glyph.HasID = true
		}
		if f.composite && len(f.Program) != 0 {
			glyph.HasID = true
			glyph.ID = uint16(cid)
			if f.glyphMap != nil {
				if int(cid)*2+1 >= len(f.glyphMap) {
					glyph.ID = 0
				} else {
					glyph.ID = uint16(f.glyphMap[cid*2])<<8 | uint16(f.glyphMap[cid*2+1])
				}
			}
			if !f.hasCIDGlyph(cid) {
				glyph.ID = 0
			}
		}
		if f.cffGlyphs != nil {
			glyph.ID, glyph.HasID = f.cffGlyphs[cid], true
			if mapped, ok := f.cffNames[cid]; ok {
				glyph.Name = mapped
			} else if !f.composite {
				glyph.Name = name
			}
		}
		glyphs = append(glyphs, glyph)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return glyphs, nil
}

// hasCIDGlyph 检查内嵌字体是否提供指定CID的有效字形
// 入参: cid 字符标识
// 返回: bool 是否提供字形
func (f *Font) hasCIDGlyph(cid uint32) bool {
	if f.cffGlyphs != nil {
		_, ok := f.cffGlyphs[cid]
		return ok
	}
	if f.glyphMap != nil {
		if int(cid)*2+1 >= len(f.glyphMap) {
			return false
		}
		id := uint32(binary.BigEndian.Uint16(f.glyphMap[cid*2:]))
		return id != 0 && (f.glyphCount == 0 || id < f.glyphCount)
	}
	return f.glyphCount == 0 || cid < f.glyphCount
}

// predefinedCodeUnicode 还原以Unicode编码命名的官方CMap字符，不推测其他编码
// 入参: name 官方编码名称, raw 字符码
// 返回: string Unicode文本, bool 是否为有效Unicode编码
func predefinedCodeUnicode(name Name, raw []byte) (string, bool) {
	encoding := string(name)
	if !strings.HasPrefix(encoding, "Uni") {
		return "", false
	}
	if strings.Contains(encoding, "-UCS2-") || strings.Contains(encoding, "-UTF16-") {
		value, err := unicodeBytes(raw)
		return value, err == nil
	}
	if strings.Contains(encoding, "-UTF8-") {
		return string(raw), utf8.Valid(raw)
	}
	if strings.Contains(encoding, "-UTF32-") && len(raw) == 4 {
		value := rune(codeNumber(raw))
		return string(value), utf8.ValidRune(value)
	}
	return "", false
}
