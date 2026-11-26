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

// maxGradientLinearStops 限制一维采样渐变展开为线性分段的最大断点数，超限时退回按需函数求值
const maxGradientLinearStops = 65536

// maxGradientTotalStops 限制单次渐变函数解析跨全部子节点累计保留的最大线性断点数
const maxGradientTotalStops = maxGradientLinearStops * 4

// maxGradientFunctionNodes 限制单次渐变函数解析允许访问的最大节点数，防止共享DAG指数展开
const maxGradientFunctionNodes = 4096

// gradientParseStateKey 作为上下文键传递单次颜色或着色解析共享的渐变函数预算与记忆表
type gradientParseStateKey struct{}

// gradientFunctionKey 标识单次解析中可复用的间接函数对象及通道数
type gradientFunctionKey struct {
	ref      Reference
	channels int
}

// streamFunctionKey 标识单次解析中按流指针与输入输出维度复用的函数条目
type streamFunctionKey struct {
	stream  *Stream
	inputs  int
	outputs int
}

// cachedCalculator 保存单次解析复用的多分量计算器函数与仿射快路径
type cachedCalculator struct {
	input       []float64
	output      []float64
	expressions []affineValue
	program     *calculatorFunction
}

// gradientParseState 保存单次渐变函数解析的节点、停点与解码字节预算及间接引用记忆表
type gradientParseState struct {
	nodes       int
	stops       int
	bytes       int64
	limit       int64
	functions   map[gradientFunctionKey]*gradientFunction
	vectors     map[gradientFunctionKey]*gradientVector
	sampled     map[streamFunctionKey]*sampledFunction
	calculators map[streamFunctionKey]*cachedCalculator
}

// withGradientParseBudget 在上下文中附着单次颜色或着色解析共享的渐变函数预算与记忆表
// 入参: ctx 取消上下文, r 文档阅读器
// 返回: context.Context 携带共享解析状态的上下文
func withGradientParseBudget(ctx context.Context, r *Reader) context.Context {
	if ctx == nil {
		return nil
	}
	if state, _ := ctx.Value(gradientParseStateKey{}).(*gradientParseState); state != nil {
		return ctx
	}
	limit, _ := effectiveStreamLimits(r, nil)
	return context.WithValue(ctx, gradientParseStateKey{}, &gradientParseState{limit: limit})
}

// gradientStateFromContext 复用上下文中已附着的渐变解析状态，未附着时创建局部状态
// 入参: ctx 取消上下文, r 文档阅读器
// 返回: *gradientParseState 渐变函数解析状态
func gradientStateFromContext(ctx context.Context, r *Reader) *gradientParseState {
	if ctx != nil {
		if state, _ := ctx.Value(gradientParseStateKey{}).(*gradientParseState); state != nil {
			return state
		}
	}
	limit, _ := effectiveStreamLimits(r, nil)
	return &gradientParseState{limit: limit}
}

// gradientParsedBytes 返回上下文共享解析状态已累计消耗的解码字节数
// 入参: ctx 取消上下文
// 返回: int64 已累计解码字节数
func gradientParsedBytes(ctx context.Context) int64 {
	if ctx == nil {
		return 0
	}
	if state, _ := ctx.Value(gradientParseStateKey{}).(*gradientParseState); state != nil {
		return state.bytes
	}
	return 0
}

// remainingStops 返回单次解析剩余允许保留的累计线性停点数
// 返回: int 剩余停点预算
func (s *gradientParseState) remainingStops() int {
	if s.stops >= maxGradientTotalStops {
		return 0
	}
	return maxGradientTotalStops - s.stops
}

// reserveStops 从单次解析总预算中扣减指定数量的线性停点
// 入参: count 新增停点数
// 返回: bool 是否在预算内并已扣减
func (s *gradientParseState) reserveStops(count int) bool {
	if count <= 0 || count > maxGradientTotalStops-s.stops {
		return false
	}
	s.stops += count
	return true
}

