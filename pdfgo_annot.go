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
	"errors"
	"fmt"
	"maps"
	"math"
)

// ErrDestinationNotFound 表示命名目标在文档目标表中不存在
var ErrDestinationNotFound = errors.New("destination not found")

// Annotation 保存注解类型、区域及原始字典，不执行动作
type Annotation struct {
	Reference  Reference
	Subtype    Name
	Rect       Rectangle
	Dictionary Dictionary
}

// PopupAnnotation 保存弹出批注及其父批注引用，Dictionary中的文字信息已按父批注覆盖
type PopupAnnotation struct {
	Annotation
	Parent Reference
	Open   bool
}

// Destination 保存文档内目标，空参数表示保持阅读器当前值
type Destination struct {
	Page       Reference
	Mode       Name
	Parameters Array
}

// ReadPopupAnnotation 读取弹出批注，继承父批注的Contents、M、C和T，不执行界面操作
// 入参: object 弹出批注字典或间接引用
// 返回: PopupAnnotation 弹出批注信息, error 解析错误
func (r *Reader) ReadPopupAnnotation(object Object) (PopupAnnotation, error) {
	var popup PopupAnnotation
	value, err := r.Resolve(object)
	if err != nil {
		return popup, err
	}
	dict, ok := value.(Dictionary)
	if !ok || dict["Subtype"] != Name("Popup") {
		return popup, fmt.Errorf("invalid popup annotation")
	}
	popup.Reference, _ = object.(Reference)
	popup.Subtype = "Popup"
	popup.Rect, err = r.rectangle(dict["Rect"])
	if err != nil {
		return popup, err
	}
	popup.Dictionary = maps.Clone(dict)
	if value, err = r.Resolve(dict["Open"]); err != nil {
		return popup, err
	} else if value != nil {
		open, ok := value.(Boolean)
		if !ok {
			return popup, fmt.Errorf("invalid popup open state")
		}
		popup.Open = bool(open)
	}
	if dict["Parent"] != nil {
		parent, ok := dict["Parent"].(Reference)
		if !ok {
			return popup, fmt.Errorf("invalid popup parent reference")
		}
		value, err = r.Resolve(parent)
		if err != nil {
			return popup, err
		}
		if value == nil {
			return popup, nil
		}
		parentDict, ok := value.(Dictionary)
		if !ok || parentDict["Subtype"] == Name("Popup") {
			return popup, fmt.Errorf("invalid popup parent annotation")
		}
		if _, ok := parentDict["Subtype"].(Name); !ok {
			return popup, fmt.Errorf("invalid popup parent subtype")
		}
		popup.Parent = parent
		for _, key := range []Name{"Contents", "M", "C", "T"} {
			delete(popup.Dictionary, key)
			if value := parentDict[key]; value != nil {
				popup.Dictionary[key] = value
			}
		}
	}
	return popup, nil
}

// Annotations 按页面顺序读取注解，外观和动作由调用方解释
// 返回: []Annotation 注解信息, error 错误信息
func (p *Page) Annotations() ([]Annotation, error) {
	value, err := p.reader.Resolve(p.Dictionary["Annots"])
	if err != nil || value == nil {
		return nil, err
	}
	array, ok := value.(Array)
	if !ok {
		return nil, fmt.Errorf("invalid page annotations")
	}
	result := make([]Annotation, 0, len(array))
	for _, object := range array {
		value, err := p.reader.Resolve(object)
		if err != nil {
			return nil, err
		}
		dict, ok := value.(Dictionary)
		if !ok {
			return nil, fmt.Errorf("invalid annotation dictionary")
		}
		subtype, ok := dict["Subtype"].(Name)
		if !ok {
			return nil, fmt.Errorf("missing annotation subtype")
		}
		box, err := p.reader.rectangle(dict["Rect"])
		if err != nil {
			return nil, err
		}
		reference, _ := object.(Reference)
		result = append(result, Annotation{Reference: reference, Subtype: subtype, Rect: box, Dictionary: dict})
	}
	return result, nil
}

