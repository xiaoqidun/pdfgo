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
	"strconv"
	"unicode/utf16"
)

// maxCMapEntries 限制展开为字符映射表时的最大总条目数，防止大范围区间展开耗尽内存
const maxCMapEntries uint64 = 262144

// maxCMapTargetBytes 限制单个Unicode映射目标编码的最大字节数
const maxCMapTargetBytes = 4096

// maxCMapTotalBytes 限制Unicode映射表累计展开与保存的最大总字节数
const maxCMapTotalBytes uint64 = 8 << 20

// unicodeMapBytes 统计Unicode映射表已占用的键与文本总字节数，超限时返回错误
// 入参: ctx 取消上下文, mapping 字符映射
// 返回: uint64 总字节数, error 超限或取消错误
func unicodeMapBytes(ctx context.Context, mapping UnicodeMap) (uint64, error) {
	var total uint64
	index := 0
	for code, text := range mapping {
		if index&255 == 0 {
			if err := ctx.Err(); err != nil {
				return 0, err
			}
		}
		index++
		added := uint64(len(code) + len(text))
		if added > maxCMapTotalBytes-total {
			return 0, fmt.Errorf("%w: unicode cmap byte limit exceeded", ErrStreamLimitExceeded)
		}
		total += added
	}
	return total, ctx.Err()
}

// errInvalidUnicodeSurrogate 表示Unicode映射中的代理项配对无效
var errInvalidUnicodeSurrogate = errors.New("invalid Unicode surrogate")

// UnicodeMap 保存字符码到Unicode文本的映射，不将字符码等同于字形编号
type UnicodeMap map[string]string

// ParseUnicodeMap 读取ToUnicode映射中的字符与范围定义
// 入参: data 已解码的CMap数据
// 返回: UnicodeMap 字符映射, error 错误信息
func ParseUnicodeMap(data []byte) (UnicodeMap, error) {
	return ParseUnicodeMapContext(context.Background(), data)
}

// ParseUnicodeMapContext 读取Unicode映射，在解析、展开及继承时检查取消
// 入参: ctx 取消上下文, data 已解码的CMap数据
// 返回: UnicodeMap 完整映射, error 解析或取消错误
func ParseUnicodeMapContext(ctx context.Context, data []byte) (UnicodeMap, error) {
	if ctx == nil {
		return nil, fmt.Errorf("invalid CMap context")
	}
	return parseUnicodeMapContext(ctx, data, nil)
}

// parseUnicodeMapContext 读取映射及命名继承，取消时不返回部分结果
// 入参: ctx 取消上下文, data 映射数据, active 当前命名继承链
// 返回: UnicodeMap 独立映射, error 解析或取消错误
func parseUnicodeMapContext(ctx context.Context, data []byte, active map[Name]bool) (UnicodeMap, error) {
	result := UnicodeMap{}
	if err := parseUnicodeMapIntoContext(ctx, result, data, active); err != nil {
		return nil, err
	}
	return result, nil
}

