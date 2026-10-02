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

// gradientVector 保留任意数量渐变分量及各分量的精确线性分段
type gradientVector struct {
	scalar    *gradientFunction
	calculate func(float64, []float64) error
	parts     []*gradientFunction
}

// readGradientVector 解析多分量采样、指数、拼接及计算器函数
// 入参: object 函数对象, channels 输出数量, depth 嵌套深度
// 返回: *gradientVector 分量函数, error 格式或求值错误
func (r *Reader) readGradientVector(object Object, channels, depth int) (*gradientVector, error) {
	if channels < 1 || depth >= 32 {
		return nil, fmt.Errorf("invalid gradient vector dimensions or recursion")
	}
	if channels <= 4 {
		function, err := r.readGradientFunction(object, channels, depth)
		if err != nil {
			return nil, err
		}
		return &gradientVector{scalar: function, parts: make([]*gradientFunction, channels), calculate: func(x float64, out []float64) error {
			values, err := function.evaluate(x)
			copy(out, values[:channels])
			return err
		}}, nil
	}
	value, err := r.Resolve(object)
	if err != nil {
		return nil, err
	}
	f := &gradientVector{parts: make([]*gradientFunction, channels)}
	if array, ok := value.(Array); ok {
		if len(array) != channels {
			return nil, fmt.Errorf("invalid gradient function count")
		}
		for c, item := range array {
			f.parts[c], err = r.readGradientFunction(item, 1, depth+1)
			if err != nil {
				return nil, err
			}
		}
		f.calculate = func(x float64, out []float64) error {
			for c, part := range f.parts {
				v, err := part.evaluate(x)
				if err != nil {
					return err
				}
				out[c] = v[0]
			}
			return nil
		}
		return f, nil
	}
	dict, ok := value.(Dictionary)
	stream, streamOK := value.(*Stream)
	if streamOK {
		dict, ok = stream.Dictionary, true
	}
	if !ok {
		return nil, fmt.Errorf("invalid gradient function")
	}
	kind, err := r.Resolve(dict["FunctionType"])
	if err != nil {
		return nil, err
	}
	domain, err := r.numberArray(dict["Domain"], 2)
	if err != nil || domain[0] > domain[1] {
		return nil, fmt.Errorf("invalid gradient function domain")
	}
	var limits []float64
	if dict["Range"] != nil || kind == Integer(0) || kind == Integer(4) {
		limits, err = r.numberArray(dict["Range"], 2*channels)
		if err != nil {
			return nil, err
		}
		for c := 0; c < channels; c++ {
			if limits[2*c] > limits[2*c+1] {
				return nil, fmt.Errorf("invalid gradient function range")
			}
		}
	}
	for c := range f.parts {
		f.parts[c] = &gradientFunction{}
	}
	var evaluate func(float64, []float64) error
	switch kind {
	case Integer(0), Integer(2):
		tint, err := r.readTintFunction(value, channels)
		if err != nil {
			return nil, err
		}
		evaluate = func(x float64, out []float64) error { tint.colorInto(x, out); return nil }
		if tint.sampled && (tint.order != 3 || tint.size < 4) || !tint.sampled && (tint.exponent == 0 || tint.exponent == 1) {
			for c := range f.parts {
				stops := projectedTintStops(tint, c)
				f.parts[c].linear = func(interval [2]float64) []GradientStop { return gradientDomain(stops, domain, interval) }
			}
		}
	case Integer(4):
		if !streamOK {
			return nil, fmt.Errorf("missing calculator function stream")
		}
		data, err := stream.Decode()
		if err != nil {
			return nil, err
		}
		program, err := compileCalculator(data)
		if err != nil {
			return nil, err
		}
		evaluate = func(x float64, out []float64) error { return evaluateCalculator(program, []float64{x}, out) }
		if expressions, err := affineCalculator(data, 1, channels); err == nil {
			for c, expression := range expressions {
				stops := []GradientStop{{Position: 0, Values: [4]float64{expression[0] + expression[1]*domain[0]}}, {Position: 1, Values: [4]float64{expression[0] + expression[1]*domain[1]}}}
				for _, stop := range stops {
					if math.IsNaN(stop.Values[0]) || math.IsInf(stop.Values[0], 0) {
						return nil, fmt.Errorf("invalid gradient function result")
					}
				}
				stops = clipGradientValues(stops, limits[2*c:2*c+2])
				f.parts[c].linear = func(interval [2]float64) []GradientStop { return gradientDomain(stops, domain, interval) }
			}
		}
	case Integer(3):
		value, err = r.Resolve(dict["Functions"])
		if err != nil {
			return nil, err
		}
		functions, ok := value.(Array)
		if !ok || len(functions) == 0 {
			return nil, fmt.Errorf("invalid stitching functions")
		}
		if domain[0] == domain[1] && len(functions) != 1 {
			return nil, fmt.Errorf("invalid stitching function domain")
		}
		bounds, err := r.numberArray(dict["Bounds"], len(functions)-1)
		if err != nil {
			return nil, err
		}
		encode, err := r.numberArray(dict["Encode"], 2*len(functions))
		if err != nil {
			return nil, err
		}
		points := append(append([]float64{domain[0]}, bounds...), domain[1])
		parts := make([]*gradientVector, len(functions))
		for i, item := range functions {
			if points[i] > points[i+1] {
				return nil, fmt.Errorf("invalid gradient stitching bounds")
			}
			parts[i], err = r.readGradientVector(item, channels, depth+1)
			if err != nil {
				return nil, err
			}
		}
		evaluate = func(x float64, out []float64) error {
			clear(out)
			for i, part := range parts {
				if x < points[i+1] || i == len(parts)-1 {
					return part.calculate(functionValue(functionPosition(x, points[i], points[i+1]), encode[2*i], encode[2*i+1]), out)
				}
			}
			return nil
		}
		for c := range f.parts {
			linear := true
			for i, part := range parts {
				linear = linear && (points[i] == points[i+1] && i < len(parts)-1 || part.parts[c].linear != nil)
			}
			if !linear {
				continue
			}
			var stops []GradientStop
			for i, part := range parts {
				if points[i] == points[i+1] {
					if i == len(parts)-1 {
						for _, stop := range part.parts[c].linear([2]float64{encode[2*i], encode[2*i]}) {
							stop.Position = 1
							stops = append(stops, stop)
						}
					}
					continue
				}
				for _, stop := range part.parts[c].linear([2]float64{encode[2*i], encode[2*i+1]}) {
					stop.Position = functionPosition(gradientPosition(points[i], points[i+1], stop.Position), domain[0], domain[1])
					stops = append(stops, stop)
				}
			}
			if limits != nil {
				stops = clipGradientValues(stops, limits[2*c:2*c+2])
			}
			f.parts[c].linear = func(interval [2]float64) []GradientStop { return gradientDomain(stops, domain, interval) }
		}
	default:
		return nil, &UnsupportedError{Feature: "gradient function type"}
	}
	f.calculate = func(x float64, out []float64) error {
		if err := evaluate(math.Max(domain[0], math.Min(domain[1], x)), out); err != nil {
			return err
		}
		for c, value := range out {
			if math.IsNaN(value) || math.IsInf(value, 0) {
				return fmt.Errorf("nonfinite gradient color")
			}
			if limits != nil {
				out[c] = math.Max(limits[2*c], math.Min(limits[2*c+1], value))
			}
		}
		return nil
	}
	if domain[0] == domain[1] {
		values := make([]float64, channels)
		if err := f.calculate(domain[0], values); err != nil {
			return nil, err
		}
		f.calculate = func(_ float64, out []float64) error { copy(out, values); return nil }
		for c, value := range values {
			f.parts[c] = constantGradientFunction([4]float64{value})
		}
	}
	return f, nil
}

