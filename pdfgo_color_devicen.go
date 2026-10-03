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
	process       *ProcessColorants
	lab           *labSpace
	transform     *gradientFunction
	components    int
	none          bool
	input, output []float64
	expressions   []affineValue
	program       []calculatorInstruction
	sampled       *sampledFunction
	nested        *tintAlternate
}

// outputComponents 返回着色函数的输出数，不使用嵌套变换后的显示分量数
// 返回: int 备用空间输入数
func (s *deviceNSpace) outputComponents() int {
	if s.nested != nil {
		return s.nested.components
	}
	return s.alternate.Components()
}

// paint 按专色浓度生成备用空间颜色并保留原始分量
// 入参: tints 各分量专色浓度, intent 渲染意图
// 返回: Paint 颜色, error 颜色转换错误
func (s *deviceNSpace) paint(tints []float64, intent Name) (Paint, error) {
	if s.none {
		_, err := s.values(tints...)
		return Paint{SourceSpace: "DeviceN", None: true}, err
	}
	values, err := s.values(tints...)
	if err != nil {
		return Paint{}, err
	}
	rgb, err := s.alternate.RGB(values[:s.alternate.Components()], intent)
	if err != nil {
		return Paint{}, err
	}
	paint := Paint{SourceSpace: "DeviceN", RGB: rgb, Space: s.alternate, Values: values, Process: s.process}
	if s.alternate.Model == "DeviceCMYK" && !s.alternate.Calibrated() {
		paint.CMYK = &values
	}
	return paint, nil
}

