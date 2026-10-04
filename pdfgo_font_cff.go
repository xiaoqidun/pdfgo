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
	"math"
	"strconv"
	"strings"
)

// cffIndex 读取CFF索引，返回切片及索引之后的位置
// 入参: data CFF数据, offset 索引起始位置
// 返回: [][]byte 索引项, int 索引结束位置, error 错误信息
func cffIndex(data []byte, offset int) ([][]byte, int, error) {
	if offset < 0 || offset > len(data)-2 {
		return nil, 0, fmt.Errorf("truncated CFF index")
	}
	count := int(binary.BigEndian.Uint16(data[offset:]))
	if count == 0 {
		return nil, offset + 2, nil
	}
	if offset+3 > len(data) {
		return nil, 0, fmt.Errorf("truncated CFF index header")
	}
	width := int(data[offset+2])
	if width < 1 || width > 4 || count+1 > (len(data)-offset-3)/width {
		return nil, 0, fmt.Errorf("invalid CFF index offsets")
	}
	base := offset + 3 + (count+1)*width
	positions := make([]int, count+1)
	for n := range positions {
		v := uint64(0)
		for _, b := range data[offset+3+n*width : offset+3+(n+1)*width] {
			v = v<<8 | uint64(b)
		}
		if v < 1 || v-1 > uint64(len(data)-base) || n > 0 && v < uint64(positions[n-1]+1) {
			return nil, 0, fmt.Errorf("invalid CFF index range")
		}
		positions[n] = int(v - 1)
	}
	if positions[0] != 0 {
		return nil, 0, fmt.Errorf("invalid first CFF index offset")
	}
	items := make([][]byte, count)
	for n := range items {
		items[n] = data[base+positions[n] : base+positions[n+1]]
	}
	return items, base + positions[count], nil
}

// cffDictionary 读取CFF字典数值及双字节操作符
// 入参: data 字典数据
// 返回: map[int][]float64 操作符及数值列表, error 错误信息
func cffDictionary(data []byte) (map[int][]float64, error) {
	result := map[int][]float64{}
	var values []float64
	for p := 0; p < len(data); {
		b := data[p]
		p++
		var value float64
		switch {
		case b <= 21:
			op := int(b)
			if b == 12 {
				if p == len(data) {
					return nil, fmt.Errorf("truncated CFF operator")
				}
				op = 1200 + int(data[p])
				p++
			}
			if len(values) == 0 {
				return nil, fmt.Errorf("missing CFF dictionary operands")
			}
			result[op] = values
			values = nil
			continue
		case b == 28:
			if p+2 > len(data) {
				return nil, fmt.Errorf("truncated CFF integer")
			}
			value = float64(int16(binary.BigEndian.Uint16(data[p:])))
			p += 2
		case b == 29:
			if p+4 > len(data) {
				return nil, fmt.Errorf("truncated CFF integer")
			}
			value = float64(int32(binary.BigEndian.Uint32(data[p:])))
			p += 4
		case b == 30:
			var text strings.Builder
			ended := false
			for p < len(data) && !ended {
				v := data[p]
				p++
				for _, n := range []byte{v >> 4, v & 15} {
					switch {
					case n < 10:
						text.WriteByte('0' + n)
					case n == 10:
						text.WriteByte('.')
					case n == 11:
						text.WriteByte('E')
					case n == 12:
						text.WriteString("E-")
					case n == 14:
						text.WriteByte('-')
					case n == 15:
						ended = true
					default:
						return nil, fmt.Errorf("invalid CFF real")
					}
					if ended {
						if v>>4 == 15 && v&15 != 15 {
							return nil, fmt.Errorf("invalid CFF real padding")
						}
						break
					}
				}
			}
			var err error
			value, err = strconv.ParseFloat(text.String(), 64)
			if !ended || err != nil || math.IsInf(value, 0) || math.IsNaN(value) {
				return nil, fmt.Errorf("invalid CFF real")
			}
		case b >= 32 && b <= 246:
			value = float64(int(b) - 139)
		case b >= 247 && b <= 254:
			if p == len(data) {
				return nil, fmt.Errorf("truncated CFF integer")
			}
			if b <= 250 {
				value = float64((int(b)-247)*256 + int(data[p]) + 108)
			} else {
				value = -float64((int(b)-251)*256 + int(data[p]) + 108)
			}
			p++
		default:
			return nil, fmt.Errorf("invalid CFF dictionary byte")
		}
		if len(values) == 48 {
			return nil, fmt.Errorf("CFF dictionary operand limit exceeded")
		}
		values = append(values, value)
	}
	if len(values) != 0 {
		return nil, fmt.Errorf("unused CFF dictionary operands")
	}
	return result, nil
}