// reserveBytes 从单次解析总字节预算中扣减指定解码或编译堆占用
// 入参: size 新增字节数
// 返回: error 超出预算时返回包装ErrStreamLimitExceeded的错误
func (s *gradientParseState) reserveBytes(size int64) error {
	if size <= 0 {
		return nil
	}
	if s.limit > 0 && size > s.limit-s.bytes {
		return fmt.Errorf("%w: gradient function memory limit exceeded", ErrStreamLimitExceeded)
	}
	s.bytes += size
	return nil
}

// checkSampledSizesBudget 解码前按多维采样尺寸和位宽预检累计字节预算
// 入参: sizes 各维度样本数, outputs 输出分量数, bits 每样本位数, state 解析状态
// 返回: error 预算超限错误
func checkSampledSizesBudget(sizes []float64, outputs, bits int, state *gradientParseState) error {
	if state == nil || state.limit <= 0 {
		return nil
	}
	remaining := uint64(max(int64(0), state.limit-state.bytes))
	limitBits := remaining * 8
	if remaining > math.MaxUint64/8 {
		limitBits = math.MaxUint64
	}
	totalBits := uint64(outputs) * uint64(bits)
	if totalBits == 0 {
		return nil
	}
	for _, size := range sizes {
		if size < 1 || size > float64(math.MaxInt64) || math.Trunc(size) != size {
			return nil
		}
		s := uint64(size)
		if s > limitBits/totalBits {
			return fmt.Errorf("%w: gradient function memory limit exceeded", ErrStreamLimitExceeded)
		}
		totalBits *= s
	}
	if (totalBits-1)/8+1 > remaining {
		return fmt.Errorf("%w: gradient function memory limit exceeded", ErrStreamLimitExceeded)
	}
	return nil
}

// gradientFunction 保存已解析的一维颜色函数及可精确展开的线性分段
type gradientFunction struct {
	value     func(float64) [4]float64
	calculate func(float64) ([4]float64, error)
	linear    func([2]float64) []GradientStop
	colorants *ColorantSpace
	colorant  func(float64, []float64) error
	bytes     int64
}

// constantGradientFunction 保存单点函数的确定结果及常量分段
// 入参: values 函数结果
// 返回: *gradientFunction 常量函数
func constantGradientFunction(values [4]float64) *gradientFunction {
	return &gradientFunction{
		bytes: 256,
		value: func(float64) [4]float64 { return values },
		linear: func([2]float64) []GradientStop {
			return []GradientStop{{Position: 0, Values: values}, {Position: 1, Values: values}}
		},
	}
}

// labGradientFunction 在源函数求值后转换Lab，不将非线性颜色变换近似为线性分段
// 入参: source 源函数, lab Lab颜色参数
// 返回: *gradientFunction 显示颜色函数
func labGradientFunction(source *gradientFunction, lab *labSpace) *gradientFunction {
	return &gradientFunction{bytes: 128 + source.bytes, calculate: func(x float64) ([4]float64, error) {
		values, err := source.evaluate(x)
		if err != nil {
			return values, err
		}
		return shadingColorValues(nil, nil, lab, values[:3])
	}}
}

// iccGradientFunction 在源函数求值后裁切并归一化ICC分量，保留范围外截断语义
// 入参: source 源函数, profile ICC源空间
// 返回: *gradientFunction 合成分量函数
func iccGradientFunction(source *gradientFunction, profile *iccColorSpace) *gradientFunction {
	if profile == nil || profile.ranges == nil {
		return source
	}
	mapped := &gradientFunction{bytes: 128 + source.bytes, calculate: func(x float64) ([4]float64, error) {
		values, err := source.evaluate(x)
		if err != nil {
			return values, err
		}
		for _, value := range values[:profile.components()] {
			if math.IsNaN(value) || math.IsInf(value, 0) {
				return values, fmt.Errorf("nonfinite shading color")
			}
		}
		return profile.normalize(values[:profile.components()]), nil
	}}
	if source.linear != nil {
		mapped.linear = func(interval [2]float64) []GradientStop {
			return iccGradientStops(source.linear(interval), profile)
		}
	}
	return mapped
}

