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
	"strconv"
	"strings"
)

// annotationIconFit 保存按钮正常图标的缩放及对齐属性
type annotationIconFit struct {
	when Name
	kind Name
	x, y float64
	full bool
}

// writeAnnotationPushButton 生成按钮正常图标及标题，组合区域等分属于排版选择
// 入参: ctx 取消上下文, annotation 控件, mk 外观属性, frame 内框, border 边框, resources 外观资源, content 内容
// 返回: error 图标、布局或文字错误
func (r *Reader) writeAnnotationPushButton(ctx context.Context, annotation Annotation, mk Dictionary, frame Rectangle, border *annotationBorder, resources Dictionary, content *strings.Builder) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	position := Integer(0)
	value, err := r.Resolve(mk["TP"])
	if err != nil {
		return err
	}
	if value != nil {
		var ok bool
		position, ok = value.(Integer)
		if !ok || position < 0 || position > 6 {
			return fmt.Errorf("invalid button text position")
		}
	}
	caption := ""
	if position != 1 {
		caption, err = r.ReadAnnotationTextContext(ctx, Annotation{Dictionary: mk}, "CA", nil)
		if err != nil {
			return err
		}
	}
	var icon *Stream
	fit := annotationIconFit{when: "A", kind: "P", x: .5, y: .5}
	if position != 0 && mk["I"] != nil {
		value, err := r.Resolve(mk["I"])
		if err != nil {
			return err
		}
		if value != nil {
			if _, ok := mk["I"].(Reference); !ok {
				return fmt.Errorf("invalid button icon reference")
			}
			var ok bool
			icon, ok = value.(*Stream)
			if !ok || icon == nil {
				return fmt.Errorf("invalid button icon form")
			}
			kind, err := r.Resolve(icon.Dictionary["Subtype"])
			if err != nil {
				return err
			}
			if kind != Name("Form") {
				return fmt.Errorf("invalid button icon form")
			}
			fit, err = r.readAnnotationIconFit(mk["IF"])
			if err != nil {
				return err
			}
		}
	}
	textFrame, iconFrame := frame, frame
	if icon != nil && caption != "" {
		x, y := frame.XMin+(frame.XMax-frame.XMin)/2, frame.YMin+(frame.YMax-frame.YMin)/2
		switch position {
		case 2:
			textFrame.YMax, iconFrame.YMin = y, y
		case 3:
			textFrame.YMin, iconFrame.YMax = y, y
		case 4:
			textFrame.XMin, iconFrame.XMax = x, x
		case 5:
			textFrame.XMax, iconFrame.XMin = x, x
		}
	}
	if icon != nil {
		if !fit.full {
			inset := math.Min(border.width, math.Min(iconFrame.XMax-iconFrame.XMin, iconFrame.YMax-iconFrame.YMin)/2)
			iconFrame.XMin += inset
			iconFrame.YMin += inset
			iconFrame.XMax -= inset
			iconFrame.YMax -= inset
		}
		if err := r.writeAnnotationButtonIcon(icon, fit, iconFrame, resources, content); err != nil {
			return err
		}
	}
	if caption != "" {
		return r.writeAnnotationButtonCaption(ctx, annotation, mk, caption, textFrame, border, resources, content)
	}
	return nil
}

