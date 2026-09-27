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
)

// deviceNSpace 保存专色分量、备用空间及着色函数
type deviceNSpace struct {
	alternate     *ColorSpace
	transform     *gradientFunction
	components    int
	input, output []float64
	expressions   []affineValue
}

// paint 按专色浓度生成备用空间颜色并保留原始分量
// 入参: tints 各分量专色浓度, intent 渲染意图
// 返回: Paint 颜色, error 颜色转换错误
func (s *deviceNSpace) paint(tints []float64, intent Name) (Paint, error) {
	values := s.values(tints...)
	rgb, err := s.alternate.RGB(values[:s.alternate.Components()], intent)
	if err != nil {
		return Paint{}, err
	}
	paint := Paint{RGB: rgb, Space: s.alternate, Values: values}
	if s.alternate.Model == "DeviceCMYK" {
		paint.CMYK = &values
	}
	return paint, nil
}

// values 将专色浓度映射到备用空间的有效分量区间
// 入参: tints 各分量专色浓度
// 返回: [4]float64 备用空间分量
func (s *deviceNSpace) values(tints ...float64) [4]float64 {
	var values [4]float64
	if s.transform != nil {
		values = s.transform.value(math.Max(0, math.Min(1, tints[0])))
	} else {
		for c, expression := range s.expressions {
			values[c] = expression[0]
			for j, coefficient := range expression[1:] {
				value := math.Max(s.input[2*j], math.Min(s.input[2*j+1], math.Max(0, math.Min(1, tints[j]))))
				values[c] += coefficient * value
			}
		}
		values = clipGradientValue(values, s.output)
	}
	for c := 0; c < s.alternate.Components(); c++ {
		values[c] = math.Max(0, math.Min(1, values[c]))
	}
	return values
}

// readSingleDeviceN 解析单分量DeviceN颜色空间，不将专色浓度解释为设备灰度
// 入参: space 颜色空间数组
// 返回: *deviceNSpace 着色定义, error 解析或未支持的分量错误
func (r *Reader) readSingleDeviceN(space Array) (*deviceNSpace, error) {
	if len(space) != 4 && len(space) != 5 {
		return nil, fmt.Errorf("invalid DeviceN color space")
	}
	object, err := r.Resolve(space[1])
	if err != nil {
		return nil, err
	}
	names, ok := object.(Array)
	if !ok || len(names) == 0 {
		return nil, fmt.Errorf("invalid DeviceN colorants")
	}
	if len(names) != 1 {
		return nil, &UnsupportedError{Feature: "DeviceN component count"}
	}
	name, ok := names[0].(Name)
	if !ok || name == "All" {
		return nil, fmt.Errorf("invalid DeviceN colorant")
	}
	if name == "None" {
		return nil, &UnsupportedError{Feature: "DeviceN None colorant"}
	}
	alternate, err := r.readColorSpace(space[2])
	if err != nil {
		return nil, err
	}
	transform, err := r.readGradientFunction(space[3], alternate.Components(), 0)
	if err != nil {
		return nil, err
	}
	return &deviceNSpace{alternate: alternate, transform: transform, components: 1}, nil
}

// readDeviceN 解析单分量函数或多分量仿射专色变换
// 入参: space 颜色空间数组
// 返回: *deviceNSpace 着色定义, error 格式或能力错误
func (r *Reader) readDeviceN(space Array) (*deviceNSpace, error) {
	if len(space) != 4 && len(space) != 5 {
		return nil, fmt.Errorf("invalid DeviceN color space")
	}
	object, err := r.Resolve(space[1])
	if err != nil {
		return nil, err
	}
	if names, ok := object.(Array); ok && len(names) == 1 {
		return r.readSingleDeviceN(space)
	}
	alternate, input, output, expressions, err := r.readDeviceNAffine(space)
	if err != nil {
		return nil, err
	}
	return &deviceNSpace{alternate: alternate, components: len(input) / 2, input: input, output: output, expressions: expressions}, nil
}

// readDeviceNAffine 读取多分量专色的备用空间、区间和仿射着色函数
// 入参: space 多色定义
// 返回: *ColorSpace 备用空间, []float64 输入区间, []float64 输出区间, []affineValue 输出表达式, error 解析错误
func (r *Reader) readDeviceNAffine(space Array) (*ColorSpace, []float64, []float64, []affineValue, error) {
	if len(space) != 4 && len(space) != 5 {
		return nil, nil, nil, nil, fmt.Errorf("invalid DeviceN color space")
	}
	value, err := r.Resolve(space[1])
	if err != nil {
		return nil, nil, nil, nil, err
	}
	names, ok := value.(Array)
	if !ok || len(names) == 0 {
		return nil, nil, nil, nil, fmt.Errorf("invalid DeviceN colorants")
	}
	if len(names) > 4 {
		return nil, nil, nil, nil, &UnsupportedError{Feature: "DeviceN component count"}
	}
	seen := map[Name]bool{}
	for _, value := range names {
		name, ok := value.(Name)
		if !ok || seen[name] || name == "All" {
			return nil, nil, nil, nil, fmt.Errorf("invalid DeviceN colorant")
		}
		if name == "None" {
			return nil, nil, nil, nil, &UnsupportedError{Feature: "DeviceN None colorant"}
		}
		seen[name] = true
	}
	alternate, err := r.readColorSpace(space[2])
	if err != nil {
		return nil, nil, nil, nil, err
	}
	value, err = r.Resolve(space[3])
	if err != nil {
		return nil, nil, nil, nil, err
	}
	stream, ok := value.(*Stream)
	if !ok || stream.Dictionary["FunctionType"] != Integer(4) {
		return nil, nil, nil, nil, &UnsupportedError{Feature: "DeviceN tint function"}
	}
	input, err := r.numberArray(stream.Dictionary["Domain"], 2*len(names))
	if err != nil {
		return nil, nil, nil, nil, err
	}
	output, err := r.numberArray(stream.Dictionary["Range"], 2*alternate.Components())
	if err != nil {
		return nil, nil, nil, nil, err
	}
	for index, bounds := range [][]float64{input, output} {
		for i := 0; i < len(bounds); i += 2 {
			if bounds[i] > bounds[i+1] || index == 0 && bounds[i] == bounds[i+1] {
				return nil, nil, nil, nil, fmt.Errorf("invalid DeviceN tint bounds")
			}
		}
	}
	data, err := stream.Decode()
	if err != nil {
		return nil, nil, nil, nil, err
	}
	expressions, err := affineCalculator(data, len(names), alternate.Components())
	if err != nil {
		return nil, nil, nil, nil, err
	}
	return alternate, input, output, expressions, nil
}
