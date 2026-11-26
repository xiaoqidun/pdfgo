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
	"bytes"
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
	text     rune
	code     string
	width    float64
	word     bool
	vertical *VerticalMetrics
}

// annotationCaptionText 保存标题字体、源编码及可选的段落整形结果
type annotationCaptionText struct {
	font    *Font
	object  Object
	letters []annotationLetter
	shaper  *annotationShaper
}

// annotationChoiceVisibleLines 按可用跨度与步距截取列表可见项，避免隐藏项触发字体解析或整形
// 入参: ctx 取消上下文, text 首个可见项起的文字, span 可用跨度, leading 单项步距
// 返回: string 可见项文字, error 步距或取消错误
func annotationChoiceVisibleLines(ctx context.Context, text string, span, leading float64) (string, error) {
	if leading <= 0 || math.IsNaN(leading) || math.IsInf(leading, 0) {
		return "", fmt.Errorf("invalid choice field line spacing")
	}
	rows := math.Ceil(span / leading)
	start := 0
	for row := 1.0; ; row++ {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		end := strings.IndexByte(text[start:], '\n')
		if end < 0 {
			return text, nil
		}
		end += start
		if row >= rows {
			return text[:end], nil
		}
		start = end + 1
	}
}

// annotationChoiceVisibleText 截取横排列表可见选项，避免隐藏项触发字体解析或整形
// 入参: ctx 取消上下文, text 首个可见项起的文字, state 默认文字状态, height 可用高度
// 返回: string 可见项文字, error 行距或取消错误
func annotationChoiceVisibleText(ctx context.Context, text string, state graphicsState, height float64) (string, error) {
	size := state.fontSize
	if size == 0 {
		size = min(12, height)
	}
	leading := state.leading
	if leading <= 0 {
		leading = size * 1.2
	}
	return annotationChoiceVisibleLines(ctx, text, height, leading)
}

// annotationRawSingleByte 提取不带Unicode字节序标记的非空单字节字符串
// 入参: value 已解析的对象
// 返回: String 原始字节串, bool 是否为非空单字节串
func annotationRawSingleByte(value Object) (String, bool) {
	raw, ok := value.(String)
	if !ok || len(raw) == 0 {
		return nil, false
	}
	if bytes.HasPrefix(raw, []byte{0xfe, 0xff}) || bytes.HasPrefix(raw, []byte{0xff, 0xfe}) || bytes.HasPrefix(raw, []byte{0xef, 0xbb, 0xbf}) {
		return nil, false
	}
	return raw, true
}

// annotationZapfDingbatsLetters 按单字节ZapfDingbats编码查找已定义字形并构造排版字符
// 入参: ctx 取消上下文, font 字体, raw 原始单字节串
// 返回: []annotationLetter 编码与度量, error 缺字、解码或取消错误
func annotationZapfDingbatsLetters(ctx context.Context, font *Font, raw String) ([]annotationLetter, error) {
	letters := make([]annotationLetter, 0, len(raw))
	for i, b := range raw {
		if i&255 == 0 {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
		}
		code := string([]byte{b})
		glyph, ok, err := font.encodingGlyph(ctx, code)
		if err != nil {
			return nil, err
		}
		if !ok {
			return nil, &UnsupportedError{Feature: fmt.Sprintf("font cannot encode ZapfDingbats byte 0x%02X", b)}
		}
		ch, _ := utf8.DecodeRuneInString(glyph.Text)
		letter := annotationLetter{text: ch, code: code, width: glyph.Width, word: glyph.WordSpace}
		if font.Vertical {
			letter.vertical = &glyph.Vertical
		}
		letters = append(letters, letter)
	}
	return letters, ctx.Err()
}

// annotationLetters 将Unicode文本映射回原字体字符码，不替换缺失字形
// 入参: ctx 取消上下文, font 字体, text 文本
// 返回: []annotationLetter 编码与度量, error 缺字或字体错误
func annotationLetters(ctx context.Context, font *Font, text string) ([]annotationLetter, error) {
	if annotationNeedsShaping(text) {
		return nil, &UnsupportedError{Feature: "generated complex-script text"}
	}
	wanted := map[rune]bool{}
	for _, c := range text {
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
			return annotationEncodedLetters(ctx, font, text, lookup)
		}
		letters = append(letters, letter)
	}
	return letters, nil
}

