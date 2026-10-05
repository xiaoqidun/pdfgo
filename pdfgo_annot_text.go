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
	"slices"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

// annotationLetter 保存外观文字的原始编码与排版度量
type annotationLetter struct {
	text  rune
	code  string
	width float64
	word  bool
}

// ReadAnnotationText 读取注解文本字段，RC和RV允许文本流，其他字段要求文本字符串
// 入参: annotation 注解, field 文本字段名, warning 异常字段警告，空值使用严格检查
// 返回: string Unicode文本, error 引用、类型或编码错误
func (r *Reader) ReadAnnotationText(annotation Annotation, field Name, warning func(Diagnostic)) (string, error) {
	value, err := r.Resolve(annotation.Dictionary[field])
	if err != nil || value == nil {
		return "", err
	}
	var data String
	switch value := value.(type) {
	case String:
		data = value
	case *Stream:
		if field != "RC" && field != "RV" {
			err = fmt.Errorf("invalid annotation %s: expected text string", field)
			break
		}
		var decoded []byte
		decoded, err = value.Decode()
		data = String(decoded)
	default:
		err = fmt.Errorf("invalid annotation %s: expected text string", field)
	}
	var text string
	if err == nil {
		text, err = DecodeTextString(data)
	}
	if err != nil {
		if warning == nil {
			return "", err
		}
		warning(Diagnostic{Message: fmt.Sprintf("invalid PDF annotation %s ignored; appearance retained", field)})
		return "", nil
	}
	return text, nil
}

// formDefaults 读取表单默认属性，不要求文档必须包含交互表单
// 返回: Dictionary 默认属性, error 目录或表单错误
func (r *Reader) formDefaults() (Dictionary, error) {
	if r.Trailer["Root"] == nil {
		return nil, nil
	}
	catalog, err := r.catalogDictionary()
	if err != nil {
		return nil, err
	}
	value, err := r.Resolve(catalog["AcroForm"])
	if err != nil || value == nil {
		return nil, err
	}
	form, ok := value.(Dictionary)
	if !ok {
		return nil, fmt.Errorf("invalid interactive form dictionary")
	}
	return form, nil
}

// annotationTextString 读取文本字符串或文本流，并统一换行符
// 入参: object 文本对象
// 返回: string Unicode文本, error 编码或类型错误
func (r *Reader) annotationTextString(object Object) (string, error) {
	value, err := r.Resolve(object)
	if err != nil || value == nil {
		return "", err
	}
	var data []byte
	switch value := value.(type) {
	case String:
		data = value
	case *Stream:
		data, err = value.Decode()
		if err != nil {
			return "", err
		}
	default:
		return "", fmt.Errorf("invalid annotation text string")
	}
	text, err := DecodeTextString(data)
	if err != nil {
		return "", err
	}
	return strings.ReplaceAll(strings.ReplaceAll(text, "\r\n", "\n"), "\r", "\n"), nil
}

// widgetAppearance 为缺少外观的控件生成当前值的静态外观
// 入参: ctx 取消上下文, page 页面, annotation 控件
// 返回: *Stream 外观流, error 属性或能力错误
func (r *Reader) widgetAppearance(ctx context.Context, page *Page, annotation Annotation) (*Stream, error) {
	field, err := r.ReadField(annotation)
	if err != nil {
		return nil, err
	}
	annotation.Dictionary = field
	return r.variableTextAppearance(ctx, page, annotation)
}

