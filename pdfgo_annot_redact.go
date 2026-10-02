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

// Redaction 保存待删改区域和移除内容后使用的覆盖属性，不表示内容已安全移除
// Regions使用页面默认用户坐标，Overlay存在时其余覆盖属性不读取
type Redaction struct {
	Regions           [][4]Point
	Overlay           *Stream
	InteriorColor     *[3]float64
	OverlayText       string
	Repeat            bool
	DefaultAppearance String
	Alignment         int
}

// ReadRedaction 读取删改注解的区域及覆盖属性，不执行内容移除或覆盖绘制
// 入参: ctx 取消上下文, annotation 删改注解
// 返回: Redaction 删改信息, error 类型、区域或覆盖属性错误
func (r *Reader) ReadRedaction(ctx context.Context, annotation Annotation) (Redaction, error) {
	var redaction Redaction
	if annotation.Subtype != "Redact" {
		return redaction, fmt.Errorf("annotation is not a redaction")
	}
	var err error
	redaction.Regions, err = r.ReadRedactionRegions(ctx, annotation)
	if err != nil {
		return redaction, err
	}
	value, err := r.Resolve(annotation.Dictionary["RO"])
	if err != nil {
		return redaction, err
	}
	if value != nil {
		stream, ok := value.(*Stream)
		if !ok {
			return redaction, fmt.Errorf("invalid redaction overlay form")
		}
		subtype, err := r.Resolve(stream.Dictionary["Subtype"])
		if err != nil {
			return redaction, err
		}
		if subtype != Name("Form") {
			return redaction, fmt.Errorf("invalid redaction overlay form")
		}
		redaction.Overlay = stream
		return redaction, nil
	}
	value, err = r.Resolve(annotation.Dictionary["IC"])
	if err != nil {
		return redaction, err
	}
	if value != nil {
		array, ok := value.(Array)
		if !ok || len(array) != 3 {
			return redaction, fmt.Errorf("invalid redaction interior color")
		}
		var color [3]float64
		for i, component := range array {
			color[i], err = r.number(component)
			if err != nil || math.IsNaN(color[i]) || color[i] < 0 || color[i] > 1 {
				return redaction, fmt.Errorf("invalid redaction interior component")
			}
		}
		redaction.InteriorColor = &color
	}
	value, err = r.Resolve(annotation.Dictionary["OverlayText"])
	if err != nil {
		return redaction, err
	}
	if value != nil {
		text, ok := value.(String)
		if !ok {
			return redaction, fmt.Errorf("invalid redaction overlay text")
		}
		redaction.OverlayText, err = DecodeTextString(text)
		if err != nil {
			return redaction, err
		}
		appearance, err := r.Resolve(annotation.Dictionary["DA"])
		if err != nil {
			return redaction, err
		}
		redaction.DefaultAppearance, ok = appearance.(String)
		if !ok {
			return redaction, fmt.Errorf("missing redaction overlay default appearance")
		}
		redaction.DefaultAppearance = append(String(nil), redaction.DefaultAppearance...)
	}
	value, err = r.Resolve(annotation.Dictionary["Repeat"])
	if err != nil {
		return redaction, err
	}
	if value != nil {
		repeat, ok := value.(Boolean)
		if !ok {
			return redaction, fmt.Errorf("invalid redaction repeat flag")
		}
		redaction.Repeat = bool(repeat)
	}
	value, err = r.Resolve(annotation.Dictionary["Q"])
	if err != nil {
		return redaction, err
	}
	if value != nil {
		alignment, ok := value.(Integer)
		if !ok || alignment < 0 || alignment > 2 {
			return redaction, fmt.Errorf("invalid redaction overlay justification")
		}
		redaction.Alignment = int(alignment)
	}
	return redaction, nil
}

