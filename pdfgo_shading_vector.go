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
	"slices"
)

// maxGradientVectorTotalStops 限制多分量渐变函数跨全部通道展开线性分段的总断点数
const maxGradientVectorTotalStops = maxGradientLinearStops

// gradientVector 保留任意数量渐变分量及各分量的精确线性分段
type gradientVector struct {
	scalar    *gradientFunction
	calculate func(float64, []float64) error
	parts     []*gradientFunction
}

// projectedTintStops 单遍提取全部输出分量的精确断点，超限或非有限时返回nil退回按需求值
// 入参: ctx 取消上下文, f 着色函数, limit 跨通道总断点预算
// 返回: [][]GradientStop 各分量线性分段, int 累计断点数, error 取消错误
func projectedTintStops(ctx context.Context, f *tintFunction, limit int) ([][]GradientStop, int, error) {
	if err := ctx.Err(); err != nil {
		return nil, 0, err
	}
	var positions []float64
	if f.sampled {
		positions = sampledGradientPositions(ctx, f)
		if err := ctx.Err(); err != nil {
			return nil, 0, err
		}
		if len(positions) == 0 {
			return nil, 0, nil
		}
	} else {
		if f.exponent == 1 {
			for c := 0; c < f.channels; c++ {
				if math.IsInf(f.values[2*c+1]-f.values[2*c], 0) {
					return nil, 0, nil
				}
			}
		}
		positions = []float64{f.domain[0], f.domain[1]}
	}
	if len(positions) > limit/f.channels {
		return nil, 0, nil
	}
	raw := *f
	raw.values = slices.Clone(f.values)
	raw.outputRange = nil
	if f.sampled {
		for c := 0; c < f.channels; c++ {
			raw.values[2*c], raw.values[2*c+1] = math.Inf(-1), math.Inf(1)
		}
	}
	backing := make([]GradientStop, len(positions)*f.channels)
	all := make([][]GradientStop, f.channels)
	for c := range all {
		all[c] = backing[c*len(positions) : (c+1)*len(positions)]
	}
	values := make([]float64, f.channels)
	for i, x := range positions {
		if i&255 == 0 {
			if err := ctx.Err(); err != nil {
				return nil, 0, err
			}
		}
		pos := functionPosition(x, f.domain[0], f.domain[1])
		if !finiteFloat(pos) || i > 0 && pos <= all[0][i-1].Position {
			return nil, 0, nil
		}
		raw.colorInto(x, values)
		for c := 0; c < f.channels; c++ {
			if !finiteFloat(values[c]) {
				return nil, 0, nil
			}
			all[c][i] = GradientStop{Position: pos, Values: [4]float64{values[c]}}
		}
	}
	limits := f.outputRange
	if f.sampled {
		limits = f.values
	}
	total := 0
	for c := range all {
		if c&255 == 0 {
			if err := ctx.Err(); err != nil {
				return nil, 0, err
			}
		}
		if limits != nil {
			all[c] = clipGradientValues(all[c], limits[2*c:2*c+2])
		}
		if len(all[c]) == 0 || len(all[c]) > limit-total {
			return nil, 0, nil
		}
		total += len(all[c])
	}
	if err := ctx.Err(); err != nil {
		return nil, 0, err
	}
	return all, total, nil
}

// readGradientVectorContext 在取消上下文内解析多分量采样、指数、拼接及计算器函数
// 入参: ctx 取消上下文, object 函数对象, channels 输出数量, depth 嵌套深度
// 返回: *gradientVector 分量函数, error 格式、求值或取消错误
func (r *Reader) readGradientVectorContext(ctx context.Context, object Object, channels, depth int) (*gradientVector, error) {
	return r.readGradientVectorState(ctx, object, channels, depth, &gradientParseState{})
}

// readGradientVectorState 携带节点预算与间接引用记忆表解析多分量渐变函数
// 入参: ctx 取消上下文, object 函数对象, channels 输出数量, depth 嵌套深度, state 解析状态
// 返回: *gradientVector 分量函数, error 格式、求值或取消错误
func (r *Reader) readGradientVectorState(ctx context.Context, object Object, channels, depth int, state *gradientParseState) (*gradientVector, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if channels < 1 || depth >= 32 {
		return nil, fmt.Errorf("invalid gradient vector dimensions or recursion")
	}
	if channels <= 4 {
		function, err := r.readGradientFunctionState(ctx, object, channels, depth, state)
		if err != nil {
			return nil, err
		}
		return &gradientVector{scalar: function, parts: make([]*gradientFunction, channels), calculate: func(x float64, out []float64) error {
			values, err := function.evaluate(x)
			copy(out, values[:channels])
			return err
		}}, nil
	}
	state.nodes++
	if state.nodes > maxGradientFunctionNodes {
		return nil, fmt.Errorf("gradient function complexity limit exceeded")
	}
	ref, isRef := object.(Reference)
	key := gradientFunctionKey{ref: ref, channels: channels}
	if isRef && state.vectors != nil {
		if cached := state.vectors[key]; cached != nil {
			return cached, nil
		}
	}
	f, err := r.readGradientVectorNode(ctx, object, channels, depth, state)
	if err != nil {
		return nil, err
	}
	if isRef {
		if state.vectors == nil {
			state.vectors = make(map[gradientFunctionKey]*gradientVector)
		}
		state.vectors[key] = f
	}
	return f, nil
}