// writeAnnotationToggleButton 恢复缺失外观的复选框及单选框，显式AS优先，自定义标题作为选中标记
// 入参: ctx 取消上下文, annotation 控件, mk 外观属性, flags 标志, frame 内框, border 边框, resources 外观资源, content 内容, bordered 是否已有边框
// 返回: error 状态、标题或字体错误
func (r *Reader) writeAnnotationToggleButton(ctx context.Context, annotation Annotation, mk Dictionary, flags Integer, frame Rectangle, border *annotationBorder, resources Dictionary, content *strings.Builder, bordered bool) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	field := annotation.Dictionary
	state, err := r.annotationButtonState(ctx, annotation, flags)
	if err != nil {
		return err
	}
	w, h := frame.XMax-frame.XMin, frame.YMax-frame.YMin
	radio := flags&(1<<15) != 0
	defaultBorder := mk["BC"] == nil && field["BS"] == nil && field["Border"] == nil
	radius := math.Max(0, math.Min(w, h)/2-1)
	if radio && (bordered || defaultBorder) {
		if defaultBorder {
			content.WriteString("0 G 1 w\n")
		}
		writeAnnotationCircle(content, w/2, h/2, radius)
		content.WriteString("S\n")
	} else if !radio && defaultBorder {
		content.WriteString("0 G 1 w\n")
		writeAnnotationOperation(content, "re S", .5, .5, math.Max(0, w-1), math.Max(0, h-1))
	}
	if state == "" || state == "Off" {
		return nil
	}
	caption, err := r.Resolve(mk["CA"])
	if err != nil {
		return err
	}
	if caption != nil {
		text, err := r.ReadAnnotationTextContext(ctx, Annotation{Dictionary: mk}, "CA", nil)
		if err != nil || text == "" {
			return err
		}
		return r.writeAnnotationButtonCaption(ctx, annotation, mk, text, frame, border, resources, content)
	}
	if radio {
		content.WriteString("0 g\n")
		writeAnnotationCircle(content, w/2, h/2, radius*.5)
		content.WriteString("f\n")
	} else {
		content.WriteString("0 G\n")
		writeAnnotationOperation(content, "w", math.Max(1, math.Min(w, h)*.08))
		content.WriteString("1 J 1 j\n")
		writeAnnotationOperation(content, "m", w*.2, h*.5)
		writeAnnotationOperation(content, "l", w*.43, h*.25)
		writeAnnotationOperation(content, "l S", w*.8, h*.78)
	}
	return nil
}

// annotationButtonState 读取当前按钮状态，Opt存在时按Kids中的零基位置区分控件
// 入参: ctx 取消上下文, annotation 已合并字段的控件, flags 字段标志
// 返回: Name 当前状态, error 状态或控件归属错误
func (r *Reader) annotationButtonState(ctx context.Context, annotation Annotation, flags Integer) (Name, error) {
	field := annotation.Dictionary
	value, err := r.Resolve(field["AS"])
	if err != nil {
		return "", err
	}
	if value != nil {
		state, ok := value.(Name)
		if !ok {
			return "", fmt.Errorf("invalid button appearance state")
		}
		return state, nil
	}
	value, err = r.Resolve(field["V"])
	if err != nil {
		return "", err
	}
	state, ok := value.(Name)
	if value != nil && !ok {
		return "", fmt.Errorf("invalid button field value")
	}
	if state == "" || state == "Off" {
		return "Off", nil
	}
	value, err = r.Resolve(field["Opt"])
	if err != nil {
		return "", err
	}
	if value != nil {
		options, ok := value.(Array)
		if !ok {
			return "", fmt.Errorf("invalid button options")
		}
		for _, object := range options {
			if err := ctx.Err(); err != nil {
				return "", err
			}
			value, err := r.Resolve(object)
			if err != nil {
				return "", err
			}
			text, ok := value.(String)
			if !ok {
				return "", fmt.Errorf("invalid button option text")
			}
			if _, err := DecodeTextString(text); err != nil {
				return "", err
			}
		}
		widgets := field["Kids"]
		if widgets == nil && field["Parent"] != nil {
			value, err := r.Resolve(field["Parent"])
			if err != nil {
				return "", err
			}
			parent, ok := value.(Dictionary)
			if !ok {
				return "", fmt.Errorf("invalid button parent")
			}
			widgets = parent["Kids"]
		}
		value, err := r.Resolve(widgets)
		if err != nil {
			return "", err
		}
		index := -1
		if value == nil && field["Parent"] == nil && len(options) == 1 {
			index = 0
		} else {
			kids, ok := value.(Array)
			if !ok || len(kids) != len(options) {
				return "", fmt.Errorf("invalid button option widgets")
			}
			for i, object := range kids {
				if err := ctx.Err(); err != nil {
					return "", err
				}
				ref, ok := object.(Reference)
				if !ok {
					return "", fmt.Errorf("invalid button widget reference")
				}
				if ref == annotation.Reference && ref != (Reference{}) {
					if index >= 0 {
						return "", fmt.Errorf("duplicate button widget reference")
					}
					index = i
				}
			}
		}
		if index < 0 {
			return "", &UnsupportedError{Feature: "button widget without option position"}
		}
		if state != Name(strconv.Itoa(index)) {
			return "Off", nil
		}
		return state, nil
	}
	if flags&(1<<15) != 0 && field["Parent"] != nil {
		return "", &UnsupportedError{Feature: "radio widget without appearance state"}
	}
	return state, nil
}

