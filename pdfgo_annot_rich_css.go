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

// annotationRichCSS 解析标准富文本内联样式，未覆盖的属性不静默丢弃
// 入参: parent 继承样式, text CSS声明
// 返回: annotationRichStyle 当前样式, error 属性或数值错误
func annotationRichCSS(parent annotationRichStyle, text string) (annotationRichStyle, error) {
	return annotationRichCSSStyle(parent, parent, text)
}

// annotationRichCSSStyle 将CSS声明应用到元素样式，inherit取父元素而非前一条声明
// 入参: parent 父元素样式, style 当前元素样式, text CSS声明
// 返回: annotationRichStyle 当前样式, error 属性或数值错误
func annotationRichCSSStyle(parent, style annotationRichStyle, text string) (annotationRichStyle, error) {
	declarations, err := splitAnnotationRichCSS(text, ';')
	if err != nil {
		return parent, err
	}
	for _, declaration := range declarations {
		if strings.TrimSpace(declaration) == "" {
			continue
		}
		name, value, ok := strings.Cut(declaration, ":")
		if !ok {
			return style, fmt.Errorf("invalid rich text style declaration")
		}
		name, value = strings.ToLower(strings.TrimSpace(name)), strings.TrimSpace(value)
		if strings.EqualFold(value, "inherit") {
			switch name {
			case "font":
				style.families, style.size, style.weight, style.italic = parent.families, parent.size, parent.weight, parent.italic
				style.stretch = parent.stretch
			case "font-family":
				style.families = parent.families
			case "font-size":
				style.size = parent.size
			case "font-weight":
				style.weight = parent.weight
			case "font-style":
				style.italic = parent.italic
			case "font-stretch":
				style.stretch = parent.stretch
			case "color":
				style.color = parent.color
			case "text-align":
				style.align = parent.align
			case "vertical-align":
				style.rise = parent.rise
			case "text-decoration":
				style.decoration = parent.decoration
			default:
				return style, &UnsupportedError{Feature: "rich text style " + name}
			}
			continue
		}
		switch name {
		case "text-align":
			switch strings.ToLower(value) {
			case "left":
				style.align = 0
			case "center":
				style.align = 1
			case "right":
				style.align = 2
			default:
				return style, &UnsupportedError{Feature: "rich text alignment " + value}
			}
		case "font-size", "vertical-align":
			n, err := annotationRichPoint(value)
			if err != nil || name == "font-size" && n < 0 {
				return style, fmt.Errorf("invalid rich text %s", name)
			}
			if name == "font-size" {
				style.size = &n
			} else {
				style.rise = parent.rise + n
				if math.IsInf(style.rise, 0) {
					return style, fmt.Errorf("rich text baseline overflow")
				}
			}
		case "font-style":
			value = strings.ToLower(value)
			if value != "normal" && value != "italic" {
				return style, &UnsupportedError{Feature: "rich text font style " + value}
			}
			italic := value == "italic"
			style.italic = &italic
		case "font-weight":
			weight, err := annotationRichWeight(value)
			if err != nil {
				return style, err
			}
			style.weight = weight
		case "font-stretch":
			stretch := annotationRichStretch(strings.ToLower(value))
			if stretch == 0 {
				return style, &UnsupportedError{Feature: "rich text font stretch " + value}
			}
			style.stretch = stretch
		case "font-family":
			families, err := annotationRichFamilies(value)
			if err != nil {
				return style, err
			}
			style.families = families
		case "font":
			fields, err := splitAnnotationRichCSS(value, ' ')
			if err != nil {
				return style, err
			}
			italic := false
			style.weight, style.italic = 400, &italic
			style.stretch = 5
			found := false
			for i, field := range fields {
				if field == "" {
					continue
				}
				field = strings.ToLower(field)
				if strings.HasSuffix(field, "pt") || field == "0" {
					style, err = annotationRichCSS(style, "font-size:"+field+";font-family:"+strings.Join(fields[i+1:], " "))
					found = true
					break
				}
				if field == "italic" {
					italic := true
					style.italic = &italic
				} else if field != "normal" {
					style.weight, err = annotationRichWeight(field)
					if err != nil {
						return style, err
					}
				}
			}
			if err != nil || !found {
				return style, fmt.Errorf("invalid rich text font shorthand")
			}
		case "color":
			color, err := annotationRichColor(value)
			if err != nil {
				return style, err
			}
			style.color = &color
		case "text-decoration":
			style.decoration = 0
			fields := strings.Fields(strings.ToLower(value))
			if len(fields) == 0 {
				return style, fmt.Errorf("invalid rich text decoration")
			}
			for _, field := range fields {
				var kind uint8
				switch field {
				case "none":
					if len(fields) != 1 {
						return style, fmt.Errorf("invalid rich text decoration combination")
					}
				case "underline":
					kind = annotationRichUnderline
				case "line-through":
					kind = annotationRichStrike
				default:
					return style, &UnsupportedError{Feature: "rich text decoration " + field}
				}
				if style.decoration&kind != 0 {
					return style, fmt.Errorf("duplicate rich text decoration")
				}
				style.decoration |= kind
			}
		default:
			return style, &UnsupportedError{Feature: "rich text style " + name}
		}
	}
	return style, nil
}

