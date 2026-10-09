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
	"math"
	"strings"
	"unicode"
	"unicode/utf8"
)

// annotationRichGlyph 保存富文本字符及实际选用的字体和样式
type annotationRichGlyph struct {
	letter annotationLetter
	font   *annotationRichFont
	style  *annotationRichStyle
}

// annotationRichLine 保存排版行的范围、对齐及混合字体度量
type annotationRichLine struct {
	glyphs  []annotationRichGlyph
	style   *annotationRichStyle
	width   float64
	ascent  float64
	descent float64
	advance float64
}

// writeAnnotationRichText 生成富文本外观，保留混合字体、颜色和基线偏移
// 入参: ctx 取消上下文, annotation 注解, object 富文本, flags 字段标志, frame 内框, border 边框, resources 外观资源, content 内容
// 返回: error 富文本、布局或字体错误
func (r *Reader) writeAnnotationRichText(ctx context.Context, annotation Annotation, object Object, flags Integer, frame Rectangle, border *annotationBorder, resources Dictionary, content *strings.Builder) error {
	dict := annotation.Dictionary
	align := Integer(0)
	if value, err := r.Resolve(dict["Q"]); err != nil {
		return err
	} else if value != nil {
		var ok bool
		align, ok = value.(Integer)
		if !ok || align < 0 || align > 2 {
			return fmt.Errorf("invalid field justification")
		}
	}
	paragraphs, err := r.readAnnotationRich(ctx, object, dict["DS"], int(align))
	if err != nil {
		return err
	}
	visible := false
	for i := range paragraphs {
		for j := range paragraphs[i].runs {
			run := &paragraphs[i].runs[j]
			if flags&(1<<13) != 0 {
				run.text = strings.Repeat("*", utf8.RuneCountInString(run.text))
			}
			visible = visible || run.text != "" && (run.style.size == nil || *run.style.size != 0)
		}
	}
	if !visible {
		return nil
	}
	comb, err := r.annotationComb(dict, flags)
	if err != nil {
		return err
	}
	w, h := frame.XMax-frame.XMin, frame.YMax-frame.YMin
	inset := math.Min(math.Max(1, border.width)+1, math.Min(w, h)/2)
	x, y, width, height := frame.XMin+inset, frame.YMin+inset, w-2*inset, h-2*inset
	if width <= 0 || height <= 0 {
		return nil
	}
	p, name, stateContent, err := r.annotationTextState(ctx, dict["DA"], resources)
	if err != nil {
		return err
	}
	if p.state.font == nil || p.state.fontSize < 0 || p.state.hscale <= 0 {
		return fmt.Errorf("invalid rich text default font")
	}
	if p.state.font.Vertical {
		return &UnsupportedError{Feature: "generated vertical rich text"}
	}
	clipWidth, clipHeight := width, height
	matrix, width, height, err := annotationTextLayout(p.textMatrix, x, y, width, height)
	if err != nil {
		return err
	}
	fonts, err := r.newAnnotationRichFonts(ctx, resources, p.state.font, name, paragraphs)
	if err != nil {
		return err
	}
	glyphs, err := annotationRichGlyphs(ctx, fonts, paragraphs, comb > 0)
	if err != nil {
		return err
	}
	multiline := annotation.Subtype == "FreeText" || flags&(1<<12) != 0
	if !multiline && len(paragraphs) > 1 {
		all := glyphs[0]
		for i := 1; i < len(glyphs); i++ {
			if len(glyphs[i]) != 0 && len(all) != 0 {
				style := glyphs[i][0].style
				space := annotationRichGlyph{letter: annotationLetter{text: ' '}, style: style}
				if style.size == nil || *style.size != 0 {
					space.font, space.letter, err = fonts.selectLetter(*style, ' ')
					if err != nil {
						return err
					}
				}
				all = append(all, space)
			}
			all = append(all, glyphs[i]...)
		}
		glyphs, paragraphs = [][]annotationRichGlyph{all}, paragraphs[:1]
	}
	if comb > 0 && len(glyphs[0]) > comb {
		return fmt.Errorf("field value exceeds comb length")
	}
	state := p.state
	if comb > 0 {
		state.spacing, state.wordSpacing = 0, 0
	}
	size := p.state.fontSize
	if size == 0 {
		low, high := 0.0, height
		for range 32 {
			if err := ctx.Err(); err != nil {
				return err
			}
			candidate := (low + high) / 2
			lines, total, err := annotationRichLayout(glyphs, paragraphs, state, candidate, width, multiline)
			if err != nil {
				return err
			}
			fits := total <= height
			for _, line := range lines {
				if comb > 0 {
					for _, glyph := range line.glyphs {
						fits = fits && annotationRichAdvance(glyph, state, candidate, false) <= width/float64(comb)
					}
				} else {
					fits = fits && line.width <= width
				}
			}
			if fits {
				low = candidate
			} else {
				high = candidate
			}
		}
		size = low
		if size == 0 {
			size = min(12, height)
		}
	}
	lines, total, err := annotationRichLayout(glyphs, paragraphs, state, size, width, multiline)
	if err != nil {
		return err
	}
	content.WriteString("q\n")
	writeAnnotationOperation(content, "re W n", x, y, clipWidth, clipHeight)
	decorated := annotationRichDecorated(lines)
	var decorations annotationRichDecorations
	var decorationDefaults string
	if decorated {
		decorationDefaults, err = annotationRichDecorationDefaults(ctx, stateContent)
		if err != nil {
			return err
		}
		content.WriteString("q\n")
	}
	content.WriteString("BT\n")
	top := y + height
	if !multiline {
		top = y + (height+total)/2
	}
	for _, line := range lines {
		left := x + float64(line.style.align)*(width-line.width)/2
		baseline := top - line.ascent
		firstCell := 0
		if comb > 0 {
			if line.style.align == 1 {
				firstCell = (comb - len(line.glyphs)) / 2
			} else if line.style.align == 2 {
				firstCell = comb - len(line.glyphs)
			}
		}
		for start := 0; start < len(line.glyphs); {
			glyph := line.glyphs[start]
			end := start + 1
			for comb == 0 && end < len(line.glyphs) && line.glyphs[end].font == glyph.font && line.glyphs[end].style == glyph.style {
				end++
			}
			fontSize := annotationRichSize(glyph, size)
			if comb > 0 {
				left = x + (float64(firstCell+start)+.5)*width/float64(comb) - annotationRichAdvance(glyph, state, size, false)/2
				if fontSize == 0 {
					start = end
					continue
				}
			}
			if err := writeAnnotationRichRun(ctx, content, line.glyphs[start:end], p.state, stateContent, matrix, size, left, baseline); err != nil {
				return err
			}
			runLeft := left
			for i := start; i < end; i++ {
				left += annotationRichAdvance(line.glyphs[i], state, size, i+1 < len(line.glyphs))
			}
			decorations.add(glyph, p.state, size, runLeft, baseline, left-runLeft)
			start = end
		}
		top -= line.advance
	}
	content.WriteString("ET\n")
	if decorated {
		content.WriteString("Q\n")
		if err := writeAnnotationRichDecorations(ctx, content, decorations.spans, decorationDefaults, matrix); err != nil {
			return err
		}
	}
	content.WriteString("Q\n")
	return nil
}

