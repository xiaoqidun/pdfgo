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
	"strings"
)

// annotationIcons 保存标准注解名称对应的矢量图标，不依赖外部字体
var annotationIcons = map[Name]string{
	"Note":         "2 2 12 12 re B 4 11 m 12 11 l 4 8 m 12 8 l 4 5 m 9 5 l S",
	"Comment":      "2 5 m 2 14 l 14 14 l 14 5 l 7 5 l 3 2 l 3 5 l h B",
	"Key":          "6 9 m 2 9 2 14 6 14 c 10 14 10 9 6 9 c h B 8 9 m 14 3 l 12 1 l 10 3 l 11 4 l 9 6 l S",
	"Help":         "5 11 m 5 15 12 15 12 11 c 12 8 8 9 8 6 c S 7.5 2 1 1 re f",
	"Paragraph":    "9 2 m 9 14 l 5 14 l 1 14 1 8 5 8 c 9 8 l S 12 14 m 12 2 l S",
	"NewParagraph": "9 2 m 9 14 l 5 14 l 1 14 1 8 5 8 c 9 8 l S 12 14 m 12 2 l 2 4 m 6 4 l 4 2 m 4 6 l S",
	"Insert":       "2 3 m 8 13 l 14 3 l S",
	"Graph":        "2 14 m 2 2 l 14 2 l 4 4 m 7 8 l 10 6 l 14 12 l S",
	"PushPin":      "5 14 m 11 14 l 10 10 l 13 7 l 3 7 l 6 10 l h B 8 7 m 8 1 l S",
	"Paperclip":    "11 5 m 5 11 l 7 13 10 13 12 11 c 16 7 10 1 6 3 c 1 6 2 10 5 13 c 8 16 l S",
	"Tag":          "2 2 m 2 9 l 8 15 l 15 8 l 9 2 l h B 7 11 1 1 re f",
	"Speaker":      "2 6 m 5 6 l 10 2 l 10 14 l 5 10 l 2 10 l h B 12 5 m 15 8 15 8 12 11 c S",
	"Mic":          "5 8 m 5 15 11 15 11 8 c 11 3 5 3 5 8 c h B 3 8 m 3 1 13 1 13 8 c 8 3 m 8 1 l 5 1 m 11 1 l S",
}

// iconAppearance 为便笺、附件、声音和插入标记生成缺省外观
// 入参: annotation 注解
// 返回: *Stream 矢量外观, error 图标或颜色错误
func (r *Reader) iconAppearance(annotation Annotation) (*Stream, error) {
	key, name := Name("Name"), Name("Note")
	switch annotation.Subtype {
	case "FileAttachment":
		name = "PushPin"
	case "Sound":
		name = "Speaker"
	case "Caret":
		key, name = "Sy", "None"
	}
	value, err := r.Resolve(annotation.Dictionary[key])
	if err != nil {
		return nil, err
	}
	if value != nil {
		var ok bool
		name, ok = value.(Name)
		if !ok {
			return nil, fmt.Errorf("invalid annotation icon name")
		}
	}
	if annotation.Subtype == "Caret" {
		switch name {
		case "None":
			name = "Insert"
		case "P":
			name = "Paragraph"
		default:
			return nil, &UnsupportedError{Feature: "caret symbol " + string(name)}
		}
	}
	path, ok := annotationIcons[name]
	if !ok {
		return nil, &UnsupportedError{Feature: "annotation icon " + string(name)}
	}
	border, err := r.readAnnotationBorder(annotation)
	if err != nil {
		return nil, err
	}
	box := annotation.Rect
	if annotation.Subtype == "Caret" {
		box, err = r.annotationContentBounds(annotation)
		if err != nil {
			return nil, err
		}
	}
	var content strings.Builder
	fmt.Fprintf(&content, "%g 0 0 %g %g %g cm 1 w 1 J 1 j 0 G\n", (box.XMax-box.XMin)/16, (box.YMax-box.YMin)/16, box.XMin, box.YMin)
	if annotation.Subtype == "Text" && annotation.Dictionary["C"] == nil {
		border.color = Array{Integer(1), Integer(1), Integer(0)}
	}
	visible, err := r.writeAnnotationColor(&content, border.color, false)
	if err != nil {
		return nil, err
	}
	if !visible {
		path = strings.ReplaceAll(path, " B", " S")
	}
	if annotation.Subtype == "Caret" {
		if !visible {
			return r.annotationAppearance(annotation, "", nil)
		}
		if _, err := r.writeAnnotationColor(&content, border.color, true); err != nil {
			return nil, err
		}
	}
	content.WriteString(path)
	return r.annotationAppearance(annotation, content.String(), nil)
}