// cffFontMapping 读取CID或内置字符编码到字形编号的映射，不重新解释轮廓
// 入参: data CFF数据, composite 是否为复合字体, encoding PDF基础编码, differences PDF编码差异, identity 是否允许非CID字形身份映射
// 返回: map[uint32]uint16 字符码或CID到字形编号的映射, map[uint32]string 字符码到字形名称的映射, error 错误信息
func cffFontMapping(data []byte, composite bool, encoding Name, differences map[uint32]string, identity bool) (map[uint32]uint16, map[uint32]string, error) {
	dict, stringsIndex, chars, err := cffFontData(data)
	if err != nil {
		return nil, nil, err
	}
	cidKeyed := false
	for operator := 1230; operator <= 1238; operator++ {
		if _, present := dict[operator]; present {
			cidKeyed = true
			break
		}
	}
	if composite && identity && !cidKeyed {
		glyphs := make(map[uint32]uint16, len(chars))
		for gid := range chars {
			glyphs[uint32(gid)] = uint16(gid)
		}
		return glyphs, nil, nil
	}
	charset, err := cffCharset(data, dict, len(chars))
	if err != nil {
		return nil, nil, err
	}
	bySID := make(map[uint32]uint16, len(charset))
	for gid, sid := range charset {
		bySID[uint32(sid)] = uint16(gid)
	}
	if composite {
		if len(dict[1230]) != 3 {
			return nil, nil, fmt.Errorf("CID CFF lacks ROS")
		}
		return bySID, nil, nil
	}
	encodingOffset := 0
	if encoding == "" {
		encodingOffset, err = cffOffset(data, dict, 16, 0)
		if err != nil {
			return nil, nil, err
		}
		if encodingOffset == 0 {
			encoding = "StandardEncoding"
		}
	}
	mapping := map[uint32]uint16{}
	if encoding == "" && encodingOffset == 1 {
		for code, sid := range cffExpertEncoding {
			if gid, ok := bySID[uint32(sid)]; ok && sid != 0 {
				mapping[uint32(code)] = gid
			}
		}
	} else if encoding == "" {
		if encodingOffset+2 > len(data) {
			return nil, nil, fmt.Errorf("truncated CFF encoding")
		}
		format, pos := data[encodingOffset], encodingOffset+1
		count := int(data[pos])
		pos++
		gid := 1
		for n := 0; n < count; n++ {
			if pos >= len(data) {
				return nil, nil, fmt.Errorf("truncated CFF encoding")
			}
			first, run := int(data[pos]), 1
			pos++
			switch format & 127 {
			case 0:
			case 1:
				if pos == len(data) {
					return nil, nil, fmt.Errorf("truncated CFF encoding range")
				}
				run += int(data[pos])
				pos++
			default:
				return nil, nil, fmt.Errorf("invalid CFF encoding format")
			}
			if first+run > 256 || gid+run > len(chars) {
				return nil, nil, fmt.Errorf("invalid CFF encoding range")
			}
			for code := first; code < first+run; code++ {
				if _, ok := mapping[uint32(code)]; ok {
					return nil, nil, fmt.Errorf("duplicate CFF character code")
				}
				mapping[uint32(code)] = uint16(gid)
				gid++
			}
		}
		if format&128 != 0 {
			if pos == len(data) {
				return nil, nil, fmt.Errorf("truncated CFF supplement")
			}
			count = int(data[pos])
			pos++
			if count > (len(data)-pos)/3 {
				return nil, nil, fmt.Errorf("truncated CFF supplements")
			}
			for n := 0; n < count; n++ {
				code := uint32(data[pos])
				sid := uint32(binary.BigEndian.Uint16(data[pos+1:]))
				pos += 3
				gid, ok := bySID[sid]
				if !ok {
					return nil, nil, fmt.Errorf("missing CFF supplement glyph")
				}
				mapping[code] = gid
			}
		}
	}
	glyphNames := map[uint32]string{}
	if encoding == "" && encodingOffset == 1 {
		for code, sid := range cffExpertEncoding {
			if sid != 0 {
				glyphNames[uint32(code)] = cffStandardNames[sid]
			}
		}
	}
	for code, gid := range mapping {
		sid := int(charset[gid])
		if sid < len(cffStandardNames) {
			glyphNames[code] = cffStandardNames[sid]
		} else {
			if sid-391 >= len(stringsIndex) {
				return nil, nil, fmt.Errorf("invalid CFF string identifier")
			}
			glyphNames[code] = string(stringsIndex[sid-391])
		}
	}
	if encoding != "" || len(differences) > 0 {
		byName := map[string]uint16{}
		for gid, sid := range charset {
			if int(sid) < len(cffStandardNames) {
				byName[cffStandardNames[sid]] = uint16(gid)
			} else if int(sid)-len(cffStandardNames) < len(stringsIndex) {
				byName[string(stringsIndex[int(sid)-len(cffStandardNames)])] = uint16(gid)
			} else {
				return nil, nil, fmt.Errorf("invalid CFF string identifier")
			}
		}
		if encoding != "" {
			var base []string
			switch encoding {
			case "WinAnsiEncoding":
				base = pdfWinAnsiNames
			case "MacRomanEncoding":
				base = pdfMacRomanNames
			case "MacExpertEncoding":
				base = pdfMacExpertNames[:]
			case "StandardEncoding":
				base = pdfStandardNames
			default:
				return nil, nil, &UnsupportedError{Feature: "external CFF encoding"}
			}
			mapping = map[uint32]uint16{}
			glyphNames = map[uint32]string{}
			for code, name := range base {
				if name != "" {
					glyphNames[uint32(code)] = name
				}
				if gid, ok := byName[name]; ok && name != "" && name != ".notdef" {
					mapping[uint32(code)] = gid
				}
			}
		}
		for code, name := range differences {
			gid, ok := byName[name]
			if !ok {
				delete(mapping, code)
				glyphNames[code] = name
				continue
			}
			mapping[code] = gid
			glyphNames[code] = name
		}
	}
	return mapping, glyphNames, nil
}

