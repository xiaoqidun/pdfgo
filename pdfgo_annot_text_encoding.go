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
	"unicode"
	"unicode/utf8"
)

// annotationEncodingNode 按Unicode字符索引字体已有的多字符编码
type annotationEncodingNode struct {
	children map[rune]*annotationEncodingNode
	letter   annotationLetter
}

// annotationEncodedLetters 复用字体明确声明的连字编码，优先保留可完整编码的单字符序列
// 入参: ctx 取消上下文, font 原字体, text 原文, lookup 单字符编码
// 返回: []annotationLetter 编码与度量, error 缺字、编码或取消错误
func annotationEncodedLetters(ctx context.Context, font *Font, text string, lookup map[rune]annotationLetter) ([]annotationLetter, error) {
	root := annotationEncodingNode{}
	wanted := make(map[rune]bool)
	for _, char := range text {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		wanted[char] = true
	}
	codes := make([]string, 0, len(font.Unicode))
	for code, value := range font.Unicode {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		first, size := utf8.DecodeRuneInString(value)
		if len(code) != 0 && len(value) <= len(text) && wanted[first] && size < len(value) && utf8.ValidString(value) {
			codes = append(codes, code)
		}
	}
	if len(codes) == 0 {
		return nil, &UnsupportedError{Feature: "field font cannot encode source text"}
	}
	slices.Sort(codes)
	for _, code := range codes {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		glyphs, err := font.DecodeContext(ctx, []byte(code))
		if err != nil || len(glyphs) != 1 || glyphs[0].Text != font.Unicode[code] {
			continue
		}
		glyph := glyphs[0]
		valid := true
		for _, char := range glyph.Text {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			if unicode.IsControl(char) || unicode.IsSpace(char) {
				valid = false
				break
			}
		}
		if !valid {
			continue
		}
		node := &root
		for _, char := range glyph.Text {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			if node.children == nil {
				node.children = make(map[rune]*annotationEncodingNode)
			}
			if node.children[char] == nil {
				node.children[char] = &annotationEncodingNode{}
			}
			node = node.children[char]
		}
		if node.letter.code == "" {
			char, _ := utf8.DecodeRuneInString(glyph.Text)
			node.letter = annotationLetter{text: char, code: code, width: glyph.Width, word: glyph.WordSpace}
			if font.Vertical {
				node.letter.vertical = &glyph.Vertical
			}
		}
	}
	runes := []rune(text)
	next := make([]int, len(runes))
	letters := make([]annotationLetter, len(runes))
	for i := len(runes) - 1; i >= 0; i-- {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if i+1 == len(runes) || next[i+1] != 0 {
			if runes[i] == '\n' {
				letters[i], next[i] = annotationLetter{text: '\n'}, i+1
				continue
			}
			if letter, ok := lookup[runes[i]]; ok {
				letters[i], next[i] = letter, i+1
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
			if node.letter.code != "" && (j+1 == len(runes) || next[j+1] != 0) {
				letters[i], next[i] = node.letter, j+1
			}
		}
	}
	result := make([]annotationLetter, 0, len(runes))
	for i := 0; i < len(runes); i = next[i] {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if next[i] == 0 {
			return nil, &UnsupportedError{Feature: fmt.Sprintf("field font cannot encode text at U+%04X", runes[i])}
		}
		result = append(result, letters[i])
	}
	return result, ctx.Err()
}
