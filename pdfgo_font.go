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
	"fmt"

	"golang.org/x/text/encoding/charmap"
)

// Font 保存PDF字体程序、字符映射及字宽，不依赖渲染后端
type Font struct {
	Name           string
	Subtype        Name
	Program        []byte
	ProgramType    Name
	Dictionary     Dictionary
	Unicode        UnicodeMap
	widths         map[uint32]float64
	defaultWidth   float64
	composite      bool
	encoding       Name
	cidMap         CIDMap
	glyphMap       []byte
	simpleCmap     []byte
	symbolCmap     bool
	symbolic       bool
	cffGlyphs      map[uint32]uint16
	cffNames       map[uint32]string
	differences    map[uint32]string
	type3Matrix    Matrix
	type3Procs     Dictionary
	type3Resources Dictionary
}

// Glyph 保存原始字符码、Unicode文本、字形名称、编号和千分之一字宽
// 缺少Unicode映射但具有字形编号时，Text为空，不推测字符含义
type Glyph struct {
	Name      string
	Code      uint32
	Text      string
	ID        uint16
	HasID     bool
	Width     float64
	WordSpace bool
}

// ReadFont 读取字体资源及嵌入程序
// 入参: object 字体字典或引用
// 返回: *Font 字体资源, error 错误信息
func (r *Reader) ReadFont(object Object) (*Font, error) {
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
	subtype, ok := dict["Subtype"].(Name)
	if !ok {
		return nil, fmt.Errorf("missing font subtype")
	}
	font := &Font{Dictionary: dict, Subtype: subtype, widths: map[uint32]float64{}}
	if name, ok := dict["BaseFont"].(Name); ok {
		font.Name = string(name)
	}
	metrics := dict
	if subtype == Name("Type0") {
		font.composite = true
		encoding, err := r.Resolve(dict["Encoding"])
		if err != nil {
			return nil, err
		}
		switch encoding {
		case Name("Identity-H"):
			font.encoding = Name("Identity-H")
		case Name("UniGB-UCS2-H"):
			font.encoding = Name("UniGB-UCS2-H")
			font.cidMap, err = loadUniGBUCS2H()
			if err != nil {
				return nil, err
			}
		default:
			return nil, &UnsupportedError{Feature: fmt.Sprintf("composite font encoding %v", encoding)}
		}
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
		if metrics["Subtype"] != Name("CIDFontType2") && metrics["Subtype"] != Name("CIDFontType0") {
			return nil, &UnsupportedError{Feature: "CID font subtype"}
		}
		if font.cidMap != nil {
			value, err := r.Resolve(metrics["CIDSystemInfo"])
			if err != nil {
				return nil, err
			}
			system, ok := value.(Dictionary)
			if !ok {
				return nil, fmt.Errorf("invalid CID system information")
			}
			registry, registryOK := system["Registry"].(String)
			ordering, orderingOK := system["Ordering"].(String)
			if !registryOK || !orderingOK || string(registry) != "Adobe" || string(ordering) != "GB1" {
				return nil, fmt.Errorf("UniGB-UCS2-H requires Adobe-GB1 CID collection")
			}
		}
		font.defaultWidth = 1000
		if metrics["DW"] != nil {
			font.defaultWidth, err = r.number(metrics["DW"])
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
				start, ok := array[n].(Integer)
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
						width, err := r.number(value)
						if err != nil {
							return nil, err
						}
						font.widths[uint32(start)+uint32(j)] = width
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
					for code := start; code <= end; code++ {
						font.widths[uint32(code)] = width
					}
				}
			}
		}
		gidMap, err := r.Resolve(metrics["CIDToGIDMap"])
		if err != nil {
			return nil, err
		}
		if gidMap != nil && gidMap != Name("Identity") {
			stream, ok := gidMap.(*Stream)
			if !ok {
				return nil, fmt.Errorf("invalid CIDToGIDMap")
			}
			font.glyphMap, err = stream.Decode()
			if err != nil {
				return nil, err
			}
			if len(font.glyphMap)%2 != 0 {
				return nil, fmt.Errorf("invalid CIDToGIDMap length")
			}
		}
	} else if subtype == Name("TrueType") || subtype == Name("Type1") || subtype == Name("Type3") {
		encoding, err := r.Resolve(dict["Encoding"])
		if err != nil {
			return nil, err
		}
		if encoding != nil {
			switch value := encoding.(type) {
			case Name:
				font.encoding = value
			case Dictionary:
				if value["BaseEncoding"] != nil {
					name, ok := value["BaseEncoding"].(Name)
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
				if !ok {
					return nil, fmt.Errorf("invalid font encoding differences")
				}
				font.differences = map[uint32]string{}
				code := 256
				for _, entry := range entries {
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
			if !ok {
				return nil, fmt.Errorf("invalid Type3 font matrix")
			}
			numbers, err := numbers(values, 6)
			if err != nil {
				return nil, err
			}
			font.type3Matrix = Matrix(numbers)
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
		value, err := r.Resolve(dict["ToUnicode"])
		if err != nil {
			return nil, err
		}
		stream, ok := value.(*Stream)
		if !ok {
			return nil, fmt.Errorf("invalid ToUnicode")
		}
		data, err := stream.Decode()
		if err != nil {
			return nil, err
		}
		font.Unicode, err = ParseUnicodeMap(data)
		if err != nil {
			return nil, err
		}
	}
	descriptor, err := r.Resolve(metrics["FontDescriptor"])
	if err != nil {
		return nil, err
	}
	if descriptor != nil {
		d, ok := descriptor.(Dictionary)
		if !ok {
			return nil, fmt.Errorf("invalid font descriptor")
		}
		flags, err := integerDefault(d, "Flags", 0)
		if err != nil {
			return nil, err
		}
		font.symbolic = flags&4 != 0
		if !font.composite && d["MissingWidth"] != nil {
			font.defaultWidth, err = r.number(d["MissingWidth"])
			if err != nil {
				return nil, err
			}
		}
		for _, key := range []Name{"FontFile2", "FontFile3", "FontFile"} {
			if d[key] == nil {
				continue
			}
			value, err := r.Resolve(d[key])
			if err != nil {
				return nil, err
			}
			stream, ok := value.(*Stream)
			if !ok {
				return nil, fmt.Errorf("invalid font program")
			}
			font.Program, err = stream.Decode()
			if err != nil {
				return nil, err
			}
			font.ProgramType = key
			if key == Name("FontFile3") {
				font.ProgramType, _ = stream.Dictionary["Subtype"].(Name)
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
	if !font.composite && font.Subtype == Name("TrueType") && len(font.Program) > 0 && font.ProgramType != "Type1C" {
		font.simpleCmap, font.symbolCmap, err = fontCmap(font.Program, font.symbolic)
		if err != nil {
			return nil, err
		}
	}
	if font.ProgramType == "Type1C" || font.ProgramType == "CIDFontType0C" {
		if !font.composite && font.encoding != "" && font.encoding != "WinAnsiEncoding" && font.encoding != "MacRomanEncoding" && font.encoding != "StandardEncoding" {
			return nil, &UnsupportedError{Feature: "external CFF encoding"}
		}
		identity := font.composite && metrics["Subtype"] == Name("CIDFontType2") && font.glyphMap == nil
		font.cffGlyphs, font.cffNames, err = cffFontMapping(font.Program, font.composite, font.encoding, font.differences, identity)
		if err != nil {
			return nil, err
		}
	}
	if font.composite && metrics["Subtype"] == Name("CIDFontType0") && font.cffGlyphs == nil {
		return nil, &UnsupportedError{Feature: "CID CFF glyph mapping without bare CFF program"}
	}
	if indirect {
		if r.fonts == nil {
			r.fonts = map[Reference]*Font{}
		}
		r.fonts[ref] = font
	}
	return font, nil
}

// Decode 解码文字串，未知Unicode映射不会以猜测文本替代
// 入参: data 原始文字字节
// 返回: []Glyph 字符信息, error 错误信息
func (f *Font) Decode(data []byte) ([]Glyph, error) {
	step := 1
	if f.composite {
		step = 2
	}
	if len(data)%step != 0 {
		return nil, fmt.Errorf("incomplete font character code")
	}
	glyphs := make([]Glyph, 0, len(data)/step)
	for n := 0; n < len(data); n += step {
		raw := data[n : n+step]
		code := uint32(codeNumber(raw))
		cid := code
		if f.cidMap != nil {
			mapped, ok := f.cidMap[string(raw)]
			if !ok {
				return nil, &UnsupportedError{Feature: "character outside predefined CID mapping"}
			}
			cid = uint32(mapped)
		}
		text, ok := f.Unicode[string(raw)]
		if !ok && f.encoding == Name("UniGB-UCS2-H") {
			var err error
			text, err = unicodeBytes(raw)
			if err != nil {
				return nil, err
			}
			ok = true
		}
		if !ok && !f.composite {
			name := f.differences[code]
			if name == "" {
				encoding := pdfStandardNames
				switch f.encoding {
				case Name("WinAnsiEncoding"):
					encoding = pdfWinAnsiNames
				case Name("MacRomanEncoding"):
					encoding = pdfMacRomanNames
				case "", Name("StandardEncoding"):
				default:
					encoding = nil
				}
				if int(code) < len(encoding) {
					name = encoding[code]
				}
			}
			if name != "" && name != ".notdef" {
				text, ok = glyphNameUnicode(name)
			}
		}
		if !ok && f.cffGlyphs != nil {
			_, ok = f.cffGlyphs[cid]
		}
		if !ok {
			if f.composite {
				return nil, &UnsupportedError{Feature: "CID without Unicode mapping"}
			}
			if f.Subtype == Name("Type3") && f.differences[code] != "" {
				text = ""
			} else if f.encoding == Name("WinAnsiEncoding") {
				text = string(charmap.Windows1252.DecodeByte(raw[0]))
			} else if raw[0] >= 32 && raw[0] <= 126 && (f.encoding == "" || f.encoding == Name("StandardEncoding")) {
				text = string(rune(raw[0]))
			} else {
				return nil, &UnsupportedError{Feature: "unmapped simple font character"}
			}
		}
		width, ok := f.widths[cid]
		if !ok {
			width = f.defaultWidth
			if len(f.widths) == 0 && len(f.Program) == 0 && f.Subtype == Name("Type1") {
				if standardWidth, found := f.coreLatinWidth(code); found {
					width = standardWidth
				}
			}
		}
		if f.Subtype == Name("Type3") {
			width *= f.type3Matrix[0] * 1000
		}
		if width == 0 && len(f.widths) == 0 && !f.composite {
			return nil, &UnsupportedError{Feature: "unembedded standard font metrics"}
		}
		glyph := Glyph{Code: code, Text: text, Width: width, WordSpace: step == 1 && code == 32}
		if f.Subtype == Name("Type3") {
			glyph.Name = f.differences[code]
			if glyph.Name == "" {
				return nil, &UnsupportedError{Feature: "Type3 character without glyph name"}
			}
		}
		if f.simpleCmap != nil {
			lookup := code
			if !f.symbolic {
				if f.encoding != Name("WinAnsiEncoding") {
					return nil, &UnsupportedError{Feature: "simple TrueType encoding without explicit WinAnsi mapping"}
				}
				lookup = uint32(charmap.Windows1252.DecodeByte(byte(code)))
			}
			id, err := cmapGlyph(f.simpleCmap, lookup)
			if err != nil {
				return nil, err
			}
			if f.symbolCmap && id == 0 {
				for _, base := range []uint32{0xF000, 0xF100, 0xF200} {
					candidate, err := cmapGlyph(f.simpleCmap, base+code)
					if err != nil {
						return nil, err
					}
					if candidate != 0 {
						if id != 0 && id != candidate {
							return nil, fmt.Errorf("ambiguous symbol cmap")
						}
						id = candidate
					}
				}
			}
			glyph.ID = id
			glyph.HasID = true
		}
		if f.composite {
			glyph.HasID = true
			glyph.ID = uint16(cid)
			if f.glyphMap != nil {
				if int(cid)*2+1 >= len(f.glyphMap) {
					glyph.ID = 0
				} else {
					glyph.ID = uint16(f.glyphMap[cid*2])<<8 | uint16(f.glyphMap[cid*2+1])
				}
			}
		}
		if f.cffGlyphs != nil {
			glyph.ID, glyph.HasID = f.cffGlyphs[cid]
			glyph.Name = f.cffNames[cid]
			if !glyph.HasID {
				return nil, fmt.Errorf("missing CFF glyph for code %d", code)
			}
		}
		glyphs = append(glyphs, glyph)
	}
	return glyphs, nil
}

// coreLatinWidth读取标准西文字体的内建宽度
func (f *Font) coreLatinWidth(code uint32) (float64, bool) {
	widths, ok := pdfCoreLatinWidths[f.Name]
	if !ok || code > 255 {
		return 0, false
	}
	name := f.differences[code]
	if name == "" {
		encoding := pdfStandardNames
		switch f.encoding {
		case Name("WinAnsiEncoding"):
			encoding = pdfWinAnsiNames
		case Name("MacRomanEncoding"):
			encoding = pdfMacRomanNames
		case "", Name("StandardEncoding"):
		default:
			return 0, false
		}
		name = encoding[code]
	}
	width, ok := widths[name]
	return float64(width), ok
}