// writeAnnotationButtonCaption 绘制按钮标题，未声明DA时选择可编码字体并自动适配字号
// 入参: ctx 取消上下文, annotation 控件, mk 外观属性, caption 标题, frame 内框, border 边框, resources 外观资源, content 内容
// 返回: error 标题、字体或布局错误
func (r *Reader) writeAnnotationButtonCaption(ctx context.Context, annotation Annotation, mk Dictionary, caption string, frame Rectangle, border *annotationBorder, resources Dictionary, content *strings.Builder) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	w, h := frame.XMax-frame.XMin, frame.YMax-frame.YMin
	inset := math.Min(math.Max(1, border.width)+1, math.Min(w, h)/2)
	if w <= 2*inset || h <= 2*inset {
		return nil
	}
	value, err := r.Resolve(annotation.Dictionary["DA"])
	if err != nil {
		return err
	}
	if value == nil {
		_, _, object, err := r.annotationCaptionFont(ctx, resources, caption)
		if err != nil {
			return err
		}
		value, err := r.Resolve(resources["Font"])
		if err != nil {
			return err
		}
		fonts, ok := value.(Dictionary)
		if value != nil && !ok {
			return fmt.Errorf("invalid button font resources")
		}
		fonts = maps.Clone(fonts)
		if fonts == nil {
			fonts = Dictionary{}
		}
		name := Name("ButtonCaption")
		for i := 1; fonts[name] != nil; i++ {
			name = Name(fmt.Sprintf("ButtonCaption%d", i))
		}
		fonts[name] = object
		resources["Font"] = fonts
		annotation.Dictionary = maps.Clone(annotation.Dictionary)
		annotation.Dictionary["DA"] = String("/" + escapeAnnotationName(name) + " 0 Tf 0 g")
	}
	return r.writeAnnotationText(ctx, annotation, mk, 0, frame, border, resources, content)
}

// writeAnnotationCircle 写入圆形闭合路径，不改变绘图样式
// 入参: content 内容, x 横坐标, y 纵坐标, radius 半径
func writeAnnotationCircle(content *strings.Builder, x, y, radius float64) {
	k := radius * 4 * (math.Sqrt2 - 1) / 3
	writeAnnotationOperation(content, "m", x+radius, y)
	writeAnnotationOperation(content, "c", x+radius, y+k, x+k, y+radius, x, y+radius)
	writeAnnotationOperation(content, "c", x-k, y+radius, x-radius, y+k, x-radius, y)
	writeAnnotationOperation(content, "c", x-radius, y-k, x-k, y-radius, x, y-radius)
	writeAnnotationOperation(content, "c h", x+k, y-radius, x+radius, y-k, x+radius, y)
}

// readAnnotationIconFit 读取标准图标适配规则，拉伸时忽略仅适用于等比的对齐数组
// 入参: object 适配字典
// 返回: annotationIconFit 缩放规则, error 属性错误
func (r *Reader) readAnnotationIconFit(object Object) (annotationIconFit, error) {
	fit := annotationIconFit{when: "A", kind: "P", x: .5, y: .5}
	value, err := r.Resolve(object)
	if err != nil || value == nil {
		return fit, err
	}
	dict, ok := value.(Dictionary)
	if !ok {
		return fit, fmt.Errorf("invalid button icon fit dictionary")
	}
	for _, field := range []struct {
		key     Name
		out     *Name
		allowed string
	}{{"SW", &fit.when, "ABSN"}, {"S", &fit.kind, "AP"}} {
		value, err := r.Resolve(dict[field.key])
		if err != nil {
			return fit, err
		}
		if value == nil {
			continue
		}
		name, ok := value.(Name)
		if !ok || len(name) != 1 || !strings.ContainsRune(field.allowed, rune(name[0])) {
			return fit, fmt.Errorf("invalid button icon fit %s", field.key)
		}
		*field.out = name
	}
	if fit.kind == "P" && dict["A"] != nil {
		values, err := r.numberArray(dict["A"], 2)
		if err != nil {
			return fit, err
		}
		if values[0] < 0 || values[0] > 1 || values[1] < 0 || values[1] > 1 {
			return fit, fmt.Errorf("invalid button icon alignment")
		}
		fit.x, fit.y = values[0], values[1]
	}
	value, err = r.Resolve(dict["FB"])
	if err != nil {
		return fit, err
	}
	if value != nil {
		full, ok := value.(Boolean)
		if !ok {
			return fit, fmt.Errorf("invalid button icon fit bounds")
		}
		fit.full = bool(full)
	}
	return fit, nil
}