// variableTextAppearance 生成普通文本和按钮外观，保留字体编码和绘图状态
// 入参: ctx 取消上下文, page 页面, annotation 注解或控件
// 返回: *Stream 外观流, error 不支持的布局或资源错误
func (r *Reader) variableTextAppearance(ctx context.Context, page *Page, annotation Annotation) (*Stream, error) {
	if annotation.Subtype == "FreeText" {
		return r.freeTextAppearance(ctx, page, annotation)
	}
	dict := annotation.Dictionary
	flags := Integer(0)
	if value, err := r.Resolve(dict["Ff"]); err != nil {
		return nil, err
	} else if value != nil {
		var ok bool
		flags, ok = value.(Integer)
		if !ok || flags < 0 {
			return nil, fmt.Errorf("invalid field flags")
		}
	}
	value, err := r.Resolve(dict["MK"])
	if err != nil {
		return nil, err
	}
	mk, ok := value.(Dictionary)
	if value != nil && !ok {
		return nil, fmt.Errorf("invalid widget appearance characteristics")
	}
	w, h := annotation.Rect.XMax-annotation.Rect.XMin, annotation.Rect.YMax-annotation.Rect.YMin
	matrix := Identity()
	if mk["R"] != nil {
		n, err := r.number(mk["R"])
		if err != nil || math.Mod(n, 90) != 0 {
			return nil, fmt.Errorf("invalid widget rotation")
		}
		switch math.Mod(math.Mod(n, 360)+360, 360) {
		case 90:
			matrix, w, h = Matrix{0, 1, -1, 0, w, 0}, h, w
		case 180:
			matrix = Matrix{-1, 0, 0, -1, w, h}
		case 270:
			matrix, w, h = Matrix{0, -1, 1, 0, 0, h}, h, w
		}
	}
	if w <= 0 || h <= 0 {
		return nil, fmt.Errorf("invalid variable text bounds")
	}
	frame := Rectangle{0, 0, w, h}
	fw, fh := frame.XMax-frame.XMin, frame.YMax-frame.YMin
	borderAnnotation := annotation
	borderAnnotation.Dictionary = maps.Clone(dict)
	background := mk["BG"]
	borderAnnotation.Dictionary["C"] = mk["BC"]
	if mk["BC"] == nil {
		borderAnnotation.Dictionary["C"] = Array{}
	}
	border, err := r.readAnnotationBorder(borderAnnotation)
	if err != nil {
		return nil, err
	}
	var content strings.Builder
	if background != nil {
		value, err := r.Resolve(background)
		if err != nil {
			return nil, err
		}
		color, ok := value.(Array)
		if !ok {
			return nil, fmt.Errorf("invalid annotation background color")
		}
		fill, err := r.writeAnnotationColor(&content, color, false)
		if err != nil {
			return nil, err
		}
		if fill {
			fmt.Fprintf(&content, "%g %g %g %g re f\n", frame.XMin, frame.YMin, fw, fh)
		}
	}
	stroke, err := r.writeAnnotationStroke(&content, border)
	if err != nil {
		return nil, err
	}
	if stroke && !(dict["FT"] == Name("Btn") && flags&(1<<15) != 0 && flags&(1<<16) == 0) {
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
			return nil, &UnsupportedError{Feature: "generated widget border style " + string(border.style)}
		}
	}
	resources := Dictionary{}
	defaults, err := r.formDefaults()
	if err != nil {
		return nil, err
	}
	if value, err := r.Resolve(defaults["DR"]); err != nil {
		return nil, err
	} else if value != nil {
		var ok bool
		resources, ok = value.(Dictionary)
		if !ok {
			return nil, fmt.Errorf("invalid form default resources")
		}
	}
	resources = maps.Clone(resources)
	stream := &Stream{Dictionary: Dictionary{"Type": Name("XObject"), "Subtype": Name("Form"), "BBox": Array{Integer(0), Integer(0), Real(w), Real(h)}, "Matrix": Array{Real(matrix[0]), Real(matrix[1]), Real(matrix[2]), Real(matrix[3]), Real(matrix[4]), Real(matrix[5])}, "Resources": resources}, reader: r}
	if dict["FT"] == Name("Btn") && flags&(1<<16) == 0 {
		if err := r.writeAnnotationToggleButton(ctx, annotation, mk, flags, frame, border, resources, &content, stroke); err != nil {
			return nil, err
		}
		stream.Data = []byte(content.String())
		return stream, nil
	}
	if dict["FT"] == Name("Btn") {
		err = r.writeAnnotationPushButton(ctx, annotation, mk, frame, border, resources, &content)
	} else {
		err = r.writeAnnotationText(ctx, annotation, mk, flags, frame, border, resources, &content)
	}
	if err != nil {
		return nil, err
	}
	stream.Data = []byte(content.String())
	return stream, nil
}

