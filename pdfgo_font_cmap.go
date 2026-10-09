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
	"unicode/utf16"
	"unicode/utf8"
)

// errFontCmapUnavailable 表示缺少适用字符表，非符号字体可改用名称表
var errFontCmapUnavailable = errors.New("TrueType cmap unavailable")

// fontTable 读取SFNT字体的指定表并校验目录与数据范围
// 入参: data 字体数据, tag 表标签
// 返回: []byte 表数据或空值, error 解析错误
func fontTable(data []byte, tag string) ([]byte, error) {
	return fontTableContext(context.Background(), data, tag)
}

// fontTableContext 读取字体表目录并响应本次取消
// 入参: ctx 取消上下文, data 字体数据, tag 表标签
// 返回: []byte 表数据或空值, error 解析或取消错误
func fontTableContext(ctx context.Context, data []byte, tag string) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if len(data) < 12 {
		return nil, fmt.Errorf("truncated SFNT font")
	}
	count := int(binary.BigEndian.Uint16(data[4:6]))
	if count > (len(data)-12)/16 {
		return nil, fmt.Errorf("invalid SFNT directory")
	}
	var table []byte
	for n := 0; n < count; n++ {
		if n&255 == 0 {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
		}
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
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return table, nil
}

// fontCmapContext 选择并校验TrueType字符表，检查目录及分组解析的取消
// 入参: ctx 取消上下文, data 字体数据, symbolic 是否为符号字体
// 返回: []byte 已校验字符表, Name 映射编码, error 解析或取消错误
func fontCmapContext(ctx context.Context, data []byte, symbolic bool) ([]byte, Name, error) {
	if err := ctx.Err(); err != nil {
		return nil, "", err
	}
	cmap, err := fontTableContext(ctx, data, "cmap")
	if err != nil {
		return nil, "", err
	}
	if len(cmap) == 0 {
		return nil, "", errFontCmapUnavailable
	}
	if len(cmap) < 4 {
		return nil, "", fmt.Errorf("truncated TrueType cmap")
	}
	entries := int(binary.BigEndian.Uint16(cmap[2:]))
	if entries > (len(cmap)-4)/8 {
		return nil, "", fmt.Errorf("invalid cmap directory")
	}
	selected, score := -1, -1
	var encodingName Name
	for n := 0; n < entries; n++ {
		if n&255 == 0 {
			if err := ctx.Err(); err != nil {
				return nil, "", err
			}
		}
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
		return nil, "", errFontCmapUnavailable
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
	case 8, 10, 12, 13:
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
	table = table[:length]
	if err := validateFontCmap(ctx, table); err != nil {
		return nil, "", err
	}
	return table, encodingName, nil
}

// validateFontCmap 校验字符表范围、分段顺序及字形数组，不展开字符集合
// 入参: ctx 取消上下文, table 已截取的字符表
// 返回: error 字符表或取消错误
func validateFontCmap(ctx context.Context, table []byte) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if len(table) < 4 {
		return fmt.Errorf("truncated cmap")
	}
	u16 := func(n int) uint16 { return binary.BigEndian.Uint16(table[n:]) }
	format := u16(0)
	switch format {
	case 0:
		if len(table) < 262 {
			return fmt.Errorf("truncated cmap format 0")
		}
	case 2:
		if len(table) < 526 {
			return fmt.Errorf("truncated cmap format 2")
		}
		maximum := 0
		for index := range 256 {
			key := int(u16(6 + 2*index))
			if key%8 != 0 || key > len(table)-526 {
				return fmt.Errorf("invalid cmap subheader")
			}
			maximum = max(maximum, key)
		}
		for key := 0; key <= maximum; key += 8 {
			if key&2047 == 0 {
				if err := ctx.Err(); err != nil {
					return err
				}
			}
			address := 518 + key
			first, count, offset := int(u16(address)), int(u16(address+2)), int(u16(address+6))
			if first+count > 256 || offset%2 != 0 || count != 0 && (address+6+offset < 526+maximum || count*2 > len(table)-address-6-offset) {
				return fmt.Errorf("invalid cmap subheader glyph range")
			}
		}
	case 4:
		if len(table) < 16 {
			return fmt.Errorf("truncated cmap format 4")
		}
		count := int(u16(6)) / 2
		if u16(6)%2 != 0 || count == 0 || count > (len(table)-16)/8 {
			return fmt.Errorf("invalid cmap segments")
		}
		for index := range count {
			if index&255 == 0 {
				if err := ctx.Err(); err != nil {
					return err
				}
			}
			end, start := int(u16(14+index*2)), int(u16(16+count*2+index*2))
			if start > end || index != 0 && end <= int(u16(12+index*2)) {
				return fmt.Errorf("invalid cmap segment order")
			}
			address := 16 + count*6 + index*2
			offset := int(u16(address))
			terminal := index == count-1 && start == 65535 && end == 65535
			if offset != 0 && !terminal && (offset%2 != 0 || address+offset < 16+count*8 || 2*(end-start+1) > len(table)-address-offset) {
				return fmt.Errorf("invalid cmap glyph offset")
			}
		}
	case 6:
		if len(table) < 10 {
			return fmt.Errorf("truncated cmap format 6")
		}
		first, count := int(u16(6)), int(u16(8))
		if first+count > 65536 || count > (len(table)-10)/2 {
			return fmt.Errorf("invalid cmap glyph range")
		}
	case 10:
		if len(table) < 20 {
			return fmt.Errorf("truncated cmap format 10")
		}
		first, count := uint64(binary.BigEndian.Uint32(table[12:])), uint64(binary.BigEndian.Uint32(table[16:]))
		if first+count > 1<<32 || count > uint64((len(table)-20)/2) {
			return fmt.Errorf("invalid cmap glyph range")
		}
	case 8, 12, 13:
		offset := 16
		if format == 8 {
			offset = 8208
		}
		if len(table) < offset {
			return fmt.Errorf("truncated cmap format %d", format)
		}
		count := uint64(binary.BigEndian.Uint32(table[offset-4:]))
		if count > uint64((len(table)-offset)/12) {
			return fmt.Errorf("invalid cmap groups")
		}
		var previous uint32
		for index := uint64(0); index < count; index++ {
			if index&255 == 0 {
				if err := ctx.Err(); err != nil {
					return err
				}
			}
			group := table[offset+int(index)*12:]
			start, end, glyph := binary.BigEndian.Uint32(group), binary.BigEndian.Uint32(group[4:]), uint64(binary.BigEndian.Uint32(group[8:]))
			if start > end || index != 0 && start <= previous {
				return fmt.Errorf("invalid cmap group order")
			}
			previous = end
			if format != 13 {
				glyph += uint64(end) - uint64(start)
			}
			if glyph > 65535 {
				return fmt.Errorf("glyph index overflow")
			}
			if format == 8 {
				if start <= 65535 && end > 65535 {
					return fmt.Errorf("mixed cmap character range")
				}
				first, last := start, end
				wide := start > 65535
				if wide {
					first, last = start>>16, end>>16
				}
				for word := first; word <= last; word++ {
					if (word-first)&255 == 0 {
						if err := ctx.Err(); err != nil {
							return err
						}
					}
					if (table[12+word/8]&(1<<(7-word%8)) != 0) != wide {
						return fmt.Errorf("invalid cmap character width")
					}
				}
			}
		}
	default:
		return &UnsupportedError{Feature: fmt.Sprintf("TrueType cmap format %d", format)}
	}
	return ctx.Err()
}