// cffFontData 读取单字体CFF的顶层字典、字符串及字形程序
// 入参: data CFF字体数据
// 返回: map[int][]float64 顶层字典, [][]byte 字符串, [][]byte 字形程序, error 错误信息
func cffFontData(data []byte) (map[int][]float64, [][]byte, [][]byte, error) {
	if len(data) < 4 || data[0] != 1 || data[2] < 4 || data[3] < 1 || data[3] > 4 {
		return nil, nil, nil, fmt.Errorf("invalid CFF header")
	}
	names, end, err := cffIndex(data, int(data[2]))
	if err != nil {
		return nil, nil, nil, err
	}
	if len(names) != 1 {
		return nil, nil, nil, &UnsupportedError{Feature: "CFF font set"}
	}
	tops, end, err := cffIndex(data, end)
	if err != nil {
		return nil, nil, nil, err
	}
	if len(tops) != 1 {
		return nil, nil, nil, fmt.Errorf("invalid CFF top dictionary count")
	}
	stringsIndex, _, err := cffIndex(data, end)
	if err != nil {
		return nil, nil, nil, err
	}
	dict, err := cffDictionary(tops[0])
	if err != nil {
		return nil, nil, nil, err
	}
	charOffset, err := cffOffset(data, dict, 17, 0)
	if err != nil {
		return nil, nil, nil, err
	}
	if charOffset == 0 {
		return nil, nil, nil, fmt.Errorf("missing CFF charstrings")
	}
	chars, _, err := cffIndex(data, charOffset)
	if err != nil {
		return nil, nil, nil, err
	}
	if len(chars) == 0 {
		return nil, nil, nil, fmt.Errorf("empty CFF charstrings")
	}
	return dict, stringsIndex, chars, nil
}