// values 将专色浓度映射到备用空间的有效分量区间
// 入参: tints 各分量专色浓度
// 返回: [4]float64 备用空间分量, error 着色函数错误
func (s *deviceNSpace) values(tints ...float64) ([4]float64, error) {
	var values [4]float64
	if len(tints) != s.components {
		return values, fmt.Errorf("invalid DeviceN component count")
	}
	for _, tint := range tints {
		if math.IsNaN(tint) || math.IsInf(tint, 0) {
			return values, fmt.Errorf("invalid DeviceN tint")
		}
	}
	if s.none {
		return values, nil
	}
	if s.sampled != nil {
		var buffer [32]float64
		input := buffer[:min(s.components, len(buffer))]
		if s.components > len(buffer) {
			input = make([]float64, s.components)
		}
		for i, tint := range tints {
			input[i] = math.Max(0, math.Min(1, tint))
		}
		s.sampled.evaluate(input, values[:s.outputComponents()])
	} else if s.transform != nil {
		var err error
		values, err = s.transform.evaluate(math.Max(0, math.Min(1, tints[0])))
		if err != nil {
			return values, err
		}
	} else if s.program != nil {
		var buffer [32]float64
		input := buffer[:min(s.components, len(buffer))]
		if s.components > len(buffer) {
			input = make([]float64, s.components)
		}
		for i, value := range tints {
			input[i] = math.Max(s.input[2*i], math.Min(s.input[2*i+1], math.Max(0, math.Min(1, value))))
		}
		if err := evaluateCalculator(s.program, input, values[:s.outputComponents()]); err != nil {
			return values, err
		}
		values = clipGradientValue(values, s.output)
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
	if s.nested != nil {
		return s.nested.values(values[:s.nested.components])
	}
	if s.lab != nil {
		color := s.lab.color(values[0], values[1], values[2])
		return [4]float64{float64(color.R) / 65535, float64(color.G) / 65535, float64(color.B) / 65535}, nil
	}
	if profile := s.alternate.profile; profile != nil && profile.ranges != nil {
		return profile.normalize(values[:s.alternate.Components()]), nil
	}
	for c := 0; c < s.alternate.Components(); c++ {
		values[c] = math.Max(0, math.Min(1, values[c]))
	}
	return values, nil
}

// readSingleDeviceN 解析单分量DeviceN颜色空间，不将专色浓度解释为设备灰度
// 入参: space 颜色空间数组, effective 是否已按资源校验并替换, depth 嵌套深度
// 返回: *deviceNSpace 着色定义, error 解析或未支持的分量错误
func (r *Reader) readSingleDeviceN(space Array, effective bool, depth int) (*deviceNSpace, error) {
	names, none, err := r.deviceNColorants(space)
	if err != nil {
		return nil, err
	}
	if len(names) != 1 {
		return nil, &UnsupportedError{Feature: "DeviceN component count"}
	}
	if none {
		if err := r.ignoredTintSpace(space); err != nil {
			return nil, err
		}
		return &deviceNSpace{alternate: &ColorSpace{Model: "DeviceRGB"}, components: 1, none: true}, nil
	}
	alternate, err := r.readTintAlternate(space[2], effective, depth+1)
	if err != nil {
		return nil, err
	}
	transform, err := r.readGradientFunction(space[3], alternate.components, 0)
	if err != nil {
		return nil, err
	}
	result := &deviceNSpace{alternate: alternate.space, lab: alternate.lab, transform: transform, components: 1, none: alternate.none}
	if alternate.separation != nil || alternate.deviceN != nil {
		result.nested, result.none = alternate, alternate.none
	}
	return result, nil
}

// readDeviceN 解析单分量函数或多分量采样与计算器专色变换
// 入参: space 颜色空间数组
// 返回: *deviceNSpace 着色定义, error 格式或能力错误
func (r *Reader) readDeviceN(space Array) (*deviceNSpace, error) {
	return r.readDeviceNSpace(space, false, 0)
}

// readDeviceNSpace 读取原始多色或已校验的默认空间替换，限制嵌套深度
// 入参: space 多色数组, effective 是否已按资源校验并替换, depth 嵌套深度
// 返回: *deviceNSpace 专色变换, error 定义或函数错误
func (r *Reader) readDeviceNSpace(space Array, effective bool, depth int) (*deviceNSpace, error) {
	if depth >= 64 {
		return nil, fmt.Errorf("color space recursion limit exceeded")
	}
	if len(space) != 4 && len(space) != 5 || space[0] != Name("DeviceN") {
		return nil, fmt.Errorf("invalid DeviceN color space")
	}
	object, err := r.Resolve(space[1])
	if err != nil {
		return nil, err
	}
	var result *deviceNSpace
	if names, ok := object.(Array); ok && len(names) == 1 {
		result, err = r.readSingleDeviceN(space, effective, depth)
	} else {
		result, err = r.readMultiDeviceN(space, effective, depth)
	}
	if err != nil {
		return nil, err
	}
	if err := r.applyNChannelProcess(space, result); err != nil {
		return nil, err
	}
	return result, nil
}

// readMultiDeviceN 读取多分量专色的备用空间、区间和着色函数
// 入参: space 多色定义, effective 是否已按资源校验并替换, depth 嵌套深度
// 返回: *deviceNSpace 专色变换, error 解析错误
func (r *Reader) readMultiDeviceN(space Array, effective bool, depth int) (*deviceNSpace, error) {
	names, none, err := r.deviceNColorants(space)
	if err != nil {
		return nil, err
	}
	if none {
		if err := r.ignoredTintSpace(space); err != nil {
			return nil, err
		}
		return &deviceNSpace{alternate: &ColorSpace{Model: "DeviceRGB"}, components: len(names), none: true}, nil
	}
	alternate, err := r.readTintAlternate(space[2], effective, depth+1)
	if err != nil {
		return nil, err
	}
	value, err := r.Resolve(space[3])
	if err != nil {
		return nil, err
	}
	stream, ok := value.(*Stream)
	var kind Object
	if ok {
		if stream == nil {
			return nil, fmt.Errorf("invalid DeviceN tint function")
		}
		kind, err = r.Resolve(stream.Dictionary["FunctionType"])
		if err != nil {
			return nil, err
		}
	}
	if kind == Integer(0) {
		function, err := r.readSampledFunction(stream, len(names), alternate.components)
		if err != nil {
			return nil, err
		}
		result := &deviceNSpace{alternate: alternate.space, lab: alternate.lab, components: len(names), sampled: function, none: alternate.none}
		if alternate.separation != nil || alternate.deviceN != nil {
			result.nested, result.none = alternate, alternate.none
		}
		return result, nil
	}
	if !ok || kind != Integer(4) {
		return nil, &UnsupportedError{Feature: "DeviceN tint function"}
	}
	input, err := r.numberArray(stream.Dictionary["Domain"], 2*len(names))
	if err != nil {
		return nil, err
	}
	output, err := r.numberArray(stream.Dictionary["Range"], 2*alternate.components)
	if err != nil {
		return nil, err
	}
	for _, bounds := range [][]float64{input, output} {
		for i := 0; i < len(bounds); i += 2 {
			if bounds[i] > bounds[i+1] {
				return nil, fmt.Errorf("invalid DeviceN tint bounds")
			}
		}
	}
	data, err := stream.Decode()
	if err != nil {
		return nil, err
	}
	program, err := compileCalculator(data)
	if err != nil {
		return nil, err
	}
	s := &deviceNSpace{alternate: alternate.space, lab: alternate.lab, components: len(names), input: input, output: output, none: alternate.none}
	if alternate.separation != nil || alternate.deviceN != nil {
		s.nested, s.none = alternate, alternate.none
	}
	s.expressions, err = affineCalculator(data, len(names), alternate.components)
	if err != nil {
		s.program = program
	}
	inputs := make([]float64, len(names))
	var outputs [4]float64
	for i := range names {
		inputs[i] = input[2*i]
	}
	if err := evaluateCalculator(program, inputs, outputs[:alternate.components]); err != nil {
		return nil, err
	}
	return s, nil
}

// readDeviceNAlternate 读取专色备用空间，Lab在着色求值后转换为RGB
// 入参: object 备用颜色空间
// 返回: *ColorSpace 输出空间, *labSpace Lab变换, error 解析错误
func (r *Reader) readDeviceNAlternate(object Object) (*ColorSpace, *labSpace, error) {
	return r.readDeviceNAlternateSpace(object, false)
}

// readDeviceNAlternateSpace 读取原始或已按资源校验的专色备用空间
// 入参: object 备用定义, effective 是否已按资源校验并替换
// 返回: *ColorSpace 输出空间, *labSpace Lab变换, error 定义错误
func (r *Reader) readDeviceNAlternateSpace(object Object, effective bool) (*ColorSpace, *labSpace, error) {
	object, err := r.resolveColorSpace(object)
	if err != nil {
		return nil, nil, err
	}
	if array, ok := object.(Array); ok && len(array) == 2 && array[0] == Name("Lab") {
		lab, err := r.readLab(array)
		return &ColorSpace{Model: "DeviceRGB", mapped: true}, lab, err
	}
	space, err := r.readEffectiveColorSpace(object, effective)
	return space, nil, err
}
