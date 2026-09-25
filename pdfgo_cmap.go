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
	"errors"
	"fmt"
	"strconv"
	"unicode/utf16"
)

// UnicodeMap 保存字符码到Unicode文本的映射，不将字符码等同于字形编号
type UnicodeMap map[string]string

var errInvalidUnicodeSurrogate = errors.New("invalid Unicode surrogate")

// ParseUnicodeMap 读取ToUnicode映射中的字符与范围定义
// 入参: data 已解码的CMap数据
// 返回: UnicodeMap 字符映射, error 错误信息
func ParseUnicodeMap(data []byte) (UnicodeMap, error) {
	p := objectParser{data: data}
	result := UnicodeMap{}
	previous := ""
	for {
		p.skipSpace()
		if p.pos == len(data) {
			return result, nil
		}
		if b := p.data[p.pos]; b == '/' || b == '(' || b == '<' || b == '[' {
			if _, err := p.object(); err != nil {
				return nil, err
			}
			previous = ""
			continue
		}
		token := p.token()
		if token == "" {
			p.pos++
			previous = ""
			continue
		}
		if token == "usecmap" {
			return nil, &UnsupportedError{Feature: "inherited ToUnicode CMap"}
		}
		if token != "beginbfchar" && token != "beginbfrange" {
			previous = token
			continue
		}
		count, err := strconv.Atoi(previous)
		if err != nil || count < 0 || count > 65536 {
			return nil, p.fail("invalid CMap mapping count")
		}
		for range count {
			from, err := p.object()
			if err != nil {
				return nil, err
			}
			first, ok := from.(String)
			if !ok || len(first) == 0 || len(first) > 4 {
				return nil, p.fail("invalid CMap source code")
			}
			if token == "beginbfchar" {
				to, err := p.object()
				if err != nil {
					return nil, err
				}
				str, ok := to.(String)
				if !ok {
					return nil, p.fail("invalid CMap Unicode value")
				}
				value, err := unicodeBytes(str)
				if err != nil {
					return nil, err
				}
				result[string(first)] = value
			} else {
				lastObject, err := p.object()
				if err != nil {
					return nil, err
				}
				last, ok := lastObject.(String)
				if !ok || len(last) != len(first) {
					return nil, p.fail("invalid CMap source range")
				}
				a, b := codeNumber(first), codeNumber(last)
				if b < a || b-a > 65535 {
					return nil, p.fail("excessive CMap source range")
				}
				to, err := p.object()
				if err != nil {
					return nil, err
				}
				for code := a; code <= b; code++ {
					var target String
					switch v := to.(type) {
					case String:
						target = append(String(nil), v...)
						carry := code - a
						for n := len(target) - 1; n >= 0; n-- {
							carry += uint64(target[n])
							target[n] = byte(carry)
							carry >>= 8
						}
						if carry != 0 {
							return nil, p.fail("CMap destination overflow")
						}
					case Array:
						if uint64(len(v)) != b-a+1 {
							return nil, p.fail("CMap destination count mismatch")
						}
						var ok bool
						target, ok = v[code-a].(String)
						if !ok {
							return nil, p.fail("invalid CMap destination")
						}
					default:
						return nil, p.fail("invalid CMap destination")
					}
					value, err := unicodeBytes(target)
					if err != nil {
						if errors.Is(err, errInvalidUnicodeSurrogate) {
							continue
						}
						return nil, err
					}
					key := make([]byte, len(first))
					v := code
					for n := len(key) - 1; n >= 0; n-- {
						key[n] = byte(v)
						v >>= 8
					}
					result[string(key)] = value
				}
			}
		}
		end := "endbfchar"
		if token == "beginbfrange" {
			end = "endbfrange"
		}
		if p.token() != end {
			return nil, p.fail("missing CMap block terminator")
		}
		previous = ""
	}
}

// codeNumber 将最多4字节的字符码解释为无符号整数
func codeNumber(code []byte) uint64 {
	var value uint64
	for _, b := range code {
		value = value<<8 | uint64(b)
	}
	return value
}

// unicodeBytes 严格解析CMap使用的UTF-16BE文本
func unicodeBytes(data []byte) (string, error) {
	if len(data) == 0 || len(data)%2 != 0 {
		return "", fmt.Errorf("invalid UTF-16BE mapping")
	}
	values := make([]uint16, len(data)/2)
	for n := range values {
		values[n] = binary.BigEndian.Uint16(data[n*2:])
	}
	for n := 0; n < len(values); n++ {
		v := values[n]
		if v >= 0xD800 && v <= 0xDBFF {
			if n+1 == len(values) || values[n+1] < 0xDC00 || values[n+1] > 0xDFFF {
				return "", errInvalidUnicodeSurrogate
			}
			n++
		} else if v >= 0xDC00 && v <= 0xDFFF {
			return "", errInvalidUnicodeSurrogate
		}
	}
	return string(utf16.Decode(values)), nil
}
