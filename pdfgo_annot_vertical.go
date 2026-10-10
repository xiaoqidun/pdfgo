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

// annotationVerticalText 保存竖排外观的文字、定位和字段布局属性
type annotationVerticalText struct {
	letters   []annotationLetter
	matrix    Matrix
	font      Name
	defaults  string
	frame     Rectangle
	clip      Rectangle
	align     Integer
	comb      int
	multiline bool
	choice    *ChoiceField
	free      bool
}

// annotationVerticalColumn 保存同列原始字符及包含纵向原点偏移的字形边界
type annotationVerticalColumn struct {
	letters []annotationLetter
	bounds  Rectangle
	advance float64
}

// write 按字体纵向度量生成外观，自右向左分列，按Q调整文字块的水平对齐
// 入参: p 默认文字状态, content 外观内容
// 返回: error 排版、度量或取消错误
func (v annotationVerticalText) write(p *pageInterpreter, content *strings.Builder) error {
	ctx, state := p.ctx, p.state
	width, height := v.frame.XMax-v.frame.XMin, v.frame.YMax-v.frame.YMin
	list := v.choice != nil && !v.choice.Combo
	size := state.fontSize
	if list && size == 0 {
		size = min(12, width/state.hscale)
	}
	if size == 0 {
		low, high := 0.0, max(width/state.hscale, height)
		for range 32 {
			candidate := (low + high) / 2
			columns, total, err := v.columns(ctx, state, candidate)
			if err != nil {
				return err
			}
			fits := total <= width
			for _, column := range columns {
				if v.comb > 0 {
					for _, letter := range column.letters {
						if err := ctx.Err(); err != nil {
							return err
						}
						box, _, err := annotationVerticalBounds(letter, state, candidate, 0)
						if err != nil {
							return err
						}
						fits = fits && box.YMax-box.YMin <= height/float64(v.comb)
					}
				} else {
					fits = fits && column.bounds.YMax-column.bounds.YMin <= height
				}
			}
			if fits {
				low = candidate
			} else {
				high = candidate
			}
		}
		size = low
	}
	if size <= 0 || math.IsInf(size, 0) || math.IsNaN(size) {
		return fmt.Errorf("invalid vertical font size")
	}
	columns, total, err := v.columns(ctx, state, size)
	if err != nil {
		return err
	}
	right := v.frame.XMin + float64(v.align)*(width-total)/2 + total
	if list {
		right = v.frame.XMax
	}
	content.WriteString("q\n")
	writeAnnotationOperation(content, "re W n", v.clip.XMin, v.clip.YMin, v.clip.XMax-v.clip.XMin, v.clip.YMax-v.clip.YMin)
	if list {
		edge := right
		for i, column := range columns {
			if err := ctx.Err(); err != nil {
				return err
			}
			if edge <= v.frame.XMin {
				break
			}
			if _, selected := slices.BinarySearch(v.choice.Selected, v.choice.TopIndex+i); selected {
				content.WriteString("q 0.153 0.392 0.714 rg\n")
				writeAnnotationOperation(content, "cm", v.matrix[:]...)
				writeAnnotationOperation(content, "re f Q", edge-column.advance, v.frame.YMin, column.advance, height)
			}
			edge -= column.advance
		}
	}
	if !v.free {
		content.WriteString("/Tx BMC\n")
	}
	content.WriteString("BT\n")
	for i, column := range columns {
		if err := ctx.Err(); err != nil {
			return err
		}
		if list && right <= v.frame.XMin {
			break
		}
		selected := false
		if list {
			_, selected = slices.BinarySearch(v.choice.Selected, v.choice.TopIndex+i)
		}
		content.WriteString("0 g\n" + v.defaults)
		fmt.Fprintf(content, "/%s ", escapeAnnotationName(v.font))
		writeAnnotationOperation(content, "Tf", size)
		if selected {
			content.WriteString("1 g\n")
		}
		x, y := right-column.bounds.XMax, v.frame.YMax-column.bounds.YMax
		if !v.multiline {
			y = v.frame.YMin + (height-column.bounds.YMax-column.bounds.YMin)/2
		}
		if v.comb > 0 {
			for j, letter := range column.letters {
				if err := ctx.Err(); err != nil {
					return err
				}
				box, _, err := annotationVerticalBounds(letter, state, size, 0)
				if err != nil {
					return err
				}
				y = v.frame.YMax - (float64(j)+.5)*height/float64(v.comb) - (box.YMin+box.YMax)/2
				if err := v.position(content, x, y); err != nil {
					return err
				}
				fmt.Fprintf(content, "<%x> Tj\n", letter.code)
			}
		} else {
			if err := v.position(content, x, y); err != nil {
				return err
			}
			content.WriteByte('<')
			for _, letter := range column.letters {
				if err := ctx.Err(); err != nil {
					return err
				}
				fmt.Fprintf(content, "%x", letter.code)
			}
			content.WriteString("> Tj\n")
		}
		right -= column.advance
	}
	content.WriteString("ET\n")
	if !v.free {
		content.WriteString("EMC\n")
	}
	content.WriteString("Q\n")
	return ctx.Err()
}

