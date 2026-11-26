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

// maxSampledEvalPoints 限制单次多维采样插值展开的最大角点或控制点数，防止高维组合爆炸
const maxSampledEvalPoints = 4096

// sampledFunction 保存多输入采样函数及高位优先的数据表
type sampledFunction struct {
	domain, encode, decode, bounds []float64
	sizes, strides, activeDims     []int
	table                          tintFunction
	order                          int
	bytes                          int64
}

// sampledMemoryBytes 估算采样函数及其数据表占用的近似字节数
// 入参: f 采样函数
// 返回: int64 估算字节数
func sampledMemoryBytes(f *sampledFunction) int64 {
	if f == nil {
		return 0
	}
	if f.bytes > 0 {
		return f.bytes
	}
	return 256 + int64(len(f.table.samples))
}

// sampledSplineSpan 计算Catmull-Rom样条权重，边界使用线性外推的虚拟样本
// 入参: position 采样位置, size 样本数量
// 返回: [4]int 样本索引, [4]float64 插值权重
func sampledSplineSpan(position float64, size int) ([4]int, [4]float64) {
	if size <= 1 {
		return [4]int{0}, [4]float64{1}
	}
	position = math.Max(0, math.Min(float64(size-1), position))
	lower := int(position)
	t := position - float64(lower)
	if size < 4 {
		return [4]int{lower, min(lower+1, size-1)}, [4]float64{1 - t, t}
	}
	t2, t3 := t*t, t*t*t
	indices := [4]int{lower - 1, lower, lower + 1, lower + 2}
	weights := [4]float64{-.5*t + t2 - .5*t3, 1 - 2.5*t2 + 1.5*t3, .5*t + 2*t2 - 1.5*t3, -.5*t2 + .5*t3}
	if lower == 0 {
		indices[0] = 0
		weights[1] += 2 * weights[0]
		weights[2] -= weights[0]
		weights[0] = 0
	}
	if lower >= size-2 {
		indices[2], indices[3] = min(indices[2], size-1), size-1
		weights[2] += 2 * weights[3]
		weights[1] -= weights[3]
		weights[3] = 0
	}
	return indices, weights
}