// WalkAnnotationAppearance 解释注解当前外观并映射到页面用户空间
// 入参: ctx 取消上下文, page 所在页面, annotation 注解, visitor 图元访问器
// 返回: error 外观缺失、解析或访问错误
func (r *Reader) WalkAnnotationAppearance(ctx context.Context, page *Page, annotation Annotation, visitor Visitor) error {
	if object := annotation.Dictionary["OC"]; object != nil {
		visible := visitor.OptionalContent
		if visible == nil {
			visible = r.OptionalContentVisible
		}
		show, err := visible(object)
		if err != nil || !show {
			return err
		}
	}
	if page.reader != r {
		return fmt.Errorf("page belongs to another reader")
	}
	if annotation.Rect.XMin == annotation.Rect.XMax || annotation.Rect.YMin == annotation.Rect.YMax {
		return ctx.Err()
	}
	if annotation.Subtype == "Popup" {
		_, err := r.ReadPopupAnnotation(annotation.Dictionary)
		return err
	}
	value, err := r.Resolve(annotation.Dictionary["AP"])
	if err != nil {
		return err
	}
	if value == nil && annotation.Subtype == "Widget" {
		stream, err := r.widgetAppearance(ctx, page, annotation)
		if err != nil {
			return err
		}
		value = Dictionary{"N": stream}
	}
	if value == nil && annotation.Subtype == "FreeText" {
		stream, err := r.variableTextAppearance(ctx, page, annotation)
		if err != nil {
			return err
		}
		value = Dictionary{"N": stream}
	}
	if value == nil && annotation.Subtype == "Link" {
		stream, err := r.linkAppearance(annotation)
		if err != nil {
			return err
		}
		value = Dictionary{"N": stream}
	}
	if value == nil && (annotation.Subtype == "Text" || annotation.Subtype == "FileAttachment" || annotation.Subtype == "Sound" || annotation.Subtype == "Caret") {
		stream, err := r.iconAppearance(annotation)
		if err != nil {
			return err
		}
		value = Dictionary{"N": stream}
	}
	if value == nil && annotation.Subtype == "Ink" {
		stream, err := r.inkAppearance(annotation)
		if err != nil {
			return err
		}
		value = Dictionary{"N": stream}
	}
	if value == nil && annotation.Subtype == "Line" {
		stream, err := r.lineAppearance(annotation)
		if err != nil {
			return err
		}
		value = Dictionary{"N": stream}
	}
	if value == nil && (annotation.Subtype == "Highlight" || annotation.Subtype == "Underline" || annotation.Subtype == "StrikeOut" || annotation.Subtype == "Squiggly") {
		stream, err := r.textMarkupAppearance(annotation)
		if err != nil {
			return err
		}
		value = Dictionary{"N": stream}
	}
	if value == nil && (annotation.Subtype == "Square" || annotation.Subtype == "Circle" || annotation.Subtype == "Polygon" || annotation.Subtype == "PolyLine") {
		stream, err := r.shapeAppearance(annotation)
		if err != nil {
			return err
		}
		value = Dictionary{"N": stream}
	}
	if value == nil && annotation.Subtype == "Screen" {
		return nil
	}
	if value == nil && annotation.Subtype == "Movie" {
		movie, err := r.ReadMovie(annotation.Dictionary["Movie"])
		if err != nil {
			return err
		}
		if movie.Poster == Boolean(true) {
			return &UnsupportedError{Feature: "poster extraction from movie"}
		}
		poster, ok := movie.Poster.(*Stream)
		if !ok {
			return nil
		}
		stream := &Stream{Dictionary: Dictionary{"Subtype": Name("Form"), "BBox": Array{Integer(0), Integer(0), Integer(1), Integer(1)}, "Resources": Dictionary{"XObject": Dictionary{"Poster": poster}}}, Data: []byte("/Poster Do"), reader: r}
		value = Dictionary{"N": stream}
	}
	appearance, ok := value.(Dictionary)
	if !ok {
		return &UnsupportedError{Feature: "annotation appearance"}
	}
	value, err = r.Resolve(appearance["N"])
	if err != nil {
		return err
	}
	if states, ok := value.(Dictionary); ok {
		selected, err := r.Resolve(annotation.Dictionary["AS"])
		if err != nil {
			return err
		}
		state, ok := selected.(Name)
		if !ok {
			return fmt.Errorf("missing annotation appearance state")
		}
		value, err = r.Resolve(states[state])
		if err != nil {
			return err
		}
		if value == nil {
			return nil
		}
	}
	stream, ok := value.(*Stream)
	if !ok || stream.Dictionary["Subtype"] != Name("Form") {
		return &UnsupportedError{Feature: "annotation appearance stream"}
	}
	box, err := r.rectangle(stream.Dictionary["BBox"])
	if err != nil {
		return err
	}
	matrix := Identity()
	if stream.Dictionary["Matrix"] != nil {
		value, err := r.Resolve(stream.Dictionary["Matrix"])
		if err != nil {
			return err
		}
		array, ok := value.(Array)
		if !ok {
			return fmt.Errorf("invalid appearance matrix")
		}
		values, err := r.numberArray(array, 6)
		if err != nil {
			return err
		}
		matrix = Matrix(values)
	}
	minX, minY, maxX, maxY := math.Inf(1), math.Inf(1), math.Inf(-1), math.Inf(-1)
	for _, point := range []Point{{box.XMin, box.YMin}, {box.XMax, box.YMin}, {box.XMax, box.YMax}, {box.XMin, box.YMax}} {
		point = matrix.Apply(point)
		minX, minY = math.Min(minX, point.X), math.Min(minY, point.Y)
		maxX, maxY = math.Max(maxX, point.X), math.Max(maxY, point.Y)
	}
	if math.IsInf(minX, 0) || math.IsInf(minY, 0) || math.IsInf(maxX, 0) || math.IsInf(maxY, 0) || math.IsNaN(minX) || math.IsNaN(minY) || math.IsNaN(maxX) || math.IsNaN(maxY) || minX == maxX || minY == maxY {
		return fmt.Errorf("invalid appearance bounds")
	}
	rect := annotation.Rect
	scaleX, scaleY := (rect.XMax-rect.XMin)/(maxX-minX), (rect.YMax-rect.YMin)/(maxY-minY)
	interpreter := pageInterpreter{reader: r, resources: page.Resources, visitor: visitor, ctx: ctx, bounds: rect}
	interpreter.patternMatrix = Identity()
	interpreter.state = graphicsState{matrix: Matrix{scaleX, 0, 0, scaleY, rect.XMin - minX*scaleX, rect.YMin - minY*scaleY}, hscale: 1, fillSpace: "DeviceGray", strokeSpace: "DeviceGray", style: Style{Fill: Paint{Alpha: 1}, Stroke: Paint{Alpha: 1}, LineWidth: 1, MiterLimit: 10}}
	return interpreter.form(stream)
}