// simpleGlyphContext 按字符表或字形名称选择字形，名称解析响应本次取消
// 入参: ctx 取消上下文, code 原始字符码, name 编码名称
// 返回: uint16 字形编号, error 映射或取消错误
func (f *Font) simpleGlyphContext(ctx context.Context, code uint32, name string) (uint16, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
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
	if f.simpleCmap == nil {
		return f.post.lookupContext(ctx, name)
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
		id, err := cmapUnicodeGlyph(f.simpleCmap, codepoint)
		if err != nil || id != 0 {
			return id, err
		}
	}
	return f.post.lookupContext(ctx, name)
}

// cmapUnicodeGlyph 查询Unicode字符，格式8按声明的宽字符前缀匹配编码值
// 入参: table 已校验字符表, code Unicode字符
// 返回: uint16 字形编号, error 映射或编码冲突错误
func cmapUnicodeGlyph(table []byte, code uint32) (uint16, error) {
	id, err := cmapGlyph(table, code)
	if err != nil || len(table) < 2 || binary.BigEndian.Uint16(table) != 8 || code <= 65535 || code > utf8.MaxRune {
		return id, err
	}
	high, low := utf16.EncodeRune(rune(code))
	packed, err := cmapGlyph(table, uint32(high)<<16|uint32(low))
	if err != nil {
		return 0, err
	}
	if id != 0 && packed != 0 && id != packed {
		return 0, fmt.Errorf("ambiguous cmap Unicode character")
	}
	if packed != 0 {
		return packed, nil
	}
	return id, nil
}

