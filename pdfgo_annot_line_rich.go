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
	"maps"
	"math"
	"strings"
)

// writeRichLineCaption 在标题局部坐标写入段落，保留样式、行内基线偏移与字节上限检查
// 入参: ctx 取消上下文, content 内容, caption 富文本标题, contentLimit 内容流字节上限
// 返回: error 取消、超限或范围溢出错误
func writeRichLineCaption(ctx context.Context, content *strings.Builder, caption *annotationLineCaption, contentLimit int) error {
	decorated := annotationRichDecorated(caption.rich)
	var decorations annotationRichDecorations
	if decorated {
		content.WriteString("q\n")
	}
	content.WriteString("BT\n")
	if contentLimit > 0 && content.Len() > contentLimit {
		return fmt.Errorf("%w: rich text appearance content limit exceeded", ErrStreamLimitExceeded)
	}
	state := graphicsState{hscale: 1}
	top := caption.height / 2
	for _, line := range caption.rich {
		if err := ctx.Err(); err != nil {
			return err
		}
		left := -caption.width/2 + float64(line.style.align)*(caption.width-line.width)/2
		baseline := top - line.ascent
		for start := 0; start < len(line.glyphs); {
			if err := ctx.Err(); err != nil {
				return err
			}
			glyph := line.glyphs[start]
			end := start + 1
			for end < len(line.glyphs) && line.glyphs[end].font == glyph.font && line.glyphs[end].style == glyph.style {
				if end&255 == 0 {
					if err := ctx.Err(); err != nil {
						return err
					}
				}
				end++
			}
			if err := writeAnnotationRichHorizontalRun(ctx, content, line.glyphs[start:end], state, "", Identity(), annotationCaptionSize, left, baseline, contentLimit); err != nil {
				return err
			}
			runLeft := left
			for i, current := range line.glyphs[start:end] {
				if i&255 == 0 {
					if err := ctx.Err(); err != nil {
						return err
					}
				}
				left += annotationRichAdvance(current, state, annotationCaptionSize, false)
			}
			if err := decorations.add(ctx, glyph, state, annotationCaptionSize, runLeft, baseline, left-runLeft); err != nil {
				return err
			}
			start = end
		}
		top -= line.advance
	}
	content.WriteString("ET\n")
	if decorated {
		content.WriteString("Q\n")
		if contentLimit > 0 && content.Len() > contentLimit {
			return fmt.Errorf("%w: rich text appearance content limit exceeded", ErrStreamLimitExceeded)
		}
		return writeAnnotationRichDecorations(ctx, content, decorations.spans, "0 g\n", Identity(), contentLimit)
	}
	if contentLimit > 0 && content.Len() > contentLimit {
		return fmt.Errorf("%w: rich text appearance content limit exceeded", ErrStreamLimitExceeded)
	}
	return ctx.Err()
}

// readRichLineCaption 读取直线富文本标题，共用字符编码及混合字体排版
// 入参: ctx 取消上下文, page 所在页面, object 富文本, caption 标题位置
// 返回: *annotationLineCaption 标题或空值, error 富文本、资源或字体错误
func (r *Reader) readRichLineCaption(ctx context.Context, page *Page, object Object, caption *annotationLineCaption) (*annotationLineCaption, error) {
	paragraphs, err := r.readAnnotationRich(ctx, object, nil, 1)
	if err != nil {
		return nil, err
	}
	var plain strings.Builder
	visible, requested := false, false
	for _, paragraph := range paragraphs {
		for _, run := range paragraph.runs {
			if run.style.size != nil && *run.style.size == 0 || run.text == "" {
				continue
			}
			if annotationNeedsShaping(run.text) {
				return nil, &UnsupportedError{Feature: "generated complex-script rich text"}
			}
			visible = true
			requested = requested || len(run.style.families) != 0 || run.style.weight != 0 || run.style.italic != nil || run.style.stretch != 0
			if len(run.style.families) == 0 {
				plain.WriteString(run.text)
			}
		}
	}
	if !visible {
		return nil, nil
	}
	encoded, err := r.annotationCaptionFont(ctx, page.Resources, plain.String())
	if err != nil {
		return nil, err
	}
	fonts := Dictionary{}
	if requested {
		value, err := r.Resolve(page.Resources["Font"])
		if err != nil {
			return nil, err
		}
		if value != nil {
			pageFonts, ok := value.(Dictionary)
			if !ok {
				return nil, fmt.Errorf("invalid page font resources")
			}
			fonts = maps.Clone(pageFonts)
		}
	}
	name := Name("CaptionFont")
	for fonts[name] != nil {
		name += "_"
	}
	fonts[name] = encoded.object
	resources := Dictionary{"Font": fonts}
	selector, err := r.newAnnotationRichFonts(ctx, resources, encoded.font, name, paragraphs)
	if err != nil {
		return nil, err
	}
	glyphs, err := annotationRichGlyphs(ctx, selector, paragraphs, false)
	if err != nil {
		return nil, err
	}
	caption.rich, caption.height, err = annotationRichLayout(ctx, glyphs, paragraphs, graphicsState{hscale: 1}, annotationCaptionSize, math.Inf(1), true)
	if err != nil {
		return nil, err
	}
	for _, line := range caption.rich {
		caption.width = max(caption.width, line.width)
	}
	if math.IsNaN(caption.width) || math.IsInf(caption.width, 0) || math.IsNaN(caption.height) || math.IsInf(caption.height, 0) {
		return nil, fmt.Errorf("annotation caption dimensions overflow")
	}
	caption.resources = resources
	return caption, nil
}