// annotationLetterLookup 收集所需Unicode字符对应的原字体编码和度量
// 入参: ctx 取消上下文, font 字体, wanted 所需字符
// 返回: map[rune]annotationLetter 字符编码, error 解码、超限或取消错误
func annotationLetterLookup(ctx context.Context, font *Font, wanted map[rune]bool) (map[rune]annotationLetter, error) {
	entries, err := font.characterLookup(ctx, wanted)
	if err != nil {
		return nil, err
	}
	lookup := map[rune]annotationLetter{}
	for ch, entry := range entries {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		letter := annotationLetter{text: ch, code: entry.code, width: entry.glyph.Width, word: entry.glyph.WordSpace}
		if font.Vertical {
			letter.vertical = &entry.glyph.Vertical
		}
		lookup[ch] = letter
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return lookup, nil
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
// 入参: ctx 取消上下文, letters 字符, state 文字状态, size 字号, width 可用宽度, multiline 是否多行
// 返回: [][]annotationLetter 排版行, error 行数超限或取消错误
func annotationLines(ctx context.Context, letters []annotationLetter, state graphicsState, size, width float64, multiline bool) ([][]annotationLetter, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !multiline {
		return [][]annotationLetter{letters}, nil
	}
	var lines [][]annotationLetter
	start, lastBreak := 0, -1
	advance := 0.0
	for i := 0; i < len(letters); i++ {
		if i&255 == 0 {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
		}
		letter := letters[i]
		if letter.text == '\n' {
			if len(lines) >= maxAnnotationRichLines {
				return nil, fmt.Errorf("%w: text line limit exceeded", ErrStreamLimitExceeded)
			}
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
			if len(lines) >= maxAnnotationRichLines {
				return nil, fmt.Errorf("%w: text line limit exceeded", ErrStreamLimitExceeded)
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
	if len(lines) >= maxAnnotationRichLines {
		return nil, fmt.Errorf("%w: text line limit exceeded", ErrStreamLimitExceeded)
	}
	return append(lines, letters[start:]), ctx.Err()
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

// ReadAnnotationText 读取注解文本字段并响应取消，RC和RV允许文本流，其他字段要求文本字符串
// 入参: ctx 取消上下文, annotation 注解, field 文本字段名
// 返回: string Unicode文本, error 引用、类型、编码、超限或取消错误
func (r *Reader) ReadAnnotationText(ctx context.Context, annotation Annotation, field Name) (string, error) {
	var warning func(Diagnostic)
	if r != nil {
		warning = r.warning
	}
	return r.readAnnotationText(ctx, annotation, field, warning)
}

// readAnnotationText 按指定警告回调读取注解文本并响应取消，取消错误不作为异常字段忽略
// 入参: ctx 取消上下文, annotation 注解, field 文本字段名, warning 异常字段警告，空值使用严格检查
// 返回: string Unicode文本, error 引用、类型、编码、超限或取消错误
func (r *Reader) readAnnotationText(ctx context.Context, annotation Annotation, field Name, warning func(Diagnostic)) (string, error) {
	if ctx == nil {
		return "", fmt.Errorf("missing annotation text context")
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	value, err := r.Resolve(annotation.Dictionary[field])
	if canceled := ctx.Err(); canceled != nil {
		return "", canceled
	}
	if err != nil || value == nil {
		return "", err
	}
	limit := annotationRichContentLimit(r)
	var data String
	switch value := value.(type) {
	case String:
		if len(value) > 3 && (len(value)-3)/2 > limit {
			err = fmt.Errorf("%w: annotation text limit exceeded", ErrStreamLimitExceeded)
			break
		}
		data = value
	case *Stream:
		if field != "RC" && field != "RV" {
			err = fmt.Errorf("invalid annotation %s: expected text string", field)
			break
		}
		if value == nil {
			err = fmt.Errorf("missing annotation text stream")
			break
		}
		var decoded []byte
		decoded, err = r.decodeAnnotationTextStream(ctx, value, int64(limit))
		data = String(decoded)
	default:
		err = fmt.Errorf("invalid annotation %s: expected text string", field)
	}
	var text string
	if err == nil {
		text, err = DecodeTextString(data)
		if err == nil && len(text) > limit {
			err = fmt.Errorf("%w: annotation text limit exceeded", ErrStreamLimitExceeded)
		}
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if err != nil {
		if warning == nil || isFatalReaderError(err) {
			return "", err
		}
		warning(Diagnostic{Message: fmt.Sprintf("invalid PDF annotation %s ignored; appearance retained", field)})
		return "", ctx.Err()
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

// decodeAnnotationTextStream 按指定字节上限解码注解文本流，不放大默认流上限
// 入参: ctx 取消上下文, stream 数据流, limit 最大解压字节数
// 返回: []byte 独立解码数据, error 解码、超限或取消错误
func (r *Reader) decodeAnnotationTextStream(ctx context.Context, stream *Stream, limit int64) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !r.ownsObject(stream.reader, stream.cache) {
		return nil, fmt.Errorf("stream belongs to another reader")
	}
	return stream.decodeWithLimit(ctx, r, r.warning, 0, limit)
}

// annotationTextString 读取文本字符串或文本流，并统一换行符
// 入参: ctx 取消上下文, object 文本对象
// 返回: string Unicode文本, error 引用、类型、编码、超限或取消错误
func (r *Reader) annotationTextString(ctx context.Context, object Object) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	value, err := r.Resolve(object)
	if canceled := ctx.Err(); canceled != nil {
		return "", canceled
	}
	if err != nil || value == nil {
		return "", err
	}
	return r.annotationTextStringValue(ctx, value)
}

// annotationTextStringValue 解码已解析的文本字符串或文本流，并统一换行符
// 入参: ctx 取消上下文, value 已解析对象
// 返回: string Unicode文本, error 类型、编码、超限或取消错误
func (r *Reader) annotationTextStringValue(ctx context.Context, value Object) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if value == nil {
		return "", nil
	}
	limit := annotationRichContentLimit(r)
	var data []byte
	var err error
	switch value := value.(type) {
	case String:
		if len(value) > 3 && (len(value)-3)/2 > limit {
			return "", fmt.Errorf("%w: annotation text limit exceeded", ErrStreamLimitExceeded)
		}
		data = value
	case *Stream:
		if value == nil {
			return "", fmt.Errorf("missing annotation text stream")
		}
		data, err = r.decodeAnnotationTextStream(ctx, value, int64(limit))
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
	if len(text) > limit {
		return "", fmt.Errorf("%w: annotation text limit exceeded", ErrStreamLimitExceeded)
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	return strings.ReplaceAll(strings.ReplaceAll(text, "\r\n", "\n"), "\r", "\n"), nil
}

// widgetAppearance 为缺少外观的控件生成当前值的静态外观
// 入参: ctx 取消上下文, page 页面, annotation 控件
// 返回: *Stream 外观流, error 属性、能力、超限或取消错误
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
// 返回: *Stream 外观流, error 边界、不支持的布局、资源、超限或取消错误
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
	rotation, err := r.Resolve(mk["R"])
	if err != nil {
		return nil, err
	}
	if rotation != nil {
		n, err := r.number(rotation)
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
	if w <= 0 || h <= 0 || math.IsNaN(w) || math.IsInf(w, 0) || math.IsNaN(h) || math.IsInf(h, 0) {
		return nil, fmt.Errorf("invalid variable text bounds")
	}
	frame := Rectangle{0, 0, w, h}
	fw, fh := frame.XMax-frame.XMin, frame.YMax-frame.YMin
	borderAnnotation := annotation
	borderAnnotation.Dictionary = maps.Clone(dict)
	borderColor, err := r.Resolve(mk["BC"])
	if err != nil {
		return nil, err
	}
	borderAnnotation.Dictionary["C"] = borderColor
	if borderColor == nil {
		borderAnnotation.Dictionary["C"] = Array{}
	}
	border, err := r.readAnnotationBorder(borderAnnotation)
	if err != nil {
		return nil, err
	}
	background, err := r.Resolve(mk["BG"])
	if err != nil {
		return nil, err
	}
	var content strings.Builder
	if background != nil {
		color, ok := background.(Array)
		if !ok {
			return nil, fmt.Errorf("invalid annotation background color")
		}
		fill, err := r.writeAnnotationColor(&content, color, false)
		if err != nil {
			return nil, err
		}
		if fill {
			writeAnnotationOperation(&content, "re f", frame.XMin, frame.YMin, fw, fh)
		}
	}
	contentLimit := annotationRichContentLimit(r)
	stroke, err := r.writeAnnotationStroke(ctx, &content, border, contentLimit)
	if err != nil {
		return nil, err
	}
	if stroke && !(dict["FT"] == Name("Btn") && flags&(1<<15) != 0 && flags&(1<<16) == 0) {
		inset := math.Min(border.width/2, math.Min(fw, fh)/2)
		switch border.style {
		case "S", "D":
			writeAnnotationOperation(&content, "re S", frame.XMin+inset, frame.YMin+inset, fw-2*inset, fh-2*inset)
		case "U":
			writeAnnotationOperation(&content, "m", frame.XMin, frame.YMin+inset)
			writeAnnotationOperation(&content, "l S", frame.XMax, frame.YMin+inset)
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
	stream := &Stream{Dictionary: Dictionary{"Type": Name("XObject"), "Subtype": Name("Form"), "BBox": Array{Integer(0), Integer(0), Real(w), Real(h)}, "Matrix": Array{Real(matrix[0]), Real(matrix[1]), Real(matrix[2]), Real(matrix[3]), Real(matrix[4]), Real(matrix[5])}, "Resources": resources}, reader: r, cache: r.cache, hasWarning: r.warning != nil}
	if dict["FT"] == Name("Btn") && flags&(1<<16) == 0 {
		err = r.writeAnnotationToggleButton(ctx, annotation, mk, flags, frame, border, resources, &content, stroke)
	} else if dict["FT"] == Name("Btn") {
		err = r.writeAnnotationPushButton(ctx, annotation, mk, frame, border, resources, &content)
	} else {
		err = r.writeAnnotationText(ctx, annotation, mk, flags, frame, border, resources, &content)
	}
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if content.Len() > contentLimit {
		return nil, fmt.Errorf("%w: widget appearance content limit exceeded", ErrStreamLimitExceeded)
	}
	stream.Data = []byte(content.String())
	return stream, nil
}

// annotationCaptionFont 选择能够编码或整形标题的横排字体，不修改源资源
// 入参: ctx 取消上下文, resources 可用资源, text 标题文字
// 返回: *annotationCaptionText 字体与文字, error 字体、编码、超限或取消错误
func (r *Reader) annotationCaptionFont(ctx context.Context, resources Dictionary, text string) (*annotationCaptionText, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	standard := Dictionary{"Type": Name("Font"), "Subtype": Name("Type1"), "BaseFont": Name("Helvetica"), "Encoding": Name("WinAnsiEncoding")}
	font, err := r.ReadFontContext(ctx, standard)
	if err != nil {
		return nil, err
	}
	encoded, encodingErr := r.encodeAnnotationCaption(ctx, font, standard, text)
	if encodingErr == nil {
		return encoded, nil
	}
	if isFatalReaderError(encodingErr) {
		return nil, encodingErr
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	value, err := r.Resolve(resources["Font"])
	if err != nil {
		return nil, err
	}
	fonts, ok := value.(Dictionary)
	if value != nil && !ok {
		return nil, fmt.Errorf("invalid caption font resources")
	}
	names := make([]Name, 0, len(fonts))
	for name := range fonts {
		names = append(names, name)
	}
	slices.Sort(names)
	for _, name := range names {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		font, err := r.ReadFontContext(ctx, fonts[name])
		if err != nil {
			if isFatalReaderError(err) {
				return nil, err
			}
			continue
		}
		if font.Vertical {
			continue
		}
		encoded, err := r.encodeAnnotationCaption(ctx, font, fonts[name], text)
		if err == nil {
			return encoded, nil
		}
		if isFatalReaderError(err) {
			return nil, err
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return nil, encodingErr
}

// encodeAnnotationCaption 使用源字体生成标题字形与编码，复杂文字通过整形后端定位
// 入参: ctx 取消上下文, font 源字体, object 字体资源, text 标题原文
// 返回: *annotationCaptionText 字体与文字, error 整形、编码、超限或取消错误
func (r *Reader) encodeAnnotationCaption(ctx context.Context, font *Font, object Object, text string) (*annotationCaptionText, error) {
	result := &annotationCaptionText{font: font, object: object}
	if r.textShaper != nil && annotationNeedsShaping(text) {
		s := &annotationShaper{ctx: ctx, shape: r.textShaper, font: font, cache: make(map[annotationShapeKey]*annotationShapedLine), entries: make(map[uint16]fontEncodedGlyph), maxBytes: annotationRichContentLimit(r)}
		if _, err := s.prepare(text); err != nil {
			return nil, err
		}
		resource, err := s.fontResource()
		if err != nil {
			return nil, err
		}
		if resource != nil {
			result.object = resource.Dictionary
		}
		result.shaper = s
	} else {
		var err error
		result.letters, err = annotationLetters(ctx, font, text)
		if err != nil {
			return nil, err
		}
	}
	return result, ctx.Err()
}

// writeAnnotationText 按字段值和默认外观生成裁剪后的文字内容
// 入参: ctx 取消上下文, annotation 注解, mk 外观属性, flags 字段标志, frame 内框, border 边框, resources 字体资源, content 内容
// 返回: error 布局、字体、编码、超限或取消错误
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
		value, err := r.readChoiceFieldContext(ctx, dict)
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
	var rawButton String
	var textErr error
	var err error
	if choice == nil {
		if err := ctx.Err(); err != nil {
			return err
		}
		value, resolveErr := r.Resolve(textObject)
		if canceled := ctx.Err(); canceled != nil {
			return canceled
		}
		if resolveErr != nil {
			return resolveErr
		}
		if !free && dict["FT"] == Name("Btn") {
			if raw, ok := annotationRawSingleByte(value); ok {
				if len(raw) > annotationRichContentLimit(r) {
					return fmt.Errorf("%w: annotation text limit exceeded", ErrStreamLimitExceeded)
				}
				rawButton = raw
			}
		}
		text, err = r.annotationTextStringValue(ctx, value)
		if err != nil && len(rawButton) > 0 && !isFatalReaderError(err) {
			textErr = err
			err = nil
		}
	} else if choice.Combo {
		if len(choice.Selected) != 0 {
			text = choice.Options[choice.Selected[0]].Label
		} else if len(choice.Values) != 0 {
			text = choice.Values[0]
		}
	} else {
		contentLimit := annotationRichContentLimit(r)
		var builder strings.Builder
		normalize := strings.NewReplacer("\r", " ", "\n", " ")
		for i, option := range choice.Options[choice.TopIndex:] {
			if err := ctx.Err(); err != nil {
				return err
			}
			extra := len(option.Label)
			if i > 0 {
				extra++
			}
			if extra > contentLimit-builder.Len() {
				return fmt.Errorf("%w: choice field text limit exceeded", ErrStreamLimitExceeded)
			}
			if i > 0 {
				builder.WriteByte('\n')
			}
			builder.WriteString(normalize.Replace(option.Label))
		}
		text = builder.String()
	}
	if err != nil {
		return err
	}
	if choice != nil && choice.Combo {
		text = strings.NewReplacer("\r", " ", "\n", " ").Replace(text)
	}
	if text == "" && len(rawButton) == 0 && !(choice != nil && !choice.Combo && len(choice.Options) > choice.TopIndex) {
		return nil
	}
	textField := !free && dict["FT"] == Name("Tx")
	if textField && flags&(1<<13) != 0 {
		text = strings.Repeat("*", utf8.RuneCountInString(text))
	}
	list := choice != nil && !choice.Combo
	multiline := free || textField && flags&(1<<12) != 0 || list
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
	w, h := frame.XMax-frame.XMin, frame.YMax-frame.YMin
	inset := math.Min(math.Max(1, border.width)+1, math.Min(w, h)/2)
	x, y, width, height := frame.XMin+inset, frame.YMin+inset, w-2*inset, h-2*inset
	if width <= 0 || height <= 0 {
		return nil
	}
	clipWidth, clipHeight := width, height
	matrix, width, height, err := annotationTextLayout(p.textMatrix, x, y, width, height)
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
	if comb > 0 && utf8.RuneCountInString(text) > comb {
		return fmt.Errorf("field value exceeds comb length")
	}
	if list {
		if p.state.font.Vertical {
			size := p.state.fontSize
			if size == 0 {
				size = min(12, width/p.state.hscale)
			}
			leading := p.state.leading
			if leading <= 0 {
				leading = size * p.state.hscale * 1.2
			}
			text, err = annotationChoiceVisibleLines(ctx, text, width, leading)
		} else {
			text, err = annotationChoiceVisibleText(ctx, text, p.state, height)
		}
		if err != nil {
			return err
		}
	}
	var letters []annotationLetter
	if len(rawButton) > 0 && p.state.font.Name == "ZapfDingbats" {
		if textErr == nil {
			letters, err = annotationLetters(ctx, p.state.font, text)
		} else {
			err = textErr
		}
		if err != nil && !isFatalReaderError(err) {
			letters, err = annotationZapfDingbatsLetters(ctx, p.state.font, rawButton)
		}
	} else {
		if textErr != nil {
			return textErr
		}
		if r.textShaper != nil && !p.state.font.Vertical && comb == 0 && annotationNeedsShaping(text) {
			layout := annotationShapedText{
				text: text, matrix: matrix, font: fontName, defaults: stateContent,
				frame: Rectangle{x, y, x + width, y + height}, clip: Rectangle{x, y, x + clipWidth, y + clipHeight},
				align: align, multiline: multiline, free: free, choice: choice,
			}
			return layout.write(ctx, r.textShaper, p, resources, content)
		}
		letters, err = annotationLetters(ctx, p.state.font, text)
	}
	if err != nil {
		return err
	}
	if comb > 0 && len(letters) != utf8.RuneCountInString(text) {
		return &UnsupportedError{Feature: "comb field requires individual character encodings"}
	}
	if p.state.font.Vertical {
		layout := annotationVerticalText{
			letters: letters, matrix: matrix, font: fontName, defaults: stateContent,
			frame: Rectangle{x, y, x + width, y + height}, clip: Rectangle{x, y, x + clipWidth, y + clipHeight},
			align: align, comb: comb, multiline: multiline, choice: choice, free: free,
		}
		return layout.write(p, content)
	}
	contentLimit := annotationRichContentLimit(r)
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
			candidate := low + (high-low)/2
			lines, err := annotationLines(ctx, letters, p.state, candidate, width, multiline)
			if err != nil {
				return err
			}
			leading := p.state.leading
			if leading <= 0 {
				leading = candidate * 1.2
			}
			fits := float64(len(lines)-1)*leading+candidate <= height
			if comb > 0 {
				fits = candidate <= height
				for i, letter := range letters {
					if i&255 == 0 {
						if err := ctx.Err(); err != nil {
							return err
						}
					}
					if !fits {
						break
					}
					fits = letter.width/1000*candidate*p.state.hscale <= width/float64(comb)
				}
			} else {
				for i, line := range lines {
					if !fits {
						break
					}
					if i&255 == 0 {
						if err := ctx.Err(); err != nil {
							return err
						}
					}
					fits = annotationLineWidth(line, p.state, candidate) <= width
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
	if size <= 0 || math.IsNaN(size) || math.IsInf(size, 0) {
		return fmt.Errorf("invalid automatic font size")
	}
	content.WriteString("q\n")
	writeAnnotationOperation(content, "re W n", x, y, clipWidth, clipHeight)
	leading := p.state.leading
	if leading <= 0 {
		leading = size * 1.2
	}
	if leading <= 0 || math.IsNaN(leading) || math.IsInf(leading, 0) {
		return fmt.Errorf("invalid text line spacing")
	}
	if list {
		content.WriteString("q\n")
		writeAnnotationOperation(content, "cm", matrix[:]...)
		for _, selected := range choice.Selected {
			if err := ctx.Err(); err != nil {
				return err
			}
			row := selected - choice.TopIndex
			if row >= 0 && float64(row)*leading < height {
				top := y + height - float64(row+1)*leading
				if !finiteFloats([]float64{x, top, width, leading}) {
					return fmt.Errorf("list selection background exceeds coordinate range")
				}
				content.WriteString("q 0.153 0.392 0.714 rg\n")
				writeAnnotationOperation(content, "re f Q", x, top, width, leading)
				if content.Len() > contentLimit {
					return fmt.Errorf("%w: text appearance content limit exceeded", ErrStreamLimitExceeded)
				}
			}
		}
		content.WriteString("Q\n")
	}
	if !free {
		content.WriteString("/Tx BMC\n")
	}
	fmt.Fprintf(content, "BT\n0 g 0 G 1 w [] 0 d\n%s\n/%s ", stateContent, escapeAnnotationName(fontName))
	writeAnnotationOperation(content, "Tf", size)
	if content.Len() > contentLimit {
		return fmt.Errorf("%w: text appearance content limit exceeded", ErrStreamLimitExceeded)
	}
	lineWidth := width
	if list {
		lineWidth = math.Inf(1)
	}
	lines, err := annotationLines(ctx, letters, p.state, size, lineWidth, multiline)
	if err != nil {
		return err
	}
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
			if i&255 == 0 {
				if err := ctx.Err(); err != nil {
					return err
				}
			}
			left := x + (float64(start+i)+.5)*width/float64(comb) - letter.width*size*p.state.hscale/2000
			if err := writeAnnotationTextMatrix(content, matrix, left, baseline); err != nil {
				return err
			}
			fmt.Fprintf(content, "<%x> Tj\n", letter.code)
			if content.Len() > contentLimit {
				return fmt.Errorf("%w: text appearance content limit exceeded", ErrStreamLimitExceeded)
			}
		}
	} else {
		for i, line := range lines {
			if err := ctx.Err(); err != nil {
				return err
			}
			if list {
				if float64(i)*leading >= height {
					break
				}
				content.WriteString("0 g\n" + stateContent)
				fmt.Fprintf(content, "/%s ", escapeAnnotationName(fontName))
				writeAnnotationOperation(content, "Tf", size)
				if _, selected := slices.BinarySearch(choice.Selected, choice.TopIndex+i); selected {
					content.WriteString("1 g\n")
				}
				if content.Len() > contentLimit {
					return fmt.Errorf("%w: text appearance content limit exceeded", ErrStreamLimitExceeded)
				}
			}
			left := x + float64(align)*(width-annotationLineWidth(line, p.state, size))/2
			if err := writeAnnotationTextMatrix(content, matrix, left, baseline-float64(i)*leading); err != nil {
				return err
			}
			content.WriteByte('<')
			for j, letter := range line {
				if j&255 == 0 {
					if err := ctx.Err(); err != nil {
						return err
					}
					if content.Len() > contentLimit {
						return fmt.Errorf("%w: text appearance content limit exceeded", ErrStreamLimitExceeded)
					}
				}
				fmt.Fprintf(content, "%x", letter.code)
			}
			content.WriteString("> Tj\n")
			if content.Len() > contentLimit {
				return fmt.Errorf("%w: text appearance content limit exceeded", ErrStreamLimitExceeded)
			}
		}
	}
	content.WriteString("ET\n")
	if !free {
		content.WriteString("EMC\n")
	}
	content.WriteString("Q\n")
	if content.Len() > contentLimit {
		return fmt.Errorf("%w: text appearance content limit exceeded", ErrStreamLimitExceeded)
	}
	return ctx.Err()
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
// 返回: *pageInterpreter 文字状态, Name 字体名, string 状态操作, error 操作、资源、超限或取消错误
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
	contentLimit := annotationRichContentLimit(r)
	var gsCache annotationGraphicsStateCache
	matrices := 0
	hasFont := false
	err = walkOperationsBorrowed(ctx, appearance, func(op Operation) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		auto := false
		switch op.Operator {
		case "Tf":
			hasFont = true
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
		case "Tc", "Tw", "Tz", "TL", "Tr", "Ts", "g", "G", "rg", "RG", "k", "K", "w", "J", "j", "M", "d", "ri", "cs", "CS", "sc", "SC", "scn", "SCN", "gs", "i":
		default:
			return &UnsupportedError{Feature: "default appearance operator " + op.Operator}
		}
		if err := p.operation(op); err != nil {
			return err
		}
		if auto {
			p.state.fontSize = 0
		}
		if op.Operator == "gs" {
			name, rewritten, err := r.annotationTextGraphicsState(p, op, &gsCache)
			if err != nil {
				return err
			}
			if name != "" {
				fontName = name
			}
			op = rewritten
		}
		if op.Operator != "Tf" && op.Operator != "Tm" {
			for _, operand := range op.Operands {
				if err := writeAnnotationOperand(&stateContent, operand); err != nil {
					return err
				}
				stateContent.WriteByte(' ')
				if stateContent.Len() > contentLimit {
					return fmt.Errorf("%w: default appearance content limit exceeded", ErrStreamLimitExceeded)
				}
			}
			stateContent.WriteString(op.Operator + "\n")
			if stateContent.Len() > contentLimit {
				return fmt.Errorf("%w: default appearance content limit exceeded", ErrStreamLimitExceeded)
			}
		}
		return nil
	})
	if err == nil && !hasFont {
		err = fmt.Errorf("missing default appearance font operator")
	}
	return p, fontName, stateContent.String(), err
}

// annotationDefaultDeviceSpace 展开颜色空间资源别名，判断其是否为标准设备色彩空间
// 入参: name 颜色空间名称, resources 合并后的资源字典
// 返回: Name 标准设备空间名，非设备空间时为空, error 引用错误
func (r *Reader) annotationDefaultDeviceSpace(name Name, resources Dictionary) (Name, error) {
	if name == "DeviceGray" || name == "DeviceRGB" || name == "DeviceCMYK" {
		return name, nil
	}
	spacesValue, err := r.Resolve(resources["ColorSpace"])
	if err != nil {
		return "", err
	}
	spaces, ok := spacesValue.(Dictionary)
	if !ok {
		return "", nil
	}
	var seen map[Name]bool
	current := name
	for {
		entry, err := r.Resolve(spaces[current])
		if err != nil || entry == nil {
			return "", err
		}
		if arr, ok := entry.(Array); ok && len(arr) == 1 {
			entry, err = r.Resolve(arr[0])
			if err != nil {
				return "", err
			}
		}
		target, ok := entry.(Name)
		if !ok {
			return "", nil
		}
		if target == "DeviceGray" || target == "DeviceRGB" || target == "DeviceCMYK" {
			return target, nil
		}
		if seen == nil {
			seen = map[Name]bool{current: true}
		}
		if seen[target] {
			return "", nil
		}
		seen[target] = true
		current = target
	}
}

// annotationDefaultColor 读取自由文本默认外观的边框颜色及色彩空间类别
// 入参: ctx 取消上下文, object 默认外观字符串, resources 合并后的资源字典
// 返回: Array 设备颜色, bool 最终填充色是否来自非设备色彩空间, error 属性或操作数错误
func (r *Reader) annotationDefaultColor(ctx context.Context, object Object, resources Dictionary) (Array, bool, error) {
	value, err := r.Resolve(object)
	if err != nil {
		return nil, false, err
	}
	data, ok := value.(String)
	if !ok {
		return nil, false, fmt.Errorf("missing free text default appearance")
	}
	color := Array{Integer(0)}
	space := Name("DeviceGray")
	err = walkOperationsBorrowed(ctx, data, func(op Operation) error {
		switch op.Operator {
		case "g", "rg", "k":
			count, target := 1, Name("DeviceGray")
			if op.Operator == "rg" {
				count, target = 3, "DeviceRGB"
			} else if op.Operator == "k" {
				count, target = 4, "DeviceCMYK"
			}
			if _, err := numbers(op.Operands, count); err != nil {
				return err
			}
			color, space = slices.Clone(op.Operands), target
		case "cs":
			if len(op.Operands) != 1 {
				return fmt.Errorf("invalid color space")
			}
			name, ok := op.Operands[0].(Name)
			if !ok {
				return fmt.Errorf("invalid color space name")
			}
			resolved, err := r.annotationDefaultDeviceSpace(name, resources)
			if err != nil {
				return err
			}
			space = resolved
			switch resolved {
			case "DeviceGray":
				color = Array{Integer(0)}
			case "DeviceRGB":
				color = Array{Integer(0), Integer(0), Integer(0)}
			case "DeviceCMYK":
				color = Array{Integer(0), Integer(0), Integer(0), Integer(1)}
			}
		case "sc", "scn":
			var count int
			switch space {
			case "DeviceGray":
				count = 1
			case "DeviceRGB":
				count = 3
			case "DeviceCMYK":
				count = 4
			default:
				if len(op.Operands) == 0 {
					return fmt.Errorf("invalid color operands")
				}
				return nil
			}
			if _, err := numbers(op.Operands, count); err != nil {
				return err
			}
			color = slices.Clone(op.Operands)
		}
		return nil
	})
	return color, space == "", err
}
