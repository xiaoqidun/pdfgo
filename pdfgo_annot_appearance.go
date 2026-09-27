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
	"fmt"
	"math"
	"strings"
)

// annotationBorder 保存注解边框和笔画的公共样式
type annotationBorder struct {
	width, rx, ry float64
	style         Name
	dash, color   Array
}

// readAnnotationBorder 读取注解笔画属性，BS优先于Border
// 入参: annotation 注解
// 返回: *annotationBorder 笔画样式, error 属性错误
func (r *Reader) readAnnotationBorder(annotation Annotation) (*annotationBorder, error) {
	width, rx, ry := 1.0, 0.0, 0.0
	style := Name("S")
	var dash Array
	value, err := r.Resolve(annotation.Dictionary["BS"])
	if err != nil {
		return nil, err
	}
	if value != nil {
		border, ok := value.(Dictionary)
		if !ok {
			return nil, fmt.Errorf("invalid annotation border style")
		}
		if border["W"] != nil {
			width, err = r.number(border["W"])
			if err != nil {
				return nil, err
			}
		}
		value, err = r.Resolve(border["S"])
		if err != nil {
			return nil, err
		}
		if value != nil {
			style, ok = value.(Name)
			if !ok {
				return nil, fmt.Errorf("invalid annotation border style name")
			}
		}
		if style == "D" {
			dash = Array{Integer(3)}
			value, err = r.Resolve(border["D"])
			if err != nil {
				return nil, err
			}
			if value != nil {
				dash, ok = value.(Array)
				if !ok {
					return nil, fmt.Errorf("invalid annotation border dash")
				}
			}
		}
	} else {
		value, err = r.Resolve(annotation.Dictionary["Border"])
		if err != nil {
			return nil, err
		}
		if value != nil {
			border, ok := value.(Array)
			if !ok || len(border) < 3 || len(border) > 4 {
				return nil, fmt.Errorf("invalid annotation border")
			}
			for i, target := range []*float64{&rx, &ry, &width} {
				*target, err = r.number(border[i])
				if err != nil {
					return nil, err
				}
			}
			if len(border) == 4 {
				value, err = r.Resolve(border[3])
				if err != nil {
					return nil, err
				}
				dash, ok = value.(Array)
				if !ok {
					return nil, fmt.Errorf("invalid annotation border dash")
				}
			}
		}
	}
	for _, n := range []float64{width, rx, ry} {
		if n < 0 || math.IsNaN(n) || math.IsInf(n, 0) {
			return nil, fmt.Errorf("invalid annotation border dimensions")
		}
	}
	value, err = r.Resolve(annotation.Dictionary["C"])
	if err != nil {
		return nil, err
	}
	color := Array{Integer(0)}
	if value != nil {
		var ok bool
		color, ok = value.(Array)
		if !ok {
			return nil, fmt.Errorf("invalid annotation border color")
		}
	}
	return &annotationBorder{width: width, rx: rx, ry: ry, style: style, dash: dash, color: color}, nil
}

// writeAnnotationStroke 写入共享笔画参数，不生成路径
// 入参: content 外观内容, border 笔画样式
// 返回: bool 是否绘制笔画, error 样式错误
func (r *Reader) writeAnnotationStroke(content *strings.Builder, border *annotationBorder) (bool, error) {
	if border.width == 0 || len(border.color) == 0 {
		return false, nil
	}
	if _, err := r.writeAnnotationColor(content, border.color, true); err != nil {
		return false, err
	}
	fmt.Fprintf(content, "%g w [", border.width)
	for _, component := range border.dash {
		n, err := r.number(component)
		if err != nil || n < 0 || math.IsNaN(n) || math.IsInf(n, 0) {
			return false, fmt.Errorf("invalid annotation dash component")
		}
		fmt.Fprintf(content, "%g ", n)
	}
	content.WriteString("] 0 d\n")
	return true, nil
}

// writeAnnotationColor 写入设备颜色，空数组表示透明
// 入参: content 外观内容, values 颜色分量, stroke 是否为描边
// 返回: bool 是否有颜色, error 颜色错误
func (r *Reader) writeAnnotationColor(content *strings.Builder, values Array, stroke bool) (bool, error) {
	if len(values) == 0 {
		return false, nil
	}
	op := map[int]string{1: "g", 3: "rg", 4: "k"}[len(values)]
	if op == "" {
		return false, fmt.Errorf("invalid annotation color components")
	}
	if stroke {
		op = strings.ToUpper(op)
	}
	for _, component := range values {
		n, err := r.number(component)
		if err != nil || n < 0 || n > 1 || math.IsNaN(n) {
			return false, fmt.Errorf("invalid annotation color component")
		}
		fmt.Fprintf(content, "%g ", n)
	}
	fmt.Fprintf(content, "%s\n", op)
	return true, nil
}

