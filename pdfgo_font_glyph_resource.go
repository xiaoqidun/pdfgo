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
	"fmt"
	"maps"
	"math"
	"slices"
	"strings"
)

// GlyphWidth 指定内嵌字形及其字体程序字宽，单位为千分之一字号
// Width不含字距、字偶调整或整形偏移，不以整形后的推进量代替
type GlyphWidth struct {
	ID    uint16
	Width float64
}

// GlyphFont 复用内嵌程序建立横排复合字体，使指定字形不受源字符编码限制
// 返回字体仅编码指定字形，Dictionary可作为同一阅读器中的字体资源，原文语义和定位由调用方保留
// 不修改源字典、字符映射或字体程序；调用方提供的字宽须与字体程序一致
// 入参: glyphs 所需字形与字宽
// 返回: *Font 新字体, error 字体或度量错误
func (f *Font) GlyphFont(glyphs []GlyphWidth) (*Font, error) {
	return f.GlyphFontContext(context.Background(), glyphs)
}

// GlyphFontContext 构造可直接编码指定字形的字体，失败或取消时不返回部分资源
// 入参: ctx 取消上下文, glyphs 所需字形与字宽
// 返回: *Font 新字体, error 字体、度量或取消错误
func (f *Font) GlyphFontContext(ctx context.Context, glyphs []GlyphWidth) (*Font, error) {
	if f == nil || ctx == nil || f.reader == nil {
		return nil, fmt.Errorf("missing font reader or glyph resource context")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if f.reader.closed {
		return nil, fmt.Errorf("reader is closed")
	}
	if len(f.Program) == 0 || len(glyphs) == 0 {
		return nil, fmt.Errorf("missing embedded program or glyph widths")
	}
	ids, info, subtype, err := f.glyphResourceMapping(ctx)
	if err != nil {
		return nil, err
	}
	widths := make(map[uint16]float64, len(glyphs))
	for _, glyph := range glyphs {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if glyph.ID == 0 || int(glyph.ID) >= len(ids) || math.IsNaN(glyph.Width) || math.IsInf(glyph.Width, 0) {
			return nil, fmt.Errorf("invalid glyph resource index or width")
		}
		cid := ids[glyph.ID]
		if previous, ok := widths[cid]; ok && previous != glyph.Width {
			return nil, fmt.Errorf("inconsistent glyph resource width")
		}
		widths[cid] = glyph.Width
	}
	cids := make([]uint16, 0, len(widths))
	for cid := range widths {
		cids = append(cids, cid)
	}
	slices.Sort(cids)
	var w Array
	for start := 0; start < len(cids); {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		end := start + 1
		for end < len(cids) && uint32(cids[end-1])+1 == uint32(cids[end]) {
			end++
		}
		values := make(Array, end-start)
		for i, cid := range cids[start:end] {
			values[i] = Real(widths[cid])
		}
		w = append(w, Integer(cids[start]), values)
		start = end
	}
	source := f.Dictionary
	if f.composite {
		value, err := f.reader.Resolve(source["DescendantFonts"])
		if err != nil {
			return nil, err
		}
		children, ok := value.(Array)
		if !ok || len(children) != 1 {
			return nil, fmt.Errorf("invalid source descendant font")
		}
		value, err = f.reader.Resolve(children[0])
		if err != nil {
			return nil, err
		}
		source, ok = value.(Dictionary)
		if !ok {
			return nil, fmt.Errorf("invalid source descendant font")
		}
	}
	value, err := f.reader.Resolve(source["BaseFont"])
	if err != nil {
		return nil, err
	}
	baseName, ok := value.(Name)
	if !ok || baseName == "" {
		return nil, fmt.Errorf("missing source base font name")
	}
	value, err = f.reader.Resolve(source["FontDescriptor"])
	if err != nil {
		return nil, err
	}
	descriptor, ok := value.(Dictionary)
	if !ok {
		return nil, fmt.Errorf("missing source font descriptor")
	}
	descriptor = maps.Clone(descriptor)
	for _, key := range []Name{"FontFile", "FontFile2", "FontFile3", "CIDSet", "CharSet"} {
		delete(descriptor, key)
	}
	program := &Stream{Data: f.Program, Dictionary: Dictionary{}, reader: f.reader}
	if subtype == "CIDFontType2" && f.ProgramType != "OpenType" {
		program.Dictionary["Length1"] = Integer(len(f.Program))
		descriptor["FontFile2"] = program
	} else {
		kind := Name("CIDFontType0C")
		if f.ProgramType == "OpenType" {
			kind = "OpenType"
		}
		program.Dictionary["Subtype"] = kind
		descriptor["FontFile3"] = program
	}
	child := Dictionary{"Type": Name("Font"), "Subtype": subtype, "BaseFont": baseName, "CIDSystemInfo": info, "FontDescriptor": descriptor, "W": w}
	if subtype == "CIDFontType2" {
		child["CIDToGIDMap"] = Name("Identity")
	}
	encoding, err := glyphResourceCMap(ctx, cids, info)
	if err != nil {
		return nil, err
	}
	name := string(baseName)
	if subtype == "CIDFontType0" {
		name += "-Glyph-H"
	}
	resource := Dictionary{"Type": Name("Font"), "Subtype": Name("Type0"), "BaseFont": Name(name), "Encoding": encoding, "DescendantFonts": Array{child}}
	return f.reader.ReadFontContext(ctx, resource)
}

// glyphResourceCMap 生成仅包含所需CID的双字节横排编码，未列出的字符映射到缺省字形
// 入参: ctx 取消上下文, cids 有序CID, info 字符集标识
// 返回: *Stream 编码流, error 取消错误
func glyphResourceCMap(ctx context.Context, cids []uint16, info Dictionary) (*Stream, error) {
	var content strings.Builder
	content.WriteString("/CIDInit /ProcSet findresource begin\n12 dict begin\nbegincmap\n")
	fmt.Fprintf(&content, "/CIDSystemInfo << /Registry <%X> /Ordering <%X> /Supplement %d >> def\n", []byte(info["Registry"].(String)), []byte(info["Ordering"].(String)), info["Supplement"])
	content.WriteString("/CMapName /Glyph-H def\n/CMapType 1 def\n/WMode 0 def\n1 begincodespacerange\n<0000> <FFFF>\nendcodespacerange\n")
	for start := 0; start < len(cids); start += 100 {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		part := cids[start:min(start+100, len(cids))]
		fmt.Fprintf(&content, "%d begincidchar\n", len(part))
		for _, cid := range part {
			fmt.Fprintf(&content, "<%04X> %d\n", cid, cid)
		}
		content.WriteString("endcidchar\n")
	}
	content.WriteString("endcmap\nCMapName currentdict /CMap defineresource pop\nend\nend\n")
	return &Stream{Data: []byte(content.String()), Dictionary: Dictionary{"Type": Name("CMap"), "CMapName": Name("Glyph-H"), "CIDSystemInfo": maps.Clone(info), "WMode": Integer(0)}}, nil
}

// glyphResourceMapping 按字体程序建立GID到CID映射，不沿用原PDF字符编码
// 入参: ctx 取消上下文
// 返回: []uint16 字形对应CID, Dictionary 字符集标识, Name 后代字体类型, error 字体程序或取消错误
func (f *Font) glyphResourceMapping(ctx context.Context) ([]uint16, Dictionary, Name, error) {
	info := Dictionary{"Registry": String("Adobe"), "Ordering": String("Identity"), "Supplement": Integer(0)}
	var cff []byte
	switch f.ProgramType {
	case "Type1C", "CIDFontType0C":
		cff = f.Program
	case "OpenType":
		var err error
		cff, err = fontTableContext(ctx, f.Program, "CFF ")
		if err != nil {
			return nil, nil, "", err
		}
	case "FontFile2":
	default:
		return nil, nil, "", &UnsupportedError{Feature: "glyph resource font program " + string(f.ProgramType)}
	}
	if len(cff) == 0 {
		if f.glyphCount == 0 || f.glyphCount > 65535 {
			return nil, nil, "", fmt.Errorf("invalid glyph resource TrueType count")
		}
		ids := make([]uint16, int(f.glyphCount))
		for i := range ids {
			if err := ctx.Err(); err != nil {
				return nil, nil, "", err
			}
			ids[i] = uint16(i)
		}
		return ids, info, "CIDFontType2", nil
	}
	dict, stringsIndex, chars, err := cffFontData(cff)
	if err != nil {
		return nil, nil, "", err
	}
	ids := make([]uint16, len(chars))
	if ros, cid := dict[1230]; cid {
		if len(ros) != 3 || ros[2] < 0 || ros[2] > math.MaxInt32 || math.Trunc(ros[2]) != ros[2] {
			return nil, nil, "", fmt.Errorf("invalid glyph resource CFF character collection")
		}
		for i, key := range []Name{"Registry", "Ordering"} {
			sid := ros[i]
			if sid < 0 || sid >= float64(len(cffStandardNames)+len(stringsIndex)) || math.Trunc(sid) != sid {
				return nil, nil, "", fmt.Errorf("invalid glyph resource CFF string")
			}
			if int(sid) < len(cffStandardNames) {
				info[key] = String(cffStandardNames[int(sid)])
			} else {
				info[key] = String(stringsIndex[int(sid)-len(cffStandardNames)])
			}
		}
		info["Supplement"] = Integer(ros[2])
		ids, err = cffCharset(cff, dict, len(chars))
		if err != nil {
			return nil, nil, "", err
		}
	} else {
		for operator := 1231; operator <= 1238; operator++ {
			if _, exists := dict[operator]; exists {
				return nil, nil, "", fmt.Errorf("missing glyph resource CFF character collection")
			}
		}
		for i := range ids {
			ids[i] = uint16(i)
		}
	}
	seen := make(map[uint16]bool, len(ids))
	for i, cid := range ids {
		if err := ctx.Err(); err != nil {
			return nil, nil, "", err
		}
		if seen[cid] || i == 0 && cid != 0 {
			return nil, nil, "", fmt.Errorf("invalid glyph resource CFF charset")
		}
		seen[cid] = true
	}
	return ids, info, "CIDFontType0", nil
}
