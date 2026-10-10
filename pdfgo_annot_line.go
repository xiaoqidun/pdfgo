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
)

// 直线标题的缺省字号及行距
const (
	annotationCaptionSize    float64 = 12
	annotationCaptionLeading         = annotationCaptionSize * 1.2
)

// annotationLineCaption 保存标题编码、度量和相对主线的布局
type annotationLineCaption struct {
	font          Object
	lines         [][]annotationLetter
	widths        []float64
	rich          []annotationRichLine
	resources     Dictionary
	width, height float64
	descent       float64
	offset        [2]float64
	top           bool
}

// lineAppearance 按引线端点、偏移和标题生成直线注解外观
// 入参: ctx 取消上下文, page 所在页面, annotation 直线注解, warning 容错诊断
// 返回: *Stream 外观流, error 属性、字体或排版错误
func (r *Reader) lineAppearance(ctx context.Context, page *Page, annotation Annotation, warning func(Diagnostic)) (*Stream, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	points, err := r.numberArray(annotation.Dictionary["L"], 4)
	if err != nil {
		return nil, err
	}
	border, err := r.readAnnotationBorder(annotation)
	if err != nil {
		return nil, err
	}
	if border.style == "B" || border.style == "I" || border.style == "U" {
		return nil, &UnsupportedError{Feature: "line border style " + string(border.style)}
	}
	var leader [3]float64
	for i, key := range []Name{"LL", "LLE", "LLO"} {
		value, err := r.Resolve(annotation.Dictionary[key])
		if err != nil {
			return nil, err
		}
		if value == nil {
			continue
		}
		if key == "LLE" {
			ll, err := r.Resolve(annotation.Dictionary["LL"])
			if err != nil {
				return nil, err
			}
			if ll == nil {
				return nil, fmt.Errorf("annotation leader extension requires LL")
			}
		}
		n, err := r.number(value)
		if err != nil || math.IsNaN(n) || math.IsInf(n, 0) || (i != 0 && n < 0) {
			return nil, fmt.Errorf("invalid annotation leader length")
		}
		leader[i] = n
	}
	for _, n := range points {
		if math.IsNaN(n) || math.IsInf(n, 0) {
			return nil, fmt.Errorf("invalid annotation line coordinate")
		}
	}
	dx, dy := points[2]-points[0], points[3]-points[1]
	length := math.Hypot(dx, dy)
	if math.IsInf(length, 0) {
		return nil, fmt.Errorf("annotation line length overflow")
	}
	caption, err := r.readLineCaption(ctx, page, annotation, warning)
	if err != nil {
		return nil, err
	}
	if length == 0 && (leader[0] != 0 || caption != nil) {
		return nil, fmt.Errorf("annotation line has no direction")
	}
	ux, uy := 0.0, 0.0
	if length != 0 {
		ux, uy = dx/length, dy/length
	}
	var main [4]float64
	copy(main[:], points)
	var content strings.Builder
	stroke, err := r.writeAnnotationStroke(&content, border)
	if err != nil {
		return nil, err
	}
	if leader[0] != 0 {
		sign := math.Copysign(1, leader[0])
		shift := leader[0] + sign*leader[2]
		extension := shift + sign*leader[1]
		if math.IsInf(extension, 0) {
			return nil, fmt.Errorf("annotation leader length overflow")
		}
		for i := 0; i < len(points); i += 2 {
			main[i], main[i+1] = points[i]-uy*shift, points[i+1]+ux*shift
			x0, y0 := points[i]-uy*sign*leader[2], points[i+1]+ux*sign*leader[2]
			x1, y1 := points[i]-uy*extension, points[i+1]+ux*extension
			for _, n := range []float64{main[i], main[i+1], x0, y0, x1, y1} {
				if math.IsNaN(n) || math.IsInf(n, 0) {
					return nil, fmt.Errorf("annotation leader coordinate overflow")
				}
			}
			if stroke {
				writeAnnotationOperation(&content, "m", x0, y0)
				writeAnnotationOperation(&content, "l S", x1, y1)
			}
		}
	}
	gap0, gap1 := length, length
	if caption != nil && !caption.top {
		padding := math.Max(1, border.width)
		if math.Abs(caption.offset[1]) <= caption.height/2+padding {
			gap0 = math.Max(0, length/2+caption.offset[0]-caption.width/2-padding)
			gap1 = math.Min(length, length/2+caption.offset[0]+caption.width/2+padding)
			if gap0 >= gap1 {
				gap0, gap1 = length, length
			}
		}
	}
	if stroke {
		if gap0 == gap1 {
			writeAnnotationOperation(&content, "m", main[0], main[1])
			writeAnnotationOperation(&content, "l S", main[2], main[3])
		} else {
			if gap0 > 0 {
				writeAnnotationOperation(&content, "m", main[0], main[1])
				writeAnnotationOperation(&content, "l S", main[0]+ux*gap0, main[1]+uy*gap0)
			}
			if gap1 < length {
				if len(border.dash) != 0 {
					if err := r.writeAnnotationDash(&content, border.dash, gap1); err != nil {
						return nil, err
					}
				}
				writeAnnotationOperation(&content, "m", main[0]+ux*gap1, main[1]+uy*gap1)
				writeAnnotationOperation(&content, "l S", main[2], main[3])
			}
		}
	}
	if err := r.writeAnnotationEndings(&content, annotation, border, main[:], stroke); err != nil {
		return nil, err
	}
	var resources Dictionary
	if caption != nil {
		x, y := caption.offset[0], caption.offset[1]
		if caption.top {
			y += caption.height/2 + math.Max(1, border.width)
		}
		cx, cy := main[0]+ux*(length/2+x)-uy*y, main[1]+uy*(length/2+x)+ux*y
		if math.IsNaN(cx) || math.IsInf(cx, 0) || math.IsNaN(cy) || math.IsInf(cy, 0) {
			return nil, fmt.Errorf("annotation caption coordinate overflow")
		}
		content.WriteString("q\n")
		writeAnnotationOperation(&content, "cm", ux, uy, -uy, ux, cx, cy)
		if caption.rich != nil {
			resources = caption.resources
			if err := writeRichLineCaption(ctx, &content, caption); err != nil {
				return nil, err
			}
		} else {
			resources = Dictionary{"Font": Dictionary{"CaptionFont": caption.font}}
			content.WriteString("BT\n/CaptionFont ")
			writeAnnotationOperation(&content, "Tf", annotationCaptionSize)
			content.WriteString("0 g\n")
			baseline := caption.height/2 - (caption.height - float64(len(caption.lines)-1)*annotationCaptionLeading) - caption.descent
			for i, line := range caption.lines {
				if err := ctx.Err(); err != nil {
					return nil, err
				}
				writeAnnotationOperation(&content, "Tm", 1, 0, 0, 1, -caption.widths[i]/2, baseline-float64(i)*annotationCaptionLeading)
				content.WriteByte('<')
				for _, letter := range line {
					fmt.Fprintf(&content, "%x", letter.code)
				}
				content.WriteString("> Tj\n")
			}
			content.WriteString("ET\n")
		}
		content.WriteString("Q\n")
	}
	return r.annotationAppearance(annotation, content.String(), resources)
}