// annotationRichGlyphs 按富文本样式选择源编码，分格可保留零字号字符的位置
// 入参: ctx 取消上下文, fonts 字体选择器, paragraphs 富文本, keepZero 是否保留零字号字符
// 返回: [][]annotationRichGlyph 段落字符, error 字体、编码或布局错误
func annotationRichGlyphs(ctx context.Context, fonts *annotationRichFonts, paragraphs []annotationRichParagraph, keepZero bool) ([][]annotationRichGlyph, error) {
	glyphs := make([][]annotationRichGlyph, len(paragraphs))
	for i := range paragraphs {
		for j := range paragraphs[i].runs {
			run := &paragraphs[i].runs[j]
			zero := run.style.size != nil && *run.style.size == 0
			if zero && !keepZero {
				continue
			}
			for _, ch := range run.text {
				if err := ctx.Err(); err != nil {
					return nil, err
				}
				if zero {
					glyphs[i] = append(glyphs[i], annotationRichGlyph{letter: annotationLetter{text: ch}, style: &run.style})
					continue
				}
				if unicode.IsMark(ch) || unicode.IsLetter(ch) && !unicode.In(ch, unicode.Latin, unicode.Greek, unicode.Cyrillic, unicode.Han, unicode.Hiragana, unicode.Katakana, unicode.Hangul) {
					return nil, &UnsupportedError{Feature: "generated complex-script rich text"}
				}
				font, letter, err := fonts.selectLetter(run.style, ch)
				if err != nil {
					return nil, err
				}
				glyphs[i] = append(glyphs[i], annotationRichGlyph{letter, font, &run.style})
			}
		}
	}
	return glyphs, nil
}