// iccGradientStops 归一化精确分段并保留源范围的截断位置，不将颜色变换近似为RGB分段
// 入参: stops 源分量分段, profile ICC源空间
// 返回: []GradientStop 合成分量分段
func iccGradientStops(stops []GradientStop, profile *iccColorSpace) []GradientStop {
	if len(stops) == 0 || len(stops) > maxGradientLinearStops {
		return nil
	}
	if profile == nil || profile.ranges == nil {
		return stops
	}
	ranges := profile.sourceRanges()
	stops = clipGradientValues(stops, ranges[:profile.components()*2])
	for i := range stops {
		stops[i].Values = profile.normalize(stops[i].Values[:profile.components()])
	}
	return stops
}

// composeGradientFunction 将单分量渐变输入着色函数，保留分段、跳变及非线性计算
// 入参: source 浓度函数, tint 着色函数
// 返回: *gradientFunction 复合颜色函数
func composeGradientFunction(source, tint *gradientFunction) *gradientFunction {
	size := int64(128) + source.bytes
	if tint != source {
		size += tint.bytes
	}
	f := &gradientFunction{bytes: size, calculate: func(x float64) ([4]float64, error) {
		values, err := source.evaluate(x)
		if err != nil {
			return [4]float64{}, err
		}
		return tint.evaluate(math.Max(0, math.Min(1, values[0])))
	}}
	if source.linear != nil && tint.linear != nil {
		f.linear = func(interval [2]float64) []GradientStop {
			stops := clipGradientValues(source.linear(interval), []float64{0, 1})
			if len(stops) == 0 {
				return nil
			}
			result := make([]GradientStop, 0, len(stops))
			for i, stop := range stops {
				if i > 0 && stops[i-1].Position < stop.Position {
					previous := stops[i-1]
					mappedStops := tint.linear([2]float64{previous.Values[0], stop.Values[0]})
					if len(mappedStops) == 0 {
						return nil
					}
					for _, mapped := range mappedStops {
						mapped.Position = gradientPosition(previous.Position, stop.Position, mapped.Position)
						if len(result) == 0 || result[len(result)-1] != mapped {
							if len(result) >= maxGradientLinearStops {
								return nil
							}
							result = append(result, mapped)
						}
					}
				} else {
					mappedStops := tint.linear([2]float64{stop.Values[0], stop.Values[0]})
					if len(mappedStops) == 0 {
						return nil
					}
					values := gradientValue(mappedStops, 0)
					mapped := GradientStop{Position: stop.Position, Values: values}
					if len(result) == 0 || result[len(result)-1] != mapped {
						if len(result) >= maxGradientLinearStops {
							return nil
						}
						result = append(result, mapped)
					}
				}
			}
			return result
		}
	}
	return f
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
	f := &gradientFunction{bytes: 128 + source.bytes + int64(len(expressions))*48, calculate: func(x float64) ([4]float64, error) {
		values, err := source.evaluate(x)
		if err != nil {
			return [4]float64{}, err
		}
		values = clipGradientValue(clipGradientValue(values, unit), input)
		return clipGradientValue(transform(values), output), nil
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

// gradientDomain 将函数定义域中的分段限制或反向映射到调用区间
// 入参: stops 定义域分段, domain 定义域, interval 调用区间
// 返回: []GradientStop 调用区间分段
func gradientDomain(stops []GradientStop, domain []float64, interval [2]float64) []GradientStop {
	return gradientInterval(stops, functionPosition(interval[0], domain[0], domain[1]), functionPosition(interval[1], domain[0], domain[1]))
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

// mergeGradientFunctions 合并独立分量的精确断点和跳变，超限或缺失分段时返回nil
// 入参: functions 分量函数, interval 输入区间
// 返回: []GradientStop 合并分段
func mergeGradientFunctions(functions []*gradientFunction, interval [2]float64) []GradientStop {
	parts := make([][]GradientStop, len(functions))
	positions := []float64{0, 1}
	for i, function := range functions {
		part := function.linear(interval)
		if len(part) == 0 || len(part) > maxGradientLinearStops {
			return nil
		}
		parts[i] = part
		for _, stop := range part {
			positions = append(positions, stop.Position)
		}
		if len(positions) > maxGradientLinearStops {
			slices.Sort(positions)
			positions = slices.Compact(positions)
			if len(positions) > maxGradientLinearStops {
				return nil
			}
		}
	}
	slices.Sort(positions)
	positions = slices.Compact(positions)
	stops := make([]GradientStop, 0, len(positions))
	for _, position := range positions {
		for _, left := range []bool{true, false} {
			stop := GradientStop{Position: position}
			for c, part := range parts {
				stop.Values[c] = gradientValueSide(part, position, left)[0]
			}
			if len(stops) == 0 || stops[len(stops)-1] != stop {
				if len(stops) >= maxGradientLinearStops {
					return nil
				}
				stops = append(stops, stop)
			}
		}
	}
	return stops
}

// sampledGradientPositions 提取采样函数定义域内的精确样本位置，超限、坍缩或取消时返回nil
// 入参: ctx 取消上下文, f 已解析采样函数, limit 最大断点数上限
// 返回: []float64 升序样本位置
func sampledGradientPositions(ctx context.Context, f *tintFunction, limit int) []float64 {
	if limit < 2 {
		return nil
	}
	positions := []float64{f.domain[0], f.domain[1]}
	if f.encoded[0] != f.encoded[1] {
		lo, hi := min(f.encoded[0], f.encoded[1]), max(f.encoded[0], f.encoded[1])
		start, end := 0, f.size-1
		if lo >= 0 {
			if lo >= float64(f.size-1) {
				start = f.size
			} else {
				start = int(math.Floor(lo)) + 1
			}
		}
		if hi <= float64(end) {
			if hi <= 0 {
				end = -1
			} else {
				end = int(math.Ceil(hi)) - 1
			}
		}
		if end >= start && int64(end)-int64(start)+3 > int64(limit) {
			return nil
		}
		var previous float64
		for i := start; i <= end; i++ {
			if i&255 == 0 && ctx.Err() != nil {
				return nil
			}
			x := functionValue(functionPosition(float64(i), f.encoded[0], f.encoded[1]), f.domain[0], f.domain[1])
			if !(x > f.domain[0] && x < f.domain[1]) || i > start && x == previous {
				return nil
			}
			previous = x
			positions = append(positions, x)
		}
	}
	slices.Sort(positions)
	return slices.Compact(positions)
}

// sampledGradientStops 展开采样函数的精确断点及范围截断，超限时返回nil退回按需求值
// 入参: ctx 取消上下文, f 已解析采样函数, limit 最大断点数上限
// 返回: []GradientStop 定义域中的线性分段
func sampledGradientStops(ctx context.Context, f *tintFunction, limit int) []GradientStop {
	positions := sampledGradientPositions(ctx, f, limit)
	if len(positions) == 0 {
		return nil
	}
	raw := *f
	raw.values = make([]float64, 2*f.channels)
	for i := 0; i < f.channels; i++ {
		raw.values[2*i], raw.values[2*i+1] = math.Inf(-1), math.Inf(1)
	}
	stops := make([]GradientStop, len(positions))
	for i, x := range positions {
		stops[i].Position = functionPosition(x, f.domain[0], f.domain[1])
		if i > 0 && stops[i].Position <= stops[i-1].Position {
			return nil
		}
		raw.colorInto(x, stops[i].Values[:f.channels])
	}
	stops = clipGradientValues(stops, f.values)
	if len(stops) > limit {
		return nil
	}
	return stops
}

// evaluate 计算颜色函数并传递运行时算术错误
// 入参: x 输入分量
// 返回: [4]float64 输出分量, error 求值错误
func (f *gradientFunction) evaluate(x float64) ([4]float64, error) {
	if f.calculate != nil {
		return f.calculate(x)
	}
	return f.value(x), nil
}

// readGradientFunctionContext 在取消上下文内解析采样、指数、拼接及计算器函数
// 入参: ctx 取消上下文, object 函数对象, channels 输出分量数, depth 嵌套深度
// 返回: *gradientFunction 颜色函数, error 解析、预算或取消错误
func (r *Reader) readGradientFunctionContext(ctx context.Context, object Object, channels, depth int) (*gradientFunction, error) {
	return r.readGradientFunctionState(ctx, object, channels, depth, gradientStateFromContext(ctx, r))
}

// readGradientFunctionState 携带节点、停点与字节预算及间接引用记忆表解析渐变函数
// 入参: ctx 取消上下文, object 函数对象, channels 输出分量数, depth 嵌套深度, state 解析状态
// 返回: *gradientFunction 颜色函数, error 解析、预算或取消错误
func (r *Reader) readGradientFunctionState(ctx context.Context, object Object, channels, depth int, state *gradientParseState) (*gradientFunction, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if channels < 1 || channels > 4 {
		return nil, &UnsupportedError{Feature: "gradient function component count"}
	}
	if depth >= 32 {
		return nil, fmt.Errorf("gradient function recursion limit exceeded")
	}
	if ctx.Value(gradientParseStateKey{}) != state {
		ctx = context.WithValue(ctx, gradientParseStateKey{}, state)
	}
	state.nodes++
	if state.nodes > maxGradientFunctionNodes {
		return nil, fmt.Errorf("gradient function complexity limit exceeded")
	}
	ref, isRef := object.(Reference)
	key := gradientFunctionKey{ref: ref, channels: channels}
	if isRef && state.functions != nil {
		if cached := state.functions[key]; cached != nil {
			return cached, nil
		}
	}
	f, err := r.readGradientFunctionNode(ctx, object, channels, depth, state)
	if err != nil {
		return nil, err
	}
	if isRef {
		if state.functions == nil {
			state.functions = make(map[gradientFunctionKey]*gradientFunction)
		}
		state.functions[key] = f
	}
	return f, nil
}

// readGradientFunctionNode 解析单个渐变函数节点并在拼接展开超限时退回按需求值
// 入参: ctx 取消上下文, object 函数对象, channels 输出分量数, depth 嵌套深度, state 解析状态
// 返回: *gradientFunction 颜色函数, error 解析、预算或取消错误
func (r *Reader) readGradientFunctionNode(ctx context.Context, object Object, channels, depth int, state *gradientParseState) (*gradientFunction, error) {
	v, err := r.Resolve(object)
	if err != nil {
		return nil, err
	}
	if array, ok := v.(Array); ok {
		if len(array) != channels {
			return nil, fmt.Errorf("invalid gradient function count")
		}
		parts := make([]*gradientFunction, channels)
		seen := make(map[*gradientFunction]bool, channels)
		size := int64(256) + int64(channels)*8
		linear := true
		for i, object := range array {
			parts[i], err = r.readGradientFunctionState(ctx, object, 1, depth+1, state)
			if err != nil {
				return nil, err
			}
			if !seen[parts[i]] {
				seen[parts[i]] = true
				size += parts[i].bytes
			}
			linear = linear && parts[i].linear != nil
		}
		f := &gradientFunction{bytes: size, calculate: func(x float64) (values [4]float64, err error) {
			for c, part := range parts {
				result, evalErr := part.evaluate(x)
				if evalErr != nil {
					return values, evalErr
				}
				values[c] = result[0]
			}
			return
		}}
		if linear {
			f.linear = func(interval [2]float64) []GradientStop { return mergeGradientFunctions(parts, interval) }
		}
		return f, nil
	}
	dict, ok := v.(Dictionary)
	if stream, streamOK := v.(*Stream); streamOK && stream != nil {
		dict, ok = stream.Dictionary, true
	}
	if !ok {
		return nil, fmt.Errorf("invalid gradient function")
	}
	kind, err := r.Resolve(dict["FunctionType"])
	if err != nil {
		return nil, err
	}
	if kind == Integer(0) || kind == Integer(2) {
		tint, err := r.readTintFunction(ctx, v, channels)
		if err != nil {
			return nil, err
		}
		f := &gradientFunction{bytes: 256 + int64(len(tint.samples)), value: func(x float64) (values [4]float64) {
			tint.colorInto(x, values[:channels])
			return
		}}
		if tint.domain[0] == tint.domain[1] {
			values := f.value(tint.domain[0])
			if !finiteFloats(values[:channels]) {
				return f, nil
			}
			return constantGradientFunction(values), nil
		}
		if tint.sampled && (tint.order != 3 || tint.size < 4) {
			if budget := min(maxGradientLinearStops, state.remainingStops()); budget > 0 {
				stops := sampledGradientStops(ctx, tint, budget)
				if state.reserveStops(len(stops)) {
					f.bytes += int64(len(stops)) * 64
					f.linear = func(interval [2]float64) []GradientStop { return gradientDomain(stops, tint.domain, interval) }
				}
			}
		} else if !tint.sampled && (tint.exponent == 0 || tint.exponent == 1) && state.remainingStops() > 0 {
			if tint.exponent == 1 {
				for c := 0; c < channels; c++ {
					if math.IsInf(tint.values[2*c+1]-tint.values[2*c], 0) {
						return f, nil
					}
				}
			}
			stops := []GradientStop{{Position: 0}, {Position: 1}}
			for i, x := range tint.domain {
				for c := 0; c < channels; c++ {
					v := functionValue(math.Pow(x, tint.exponent), tint.values[2*c], tint.values[2*c+1])
					if !finiteFloat(v) {
						return f, nil
					}
					stops[i].Values[c] = v
				}
			}
			stops = clipGradientValues(stops, tint.outputRange)
			if state.reserveStops(len(stops)) {
				f.bytes += int64(len(stops)) * 64
				f.linear = func(interval [2]float64) []GradientStop { return gradientDomain(stops, tint.domain, interval) }
			}
		}
		return f, nil
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
	if rangeValue != nil || kind == Integer(4) {
		limits, err = r.numberArray(rangeValue, 2*channels)
		if err != nil || !finiteFloats(limits) {
			return nil, fmt.Errorf("invalid gradient function range")
		}
		for i := 0; i < len(limits); i += 2 {
			if limits[i] > limits[i+1] {
				return nil, fmt.Errorf("invalid gradient function range")
			}
		}
	}
	if kind == Integer(4) {
		stream, ok := v.(*Stream)
		if !ok {
			return nil, fmt.Errorf("missing calculator function stream")
		}
		data, err := r.DecodeStreamContext(ctx, stream)
		if err != nil {
			return nil, err
		}
		if state.limit > 0 && int64(len(data)) > state.limit-state.bytes {
			return nil, fmt.Errorf("%w: gradient function memory limit exceeded", ErrStreamLimitExceeded)
		}
		program, err := newCalculatorFunction(data, r.warning)
		if err != nil {
			return nil, err
		}
		progBytes := calculatorProgramBytes(program)
		if err := state.reserveBytes(max(int64(len(data)), progBytes)); err != nil {
			return nil, err
		}
		if domain[0] == domain[1] {
			var values [4]float64
			if err := program.evaluate(domain[:1], values[:channels]); err != nil {
				return nil, err
			}
			return constantGradientFunction(clipGradientValue(values, limits)), nil
		}
		expressions, err := affineCalculator(data, 1, channels)
		if err != nil {
			return &gradientFunction{bytes: 256 + progBytes, calculate: func(x float64) (values [4]float64, err error) {
				x = math.Max(domain[0], math.Min(domain[1], x))
				err = program.evaluate([]float64{x}, values[:channels])
				if err != nil {
					return values, err
				}
				return clipGradientValue(values, limits), nil
			}}, nil
		}
		var output [4]float64
		if err := program.evaluate(domain[:1], output[:channels]); err != nil {
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
		if !state.reserveStops(len(stops)) {
			return &gradientFunction{bytes: 256 + progBytes, calculate: func(x float64) (values [4]float64, err error) {
				x = math.Max(domain[0], math.Min(domain[1], x))
				err = program.evaluate([]float64{x}, values[:channels])
				if err != nil {
					return values, err
				}
				return clipGradientValue(values, limits), nil
			}}, nil
		}
		return &gradientFunction{
			bytes:  256 + int64(len(stops))*64,
			value:  func(x float64) [4]float64 { return gradientValue(stops, functionPosition(x, domain[0], domain[1])) },
			linear: func(interval [2]float64) []GradientStop { return gradientDomain(stops, domain, interval) },
		}, nil
	}
	if kind != Integer(3) {
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
	if domain[0] == domain[1] && len(functions) != 1 {
		return nil, fmt.Errorf("invalid stitching function domain")
	}
	bounds, err := r.numberArray(dict["Bounds"], len(functions)-1)
	if err != nil || !finiteFloats(bounds) {
		return nil, fmt.Errorf("invalid gradient stitching bounds")
	}
	encode, err := r.numberArray(dict["Encode"], len(functions)*2)
	if err != nil || !finiteFloats(encode) {
		return nil, fmt.Errorf("invalid gradient stitching encode")
	}
	points := append(append([]float64{domain[0]}, bounds...), domain[1])
	parts := make([]*gradientFunction, len(functions))
	seen := make(map[*gradientFunction]bool, len(functions))
	size := int64(256) + int64(len(functions))*24
	linear := true
	for i, object := range functions {
		if points[i] > points[i+1] {
			return nil, fmt.Errorf("invalid gradient stitching bounds")
		}
		parts[i], err = r.readGradientFunctionState(ctx, object, channels, depth+1, state)
		if err != nil {
			return nil, err
		}
		if !seen[parts[i]] {
			seen[parts[i]] = true
			size += parts[i].bytes
		}
		linear = linear && (points[i] == points[i+1] && i < len(functions)-1 || parts[i].linear != nil)
	}
	f := &gradientFunction{bytes: size, calculate: func(x float64) (values [4]float64, err error) {
		x = math.Max(domain[0], math.Min(domain[1], x))
		for i, part := range parts {
			if x < points[i+1] || i == len(parts)-1 {
				t := functionValue(functionPosition(x, points[i], points[i+1]), encode[2*i], encode[2*i+1])
				values, err = part.evaluate(t)
				if err != nil {
					return values, err
				}
				break
			}
		}
		return clipGradientValue(values, limits), nil
	}}
	if domain[0] == domain[1] {
		values, err := f.evaluate(domain[0])
		if err != nil {
			return nil, err
		}
		return constantGradientFunction(values), nil
	}
	budget := min(maxGradientLinearStops, state.remainingStops())
	if linear && budget > 0 {
		var stops []GradientStop
		for i, part := range parts {
			if i&255 == 0 {
				if err := ctx.Err(); err != nil {
					return nil, err
				}
			}
			if points[i] == points[i+1] {
				if i == len(parts)-1 {
					partStops := part.linear([2]float64{encode[2*i], encode[2*i]})
					if len(partStops) == 0 || len(partStops) > budget-len(stops) {
						stops = nil
						break
					}
					for _, stop := range partStops {
						stop.Position = 1
						stops = append(stops, stop)
					}
				}
				continue
			}
			partStops := part.linear([2]float64{encode[2*i], encode[2*i+1]})
			if len(partStops) == 0 || len(partStops) > budget-len(stops) {
				stops = nil
				break
			}
			for _, stop := range partStops {
				position := gradientPosition(points[i], points[i+1], stop.Position)
				stop.Position = functionPosition(position, domain[0], domain[1])
				stops = append(stops, stop)
			}
		}
		if len(stops) > 0 {
			stops = clipGradientValues(stops, limits)
			if len(stops) <= budget && state.reserveStops(len(stops)) {
				f.bytes += int64(len(stops)) * 64
				f.linear = func(interval [2]float64) []GradientStop { return gradientDomain(stops, domain, interval) }
			}
		}
	}
	return f, nil
}