// readLineCaption 读取标题及相对偏移，缺省外观采用12点字体
// 入参: ctx 取消上下文, page 所在页面, annotation 直线注解, warning 容错诊断
// 返回: *annotationLineCaption 标题或空值, error 属性或排版错误
func (r *Reader) readLineCaption(ctx context.Context, page *Page, annotation Annotation, warning func(Diagnostic)) (*annotationLineCaption, error) {
	value, err := r.Resolve(annotation.Dictionary["Cap"])
	if err != nil {
		return nil, err
	}
	if value == nil || value == Boolean(false) {
		return nil, nil
	}
	if value != Boolean(true) {
		return nil, fmt.Errorf("invalid annotation caption flag")
	}
	caption := &annotationLineCaption{}
	value, err = r.Resolve(annotation.Dictionary["CP"])
	if err != nil {
		return nil, err
	}
	switch value {
	case nil, Name("Inline"):
	case Name("Top"):
		caption.top = true
	default:
		return nil, fmt.Errorf("invalid annotation caption position")
	}
	if annotation.Dictionary["CO"] != nil {
		offset, err := r.numberArray(annotation.Dictionary["CO"], 2)
		if err != nil {
			return nil, err
		}
		for i, n := range offset {
			if math.IsNaN(n) || math.IsInf(n, 0) {
				return nil, fmt.Errorf("invalid annotation caption offset")
			}
			caption.offset[i] = n
		}
	}
	value, err = r.Resolve(annotation.Dictionary["RC"])
	if err != nil {
		return nil, err
	}
	if value != nil {
		return r.readRichLineCaption(ctx, page, value, caption)
	}
	text, err := r.ReadAnnotationTextContext(ctx, annotation, "Contents", warning)
	if err != nil || text == "" {
		return nil, err
	}
	text = strings.NewReplacer("\r\n", "\n", "\r", "\n").Replace(text)
	font, letters, object, err := r.annotationCaptionFont(ctx, page.Resources, text)
	if err != nil {
		return nil, err
	}
	caption.font = object
	state := graphicsState{hscale: 1}
	caption.lines = annotationLines(letters, state, annotationCaptionSize, math.Inf(1), true)
	caption.widths = make([]float64, len(caption.lines))
	for i, line := range caption.lines {
		caption.widths[i] = annotationLineWidth(line, state, annotationCaptionSize)
		caption.width = math.Max(caption.width, caption.widths[i])
	}
	ascent, descent := annotationCaptionSize*.8, -annotationCaptionSize*.2
	if box := font.BoundingBox; box != nil && box.YMin < box.YMax {
		ascent, descent = box.YMax*annotationCaptionSize/1000, box.YMin*annotationCaptionSize/1000
	}
	caption.descent = descent
	caption.height = ascent - descent + float64(len(caption.lines)-1)*annotationCaptionLeading
	if math.IsNaN(caption.width) || math.IsInf(caption.width, 0) || math.IsNaN(caption.height) || math.IsInf(caption.height, 0) {
		return nil, fmt.Errorf("annotation caption dimensions overflow")
	}
	return caption, nil
}