// linkAppearance 按链接的颜色和边框属性生成缺省外观，边框位于注解矩形内
// 入参: annotation 链接注解
// 返回: *Stream 外观流, error 属性或未支持的边框错误
func (r *Reader) linkAppearance(annotation Annotation) (*Stream, error) {
	w, h := annotation.Rect.XMax-annotation.Rect.XMin, annotation.Rect.YMax-annotation.Rect.YMin
	if w <= 0 || h <= 0 {
		return nil, fmt.Errorf("invalid link bounds")
	}
	border, err := r.readAnnotationBorder(annotation)
	if err != nil {
		return nil, err
	}
	var content strings.Builder
	stroke, err := r.writeAnnotationStroke(&content, border)
	if err != nil {
		return nil, err
	}
	if stroke {
		width, rx, ry, style := border.width, border.rx, border.ry, border.style
		x0, y0 := math.Min(width/2, w/2), math.Min(width/2, h/2)
		x1, y1 := w-x0, h-y0
		switch style {
		case "B", "I":
			return nil, &UnsupportedError{Feature: "beveled or inset link border"}
		case "U":
			fmt.Fprintf(&content, "%g %g m %g %g l S\n", x0, y0, x1, y0)
		default:
			rx, ry = math.Min(rx, (x1-x0)/2), math.Min(ry, (y1-y0)/2)
			if rx == 0 || ry == 0 {
				fmt.Fprintf(&content, "%g %g %g %g re S\n", x0, y0, x1-x0, y1-y0)
			} else {
				k := 4 * (math.Sqrt2 - 1) / 3
				fmt.Fprintf(&content, "%g %g m %g %g l\n", x0+rx, y0, x1-rx, y0)
				fmt.Fprintf(&content, "%g %g %g %g %g %g c\n", x1-rx+k*rx, y0, x1, y0+ry-k*ry, x1, y0+ry)
				fmt.Fprintf(&content, "%g %g l %g %g %g %g %g %g c\n", x1, y1-ry, x1, y1-ry+k*ry, x1-rx+k*rx, y1, x1-rx, y1)
				fmt.Fprintf(&content, "%g %g l %g %g %g %g %g %g c\n", x0+rx, y1, x0+rx-k*rx, y1, x0, y1-ry+k*ry, x0, y1-ry)
				fmt.Fprintf(&content, "%g %g l %g %g %g %g %g %g c h S\n", x0, y0+ry, x0, y0+ry-k*ry, x0+rx-k*rx, y0, x0+rx, y0)
			}
		}
	}
	return &Stream{Dictionary: Dictionary{"Type": Name("XObject"), "Subtype": Name("Form"), "BBox": Array{Integer(0), Integer(0), Real(w), Real(h)}}, Data: []byte(content.String()), reader: r}, nil
}

// inkAppearance 按原始手写坐标生成线段外观，保留公共笔画和不透明度
// 入参: annotation 手写注解
// 返回: *Stream 外观流, error 笔画或坐标错误
func (r *Reader) inkAppearance(annotation Annotation) (*Stream, error) {
	border, err := r.readAnnotationBorder(annotation)
	if err != nil {
		return nil, err
	}
	if border.style == "B" || border.style == "I" || border.style == "U" {
		return nil, &UnsupportedError{Feature: "ink border style " + string(border.style)}
	}
	value, err := r.Resolve(annotation.Dictionary["InkList"])
	if err != nil {
		return nil, err
	}
	paths, ok := value.(Array)
	if !ok {
		return nil, fmt.Errorf("invalid ink paths")
	}
	alpha := 1.0
	if annotation.Dictionary["CA"] != nil {
		alpha, err = r.number(annotation.Dictionary["CA"])
		if err != nil || alpha < 0 || alpha > 1 || math.IsNaN(alpha) {
			return nil, fmt.Errorf("invalid annotation opacity")
		}
	}
	var content strings.Builder
	stroke, err := r.writeAnnotationStroke(&content, border)
	if err != nil {
		return nil, err
	}
	content.WriteString("/Opacity gs 1 J 1 j\n")
	for _, path := range paths {
		value, err := r.Resolve(path)
		if err != nil {
			return nil, err
		}
		points, ok := value.(Array)
		if !ok || len(points) < 2 || len(points)%2 != 0 {
			return nil, fmt.Errorf("invalid ink coordinates")
		}
		coordinates, err := r.numberArray(points, len(points))
		if err != nil {
			return nil, err
		}
		if !stroke {
			continue
		}
		fmt.Fprintf(&content, "%g %g m\n", coordinates[0], coordinates[1])
		if len(coordinates) == 2 {
			fmt.Fprintf(&content, "%g %g l\n", coordinates[0], coordinates[1])
		}
		for i := 2; i < len(coordinates); i += 2 {
			fmt.Fprintf(&content, "%g %g l\n", coordinates[i], coordinates[i+1])
		}
	}
	content.WriteString("S\n")
	box := annotation.Rect
	resources := Dictionary{"ExtGState": Dictionary{"Opacity": Dictionary{"CA": Real(alpha), "ca": Real(alpha)}}}
	return &Stream{Dictionary: Dictionary{"Type": Name("XObject"), "Subtype": Name("Form"), "BBox": Array{Real(box.XMin), Real(box.YMin), Real(box.XMax), Real(box.YMax)}, "Resources": resources}, Data: []byte(content.String()), reader: r}, nil
}