// cmapGlyph 从已校验的字符表查询字形编号，分段映射使用二分查找
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
		low, high := 0, count
		for low < high {
			middle := low + (high-low)/2
			if uint32(u16(14+middle*2)) < code {
				low = middle + 1
			} else {
				high = middle
			}
		}
		if n := low; n < count {
			start := uint32(u16(16 + count*2 + n*2))
			if code < start {
				return 0, nil
			}
			delta := u16(16 + count*4 + n*2)
			address := 16 + count*6 + n*2
			offset := u16(address)
			if offset == 0 {
				return uint16(code) + delta, nil
			}
			glyphAddress := address + int(offset) + int(code-start)*2
			if offset%2 != 0 || glyphAddress < 16+count*8 || glyphAddress+2 > len(table) {
				return 0, fmt.Errorf("invalid cmap glyph offset")
			}
			glyph := u16(glyphAddress)
			if glyph != 0 {
				glyph += delta
			}
			return glyph, nil
		}
	case 8, 12, 13:
		format, offset := u16(0), 16
		if format == 8 {
			offset = 8208
		}
		if len(table) < offset {
			return 0, fmt.Errorf("truncated cmap format %d", format)
		}
		count := uint64(binary.BigEndian.Uint32(table[offset-4:]))
		if count > uint64((len(table)-offset)/12) {
			return 0, fmt.Errorf("invalid cmap groups")
		}
		if format == 8 {
			word := code
			if code > 65535 {
				word >>= 16
			}
			if (table[12+word/8]&(1<<(7-word%8)) != 0) != (code > 65535) {
				return 0, nil
			}
		}
		low, high := 0, int(count)
		for low < high {
			middle := low + (high-low)/2
			if binary.BigEndian.Uint32(table[offset+middle*12+4:]) < code {
				low = middle + 1
			} else {
				high = middle
			}
		}
		if low < int(count) {
			group := table[offset+low*12:]
			start, end, glyph := binary.BigEndian.Uint32(group), binary.BigEndian.Uint32(group[4:]), binary.BigEndian.Uint32(group[8:])
			if start > end {
				return 0, fmt.Errorf("reversed cmap range")
			}
			if code >= start && code <= end {
				id := uint64(glyph)
				if format != 13 {
					id += uint64(code - start)
				}
				if id > 65535 {
					return 0, fmt.Errorf("glyph index overflow")
				}
				return uint16(id), nil
			}
		}
	default:
		return 0, &UnsupportedError{Feature: fmt.Sprintf("TrueType cmap format %d", u16(0))}
	}
	return 0, nil
}