// writeAnnotationRichRun 写入同一字体及样式的字符，保留原始编码和默认外观状态
// 入参: ctx 取消上下文, content 内容, glyphs 字符, state 默认状态, defaults 默认操作, matrix 定位矩阵, size 默认字号, x 横坐标, y 基线
// 返回: error 取消错误
func writeAnnotationRichRun(ctx context.Context, content *strings.Builder, glyphs []annotationRichGlyph, state graphicsState, defaults string, matrix Matrix, size, x, y float64) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	glyph := glyphs[0]
	content.WriteString("0 g\n" + defaults)
	fmt.Fprintf(content, "/%s ", escapeAnnotationName(glyph.font.name))
	writeAnnotationOperation(content, "Tf", annotationRichSize(glyph, size))
	writeAnnotationOperation(content, "Ts", state.rise+glyph.style.rise)
	if glyph.style.color != nil {
		color := glyph.style.color
		writeAnnotationOperation(content, "rg", color[0], color[1], color[2])
	}
	writeAnnotationTextMatrix(content, matrix, x, y)
	content.WriteByte('<')
	for _, current := range glyphs {
		if err := ctx.Err(); err != nil {
			return err
		}
		fmt.Fprintf(content, "%x", current.letter.code)
	}
	content.WriteString("> Tj\n")
	return nil
}

// annotationRichSize 读取字符的显式字号或默认外观字号
// 入参: glyph 字符, size 默认字号
// 返回: float64 字号
func annotationRichSize(glyph annotationRichGlyph, size float64) float64 {
	if glyph.style.size != nil {
		return *glyph.style.size
	}
	return size
}

// annotationRichAdvance 计算混合字号字符推进量，行末不计额外间距
// 入参: glyph 字符, state 文字状态, size 默认字号, following 后续是否有字符
// 返回: float64 推进量
func annotationRichAdvance(glyph annotationRichGlyph, state graphicsState, size float64, following bool) float64 {
	advance := glyph.letter.width / 1000 * annotationRichSize(glyph, size)
	if following {
		advance += state.spacing
		if glyph.letter.word {
			advance += state.wordSpacing
		}
	}
	return advance * state.hscale
}

// annotationRichLayout 按段落、词边界及混合字号计算排版行
// 入参: paragraphs 段落字符, styles 段落样式, state 文字状态, size 默认字号, width 行宽, multiline 是否折行
// 返回: []annotationRichLine 排版行, float64 总高度, error 度量溢出
func annotationRichLayout(paragraphs [][]annotationRichGlyph, styles []annotationRichParagraph, state graphicsState, size, width float64, multiline bool) ([]annotationRichLine, float64, error) {
	var lines []annotationRichLine
	for p, glyphs := range paragraphs {
		start, lastBreak, advance := 0, -1, 0.0
		for i := 0; i < len(glyphs); i++ {
			next := annotationRichAdvance(glyphs[i], state, size, false)
			if i > start {
				next += annotationRichAdvance(glyphs[i-1], state, size, true) - annotationRichAdvance(glyphs[i-1], state, size, false)
			}
			if multiline && advance+next > width && i > start {
				end := i
				if lastBreak >= start {
					end = lastBreak + 1
				}
				lineEnd := end
				for lineEnd > start && glyphs[lineEnd-1].letter.text == ' ' {
					lineEnd--
				}
				lines = append(lines, annotationRichLine{glyphs: glyphs[start:lineEnd], style: &styles[p].style})
				for end < len(glyphs) && glyphs[end].letter.text == ' ' {
					end++
				}
				start, lastBreak, advance, i = end, -1, 0, end-1
				continue
			}
			advance += next
			if glyphs[i].letter.text == ' ' {
				lastBreak = i
			}
		}
		lines = append(lines, annotationRichLine{glyphs: glyphs[start:], style: &styles[p].style})
	}
	total := 0.0
	for i := range lines {
		line := &lines[i]
		largest := size
		if line.style.size != nil {
			largest = *line.style.size
		}
		if len(line.glyphs) != 0 {
			largest = 0
		}
		for j, glyph := range line.glyphs {
			n := annotationRichSize(glyph, size)
			largest = max(largest, n)
			line.width += annotationRichAdvance(glyph, state, size, j+1 < len(line.glyphs))
			if n == 0 {
				continue
			}
			ascent, descent := .8*n, -.2*n
			if box := glyph.font.font.BoundingBox; box != nil && box.YMin < box.YMax {
				ascent, descent = box.YMax*n/1000, box.YMin*n/1000
			}
			line.ascent = max(line.ascent, ascent+state.rise+glyph.style.rise)
			line.descent = min(line.descent, descent+state.rise+glyph.style.rise)
		}
		if len(line.glyphs) == 0 {
			line.ascent, line.descent = .8*largest, -.2*largest
		}
		line.advance = max(line.ascent-line.descent, largest*1.2)
		if state.leading > 0 {
			line.advance = max(line.ascent-line.descent, state.leading)
		}
		total += line.advance
		if i+1 == len(lines) {
			total += line.ascent - line.descent - line.advance
		}
		for _, n := range []float64{line.width, line.ascent, line.descent, line.advance, total} {
			if math.IsNaN(n) || math.IsInf(n, 0) {
				return nil, 0, fmt.Errorf("rich text dimensions overflow")
			}
		}
	}
	return lines, total, nil
}
