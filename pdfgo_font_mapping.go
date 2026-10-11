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
	"unicode/utf8"
)

// GlyphMap 汇总Unicode单字符到内嵌字体字形编号的映射，不拆分多字符映射
// 无法确定字形编号的字符不纳入结果，返回的映射可由调用方独立修改
// 返回: map[rune]uint16 字符与字形编号, error 映射错误
func (f *Font) GlyphMap() (map[rune]uint16, error) {
	return f.GlyphMapContext(context.Background())
}

// GlyphMapContext 按取消上下文汇总单字符字形映射，显式ToUnicode优先
// 入参: ctx 取消上下文
// 返回: map[rune]uint16 独立字形映射, error 映射或取消错误
func (f *Font) GlyphMapContext(ctx context.Context) (map[rune]uint16, error) {
	if f == nil || ctx == nil {
		return nil, fmt.Errorf("missing font or mapping context")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if len(f.Program) == 0 {
		return make(map[rune]uint16), nil
	}
	codes, err := f.encodingCodes(ctx, func(value string) bool {
		return utf8.ValidString(value) && utf8.RuneCountInString(value) == 1
	}, !f.composite && f.Subtype != "Type0")
	if err != nil {
		return nil, err
	}
	mapping := make(map[rune]uint16)
	for _, code := range codes {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		glyph, ok, err := f.encodingGlyph(ctx, code)
		if err != nil {
			return nil, err
		}
		if !ok || !glyph.HasID || !utf8.ValidString(glyph.Text) || utf8.RuneCountInString(glyph.Text) != 1 {
			continue
		}
		ch, _ := utf8.DecodeRuneInString(glyph.Text)
		if _, exists := mapping[ch]; !exists {
			mapping[ch] = glyph.ID
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return mapping, nil
}
