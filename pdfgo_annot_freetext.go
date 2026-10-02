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

// freeTextAppearance 按文字内框、背景及引出线生成自由文本外观
// 入参: ctx 取消上下文, page 所在页面, annotation 自由文本注解
// 返回: *Stream 外观流, error 属性、字体或排版错误
func (r *Reader) freeTextAppearance(ctx context.Context, page *Page, annotation Annotation) (*Stream, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	dict := annotation.Dictionary
	value, err := r.Resolve(dict["IT"])
	if err != nil {
		return nil, err
	}
	switch value {
	case nil, Name("FreeText"), Name("FreeTextTypeWriter"), Name("FreeTextCallout"):
	default:
		return nil, fmt.Errorf("invalid free text intent")
	}
	callout := value == Name("FreeTextCallout")
	cloud, err := r.annotationBorderEffect(dict["BE"])
	if err != nil {
		return nil, err
	}
	box := annotation.Rect
	w, h := box.XMax-box.XMin, box.YMax-box.YMin
	for _, n := range []float64{box.XMin, box.YMin, box.XMax, box.YMax, w, h} {
		if math.IsNaN(n) || math.IsInf(n, 0) {
			return nil, fmt.Errorf("invalid free text bounds")
		}
	}
	frame, err := r.annotationContentBounds(annotation)
	if err != nil {
		return nil, err
	}
	frame.XMin, frame.XMax = frame.XMin-box.XMin, frame.XMax-box.XMin
	frame.YMin, frame.YMax = frame.YMin-box.YMin, frame.YMax-box.YMin
	color, err := r.annotationDefaultColor(ctx, dict["DA"])
	if err != nil {
		return nil, err
	}
	borderAnnotation := annotation
	borderAnnotation.Dictionary = maps.Clone(dict)
	borderAnnotation.Dictionary["C"] = color
	border, err := r.readAnnotationBorder(borderAnnotation)
	if err != nil {
		return nil, err
	}
	var content strings.Builder
	stroke, err := r.writeAnnotationStroke(&content, border)
	if err != nil {
		return nil, err
	}
	if callout {
		if err := r.writeFreeTextCallout(&content, annotation, border, stroke); err != nil {
			return nil, err
		}
	}
	fw, fh := frame.XMax-frame.XMin, frame.YMax-frame.YMin
	value, err = r.Resolve(dict["C"])
	if err != nil {
		return nil, err
	}
	fill := false
	if value != nil {
		components, ok := value.(Array)
		if !ok {
			return nil, fmt.Errorf("invalid annotation background color")
		}
		fill, err = r.writeAnnotationColor(&content, components, false)
		if err != nil {
			return nil, err
		}
		if fill && cloud == 0 {
			fmt.Fprintf(&content, "%g %g %g %g re f\n", frame.XMin, frame.YMin, fw, fh)
		}
	}
	if cloud > 0 {
		outline := frame
		if stroke {
			inset := math.Min(border.width/2, math.Min(fw, fh)/2)
			outline = Rectangle{frame.XMin + inset, frame.YMin + inset, frame.XMax - inset, frame.YMax - inset}
		}
		if err := writeAnnotationCloudFrame(&content, outline, cloud, false); err != nil {
			return nil, err
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
	} else if stroke {
		inset := math.Min(border.width/2, math.Min(fw, fh)/2)
		switch border.style {
		case "S", "D":
			fmt.Fprintf(&content, "%g %g %g %g re S\n", frame.XMin+inset, frame.YMin+inset, fw-2*inset, fh-2*inset)
		case "U":
			fmt.Fprintf(&content, "%g %g m %g %g l S\n", frame.XMin, frame.YMin+inset, frame.XMax, frame.YMin+inset)
		case "B", "I":
			if err := r.writeAnnotationRelief(&content, border, frame); err != nil {
				return nil, err
			}
		default:
			return nil, &UnsupportedError{Feature: "generated free text border style " + string(border.style)}
		}
	}
	resources := maps.Clone(page.Resources)
	if resources == nil {
		resources = Dictionary{}
	}
	if err := r.writeAnnotationText(ctx, annotation, nil, 0, frame, border, resources, &content); err != nil {
		return nil, err
	}
	local := annotation
	local.Rect = Rectangle{0, 0, w, h}
	return r.annotationAppearance(local, content.String(), resources)
}

// writeFreeTextCallout 按页面坐标绘制折线，端部样式只作用于起点
// 入参: content 外观内容, annotation 自由文本注解, border 笔画样式, stroke 是否可见
// 返回: error 引出线坐标或端部样式错误
func (r *Reader) writeFreeTextCallout(content *strings.Builder, annotation Annotation, border *annotationBorder, stroke bool) error {
	value, err := r.Resolve(annotation.Dictionary["CL"])
	if err != nil || value == nil {
		return err
	}
	array, ok := value.(Array)
	if !ok || len(array) != 4 && len(array) != 6 {
		return fmt.Errorf("invalid free text callout coordinates")
	}
	points, err := r.numberArray(array, len(array))
	if err != nil {
		return err
	}
	for i := range points {
		origin := annotation.Rect.XMin
		if i%2 != 0 {
			origin = annotation.Rect.YMin
		}
		points[i] -= origin
		if math.IsNaN(points[i]) || math.IsInf(points[i], 0) {
			return fmt.Errorf("invalid free text callout coordinate")
		}
	}
	if stroke {
		fmt.Fprintf(content, "%g %g m\n", points[0], points[1])
		for i := 2; i < len(points); i += 2 {
			fmt.Fprintf(content, "%g %g l\n", points[i], points[i+1])
		}
		content.WriteString("S\n")
	}
	ending, err := r.Resolve(annotation.Dictionary["LE"])
	if err != nil {
		return err
	}
	if ending == nil {
		return nil
	}
	if _, ok := ending.(Name); !ok {
		return fmt.Errorf("invalid free text callout ending")
	}
	line := Annotation{Dictionary: Dictionary{"LE": Array{ending, Name("None")}, "IC": annotation.Dictionary["C"]}}
	return r.writeAnnotationEndings(content, line, border, points, stroke)
}