// annotationCaptionFont 选择可编码标题的横排字体，不修改源资源
// 入参: ctx 取消上下文, resources 可用资源, text 标题文字
// 返回: *Font 字体, []annotationLetter 编码, Object 字体资源, error 字体或编码错误
func (r *Reader) annotationCaptionFont(ctx context.Context, resources Dictionary, text string) (*Font, []annotationLetter, Object, error) {
	if err := ctx.Err(); err != nil {
		return nil, nil, nil, err
	}
	standard := Dictionary{"Type": Name("Font"), "Subtype": Name("Type1"), "BaseFont": Name("Helvetica"), "Encoding": Name("WinAnsiEncoding")}
	font, err := r.ReadFontContext(ctx, standard)
	if err != nil {
		return nil, nil, nil, err
	}
	letters, encodingErr := annotationLetters(ctx, font, text)
	if encodingErr == nil {
		return font, letters, standard, nil
	}
	if err := ctx.Err(); err != nil {
		return nil, nil, nil, err
	}
	value, err := r.Resolve(resources["Font"])
	if err != nil {
		return nil, nil, nil, err
	}
	fonts, ok := value.(Dictionary)
	if value != nil && !ok {
		return nil, nil, nil, fmt.Errorf("invalid caption font resources")
	}
	names := make([]Name, 0, len(fonts))
	for name := range fonts {
		names = append(names, name)
	}
	slices.Sort(names)
	for _, name := range names {
		if err := ctx.Err(); err != nil {
			return nil, nil, nil, err
		}
		font, err := r.ReadFontContext(ctx, fonts[name])
		if err != nil || font.Vertical {
			continue
		}
		letters, err := annotationLetters(ctx, font, text)
		if err == nil {
			return font, letters, fonts[name], nil
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, nil, nil, err
	}
	return nil, nil, nil, encodingErr
}

// writeAnnotationText 按字段值和默认外观生成裁剪后的文字内容
// 入参: ctx 取消上下文, annotation 注解, mk 外观属性, flags 字段标志, frame 内框, border 边框, resources 字体资源, content 内容
// 返回: error 布局、字体或编码错误
func (r *Reader) writeAnnotationText(ctx context.Context, annotation Annotation, mk Dictionary, flags Integer, frame Rectangle, border *annotationBorder, resources Dictionary, content *strings.Builder) error {
	dict := annotation.Dictionary
	free := annotation.Subtype == "FreeText"
	richField := Name("RC")
	if !free {
		richField = "RV"
	}
	if free || dict["FT"] == Name("Tx") && flags&(1<<25) != 0 {
		rich, err := r.Resolve(dict[richField])
		if err != nil {
			return err
		}
		if rich != nil {
			return r.writeAnnotationRichText(ctx, annotation, rich, flags, frame, border, resources, content)
		}
	}
	textObject := dict["V"]
	var choice *ChoiceField
	if !free && dict["FT"] == Name("Sig") && textObject == nil {
		return nil
	}
	if !free && dict["FT"] == Name("Ch") {
		value, err := r.readChoiceField(dict)
		if err != nil {
			return err
		}
		choice = &value
	}
	if free {
		textObject = dict["Contents"]
	} else if dict["FT"] == Name("Btn") {
		textObject = mk["CA"]
	} else if dict["FT"] != Name("Tx") && choice == nil {
		return &UnsupportedError{Feature: "generated field type"}
	}
	var text string
	var err error
	if choice == nil {
		text, err = r.annotationTextString(textObject)
	} else if choice.Combo {
		if len(choice.Selected) != 0 {
			text = choice.Options[choice.Selected[0]].Label
		} else if len(choice.Values) != 0 {
			text = choice.Values[0]
		}
	} else {
		labels := make([]string, 0, len(choice.Options)-choice.TopIndex)
		normalize := strings.NewReplacer("\r", " ", "\n", " ")
		for _, option := range choice.Options[choice.TopIndex:] {
			labels = append(labels, normalize.Replace(option.Label))
		}
		text = strings.Join(labels, "\n")
	}
	if err != nil {
		return err
	}
	if choice != nil && choice.Combo {
		text = strings.NewReplacer("\r", " ", "\n", " ").Replace(text)
	}
	if text == "" && !(choice != nil && !choice.Combo && len(choice.Options) > choice.TopIndex) {
		return nil
	}
	if flags&(1<<13) != 0 {
		text = strings.Repeat("*", utf8.RuneCountInString(text))
	}
	list := choice != nil && !choice.Combo
	multiline := free || flags&(1<<12) != 0 || list
	if !multiline {
		text = strings.ReplaceAll(text, "\n", " ")
	}
	p, fontName, stateContent, err := r.annotationTextState(ctx, dict["DA"], resources)
	if err != nil {
		return err
	}
	if p.state.font == nil || p.state.fontSize < 0 || p.state.hscale <= 0 {
		return fmt.Errorf("invalid variable text font")
	}
	if p.state.font.Vertical || p.textMatrix[0] != 1 || p.textMatrix[1] != 0 || p.textMatrix[2] != 0 || p.textMatrix[3] != 1 {
		return &UnsupportedError{Feature: "generated transformed text"}
	}
	w, h := frame.XMax-frame.XMin, frame.YMax-frame.YMin
	inset := math.Min(math.Max(1, border.width)+1, math.Min(w, h)/2)
	x, y, width, height := frame.XMin+inset, frame.YMin+inset, w-2*inset, h-2*inset
	if width <= 0 || height <= 0 {
		return nil
	}
	letters, err := annotationLetters(ctx, p.state.font, text)
	if err != nil {
		return err
	}
	align := Integer(0)
	if !free && dict["FT"] == Name("Btn") {
		align = 1
	} else if value, err := r.Resolve(dict["Q"]); err != nil {
		return err
	} else if value != nil {
		var ok bool
		align, ok = value.(Integer)
		if !ok || align < 0 || align > 2 {
			return fmt.Errorf("invalid field justification")
		}
	}
	comb, err := r.annotationComb(dict, flags)
	if err != nil {
		return err
	}
	if comb > 0 && len(letters) > comb {
		return fmt.Errorf("field value exceeds comb length")
	}
	size := p.state.fontSize
	if list && size == 0 {
		size = math.Min(12, height)
	}
	if size == 0 {
		low, high := 0.0, height
		for range 32 {
			if err := ctx.Err(); err != nil {
				return err
			}
			candidate := (low + high) / 2
			lines := annotationLines(letters, p.state, candidate, width, multiline)
			leading := p.state.leading
			if leading <= 0 {
				leading = candidate * 1.2
			}
			fits := float64(len(lines)-1)*leading+candidate <= height
			if comb > 0 {
				fits = candidate <= height
				for _, letter := range letters {
					fits = fits && letter.width/1000*candidate*p.state.hscale <= width/float64(comb)
				}
			} else {
				for _, line := range lines {
					fits = fits && annotationLineWidth(line, p.state, candidate) <= width
				}
			}
			if fits {
				low = candidate
			} else {
				high = candidate
			}
		}
		size = low
	}
	if size <= 0 {
		return fmt.Errorf("invalid automatic font size")
	}
	fmt.Fprintf(content, "q %g %g %g %g re W n\n", x, y, width, height)
	leading := p.state.leading
	if leading <= 0 {
		leading = size * 1.2
	}
	if list {
		for _, selected := range choice.Selected {
			row := selected - choice.TopIndex
			if row >= 0 && float64(row)*leading < height {
				fmt.Fprintf(content, "q 0.153 0.392 0.714 rg %g %g %g %g re f Q\n", x, y+height-float64(row+1)*leading, width, leading)
			}
		}
	}
	if !free {
		content.WriteString("/Tx BMC\n")
	}
	fmt.Fprintf(content, "BT\n%s\n/%s %g Tf\n", stateContent, escapeAnnotationName(fontName), size)
	lineWidth := width
	if list {
		lineWidth = math.Inf(1)
	}
	lines := annotationLines(letters, p.state, size, lineWidth, multiline)
	baseline := y + (height-size)/2 + size*.2 - p.state.rise
	if multiline {
		baseline = y + height - size - p.state.rise
	}
	if comb > 0 {
		start := 0
		if align == 1 {
			start = (comb - len(letters)) / 2
		} else if align == 2 {
			start = comb - len(letters)
		}
		for i, letter := range letters {
			left := x + (float64(start+i)+.5)*width/float64(comb) - letter.width*size*p.state.hscale/2000
			fmt.Fprintf(content, "1 0 0 1 %g %g Tm <%x> Tj\n", left, baseline, letter.code)
		}
	} else {
		for i, line := range lines {
			if list {
				if float64(i)*leading >= height {
					break
				}
				content.WriteString("0 g\n" + stateContent)
				if _, selected := slices.BinarySearch(choice.Selected, choice.TopIndex+i); selected {
					content.WriteString("1 g\n")
				}
			}
			left := x + float64(align)*(width-annotationLineWidth(line, p.state, size))/2
			fmt.Fprintf(content, "1 0 0 1 %g %g Tm <", left, baseline-float64(i)*leading)
			for _, letter := range line {
				fmt.Fprintf(content, "%x", letter.code)
			}
			content.WriteString("> Tj\n")
		}
	}
	content.WriteString("ET\n")
	if !free {
		content.WriteString("EMC\n")
	}
	content.WriteString("Q\n")
	return nil
}

// annotationComb 读取文本字段有效的分格数量，忽略不适用的标志
// 入参: dict 字段, flags 字段标志
// 返回: int 分格数量, error 最大长度错误
func (r *Reader) annotationComb(dict Dictionary, flags Integer) (int, error) {
	if dict["FT"] != Name("Tx") || flags&(1<<24) == 0 || flags&((1<<12)|(1<<13)|(1<<20)) != 0 {
		return 0, nil
	}
	value, err := r.Resolve(dict["MaxLen"])
	if err != nil || value == nil {
		return 0, err
	}
	n, ok := value.(Integer)
	if !ok || n <= 0 || int64(int(n)) != int64(n) {
		return 0, fmt.Errorf("invalid field maximum length")
	}
	return int(n), nil
}

// annotationTextState 解析默认外观的字体和文字状态，保留合法绘图操作
// 入参: ctx 取消上下文, object 默认外观, resources 外观资源
// 返回: *pageInterpreter 文字状态, Name 字体名, string 状态操作, error 操作或资源错误
func (r *Reader) annotationTextState(ctx context.Context, object Object, resources Dictionary) (*pageInterpreter, Name, string, error) {
	value, err := r.Resolve(object)
	if err != nil {
		return nil, "", "", err
	}
	appearance, ok := value.(String)
	if !ok {
		return nil, "", "", fmt.Errorf("missing variable text default appearance")
	}
	p := &pageInterpreter{reader: r, resources: resources, ctx: ctx, inText: true, textMatrix: Identity(), lineMatrix: Identity()}
	p.state = graphicsState{matrix: Identity(), hscale: 1, fillSpace: "DeviceGray", strokeSpace: "DeviceGray", style: Style{
		Fill: Paint{SourceSpace: "DeviceGray", Alpha: 1}, Stroke: Paint{SourceSpace: "DeviceGray", Alpha: 1}, LineWidth: 1, MiterLimit: 10,
	}}
	var fontName Name
	var stateContent strings.Builder
	matrices := 0
	err = WalkOperations(ctx, appearance, func(op Operation) error {
		auto := false
		switch op.Operator {
		case "Tf":
			if len(op.Operands) == 2 {
				fontName, _ = op.Operands[0].(Name)
				if size, err := numbers(op.Operands[1:], 1); err == nil && size[0] == 0 {
					auto = true
					op.Operands = []Object{op.Operands[0], Integer(1)}
				}
			}
		case "Tm":
			matrices++
			if matrices > 1 {
				return fmt.Errorf("multiple default appearance text matrices")
			}
		case "Tc", "Tw", "Tz", "TL", "Tr", "Ts", "g", "G", "rg", "RG", "k", "K", "w", "J", "j", "M", "d", "ri", "cs", "CS", "sc", "SC", "scn", "SCN":
		default:
			return &UnsupportedError{Feature: "default appearance operator " + op.Operator}
		}
		if err := p.operation(op); err != nil {
			return err
		}
		if auto {
			p.state.fontSize = 0
		}
		if op.Operator != "Tf" && op.Operator != "Tm" {
			for _, operand := range op.Operands {
				if err := writeAnnotationOperand(&stateContent, operand); err != nil {
					return err
				}
				stateContent.WriteByte(' ')
			}
			stateContent.WriteString(op.Operator + "\n")
		}
		return nil
	})
	return p, fontName, stateContent.String(), err
}

// annotationLetters 将Unicode文本映射回原字体字符码，不替换缺失字形
// 入参: ctx 取消上下文, font 字体, text 文本
// 返回: []annotationLetter 编码与度量, error 缺字或字体错误
func annotationLetters(ctx context.Context, font *Font, text string) ([]annotationLetter, error) {
	wanted := map[rune]bool{}
	for _, c := range text {
		if unicode.IsMark(c) || unicode.IsLetter(c) && !unicode.In(c, unicode.Latin, unicode.Greek, unicode.Cyrillic, unicode.Han, unicode.Hiragana, unicode.Katakana, unicode.Hangul) {
			return nil, &UnsupportedError{Feature: "generated complex-script text"}
		}
		wanted[c] = true
	}
	lookup, err := annotationLetterLookup(ctx, font, wanted)
	if err != nil {
		return nil, err
	}
	letters := make([]annotationLetter, 0, utf8.RuneCountInString(text))
	for _, c := range text {
		if c == '\n' {
			letters = append(letters, annotationLetter{text: c})
			continue
		}
		letter, ok := lookup[c]
		if !ok {
			return nil, &UnsupportedError{Feature: fmt.Sprintf("field font has no encoding for U+%04X", c)}
		}
		letters = append(letters, letter)
	}
	return letters, nil
}

// annotationLetterLookup 收集所需Unicode字符对应的原字体编码和度量
// 入参: ctx 取消上下文, font 字体, wanted 所需字符
// 返回: map[rune]annotationLetter 字符编码, error 取消错误
func annotationLetterLookup(ctx context.Context, font *Font, wanted map[rune]bool) (map[rune]annotationLetter, error) {
	codes := make([]string, 0, len(wanted)+256)
	for code, value := range font.Unicode {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		c, n := utf8.DecodeRuneInString(value)
		if n == len(value) && wanted[c] {
			codes = append(codes, code)
		}
	}
	if !font.composite {
		for i := 0; i < 256; i++ {
			codes = append(codes, string([]byte{byte(i)}))
		}
	}
	slices.Sort(codes)
	lookup := map[rune]annotationLetter{}
	for _, code := range slices.Compact(codes) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		glyphs, err := font.DecodeContext(ctx, []byte(code))
		if err != nil || len(glyphs) != 1 || utf8.RuneCountInString(glyphs[0].Text) != 1 {
			continue
		}
		glyph := glyphs[0]
		letter, _ := utf8.DecodeRuneInString(glyph.Text)
		if wanted['\u00a0'] && !font.composite && font.encoding == "WinAnsiEncoding" && code == "\xa0" && glyph.Name == "space" && glyph.Text == " " && font.Unicode[code] == "" {
			lookup['\u00a0'] = annotationLetter{'\u00a0', code, glyph.Width, glyph.WordSpace}
		}
		if _, ok := lookup[letter]; !ok && wanted[letter] {
			lookup[letter] = annotationLetter{letter, code, glyph.Width, glyph.WordSpace}
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return lookup, nil
}

// annotationDefaultColor 读取自由文本默认外观的边框颜色
// 入参: ctx 取消上下文, object 默认外观字符串
// 返回: Array 设备颜色, error 属性或颜色空间错误
func (r *Reader) annotationDefaultColor(ctx context.Context, object Object) (Array, error) {
	value, err := r.Resolve(object)
	if err != nil {
		return nil, err
	}
	data, ok := value.(String)
	if !ok {
		return nil, fmt.Errorf("missing free text default appearance")
	}
	color := Array{Integer(0)}
	err = WalkOperations(ctx, data, func(op Operation) error {
		switch op.Operator {
		case "g", "rg", "k":
			count := map[string]int{"g": 1, "rg": 3, "k": 4}[op.Operator]
			if _, err := numbers(op.Operands, count); err != nil {
				return err
			}
			color = slices.Clone(op.Operands)
		case "cs", "sc", "scn":
			return &UnsupportedError{Feature: "generated free text non-device border color"}
		}
		return nil
	})
	return color, err
}

// annotationLineWidth 计算文本行推进量，末字不计字符间距
// 入参: letters 字符, state 文字状态, size 字号
// 返回: float64 行宽
func annotationLineWidth(letters []annotationLetter, state graphicsState, size float64) float64 {
	width := 0.0
	for i, letter := range letters {
		width += letter.width / 1000 * size
		if i+1 < len(letters) {
			width += state.spacing
			if letter.word {
				width += state.wordSpacing
			}
		}
	}
	return width * state.hscale
}

// annotationLines 按显式换行和词边界折行，长词按字符拆分
// 入参: letters 字符, state 文字状态, size 字号, width 可用宽度, multiline 是否多行
// 返回: [][]annotationLetter 排版行
func annotationLines(letters []annotationLetter, state graphicsState, size, width float64, multiline bool) [][]annotationLetter {
	if !multiline {
		return [][]annotationLetter{letters}
	}
	var lines [][]annotationLetter
	start, lastBreak := 0, -1
	advance := 0.0
	for i := 0; i < len(letters); i++ {
		letter := letters[i]
		if letter.text == '\n' {
			lines = append(lines, letters[start:i])
			start, lastBreak, advance = i+1, -1, 0
			continue
		}
		next := (letter.width / 1000 * size) * state.hscale
		if i > start {
			prev := letters[i-1]
			next += state.spacing * state.hscale
			if prev.word {
				next += state.wordSpacing * state.hscale
			}
		}
		if advance+next > width && i > start {
			end := i
			if lastBreak >= start {
				end = lastBreak + 1
			}
			lines = append(lines, letters[start:end])
			start, lastBreak, advance = end, -1, 0
			i = end - 1
			continue
		}
		advance += next
		if unicode.IsSpace(letter.text) && letter.text != '\u00a0' && letter.text != '\u2007' && letter.text != '\u202f' {
			lastBreak = i
		}
	}
	return append(lines, letters[start:])
}

// escapeAnnotationName 将资源名编码为PDF名称，不改变原始字节
// 入参: name 资源名称
// 返回: string 可写入内容流的名称
func escapeAnnotationName(name Name) string {
	var out strings.Builder
	for _, b := range []byte(name) {
		if b <= 32 || b >= 127 || strings.ContainsRune("#%()/<>[]{}", rune(b)) {
			fmt.Fprintf(&out, "#%02X", b)
		} else {
			out.WriteByte(b)
		}
	}
	return out.String()
}

// writeAnnotationOperand 写入已验证的默认外观操作数
// 入参: content 内容, value 操作数
// 返回: error 不支持的操作数错误
func writeAnnotationOperand(content *strings.Builder, value Object) error {
	switch value := value.(type) {
	case Name:
		content.WriteString("/" + escapeAnnotationName(value))
	case Integer:
		fmt.Fprintf(content, "%d", value)
	case Real:
		content.WriteString(strconv.FormatFloat(float64(value), 'f', -1, 64))
	case Array:
		content.WriteByte('[')
		for _, item := range value {
			if err := writeAnnotationOperand(content, item); err != nil {
				return err
			}
			content.WriteByte(' ')
		}
		content.WriteByte(']')
	default:
		return fmt.Errorf("invalid default appearance operand")
	}
	return nil
}
