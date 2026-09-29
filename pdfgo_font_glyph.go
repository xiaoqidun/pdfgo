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
	"embed"
	"strconv"
	"strings"
	"sync"
)

// adobeGlyphResources 保存Adobe字形映射、命名资源及授权文件
// https://github.com/adobe-type-tools/agl-aglfn
//
//go:embed assets/glyph/agl-aglfn
var adobeGlyphResources embed.FS

// adobeGlyphNames 按需构建字形名称与Unicode映射
var adobeGlyphNames = sync.OnceValue(func() map[string]string {
	return loadGlyphNames("glyphlist.txt")
})

// dingbatGlyphNames 按需合并特殊符号与通用字形名称映射
var dingbatGlyphNames = sync.OnceValue(func() map[string]string {
	names := loadGlyphNames("glyphlist.txt")
	for name, value := range loadGlyphNames("zapfdingbats.txt") {
		names[name] = value
	}
	return names
})

// loadGlyphNames 加载内置字形映射
// 入参: name 内置映射文件名
// 返回: map[string]string 名称映射
func loadGlyphNames(name string) map[string]string {
	data, err := adobeGlyphResources.ReadFile("assets/glyph/agl-aglfn/" + name)
	if err != nil {
		panic(err)
	}
	return parseGlyphNames(data)
}

// parseGlyphNames 读取Adobe字形列表的名称和Unicode字段
// 入参: data 字形列表资源
// 返回: map[string]string 名称映射
func parseGlyphNames(data []byte) map[string]string {
	names := make(map[string]string)
	for _, line := range strings.Split(string(data), "\n") {
		if line == "" || line[0] == '#' {
			continue
		}
		name, codes, ok := strings.Cut(line, ";")
		if !ok {
			continue
		}
		var value strings.Builder
		for _, code := range strings.Fields(codes) {
			n, err := strconv.ParseUint(code, 16, 32)
			if err != nil {
				continue
			}
			value.WriteRune(rune(n))
		}
		names[name] = value.String()
	}
	return names
}

// glyphNameUnicode 按Adobe字形名称规则取得Unicode文本
// 入参: name 字形名称
// 返回: string Unicode文本, bool 是否有明确映射
func glyphNameUnicode(name string) (string, bool) {
	return glyphNameUnicodeMap(name, adobeGlyphNames())
}

// glyphNameUnicodeMap 按指定字形列表解析名称、后缀及组合字形
// 入参: name 字形名称, names 字形列表
// 返回: string Unicode文本, bool 是否有明确映射
func glyphNameUnicodeMap(name string, names map[string]string) (string, bool) {
	name, _, _ = strings.Cut(name, ".")
	if value, ok := names[name]; ok {
		return value, true
	}
	if strings.Contains(name, "_") {
		var value strings.Builder
		for _, part := range strings.Split(name, "_") {
			text, ok := glyphNameUnicodeMap(part, names)
			if !ok {
				return "", false
			}
			value.WriteString(text)
		}
		return value.String(), true
	}
	if strings.HasPrefix(name, "uni") && len(name) > 3 && (len(name)-3)%4 == 0 {
		var value strings.Builder
		for i := 3; i < len(name); i += 4 {
			n, err := strconv.ParseUint(name[i:i+4], 16, 16)
			if err != nil || n >= 0xD800 && n <= 0xDFFF {
				return "", false
			}
			value.WriteRune(rune(n))
		}
		return value.String(), true
	}
	if strings.HasPrefix(name, "u") && len(name) >= 5 && len(name) <= 7 {
		n, err := strconv.ParseUint(name[1:], 16, 32)
		if err == nil && n <= 0x10FFFF && (n < 0xD800 || n > 0xDFFF) {
			return string(rune(n)), true
		}
	}
	return "", false
}