// projectedTintStops 提取单个输出分量的精确断点，不缩减函数输出数量
// 入参: f 着色函数, channel 输出分量
// 返回: []GradientStop 定义域中的线性分段
func projectedTintStops(f *tintFunction, channel int) []GradientStop {
	positions := []float64{f.domain[0], f.domain[1]}
	if f.sampled && f.encoded[0] != f.encoded[1] {
		for i := 0; i < f.size; i++ {
			x := functionValue(functionPosition(float64(i), f.encoded[0], f.encoded[1]), f.domain[0], f.domain[1])
			if x > f.domain[0] && x < f.domain[1] {
				positions = append(positions, x)
			}
		}
	}
	slices.Sort(positions)
	raw := *f
	raw.values = slices.Clone(f.values)
	raw.outputRange = nil
	if f.sampled {
		for c := 0; c < f.channels; c++ {
			raw.values[2*c], raw.values[2*c+1] = math.Inf(-1), math.Inf(1)
		}
	}
	values := make([]float64, f.channels)
	var stops []GradientStop
	for _, x := range slices.Compact(positions) {
		raw.colorInto(x, values)
		stops = append(stops, GradientStop{Position: functionPosition(x, f.domain[0], f.domain[1]), Values: [4]float64{values[channel]}})
	}
	limits := f.outputRange
	if f.sampled {
		limits = f.values
	}
	if limits != nil {
		stops = clipGradientValues(stops, limits[2*channel:2*channel+2])
	}
	return stops
}