// ReadField 读取控件字段并补齐可继承属性，不修改原始注解字典
// 入参: annotation 表单控件
// 返回: Dictionary 字段属性, error 字段或继承链错误
func (r *Reader) ReadField(annotation Annotation) (Dictionary, error) {
	if annotation.Subtype != "Widget" {
		return nil, fmt.Errorf("annotation is not a widget")
	}
	result := make(Dictionary, len(annotation.Dictionary))
	for key, value := range annotation.Dictionary {
		result[key] = value
	}
	inherited := []Name{"FT", "Ff", "V", "DV", "DA", "Q", "Opt", "MaxLen", "I", "TI", "RV", "DS"}
	for _, key := range inherited {
		delete(result, key)
	}
	seen := map[Reference]bool{}
	current := annotation.Dictionary
	for current != nil {
		for _, key := range inherited {
			if result[key] == nil {
				value, err := r.Resolve(current[key])
				if err != nil {
					return nil, err
				}
				result[key] = value
			}
		}
		parent := current["Parent"]
		if parent == nil {
			break
		}
		ref, ok := parent.(Reference)
		if !ok || seen[ref] {
			return nil, fmt.Errorf("invalid field parent chain")
		}
		seen[ref] = true
		value, err := r.Resolve(parent)
		if err != nil {
			return nil, err
		}
		current, ok = value.(Dictionary)
		if !ok {
			return nil, fmt.Errorf("invalid field parent")
		}
	}
	defaults, err := r.formDefaults()
	if err != nil {
		return nil, err
	}
	for _, key := range []Name{"DA", "Q"} {
		if result[key] == nil {
			result[key], err = r.Resolve(defaults[key])
			if err != nil {
				return nil, err
			}
		}
	}
	return result, nil
}