// writeAnnotationButtonIcon 按变换后的图标范围缩放，保留源表单及独立资源
// 入参: icon 图标表单, fit 适配规则, frame 可用范围, resources 外观资源, content 内容
// 返回: error 范围、矩阵或资源错误
func (r *Reader) writeAnnotationButtonIcon(icon *Stream, fit annotationIconFit, frame Rectangle, resources Dictionary, content *strings.Builder) error {
	w, h := frame.XMax-frame.XMin, frame.YMax-frame.YMin
	if w <= 0 || h <= 0 {
		return nil
	}
	box, err := r.rectangle(icon.Dictionary["BBox"])
	if err != nil {
		return err
	}
	matrix := Identity()
	if icon.Dictionary["Matrix"] != nil {
		values, err := r.numberArray(icon.Dictionary["Matrix"], 6)
		if err != nil {
			return err
		}
		matrix = Matrix(values)
	}
	minX, minY, maxX, maxY := math.Inf(1), math.Inf(1), math.Inf(-1), math.Inf(-1)
	for _, point := range [4]Point{{box.XMin, box.YMin}, {box.XMax, box.YMin}, {box.XMax, box.YMax}, {box.XMin, box.YMax}} {
		point = matrix.Apply(point)
		minX, minY = math.Min(minX, point.X), math.Min(minY, point.Y)
		maxX, maxY = math.Max(maxX, point.X), math.Max(maxY, point.Y)
	}
	iw, ih := maxX-minX, maxY-minY
	if math.IsNaN(iw) || math.IsNaN(ih) || math.IsInf(iw, 0) || math.IsInf(ih, 0) {
		return fmt.Errorf("invalid transformed button icon bounds")
	}
	if iw == 0 || ih == 0 {
		return nil
	}
	sx, sy := 1.0, 1.0
	if fit.when == "A" || fit.when == "B" && (iw > w || ih > h) || fit.when == "S" && iw < w && ih < h {
		sx, sy = w/iw, h/ih
		if fit.kind == "P" {
			sx = math.Min(sx, sy)
			sy = sx
		}
	}
	x, y := frame.XMin+(w-iw*sx)*fit.x-minX*sx, frame.YMin+(h-ih*sy)*fit.y-minY*sy
	for _, n := range [4]float64{sx, sy, x, y} {
		if math.IsNaN(n) || math.IsInf(n, 0) {
			return fmt.Errorf("invalid button icon fit matrix")
		}
	}
	value, err := r.Resolve(resources["XObject"])
	if err != nil {
		return err
	}
	objects, ok := value.(Dictionary)
	if value != nil && !ok {
		return fmt.Errorf("invalid button appearance XObject resources")
	}
	objects = maps.Clone(objects)
	if objects == nil {
		objects = Dictionary{}
	}
	name := Name("ButtonIcon")
	for i := 1; objects[name] != nil; i++ {
		name = Name(fmt.Sprintf("ButtonIcon%d", i))
	}
	objects[name] = icon
	resources["XObject"] = objects
	content.WriteString("q\n")
	writeAnnotationOperation(content, "re W n", frame.XMin, frame.YMin, w, h)
	writeAnnotationOperation(content, "cm", sx, 0, 0, sy, x, y)
	content.WriteString("/" + escapeAnnotationName(name) + " Do\nQ\n")
	return nil
}
