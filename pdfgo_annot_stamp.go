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

// annotationStampLabels 保存标准图章名称对应的缺省标题，图形样式由阅读器定义
var annotationStampLabels = map[Name]string{
	"Approved":            "APPROVED",
	"Experimental":        "EXPERIMENTAL",
	"NotApproved":         "NOT APPROVED",
	"AsIs":                "AS IS",
	"Expired":             "EXPIRED",
	"NotForPublicRelease": "NOT FOR PUBLIC RELEASE",
	"Confidential":        "CONFIDENTIAL",
	"Final":               "FINAL",
	"Sold":                "SOLD",
	"Departmental":        "DEPARTMENTAL",
	"ForComment":          "FOR COMMENT",
	"TopSecret":           "TOP SECRET",
	"Draft":               "DRAFT",
	"ForPublicRelease":    "FOR PUBLIC RELEASE",
}

// stampAppearance 为十四种标准橡皮图章生成缺省外观，标题按字体度量居中并缩放
// 入参: annotation 图章注解
// 返回: *Stream 矢量与标题外观, error 名称、颜色或尺寸错误
func (r *Reader) stampAppearance(annotation Annotation) (*Stream, error) {
	value, err := r.Resolve(annotation.Dictionary["Name"])
	if err != nil {
		return nil, err
	}
	name := Name("Draft")
	if value != nil {
		var ok bool
		name, ok = value.(Name)
		if !ok {
			return nil, fmt.Errorf("invalid stamp icon name")
		}
	}
	label, ok := annotationStampLabels[name]
	if !ok {
		return nil, &UnsupportedError{Feature: "stamp icon " + string(name)}
	}
	box := annotation.Rect
	w, h := box.XMax-box.XMin, box.YMax-box.YMin
	if w <= 0 || h <= 0 || math.IsInf(w, 0) || math.IsInf(h, 0) || math.IsNaN(w) || math.IsNaN(h) {
		return nil, fmt.Errorf("invalid stamp bounds")
	}
	fontObject := Dictionary{"Type": Name("Font"), "Subtype": Name("Type1"), "BaseFont": Name("Helvetica-Bold"), "Encoding": Name("WinAnsiEncoding")}
	font, err := r.ReadFont(fontObject)
	if err != nil {
		return nil, err
	}
	glyphs, err := font.Decode([]byte(label))
	if err != nil {
		return nil, err
	}
	advance := 0.0
	for _, glyph := range glyphs {
		advance += glyph.Width
	}
	ascent, descent := .8, -.2
	if bounds := font.BoundingBox; bounds != nil && bounds.YMax > bounds.YMin {
		ascent, descent = bounds.YMax/1000, bounds.YMin/1000
	}
	margin := math.Min(w, h) * .08
	size := math.Min((w-4*margin)/(advance/1000), (h-4*margin)/(ascent-descent))
	x := box.XMin + (w-advance/1000*size)/2
	y := box.YMin + (h-(ascent-descent)*size)/2 - descent*size
	var content strings.Builder
	color := Array{Real(.95)}
	value, err = r.Resolve(annotation.Dictionary["C"])
	if err != nil {
		return nil, err
	}
	if value != nil {
		color, ok = value.(Array)
		if !ok {
			return nil, fmt.Errorf("invalid stamp color")
		}
	}
	fill, err := r.writeAnnotationColor(&content, color, false)
	if err != nil {
		return nil, err
	}
	foreground := 0.0
	if fill {
		space, err := r.readColorSpace(map[int]Name{1: "DeviceGray", 3: "DeviceRGB", 4: "DeviceCMYK"}[len(color)])
		if err != nil {
			return nil, err
		}
		values := make([]float64, len(color))
		for index, component := range color {
			values[index], err = r.number(component)
			if err != nil {
				return nil, err
			}
		}
		light, err := space.Luminosity(values, "")
		if err != nil {
			return nil, err
		}
		if light < .5 {
			foreground = 1
		}
	}
	writeAnnotationOperation(&content, "w", math.Min(w, h)*.03)
	writeAnnotationOperation(&content, "G", foreground)
	writeAnnotationOperation(&content, "re", box.XMin+margin, box.YMin+margin, w-2*margin, h-2*margin)
	if fill {
		content.WriteString("B\n")
	} else {
		content.WriteString("S\n")
	}
	writeAnnotationOperation(&content, "g", foreground)
	content.WriteString("BT\n/StampFont ")
	writeAnnotationOperation(&content, "Tf", size)
	writeAnnotationOperation(&content, "Tm", 1, 0, 0, 1, x, y)
	fmt.Fprintf(&content, "<%X> Tj\nET\n", []byte(label))
	resources := Dictionary{"Font": Dictionary{"StampFont": fontObject}}
	return r.annotationAppearance(annotation, content.String(), resources)
}
