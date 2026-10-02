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
	"encoding/binary"
	"fmt"
	"unicode/utf8"
)

// fontTable 读取SFNT字体的指定表并校验目录与数据范围
// 入参: data 字体数据, tag 表标签
// 返回: []byte 表数据或空值, error 解析错误
func fontTable(data []byte, tag string) ([]byte, error) {
	if len(data) < 12 {
		return nil, fmt.Errorf("truncated SFNT font")
	}
	count := int(binary.BigEndian.Uint16(data[4:6]))
	if count > (len(data)-12)/16 {
		return nil, fmt.Errorf("invalid SFNT directory")
	}
	var table []byte
	for n := 0; n < count; n++ {
		record := data[12+n*16:]
		if string(record[:4]) != tag {
			continue
		}
		offset, length := uint64(binary.BigEndian.Uint32(record[8:])), uint64(binary.BigEndian.Uint32(record[12:]))
		if offset+length > uint64(len(data)) {
			return nil, fmt.Errorf("invalid SFNT %s table", tag)
		}
		if table != nil {
			return nil, fmt.Errorf("duplicate SFNT %s table", tag)
		}
		table = data[offset : offset+length]
	}
	return table, nil
}

// fontCmap 读取指定TrueType字符表，不把ToUnicode当作字形索引
// 入参: data 字体数据, symbolic 是否为符号字体
// 返回: []byte 字符映射表, Name 映射编码, error 解析错误
func fontCmap(data []byte, symbolic bool) ([]byte, Name, error) {
	cmap, err := fontTable(data, "cmap")
	if err != nil {
		return nil, "", err
	}
	if len(cmap) < 4 {
		return nil, "", fmt.Errorf("missing TrueType cmap")
	}
	entries := int(binary.BigEndian.Uint16(cmap[2:]))
	if entries > (len(cmap)-4)/8 {
		return nil, "", fmt.Errorf("invalid cmap directory")
	}
	selected, score := -1, -1
	var encodingName Name
	for n := 0; n < entries; n++ {
		record := cmap[4+n*8:]
		platform, encoding := binary.BigEndian.Uint16(record), binary.BigEndian.Uint16(record[2:])
		priority := -1
		if symbolic {
			if entries == 1 {
				priority = 1
			}
			if platform == 1 && encoding == 0 {
				priority = 2
			}
			if platform == 3 && encoding == 0 {
				priority = 3
			}
		} else {
			if platform == 1 && encoding == 0 {
				priority = 0
			}
			if platform == 0 && encoding != 5 {
				priority = 1
			}
			if platform == 3 && encoding == 1 {
				priority = 3
			}
			if platform == 3 && encoding == 10 {
				priority = 2
			}
		}
		if priority > score {
			selected = n
			score = priority
			encodingName = "Unicode"
			if platform == 3 && encoding == 0 {
				encodingName = "Symbol"
			} else if platform == 1 && encoding == 0 {
				encodingName = "MacRomanEncoding"
			}
		}
	}
	if selected < 0 {
		return nil, "", &UnsupportedError{Feature: "TrueType cmap selection"}
	}
	offset := uint64(binary.BigEndian.Uint32(cmap[4+selected*8+4:]))
	if offset+2 > uint64(len(cmap)) {
		return nil, "", fmt.Errorf("invalid cmap offset")
	}
	table := cmap[offset:]
	format := binary.BigEndian.Uint16(table)
	var length uint64
	switch format {
	case 0, 2, 4, 6:
		if len(table) < 4 {
			return nil, "", fmt.Errorf("truncated cmap")
		}
		length = uint64(binary.BigEndian.Uint16(table[2:]))
	case 10, 12, 13:
		if len(table) < 8 {
			return nil, "", fmt.Errorf("truncated cmap")
		}
		length = uint64(binary.BigEndian.Uint32(table[4:]))
	default:
		return nil, "", &UnsupportedError{Feature: fmt.Sprintf("TrueType cmap format %d", format)}
	}
	if length < 4 || length > uint64(len(table)) {
		return nil, "", fmt.Errorf("invalid cmap length")
	}
	return table[:length], encodingName, nil
}

// simpleGlyph 按TrueType内嵌字符表选择字形，名称无法映射时使用post表
// 入参: code 原始单字节字符码, name 非符号字体的编码名称
// 返回: uint16 字形编号，未定义字符为零, error 字符表或名称表错误
func (f *Font) simpleGlyph(code uint32, name string) (uint16, error) {
	if f.symbolic {
		id, err := cmapGlyph(f.simpleCmap, code)
		if err != nil || f.cmapEncoding != "Symbol" {
			return id, err
		}
		for _, base := range [...]uint32{0xF000, 0xF100, 0xF200} {
			candidate, err := cmapGlyph(f.simpleCmap, base+code)
			if err != nil {
				return 0, err
			}
			if candidate != 0 {
				if id != 0 && id != candidate {
					return 0, fmt.Errorf("ambiguous symbol cmap")
				}
				id = candidate
			}
		}
		return id, nil
	}
	if name == "" || name == ".notdef" {
		return 0, nil
	}
	var codepoint uint32
	mapped := false
	if f.cmapEncoding == "MacRomanEncoding" {
		for index, glyphName := range pdfMacRomanNames {
			if index == 219 {
				glyphName = "Euro"
			}
			if glyphName == name {
				codepoint, mapped = uint32(index), true
				break
			}
		}
	} else {
		text, found := glyphNameUnicode(name)
		character, size := utf8.DecodeRuneInString(text)
		if found && size == len(text) && size != 0 {
			codepoint, mapped = uint32(character), true
		}
	}
	if mapped {
		id, err := cmapGlyph(f.simpleCmap, codepoint)
		if err != nil || id != 0 {
			return id, err
		}
	}
	return f.post.lookup(name)
}