// parseUnicodeMapIntoContext 读取映射及命名继承并增量写入目标表，检查取消、总条目与总字节上限
// 入参: ctx 取消上下文, target 目标映射, data 映射数据, active 当前命名继承链
// 返回: error 解析、超限或取消错误
func parseUnicodeMapIntoContext(ctx context.Context, target UnicodeMap, data []byte, active map[Name]bool) error {
	p := objectParser{data: data}
	var inherited UnicodeMap
	var localWritten map[string]bool
	trackLocal := len(target) > 0
	hasUseCMap := false
	wroteLocal := false
	var expanded uint64
	totalBytes, err := unicodeMapBytes(ctx, target)
	if err != nil {
		return err
	}
	previous := ""
	var name Name
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		p.skipSpace()
		if p.pos == len(data) {
			index := 0
			for code, text := range inherited {
				if index&255 == 0 {
					if err := ctx.Err(); err != nil {
						return err
					}
				}
				index++
				if trackLocal {
					if localWritten[code] {
						continue
					}
					old, exists := target[code]
					if !exists && uint64(len(target)) >= maxCMapEntries {
						return fmt.Errorf("%w: unicode cmap entry limit exceeded", ErrStreamLimitExceeded)
					}
					add := uint64(len(code) + len(text))
					if exists {
						if len(text) > len(old) {
							add = uint64(len(text) - len(old))
						} else {
							add = 0
						}
					}
					if add > maxCMapTotalBytes-totalBytes {
						return fmt.Errorf("%w: unicode cmap byte limit exceeded", ErrStreamLimitExceeded)
					}
					totalBytes += add
					target[code] = text
					continue
				}
				if _, exists := target[code]; !exists {
					if uint64(len(target)) >= maxCMapEntries {
						return fmt.Errorf("%w: unicode cmap entry limit exceeded", ErrStreamLimitExceeded)
					}
					add := uint64(len(code) + len(text))
					if add > maxCMapTotalBytes-totalBytes {
						return fmt.Errorf("%w: unicode cmap byte limit exceeded", ErrStreamLimitExceeded)
					}
					totalBytes += add
					target[code] = text
				}
			}
			return ctx.Err()
		}
		if b := p.data[p.pos]; b == '/' || b == '(' || b == '<' || b == '[' {
			object, err := p.object()
			if err != nil {
				return err
			}
			name, _ = object.(Name)
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
			if name == "" || hasUseCMap {
				return p.fail("invalid ToUnicode CMap inheritance")
			}
			loaded, err := loadUnicodeCMapContext(ctx, name, active)
			if err != nil {
				return err
			}
			hasUseCMap = true
			if !wroteLocal {
				totalBytes, err = copyUnicodeMapContext(ctx, target, loaded)
				if err != nil {
					return err
				}
			} else {
				inherited = loaded
			}
			name = ""
			continue
		}
		if token != "beginbfchar" && token != "beginbfrange" {
			previous = token
			name = ""
			continue
		}
		count, err := strconv.Atoi(previous)
		if err != nil || count < 0 || count > 65536 {
			return p.fail("invalid CMap mapping count")
		}
		for index := range count {
			if index&255 == 0 {
				if err := ctx.Err(); err != nil {
					return err
				}
			}
			from, err := p.object()
			if err != nil {
				return err
			}
			first, ok := from.(String)
			if !ok || len(first) == 0 || len(first) > 4 {
				return p.fail("invalid CMap source code")
			}
			if token == "beginbfchar" {
				to, err := p.object()
				if err != nil {
					return err
				}
				str, ok := to.(String)
				if !ok {
					return p.fail("invalid CMap Unicode value")
				}
				if len(str) > maxCMapTargetBytes {
					return fmt.Errorf("%w: unicode cmap value limit exceeded", ErrStreamLimitExceeded)
				}
				if expanded >= maxCMapEntries {
					return fmt.Errorf("%w: unicode cmap entry limit exceeded", ErrStreamLimitExceeded)
				}
				expanded++
				rawBytes := uint64(len(first) + len(str))
				if rawBytes > maxCMapTotalBytes-totalBytes {
					return fmt.Errorf("%w: unicode cmap byte limit exceeded", ErrStreamLimitExceeded)
				}
				totalBytes += rawBytes
				value, err := unicodeBytes(str)
				if err != nil {
					return err
				}
				if len(value) > len(str) {
					extra := uint64(len(value) - len(str))
					if extra > maxCMapTotalBytes-totalBytes {
						return fmt.Errorf("%w: unicode cmap byte limit exceeded", ErrStreamLimitExceeded)
					}
					totalBytes += extra
				}
				key := string(first)
				if _, exists := target[key]; !exists && uint64(len(target)) >= maxCMapEntries {
					return fmt.Errorf("%w: unicode cmap entry limit exceeded", ErrStreamLimitExceeded)
				}
				target[key] = value
				if trackLocal {
					if localWritten == nil {
						localWritten = map[string]bool{}
					}
					localWritten[key] = true
				}
				wroteLocal = true
			} else {
				lastObject, err := p.object()
				if err != nil {
					return err
				}
				last, ok := lastObject.(String)
				if !ok || len(last) != len(first) {
					return p.fail("invalid CMap source range")
				}
				a, b := codeNumber(first), codeNumber(last)
				if b < a || b-a > 65535 {
					return p.fail("excessive CMap source range")
				}
				span := b - a + 1
				if span > maxCMapEntries-expanded {
					return fmt.Errorf("%w: unicode cmap entry limit exceeded", ErrStreamLimitExceeded)
				}
				expanded += span
				to, err := p.object()
				if err != nil {
					return err
				}
				var baseTarget String
				var listTarget Array
				var scratch String
				switch v := to.(type) {
				case String:
					if len(v) > maxCMapTargetBytes {
						return fmt.Errorf("%w: unicode cmap value limit exceeded", ErrStreamLimitExceeded)
					}
					baseTarget = v
					scratch = make(String, len(v))
				case Array:
					if uint64(len(v)) != span {
						return p.fail("CMap destination count mismatch")
					}
					listTarget = v
				default:
					return p.fail("invalid CMap destination")
				}
				for code := a; code <= b; code++ {
					if (code-a)&255 == 0 {
						if err := ctx.Err(); err != nil {
							return err
						}
					}
					var targetCode String
					if listTarget == nil {
						copy(scratch, baseTarget)
						carry := code - a
						for n := len(scratch) - 1; n >= 0 && carry != 0; n-- {
							carry += uint64(scratch[n])
							scratch[n] = byte(carry)
							carry >>= 8
						}
						if carry != 0 {
							return p.fail("CMap destination overflow")
						}
						targetCode = scratch
					} else {
						var ok bool
						targetCode, ok = listTarget[code-a].(String)
						if !ok {
							return p.fail("invalid CMap destination")
						}
						if len(targetCode) > maxCMapTargetBytes {
							return fmt.Errorf("%w: unicode cmap value limit exceeded", ErrStreamLimitExceeded)
						}
					}
					rawBytes := uint64(len(first) + len(targetCode))
					if rawBytes > maxCMapTotalBytes-totalBytes {
						return fmt.Errorf("%w: unicode cmap byte limit exceeded", ErrStreamLimitExceeded)
					}
					totalBytes += rawBytes
					value, err := unicodeBytes(targetCode)
					if err != nil {
						if errors.Is(err, errInvalidUnicodeSurrogate) {
							continue
						}
						return err
					}
					if len(value) > len(targetCode) {
						extra := uint64(len(value) - len(targetCode))
						if extra > maxCMapTotalBytes-totalBytes {
							return fmt.Errorf("%w: unicode cmap byte limit exceeded", ErrStreamLimitExceeded)
						}
						totalBytes += extra
					}
					var raw [4]byte
					keyBytes := raw[:len(first)]
					v := code
					for n := len(keyBytes) - 1; n >= 0; n-- {
						keyBytes[n] = byte(v)
						v >>= 8
					}
					key := string(keyBytes)
					if _, exists := target[key]; !exists && uint64(len(target)) >= maxCMapEntries {
						return fmt.Errorf("%w: unicode cmap entry limit exceeded", ErrStreamLimitExceeded)
					}
					target[key] = value
					if trackLocal {
						if localWritten == nil {
							localWritten = map[string]bool{}
						}
						localWritten[key] = true
					}
					wroteLocal = true
				}
			}
		}
		end := "endbfchar"
		if token == "beginbfrange" {
			end = "endbfrange"
		}
		if p.token() != end {
			return p.fail("missing CMap block terminator")
		}
		previous = ""
	}
}

// codeNumber 将最多4字节的字符码解释为无符号整数
// 入参: code 高位字节在前的字符码
// 返回: uint64 字符码数值
func codeNumber(code []byte) uint64 {
	var value uint64
	for _, b := range code {
		value = value<<8 | uint64(b)
	}
	return value
}

// unicodeBytes 严格解析CMap使用的UTF-16BE文本
// 入参: data UTF-16BE编码数据
// 返回: string Unicode文本, error 编码错误
func unicodeBytes(data []byte) (string, error) {
	if len(data)%2 != 0 {
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