// deviceNWideGradient 组合多分量渐变与备用空间映射，保留精确分段
// 入参: function 渐变函数, tint 专色变换, domain 输入区间
// 返回: []GradientStop 备用空间分段, *ColorSpace 输出空间, *gradientFunction 求值函数, error 解析错误
func (r *Reader) deviceNWideGradient(function Object, tint *deviceNSpace, domain [2]float64) ([]GradientStop, *ColorSpace, *gradientFunction, error) {
	source, err := r.readGradientVector(function, tint.components, 0)
	if err != nil {
		return nil, nil, nil, err
	}
	mapped := &gradientFunction{calculate: func(x float64) ([4]float64, error) {
		var buffer [32]float64
		values := buffer[:min(tint.components, len(buffer))]
		if tint.components > len(buffer) {
			values = make([]float64, tint.components)
		}
		if err := source.calculate(x, values); err != nil {
			return [4]float64{}, err
		}
		return tint.values(values...)
	}}
	linear := tint.expressions != nil && tint.lab == nil
	for _, part := range source.parts {
		linear = linear && part.linear != nil
	}
	if linear {
		mapped.linear = func(interval [2]float64) []GradientStop {
			parts := make([][]GradientStop, tint.components)
			var positions []float64
			for c, part := range source.parts {
				parts[c] = clipGradientValues(clipGradientValues(part.linear(interval), []float64{0, 1}), tint.input[2*c:2*c+2])
				for _, stop := range parts[c] {
					positions = append(positions, stop.Position)
				}
			}
			slices.Sort(positions)
			var stops []GradientStop
			values := make([]float64, tint.components)
			for _, position := range slices.Compact(positions) {
				for _, left := range []bool{true, false} {
					for c, part := range parts {
						values[c] = gradientValueSide(part, position, left)[0]
					}
					stop := GradientStop{Position: position}
					for c, expression := range tint.expressions {
						stop.Values[c] = expression[0]
						for j, coefficient := range expression[1:] {
							stop.Values[c] += coefficient * values[j]
						}
					}
					stops = append(stops, stop)
				}
			}
			return clipGradientValues(slices.Compact(stops), tint.output)
		}
	}
	var stops []GradientStop
	if mapped.linear != nil {
		stops = clipGradientValues(mapped.linear(domain), gradientUnitBounds(tint.alternate.Components()))
	}
	return stops, tint.alternate, mapped, nil
}
