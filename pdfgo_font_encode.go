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
	"slices"
	"unicode/utf8"
)

// fontEncodedGlyph 保存源字体字符码及对应字形，不改变字体资源
type fontEncodedGlyph struct {
	code  string
	glyph Glyph
}

// fontEncodingNode 按Unicode字符索引源字体的多字符映射
type fontEncodingNode struct {
	children map[rune]*fontEncodingNode
	entry    *fontEncodedGlyph
}

// Encode 将Unicode文本编码为源字体字符码，不执行整形、换行或字体替换
// 优先使用可完整编码的单字符序列；缺少映射或字形时返回错误，不返回部分结果
// 入参: text Unicode文本
// 返回: []byte 可用于该字体文字显示操作的字符码, error 编码错误
func (f *Font) Encode(text string) ([]byte, error) {
	return f.EncodeContext(context.Background(), text)
}

// EncodeContext 将Unicode文本编码为源字体字符码，编码过程可取消
// 入参: ctx 取消上下文, text Unicode文本
// 返回: []byte 可用于该字体文字显示操作的字符码, error 编码或取消错误
func (f *Font) EncodeContext(ctx context.Context, text string) ([]byte, error) {
	if ctx == nil || f == nil {
		return nil, fmt.Errorf("missing font or encoding context")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !utf8.ValidString(text) {
		return nil, fmt.Errorf("invalid UTF-8 font text")
	}
	if text == "" {
		return nil, nil
	}
	wanted := make(map[rune]bool)
	for _, ch := range text {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		wanted[ch] = true
	}
	lookup, err := f.characterLookup(ctx, wanted)
	if err != nil {
		return nil, err
	}
	var result []byte
	complete := true
	for _, ch := range text {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		entry, ok := lookup[ch]
		if !ok {
			complete = false
			break
		}
		result = append(result, entry.code...)
	}
	if complete {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		return result, nil
	}
	sequences, err := f.sequenceLookup(ctx, text)
	if err != nil {
		return nil, err
	}
	entries, err := encodeFontSequence(ctx, text, lookup, sequences)
	if err != nil {
		return nil, err
	}
	result = nil
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		result = append(result, entry.code...)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return result, nil
}

// characterCodes 收集所需文字的候选字符码，优先保留显式Unicode映射
// 入参: ctx 取消上下文, wanted 所需字符
// 返回: []string 候选字符码, error 取消错误
func (f *Font) characterCodes(ctx context.Context, wanted map[rune]bool) ([]string, error) {
	return f.encodingCodes(ctx, func(value string) bool {
		ch, n := utf8.DecodeRuneInString(value)
		return n == len(value) && wanted[ch]
	}, !f.composite)
}

// encodingCodes 收集符合条件的字符码，复合字体沿标准字符集反查CID
// 入参: ctx 取消上下文, accept Unicode映射筛选, simple 是否包含简单字体字符码
// 返回: []string 候选字符码, error 取消错误
func (f *Font) encodingCodes(ctx context.Context, accept func(string) bool, simple bool) ([]string, error) {
	var codes []string
	for code, value := range f.Unicode {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if accept(value) {
			codes = append(codes, code)
		}
	}
	slices.Sort(codes)
	codes = slices.Compact(codes)
	var extra []string
	if simple {
		for i := 0; i < 256; i++ {
			extra = append(extra, string([]byte{byte(i)}))
		}
	}
	if f.composite {
		var cids []uint16
		for raw, value := range f.cidUnicode {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			if len(raw) == 2 && accept(value) {
				cid := uint16(raw[0])<<8 | uint16(raw[1])
				if len(f.Program) == 0 || f.hasCIDGlyph(uint32(cid)) {
					cids = append(cids, cid)
				}
			}
		}
		slices.Sort(cids)
		if f.cidMap != nil {
			var err error
			extra, err = f.cidMap.characterCodes(ctx, cids)
			if err != nil {
				return nil, err
			}
		} else {
			for _, cid := range cids {
				extra = append(extra, string([]byte{byte(cid >> 8), byte(cid)}))
			}
		}
	}
	slices.Sort(extra)
	codes = append(codes, slices.Compact(extra)...)
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return codes, nil
}

// characterLookup 查找单字符的原始编码，保留字体字宽和书写方向度量
// 入参: ctx 取消上下文, wanted 所需字符
// 返回: map[rune]fontEncodedGlyph 编码与字形, error 取消错误
func (f *Font) characterLookup(ctx context.Context, wanted map[rune]bool) (map[rune]fontEncodedGlyph, error) {
	codes, err := f.characterCodes(ctx, wanted)
	if err != nil {
		return nil, err
	}
	lookup := make(map[rune]fontEncodedGlyph)
	for _, code := range codes {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		glyph, ok, _ := f.encodingGlyph(ctx, code)
		if !ok || utf8.RuneCountInString(glyph.Text) != 1 {
			continue
		}
		ch, _ := utf8.DecodeRuneInString(glyph.Text)
		entry := fontEncodedGlyph{code: code, glyph: glyph}
		if wanted['\u00a0'] && !f.composite && f.encoding == "WinAnsiEncoding" && code == "\xa0" && glyph.Name == "space" && glyph.Text == " " && f.Unicode[code] == "" {
			lookup['\u00a0'] = entry
		}
		if _, ok := lookup[ch]; !ok && wanted[ch] {
			lookup[ch] = entry
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return lookup, nil
}

// sequenceLookup 收集原文可能使用的多字符编码，不合成字形或修改映射
// 入参: ctx 取消上下文, text Unicode原文
// 返回: map[string]fontEncodedGlyph 多字符编码, error 取消错误
func (f *Font) sequenceLookup(ctx context.Context, text string) (map[string]fontEncodedGlyph, error) {
	wanted := make(map[rune]bool)
	for _, ch := range text {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		wanted[ch] = true
	}
	accept := func(value string) bool {
		ch, n := utf8.DecodeRuneInString(value)
		return len(value) <= len(text) && n < len(value) && wanted[ch] && utf8.ValidString(value)
	}
	codes, err := f.encodingCodes(ctx, accept, false)
	if err != nil {
		return nil, err
	}
	lookup := make(map[string]fontEncodedGlyph)
	for _, code := range codes {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		glyph, ok, _ := f.encodingGlyph(ctx, code)
		if !ok || !accept(glyph.Text) {
			continue
		}
		if _, ok := lookup[glyph.Text]; !ok {
			lookup[glyph.Text] = fontEncodedGlyph{code: code, glyph: glyph}
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return lookup, nil
}

// encodingGlyph 验证候选字符码对应单个已定义字形，不使用缺省字形代替原文
// 入参: ctx 取消上下文, code 字符码
// 返回: Glyph 原字形, bool 是否可用于编码, error 解码错误
func (f *Font) encodingGlyph(ctx context.Context, code string) (Glyph, bool, error) {
	if f.cidMap != nil {
		data := []byte(code)
		if n, valid := f.cidMap.next(data); !valid || n != len(data) {
			return Glyph{}, false, nil
		}
		mapped := false
		for current := f.cidMap; current != nil; current = current.base {
			if cid, found := current.mapped(data); found {
				mapped = cid != 0
				break
			}
		}
		if !mapped {
			return Glyph{}, false, nil
		}
	}
	glyphs, err := f.DecodeContext(ctx, []byte(code))
	if err != nil {
		return Glyph{}, false, err
	}
	if len(glyphs) != 1 || glyphs[0].HasID && glyphs[0].ID == 0 || glyphs[0].Name == ".notdef" {
		return Glyph{}, false, nil
	}
	return glyphs[0], true, nil
}

// encodeFontSequence 选择可完整覆盖原文的编码，不在连字匹配失败时丢弃字符
// 入参: ctx 取消上下文, text 原文, lookup 单字符编码, sequences 多字符编码
// 返回: []*fontEncodedGlyph 只读编码与字形, error 缺字或取消错误
func encodeFontSequence(ctx context.Context, text string, lookup map[rune]fontEncodedGlyph, sequences map[string]fontEncodedGlyph) ([]*fontEncodedGlyph, error) {
	singles := make(map[rune]*fontEncodedGlyph, len(lookup))
	for ch, entry := range lookup {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		singles[ch] = &entry
	}
	root := fontEncodingNode{}
	for value, entry := range sequences {
		node := &root
		for _, ch := range value {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			if node.children == nil {
				node.children = make(map[rune]*fontEncodingNode)
			}
			if node.children[ch] == nil {
				node.children[ch] = &fontEncodingNode{}
			}
			node = node.children[ch]
		}
		node.entry = &entry
	}
	runes := []rune(text)
	next := make([]int, len(runes))
	entries := make([]*fontEncodedGlyph, len(runes))
	for i := len(runes) - 1; i >= 0; i-- {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if i+1 == len(runes) || next[i+1] != 0 {
			if entry, ok := singles[runes[i]]; ok {
				entries[i], next[i] = entry, i+1
				continue
			}
		}
		node := &root
		for j := i; j < len(runes); j++ {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			node = node.children[runes[j]]
			if node == nil {
				break
			}
			if node.entry != nil && (j+1 == len(runes) || next[j+1] != 0) {
				entries[i], next[i] = node.entry, j+1
			}
		}
	}
	result := entries[:0]
	for i := 0; i < len(runes); i = next[i] {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if next[i] == 0 {
			return nil, &UnsupportedError{Feature: fmt.Sprintf("font cannot encode text at U+%04X", runes[i])}
		}
		result = append(result, entries[i])
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	clear(entries[len(result):])
	return result, nil
}