// position 写入保留线性变换的竖排定位矩阵，拒绝坐标溢出
// 入参: content 外观内容, x 横坐标, y 纵向原点
// 返回: error 坐标错误
func (v annotationVerticalText) position(content *strings.Builder, x, y float64) error {
	point := v.matrix.Apply(Point{X: x, Y: y})
	if math.IsNaN(point.X) || math.IsInf(point.X, 0) || math.IsNaN(point.Y) || math.IsInf(point.Y, 0) {
		return fmt.Errorf("vertical text position overflow")
	}
	writeAnnotationOperation(content, "Tm", v.matrix[0], v.matrix[1], v.matrix[2], v.matrix[3], point.X, point.Y)
	return nil
}

// columns 按显式换行和可用高度划分竖排文字，选择列表的每项独占一列
// 入参: ctx 取消上下文, state 文字状态, size 字号
// 返回: []annotationVerticalColumn 文字列, float64 文字块宽度, error 度量或取消错误
func (v annotationVerticalText) columns(ctx context.Context, state graphicsState, size float64) ([]annotationVerticalColumn, float64, error) {
	var columns []annotationVerticalColumn
	start, pen := 0, 0.0
	var bounds Rectangle
	appendColumn := func(end int) {
		if end == start {
			bounds = Rectangle{-size * state.hscale / 2, 0, size * state.hscale / 2, 0}
		}
		columns = append(columns, annotationVerticalColumn{letters: v.letters[start:end], bounds: bounds})
		start, pen = end, 0
	}
	wrap := v.multiline && v.comb == 0 && (v.choice == nil || v.choice.Combo)
	for i, letter := range v.letters {
		if err := ctx.Err(); err != nil {
			return nil, 0, err
		}
		if letter.text == '\n' {
			appendColumn(i)
			start = i + 1
			continue
		}
		box, advance, err := annotationVerticalBounds(letter, state, size, pen)
		if err != nil {
			return nil, 0, err
		}
		combined := box
		if i > start {
			combined = Rectangle{min(bounds.XMin, box.XMin), min(bounds.YMin, box.YMin), max(bounds.XMax, box.XMax), max(bounds.YMax, box.YMax)}
		}
		if wrap && i > start && combined.YMax-combined.YMin > v.frame.YMax-v.frame.YMin {
			appendColumn(i)
			box, advance, err = annotationVerticalBounds(letter, state, size, 0)
			if err != nil {
				return nil, 0, err
			}
			combined = box
		}
		bounds, pen = combined, pen+advance
	}
	appendColumn(len(v.letters))
	total := 0.0
	for i := range columns {
		if err := ctx.Err(); err != nil {
			return nil, 0, err
		}
		column := &columns[i]
		leading := state.leading
		if leading <= 0 {
			leading = size * state.hscale * 1.2
		}
		column.advance = max(column.bounds.XMax-column.bounds.XMin, leading)
		if i+1 < len(columns) {
			total += column.advance
		} else {
			total += column.bounds.XMax - column.bounds.XMin
		}
	}
	if math.IsNaN(total) || math.IsInf(total, 0) {
		return nil, 0, fmt.Errorf("vertical text dimensions overflow")
	}
	return columns, total, ctx.Err()
}

// annotationVerticalBounds 按PDF纵向原点、字距及水平缩放计算字形边界与推进量
// 入参: letter 字符, state 文字状态, size 字号, pen 当前纵向位置
// 返回: Rectangle 字形边界, float64 纵向推进量, error 度量错误
func annotationVerticalBounds(letter annotationLetter, state graphicsState, size, pen float64) (Rectangle, float64, error) {
	if letter.vertical == nil {
		return Rectangle{}, 0, fmt.Errorf("missing vertical glyph metrics")
	}
	box := Rectangle{0, -200, letter.width, 800}
	if bounds := state.font.BoundingBox; bounds != nil && bounds.XMin < bounds.XMax && bounds.YMin < bounds.YMax {
		box = *bounds
	}
	metric := letter.vertical
	box = Rectangle{
		(box.XMin - metric.Origin.X) * size * state.hscale / 1000,
		(box.YMin-metric.Origin.Y)*size/1000 + state.rise + pen,
		(box.XMax - metric.Origin.X) * size * state.hscale / 1000,
		(box.YMax-metric.Origin.Y)*size/1000 + state.rise + pen,
	}
	advance := metric.Advance*size/1000 + state.spacing
	if letter.word {
		advance += state.wordSpacing
	}
	for _, n := range []float64{box.XMin, box.YMin, box.XMax, box.YMax, advance} {
		if math.IsNaN(n) || math.IsInf(n, 0) {
			return Rectangle{}, 0, fmt.Errorf("vertical text dimensions overflow")
		}
	}
	return box, advance, nil
}
