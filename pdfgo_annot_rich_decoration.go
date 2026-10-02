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

// 富文本装饰类型
const (
	annotationRichUnderline uint8 = 1 << iota
	annotationRichStrike
)

// annotationRichDecoration 保留声明元素的装饰样式，后代颜色和基线不改变祖先装饰
type annotationRichDecoration struct {
	parent *annotationRichDecoration
	kind   uint8
	size   *float64
	color  *[3]float64
	rise   float64
}

// annotationRichDecorationSpan 保存一段装饰的局部坐标及来源
type annotationRichDecorationSpan struct {
	origin            *annotationRichDecoration
	x, y, width, size float64
}

// annotationRichDecorations 合并同一来源的连续装饰，避免样式切换处出现接缝
type annotationRichDecorations struct {
	spans []annotationRichDecorationSpan
	last  map[*annotationRichDecoration]int
}

// annotationRichDecorated 判断排版文字是否需要装饰，普通文字不建立额外绘制状态
// 入参: lines 排版行
// 返回: bool 是否存在装饰
func annotationRichDecorated(lines []annotationRichLine) bool {
	for _, line := range lines {
		for _, glyph := range line.glyphs {
			if glyph.style.decorations != nil {
				return true
			}
		}
	}
	return false
}

// add 收集一段文字的祖先装饰，横向范围包括适用的字词间距
// 入参: glyph 字符, state 文字状态, size 默认字号, x 起点, y 基线, width 推进量
func (d *annotationRichDecorations) add(glyph annotationRichGlyph, state graphicsState, size, x, y, width float64) {
	if annotationRichSize(glyph, size) == 0 || width == 0 {
		return
	}
	start := len(d.spans)
	for origin := glyph.style.decorations; origin != nil; origin = origin.parent {
		fontSize := size
		if origin.size != nil {
			fontSize = *origin.size
		}
		if fontSize != 0 {
			baseline := y + state.rise + origin.rise
			if index, ok := d.last[origin]; ok {
				previous := &d.spans[index]
				if previous.y == baseline && previous.size == fontSize && previous.x+previous.width == x && previous.width*width > 0 {
					previous.width += width
					continue
				}
			}
			d.spans = append(d.spans, annotationRichDecorationSpan{origin, x, baseline, width, fontSize})
		}
	}
	slices.Reverse(d.spans[start:])
	if len(d.spans) != start && d.last == nil {
		d.last = make(map[*annotationRichDecoration]int)
	}
	for i := start; i < len(d.spans); i++ {
		d.last[d.spans[i].origin] = i
	}
}

// annotationRichDecorationDefaults 保留默认外观的填充颜色及意图，不在文字对象外重放文字操作
// 入参: ctx 取消上下文, defaults 默认外观操作
// 返回: string 默认绘制操作, error 操作或取消错误
func annotationRichDecorationDefaults(ctx context.Context, defaults string) (string, error) {
	var content strings.Builder
	content.WriteString("0 g\n")
	err := WalkOperations(ctx, []byte(defaults), func(op Operation) error {
		switch op.Operator {
		case "g", "rg", "k", "cs", "sc", "scn", "ri":
			for _, operand := range op.Operands {
				if err := writeAnnotationOperand(&content, operand); err != nil {
					return err
				}
				content.WriteByte(' ')
			}
			content.WriteString(op.Operator + "\n")
		}
		return nil
	})
	return content.String(), err
}

// writeAnnotationRichDecorations 写入字号比例的装饰矩形，不继承文字裁剪或描边参数
// 入参: ctx 取消上下文, content 外观内容, spans 装饰片段, defaults 默认填充操作
// 返回: error 取消或范围溢出
func writeAnnotationRichDecorations(ctx context.Context, content *strings.Builder, spans []annotationRichDecorationSpan, defaults string) error {
	for _, span := range spans {
		if err := ctx.Err(); err != nil {
			return err
		}
		content.WriteString(defaults)
		if color := span.origin.color; color != nil {
			writeAnnotationOperation(content, "rg", color[0], color[1], color[2])
		}
		thickness := .05 * span.size
		for _, line := range [...]struct {
			kind   uint8
			offset float64
		}{{annotationRichUnderline, -.1}, {annotationRichStrike, .3}} {
			if span.origin.kind&line.kind == 0 {
				continue
			}
			y := span.y + line.offset*span.size - thickness/2
			for _, n := range [...]float64{span.x, y, span.width, thickness, span.x + span.width, y + thickness} {
				if math.IsNaN(n) || math.IsInf(n, 0) {
					return fmt.Errorf("rich text decoration dimensions overflow")
				}
			}
			writeAnnotationOperation(content, "re f", span.x, y, span.width, thickness)
		}
	}
	return nil
}
