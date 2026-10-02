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
	base, _, object, err := r.annotationCaptionFont(ctx, page.Resources, plain.String())
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
	fonts[name] = object
	resources := Dictionary{"Font": fonts}
	selector, err := r.newAnnotationRichFonts(ctx, resources, base, name, paragraphs)
	if err != nil {
		return nil, err
	}
	glyphs, err := annotationRichGlyphs(ctx, selector, paragraphs, false)
	if err != nil {
		return nil, err
	}
	caption.rich, caption.height, err = annotationRichLayout(glyphs, paragraphs, graphicsState{hscale: 1}, annotationCaptionSize, math.Inf(1), true)
	if err != nil {
		return nil, err
	}
	for _, line := range caption.rich {
		caption.width = max(caption.width, line.width)
	}
	caption.resources = resources
	return caption, nil
}

// writeRichLineCaption 在标题局部坐标写入段落，保留样式和行内基线偏移
// 入参: ctx 取消上下文, content 内容, caption 富文本标题
// 返回: error 取消错误
func writeRichLineCaption(ctx context.Context, content *strings.Builder, caption *annotationLineCaption) error {
	decorated := annotationRichDecorated(caption.rich)
	var decorations annotationRichDecorations
	if decorated {
		content.WriteString("q\n")
	}
	content.WriteString("BT\n")
	state := graphicsState{hscale: 1}
	top := caption.height / 2
	for _, line := range caption.rich {
		if err := ctx.Err(); err != nil {
			return err
		}
		left := -caption.width/2 + float64(line.style.align)*(caption.width-line.width)/2
		baseline := top - line.ascent
		for start := 0; start < len(line.glyphs); {
			glyph := line.glyphs[start]
			end := start + 1
			for end < len(line.glyphs) && line.glyphs[end].font == glyph.font && line.glyphs[end].style == glyph.style {
				end++
			}
			if err := writeAnnotationRichRun(ctx, content, line.glyphs[start:end], state, "", annotationCaptionSize, left, baseline); err != nil {
				return err
			}
			runLeft := left
			for _, current := range line.glyphs[start:end] {
				left += annotationRichAdvance(current, state, annotationCaptionSize, false)
			}
			decorations.add(glyph, state, annotationCaptionSize, runLeft, baseline, left-runLeft)
			start = end
		}
		top -= line.advance
	}
	content.WriteString("ET\n")
	if decorated {
		content.WriteString("Q\n")
		return writeAnnotationRichDecorations(ctx, content, decorations.spans, "0 g\n")
	}
	return nil
}