// annotationRichStretch 读取PDF富文本允许的九种CSS字体宽度
// 入参: value 小写宽度关键字
// 返回: int 从窄到宽的类别，无效值为零
func annotationRichStretch(value string) int {
	switch value {
	case "ultra-condensed":
		return 1
	case "extra-condensed":
		return 2
	case "condensed":
		return 3
	case "semi-condensed":
		return 4
	case "normal":
		return 5
	case "semi-expanded":
		return 6
	case "expanded":
		return 7
	case "extra-expanded":
		return 8
	case "ultra-expanded":
		return 9
	}
	return 0
}

// splitAnnotationRichCSS 按引号和括号边界拆分CSS属性或列表
// 入参: text CSS内容, separator 分隔符
// 返回: []string 内容片段, error 未闭合的字符串或括号
func splitAnnotationRichCSS(text string, separator byte) ([]string, error) {
	var fields []string
	start, depth, quote := 0, 0, byte(0)
	for i := 0; i < len(text); i++ {
		ch := text[i]
		if ch == '\\' {
			if i+1 == len(text) {
				return nil, fmt.Errorf("incomplete rich text CSS escape")
			}
			i++
			continue
		}
		if quote != 0 {
			if ch == quote {
				quote = 0
			}
			continue
		}
		switch ch {
		case '\'', '"':
			quote = ch
		case '(':
			depth++
		case ')':
			depth--
			if depth < 0 {
				return nil, fmt.Errorf("invalid rich text CSS parentheses")
			}
		}
		if depth == 0 && (ch == separator || separator == ' ' && (ch == '\t' || ch == '\r' || ch == '\n')) {
			fields = append(fields, text[start:i])
			start = i + 1
		}
	}
	if quote != 0 || depth != 0 {
		return nil, fmt.Errorf("incomplete rich text CSS value")
	}
	return append(fields, text[start:]), nil
}

// annotationRichPoint 解析富文本的点单位数值，零允许省略单位
// 入参: value CSS数值
// 返回: float64 点数, error 单位或数值错误
func annotationRichPoint(value string) (float64, error) {
	value = strings.ToLower(value)
	if value != "0" && !strings.HasSuffix(value, "pt") {
		return 0, &UnsupportedError{Feature: "rich text length " + value}
	}
	text := strings.TrimSuffix(value, "pt")
	if strings.ContainsAny(text, "eExXpP") {
		return 0, fmt.Errorf("invalid rich text decimal")
	}
	n, err := strconv.ParseFloat(text, 64)
	if err != nil || math.IsNaN(n) || math.IsInf(n, 0) {
		return 0, fmt.Errorf("invalid rich text length")
	}
	return n, nil
}

// annotationRichWeight 解析CSS字重的标准关键字或百位数值
// 入参: value 字重
// 返回: int 字重, error 属性错误
func annotationRichWeight(value string) (int, error) {
	value = strings.ToLower(value)
	switch value {
	case "normal":
		return 400, nil
	case "bold":
		return 700, nil
	}
	n, err := strconv.Atoi(value)
	if err != nil || n < 100 || n > 900 || n%100 != 0 {
		return 0, fmt.Errorf("invalid rich text font weight")
	}
	return n, nil
}

// annotationRichFamilies 解析有序字体名称列表，保留名称中的空格
// 入参: value 字体列表
// 返回: []annotationRichFamilyName 字体名称, error 空名称或引号错误
func annotationRichFamilies(value string) ([]annotationRichFamilyName, error) {
	fields, err := splitAnnotationRichCSS(value, ',')
	if err != nil {
		return nil, err
	}
	families := make([]annotationRichFamilyName, 0, len(fields))
	for _, field := range fields {
		field = strings.TrimSpace(field)
		quoted := false
		if len(field) >= 2 && (field[0] == '\'' || field[0] == '"') && field[len(field)-1] == field[0] {
			quoted = true
			field = field[1 : len(field)-1]
		}
		if field == "" || strings.ContainsAny(field, "\\") {
			return nil, &UnsupportedError{Feature: "rich text font family " + field}
		}
		generic := false
		if !quoted {
			switch strings.ToLower(field) {
			case "serif", "sans-serif", "monospace", "cursive", "fantasy":
				generic = true
			}
			field = strings.Join(strings.Fields(field), " ")
		}
		families = append(families, annotationRichFamilyName{field, generic})
	}
	return families, nil
}

// annotationRichColor 解析标准十六进制或RGB十进制颜色，限制到sRGB分量范围
// 入参: value CSS颜色
// 返回: [3]float64 颜色分量, error 颜色错误
func annotationRichColor(value string) ([3]float64, error) {
	value = strings.ToLower(value)
	var color [3]float64
	if len(value) == 7 && value[0] == '#' {
		for i := range color {
			n, err := strconv.ParseUint(value[1+2*i:3+2*i], 16, 8)
			if err != nil {
				return color, fmt.Errorf("invalid rich text hex color")
			}
			color[i] = float64(n) / 255
		}
		return color, nil
	}
	if !strings.HasPrefix(value, "rgb(") || !strings.HasSuffix(value, ")") {
		return color, &UnsupportedError{Feature: "rich text color " + value}
	}
	values := strings.Split(value[4:len(value)-1], ",")
	if len(values) != 3 {
		return color, fmt.Errorf("invalid rich text RGB color")
	}
	for i, component := range values {
		if strings.ContainsAny(component, "eExXpP") {
			return color, fmt.Errorf("invalid rich text RGB decimal")
		}
		n, err := strconv.ParseFloat(strings.TrimSpace(component), 64)
		if err != nil || math.IsNaN(n) || math.IsInf(n, 0) {
			return color, fmt.Errorf("invalid rich text RGB component")
		}
		color[i] = min(1, max(0, n/255))
	}
	return color, nil
}
