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
	"math"
	"os"
	"strings"
	"unicode/utf16"
	"unicode/utf8"
)

// maxFontCacheEntries 限制单个Reader常驻缓存的字体最大条数
const maxFontCacheEntries = 256

// maxFontCacheBytes 限制单个Reader常驻缓存的字体总字节上限
const maxFontCacheBytes int64 = 128 << 20

// Font 保存PDF字体程序、字符映射及字宽（不依赖渲染后端），可由阅读器缓存共享，所有字段及切片均只读
// Vertical为true时采用竖排度量（字形仍以横排原点描述）；Recovered表示已诊断恢复，Dictionary仍保留原始声明
// BoundingBox为非Type3描述符边界，FallbackBoundingBox为未内嵌标准字体AFM保守边界（千分之一文字空间，不用于排版）
type Font struct {
	Name                 string
	Subtype              Name
	Program              []byte
	ProgramType          Name
	Dictionary           Dictionary
	BoundingBox          *Rectangle
	FallbackBoundingBox  *Rectangle
	Unicode              UnicodeMap
	Vertical             bool
	Recovered            bool
	widths               map[uint32]float64
	defaultWidth         float64
	defaultWidthDeclared bool
	allowMissingWidth    bool
	programMetrics       *fontProgramMetrics
	ttfMetrics           *ttfFontMetrics
	coreName             string
	cidWidths            cidMetrics[float64]
	verticals            cidMetrics[VerticalMetrics]
	defaultVertical      [2]float64
	composite            bool
	encoding             Name
	cidMap               *cidCMap
	cidUnicode           UnicodeMap
	glyphMap             []byte
	glyphCount           uint32
	simpleCmap           []byte
	cmapEncoding         Name
	cmapRecovered        bool
	symbolic             bool
	post                 *fontPostMapping
	cffGlyphs            map[uint32]uint16
	cffNames             map[uint32]string
	differences          map[uint32]string
	type3Matrix          Matrix
	type3Bounds          *Rectangle
	type3Procs           Dictionary
	type3ProcState       map[Name]uint8
	type3Resources       Dictionary
	reader               *Reader
	cache                *objectCache
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

// isPredefinedUnicodeCMap 判断名称是否属于以Unicode编码命名的官方CMap
// 入参: name 官方编码名称
// 返回: bool 是否为预定义Unicode CMap
func isPredefinedUnicodeCMap(name Name) bool {
	encoding := string(name)
	return strings.HasPrefix(encoding, "Uni") && (strings.Contains(encoding, "-UCS2-") || strings.Contains(encoding, "-UTF16-") || strings.Contains(encoding, "-UTF8-") || strings.Contains(encoding, "-UTF32-"))
}

// predefinedUnicodeCode 将单字符按以Unicode编码命名的官方CMap反向编码为原始字符码
// 入参: name 官方编码名称, ch Unicode字符
// 返回: string 原始字符码, bool 是否为有效Unicode编码
func predefinedUnicodeCode(name Name, ch rune) (string, bool) {
	if !utf8.ValidRune(ch) {
		return "", false
	}
	encoding := string(name)
	if !strings.HasPrefix(encoding, "Uni") {
		return "", false
	}
	if strings.Contains(encoding, "-UCS2-") {
		if ch > 0xFFFF {
			return "", false
		}
		return string([]byte{byte(ch >> 8), byte(ch)}), true
	}
	if strings.Contains(encoding, "-UTF16-") {
		if ch <= 0xFFFF {
			return string([]byte{byte(ch >> 8), byte(ch)}), true
		}
		r1, r2 := utf16.EncodeRune(ch)
		return string([]byte{byte(r1 >> 8), byte(r1), byte(r2 >> 8), byte(r2)}), true
	}
	if strings.Contains(encoding, "-UTF8-") {
		var buf [utf8.UTFMax]byte
		n := utf8.EncodeRune(buf[:], ch)
		return string(buf[:n]), true
	}
	if strings.Contains(encoding, "-UTF32-") {
		return string([]byte{byte(ch >> 24), byte(ch >> 16), byte(ch >> 8), byte(ch)}), true
	}
	return "", false
}

// ReadFont 读取字体资源及嵌入程序
// 入参: object 字体字典或引用
// 返回: *Font 字体资源, error 错误信息
func (r *Reader) ReadFont(object Object) (*Font, error) {
	return r.ReadFontContext(context.Background(), object)
}

// ReadFontContext 读取字体及映射，解码和度量展开使用本次取消上下文
// 入参: ctx 取消上下文, object 字体字典或引用
// 返回: *Font 完整字体资源, error 读取、超限或取消错误
func (r *Reader) ReadFontContext(ctx context.Context, object Object) (*Font, error) {
	if r == nil || ctx == nil {
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
	font := &Font{Dictionary: dict, Subtype: subtype, widths: map[uint32]float64{}, reader: r, cache: r.cache}
	value, err = r.Resolve(dict["BaseFont"])
	if err != nil {
		return nil, err
	}
	invalidName := false
	if value != nil {
		name, ok := value.(Name)
		if !ok {
			if r.warning == nil {
				return nil, fmt.Errorf("invalid base font name")
			}
			invalidName = true
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
	invalidGIDMap := false
	noEncoding := false
	if subtype == Name("Type0") {
		font.composite = true
		encoding, err := r.Resolve(dict["Encoding"])
		if err != nil {
			return nil, err
		}
		font.encoding, _ = encoding.(Name)
		font.cidMap, err = r.readCIDCMapContext(ctx, dict["Encoding"])
		if err != nil {
			return nil, err
		}
		font.Vertical = font.cidMap.vertical
		font.Recovered = font.cidMap.recovered
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
			var builder cidMetricBuilder[float64]
			entries := 0
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
					if len(widths) > maxCIDMetricEntries-entries {
						return nil, fmt.Errorf("%w: CID width entry limit exceeded", ErrStreamLimitExceeded)
					}
					entries += len(widths)
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
						builder.add(uint32(start)+uint32(j), uint32(start)+uint32(j), width)
					}
				} else {
					end, ok := values.(Integer)
					if !ok || end < start || end > 65535 || n >= len(array) {
						return nil, fmt.Errorf("invalid CID width range")
					}
					if entries >= maxCIDMetricEntries {
						return nil, fmt.Errorf("%w: CID width entry limit exceeded", ErrStreamLimitExceeded)
					}
					entries++
					width, err := r.number(array[n])
					if err != nil {
						return nil, err
					}
					n++
					builder.add(uint32(start), uint32(end), width)
				}
			}
			font.cidWidths, err = builder.finish(ctx)
			if err != nil {
				return nil, err
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
		_, gidStream := gidMap.(*Stream)
		if gidMap != nil && gidMap != Name("Identity") && !gidStream && r.warning != nil && cidSubtype == "CIDFontType2" {
			invalidGIDMap = true
		} else if gidMap != nil && gidMap != Name("Identity") {
			stream, ok := gidMap.(*Stream)
			if !ok || stream == nil {
				return nil, fmt.Errorf("invalid CIDToGIDMap")
			}
			font.glyphMap, err = r.DecodeStreamContext(ctx, stream)
			if err != nil {
				return nil, err
			}
			if len(font.glyphMap)%2 != 0 {
				return nil, fmt.Errorf("invalid CIDToGIDMap length")
			}
			if font.glyphMap == nil {
				font.glyphMap = []byte{}
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
			if first < 0 || first > 255 || first != float64(int(first)) {
				return nil, fmt.Errorf("invalid simple font width range")
			}
			if int(first)+len(array) > 256 {
				if r.warning == nil {
					return nil, fmt.Errorf("invalid simple font width range")
				}
				array = array[:256-int(first)]
				font.Recovered = true
				r.warning(Diagnostic{Message: fmt.Sprintf("PDF font %q Widths exceeds 256 entries; truncated to 256", font.Name)})
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
			boxValue, err := r.Resolve(dict["FontBBox"])
			if err != nil {
				if r.warning == nil || isFatalReaderError(err) {
					return nil, err
				}
				font.Recovered = true
				r.warning(Diagnostic{Message: fmt.Sprintf("PDF font %q invalid FontBBox ignored: %v", font.Name, err)})
			} else if boxValue != nil {
				box, err := r.rectangle(boxValue)
				if err != nil {
					if r.warning == nil || isFatalReaderError(err) {
						return nil, err
					}
					font.Recovered = true
					r.warning(Diagnostic{Message: fmt.Sprintf("PDF font %q invalid FontBBox ignored: %v", font.Name, err)})
				} else {
					font.type3Bounds = &box
				}
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
		font.Unicode, err = r.readUnicodeCMapContext(ctx, dict["ToUnicode"], nil, nil, 0)
		if err != nil {
			if cause := ctx.Err(); cause != nil {
				return nil, cause
			}
			if r.warning == nil || isFatalReaderError(err) {
				return nil, err
			}
			font.Unicode = nil
			font.Recovered = true
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
			if r.warning == nil || isFatalReaderError(err) {
				return nil, err
			}
			font.Recovered = true
			r.warning(Diagnostic{Message: fmt.Sprintf("PDF font %q invalid FontBBox ignored: %v", font.Name, err)})
		}
		if subtype != "Type3" && value != nil {
			array, ok := value.(Array)
			if !ok || len(array) != 4 {
				if r.warning == nil {
					return nil, fmt.Errorf("invalid font bounding box")
				}
				font.Recovered = true
				r.warning(Diagnostic{Message: fmt.Sprintf("PDF font %q invalid FontBBox ignored", font.Name)})
			} else {
				var values [4]float64
				var boxErr error
				for i, value := range array {
					values[i], boxErr = r.number(value)
					if boxErr != nil {
						break
					}
				}
				if boxErr != nil {
					if r.warning == nil || isFatalReaderError(boxErr) {
						return nil, fmt.Errorf("font bounding box: %w", boxErr)
					}
					font.Recovered = true
					r.warning(Diagnostic{Message: fmt.Sprintf("PDF font %q invalid FontBBox ignored: %v", font.Name, boxErr)})
				} else {
					boundsDeclared = true
					bounds := Rectangle{min(values[0], values[2]), min(values[1], values[3]), max(values[0], values[2]), max(values[1], values[3])}
					if bounds.XMin < bounds.XMax && bounds.YMin < bounds.YMax {
						font.BoundingBox = &bounds
					}
				}
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
			font.defaultWidthDeclared = true
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
			font.Program, err = r.DecodeStreamContext(ctx, stream)
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
	if len(font.Program) >= 12 && (string(font.Program[:4]) == "OTTO" || string(font.Program[:4]) == "\x00\x01\x00\x00" || string(font.Program[:4]) == "true") {
		actual := Name("FontFile2")
		if string(font.Program[:4]) == "OTTO" {
			actual = "OpenType"
		}
		mismatch := font.ProgramType != actual && !(actual == "FontFile2" && font.ProgramType == "OpenType") || font.composite && cidSubtype == "CIDFontType0" && actual == "FontFile2"
		if mismatch {
			if r.warning == nil {
				return nil, fmt.Errorf("font program container differs from declared subtype")
			}
			if _, err := fontTableContext(ctx, font.Program, "maxp"); err != nil {
				return nil, err
			}
			font.ProgramType, font.Recovered = actual, true
			if font.composite && actual == "FontFile2" {
				cidSubtype = "CIDFontType2"
			}
			r.warning(Diagnostic{Message: fmt.Sprintf("PDF font %q program container recovered as %s; original glyph IDs retained", font.Name, actual)})
		}
	}
	if invalidName {
		if len(font.Program) == 0 || subtype == "Type3" {
			return nil, fmt.Errorf("invalid base font name without embedded glyph program")
		}
		font.Recovered = true
		r.warning(Diagnostic{Message: "invalid PDF base font name; embedded glyph program retained"})
	}
	if (subtype == Name("Type1") || subtype == Name("MMType1")) && font.ProgramType == "FontFile" && font.encoding == "" {
		builtIn, err := type1BuiltInEncoding(ctx, font.Program)
		if err != nil {
			return nil, err
		}
		for code, name := range font.differences {
			builtIn[code] = name
		}
		font.differences = builtIn
	}
	if (subtype == "Type1" || subtype == "MMType1") && font.ProgramType == "FontFile" {
		font.programMetrics = &fontProgramMetrics{program: font.Program, type1: true}
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
	trueTypeContainer := font.Subtype == Name("TrueType") || font.ProgramType == Name("FontFile2") || font.ProgramType == Name("OpenType") && len(font.Program) >= 12 && (string(font.Program[:4]) == "\x00\x01\x00\x00" || string(font.Program[:4]) == "true")
	if !font.composite && font.Subtype != Name("Type3") && len(font.Program) > 0 && cffProgram == nil && trueTypeContainer {
		if font.Subtype == Name("TrueType") {
			font.symbolic = font.symbolic || noEncoding
		}
		cmapWarning := r.warning
		if cmapWarning != nil {
			cmapWarning = func(d Diagnostic) {
				font.Recovered = true
				font.cmapRecovered = true
				r.warning(d)
			}
		}
		font.simpleCmap, font.cmapEncoding, err = fontCmapContext(ctx, font.Program, font.symbolic, cmapWarning)
		if errors.Is(err, errFontCmapUnavailable) && !font.symbolic {
			glyphs, readErr := fontPostGlyphsContext(ctx, font.Program)
			if readErr != nil {
				return nil, readErr
			}
			if len(glyphs) != 0 {
				font.post = &fontPostMapping{program: font.Program, glyphs: glyphs, ready: true}
				err = nil
			} else if noEncoding {
				symCmap, symEnc, symErr := fontCmapContext(ctx, font.Program, true, cmapWarning)
				if symErr == nil {
					font.symbolic = true
					font.simpleCmap, font.cmapEncoding, err = symCmap, symEnc, nil
				} else if cause := ctx.Err(); cause != nil {
					return nil, cause
				}
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
		font.cffGlyphs, font.cffNames, err = cffFontMappingWithGlyphMap(ctx, cffProgram, font.composite, font.encoding, font.differences, font.ProgramType, font.glyphMap)
		if err != nil {
			return nil, err
		}
		if !font.composite && len(font.widths) == 0 && !font.defaultWidthDeclared {
			font.programMetrics = &fontProgramMetrics{program: cffProgram}
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
	if !font.composite && len(font.Program) != 0 && cffProgram == nil && font.ProgramType != "FontFile" && (font.simpleCmap != nil || font.post != nil) && len(font.widths) == 0 && !font.defaultWidthDeclared && r.warning != nil {
		metrics, err := readTTFFontMetrics(ctx, font.Program)
		if cause := ctx.Err(); cause != nil {
			return nil, cause
		}
		if err == nil {
			font.ttfMetrics = metrics
			font.Recovered = true
			r.warning(Diagnostic{Message: fmt.Sprintf("PDF font %q missing Widths; using embedded TrueType hmtx metrics", font.Name)})
		}
	}
	if font.composite && len(font.Program) != 0 && cidSubtype == Name("CIDFontType0") && font.cffGlyphs == nil {
		return nil, &UnsupportedError{Feature: "CID CFF glyph mapping without CFF program"}
	}
	if invalidGIDMap {
		font.Recovered = true
		r.warning(Diagnostic{Message: fmt.Sprintf("PDF font %q invalid non-stream CIDToGIDMap; default Identity mapping retained", font.Name)})
	}
	if font.composite && metrics["CIDSystemInfo"] != nil {
		hasEmbeddedCID := len(font.Program) != 0 && (font.cffGlyphs != nil || font.glyphCount != 0)
		value, err := r.Resolve(metrics["CIDSystemInfo"])
		if err != nil {
			if r.warning == nil || !hasEmbeddedCID || isFatalReaderError(err) {
				return nil, err
			}
			font.Recovered = true
			r.warning(Diagnostic{Message: fmt.Sprintf("PDF font %q invalid CIDSystemInfo ignored: %v; embedded CID glyph mapping retained", font.Name, err)})
		}
		if value != nil {
			info, ok := value.(Dictionary)
			if !ok {
				if r.warning == nil || !hasEmbeddedCID {
					return nil, fmt.Errorf("invalid CID system information")
				}
				font.Recovered = true
				r.warning(Diagnostic{Message: fmt.Sprintf("PDF font %q invalid CIDSystemInfo ignored; embedded CID glyph mapping retained", font.Name)})
			} else {
				registry, regErr := r.Resolve(info["Registry"])
				if regErr != nil && (r.warning == nil || !hasEmbeddedCID || isFatalReaderError(regErr)) {
					return nil, regErr
				}
				ordering, ordErr := r.Resolve(info["Ordering"])
				if ordErr != nil && (r.warning == nil || !hasEmbeddedCID || isFatalReaderError(ordErr)) {
					return nil, ordErr
				}
				if regErr != nil || ordErr != nil {
					font.Recovered = true
					r.warning(Diagnostic{Message: fmt.Sprintf("PDF font %q invalid CIDSystemInfo ignored; embedded CID glyph mapping retained", font.Name)})
				} else {
					reg, regOK := registry.(String)
					ord, ordOK := ordering.(String)
					if !regOK || !ordOK {
						if r.warning == nil || !hasEmbeddedCID {
							return nil, fmt.Errorf("invalid CID character collection")
						}
						font.Recovered = true
						r.warning(Diagnostic{Message: fmt.Sprintf("PDF font %q invalid CID character collection ignored; embedded CID glyph mapping retained", font.Name)})
					} else {
						mismatch := font.cidMap.ordering != "" && font.cidMap.ordering != "Identity" && (font.cidMap.ordering != string(ord) || font.cidMap.registry != string(reg))
						if mismatch {
							if r.warning == nil || !hasEmbeddedCID {
								return nil, fmt.Errorf("CMap and font character collections differ")
							}
							font.Recovered = true
							r.warning(Diagnostic{Message: fmt.Sprintf("PDF font %q CMap and font character collections differ; embedded CID glyph mapping retained without collection Unicode inference", font.Name)})
						}
						if !mismatch && string(reg) == "Adobe" {
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
				}
			}
		} else if err == nil {
			if r.warning == nil || !hasEmbeddedCID {
				return nil, fmt.Errorf("invalid CID system information")
			}
			font.Recovered = true
			r.warning(Diagnostic{Message: fmt.Sprintf("PDF font %q invalid CIDSystemInfo ignored; embedded CID glyph mapping retained", font.Name)})
		}
	}
	if !font.composite && len(font.Program) == 0 && len(font.widths) == 0 && !font.defaultWidthDeclared && r.warning != nil {
		if name := coreFontAlias(font.Name); name != "" {
			font.coreName, font.Recovered = name, true
			r.warning(Diagnostic{Message: fmt.Sprintf("PDF font %q missing widths recovered from existing Base14 %s metrics; source font name retained", font.Name, name)})
		}
		isCore := false
		if font.Subtype == "Type1" || font.coreName != "" {
			fonts, err := coreFonts()
			if err != nil {
				return nil, err
			}
			name := font.Name
			if font.coreName != "" {
				name = font.coreName
			}
			isCore = fonts[name] != nil
		}
		if !isCore {
			font.allowMissingWidth = true
			font.Recovered = true
			r.warning(Diagnostic{Message: fmt.Sprintf("PDF font %q missing Widths without embedded program; standard MissingWidth=0 used", font.Name)})
		}
	}
	if (font.Subtype == "Type1" || font.coreName != "") && len(font.Program) == 0 && !boundsDeclared {
		fonts, err := coreFonts()
		if err != nil {
			return nil, err
		}
		name := font.Name
		if font.coreName != "" {
			name = font.coreName
		}
		if metrics := fonts[name]; metrics != nil && metrics.bounds != nil {
			bounds := *metrics.bounds
			font.FallbackBoundingBox = &bounds
		}
	}
	if font.Subtype == Name("Type3") && len(font.type3Procs) != 0 {
		state := make(map[Name]uint8)
		var checked map[Name]bool
		for code := 0; code < 256; code++ {
			name := Name(font.simpleGlyphName(uint32(code), nil))
			if name == "" || name == ".notdef" || checked[name] {
				continue
			}
			if checked == nil {
				checked = make(map[Name]bool)
			}
			checked[name] = true
			proc := font.type3Procs[name]
			if proc == nil {
				continue
			}
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			resolved, err := r.Resolve(proc)
			if err != nil {
				if isFatalReaderError(err) {
					return nil, err
				}
				state[name] = 2
				continue
			}
			if resolved == nil {
				continue
			}
			if stream, ok := resolved.(*Stream); ok && stream != nil {
				state[name] = 1
			} else {
				state[name] = 2
			}
		}
		if len(state) != 0 {
			font.type3ProcState = state
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if indirect {
		r.cacheFont(ref, font)
	}
	return font, nil
}

// fontMemoryBytes 估算单个已解析字体对象的近似常驻内存字节数
// 入参: f 字体资源
// 返回: int64 估算字节数
func fontMemoryBytes(f *Font) int64 {
	if f == nil {
		return 0
	}
	cost := int64(512) + int64(len(f.Name)) + int64(len(f.Subtype)) + int64(len(f.Program)) + int64(len(f.ProgramType)) + int64(len(f.coreName)) + int64(len(f.encoding)) + int64(len(f.cmapEncoding)) + int64(len(f.glyphMap)) + int64(len(f.simpleCmap))
	cost += int64(len(f.Dictionary)+len(f.type3Procs)+len(f.type3Resources)) * 64
	cost += int64(len(f.widths))*24 + int64(len(f.type3ProcState))*24 + int64(len(f.cidWidths.ranges))*24 + int64(len(f.verticals.ranges))*40 + int64(len(f.cffGlyphs))*16
	for k, v := range f.Unicode {
		cost += 64 + int64(len(k)) + int64(len(v))
	}
	for k, v := range f.cidUnicode {
		cost += 64 + int64(len(k)) + int64(len(v))
	}
	for _, name := range f.cffNames {
		cost += 48 + int64(len(name))
	}
	for _, name := range f.differences {
		cost += 48 + int64(len(name))
	}
	for current := f.cidMap; current != nil; current = current.base {
		cost += 512 + int64(len(current.ranges))*32 + int64(len(current.spaces))*16 + int64(len(current.notdef))*32 + int64(len(current.registry)) + int64(len(current.ordering))
		for _, bucket := range current.spaceByFirst {
			cost += int64(len(bucket)) * 2
		}
		if current.spaceSecondMask != nil {
			cost += 8192
		}
		for _, sb := range current.spaceSecond {
			if sb == nil {
				continue
			}
			cost += 6144
			for _, sub := range sb.bySecond {
				cost += int64(len(sub)) * 2
			}
		}
	}
	if f.ttfMetrics != nil {
		cost += 64 + int64(len(f.ttfMetrics.hmtx))
	}
	if f.post != nil {
		f.post.mu.RLock()
		cost += 64 + int64(len(f.post.glyphs))*48
		f.post.mu.RUnlock()
	}
	if f.programMetrics != nil {
		cost += 128
	}
	return cost
}

// cacheFont 在条目数与总字节上限内缓存已解析字体，超限时清空旧缓存
// 入参: ref 字体间接引用, font 已解析字体
func (r *Reader) cacheFont(ref Reference, font *Font) {
	cost := fontMemoryBytes(font)
	if cost > maxFontCacheBytes {
		return
	}
	if r.fonts == nil {
		r.fonts = make(map[Reference]*Font)
	} else if len(r.fonts) >= maxFontCacheEntries || cost > maxFontCacheBytes-r.fontCacheBytes {
		clear(r.fonts)
		r.fontCacheBytes = 0
	}
	r.fonts[ref] = font
	r.fontCacheBytes += cost
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
	if f == nil || ctx == nil {
		return nil, fmt.Errorf("invalid font context")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if len(data) == 0 {
		return nil, nil
	}
	var type1Metrics *type1FontMetrics
	var cffMetrics *cffFontMetrics
	if f.programMetrics != nil && len(f.widths) == 0 && !f.defaultWidthDeclared {
		var err error
		type1Metrics, cffMetrics, err = f.programMetrics.load(ctx)
		if err != nil {
			return nil, err
		}
	}
	var core *coreFontMetrics
	if (f.Subtype == "Type1" || f.coreName != "") && len(f.Program) == 0 {
		fonts, err := coreFonts()
		if err != nil {
			return nil, err
		}
		name := f.Name
		if f.coreName != "" {
			name = f.coreName
		}
		core = fonts[name]
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
			var err error
			step, valid, err = f.cidMap.nextContext(ctx, data[n:])
			if err != nil {
				return nil, err
			}
		}
		raw := data[n : n+step]
		code := uint32(codeNumber(raw))
		cid := code
		if f.cidMap != nil {
			if err := f.cidMap.validateCode(raw, valid); err != nil {
				return nil, err
			}
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
			name = f.simpleGlyphName(code, core)
		}
		if type1Metrics != nil {
			if _, exists := type1Metrics.glyphs[name]; !exists {
				name = ".notdef"
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
		if core != nil && name != "" {
			if _, exists := core.widths[name]; !exists {
				name = ".notdef"
			}
		}
		if name == ".notdef" && !f.composite {
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
			} else {
				return nil, &UnsupportedError{Feature: "unmapped simple font character"}
			}
		}
		var glyphID uint16
		if f.ttfMetrics != nil {
			var err error
			glyphID, err = f.resolvedSimpleGlyphContext(ctx, code, name)
			if err != nil {
				return nil, err
			}
		}
		width, ok := f.widths[cid]
		if f.composite {
			width, ok = f.cidWidths.get(cid)
		}
		if !ok {
			width = f.defaultWidth
			if type1Metrics != nil {
				embeddedWidth, err := type1Metrics.glyphWidth(ctx, name)
				if err != nil {
					return nil, err
				}
				width, ok = embeddedWidth, true
			}
			if cffMetrics != nil {
				embeddedWidth, err := cffMetrics.glyphWidth(ctx, f.cffGlyphs[cid])
				if err != nil {
					return nil, err
				}
				width, ok = embeddedWidth, true
			}
			if f.ttfMetrics != nil {
				width, ok = f.ttfMetrics.glyphWidth(glyphID), true
			}
			if len(f.widths) == 0 && core != nil {
				if standardWidth, found := core.widths[name]; found {
					width = standardWidth
					ok = true
				}
			}
		}
		if f.Subtype == Name("Type3") {
			if f.type3Matrix[1] != 0 {
				width *= math.Hypot(f.type3Matrix[0], f.type3Matrix[1]) * 1000
			} else {
				width *= f.type3Matrix[0] * 1000
			}
		}
		if !ok && width == 0 && len(f.widths) == 0 && !f.composite && !f.defaultWidthDeclared && !f.allowMissingWidth && (core == nil || name != ".notdef") {
			if len(f.Program) != 0 {
				return nil, &UnsupportedError{Feature: "embedded font metrics"}
			}
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
		if f.ttfMetrics != nil {
			glyph.ID, glyph.HasID = glyphID, true
		} else if f.simpleCmap != nil || f.post != nil {
			id, err := f.resolvedSimpleGlyphContext(ctx, code, name)
			if err != nil {
				return nil, err
			}
			glyph.ID, glyph.HasID = id, true
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

// simpleGlyphName 解析非符号简单字体字符码对应的字形名称，未知外部编码返回空字符串
// 入参: code 字符码, core 标准14字体度量，可为空
// 返回: string 字形名称
func (f *Font) simpleGlyphName(code uint32, core *coreFontMetrics) string {
	name, mapped := f.differences[code]
	if f.cffNames != nil {
		name, mapped = f.cffNames[code]
	}
	if !mapped && f.encoding == "" && (f.cffNames != nil || f.Subtype == "Type3") {
		return ".notdef"
	}
	if mapped {
		return name
	}
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
	if encoding != nil && name == "" {
		return ".notdef"
	}
	return name
}

// resolvedSimpleGlyphContext 查找并校验简单TrueType字体的字形编号，越界编号归一为.notdef
// 入参: ctx 取消上下文, code 字符码, name 字形名称
// 返回: uint16 有效字形编号, error 映射、恢复校验或取消错误
func (f *Font) resolvedSimpleGlyphContext(ctx context.Context, code uint32, name string) (uint16, error) {
	id, err := f.simpleGlyphContext(ctx, code, name)
	if err != nil {
		return 0, err
	}
	if f.cmapRecovered {
		undefined := !f.symbolic && (name == "" || name == ".notdef")
		blank := !f.symbolic && (name == "space" || name == "nonbreakingspace")
		if id == 0 && !undefined && !blank || f.glyphCount != 0 && uint32(id) >= f.glyphCount {
			return 0, fmt.Errorf("font %q %w for character code %#x (%q)", f.Name, errRecoveredCmapGlyphUnavailable, code, name)
		}
	}
	if f.glyphCount != 0 && uint32(id) >= f.glyphCount {
		return 0, nil
	}
	return id, nil
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
