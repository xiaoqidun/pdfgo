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
	"math"
	"strings"
)

// ErrDestinationNotFound 表示命名目标在文档目标表中不存在
var ErrDestinationNotFound = errors.New("destination not found")

// Annotation 保存注解类型、区域及原始字典，不执行动作
type Annotation struct {
	Subtype    Name
	Rect       Rectangle
	Dictionary Dictionary
}

// Destination 保存文档内目标，空参数表示保持阅读器当前值
type Destination struct {
	Page       Reference
	Mode       Name
	Parameters Array
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
		result = append(result, Annotation{Subtype: subtype, Rect: box, Dictionary: dict})
	}
	return result, nil
}

// WalkAnnotationAppearance 解释注解当前外观并映射到页面用户空间
// 入参: ctx 取消上下文, page 所在页面, annotation 注解, visitor 图元访问器
// 返回: error 外观缺失、解析或访问错误
func (r *Reader) WalkAnnotationAppearance(ctx context.Context, page *Page, annotation Annotation, visitor Visitor) error {
	if page.reader != r {
		return fmt.Errorf("page belongs to another reader")
	}
	value, err := r.Resolve(annotation.Dictionary["AP"])
	if err != nil {
		return err
	}
	if value == nil && annotation.Subtype == "Widget" {
		stream, err := r.emptyTextAppearance(annotation)
		if err != nil {
			return err
		}
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
		state, ok := annotation.Dictionary["AS"].(Name)
		if !ok {
			return fmt.Errorf("missing annotation appearance state")
		}
		value, err = r.Resolve(states[state])
		if err != nil {
			return err
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
		values, err := numbers(array, 6)
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
	interpreter := pageInterpreter{reader: r, resources: page.Resources, visitor: visitor, ctx: ctx}
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
	inherited := []Name{"FT", "Ff", "V", "DV", "DA", "Q", "Opt", "MaxLen"}
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
	return result, nil
}

// emptyTextAppearance 按空文本字段的背景和边框生成外观，不猜测缺失文本布局
// 入参: annotation 表单控件
// 返回: *Stream 外观流, error 字段或外观错误
func (r *Reader) emptyTextAppearance(annotation Annotation) (*Stream, error) {
	field, err := r.ReadField(annotation)
	if err != nil {
		return nil, err
	}
	value, empty := field["V"].(String)
	if field["FT"] != Name("Tx") || field["V"] != nil && (!empty || len(value) != 0) || field["RV"] != nil {
		return nil, &UnsupportedError{Feature: "widget appearance generation"}
	}
	object, err := r.Resolve(annotation.Dictionary["MK"])
	if err != nil {
		return nil, err
	}
	mk, ok := object.(Dictionary)
	if object != nil && !ok {
		return nil, fmt.Errorf("invalid widget appearance characteristics")
	}
	w, h := annotation.Rect.XMax-annotation.Rect.XMin, annotation.Rect.YMax-annotation.Rect.YMin
	if w <= 0 || h <= 0 {
		return nil, fmt.Errorf("invalid widget bounds")
	}
	var content strings.Builder
	for _, key := range []Name{"BG", "BC"} {
		object, err := r.Resolve(mk[key])
		if err != nil {
			return nil, err
		}
		if object == nil {
			continue
		}
		values, ok := object.(Array)
		if !ok {
			return nil, fmt.Errorf("invalid widget color")
		}
		if len(values) == 0 {
			continue
		}
		operators := map[int]string{1: "g", 3: "rg", 4: "k"}
		op, ok := operators[len(values)]
		if !ok {
			return nil, fmt.Errorf("invalid widget color components")
		}
		if key == "BC" {
			op = strings.ToUpper(op)
		}
		for _, value := range values {
			n, err := r.number(value)
			if err != nil {
				return nil, err
			}
			fmt.Fprintf(&content, "%g ", n)
		}
		fmt.Fprintf(&content, "%s\n", op)
		if key == "BG" {
			fmt.Fprintf(&content, "0 0 %g %g re f\n", w, h)
			continue
		}
		object, err = r.Resolve(annotation.Dictionary["BS"])
		if err != nil {
			return nil, err
		}
		style, ok := object.(Dictionary)
		if object != nil && !ok {
			return nil, fmt.Errorf("invalid widget border style")
		}
		width := 1.0
		if style["W"] != nil {
			width, err = r.number(style["W"])
			if err != nil || width < 0 {
				return nil, fmt.Errorf("invalid widget border width")
			}
		}
		if style["S"] != nil && style["S"] != Name("S") {
			return nil, &UnsupportedError{Feature: "generated widget border style"}
		}
		if width > 0 {
			fmt.Fprintf(&content, "%g w %g %g %g %g re S\n", width, width/2, width/2, math.Max(0, w-width), math.Max(0, h-width))
		}
	}
	return &Stream{Dictionary: Dictionary{"Type": Name("XObject"), "Subtype": Name("Form"), "BBox": Array{Integer(0), Integer(0), Real(w), Real(h)}}, Data: []byte(content.String()), reader: r}, nil
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
	mode, ok := array[1].(Name)
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
		if parameters[n] != nil {
			if _, err := r.number(parameters[n]); err != nil {
				return Destination{}, err
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
		seen := map[Reference]bool{}
		var walk func(Object, int) error
		walk = func(object Object, depth int) error {
			if object == nil {
				return nil
			}
			if depth > 256 {
				return fmt.Errorf("destination name tree depth exceeded")
			}
			if ref, ok := object.(Reference); ok {
				if seen[ref] {
					return fmt.Errorf("repeated destination name tree node")
				}
				seen[ref] = true
			}
			value, err := r.Resolve(object)
			if err != nil {
				return err
			}
			node, ok := value.(Dictionary)
			if !ok {
				return fmt.Errorf("invalid destination name tree node")
			}
			value, err = r.Resolve(node["Names"])
			if err != nil {
				return err
			}
			if value != nil {
				items, ok := value.(Array)
				if !ok || len(items)%2 != 0 {
					return fmt.Errorf("invalid destination name pairs")
				}
				for n := 0; n < len(items); n += 2 {
					name, ok := items[n].(String)
					if !ok {
						return fmt.Errorf("invalid destination name")
					}
					if _, exists := result[string(name)]; exists {
						return fmt.Errorf("duplicate destination name")
					}
					result[string(name)] = items[n+1]
				}
			}
			value, err = r.Resolve(node["Kids"])
			if err != nil {
				return err
			}
			if value != nil {
				kids, ok := value.(Array)
				if !ok {
					return fmt.Errorf("invalid destination name tree children")
				}
				for _, kid := range kids {
					if err := walk(kid, depth+1); err != nil {
						return err
					}
				}
			}
			return nil
		}
		if err := walk(dict["Dests"], 0); err != nil {
			return err
		}
	}
	r.destinations = result
	r.legacyDestinations = legacy
	return nil
}