// readSampledFunctionContext 在本次取消上下文内解码和验证采样函数
// 入参: ctx 取消上下文, stream 函数数据流, inputs 输入数量, outputs 输出数量
// 返回: *sampledFunction 函数, error 格式、解码、超限或取消错误
func (r *Reader) readSampledFunctionContext(ctx context.Context, stream *Stream, inputs, outputs int) (*sampledFunction, error) {
	if ctx == nil || stream == nil || inputs <= 0 || outputs <= 0 {
		return nil, fmt.Errorf("invalid sampled function evaluation")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	state, _ := ctx.Value(gradientParseStateKey{}).(*gradientParseState)
	key := streamFunctionKey{stream: stream, inputs: inputs, outputs: outputs}
	if state != nil {
		if cached := state.sampled[key]; cached != nil {
			return cached, nil
		}
	}
	dict := stream.Dictionary
	f := &sampledFunction{}
	var err error
	f.domain, err = r.numberArray(dict["Domain"], 2*inputs)
	if err != nil {
		return nil, err
	}
	f.bounds, err = r.numberArray(dict["Range"], 2*outputs)
	if err != nil {
		return nil, err
	}
	if !finiteFloats(f.domain) || !finiteFloats(f.bounds) {
		return nil, fmt.Errorf("invalid sampled function bounds")
	}
	for _, ranges := range [][]float64{f.domain, f.bounds} {
		for i := 0; i < len(ranges); i += 2 {
			if ranges[i] > ranges[i+1] {
				return nil, fmt.Errorf("invalid sampled function bounds")
			}
		}
	}
	sizes, err := r.numberArray(dict["Size"], inputs)
	if err != nil {
		return nil, err
	}
	if !finiteFloats(sizes) {
		return nil, fmt.Errorf("invalid or truncated sampled function size")
	}
	bits, err := r.number(dict["BitsPerSample"])
	if err != nil || bits != 1 && bits != 2 && bits != 4 && bits != 8 && bits != 12 && bits != 16 && bits != 24 && bits != 32 {
		return nil, fmt.Errorf("invalid sampled function depth")
	}
	order, err := r.Resolve(dict["Order"])
	if err != nil {
		return nil, err
	}
	if order != nil && order != Integer(1) && order != Integer(3) {
		return nil, fmt.Errorf("invalid sampled function order")
	}
	if order == Integer(3) {
		f.order = 3
	}
	if state != nil {
		if err := checkSampledSizesBudget(sizes, outputs, int(bits), state); err != nil {
			return nil, err
		}
	}
	data, err := r.DecodeStreamContext(ctx, stream)
	if err != nil {
		return nil, err
	}
	if state != nil {
		if err := state.reserveBytes(int64(len(data))); err != nil {
			return nil, err
		}
	}
	f.table = tintFunction{samples: data, bits: int(bits), channels: outputs}
	f.sizes, f.strides = make([]int, inputs), make([]int, inputs)
	f.encode = make([]float64, 2*inputs)
	count := outputs
	evalPoints := 1
	for i, size := range sizes {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if size < 1 || math.Trunc(size) != size || size >= float64(int(^uint(0)>>1))/float64(count) || size > float64(len(data))*8/(float64(count)*bits) {
			return nil, fmt.Errorf("invalid or truncated sampled function size")
		}
		f.sizes[i], f.strides[i] = int(size), count
		count *= int(size)
		f.encode[2*i+1] = size - 1
		if int(size) > 1 {
			branch := 2
			if f.order == 3 && int(size) >= 4 {
				branch = 4
			}
			if evalPoints > maxSampledEvalPoints/branch {
				return nil, fmt.Errorf("sampled function interpolation complexity exceeds limit")
			}
			evalPoints *= branch
			f.activeDims = append(f.activeDims, i)
		}
	}
	value, err := r.Resolve(dict["Encode"])
	if err != nil {
		return nil, err
	}
	if value != nil {
		f.encode, err = r.numberArray(value, 2*inputs)
		if err != nil {
			return nil, err
		}
		if !finiteFloats(f.encode) {
			return nil, fmt.Errorf("invalid sampled function encode")
		}
	}
	f.decode = f.bounds
	customDecode := false
	value, err = r.Resolve(dict["Decode"])
	if err != nil {
		return nil, err
	}
	if value != nil {
		f.decode, err = r.numberArray(value, 2*outputs)
		if err != nil {
			return nil, err
		}
		if !finiteFloats(f.decode) {
			return nil, fmt.Errorf("invalid sampled function decode")
		}
		customDecode = true
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	f.bytes = 256 + int64(len(data)) + int64(len(f.domain)+len(f.encode)+len(f.bounds)+len(f.sizes)+len(f.strides)+len(f.activeDims))*8
	if customDecode {
		f.bytes += int64(len(f.decode)) * 8
	}
	if state != nil {
		if state.sampled == nil {
			state.sampled = make(map[streamFunctionKey]*sampledFunction)
		}
		state.sampled[key] = f
	}
	return f, nil
}

// evaluate 按第一维最快变化的采样顺序进行插值
// 入参: inputs 输入分量, outputs 输出缓冲区
func (f *sampledFunction) evaluate(inputs, outputs []float64) {
	if f.order == 3 {
		f.evaluateCubic(inputs, outputs)
		return
	}
	for _, v := range inputs {
		if math.IsNaN(v) {
			for c := range outputs {
				outputs[c] = math.NaN()
			}
			return
		}
	}
	active := f.activeDims
	var lowerBuffer, upperBuffer [12]int
	var weightBuffer [12]float64
	lower, upper, weights := lowerBuffer[:len(active)], upperBuffer[:len(active)], weightBuffer[:len(active)]
	for k, i := range active {
		value := math.Max(f.domain[2*i], math.Min(f.domain[2*i+1], inputs[i]))
		value = functionValue(functionPosition(value, f.domain[2*i], f.domain[2*i+1]), f.encode[2*i], f.encode[2*i+1])
		value = math.Max(0, math.Min(float64(f.sizes[i]-1), value))
		if math.IsNaN(value) {
			for c := range outputs {
				outputs[c] = math.NaN()
			}
			return
		}
		lower[k] = int(value)
		upper[k] = min(lower[k]+1, f.sizes[i]-1)
		weights[k] = value - float64(lower[k])
	}
	clear(outputs)
	steps := 0
	var accumulate func(int, int, float64)
	accumulate = func(dimension, index int, weight float64) {
		if steps >= maxSampledEvalPoints {
			return
		}
		if dimension == len(active) {
			steps++
			for c := range outputs {
				outputs[c] += weight * f.table.sample(index+c)
			}
			return
		}
		stride := f.strides[active[dimension]]
		if lower[dimension] == upper[dimension] {
			accumulate(dimension+1, index+lower[dimension]*stride, weight)
			return
		}
		if weights[dimension] != 1 {
			accumulate(dimension+1, index+lower[dimension]*stride, weight*(1-weights[dimension]))
		}
		if weights[dimension] != 0 {
			accumulate(dimension+1, index+upper[dimension]*stride, weight*weights[dimension])
		}
	}
	accumulate(0, 0, 1)
	maxSample := float64(uint64(1)<<uint(f.table.bits) - 1)
	for c, value := range outputs {
		value = functionValue(value/maxSample, f.decode[2*c], f.decode[2*c+1])
		outputs[c] = math.Max(f.bounds[2*c], math.Min(f.bounds[2*c+1], value))
	}
}

// evaluateCubic 使用张量积三次样条，少于四个样本的维度使用线性插值
// 入参: inputs 输入分量, outputs 输出缓冲区
func (f *sampledFunction) evaluateCubic(inputs, outputs []float64) {
	for _, v := range inputs {
		if math.IsNaN(v) {
			for c := range outputs {
				outputs[c] = math.NaN()
			}
			return
		}
	}
	active := f.activeDims
	var indexBuffer [12][4]int
	var weightBuffer [12][4]float64
	indices, weights := indexBuffer[:len(active)], weightBuffer[:len(active)]
	for k, i := range active {
		value := math.Max(f.domain[2*i], math.Min(f.domain[2*i+1], inputs[i]))
		value = functionValue(functionPosition(value, f.domain[2*i], f.domain[2*i+1]), f.encode[2*i], f.encode[2*i+1])
		if math.IsNaN(value) {
			for c := range outputs {
				outputs[c] = math.NaN()
			}
			return
		}
		indices[k], weights[k] = sampledSplineSpan(value, f.sizes[i])
	}
	clear(outputs)
	steps := 0
	var accumulate func(int, int, float64)
	accumulate = func(dimension, index int, weight float64) {
		if steps >= maxSampledEvalPoints {
			return
		}
		if dimension == len(active) {
			steps++
			for c := range outputs {
				outputs[c] += weight * f.table.sample(index+c)
			}
			return
		}
		stride := f.strides[active[dimension]]
		for i, factor := range weights[dimension] {
			if factor != 0 {
				accumulate(dimension+1, index+indices[dimension][i]*stride, weight*factor)
			}
		}
	}
	accumulate(0, 0, 1)
	maxSample := float64(uint64(1)<<uint(f.table.bits) - 1)
	for c, value := range outputs {
		value = functionValue(value/maxSample, f.decode[2*c], f.decode[2*c+1])
		outputs[c] = math.Max(f.bounds[2*c], math.Min(f.bounds[2*c+1], value))
	}
}
