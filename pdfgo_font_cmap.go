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
)

// fontCmap 读取指定TrueType字符表，不把ToUnicode当作字形索引
// 入参: data 字体数据, symbolic 是否为符号字体
// 返回: []byte 字符映射表, Name 映射编码, error 解析错误
func fontCmap(data []byte, symbolic bool) ([]byte, Name, error) {
	if len(data) < 12 {
		return nil, "", fmt.Errorf("truncated TrueType font")
	}
	count := int(binary.BigEndian.Uint16(data[4:6]))
	if count > (len(data)-12)/16 {
		return nil, "", fmt.Errorf("invalid TrueType directory")
	}
	var cmap []byte
	for n := 0; n < count; n++ {
		record := data[12+n*16:]
		if string(record[:4]) != "cmap" {
			continue
		}
		offset, length := uint64(binary.BigEndian.Uint32(record[8:])), uint64(binary.BigEndian.Uint32(record[12:]))
		if offset+length > uint64(len(data)) {
			return nil, "", fmt.Errorf("invalid TrueType cmap")
		}
		cmap = data[offset : offset+length]
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
				priority = 2
			}
			if platform == 3 && encoding == 10 {
				priority = 3
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
	case 0, 4, 6:
		if len(table) < 4 {
			return nil, "", fmt.Errorf("truncated cmap")
		}
		length = uint64(binary.BigEndian.Uint16(table[2:]))
	case 12:
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
	case 12:
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
				id := uint64(glyph) + uint64(code-start)
				if id > 65535 {
					return 0, fmt.Errorf("glyph index overflow")
				}
				return uint16(id), nil
			}
		}
	}
	return 0, nil
}
