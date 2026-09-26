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
	"slices"
)

// gradientFunction 保存已解析的一维颜色函数及可精确展开的线性分段
type gradientFunction struct {
	value  func(float64) [4]float64
	linear func([2]float64) []GradientStop
}

// deviceNGradientFunction 在源函数后应用多色仿射映射，保留非线性和范围截断
// 入参: source 源函数, input 着色输入区间, output 着色输出区间, expressions 仿射输出表达式
// 返回: *gradientFunction 备用空间函数
func deviceNGradientFunction(source *gradientFunction, input, output []float64, expressions []affineValue) *gradientFunction {
	unit := gradientUnitBounds(len(input) / 2)
	transform := func(values [4]float64) (result [4]float64) {
		for c, expression := range expressions {
			result[c] = expression[0]
			for j, coefficient := range expression[1:] {
				result[c] += coefficient * values[j]
			}
		}
		return
	}
	f := &gradientFunction{value: func(x float64) [4]float64 {
		values := clipGradientValue(clipGradientValue(source.value(x), unit), input)
		return clipGradientValue(transform(values), output)
	}}
	if source.linear != nil {
		f.linear = func(interval [2]float64) []GradientStop {
			stops := clipGradientValues(clipGradientValues(source.linear(interval), unit), input)
			for i := range stops {
				stops[i].Values = transform(stops[i].Values)
			}
			return clipGradientValues(stops, output)
		}
	}
	return f
}

// readGradientFunction 解析采样、指数、拼接及仿射计算器函数，不近似非线性函数
// 入参: object 函数对象, channels 输出分量数, depth 嵌套深度
// 返回: *gradientFunction 颜色函数, error 解析错误
func (r *Reader) readGradientFunction(object Object, channels, depth int) (*gradientFunction, error) {
	if depth >= 32 {
		return nil, fmt.Errorf("gradient function recursion limit exceeded")
	}
	v, err := r.Resolve(object)
	if err != nil {
		return nil, err
	}
	if array, ok := v.(Array); ok {
		if len(array) != channels {
			return nil, fmt.Errorf("invalid gradient function count")
		}
		parts := make([]*gradientFunction, channels)
		linear := true
		for i, object := range array {
			parts[i], err = r.readGradientFunction(object, 1, depth+1)
			if err != nil {
				return nil, err
			}
			linear = linear && parts[i].linear != nil
		}
		f := &gradientFunction{value: func(x float64) (values [4]float64) {
			for c, part := range parts {
				values[c] = part.value(x)[0]
			}
			return
		}}
		if linear {
			f.linear = func(interval [2]float64) []GradientStop { return mergeGradientFunctions(parts, interval) }
		}
		return f, nil
	}
	dict, ok := v.(Dictionary)
	if stream, streamOK := v.(*Stream); streamOK {
		dict, ok = stream.Dictionary, true
	}
	if !ok {
		return nil, fmt.Errorf("invalid gradient function")
	}
	if dict["FunctionType"] == Integer(0) || dict["FunctionType"] == Integer(2) {
		tint, err := r.readTintFunction(v, channels)
		if err != nil {
			return nil, err
		}
		f := &gradientFunction{value: func(x float64) (values [4]float64) {
			tint.colorInto(x, values[:channels])
			return
		}}
		if tint.sampled {
			stops := sampledGradientStops(tint)
			f.linear = func(interval [2]float64) []GradientStop { return gradientDomain(stops, tint.domain, interval) }
		} else if tint.exponent == 0 || tint.exponent == 1 {
			stops := []GradientStop{{Position: 0}, {Position: 1}}
			for i, x := range tint.domain {
				for c := 0; c < channels; c++ {
					stops[i].Values[c] = tint.values[2*c] + math.Pow(x, tint.exponent)*(tint.values[2*c+1]-tint.values[2*c])
				}
			}
			stops = clipGradientValues(stops, tint.outputRange)
			f.linear = func(interval [2]float64) []GradientStop { return gradientDomain(stops, tint.domain, interval) }
		}
		return f, nil
	}
	domain, err := r.numberArray(dict["Domain"], 2)
	if err != nil || domain[0] >= domain[1] {
		return nil, fmt.Errorf("invalid gradient function domain")
	}
	var limits []float64
	if dict["Range"] != nil || dict["FunctionType"] == Integer(4) {
		limits, err = r.numberArray(dict["Range"], 2*channels)
		if err != nil {
			return nil, err
		}
		for i := 0; i < len(limits); i += 2 {
			if limits[i] > limits[i+1] {
				return nil, fmt.Errorf("invalid gradient function range")
			}
		}
	}
	if dict["FunctionType"] == Integer(4) {
		stream, ok := v.(*Stream)
		if !ok {
			return nil, fmt.Errorf("missing calculator function stream")
		}
		data, err := stream.Decode()
		if err != nil {
			return nil, err
		}
		expressions, err := affineCalculator(data, 1, channels)
		if err != nil {
			return nil, err
		}
		stops := []GradientStop{{Position: 0}, {Position: 1}}
		for i, x := range domain {
			for c, expression := range expressions {
				stops[i].Values[c] = expression[0] + x*expression[1]
				if math.IsNaN(stops[i].Values[c]) || math.IsInf(stops[i].Values[c], 0) {
					return nil, fmt.Errorf("invalid gradient function result")
				}
			}
		}
		stops = clipGradientValues(stops, limits)
		return &gradientFunction{
			value:  func(x float64) [4]float64 { return gradientValue(stops, (x-domain[0])/(domain[1]-domain[0])) },
			linear: func(interval [2]float64) []GradientStop { return gradientDomain(stops, domain, interval) },
		}, nil
	}
	if dict["FunctionType"] != Integer(3) {
		return nil, &UnsupportedError{Feature: "gradient function type"}
	}
	v, err = r.Resolve(dict["Functions"])
	if err != nil {
		return nil, err
	}
	functions, ok := v.(Array)
	if !ok || len(functions) == 0 {
		return nil, fmt.Errorf("invalid stitching functions")
	}
	bounds, err := r.numberArray(dict["Bounds"], len(functions)-1)
	if err != nil {
		return nil, err
	}
	encode, err := r.numberArray(dict["Encode"], len(functions)*2)
	if err != nil {
		return nil, err
	}
	points := append(append([]float64{domain[0]}, bounds...), domain[1])
	parts := make([]*gradientFunction, len(functions))
	linear := true
	for i, object := range functions {
		if points[i] > points[i+1] {
			return nil, fmt.Errorf("invalid gradient stitching bounds")
		}
		parts[i], err = r.readGradientFunction(object, channels, depth+1)
		if err != nil {
			return nil, err
		}
		linear = linear && (points[i] == points[i+1] || parts[i].linear != nil)
	}
	f := &gradientFunction{value: func(x float64) (values [4]float64) {
		x = math.Max(domain[0], math.Min(domain[1], x))
		for i, part := range parts {
			if points[i] == points[i+1] {
				continue
			}
			if x < points[i+1] || points[i+1] == domain[1] {
				t := encode[2*i] + (x-points[i])/(points[i+1]-points[i])*(encode[2*i+1]-encode[2*i])
				values = part.value(t)
				break
			}
		}
		return clipGradientValue(values, limits)
	}}
	if linear {
		var stops []GradientStop
		for i, part := range parts {
			if points[i] == points[i+1] {
				continue
			}
			for _, stop := range part.linear([2]float64{encode[2*i], encode[2*i+1]}) {
				position := points[i] + stop.Position*(points[i+1]-points[i])
				if stop.Position == 1 {
					position = points[i+1]
				}
				stop.Position = (position - domain[0]) / (domain[1] - domain[0])
				stops = append(stops, stop)
			}
		}
		stops = clipGradientValues(stops, limits)
		f.linear = func(interval [2]float64) []GradientStop { return gradientDomain(stops, domain, interval) }
	}
	return f, nil
}

