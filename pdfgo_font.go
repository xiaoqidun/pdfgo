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
	Name         string
	Subtype      Name
	Program      []byte
	ProgramType  Name
	Dictionary   Dictionary
	Unicode      UnicodeMap
	widths       map[uint32]float64
	defaultWidth float64
	composite    bool
	encoding     Name
	glyphMap     []byte
	simpleCmap   []byte
	symbolCmap   bool
	symbolic     bool
}

// Glyph 保存原始字符码、Unicode文本、字形编号和千分之一字宽
type Glyph struct {
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
		if encoding != Name("Identity-H") {
			return nil, &UnsupportedError{Feature: "composite font encoding other than Identity-H"}
		}
		font.encoding = Name("Identity-H")
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
		if metrics["Subtype"] != Name("CIDFontType2") {
			return nil, &UnsupportedError{Feature: "non-TrueType CID font"}
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
	} else if subtype == Name("TrueType") || subtype == Name("Type1") {
		encoding, err := r.Resolve(dict["Encoding"])
		if err != nil {
			return nil, err
		}
		if encoding != nil {
			name, ok := encoding.(Name)
			if !ok {
				return nil, &UnsupportedError{Feature: "font encoding differences"}
			}
			font.encoding = name
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
	if !font.composite && font.Subtype == Name("TrueType") && len(font.Program) > 0 {
		font.simpleCmap, font.symbolCmap, err = fontCmap(font.Program, font.symbolic)
		if err != nil {
			return nil, err
		}
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
		text, ok := f.Unicode[string(raw)]
		if !ok {
			if f.composite {
				return nil, &UnsupportedError{Feature: "CID without Unicode mapping"}
			}
			if f.encoding == Name("WinAnsiEncoding") {
				text = string(charmap.Windows1252.DecodeByte(raw[0]))
			} else if raw[0] >= 32 && raw[0] <= 126 && (f.encoding == "" || f.encoding == Name("StandardEncoding")) {
				text = string(rune(raw[0]))
			} else {
				return nil, &UnsupportedError{Feature: "unmapped simple font character"}
			}
		}
		width, ok := f.widths[code]
		if !ok {
			width = f.defaultWidth
		}
		if width == 0 && len(f.widths) == 0 && !f.composite {
			return nil, &UnsupportedError{Feature: "unembedded standard font metrics"}
		}
		glyph := Glyph{Code: code, Text: text, Width: width, WordSpace: step == 1 && code == 32}
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
			glyph.ID = uint16(code)
			if f.glyphMap != nil {
				if int(code)*2+1 >= len(f.glyphMap) {
					glyph.ID = 0
				} else {
					glyph.ID = uint16(f.glyphMap[code*2])<<8 | uint16(f.glyphMap[code*2+1])
				}
			}
		}
		glyphs = append(glyphs, glyph)
	}
	return glyphs, nil
}