// BaseURI 读取文档声明的链接基准地址，不访问外部资源
// 返回: string 基准地址，未声明时为空, error 错误信息
func (r *Reader) BaseURI() (string, error) {
	root, err := r.Resolve(r.Trailer["Root"])
	if err != nil {
		return "", err
	}
	catalog, ok := root.(Dictionary)
	if !ok {
		return "", fmt.Errorf("invalid catalog")
	}
	value, err := r.Resolve(catalog["URI"])
	if err != nil || value == nil {
		return "", err
	}
	dict, ok := value.(Dictionary)
	if !ok {
		return "", fmt.Errorf("invalid URI dictionary")
	}
	value, err = r.Resolve(dict["Base"])
	if err != nil || value == nil {
		return "", err
	}
	base, ok := value.(String)
	if !ok {
		return "", fmt.Errorf("invalid base URI")
	}
	return string(base), nil
}

// ReadDestination 解析直接目标或名称树中的命名目标，不跳转页面
// 入参: object 目标数组、名称、字符串或引用
// 返回: Destination 跳转目标, error 错误信息
func (r *Reader) ReadDestination(object Object) (Destination, error) {
	value, err := r.Resolve(object)
	if err != nil {
		return Destination{}, err
	}
	var name string
	named, legacy := false, false
	switch v := value.(type) {
	case Name:
		name = string(v)
		named, legacy = true, true
	case String:
		name = string(v)
		named = true
	}
	if named {
		if err := r.readDestinations(); err != nil {
			return Destination{}, err
		}
		target := r.destinations[name]
		if legacy {
			target = r.legacyDestinations[Name(name)]
		}
		if target == nil {
			return Destination{}, fmt.Errorf("%w: %q", ErrDestinationNotFound, name)
		}
		value, err = r.Resolve(target)
		if err != nil {
			return Destination{}, err
		}
	}
	if dict, ok := value.(Dictionary); ok {
		value, err = r.Resolve(dict["D"])
		if err != nil {
			return Destination{}, err
		}
	}
	array, ok := value.(Array)
	if !ok || len(array) < 2 {
		return Destination{}, fmt.Errorf("invalid or missing destination")
	}
	page, ok := array[0].(Reference)
	if !ok {
		return Destination{}, fmt.Errorf("invalid destination page")
	}
	modeValue, err := r.Resolve(array[1])
	if err != nil {
		return Destination{}, err
	}
	mode, ok := modeValue.(Name)
	counts := map[Name]int{"XYZ": 3, "Fit": 0, "FitH": 1, "FitV": 1, "FitR": 4, "FitB": 0, "FitBH": 1, "FitBV": 1}
	count, known := counts[mode]
	if !ok || !known || len(array) != count+2 {
		return Destination{}, fmt.Errorf("invalid destination mode or parameters")
	}
	parameters := make(Array, count)
	for n, v := range array[2:] {
		parameters[n], err = r.Resolve(v)
		if err != nil {
			return Destination{}, err
		}
		if parameters[n] == nil && mode == "FitR" {
			return Destination{}, fmt.Errorf("null rectangle destination parameter")
		}
		if parameters[n] != nil {
			value, err := r.number(parameters[n])
			if err != nil {
				return Destination{}, err
			}
			if math.IsNaN(value) || math.IsInf(value, 0) || mode == "XYZ" && n == 2 && value < 0 {
				return Destination{}, fmt.Errorf("invalid destination parameter")
			}
		}
	}
	return Destination{Page: page, Mode: mode, Parameters: parameters}, nil
}

// readDestinations 缓存旧式目标字典和名称树，检测递归引用
// 返回: error 错误信息
func (r *Reader) readDestinations() error {
	if r.destinations != nil {
		return nil
	}
	root, err := r.Resolve(r.Trailer["Root"])
	if err != nil {
		return err
	}
	catalog, ok := root.(Dictionary)
	if !ok {
		return fmt.Errorf("invalid catalog")
	}
	result := map[string]Object{}
	var legacy Dictionary
	value, err := r.Resolve(catalog["Dests"])
	if err != nil {
		return err
	}
	if value != nil {
		dict, ok := value.(Dictionary)
		if !ok {
			return fmt.Errorf("invalid destinations dictionary")
		}
		legacy = dict
	}
	names, err := r.Resolve(catalog["Names"])
	if err != nil {
		return err
	}
	if names != nil {
		dict, ok := names.(Dictionary)
		if !ok {
			return fmt.Errorf("invalid names dictionary")
		}
		if err := r.WalkNameTree(context.Background(), dict["Dests"], func(name string, value Object) error {
			result[name] = value
			return nil
		}); err != nil {
			return err
		}
	}
	r.destinations = result
	r.legacyDestinations = legacy
	return nil
}