// lineAppearance 按原始端点和笔画属性生成直线注解外观
// 入参: annotation 直线注解
// 返回: *Stream 外观流, error 属性错误或未支持的附加效果
func (r *Reader) lineAppearance(annotation Annotation) (*Stream, error) {
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
	value, err := r.Resolve(annotation.Dictionary["LE"])
	if err != nil {
		return nil, err
	}
	if value != nil {
		endings, ok := value.(Array)
		if !ok || len(endings) != 2 {
			return nil, fmt.Errorf("invalid annotation line endings")
		}
		for _, ending := range endings {
			value, err := r.Resolve(ending)
			if err != nil {
				return nil, err
			}
			if value != Name("None") {
				return nil, &UnsupportedError{Feature: "annotation line ending"}
			}
		}
	}
	for _, key := range []Name{"LL", "LLE", "LLO"} {
		if annotation.Dictionary[key] == nil {
			continue
		}
		n, err := r.number(annotation.Dictionary[key])
		if err != nil || math.IsNaN(n) || math.IsInf(n, 0) || (key != "LL" && n < 0) {
			return nil, fmt.Errorf("invalid annotation leader length")
		}
		if n != 0 {
			return nil, &UnsupportedError{Feature: "annotation leader line"}
		}
	}
	value, err = r.Resolve(annotation.Dictionary["Cap"])
	if err != nil {
		return nil, err
	}
	if value != nil {
		caption, ok := value.(Boolean)
		if !ok {
			return nil, fmt.Errorf("invalid annotation caption flag")
		}
		if caption {
			return nil, &UnsupportedError{Feature: "annotation line caption"}
		}
	}
	alpha := 1.0
	if annotation.Dictionary["CA"] != nil {
		alpha, err = r.number(annotation.Dictionary["CA"])
		if err != nil || alpha < 0 || alpha > 1 || math.IsNaN(alpha) {
			return nil, fmt.Errorf("invalid annotation opacity")
		}
	}
	var content strings.Builder
	stroke, err := r.writeAnnotationStroke(&content, border)
	if err != nil {
		return nil, err
	}
	if stroke {
		fmt.Fprintf(&content, "/Opacity gs %g %g m %g %g l S\n", points[0], points[1], points[2], points[3])
	}
	box := annotation.Rect
	resources := Dictionary{"ExtGState": Dictionary{"Opacity": Dictionary{"CA": Real(alpha), "ca": Real(alpha)}}}
	return &Stream{Dictionary: Dictionary{"Subtype": Name("Form"), "BBox": Array{Real(box.XMin), Real(box.YMin), Real(box.XMax), Real(box.YMax)}, "Resources": resources}, Data: []byte(content.String()), reader: r}, nil
}

