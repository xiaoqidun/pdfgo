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
	"unicode"
	"unicode/utf8"
)

// annotationEncodedLetters 复用源字体多字符编码，保持注释空白及换行边界
// 入参: ctx 取消上下文, font 原字体, text 原文, lookup 单字符编码
// 返回: []annotationLetter 编码与度量, error 缺字、编码或取消错误
func annotationEncodedLetters(ctx context.Context, font *Font, text string, lookup map[rune]annotationLetter) ([]annotationLetter, error) {
	sequences, err := font.sequenceLookup(ctx, text)
	if err != nil {
		return nil, err
	}
	for value := range sequences {
		for _, ch := range value {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			if unicode.IsControl(ch) || unicode.IsSpace(ch) {
				delete(sequences, value)
				break
			}
		}
	}
	singles := make(map[rune]fontEncodedGlyph, len(lookup)+1)
	for ch, letter := range lookup {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		glyph := Glyph{Text: string(ch), Width: letter.width, WordSpace: letter.word}
		if letter.vertical != nil {
			glyph.Vertical = *letter.vertical
		}
		singles[ch] = fontEncodedGlyph{code: letter.code, glyph: glyph}
	}
	singles['\n'] = fontEncodedGlyph{glyph: Glyph{Text: "\n"}}
	entries, err := encodeFontSequence(ctx, text, singles, sequences)
	if err != nil {
		return nil, err
	}
	letters := make([]annotationLetter, len(entries))
	for i, entry := range entries {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		ch, _ := utf8.DecodeRuneInString(entry.glyph.Text)
		letters[i] = annotationLetter{text: ch, code: entry.code, width: entry.glyph.Width, word: entry.glyph.WordSpace}
		if font.Vertical && ch != '\n' {
			letters[i].vertical = &entry.glyph.Vertical
		}
	}
	return letters, nil
}
