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
}

// readSampledFunction 读取线性插值采样函数并验证数据范围
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
	for index, ranges := range [][]float64{f.domain, f.bounds} {
		for i := 0; i < len(ranges); i += 2 {
			if ranges[i] > ranges[i+1] || index == 0 && ranges[i] == ranges[i+1] {
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
	if order != nil && order != Integer(1) {
		return nil, &UnsupportedError{Feature: "cubic sampled function"}
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

// evaluate 按第一维最快变化的采样顺序进行多线性插值
// 入参: inputs 输入分量, outputs 输出缓冲区
func (f *sampledFunction) evaluate(inputs, outputs []float64) {
	var lower, upper [4]int
	var weights [4]float64
	for i, value := range inputs {
		value = math.Max(f.domain[2*i], math.Min(f.domain[2*i+1], value))
		value = f.encode[2*i] + (value-f.domain[2*i])/(f.domain[2*i+1]-f.domain[2*i])*(f.encode[2*i+1]-f.encode[2*i])
		value = math.Max(0, math.Min(float64(f.sizes[i]-1), value))
		lower[i] = int(value)
		upper[i] = min(lower[i]+1, f.sizes[i]-1)
		weights[i] = value - float64(lower[i])
	}
	clear(outputs)
	for corner := 0; corner < 1<<len(inputs); corner++ {
		index, weight := 0, 1.0
		for i := range inputs {
			if corner&(1<<i) == 0 {
				index += lower[i] * f.strides[i]
				weight *= 1 - weights[i]
			} else {
				index += upper[i] * f.strides[i]
				weight *= weights[i]
			}
		}
		if weight == 0 {
			continue
		}
		for c := range outputs {
			outputs[c] += weight * f.table.sample(index+c)
		}
	}
	maxSample := math.Exp2(float64(f.table.bits)) - 1
	for c, value := range outputs {
		value = f.decode[2*c] + value/maxSample*(f.decode[2*c+1]-f.decode[2*c])
		outputs[c] = math.Max(f.bounds[2*c], math.Min(f.bounds[2*c+1], value))
	}
}
