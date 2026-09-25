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
	"bytes"
	"fmt"
	"unicode/utf8"
)

var pdfDocCharacters = map[byte]rune{
	0x18: 0x02d8, 0x19: 0x02c7, 0x1a: 0x02c6, 0x1b: 0x02d9,
	0x1c: 0x02dd, 0x1d: 0x02db, 0x1e: 0x02da, 0x1f: 0x02dc, 0x7f: 0,
	0x80: 0x2022, 0x81: 0x2020, 0x82: 0x2021, 0x83: 0x2026, 0x84: 0x2014,
	0x85: 0x2013, 0x86: 0x0192, 0x87: 0x2044, 0x88: 0x2039, 0x89: 0x203a,
	0x8a: 0x2212, 0x8b: 0x2030, 0x8c: 0x201e, 0x8d: 0x201c, 0x8e: 0x201d,
	0x8f: 0x2018, 0x90: 0x2019, 0x91: 0x201a, 0x92: 0x2122, 0x93: 0xfb01,
	0x94: 0xfb02, 0x95: 0x0141, 0x96: 0x0152, 0x97: 0x0160, 0x98: 0x0178,
	0x99: 0x017d, 0x9a: 0x0131, 0x9b: 0x0142, 0x9c: 0x0153, 0x9d: 0x0161,
	0x9e: 0x017e, 0x9f: 0, 0xa0: 0x20ac,
}

// DecodeTextString按照PDF文本串编码读取Unicode文字
// 入参: value PDF字符串
// 返回: string Unicode文字, error无效编码
func DecodeTextString(value String) (string, error) {
	data := []byte(value)
	if bytes.HasPrefix(data, []byte{0xfe, 0xff}) {
		if len(data) == 2 {
			return "", nil
		}
		return unicodeBytes(data[2:])
	}
	if bytes.HasPrefix(data, []byte{0xff, 0xfe}) {
		body := data[2:]
		if len(body) == 0 {
			return "", nil
		}
		if len(body)%2 != 0 {
			return "", fmt.Errorf("invalid UTF-16 text string")
		}
		converted := make([]byte, len(body))
		for i := 0; i < len(body); i += 2 {
			converted[i], converted[i+1] = body[i+1], body[i]
		}
		return unicodeBytes(converted)
	}
	if bytes.HasPrefix(data, []byte{0xef, 0xbb, 0xbf}) {
		if !utf8.Valid(data[3:]) {
			return "", fmt.Errorf("invalid UTF-8 text string")
		}
		return string(data[3:]), nil
	}
	runes := make([]rune, 0, len(data))
	for _, code := range data {
		if mapped, ok := pdfDocCharacters[code]; ok {
			if mapped == 0 {
				return "", fmt.Errorf("undefined PDFDocEncoding byte %02x", code)
			}
			runes = append(runes, mapped)
		} else {
			runes = append(runes, rune(code))
		}
	}
	return string(runes), nil
}
