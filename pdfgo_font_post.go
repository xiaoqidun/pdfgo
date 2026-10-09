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
	"fmt"
	"strings"
	"sync"
)

// fontPostMapping 延迟解析只读名称表，字体副本共用映射而不复制同步状态
type fontPostMapping struct {
	program []byte
	mu      sync.RWMutex
	ready   bool
	glyphs  map[string]uint16
	err     error
}

// macPostNames 保存TrueType标准Macintosh字形编号对应的名称，不等同于字符编码
var macPostNames = func() []string {
	names := make([]string, 0, 258)
	names = append(names, ".notdef", ".null", "nonmarkingreturn")
	names = append(names, pdfMacRomanNames[32:127]...)
	names = append(names, pdfMacRomanNames[128:256]...)
	names[172] = "nonbreakingspace"
	return append(names, strings.Fields(`
Lslash lslash Scaron scaron Zcaron zcaron brokenbar Eth eth Yacute yacute Thorn thorn minus multiply onesuperior
twosuperior threesuperior onehalf onequarter threequarters franc Gbreve gbreve Idotaccent Scedilla scedilla Cacute cacute Ccaron ccaron dcroat
`)...)
}()

// lookup 查询名称映射，空名称表返回未定义字形
// 入参: name 字形名称
// 返回: uint16 字形编号, error 实际使用的名称表错误
func (p *fontPostMapping) lookup(name string) (uint16, error) {
	return p.lookupContext(context.Background(), name)
}

// lookupContext 查询字形名称，取消的解析不进入共用缓存
// 入参: ctx 取消上下文, name 字形名称
// 返回: uint16 字形编号, error 名称表或取消错误
func (p *fontPostMapping) lookupContext(ctx context.Context, name string) (uint16, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if p == nil {
		return 0, nil
	}
	p.mu.RLock()
	if p.ready {
		glyph, err := p.glyphs[name], p.err
		p.mu.RUnlock()
		if cancelled := ctx.Err(); cancelled != nil {
			return 0, cancelled
		}
		return glyph, err
	}
	p.mu.RUnlock()
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if !p.ready {
		glyphs, err := fontPostGlyphsContext(ctx, p.program)
		if cancelled := ctx.Err(); cancelled != nil {
			return 0, cancelled
		}
		p.glyphs, p.err, p.ready = glyphs, err, true
	}
	return p.glyphs[name], p.err
}

// fontPostGlyphsContext 解析字形名称表，检查数组及名称展开的取消
// 入参: ctx 取消上下文, data SFNT字体数据
// 返回: map[string]uint16 完整名称映射, error 解析或取消错误
func fontPostGlyphsContext(ctx context.Context, data []byte) (map[string]uint16, error) {
	table, err := fontTableContext(ctx, data, "post")
	if err != nil || len(table) == 0 {
		return nil, err
	}
	if len(table) < 32 {
		return nil, fmt.Errorf("truncated TrueType post header")
	}
	format := binary.BigEndian.Uint32(table)
	if format == 0x30000 {
		return nil, nil
	}
	maxp, err := fontTableContext(ctx, data, "maxp")
	if err != nil {
		return nil, err
	}
	if len(maxp) < 6 {
		return nil, fmt.Errorf("missing TrueType post glyph count")
	}
	count := int(binary.BigEndian.Uint16(maxp[4:]))
	result := make(map[string]uint16)
	add := func(index int, name string) {
		if name != "" && name != ".notdef" {
			if _, exists := result[name]; !exists {
				result[name] = uint16(index)
			}
		}
	}
	switch format {
	case 0x10000:
		if count != len(macPostNames) {
			return nil, fmt.Errorf("invalid TrueType post standard glyph count")
		}
		for index, name := range macPostNames {
			add(index, name)
		}
	case 0x20000, 0x25000:
		if len(table) < 34 || int(binary.BigEndian.Uint16(table[32:])) != count {
			return nil, fmt.Errorf("invalid TrueType post glyph count")
		}
		if format == 0x25000 {
			if count > len(table)-34 {
				return nil, fmt.Errorf("truncated TrueType post offsets")
			}
			for index, offset := range table[34 : 34+count] {
				if index&255 == 0 {
					if err := ctx.Err(); err != nil {
						return nil, err
					}
				}
				nameIndex := index + int(int8(offset))
				if nameIndex < 0 || nameIndex >= len(macPostNames) {
					return nil, fmt.Errorf("invalid TrueType post name offset")
				}
				add(index, macPostNames[nameIndex])
			}
			break
		}
		if count > (len(table)-34)/2 {
			return nil, fmt.Errorf("truncated TrueType post indices")
		}
		customCount := 0
		for index := range count {
			if index&255 == 0 {
				if err := ctx.Err(); err != nil {
					return nil, err
				}
			}
			name := int(binary.BigEndian.Uint16(table[34+index*2:]))
			customCount = max(customCount, name-len(macPostNames)+1)
		}
		position := 34 + count*2
		if customCount > len(table)-position {
			return nil, fmt.Errorf("truncated TrueType post names")
		}
		custom := make([]string, customCount)
		for index := range custom {
			if index&255 == 0 {
				if err := ctx.Err(); err != nil {
					return nil, err
				}
			}
			if position >= len(table) {
				return nil, fmt.Errorf("truncated TrueType post name")
			}
			length := int(table[position])
			position++
			if length > len(table)-position {
				return nil, fmt.Errorf("truncated TrueType post name")
			}
			custom[index] = string(table[position : position+length])
			position += length
		}
		for index := range count {
			if index&255 == 0 {
				if err := ctx.Err(); err != nil {
					return nil, err
				}
			}
			name := int(binary.BigEndian.Uint16(table[34+index*2:]))
			if name < len(macPostNames) {
				add(index, macPostNames[name])
			} else {
				add(index, custom[name-len(macPostNames)])
			}
		}
	case 0x40000:
		if count > (len(table)-32)/2 {
			return nil, fmt.Errorf("truncated TrueType post character codes")
		}
		for index := range count {
			if index&255 == 0 {
				if err := ctx.Err(); err != nil {
					return nil, err
				}
			}
			code := binary.BigEndian.Uint16(table[32+index*2:])
			if code != 0xffff {
				add(index, fmt.Sprintf("a%04X", code))
			}
		}
	default:
		return nil, &UnsupportedError{Feature: fmt.Sprintf("TrueType post format %#x", format)}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return result, nil
}
