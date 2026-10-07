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
)

// ThreeDMeasurement 保存三维测量及批注参数，不执行投影或重算声明的测量值
// 空指针表示字段不适用或未提供，Comment不递归展开关联批注
type ThreeDMeasurement struct {
	Subtype         Name
	Name            string
	Plane           *[3]float64
	Anchors         [4]*[3]float64
	Parts           [2]*string
	Directions      [2]*[3]float64
	TextPosition    [3]float64
	TextX           *[3]float64
	TextY           *[3]float64
	TextBox         *[2]int64
	TextSize        float64
	Color           [3]float64
	Value           *float64
	Units           *string
	Precision       int64
	UserText        string
	Degrees         bool
	ExtensionLength float64
	ShowCircle      bool
	Radius          bool
	Comment         Reference
	Dictionary      Dictionary `json:"-"`
}

// ReadThreeDMeasurements 读取视图的MA数组，取消或解析失败时不返回部分结果
// 入参: ctx 取消上下文, object 测量数组或引用，可为空
// 返回: []ThreeDMeasurement 测量数据, error 类型、参数或取消错误
func (r *Reader) ReadThreeDMeasurements(ctx context.Context, object Object) ([]ThreeDMeasurement, error) {
	if ctx == nil || r == nil || r.closed {
		return nil, fmt.Errorf("invalid 3D measurement reader or context")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	value, err := r.Resolve(object)
	if err != nil || value == nil {
		return nil, err
	}
	array, ok := value.(Array)
	if !ok {
		return nil, fmt.Errorf("invalid 3D measurement array")
	}
	var result []ThreeDMeasurement
	for _, object := range array {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		measurement, err := r.ReadThreeDMeasurement(object)
		if err != nil {
			return nil, err
		}
		result = append(result, measurement)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return result, nil
}

// ReadThreeDMeasurement 按PDF标准表326至331解析五类三维测量和批注
// 入参: object 测量字典或引用
// 返回: ThreeDMeasurement 测量参数, error 类型、参数或引用错误
func (r *Reader) ReadThreeDMeasurement(object Object) (ThreeDMeasurement, error) {
	result := ThreeDMeasurement{TextSize: 12, Color: [3]float64{1, 1, 1}, Precision: 3, Degrees: true, ExtensionLength: 60, Radius: true}
	if r == nil || r.closed {
		return result, fmt.Errorf("invalid 3D measurement reader")
	}
	dict, err := r.mediaDictionary(object, false)
	if err != nil {
		return result, err
	}
	result.Dictionary = dict
	kind, err := r.Resolve(dict["Type"])
	if err != nil {
		return result, err
	}
	if kind != nil && kind != Name("3DMeasure") {
		return result, fmt.Errorf("invalid 3D measurement type")
	}
	value, err := r.Resolve(dict["Subtype"])
	if err != nil {
		return result, err
	}
	result.Subtype, _ = value.(Name)
	switch result.Subtype {
	case "LD3", "PD3", "AD3", "RD3", "3DC":
	default:
		return result, &UnsupportedError{Feature: "3D measurement subtype " + string(result.Subtype)}
	}
	for _, field := range []struct {
		key Name
		dst *string
	}{{"TRL", &result.Name}, {"UT", &result.UserText}} {
		text, err := r.threeDMeasureText(dict[field.key], false)
		if err != nil {
			return result, fmt.Errorf("3D measurement %s: %w", field.key, err)
		}
		if text != nil {
			*field.dst = *text
		}
	}
	for _, field := range []struct {
		key      Name
		dst      **[3]float64
		active   bool
		required bool
	}{
		{"A1", &result.Anchors[0], true, true},
		{"A2", &result.Anchors[1], result.Subtype != "3DC", true},
		{"AP", &result.Plane, result.Subtype != "3DC", true},
		{"TY", &result.TextY, result.Subtype != "3DC", true},
		{"TX", &result.TextX, result.Subtype == "AD3" || result.Subtype == "RD3", true},
		{"D1", &result.Directions[0], result.Subtype == "PD3" || result.Subtype == "AD3", true},
		{"D2", &result.Directions[1], result.Subtype == "AD3", true},
		{"A3", &result.Anchors[2], result.Subtype == "RD3", false},
		{"A4", &result.Anchors[3], result.Subtype == "RD3", false},
	} {
		if field.active {
			*field.dst, err = r.threeDMeasureVector(dict[field.key], field.required)
			if err != nil {
				return result, fmt.Errorf("3D measurement %s: %w", field.key, err)
			}
		}
	}
	if (result.Anchors[2] == nil) != (result.Anchors[3] == nil) {
		return result, fmt.Errorf("incomplete 3D measurement arc")
	}
	position, err := r.threeDMeasureVector(dict["TP"], true)
	if err != nil {
		return result, fmt.Errorf("3D measurement TP: %w", err)
	}
	result.TextPosition = *position
	if result.Subtype != "RD3" {
		result.Parts[0], err = r.threeDMeasureText(dict["N1"], false)
		if err != nil {
			return result, err
		}
	}
	if result.Subtype != "3DC" {
		result.Parts[1], err = r.threeDMeasureText(dict["N2"], false)
		if err != nil {
			return result, err
		}
		value, err := r.number(dict["V"])
		if err != nil || math.IsNaN(value) || math.IsInf(value, 0) {
			return result, fmt.Errorf("invalid 3D measurement value")
		}
		result.Value = &value
		precision, err := r.Resolve(dict["P"])
		if err != nil {
			return result, err
		}
		if precision != nil {
			n, ok := precision.(Integer)
			if !ok || n < 0 {
				return result, fmt.Errorf("invalid 3D measurement precision")
			}
			result.Precision = int64(n)
		}
		if result.Subtype != "AD3" {
			result.Units, err = r.threeDMeasureText(dict["U"], true)
			if err != nil {
				return result, err
			}
		}
	}
	if err := r.threeDNumber(dict, "TS", &result.TextSize); err != nil {
		return result, err
	}
	if result.TextSize < 0 {
		return result, fmt.Errorf("invalid 3D measurement text size")
	}
	color, err := r.threeDMeasureVector(dict["C"], false)
	if err != nil {
		return result, fmt.Errorf("3D measurement C: %w", err)
	}
	if color != nil {
		for _, component := range color {
			if component < 0 || component > 1 {
				return result, fmt.Errorf("invalid 3D measurement color")
			}
		}
		result.Color = *color
	}
	for _, field := range []struct {
		key    Name
		dst    *bool
		active bool
	}{{"DR", &result.Degrees, result.Subtype == "AD3"}, {"SC", &result.ShowCircle, result.Subtype == "RD3"}, {"R", &result.Radius, result.Subtype == "RD3"}} {
		if !field.active {
			continue
		}
		value, err := r.Resolve(dict[field.key])
		if err != nil {
			return result, err
		}
		if value != nil {
			flag, ok := value.(Boolean)
			if !ok {
				return result, fmt.Errorf("invalid 3D measurement %s", field.key)
			}
			*field.dst = bool(flag)
		}
	}
	if result.Subtype == "RD3" {
		if err := r.threeDNumber(dict, "EL", &result.ExtensionLength); err != nil {
			return result, err
		}
		if result.ExtensionLength < 0 {
			return result, fmt.Errorf("invalid 3D measurement extension length")
		}
	}
	if result.Subtype == "3DC" {
		value, err := r.Resolve(dict["TB"])
		if err != nil {
			return result, err
		}
		if value != nil {
			array, ok := value.(Array)
			if !ok || len(array) != 2 {
				return result, fmt.Errorf("invalid 3D comment text box")
			}
			result.TextBox = new([2]int64)
			for i, entry := range array {
				value, err := r.Resolve(entry)
				if err != nil {
					return result, err
				}
				n, ok := value.(Integer)
				if !ok || n < 0 {
					return result, fmt.Errorf("invalid 3D comment text box size")
				}
				result.TextBox[i] = int64(n)
			}
		}
	}
	comment, err := r.Resolve(dict["S"])
	if err != nil {
		return result, err
	}
	if comment != nil {
		ref, ok := dict["S"].(Reference)
		if !ok {
			return result, fmt.Errorf("invalid 3D measurement comment reference")
		}
		dict, ok := comment.(Dictionary)
		if !ok {
			return result, fmt.Errorf("invalid 3D measurement comment")
		}
		subtype, err := r.Resolve(dict["Subtype"])
		if err != nil {
			return result, err
		}
		if subtype != Name("Projection") {
			return result, fmt.Errorf("invalid 3D measurement comment subtype")
		}
		result.Comment = ref
	}
	return result, nil
}

// threeDMeasureVector 读取有限的三维坐标或向量，不归一化原始数据
// 入参: object 数组或引用, required 是否必须提供
// 返回: *[3]float64 坐标, error 类型、数值或引用错误
func (r *Reader) threeDMeasureVector(object Object, required bool) (*[3]float64, error) {
	value, err := r.Resolve(object)
	if err != nil || value == nil && !required {
		return nil, err
	}
	array, ok := value.(Array)
	if !ok || len(array) != 3 {
		return nil, fmt.Errorf("invalid 3D measurement vector")
	}
	result := new([3]float64)
	for i, object := range array {
		value, err := r.number(object)
		if err != nil || math.IsNaN(value) || math.IsInf(value, 0) {
			return nil, fmt.Errorf("invalid 3D measurement coordinate")
		}
		result[i] = value
	}
	return result, nil
}

// threeDMeasureText 读取测量文本字符串，保留缺省与显式空字符串的区别
// 入参: object 文本或引用, required 是否必须提供
// 返回: *string 文本, error 类型、编码或引用错误
func (r *Reader) threeDMeasureText(object Object, required bool) (*string, error) {
	value, err := r.Resolve(object)
	if err != nil || value == nil && !required {
		return nil, err
	}
	data, ok := value.(String)
	if !ok {
		return nil, fmt.Errorf("invalid 3D measurement text")
	}
	text, err := DecodeTextString(data)
	if err != nil {
		return nil, err
	}
	return &text, nil
}
