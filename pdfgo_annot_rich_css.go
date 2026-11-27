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
	"strconv"
	"strings"
)

// maxAnnotationRichCSSParts 限制单次富文本CSS属性或列表切分的最大项数
const maxAnnotationRichCSSParts = 4096

// annotationRichCSSStyle 将CSS声明应用到元素样式，inherit取父元素而非前一条声明
// 入参: ctx 取消上下文, parent 父元素样式, style 当前元素样式, text CSS声明
// 返回: annotationRichStyle 当前样式, error 属性、数值、超限或取消错误
func annotationRichCSSStyle(ctx context.Context, parent, style annotationRichStyle, text string) (annotationRichStyle, error) {
	if err := ctx.Err(); err != nil {
		return style, err
	}
	declarations, err := splitAnnotationRichCSS(ctx, text, ';')
	if err != nil {
		return parent, err
	}
	for index, declaration := range declarations {
		if index&63 == 0 {
			if err := ctx.Err(); err != nil {
				return style, err
			}
		}
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
			if err != nil {
				return style, err
			}
			if name == "font-size" && n < 0 {
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
			families, err := annotationRichFamilies(ctx, value)
			if err != nil {
				return style, err
			}
			style.families = families
		case "font":
			var tokens []string
			if err := walkAnnotationRichCSS(ctx, value, ' ', func(part string) error {
				if part != "" {
					tokens = append(tokens, part)
				}
				return nil
			}); err != nil {
				return style, err
			}
			italic := false
			style.weight, style.italic = 400, &italic
			style.stretch = 5
			pos := 0
			for pos < len(tokens) {
				lower := strings.ToLower(tokens[pos])
				if lower == "italic" {
					italic = true
					pos++
					continue
				}
				if lower == "oblique" {
					return style, &UnsupportedError{Feature: "rich text font style " + lower}
				}
				if lower == "normal" {
					pos++
					continue
				}
				if weight, werr := annotationRichWeight(lower); werr == nil {
					style.weight = weight
					pos++
					continue
				}
				break
			}
			rest := tokens[pos:]
			if len(rest) < 2 {
				return style, fmt.Errorf("invalid rich text font shorthand")
			}
			firstSize, firstErr := annotationRichPoint(rest[0])
			lastSize, lastErr := annotationRichPoint(rest[len(rest)-1])
			if (firstErr == nil) == (lastErr == nil) {
				return style, fmt.Errorf("invalid rich text font shorthand")
			}
			size, family := firstSize, strings.Join(rest[1:], " ")
			if firstErr != nil {
				size, family = lastSize, strings.Join(rest[:len(rest)-1], " ")
			}
			if size < 0 {
				return style, fmt.Errorf("invalid rich text font-size")
			}
			if strings.EqualFold(family, "inherit") {
				return style, fmt.Errorf("invalid rich text font shorthand")
			}
			families, err := annotationRichFamilies(ctx, family)
			if err != nil {
				return style, err
			}
			style.size, style.families = &size, families
		case "color":
			color, err := annotationRichColor(value)
			if err != nil {
				return style, err
			}
			style.color = &color
		case "text-decoration":
			style.decoration = 0
			var fields []string
			for i := 0; i < len(value); {
				for i < len(value) && (value[i] == ' ' || value[i] == '\t' || value[i] == '\r' || value[i] == '\n') {
					if i&255 == 0 {
						if err := ctx.Err(); err != nil {
							return style, err
						}
					}
					i++
				}
				if i == len(value) {
					break
				}
				start := i
				for i < len(value) && value[i] != ' ' && value[i] != '\t' && value[i] != '\r' && value[i] != '\n' {
					if i&255 == 0 {
						if err := ctx.Err(); err != nil {
							return style, err
						}
					}
					i++
				}
				if len(fields) >= maxAnnotationRichCSSParts {
					return style, fmt.Errorf("%w: rich text decoration part limit exceeded", ErrStreamLimitExceeded)
				}
				fields = append(fields, strings.ToLower(value[start:i]))
			}
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
	return style, ctx.Err()
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

// walkAnnotationRichCSS 按引号和括号边界遍历CSS属性或列表片段并限制切分项数量
// 入参: ctx 取消上下文, text CSS内容, separator 分隔符, fn 片段回调
// 返回: error 未闭合的字符串或括号、超限、取消或回调错误
func walkAnnotationRichCSS(ctx context.Context, text string, separator byte, fn func(string) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	start, depth, quote, parts := 0, 0, byte(0), 0
	for i := 0; i < len(text); i++ {
		if i&255 == 0 {
			if err := ctx.Err(); err != nil {
				return err
			}
		}
		ch := text[i]
		if ch == '\\' {
			if i+1 == len(text) {
				return fmt.Errorf("incomplete rich text CSS escape")
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
				return fmt.Errorf("invalid rich text CSS parentheses")
			}
		}
		if depth == 0 && (ch == separator || separator == ' ' && (ch == '\t' || ch == '\r' || ch == '\n')) {
			parts++
			if parts >= maxAnnotationRichCSSParts {
				return fmt.Errorf("%w: rich text CSS part limit exceeded", ErrStreamLimitExceeded)
			}
			if err := fn(text[start:i]); err != nil {
				return err
			}
			start = i + 1
		}
	}
	if quote != 0 || depth != 0 {
		return fmt.Errorf("incomplete rich text CSS value")
	}
	if err := fn(text[start:]); err != nil {
		return err
	}
	return ctx.Err()
}

// splitAnnotationRichCSS 按引号和括号边界拆分CSS属性或列表并限制切分项数量
// 入参: ctx 取消上下文, text CSS内容, separator 分隔符
// 返回: []string 内容片段, error 未闭合的字符串或括号、超限或取消错误
func splitAnnotationRichCSS(ctx context.Context, text string, separator byte) ([]string, error) {
	var fields []string
	err := walkAnnotationRichCSS(ctx, text, separator, func(part string) error {
		fields = append(fields, part)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return fields, nil
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

// maxAnnotationRichFamilies 限制富文本单条样式允许声明的唯一字体家族数量上限
const maxAnnotationRichFamilies = 64

// annotationRichFamilies 解析有序字体名称列表，保留名称中的空格并去重限制数量
// 入参: ctx 取消上下文, value 字体列表
// 返回: []annotationRichFamilyName 字体名称, error 空名称、超限、引号或取消错误
func annotationRichFamilies(ctx context.Context, value string) ([]annotationRichFamilyName, error) {
	families := make([]annotationRichFamilyName, 0, 8)
	seen := make(map[string]bool, 8)
	err := walkAnnotationRichCSS(ctx, value, ',', func(field string) error {
		field = strings.TrimSpace(field)
		quoted := false
		if len(field) >= 2 && (field[0] == '\'' || field[0] == '"') && field[len(field)-1] == field[0] {
			quoted = true
			field = field[1 : len(field)-1]
		}
		if field == "" || strings.ContainsAny(field, "\\") {
			return &UnsupportedError{Feature: "rich text font family " + field}
		}
		generic := false
		if !quoted {
			switch strings.ToLower(field) {
			case "serif", "sans-serif", "monospace", "cursive", "fantasy":
				generic = true
			}
			field = strings.Join(strings.Fields(field), " ")
		}
		item := annotationRichFamilyName{field, generic}
		key := strings.ToLower(annotationRichFamily(item))
		if seen[key] {
			return nil
		}
		if len(families) >= maxAnnotationRichFamilies {
			return fmt.Errorf("%w: rich text font family limit exceeded", ErrStreamLimitExceeded)
		}
		seen[key] = true
		families = append(families, item)
		return nil
	})
	if err != nil {
		return nil, err
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
	values := strings.SplitN(value[4:len(value)-1], ",", 4)
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