// gradientDomain 将函数定义域中的分段限制或反向映射到调用区间
// 入参: stops 定义域分段, domain 定义域, interval 调用区间
// 返回: []GradientStop 调用区间分段
func gradientDomain(stops []GradientStop, domain []float64, interval [2]float64) []GradientStop {
	return gradientInterval(stops, (interval[0]-domain[0])/(domain[1]-domain[0]), (interval[1]-domain[0])/(domain[1]-domain[0]))
}

// clipGradientValue 按函数范围限制各输出分量
// 入参: values 原值, limits 分量上下界
// 返回: [4]float64 截断值
func clipGradientValue(values [4]float64, limits []float64) [4]float64 {
	for c := 0; c < len(limits)/2; c++ {
		values[c] = math.Max(limits[2*c], math.Min(limits[2*c+1], values[c]))
	}
	return values
}

// mergeGradientFunctions 合并独立分量的精确断点和跳变
// 入参: functions 分量函数, interval 输入区间
// 返回: []GradientStop 合并分段
func mergeGradientFunctions(functions []*gradientFunction, interval [2]float64) []GradientStop {
	parts := make([][]GradientStop, len(functions))
	positions := []float64{0, 1}
	for i, function := range functions {
		parts[i] = function.linear(interval)
		for _, stop := range parts[i] {
			positions = append(positions, stop.Position)
		}
	}
	slices.Sort(positions)
	var stops []GradientStop
	for _, position := range slices.Compact(positions) {
		for _, left := range []bool{true, false} {
			stop := GradientStop{Position: position}
			for c, part := range parts {
				stop.Values[c] = gradientValueSide(part, position, left)[0]
			}
			stops = append(stops, stop)
		}
	}
	return slices.Compact(stops)
}

// sampledGradientStops 展开采样函数的精确断点及范围截断
// 入参: f 已解析采样函数
// 返回: []GradientStop 定义域中的线性分段
func sampledGradientStops(f *tintFunction) []GradientStop {
	positions := []float64{f.domain[0], f.domain[1]}
	if f.encoded[0] != f.encoded[1] {
		for i := 0; i < f.size; i++ {
			x := f.domain[0] + (float64(i)-f.encoded[0])/(f.encoded[1]-f.encoded[0])*(f.domain[1]-f.domain[0])
			if x > f.domain[0] && x < f.domain[1] {
				positions = append(positions, x)
			}
		}
	}
	slices.Sort(positions)
	positions = slices.Compact(positions)
	raw := *f
	raw.values = make([]float64, 2*f.channels)
	for i := 0; i < f.channels; i++ {
		raw.values[2*i], raw.values[2*i+1] = math.Inf(-1), math.Inf(1)
	}
	stops := make([]GradientStop, len(positions))
	for i, x := range positions {
		stops[i].Position = (x - f.domain[0]) / (f.domain[1] - f.domain[0])
		raw.colorInto(x, stops[i].Values[:f.channels])
	}
	return clipGradientValues(stops, f.values)
}