// readGradientVectorNode 解析单个高通道渐变函数节点并在拼接展开超限时退回按需求值
// 入参: ctx 取消上下文, object 函数对象, channels 输出数量, depth 嵌套深度, state 解析状态
// 返回: *gradientVector 分量函数, error 格式、求值或取消错误
func (r *Reader) readGradientVectorNode(ctx context.Context, object Object, channels, depth int, state *gradientParseState) (*gradientVector, error) {
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
			f.parts[c], err = r.readGradientFunctionState(ctx, item, 1, depth+1, state)
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
	if streamOK && stream != nil {
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
	if err != nil || !finiteFloats(domain) || domain[0] > domain[1] {
		return nil, fmt.Errorf("invalid gradient function domain")
	}
	rangeValue, err := r.Resolve(dict["Range"])
	if err != nil {
		return nil, err
	}
	var limits []float64
	if rangeValue != nil || kind == Integer(0) || kind == Integer(4) {
		limits, err = r.numberArray(rangeValue, 2*channels)
		if err != nil || !finiteFloats(limits) {
			return nil, fmt.Errorf("invalid gradient function range")
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
	budget := min(maxGradientVectorTotalStops, maxGradientVectorTotalStops*4-state.vectorStops)
	var evaluate func(float64, []float64) error
	switch kind {
	case Integer(0), Integer(2):
		tint, err := r.readTintFunction(ctx, value, channels)
		if err != nil {
			return nil, err
		}
		evaluate = func(x float64, out []float64) error { tint.colorInto(x, out); return nil }
		if domain[0] < domain[1] && budget > 0 && (tint.sampled && (tint.order != 3 || tint.size < 4) || !tint.sampled && (tint.exponent == 0 || tint.exponent == 1)) {
			allStops, total, err := projectedTintStops(ctx, tint, budget)
			if err != nil {
				return nil, err
			}
			if len(allStops) == channels {
				state.vectorStops += total
				for c, stops := range allStops {
					f.parts[c].linear = func(interval [2]float64) []GradientStop { return gradientDomain(stops, domain, interval) }
				}
			}
		}
	case Integer(4):
		if !streamOK {
			return nil, fmt.Errorf("missing calculator function stream")
		}
		data, err := r.DecodeStreamContext(ctx, stream)
		if err != nil {
			return nil, err
		}
		program, err := newCalculatorFunction(data, r.warning)
		if err != nil {
			return nil, err
		}
		evaluate = func(x float64, out []float64) error { return program.evaluate([]float64{x}, out) }
		if expressions, err := affineCalculator(data, 1, channels); err == nil {
			output := make([]float64, channels)
			if err := program.evaluate(domain[:1], output); err != nil {
				return nil, err
			}
			exceeded := domain[0] == domain[1] || budget <= 0 || channels > budget/2
			var allStops [][]GradientStop
			if !exceeded {
				allStops = make([][]GradientStop, channels)
			}
			total := 0
			for c, expression := range expressions {
				if c&255 == 0 {
					if err := ctx.Err(); err != nil {
						return nil, err
					}
				}
				stops := []GradientStop{{Position: 0, Values: [4]float64{expression[0] + expression[1]*domain[0]}}, {Position: 1, Values: [4]float64{expression[0] + expression[1]*domain[1]}}}
				for _, stop := range stops {
					if !finiteFloat(stop.Values[0]) {
						return nil, fmt.Errorf("invalid gradient function result")
					}
				}
				if exceeded {
					continue
				}
				stops = clipGradientValues(stops, limits[2*c:2*c+2])
				if len(stops) == 0 || len(stops) > budget-total {
					exceeded = true
					allStops = nil
					continue
				}
				allStops[c] = stops
				total += len(stops)
			}
			if !exceeded {
				state.vectorStops += total
				for c, stops := range allStops {
					f.parts[c].linear = func(interval [2]float64) []GradientStop { return gradientDomain(stops, domain, interval) }
				}
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
		if err != nil || !finiteFloats(bounds) {
			return nil, fmt.Errorf("invalid gradient stitching bounds")
		}
		encode, err := r.numberArray(dict["Encode"], 2*len(functions))
		if err != nil || !finiteFloats(encode) {
			return nil, fmt.Errorf("invalid gradient stitching encode")
		}
		points := append(append([]float64{domain[0]}, bounds...), domain[1])
		parts := make([]*gradientVector, len(functions))
		for i, item := range functions {
			if points[i] > points[i+1] {
				return nil, fmt.Errorf("invalid gradient stitching bounds")
			}
			parts[i], err = r.readGradientVectorState(ctx, item, channels, depth+1, state)
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
		budget = min(maxGradientVectorTotalStops, maxGradientVectorTotalStops*4-state.vectorStops)
		if domain[0] < domain[1] && budget > 0 {
			allStops := make([][]GradientStop, channels)
			total := 0
			exceeded := false
			for c := range f.parts {
				if err := ctx.Err(); err != nil {
					return nil, err
				}
				linear := true
				for i, part := range parts {
					linear = linear && (points[i] == points[i+1] && i < len(parts)-1 || part.parts[c].linear != nil)
				}
				if !linear {
					continue
				}
				var stops []GradientStop
				for i, part := range parts {
					if i&255 == 0 {
						if err := ctx.Err(); err != nil {
							return nil, err
						}
					}
					if points[i] == points[i+1] {
						if i == len(parts)-1 {
							partStops := part.parts[c].linear([2]float64{encode[2*i], encode[2*i]})
							if len(partStops) == 0 {
								stops = nil
								break
							}
							if len(partStops) > budget-total-len(stops) {
								stops = nil
								exceeded = true
								break
							}
							for _, stop := range partStops {
								stop.Position = 1
								stops = append(stops, stop)
							}
						}
						continue
					}
					partStops := part.parts[c].linear([2]float64{encode[2*i], encode[2*i+1]})
					if len(partStops) == 0 {
						stops = nil
						break
					}
					if len(partStops) > budget-total-len(stops) {
						stops = nil
						exceeded = true
						break
					}
					for _, stop := range partStops {
						stop.Position = functionPosition(gradientPosition(points[i], points[i+1], stop.Position), domain[0], domain[1])
						stops = append(stops, stop)
					}
				}
				if exceeded {
					allStops = nil
					break
				}
				if len(stops) > 0 && limits != nil {
					stops = clipGradientValues(stops, limits[2*c:2*c+2])
				}
				if len(stops) > budget-total {
					allStops = nil
					exceeded = true
					break
				}
				if len(stops) > 0 {
					allStops[c] = stops
					total += len(stops)
				}
			}
			if !exceeded {
				state.vectorStops += total
				for c, stops := range allStops {
					if len(stops) > 0 {
						f.parts[c].linear = func(interval [2]float64) []GradientStop { return gradientDomain(stops, domain, interval) }
					}
				}
			}
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

// deviceNWideGradientContext 在取消上下文内组合多分量渐变与备用空间映射，保留精确分段
// 入参: ctx 取消上下文, function 渐变函数, tint 专色变换, domain 输入区间
// 返回: []GradientStop 备用空间分段, *ColorSpace 输出空间, *gradientFunction 求值函数, error 解析或取消错误
func (r *Reader) deviceNWideGradientContext(ctx context.Context, function Object, tint *deviceNSpace, domain [2]float64) ([]GradientStop, *ColorSpace, *gradientFunction, error) {
	source, err := r.readGradientVectorContext(ctx, function, tint.components, 0)
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
	linear := tint.expressions != nil && tint.lab == nil && tint.nested == nil
	mapped.colorants, mapped.colorant = tint.colorants, source.calculate
	for _, part := range source.parts {
		linear = linear && part.linear != nil
	}
	if linear {
		mapped.linear = func(interval [2]float64) []GradientStop {
			if ctx.Err() != nil {
				return nil
			}
			parts := make([][]GradientStop, tint.components)
			var positions []float64
			totalStops := 0
			for c, part := range source.parts {
				if c&255 == 0 && ctx.Err() != nil {
					return nil
				}
				parts[c] = clipGradientValues(clipGradientValues(part.linear(interval), []float64{0, 1}), tint.input[2*c:2*c+2])
				if len(parts[c]) == 0 || len(parts[c]) > maxGradientVectorTotalStops-totalStops {
					return nil
				}
				totalStops += len(parts[c])
				for _, stop := range parts[c] {
					positions = append(positions, stop.Position)
				}
			}
			slices.Sort(positions)
			positions = slices.Compact(positions)
			if int64(len(positions))*int64(tint.components) > int64(maxGradientVectorTotalStops)*4 {
				return nil
			}
			stops := make([]GradientStop, 0, len(positions))
			values := make([]float64, tint.components)
			for i, position := range positions {
				if i&255 == 0 && ctx.Err() != nil {
					return nil
				}
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
					if len(stops) == 0 || stops[len(stops)-1] != stop {
						if len(stops) >= maxGradientLinearStops {
							return nil
						}
						stops = append(stops, stop)
					}
				}
			}
			return iccGradientStops(clipGradientValues(stops, tint.output), tint.alternate.profile)
		}
	}
	var stops []GradientStop
	if mapped.linear != nil {
		stops = clipGradientValues(mapped.linear(domain), gradientUnitBounds(tint.alternate.Components()))
		if err := ctx.Err(); err != nil {
			return nil, nil, nil, err
		}
	}
	return stops, tint.alternate, mapped, nil
}
