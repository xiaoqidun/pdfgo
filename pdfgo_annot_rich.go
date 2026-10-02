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
	"encoding/xml"
	"fmt"
	"io"
	"strings"
)

// annotationRichStyle 保存富文本继承样式，未指定字号、字体和颜色时沿用默认外观
type annotationRichStyle struct {
	families    []annotationRichFamilyName
	size        *float64
	color       *[3]float64
	weight      int
	stretch     int
	italic      *bool
	rise        float64
	align       int
	decoration  uint8
	decorations *annotationRichDecoration
}

// annotationRichFamilyName 区分字面字体名称和未加引号的通用字体族
type annotationRichFamilyName struct {
	name    string
	generic bool
}

// annotationRichRun 保存同一样式的Unicode文字
type annotationRichRun struct {
	text  string
	style annotationRichStyle
}

// annotationRichParagraph 保存独立段落及其行内文字
type annotationRichParagraph struct {
	style annotationRichStyle
	runs  []annotationRichRun
}

// readAnnotationRich 读取PDF文本编码的富文本XML，保留段落和行内样式
// 入参: ctx 取消上下文, object 富文本对象, defaults 默认样式字符串, align 默认对齐
// 返回: []annotationRichParagraph 段落, error XML、样式或引用错误
func (r *Reader) readAnnotationRich(ctx context.Context, object Object, defaults Object, align int) ([]annotationRichParagraph, error) {
	text, err := r.annotationTextString(object)
	if err != nil {
		return nil, err
	}
	style := annotationRichStyle{align: align}
	if defaults != nil {
		value, err := r.ReadAnnotationText(Annotation{Dictionary: Dictionary{"DS": defaults}}, "DS", nil)
		if err != nil {
			return nil, err
		}
		style, err = annotationRichCSS(style, value)
		if err != nil {
			return nil, err
		}
	}
	decoder := xml.NewDecoder(strings.NewReader(text))
	decoder.CharsetReader = func(charset string, input io.Reader) (io.Reader, error) {
		switch strings.ToLower(charset) {
		case "utf-16", "utf-16be", "utf-16le", "us-ascii":
			return input, nil
		default:
			return nil, &UnsupportedError{Feature: "rich text XML encoding " + charset}
		}
	}
	var stack []annotationRichStyle
	var paragraphs []annotationRichParagraph
	paragraph := -1
	root, closed := false, false
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		token, err := decoder.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("invalid rich text XML: %w", err)
		}
		switch token := token.(type) {
		case xml.StartElement:
			if closed {
				return nil, fmt.Errorf("invalid rich text document structure")
			}
			if token.Name.Space != "http://www.w3.org/1999/xhtml" {
				return nil, fmt.Errorf("invalid rich text element namespace")
			}
			current := style
			if len(stack) != 0 {
				current = stack[len(stack)-1]
			}
			parent := current
			if len(stack) != 0 {
				current.decoration = 0
			}
			switch token.Name.Local {
			case "body":
				if root || len(stack) != 0 {
					return nil, fmt.Errorf("invalid rich text root")
				}
				root = true
			case "p":
				if len(stack) != 1 {
					return nil, fmt.Errorf("invalid rich text paragraph nesting")
				}
			case "b":
				current.weight = 700
			case "i":
				italic := true
				current.italic = &italic
			case "span":
			default:
				return nil, &UnsupportedError{Feature: "rich text element " + token.Name.Local}
			}
			if !root {
				return nil, fmt.Errorf("missing rich text body")
			}
			for _, attr := range token.Attr {
				switch {
				case attr.Name.Space == "" && attr.Name.Local == "style":
					current, err = annotationRichCSSStyle(parent, current, attr.Value)
					if err != nil {
						return nil, err
					}
				case attr.Name.Space == "xmlns" || attr.Name.Local == "xmlns", attr.Name.Space == "http://www.xfa.org/schema/xfa-data/1.0" && (attr.Name.Local == "contentType" || attr.Name.Local == "APIVersion" || attr.Name.Local == "spec"):
				default:
					return nil, &UnsupportedError{Feature: "rich text attribute " + attr.Name.Local}
				}
			}
			if current.decoration != 0 {
				current.decorations = &annotationRichDecoration{parent: current.decorations, kind: current.decoration, size: current.size, color: current.color, rise: current.rise}
			}
			if token.Name.Local == "p" {
				paragraphs = append(paragraphs, annotationRichParagraph{style: current})
				paragraph = len(paragraphs) - 1
			}
			stack = append(stack, current)
		case xml.EndElement:
			if len(stack) == 0 {
				return nil, fmt.Errorf("invalid rich text closing element")
			}
			if token.Name.Local == "p" {
				paragraph = -1
			}
			stack = stack[:len(stack)-1]
			closed = len(stack) == 0
		case xml.CharData:
			if len(stack) == 0 {
				if strings.TrimSpace(string(token)) != "" {
					return nil, fmt.Errorf("rich text outside body")
				}
				continue
			}
			if paragraph < 0 {
				if strings.Trim(string(token), " \t\r\n") == "" {
					continue
				}
				paragraphs = append(paragraphs, annotationRichParagraph{style: stack[0]})
				paragraph = len(paragraphs) - 1
			}
			p := &paragraphs[paragraph]
			var normalized strings.Builder
			space := len(p.runs) == 0
			if !space {
				space = strings.HasSuffix(p.runs[len(p.runs)-1].text, " ")
			}
			for _, ch := range string(token) {
				if ch == ' ' || ch == '\t' || ch == '\r' || ch == '\n' {
					if !space {
						normalized.WriteByte(' ')
					}
					space = true
				} else {
					normalized.WriteRune(ch)
					space = false
				}
			}
			if normalized.Len() != 0 {
				p.runs = append(p.runs, annotationRichRun{normalized.String(), stack[len(stack)-1]})
			}
		case xml.Directive:
			return nil, &UnsupportedError{Feature: "rich text XML directive"}
		}
	}
	if !root || !closed || len(stack) != 0 {
		return nil, fmt.Errorf("incomplete rich text body")
	}
	for i := range paragraphs {
		runs := paragraphs[i].runs
		for len(runs) != 0 {
			runs[len(runs)-1].text = strings.TrimRight(runs[len(runs)-1].text, " ")
			if runs[len(runs)-1].text != "" {
				break
			}
			runs = runs[:len(runs)-1]
		}
		paragraphs[i].runs = runs
	}
	return paragraphs, nil
}
