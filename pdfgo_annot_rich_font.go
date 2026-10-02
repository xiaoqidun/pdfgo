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
	"slices"
	"strings"
)

// annotationRichFont 保存外观内的字体资源、家族属性及所需字符映射
type annotationRichFont struct {
	name    Name
	font    *Font
	family  string
	weight  int
	stretch int
	italic  bool
	letters map[rune]annotationLetter
}

// annotationRichFonts 在单次外观生成中选择和复用字体，不修改源资源
type annotationRichFonts struct {
	reader  *Reader
	ctx     context.Context
	fonts   Dictionary
	entries []*annotationRichFont
	wanted  map[rune]bool
	base    *annotationRichFont
}

// newAnnotationRichFonts 建立外观独立的字体字典及字符映射
// 入参: ctx 取消上下文, resources 外观资源, base 默认字体, name 默认字体名, paragraphs 富文本
// 返回: *annotationRichFonts 字体选择器, error 字体或资源错误
func (r *Reader) newAnnotationRichFonts(ctx context.Context, resources Dictionary, base *Font, name Name, paragraphs []annotationRichParagraph) (*annotationRichFonts, error) {
	value, err := r.Resolve(resources["Font"])
	if err != nil {
		return nil, err
	}
	fonts, ok := value.(Dictionary)
	if !ok {
		return nil, fmt.Errorf("invalid rich text font resources")
	}
	s := &annotationRichFonts{reader: r, ctx: ctx, fonts: maps.Clone(fonts), wanted: map[rune]bool{}}
	for _, paragraph := range paragraphs {
		for _, run := range paragraph.runs {
			if run.style.size != nil && *run.style.size == 0 {
				continue
			}
			for _, ch := range run.text {
				s.wanted[ch] = true
			}
		}
	}
	s.wanted[' '] = true
	s.base, err = s.add(name, base)
	if err != nil {
		return nil, err
	}
	families := map[string]bool{}
	for _, paragraph := range paragraphs {
		for _, run := range paragraph.runs {
			if run.style.size != nil && *run.style.size == 0 {
				continue
			}
			for _, family := range run.style.families {
				families[strings.ToLower(annotationRichFamily(family))] = true
			}
			if len(run.style.families) == 0 && (run.style.weight != 0 || run.style.italic != nil || run.style.stretch != 0) {
				families[strings.ToLower(s.base.family)] = true
			}
		}
	}
	if len(families) == 0 {
		resources["Font"] = s.fonts
		return s, nil
	}
	names := make([]Name, 0, len(fonts))
	for name := range fonts {
		names = append(names, name)
	}
	slices.Sort(names)
	for _, other := range names {
		if other == name {
			continue
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		family, err := r.annotationRichResourceFamily(fonts[other])
		if err != nil || !families[strings.ToLower(family)] {
			continue
		}
		font, err := r.ReadFont(fonts[other])
		if err != nil {
			return nil, err
		}
		if font.Vertical {
			continue
		}
		if _, err := s.add(other, font); err != nil {
			return nil, err
		}
	}
	resources["Font"] = s.fonts
	return s, nil
}

// annotationRichResourceFamily 读取字体家族元信息，不解码无关字体程序
// 入参: object 字体资源
// 返回: string 家族, error 字典或引用错误
func (r *Reader) annotationRichResourceFamily(object Object) (string, error) {
	value, err := r.Resolve(object)
	if err != nil {
		return "", err
	}
	dict, ok := value.(Dictionary)
	if !ok {
		return "", fmt.Errorf("invalid rich text font dictionary")
	}
	name, _ := dict["BaseFont"].(Name)
	family, _, _ := annotationRichStandardFace(string(name))
	if family == "" {
		family = string(name)
		if len(family) > 7 && family[6] == '+' && strings.Trim(family[:6], "ABCDEFGHIJKLMNOPQRSTUVWXYZ") == "" {
			family = family[7:]
		}
	}
	if dict["Subtype"] == Name("Type0") {
		value, err = r.Resolve(dict["DescendantFonts"])
		if err != nil {
			return "", err
		}
		children, ok := value.(Array)
		if !ok || len(children) != 1 {
			return "", fmt.Errorf("invalid rich text descendant fonts")
		}
		value, err = r.Resolve(children[0])
		if err != nil {
			return "", err
		}
		dict, ok = value.(Dictionary)
		if !ok {
			return "", fmt.Errorf("invalid rich text descendant font")
		}
	}
	value, err = r.Resolve(dict["FontDescriptor"])
	if err != nil {
		return "", err
	}
	if descriptor, ok := value.(Dictionary); ok && descriptor["FontFamily"] != nil {
		return r.ReadAnnotationText(Annotation{Dictionary: descriptor}, "FontFamily", nil)
	}
	return family, nil
}

// add 注册可复用字体并按描述符读取家族、字重、宽度和斜体属性
// 入参: name 外观资源名, font 字体
// 返回: *annotationRichFont 字体信息, error 描述符或字符映射错误
func (s *annotationRichFonts) add(name Name, font *Font) (*annotationRichFont, error) {
	family, weight, italic := annotationRichStandardFace(font.Name)
	stretch := 5
	if family == "" {
		family, weight = font.Name, 400
		if len(family) > 7 && family[6] == '+' && strings.Trim(family[:6], "ABCDEFGHIJKLMNOPQRSTUVWXYZ") == "" {
			family = family[7:]
		}
	}
	dict := font.Dictionary
	if font.composite {
		value, err := s.reader.Resolve(dict["DescendantFonts"])
		if err != nil {
			return nil, err
		}
		value, err = s.reader.Resolve(value.(Array)[0])
		if err != nil {
			return nil, err
		}
		dict = value.(Dictionary)
	}
	value, err := s.reader.Resolve(dict["FontDescriptor"])
	if err != nil {
		return nil, err
	}
	if descriptor, ok := value.(Dictionary); ok {
		value, err := s.reader.Resolve(descriptor["FontStretch"])
		if err != nil {
			return nil, err
		}
		if value != nil {
			switch value {
			case Name("UltraCondensed"):
				stretch = 1
			case Name("ExtraCondensed"):
				stretch = 2
			case Name("Condensed"):
				stretch = 3
			case Name("SemiCondensed"):
				stretch = 4
			case Name("Normal"):
				stretch = 5
			case Name("SemiExpanded"):
				stretch = 6
			case Name("Expanded"):
				stretch = 7
			case Name("ExtraExpanded"):
				stretch = 8
			case Name("UltraExpanded"):
				stretch = 9
			default:
				return nil, fmt.Errorf("invalid rich text font descriptor stretch")
			}
		}
		if descriptor["FontFamily"] != nil {
			family, err = s.reader.ReadAnnotationText(Annotation{Dictionary: descriptor}, "FontFamily", nil)
			if err != nil {
				return nil, err
			}
		}
		if descriptor["FontWeight"] != nil {
			n, err := s.reader.number(descriptor["FontWeight"])
			if err != nil || n < 100 || n > 900 {
				return nil, fmt.Errorf("invalid rich text font descriptor weight")
			}
			weight = int(n)
		}
		if descriptor["Flags"] != nil {
			value, err := s.reader.Resolve(descriptor["Flags"])
			if err != nil {
				return nil, err
			}
			flags, ok := value.(Integer)
			if !ok {
				return nil, fmt.Errorf("invalid rich text font descriptor flags")
			}
			italic = flags&(1<<6) != 0
		}
	}
	letters, err := annotationLetterLookup(s.ctx, font, s.wanted)
	if err != nil {
		return nil, err
	}
	entry := &annotationRichFont{name: name, font: font, family: family, weight: weight, stretch: stretch, italic: italic, letters: letters}
	s.entries = append(s.entries, entry)
	return entry, nil
}

// selectLetter 按字体列表、样式及字形覆盖选择原始字符编码
// 入参: style 文字样式, ch 字符
// 返回: *annotationRichFont 字体, annotationLetter 编码, error 未覆盖的字体或字形
func (s *annotationRichFonts) selectLetter(style annotationRichStyle, ch rune) (*annotationRichFont, annotationLetter, error) {
	families := style.families
	weight, italic, stretch := style.weight, s.base.italic, style.stretch
	if weight == 0 {
		weight = s.base.weight
	}
	if style.italic != nil {
		italic = *style.italic
	}
	if stretch == 0 {
		stretch = 5
	}
	if len(families) == 0 {
		if style.weight == 0 && style.italic == nil && style.stretch == 0 {
			if letter, ok := s.base.letters[ch]; ok {
				return s.base, letter, nil
			}
			return nil, annotationLetter{}, &UnsupportedError{Feature: fmt.Sprintf("field font has no encoding for U+%04X", ch)}
		}
		families = []annotationRichFamilyName{{name: s.base.family}}
	}
	for _, requested := range families {
		family := annotationRichFamily(requested)
		if standard := annotationRichStandardName(family, weight, italic); standard != "" {
			exists := false
			for _, entry := range s.entries {
				exists = exists || entry.font.Name == standard && strings.EqualFold(entry.family, family) && entry.italic == italic
			}
			if !exists {
				object := Dictionary{"Type": Name("Font"), "Subtype": Name("Type1"), "BaseFont": Name(standard), "Encoding": Name("WinAnsiEncoding")}
				font, err := s.reader.ReadFont(object)
				if err != nil {
					return nil, annotationLetter{}, err
				}
				name := Name(fmt.Sprintf("RichFont%d", len(s.fonts)))
				for s.fonts[name] != nil {
					name += "_"
				}
				if _, err := s.add(name, font); err != nil {
					return nil, annotationLetter{}, err
				}
				s.fonts[name] = object
			}
		}
		var best *annotationRichFont
		bestWidth, bestWeight := 0, 0
		for _, entry := range s.entries {
			if !strings.EqualFold(family, entry.family) || entry.italic != italic {
				continue
			}
			widthRank := annotationRichStretchRank(stretch, entry.stretch)
			weightRank := annotationRichWeightRank(weight, entry.weight)
			if best == nil || widthRank < bestWidth || widthRank == bestWidth && weightRank < bestWeight {
				best, bestWidth, bestWeight = entry, widthRank, weightRank
			} else if widthRank == bestWidth && weightRank == bestWeight {
				if _, ok := best.letters[ch]; !ok {
					best = entry
				}
			}
		}
		if best != nil {
			if letter, ok := best.letters[ch]; ok {
				return best, letter, nil
			}
		}
	}
	return nil, annotationLetter{}, &UnsupportedError{Feature: fmt.Sprintf("rich text fonts have no matching face and encoding for U+%04X", ch)}
}

// annotationRichStretchRank 为缺失宽度选择替代字体，窄类先向窄侧、宽类先向宽侧查找
// 入参: wanted 请求宽度, actual 可用宽度
// 返回: int 匹配优先级，数值越小越优先
func annotationRichStretchRank(wanted, actual int) int {
	if actual == 0 {
		actual = 5
	}
	if wanted <= 5 {
		if actual <= wanted {
			return wanted - actual
		}
		return actual + 9
	}
	if actual >= wanted {
		return actual - wanted
	}
	return 18 - actual
}

// annotationRichWeightRank 按CSS2字重替代顺序计算匹配优先级
// 入参: wanted 请求字重, actual 可用字重
// 返回: int 优先级，数值越小越优先
func annotationRichWeightRank(wanted, actual int) int {
	if wanted == actual {
		return 0
	}
	if wanted == 400 && actual == 500 || wanted == 500 && actual == 400 {
		return 1
	}
	if wanted <= 500 {
		if actual < wanted {
			return wanted - actual + 2
		}
		return actual + 1000
	}
	if actual > wanted {
		return actual - wanted + 2
	}
	return 1000 - actual + 1000
}

// annotationRichFamily 将通用字体族映射到当前可生成的标准字体族
// 入参: family CSS字体族
// 返回: string 字体族
func annotationRichFamily(family annotationRichFamilyName) string {
	if !family.generic {
		return family.name
	}
	switch strings.ToLower(family.name) {
	case "serif":
		return "Times"
	case "sans-serif":
		return "Helvetica"
	case "monospace":
		return "Courier"
	}
	return family.name
}

// annotationRichStandardFace 读取标准字体名称明确指定的家族和样式
// 入参: name 字体名称
// 返回: string 家族, int 字重, bool 斜体
func annotationRichStandardFace(name string) (string, int, bool) {
	switch name {
	case "Helvetica", "Helvetica-Bold", "Helvetica-Oblique", "Helvetica-BoldOblique":
		return "Helvetica", annotationRichFaceWeight(name), strings.Contains(name, "Oblique")
	case "Courier", "Courier-Bold", "Courier-Oblique", "Courier-BoldOblique":
		return "Courier", annotationRichFaceWeight(name), strings.Contains(name, "Oblique")
	case "Times-Roman", "Times-Bold", "Times-Italic", "Times-BoldItalic":
		return "Times", annotationRichFaceWeight(name), strings.Contains(name, "Italic")
	}
	return "", 0, false
}

// annotationRichFaceWeight 读取标准字体名称的常规或粗体字重
// 入参: name 标准字体名
// 返回: int 字重
func annotationRichFaceWeight(name string) int {
	if strings.Contains(name, "Bold") {
		return 700
	}
	return 400
}

// annotationRichStandardName 为标准家族选择最接近请求样式的字体
// 入参: family 家族, weight 字重, italic 斜体
// 返回: string 标准字体名，非标准家族为空
func annotationRichStandardName(family string, weight int, italic bool) string {
	var regular, bold, oblique, both string
	switch {
	case strings.EqualFold(family, "Helvetica"):
		regular, bold, oblique, both = "Helvetica", "Helvetica-Bold", "Helvetica-Oblique", "Helvetica-BoldOblique"
	case strings.EqualFold(family, "Courier"):
		regular, bold, oblique, both = "Courier", "Courier-Bold", "Courier-Oblique", "Courier-BoldOblique"
	case strings.EqualFold(family, "Times"):
		regular, bold, oblique, both = "Times-Roman", "Times-Bold", "Times-Italic", "Times-BoldItalic"
	default:
		return ""
	}
	useBold := annotationRichWeightRank(weight, 700) < annotationRichWeightRank(weight, 400)
	if italic && useBold {
		return both
	}
	if italic {
		return oblique
	}
	if useBold {
		return bold
	}
	return regular
}
