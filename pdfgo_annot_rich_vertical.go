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
	"slices"
	"strings"
)

// annotationRichVertical 保存富文本竖排的字体、段落和外观坐标
type annotationRichVertical struct {
	glyphs     [][]annotationRichGlyph
	paragraphs []annotationRichParagraph
	state      graphicsState
	defaults   string
	matrix     Matrix
	frame      Rectangle
	clip       Rectangle
	multiline  bool
	comb       int
	align      int
}

// annotationRichColumn 保存混合字体列及其占用范围
type annotationRichColumn struct {
	glyphs  []annotationRichGlyph
	style   *annotationRichStyle
	bounds  Rectangle
	advance float64
}

// annotationRichVerticalDecoration 保存竖排装饰的来源和范围
type annotationRichVerticalDecoration struct {
	origin             *annotationRichDecoration
	x, y, height, size float64
}

// write 生成混合字体竖排外观，文字裁剪与装饰分别绘制
// 入参: ctx 取消上下文, content 外观内容
// 返回: error 布局、度量或取消错误
func (v annotationRichVertical) write(ctx context.Context, content *strings.Builder) error {
	width, height := v.frame.XMax-v.frame.XMin, v.frame.YMax-v.frame.YMin
	size := v.state.fontSize
	if size == 0 {
		low, high := 0.0, height
		for range 32 {
			candidate := (low + high) / 2
			columns, total, err := v.columns(ctx, candidate)
			if err != nil {
				return err
			}
			fits := total <= width
			for _, column := range columns {
				if v.comb == 0 {
					fits = fits && column.bounds.YMax-column.bounds.YMin <= height
				} else {
					for _, glyph := range column.glyphs {
						if err := ctx.Err(); err != nil {
							return err
						}
						box, _, _, err := annotationRichVerticalBounds(glyph, v.state, candidate, 0)
						if err != nil {
							return err
						}
						fits = fits && box.YMax-box.YMin <= height/float64(v.comb)
					}
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
	columns, total, err := v.columns(ctx, size)
	if err != nil {
		return err
	}
	decorated := false
	for _, column := range columns {
		for _, glyph := range column.glyphs {
			if err := ctx.Err(); err != nil {
				return err
			}
			decorated = decorated || glyph.style.decorations != nil
		}
	}
	var decorationDefaults string
	if decorated {
		decorationDefaults, err = annotationRichDecorationDefaults(ctx, v.defaults)
		if err != nil {
			return err
		}
	}
	content.WriteString("q\n")
	writeAnnotationOperation(content, "re W n", v.clip.XMin, v.clip.YMin, v.clip.XMax-v.clip.XMin, v.clip.YMax-v.clip.YMin)
	if decorated {
		content.WriteString("q\n")
	}
	content.WriteString("BT\n")
	right := v.frame.XMin + float64(v.align)*(width-total)/2 + total
	var decorations []annotationRichVerticalDecoration
	var lastDecoration map[*annotationRichDecoration]int
	if decorated {
		lastDecoration = make(map[*annotationRichDecoration]int)
	}
	for _, column := range columns {
		ink := column.bounds.XMax - column.bounds.XMin
		x := right - column.advance + float64(column.style.align)*(column.advance-ink)/2 - column.bounds.XMin
		y := v.frame.YMax - column.bounds.YMax
		if !v.multiline {
			y = v.frame.YMin + (height-column.bounds.YMax-column.bounds.YMin)/2
		}
		pen := 0.0
		for i := 0; i < len(column.glyphs); {
			glyph := column.glyphs[i]
			if err := ctx.Err(); err != nil {
				return err
			}
			box, advance, offset, err := annotationRichVerticalBounds(glyph, v.state, size, pen)
			if err != nil {
				return err
			}
			fontSize := annotationRichSize(glyph, size)
			if fontSize == 0 {
				i++
				continue
			}
			end := i + 1
			for v.comb == 0 && glyph.font.font.Vertical && end < len(column.glyphs) && column.glyphs[end].font == glyph.font && column.glyphs[end].style == glyph.style {
				if err := ctx.Err(); err != nil {
					return err
				}
				_, next, _, err := annotationRichVerticalBounds(column.glyphs[end], v.state, size, pen+advance)
				if err != nil {
					return err
				}
				advance += next
				end++
			}
			baseline := y + pen
			if v.comb > 0 {
				baseline = v.frame.YMax - (float64(i)+.5)*height/float64(v.comb) - (box.YMin+box.YMax)/2 + pen
			}
			point := v.matrix.Apply(Point{x + offset.X, baseline + offset.Y})
			if math.IsNaN(point.X) || math.IsInf(point.X, 0) || math.IsNaN(point.Y) || math.IsInf(point.Y, 0) {
				return fmt.Errorf("vertical rich text position overflow")
			}
			if err := writeAnnotationRichRun(ctx, content, column.glyphs[i:end], v.state, v.defaults, v.matrix, size, x+offset.X, baseline+offset.Y); err != nil {
				return err
			}
			if decorated {
				start := len(decorations)
				for origin := glyph.style.decorations; origin != nil; origin = origin.parent {
					if err := ctx.Err(); err != nil {
						return err
					}
					n := size
					if origin.size != nil {
						n = *origin.size
					}
					if n == 0 {
						continue
					}
					top := baseline + v.state.rise + origin.rise
					length := advance
					if v.comb > 0 {
						length = -height / float64(v.comb)
					}
					if index, ok := lastDecoration[origin]; ok {
						previous := &decorations[index]
						if previous.x == x && previous.y+previous.height == top && previous.size == n && previous.height*length > 0 {
							previous.height += length
							continue
						}
					}
					decorations = append(decorations, annotationRichVerticalDecoration{origin, x, top, length, n})
				}
				slices.Reverse(decorations[start:])
				for j := start; j < len(decorations); j++ {
					lastDecoration[decorations[j].origin] = j
				}
			}
			pen += advance
			i = end
		}
		right -= column.advance
	}
	content.WriteString("ET\n")
	if decorated {
		content.WriteString("Q\n")
		if err := v.decorate(ctx, content, decorations, decorationDefaults); err != nil {
			return err
		}
	}
	content.WriteString("Q\n")
	return ctx.Err()
}

// columns 按段落及可用高度划分混合字体列，列间距覆盖实际字形范围
// 入参: ctx 取消上下文, size 默认字号
// 返回: []annotationRichColumn 文字列, float64 总宽度, error 度量或取消错误
func (v annotationRichVertical) columns(ctx context.Context, size float64) ([]annotationRichColumn, float64, error) {
	var columns []annotationRichColumn
	for p, glyphs := range v.glyphs {
		start, pen, largest := 0, 0.0, 0.0
		var bounds Rectangle
		have := false
		appendColumn := func(end int) {
			if !have {
				largest = size
				if value := v.paragraphs[p].style.size; value != nil {
					largest = *value
				}
				bounds = Rectangle{-largest * v.state.hscale / 2, 0, largest * v.state.hscale / 2, 0}
			}
			advance := max(bounds.XMax-bounds.XMin, largest*v.state.hscale*1.2)
			if v.state.leading > 0 {
				advance = max(bounds.XMax-bounds.XMin, v.state.leading)
			}
			columns = append(columns, annotationRichColumn{glyphs[start:end], &v.paragraphs[p].style, bounds, advance})
			start, pen, largest, have = end, 0, 0, false
		}
		for i, glyph := range glyphs {
			if err := ctx.Err(); err != nil {
				return nil, 0, err
			}
			n := annotationRichSize(glyph, size)
			if n == 0 {
				continue
			}
			box, advance, _, err := annotationRichVerticalBounds(glyph, v.state, size, pen)
			if err != nil {
				return nil, 0, err
			}
			combined := box
			if have {
				combined = Rectangle{min(bounds.XMin, box.XMin), min(bounds.YMin, box.YMin), max(bounds.XMax, box.XMax), max(bounds.YMax, box.YMax)}
			}
			if v.multiline && v.comb == 0 && have && combined.YMax-combined.YMin > v.frame.YMax-v.frame.YMin {
				appendColumn(i)
				box, advance, _, err = annotationRichVerticalBounds(glyph, v.state, size, 0)
				if err != nil {
					return nil, 0, err
				}
				combined = box
			}
			bounds, pen, largest, have = combined, pen+advance, max(largest, n), true
		}
		appendColumn(len(glyphs))
	}
	total := 0.0
	for _, column := range columns {
		if err := ctx.Err(); err != nil {
			return nil, 0, err
		}
		total += column.advance
		if math.IsInf(total, 0) || math.IsNaN(total) {
			return nil, 0, fmt.Errorf("vertical rich text dimensions overflow")
		}
	}
	return columns, total, ctx.Err()
}

// annotationRichVerticalBounds 计算原字体的竖排边界，横排字体按正立字框置入单元
// 入参: glyph 字符, state 默认文字状态, size 默认字号, pen 列内位置
// 返回: Rectangle 字形边界, float64 推进量, Point 横排字体定位补偿, error 度量错误
func annotationRichVerticalBounds(glyph annotationRichGlyph, state graphicsState, size, pen float64) (Rectangle, float64, Point, error) {
	n := annotationRichSize(glyph, size)
	if n == 0 {
		return Rectangle{}, 0, Point{}, nil
	}
	state.font, state.rise = glyph.font.font, state.rise+glyph.style.rise
	letter := glyph.letter
	var offset Point
	if !state.font.Vertical {
		box := Rectangle{0, -200, letter.width, 800}
		if b := state.font.BoundingBox; b != nil && b.XMin < b.XMax && b.YMin < b.YMax {
			box = *b
		}
		metrics := VerticalMetrics{Origin: Point{(box.XMin + box.XMax) / 2, box.YMax}, Advance: -max(1000, box.YMax-box.YMin)}
		letter.vertical = &metrics
		offset = Point{-metrics.Origin.X * n * state.hscale / 1000, -metrics.Origin.Y * n / 1000}
	}
	box, advance, err := annotationVerticalBounds(letter, state, n, pen)
	return box, advance, offset, err
}

// decorate 绘制竖排右侧下划线和居中删除线，装饰颜色及字号取自声明元素
// 入参: ctx 取消上下文, content 外观内容, spans 装饰片段, defaults 默认填充状态
// 返回: error 范围或取消错误
func (v annotationRichVertical) decorate(ctx context.Context, content *strings.Builder, spans []annotationRichVerticalDecoration, defaults string) error {
	for _, span := range spans {
		if err := ctx.Err(); err != nil {
			return err
		}
		if span.height == 0 {
			continue
		}
		content.WriteString("q\n" + defaults)
		writeAnnotationOperation(content, "cm", v.matrix[:]...)
		if c := span.origin.color; c != nil {
			writeAnnotationOperation(content, "rg", c[0], c[1], c[2])
		}
		thickness := span.size * v.state.hscale * .05
		for _, line := range [...]struct {
			kind   uint8
			offset float64
		}{{annotationRichUnderline, .6}, {annotationRichStrike, 0}} {
			if span.origin.kind&line.kind == 0 {
				continue
			}
			x := span.x + line.offset*span.size*v.state.hscale - thickness/2
			for _, n := range [...]float64{x, span.y, thickness, span.height, x + thickness, span.y + span.height} {
				if math.IsInf(n, 0) || math.IsNaN(n) {
					return fmt.Errorf("vertical rich text decoration dimensions overflow")
				}
			}
			writeAnnotationOperation(content, "re f", x, span.y, thickness, span.height)
		}
		content.WriteString("Q\n")
	}
	return ctx.Err()
}