// shapeAppearance 按矩形或椭圆的边界、内缩量和颜色生成外观
// 入参: annotation 图形注解
// 返回: *Stream 外观流, error 属性错误或不支持的边框效果
func (r *Reader) shapeAppearance(annotation Annotation) (*Stream, error) {
	border, err := r.readAnnotationBorder(annotation)
	if err != nil {
		return nil, err
	}
	if border.style == "B" || border.style == "I" || border.style == "U" {
		return nil, &UnsupportedError{Feature: "shape border style " + string(border.style)}
	}
	value, err := r.Resolve(annotation.Dictionary["BE"])
	if err != nil {
		return nil, err
	}
	if value != nil {
		effect, ok := value.(Dictionary)
		if !ok {
			return nil, fmt.Errorf("invalid annotation border effect")
		}
		style, err := r.Resolve(effect["S"])
		if err != nil {
			return nil, err
		}
		if style != nil && style != Name("S") {
			return nil, &UnsupportedError{Feature: "annotation border effect"}
		}
	}
	box := annotation.Rect
	inner := box
	if annotation.Dictionary["RD"] != nil {
		inset, err := r.numberArray(annotation.Dictionary["RD"], 4)
		if err != nil {
			return nil, err
		}
		for _, n := range inset {
			if n < 0 {
				return nil, fmt.Errorf("invalid annotation inset")
			}
		}
		inner.XMin += inset[0]
		inner.YMax -= inset[1]
		inner.XMax -= inset[2]
		inner.YMin += inset[3]
	}
	if inner.XMin >= inner.XMax || inner.YMin >= inner.YMax {
		return nil, fmt.Errorf("empty annotation shape")
	}
	var content strings.Builder
	stroke, err := r.writeAnnotationStroke(&content, border)
	if err != nil {
		return nil, err
	}
	value, err = r.Resolve(annotation.Dictionary["IC"])
	if err != nil {
		return nil, err
	}
	var fill bool
	if value != nil {
		components, ok := value.(Array)
		if !ok {
			return nil, fmt.Errorf("invalid annotation interior color")
		}
		fill, err = r.writeAnnotationColor(&content, components, false)
		if err != nil {
			return nil, err
		}
	}
	if stroke {
		dx, dy := math.Min(border.width/2, (inner.XMax-inner.XMin)/2), math.Min(border.width/2, (inner.YMax-inner.YMin)/2)
		inner.XMin += dx
		inner.XMax -= dx
		inner.YMin += dy
		inner.YMax -= dy
	}
	x0, y0, x1, y1 := inner.XMin, inner.YMin, inner.XMax, inner.YMax
	if annotation.Subtype == "Square" {
		fmt.Fprintf(&content, "%g %g %g %g re\n", x0, y0, x1-x0, y1-y0)
	} else {
		cx, cy, rx, ry := (x0+x1)/2, (y0+y1)/2, (x1-x0)/2, (y1-y0)/2
		k := 4 * (math.Sqrt2 - 1) / 3
		fmt.Fprintf(&content, "%g %g m\n", x1, cy)
		fmt.Fprintf(&content, "%g %g %g %g %g %g c\n", x1, cy+k*ry, cx+k*rx, y1, cx, y1)
		fmt.Fprintf(&content, "%g %g %g %g %g %g c\n", cx-k*rx, y1, x0, cy+k*ry, x0, cy)
		fmt.Fprintf(&content, "%g %g %g %g %g %g c\n", x0, cy-k*ry, cx-k*rx, y0, cx, y0)
		fmt.Fprintf(&content, "%g %g %g %g %g %g c h\n", cx+k*rx, y0, x1, cy-k*ry, x1, cy)
	}
	switch {
	case fill && stroke:
		content.WriteString("B\n")
	case fill:
		content.WriteString("f\n")
	case stroke:
		content.WriteString("S\n")
	default:
		content.WriteString("n\n")
	}
	bbox := Array{Real(box.XMin), Real(box.YMin), Real(box.XMax), Real(box.YMax)}
	body := &Stream{Dictionary: Dictionary{"Subtype": Name("Form"), "BBox": bbox}, Data: []byte(content.String()), reader: r}
	if annotation.Dictionary["CA"] == nil {
		return body, nil
	}
	alpha, err := r.number(annotation.Dictionary["CA"])
	if err != nil || alpha < 0 || alpha > 1 || math.IsNaN(alpha) {
		return nil, fmt.Errorf("invalid annotation opacity")
	}
	if alpha == 1 {
		return body, nil
	}
	body.Dictionary["Group"] = Dictionary{"S": Name("Transparency"), "I": Boolean(true)}
	resources := Dictionary{"XObject": Dictionary{"Shape": body}, "ExtGState": Dictionary{"Opacity": Dictionary{"CA": Real(alpha), "ca": Real(alpha)}}}
	return &Stream{Dictionary: Dictionary{"Subtype": Name("Form"), "BBox": bbox, "Resources": resources}, Data: []byte("/Opacity gs /Shape Do"), reader: r}, nil
}