// cmapGlyph 从已选定的字符表查询字形编号
// 入参: table 字符映射表, code 字符编码
// 返回: uint16 字形编号, error 解析错误
func cmapGlyph(table []byte, code uint32) (uint16, error) {
	if len(table) < 4 {
		return 0, fmt.Errorf("truncated cmap")
	}
	u16 := func(n int) uint16 { return binary.BigEndian.Uint16(table[n:]) }
	switch u16(0) {
	case 0:
		if len(table) < 262 {
			return 0, fmt.Errorf("truncated cmap format 0")
		}
		if code < 256 {
			return uint16(table[6+code]), nil
		}
	case 6:
		if len(table) < 10 {
			return 0, fmt.Errorf("truncated cmap format 6")
		}
		first, count := uint32(u16(6)), uint32(u16(8))
		if uint64(10)+uint64(count)*2 > uint64(len(table)) {
			return 0, fmt.Errorf("truncated cmap glyphs")
		}
		if code >= first && code-first < count {
			return u16(10 + int(code-first)*2), nil
		}
	case 2:
		if len(table) < 526 {
			return 0, fmt.Errorf("truncated cmap format 2")
		}
		if code > 65535 {
			return 0, nil
		}
		key := int(u16(6 + int(code>>8)*2))
		if code < 256 {
			if u16(6+int(code)*2) != 0 {
				return 0, nil
			}
			key = 0
		} else if key == 0 {
			return 0, nil
		}
		if key%8 != 0 || 526+key > len(table) {
			return 0, fmt.Errorf("invalid cmap subheader")
		}
		address := 518 + key
		first, count := uint32(u16(address)), uint32(u16(address+2))
		if first+count > 256 {
			return 0, fmt.Errorf("invalid cmap byte range")
		}
		low := code & 255
		if low < first || low-first >= count {
			return 0, nil
		}
		offset := address + 6 + int(u16(address+6)) + int(low-first)*2
		if offset+2 > len(table) {
			return 0, fmt.Errorf("invalid cmap glyph offset")
		}
		glyph := u16(offset)
		if glyph != 0 {
			glyph += u16(address + 4)
		}
		return glyph, nil
	case 10:
		if len(table) < 20 {
			return 0, fmt.Errorf("truncated cmap format 10")
		}
		first := uint64(binary.BigEndian.Uint32(table[12:]))
		count := uint64(binary.BigEndian.Uint32(table[16:]))
		if count > uint64((len(table)-20)/2) || first+count > 1<<32 {
			return 0, fmt.Errorf("invalid cmap glyph range")
		}
		if uint64(code) >= first && uint64(code)-first < count {
			return u16(20 + int(uint64(code)-first)*2), nil
		}
	case 4:
		if len(table) < 16 {
			return 0, fmt.Errorf("truncated cmap format 4")
		}
		count := int(u16(6)) / 2
		if count == 0 || 16+count*8 > len(table) {
			return 0, fmt.Errorf("invalid cmap segments")
		}
		if code > 65535 {
			return 0, nil
		}
		for n := 0; n < count; n++ {
			end, start := uint32(u16(14+n*2)), uint32(u16(16+count*2+n*2))
			if code < start || code > end {
				continue
			}
			delta := u16(16 + count*4 + n*2)
			address := 16 + count*6 + n*2
			offset := u16(address)
			if offset == 0 {
				return uint16(code) + delta, nil
			}
			glyphAddress := address + int(offset) + int(code-start)*2
			if glyphAddress < 0 || glyphAddress+2 > len(table) {
				return 0, fmt.Errorf("invalid cmap glyph offset")
			}
			glyph := u16(glyphAddress)
			if glyph != 0 {
				glyph += delta
			}
			return glyph, nil
		}
	case 12, 13:
		if len(table) < 16 {
			return 0, fmt.Errorf("truncated cmap format 12")
		}
		count := uint64(binary.BigEndian.Uint32(table[12:]))
		if count > uint64((len(table)-16)/12) {
			return 0, fmt.Errorf("invalid cmap groups")
		}
		for n := uint64(0); n < count; n++ {
			group := table[16+n*12:]
			start, end, glyph := binary.BigEndian.Uint32(group), binary.BigEndian.Uint32(group[4:]), binary.BigEndian.Uint32(group[8:])
			if start > end {
				return 0, fmt.Errorf("reversed cmap range")
			}
			if code >= start && code <= end {
				id := uint64(glyph)
				if u16(0) == 12 {
					id += uint64(code - start)
				}
				if id > 65535 {
					return 0, fmt.Errorf("glyph index overflow")
				}
				return uint16(id), nil
			}
		}
	}
	return 0, nil
}
