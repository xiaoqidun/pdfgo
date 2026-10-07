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
	"strconv"
	"strings"
)

// annotationBorder 保存注解边框和笔画的公共样式
type annotationBorder struct {
	width, rx, ry float64
	style         Name
	dash, color   Array
}

// ReadAnnotationBackground 按PDF标准表189读取外观特征中的背景色，空值表示透明
// 入参: annotation 注解，仅读取MK中的BG，不分析外观流
// 返回: *[3]float64 设备RGB背景色, error 字典、颜色或引用错误
func (r *Reader) ReadAnnotationBackground(annotation Annotation) (*[3]float64, error) {
	value, err := r.Resolve(annotation.Dictionary["MK"])
	if err != nil || value == nil {
		return nil, err
	}
	dict, ok := value.(Dictionary)
	if !ok {
		return nil, fmt.Errorf("invalid annotation appearance characteristics")
	}
	value, err = r.Resolve(dict["BG"])
	if err != nil || value == nil {
		return nil, err
	}
	array, ok := value.(Array)
	if !ok || len(array) != 0 && len(array) != 1 && len(array) != 3 && len(array) != 4 {
		return nil, fmt.Errorf("invalid annotation background color")
	}
	if len(array) == 0 {
		return nil, nil
	}
	var values [4]float64
	for i, component := range array {
		n, err := r.number(component)
		if err != nil || math.IsNaN(n) || math.IsInf(n, 0) || n < 0 || n > 1 {
			return nil, fmt.Errorf("invalid annotation background component")
		}
		values[i] = n
	}
	color := [3]float64{values[0], values[1], values[2]}
	if len(array) == 1 {
		color = [3]float64{values[0], values[0], values[0]}
	} else if len(array) == 4 {
		color = deviceCMYKRGB(values[:])
	}
	return &color, nil
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
		switch style {
		case "S", "D", "B", "I", "U":
		default:
			style = "S"
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

// writeAnnotationOperation 以PDF十进制数字编码外观操作，避免科学计数法
// 入参: content 外观内容, operator 操作符, values 数值
func writeAnnotationOperation(content *strings.Builder, operator string, values ...float64) {
	var buffer [256]byte
	data := buffer[:0]
	for _, value := range values {
		data = strconv.AppendFloat(data, value, 'f', -1, 64)
		data = append(data, ' ')
	}
	data = append(data, operator...)
	data = append(data, '\n')
	content.Write(data)
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
	content.WriteString(strconv.FormatFloat(border.width, 'f', -1, 64))
	content.WriteString(" w ")
	if err := r.writeAnnotationDash(content, border.dash, 0); err != nil {
		return false, err
	}
	return true, nil
}

// writeAnnotationDash 写入虚线数组及沿路径推进的相位
// 入参: content 外观内容, dash 虚线数组, phase 起始相位
// 返回: error 虚线分量错误
func (r *Reader) writeAnnotationDash(content *strings.Builder, dash Array, phase float64) error {
	content.WriteByte('[')
	for _, component := range dash {
		n, err := r.number(component)
		if err != nil || n < 0 || math.IsNaN(n) || math.IsInf(n, 0) {
			return fmt.Errorf("invalid annotation dash component")
		}
		content.WriteString(strconv.FormatFloat(n, 'f', -1, 64))
		content.WriteByte(' ')
	}
	content.WriteString("] ")
	writeAnnotationOperation(content, "d", phase)
	return nil
}

// writeAnnotationColor 写入设备颜色，空数组表示透明
// 入参: content 外观内容, values 颜色分量, stroke 是否为描边
// 返回: bool 是否有颜色, error 颜色错误
func (r *Reader) writeAnnotationColor(content *strings.Builder, values Array, stroke bool) (bool, error) {
	if len(values) == 0 {
		return false, nil
	}
	var op string
	switch len(values) {
	case 1:
		op = "g"
	case 3:
		op = "rg"
	case 4:
		op = "k"
	default:
		return false, fmt.Errorf("invalid annotation color components")
	}
	if stroke {
		op = strings.ToUpper(op)
	}
	var numbers [4]float64
	for i, component := range values {
		n, err := r.number(component)
		if err != nil || n < 0 || n > 1 || math.IsNaN(n) {
			return false, fmt.Errorf("invalid annotation color component")
		}
		numbers[i] = n
	}
	writeAnnotationOperation(content, op, numbers[:len(values)]...)
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
			if err := r.writeAnnotationRelief(&content, border, Rectangle{0, 0, w, h}); err != nil {
				return nil, err
			}
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

// textMarkupAppearance 按文字四边形生成高亮、下划线、删除线或波浪线外观
// 入参: annotation 文字标记注解
// 返回: *Stream 外观流, error 坐标或颜色错误
func (r *Reader) textMarkupAppearance(annotation Annotation) (*Stream, error) {
	value, err := r.Resolve(annotation.Dictionary["QuadPoints"])
	if err != nil {
		return nil, err
	}
	array, ok := value.(Array)
	if !ok || len(array) == 0 || len(array)%8 != 0 {
		return nil, fmt.Errorf("invalid text markup quadrilaterals")
	}
	values, err := r.numberArray(array, len(array))
	if err != nil {
		return nil, err
	}
	border, err := r.readAnnotationBorder(annotation)
	if err != nil {
		return nil, err
	}
	var content strings.Builder
	visible, err := r.writeAnnotationColor(&content, border.color, annotation.Subtype != "Highlight")
	if err != nil {
		return nil, err
	}
	for i := 0; i < len(values); i += 8 {
		points := [4]Point{}
		for j := range points {
			points[j] = Point{values[i+j*2], values[i+j*2+1]}
		}
		points, err = annotationQuadrilateral(points)
		if err != nil {
			return nil, err
		}
		a, b, c, d := points[0], points[1], points[2], points[3]
		cross := func(a, b, c Point) float64 { return (b.X-a.X)*(c.Y-a.Y) - (b.Y-a.Y)*(c.X-a.X) }
		dx, dy := b.X-a.X, b.Y-a.Y
		length := math.Hypot(dx, dy)
		height := (cross(a, b, c) + cross(a, b, d)) / (2 * length)
		width := math.Min(1, height/12)
		if math.IsNaN(height) || math.IsInf(height, 0) || math.IsInf(length, 0) || width <= 0 {
			return nil, fmt.Errorf("invalid text markup dimensions")
		}
		if !visible {
			continue
		}
		if annotation.Subtype == "Highlight" {
			fmt.Fprintf(&content, "%g %g m %g %g l %g %g l %g %g l h f\n", a.X, a.Y, b.X, b.Y, c.X, c.Y, d.X, d.Y)
			continue
		}
		fmt.Fprintf(&content, "%g w\n", width)
		if annotation.Subtype == "StrikeOut" {
			a, b = Point{(a.X + d.X) / 2, (a.Y + d.Y) / 2}, Point{(b.X + c.X) / 2, (b.Y + c.Y) / 2}
		}
		fmt.Fprintf(&content, "%g %g m\n", a.X, a.Y)
		if annotation.Subtype == "Squiggly" {
			steps := math.Ceil(length / (2 * width))
			if math.IsInf(steps, 0) || steps >= float64(int(^uint(0)>>1)) {
				return nil, fmt.Errorf("text markup wave count overflow")
			}
			for j := 1; j < int(steps); j++ {
				distance := float64(j) * length / steps
				offset := float64(j%2) * 2 * width
				fmt.Fprintf(&content, "%g %g l\n", a.X+(distance*dx-offset*dy)/length, a.Y+(distance*dy+offset*dx)/length)
			}
		}
		fmt.Fprintf(&content, "%g %g l S\n", b.X, b.Y)
	}
	return r.annotationAppearance(annotation, content.String(), nil)
}

// shapeAppearance 按图形边界或顶点、笔画和内部颜色生成外观
// 入参: annotation 图形注解
// 返回: *Stream 外观流, error 属性错误或不支持的边框效果
func (r *Reader) shapeAppearance(annotation Annotation) (*Stream, error) {
	border, err := r.readAnnotationBorder(annotation)
	if err != nil {
		return nil, err
	}
	cloud := 0.0
	if annotation.Subtype != "PolyLine" {
		cloud, err = r.annotationBorderEffect(annotation.Dictionary["BE"])
		if err != nil {
			return nil, err
		}
	}
	if cloud == 0 && (border.style == "B" || border.style == "I" || border.style == "U") {
		return nil, &UnsupportedError{Feature: "shape border style " + string(border.style)}
	}
	box := annotation.Rect
	inner := box
	if annotation.Subtype == "Square" || annotation.Subtype == "Circle" {
		inner, err = r.annotationContentBounds(annotation)
		if err != nil {
			return nil, err
		}
	}
	if inner.XMin >= inner.XMax || inner.YMin >= inner.YMax {
		return nil, fmt.Errorf("empty annotation shape")
	}
	var content strings.Builder
	stroke, err := r.writeAnnotationStroke(&content, border)
	if err != nil {
		return nil, err
	}
	value, err := r.Resolve(annotation.Dictionary["IC"])
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
	var vertices []float64
	if annotation.Subtype == "Polygon" || annotation.Subtype == "PolyLine" {
		value, err := r.Resolve(annotation.Dictionary["Vertices"])
		if err != nil {
			return nil, err
		}
		points, ok := value.(Array)
		minimum := 4
		if annotation.Subtype == "Polygon" {
			minimum = 6
		}
		if !ok || len(points) < minimum || len(points)%2 != 0 {
			return nil, fmt.Errorf("invalid annotation vertices")
		}
		vertices, err = r.numberArray(points, len(points))
		if err != nil {
			return nil, err
		}
		if annotation.Dictionary["Path"] != nil {
			return nil, &UnsupportedError{Feature: "annotation curved path"}
		}
		if cloud > 0 {
			if err := writeAnnotationCloud(&content, vertices, 3*cloud); err != nil {
				return nil, err
			}
		} else {
			fmt.Fprintf(&content, "%g %g m\n", vertices[0], vertices[1])
			for j := 2; j < len(vertices); j += 2 {
				fmt.Fprintf(&content, "%g %g l\n", vertices[j], vertices[j+1])
			}
			if annotation.Subtype == "Polygon" {
				content.WriteString("h\n")
			} else {
				fill = false
			}
		}
	} else if cloud > 0 {
		if err := writeAnnotationCloudFrame(&content, inner, cloud, annotation.Subtype == "Circle"); err != nil {
			return nil, err
		}
	} else if annotation.Subtype == "Square" {
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
	if annotation.Subtype == "PolyLine" {
		if err := r.writeAnnotationEndings(&content, annotation, border, vertices, stroke); err != nil {
			return nil, err
		}
	}
	return r.annotationAppearance(annotation, content.String(), nil)
}

// annotationBorderEffect 读取云线强度，普通效果忽略强度字段
// 入参: object 边框效果字典或引用
// 返回: float64 云线强度, error 属性错误
func (r *Reader) annotationBorderEffect(object Object) (float64, error) {
	value, err := r.Resolve(object)
	if err != nil || value == nil {
		return 0, err
	}
	dict, ok := value.(Dictionary)
	if !ok {
		return 0, fmt.Errorf("invalid annotation border effect")
	}
	value, err = r.Resolve(dict["S"])
	if err != nil {
		return 0, err
	}
	switch value {
	case nil, Name("S"):
		return 0, nil
	case Name("C"):
		intensity := 0.0
		value, err := r.Resolve(dict["I"])
		if err != nil {
			return 0, err
		}
		if value != nil {
			intensity, err = r.number(value)
			if err != nil || math.IsNaN(intensity) || intensity < 0 || intensity > 2 {
				return 0, fmt.Errorf("invalid annotation border intensity")
			}
		}
		return intensity, nil
	default:
		return 0, fmt.Errorf("invalid annotation border effect style")
	}
}

// writeAnnotationEndings 按端点切线方向绘制标准线端样式，闭合样式使用IC内部颜色
// 入参: content 外观内容, annotation 注解, border 笔画样式, points 顶点坐标, stroke 是否可见
// 返回: error 样式或端点错误
func (r *Reader) writeAnnotationEndings(content *strings.Builder, annotation Annotation, border *annotationBorder, points []float64, stroke bool) error {
	value, err := r.Resolve(annotation.Dictionary["LE"])
	if err != nil {
		return err
	}
	if value == nil {
		return nil
	}
	endings, ok := value.(Array)
	if !ok || len(endings) != 2 {
		return fmt.Errorf("invalid annotation line endings")
	}
	for index, item := range endings {
		value, err := r.Resolve(item)
		if err != nil {
			return err
		}
		style, ok := value.(Name)
		if !ok {
			return fmt.Errorf("invalid annotation line ending")
		}
		switch style {
		case "None":
			continue
		case "Square", "Circle", "Diamond", "OpenArrow", "ClosedArrow", "Butt", "ROpenArrow", "RClosedArrow", "Slash":
		default:
			return &UnsupportedError{Feature: "annotation line ending " + string(style)}
		}
		if !stroke {
			continue
		}
		start, step := 0, 2
		if index == 1 {
			start, step = len(points)-2, -2
		}
		x, y := points[start], points[start+1]
		dx, dy := 0.0, 0.0
		for j := start + step; j >= 0 && j < len(points); j += step {
			dx, dy = points[j]-x, points[j+1]-y
			if dx != 0 || dy != 0 {
				break
			}
		}
		length := math.Hypot(dx, dy)
		if length == 0 {
			return fmt.Errorf("annotation line ending has no direction")
		}
		dx, dy = dx/length, dy/length
		fmt.Fprintf(content, "q [] 0 d %g %g %g %g %g %g cm\n", dx, dy, -dy, dx, x, y)
		fill := false
		value, err = r.Resolve(annotation.Dictionary["IC"])
		if err != nil {
			return err
		}
		if value != nil {
			components, ok := value.(Array)
			if !ok {
				return fmt.Errorf("invalid annotation interior color")
			}
			fill, err = r.writeAnnotationColor(content, components, false)
			if err != nil {
				return err
			}
		}
		size, closed := 3*border.width, false
		switch style {
		case "Square":
			fmt.Fprintf(content, "%g %g %g %g re\n", -size/2, -size/2, size, size)
			closed = true
		case "Diamond":
			fmt.Fprintf(content, "%g 0 m 0 %g l %g 0 l 0 %g l h\n", -size/2, size/2, size/2, -size/2)
			closed = true
		case "Circle":
			radius := size / 2
			k := 4 * (math.Sqrt2 - 1) / 3 * radius
			fmt.Fprintf(content, "%g 0 m %g %g %g %g 0 %g c %g %g %g %g %g 0 c %g %g %g %g 0 %g c %g %g %g %g %g 0 c h\n", radius, radius, k, k, radius, radius, -k, radius, -radius, k, -radius, -radius, -k, -k, -radius, -radius, k, -radius, radius, -k, radius)
			closed = true
		case "OpenArrow", "ClosedArrow", "ROpenArrow", "RClosedArrow":
			x := size
			if style == "ROpenArrow" || style == "RClosedArrow" {
				x = -x
			}
			fmt.Fprintf(content, "%g %g m 0 0 l %g %g l\n", x, size/2, x, -size/2)
			closed = style == "ClosedArrow" || style == "RClosedArrow"
			if closed {
				content.WriteString("h\n")
			}
		case "Butt":
			fmt.Fprintf(content, "0 %g m 0 %g l\n", -size/2, size/2)
		case "Slash":
			fmt.Fprintf(content, "%g %g m %g %g l\n", -size/4, -size*math.Sqrt(3)/4, size/4, size*math.Sqrt(3)/4)
		}
		if closed && fill {
			content.WriteString("B Q\n")
		} else {
			content.WriteString("S Q\n")
		}
	}
	return nil
}

// annotationContentBounds 按矩形差值取得注解内部绘制区域
// 入参: annotation 注解
// 返回: Rectangle 内部区域, error 差值或边界错误
func (r *Reader) annotationContentBounds(annotation Annotation) (Rectangle, error) {
	box := annotation.Rect
	if annotation.Dictionary["RD"] != nil {
		inset, err := r.numberArray(annotation.Dictionary["RD"], 4)
		if err != nil {
			return Rectangle{}, err
		}
		for _, value := range inset {
			if value < 0 || math.IsNaN(value) || math.IsInf(value, 0) {
				return Rectangle{}, fmt.Errorf("invalid annotation inset")
			}
		}
		box.XMin += inset[0]
		box.YMax -= inset[1]
		box.XMax -= inset[2]
		box.YMin += inset[3]
	}
	if box.XMin >= box.XMax || box.YMin >= box.YMax {
		return Rectangle{}, fmt.Errorf("empty annotation content bounds")
	}
	return box, nil
}

// annotationAppearance 将组合图形的不透明度应用一次，避免填充和描边重叠变暗
// 入参: annotation 注解, content 外观绘制内容, resources 外观资源
// 返回: *Stream 外观流, error 不透明度错误
func (r *Reader) annotationAppearance(annotation Annotation, content string, resources Dictionary) (*Stream, error) {
	box := annotation.Rect
	bbox := Array{Real(box.XMin), Real(box.YMin), Real(box.XMax), Real(box.YMax)}
	body := &Stream{Dictionary: Dictionary{"Subtype": Name("Form"), "BBox": bbox}, Data: []byte(content), reader: r}
	if resources != nil {
		body.Dictionary["Resources"] = resources
	}
	if annotation.Dictionary["CA"] == nil && annotation.Subtype != "Highlight" {
		return body, nil
	}
	alpha := 1.0
	if annotation.Dictionary["CA"] != nil {
		var err error
		alpha, err = r.number(annotation.Dictionary["CA"])
		if err != nil || alpha < 0 || alpha > 1 || math.IsNaN(alpha) {
			return nil, fmt.Errorf("invalid annotation opacity")
		}
	}
	if alpha == 1 && annotation.Subtype != "Highlight" {
		return body, nil
	}
	body.Dictionary["Group"] = Dictionary{"S": Name("Transparency"), "I": Boolean(true)}
	state := Dictionary{"CA": Real(alpha), "ca": Real(alpha)}
	if annotation.Subtype == "Highlight" {
		state["BM"] = Name("Multiply")
	}
	resources = Dictionary{"XObject": Dictionary{"Shape": body}, "ExtGState": Dictionary{"Opacity": state}}
	return &Stream{Dictionary: Dictionary{"Subtype": Name("Form"), "BBox": bbox, "Resources": resources}, Data: []byte("/Opacity gs /Shape Do"), reader: r}, nil
}