// cffOffset 读取顶层字典的预定义值或数据偏移
// 入参: data CFF数据, dict 顶层字典, op 操作符, fallback 默认值
// 返回: int 偏移或预定义值, error 错误信息
func cffOffset(data []byte, dict map[int][]float64, op, fallback int) (int, error) {
	values, ok := dict[op]
	if !ok {
		return fallback, nil
	}
	if len(values) != 1 || values[0] < 0 || values[0] > float64(len(data)) || values[0] != math.Trunc(values[0]) {
		return 0, fmt.Errorf("invalid CFF offset")
	}
	return int(values[0]), nil
}

// cffCharset 按CFF规范第13节及附录C读取字形标识
// 入参: data CFF数据, dict 顶层字典, count 字形数
// 返回: []uint16 SID或CID列表, error 错误信息
func cffCharset(data []byte, dict map[int][]float64, count int) ([]uint16, error) {
	offset, err := cffOffset(data, dict, 15, 0)
	if err != nil {
		return nil, err
	}
	if ros, ok := dict[1230]; ok && (len(ros) != 3 || offset <= 2) {
		return nil, fmt.Errorf("invalid CID CFF charset")
	}
	charset := make([]uint16, count)
	if offset <= 2 {
		if offset == 0 {
			if count > 229 {
				return nil, fmt.Errorf("invalid ISOAdobe charset length")
			}
			for gid := range charset {
				charset[gid] = uint16(gid)
			}
		} else {
			predefined := cffExpertCharset[:]
			if offset == 2 {
				predefined = cffExpertSubsetCharset[:]
			}
			if count > len(predefined) {
				return nil, fmt.Errorf("invalid predefined CFF charset length")
			}
			copy(charset, predefined)
		}
		return charset, nil
	}
	if offset >= len(data) {
		return nil, fmt.Errorf("invalid CFF charset offset")
	}
	format, pos := data[offset], offset+1
	if format > 2 {
		return nil, fmt.Errorf("invalid CFF charset format")
	}
	seen := map[uint16]bool{0: true}
	for gid := 1; gid < count; {
		if pos+2 > len(data) {
			return nil, fmt.Errorf("truncated CFF charset")
		}
		first := int(binary.BigEndian.Uint16(data[pos:]))
		pos += 2
		run := 1
		switch format {
		case 1:
			if pos == len(data) {
				return nil, fmt.Errorf("truncated CFF charset range")
			}
			run += int(data[pos])
			pos++
		case 2:
			if pos+2 > len(data) {
				return nil, fmt.Errorf("truncated CFF charset range")
			}
			run += int(binary.BigEndian.Uint16(data[pos:]))
			pos += 2
		}
		if run > count-gid || first+run > 65536 {
			return nil, fmt.Errorf("invalid CFF charset range")
		}
		for n := 0; n < run; n++ {
			sid := uint16(first + n)
			if seen[sid] {
				return nil, fmt.Errorf("duplicate CFF charset identifier")
			}
			seen[sid] = true
			charset[gid] = sid
			gid++
		}
	}
	return charset, nil
}