// ReadRedactionRegions 读取删改区域，QuadPoints优先于Rect，保留空数组所表示的空区域
// 入参: ctx 取消上下文, annotation 删改注解
// 返回: [][4]Point 逆时针四边形, error 类型或几何错误
func (r *Reader) ReadRedactionRegions(ctx context.Context, annotation Annotation) ([][4]Point, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if annotation.Subtype != "Redact" {
		return nil, fmt.Errorf("annotation is not a redaction")
	}
	value, err := r.Resolve(annotation.Dictionary["QuadPoints"])
	if err != nil {
		return nil, err
	}
	if value == nil {
		box := annotation.Rect
		if box.XMin == box.XMax || box.YMin == box.YMax {
			return nil, nil
		}
		quad, err := annotationQuadrilateral([4]Point{{box.XMin, box.YMin}, {box.XMax, box.YMin}, {box.XMax, box.YMax}, {box.XMin, box.YMax}})
		if err != nil {
			return nil, err
		}
		return [][4]Point{quad}, nil
	}
	array, ok := value.(Array)
	if !ok || len(array)%8 != 0 {
		return nil, fmt.Errorf("invalid redaction quadrilaterals")
	}
	regions := make([][4]Point, 0, len(array)/8)
	for i := 0; i < len(array); i += 8 {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		var points [4]Point
		for j := range points {
			points[j].X, err = r.number(array[i+2*j])
			if err != nil {
				return nil, err
			}
			points[j].Y, err = r.number(array[i+2*j+1])
			if err != nil {
				return nil, err
			}
		}
		quad, err := annotationQuadrilateral(points)
		if err != nil {
			return nil, err
		}
		regions = append(regions, quad)
	}
	return regions, nil
}

// annotationQuadrilateral 校验逆时针凸四边形，兼容既有文字标记的Z序坐标
// 入参: points 原始顶点
// 返回: [4]Point 逆时针顶点, error 坐标或几何错误
func annotationQuadrilateral(points [4]Point) ([4]Point, error) {
	for _, point := range points {
		if math.IsNaN(point.X) || math.IsNaN(point.Y) || math.IsInf(point.X, 0) || math.IsInf(point.Y, 0) {
			return points, fmt.Errorf("invalid annotation quadrilateral coordinates")
		}
	}
	cross := func(a, b, c Point) float64 { return (b.X-a.X)*(c.Y-a.Y) - (b.Y-a.Y)*(c.X-a.X) }
	if cross(points[0], points[1], points[2]) < 0 && cross(points[1], points[2], points[3]) > 0 {
		points = [4]Point{points[2], points[3], points[1], points[0]}
	}
	for i := range points {
		turn := cross(points[i], points[(i+1)%4], points[(i+2)%4])
		if turn <= 0 || math.IsNaN(turn) || math.IsInf(turn, 0) {
			return points, fmt.Errorf("invalid annotation quadrilateral")
		}
	}
	return points, nil
}

// redactionAppearance 生成待删改区域的笔画预览，不使用应用删改后的覆盖属性
// 入参: ctx 取消上下文, annotation 删改注解
// 返回: *Stream 待删改外观, error 取消、区域或笔画错误
func (r *Reader) redactionAppearance(ctx context.Context, annotation Annotation) (*Stream, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	regions, err := r.ReadRedactionRegions(ctx, annotation)
	if err != nil {
		return nil, err
	}
	border, err := r.readAnnotationBorder(Annotation{Dictionary: Dictionary{"C": annotation.Dictionary["C"], "Border": annotation.Dictionary["Border"]}})
	if err != nil {
		return nil, err
	}
	var content strings.Builder
	stroke, err := r.writeAnnotationStroke(&content, border)
	if err != nil {
		return nil, err
	}
	if stroke {
		for _, region := range regions {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			writeAnnotationOperation(&content, "m", region[0].X, region[0].Y)
			for _, point := range region[1:] {
				writeAnnotationOperation(&content, "l", point.X, point.Y)
			}
			content.WriteString("h S\n")
		}
	}
	return r.annotationAppearance(annotation, content.String(), nil)
}
