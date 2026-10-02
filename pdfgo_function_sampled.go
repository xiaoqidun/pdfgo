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

// sampledFunction 保存多输入采样函数及高位优先的数据表
type sampledFunction struct {
	domain, encode, decode, bounds []float64
	sizes, strides                 []int
	table                          tintFunction
	order                          int
}

// readSampledFunction 读取线性或三次插值采样函数并验证数据范围
// 入参: stream 函数数据流, inputs 输入数量, outputs 输出数量
// 返回: *sampledFunction 函数, error 格式或能力错误
func (r *Reader) readSampledFunction(stream *Stream, inputs, outputs int) (*sampledFunction, error) {
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
	data, err := stream.Decode()
	if err != nil {
		return nil, err
	}
	f.table = tintFunction{samples: data, bits: int(bits), channels: outputs}
	f.sizes, f.strides = make([]int, inputs), make([]int, inputs)
	f.encode = make([]float64, 2*inputs)
	count := outputs
	for i, size := range sizes {
		if size < 1 || math.Trunc(size) != size || size >= float64(int(^uint(0)>>1))/float64(count) || size > float64(len(data))*8/(float64(count)*bits) {
			return nil, fmt.Errorf("invalid or truncated sampled function size")
		}
		f.sizes[i], f.strides[i] = int(size), count
		count *= int(size)
		f.encode[2*i+1] = size - 1
	}
	if dict["Encode"] != nil {
		f.encode, err = r.numberArray(dict["Encode"], 2*inputs)
		if err != nil {
			return nil, err
		}
	}
	f.decode = f.bounds
	if dict["Decode"] != nil {
		f.decode, err = r.numberArray(dict["Decode"], 2*outputs)
		if err != nil {
			return nil, err
		}
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
	var lowerBuffer, upperBuffer [32]int
	var weightBuffer [32]float64
	lower, upper, weights := lowerBuffer[:min(len(inputs), 32)], upperBuffer[:min(len(inputs), 32)], weightBuffer[:min(len(inputs), 32)]
	if len(inputs) > 32 {
		lower, upper, weights = make([]int, len(inputs)), make([]int, len(inputs)), make([]float64, len(inputs))
	}
	for i, value := range inputs {
		value = math.Max(f.domain[2*i], math.Min(f.domain[2*i+1], value))
		value = functionValue(functionPosition(value, f.domain[2*i], f.domain[2*i+1]), f.encode[2*i], f.encode[2*i+1])
		value = math.Max(0, math.Min(float64(f.sizes[i]-1), value))
		lower[i] = int(value)
		upper[i] = min(lower[i]+1, f.sizes[i]-1)
		weights[i] = value - float64(lower[i])
	}
	clear(outputs)
	var accumulate func(int, int, float64)
	accumulate = func(dimension, index int, weight float64) {
		if dimension == len(inputs) {
			for c := range outputs {
				outputs[c] += weight * f.table.sample(index+c)
			}
			return
		}
		if weights[dimension] != 1 {
			accumulate(dimension+1, index+lower[dimension]*f.strides[dimension], weight*(1-weights[dimension]))
		}
		if weights[dimension] != 0 {
			accumulate(dimension+1, index+upper[dimension]*f.strides[dimension], weight*weights[dimension])
		}
	}
	accumulate(0, 0, 1)
	maxSample := math.Exp2(float64(f.table.bits)) - 1
	for c, value := range outputs {
		value = functionValue(value/maxSample, f.decode[2*c], f.decode[2*c+1])
		outputs[c] = math.Max(f.bounds[2*c], math.Min(f.bounds[2*c+1], value))
	}
}

// evaluateCubic 使用张量积三次样条，少于四个样本的维度使用线性插值
// 入参: inputs 输入分量, outputs 输出缓冲区
func (f *sampledFunction) evaluateCubic(inputs, outputs []float64) {
	var indexBuffer [32][4]int
	var weightBuffer [32][4]float64
	indices, weights := indexBuffer[:min(len(inputs), 32)], weightBuffer[:min(len(inputs), 32)]
	if len(inputs) > 32 {
		indices, weights = make([][4]int, len(inputs)), make([][4]float64, len(inputs))
	}
	for i, value := range inputs {
		value = math.Max(f.domain[2*i], math.Min(f.domain[2*i+1], value))
		value = functionValue(functionPosition(value, f.domain[2*i], f.domain[2*i+1]), f.encode[2*i], f.encode[2*i+1])
		indices[i], weights[i] = sampledSplineSpan(value, f.sizes[i])
	}
	clear(outputs)
	var accumulate func(int, int, float64)
	accumulate = func(dimension, index int, weight float64) {
		if dimension == len(inputs) {
			for c := range outputs {
				outputs[c] += weight * f.table.sample(index+c)
			}
			return
		}
		for i, factor := range weights[dimension] {
			if factor != 0 {
				accumulate(dimension+1, index+indices[dimension][i]*f.strides[dimension], weight*factor)
			}
		}
	}
	accumulate(0, 0, 1)
	maxSample := math.Exp2(float64(f.table.bits)) - 1
	for c, value := range outputs {
		value = functionValue(value/maxSample, f.decode[2*c], f.decode[2*c+1])
		outputs[c] = math.Max(f.bounds[2*c], math.Min(f.bounds[2*c+1], value))
	}
}

// sampledSplineSpan 计算Catmull-Rom样条权重，边界使用线性外推的虚拟样本
// 入参: position 采样位置, size 样本数量
// 返回: [4]int 样本索引, [4]float64 插值权重
func sampledSplineSpan(position float64, size int) ([4]int, [4]float64) {
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
