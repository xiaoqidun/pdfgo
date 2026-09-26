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

// separationSpace 保存分色名、备用设备空间及着色变换
type separationSpace struct {
	name      Name
	alternate Name
	lab       *labSpace
	transform *tintFunction
}

// tintFunction 保存单输入函数的定义及采样数据
type tintFunction struct {
	domain, values, decode, outputRange []float64
	samples                             []byte
	size, bits, channels                int
	exponent                            float64
	encoded                             [2]float64
	sampled                             bool
}

// readSeparation 读取分色颜色空间及其着色函数
// 入参: space 分色颜色空间数组
// 返回: *separationSpace 分色定义, error 错误信息
func (r *Reader) readSeparation(space Array) (*separationSpace, error) {
	if len(space) != 4 || space[0] != Name("Separation") {
		return nil, fmt.Errorf("invalid Separation color space")
	}
	name, ok := space[1].(Name)
	if !ok {
		return nil, fmt.Errorf("invalid Separation colorant")
	}
	object, err := r.Resolve(space[2])
	if err != nil {
		return nil, err
	}
	alternate, ok := object.(Name)
	var lab *labSpace
	if !ok {
		array, arrayOK := object.(Array)
		if !arrayOK || len(array) != 2 || array[0] != Name("Lab") {
			return nil, &UnsupportedError{Feature: "Separation alternate color space"}
		}
		lab, err = r.readLab(array)
		if err != nil {
			return nil, err
		}
		alternate = "Lab"
	}
	channels := 0
	switch alternate {
	case "DeviceGray":
		channels = 1
	case "DeviceRGB":
		channels = 3
	case "Lab":
		channels = 3
	case "DeviceCMYK":
		channels = 4
	default:
		return nil, &UnsupportedError{Feature: "Separation alternate color space"}
	}
	transform, err := r.readTintFunction(space[3], channels)
	if err != nil {
		return nil, err
	}
	return &separationSpace{name: name, alternate: alternate, lab: lab, transform: transform}, nil
}

// readTintFunction 读取一维采样或指数着色函数
// 入参: object 函数对象, channels 输出分量数
// 返回: *tintFunction 函数定义, error 错误信息
func (r *Reader) readTintFunction(object Object, channels int) (*tintFunction, error) {
	value, err := r.Resolve(object)
	if err != nil {
		return nil, err
	}
	var dict Dictionary
	var stream *Stream
	switch v := value.(type) {
	case Dictionary:
		dict = v
	case *Stream:
		dict, stream = v.Dictionary, v
	default:
		return nil, fmt.Errorf("invalid tint function")
	}
	domain, err := r.numberArray(dict["Domain"], 2)
	if err != nil || domain[0] >= domain[1] {
		return nil, fmt.Errorf("invalid tint function domain")
	}
	f := &tintFunction{domain: domain, channels: channels}
	switch dict["FunctionType"] {
	case Integer(0):
		if stream == nil {
			return nil, fmt.Errorf("missing sampled tint data")
		}
		size, err := r.numberArray(dict["Size"], 1)
		if err != nil || size[0] < 1 || size[0] > float64(int(^uint(0)>>1)) || math.Trunc(size[0]) != size[0] {
			return nil, fmt.Errorf("invalid sampled tint size")
		}
		bits, err := r.number(dict["BitsPerSample"])
		if err != nil || bits != 1 && bits != 2 && bits != 4 && bits != 8 && bits != 12 && bits != 16 && bits != 24 && bits != 32 {
			return nil, fmt.Errorf("invalid sampled tint depth")
		}
		if dict["Order"] != nil && dict["Order"] != Integer(1) {
			return nil, &UnsupportedError{Feature: "cubic sampled tint function"}
		}
		f.decode, err = r.numberArray(dict["Decode"], 2*channels)
		if dict["Decode"] == nil {
			f.decode, err = r.numberArray(dict["Range"], 2*channels)
		}
		if err != nil {
			return nil, fmt.Errorf("invalid sampled tint decode: %w", err)
		}
		f.values, err = r.numberArray(dict["Range"], 2*channels)
		if err != nil {
			return nil, fmt.Errorf("invalid sampled tint range: %w", err)
		}
		for n := 0; n < channels; n++ {
			if f.values[2*n] > f.values[2*n+1] {
				return nil, fmt.Errorf("invalid sampled tint range")
			}
		}
		f.encoded = [2]float64{0, size[0] - 1}
		if dict["Encode"] != nil {
			encode, err := r.numberArray(dict["Encode"], 2)
			if err != nil {
				return nil, err
			}
			copy(f.encoded[:], encode)
		}
		f.samples, err = stream.Decode()
		if err != nil {
			return nil, err
		}
		f.size, f.bits, f.sampled = int(size[0]), int(bits), true
		if uint64(f.size) > uint64(len(f.samples))*8/uint64(channels*f.bits) {
			return nil, fmt.Errorf("truncated sampled tint data")
		}
	case Integer(2):
		f.exponent, err = r.number(dict["N"])
		if err != nil || f.exponent <= 0 || math.IsNaN(f.exponent) || math.IsInf(f.exponent, 0) {
			return nil, fmt.Errorf("invalid tint exponent")
		}
		f.values = make([]float64, channels*2)
		for n := 0; n < channels; n++ {
			f.values[2*n+1] = 1
		}
		for _, entry := range []struct {
			key   Name
			index int
		}{{"C0", 0}, {"C1", 1}} {
			if dict[entry.key] == nil {
				continue
			}
			values, err := r.numberArray(dict[entry.key], channels)
			if err != nil {
				return nil, err
			}
			for n, v := range values {
				f.values[2*n+entry.index] = v
			}
		}
		if dict["Range"] != nil {
			f.outputRange, err = r.numberArray(dict["Range"], 2*channels)
			if err != nil {
				return nil, err
			}
			for n := 0; n < channels; n++ {
				if f.outputRange[2*n] > f.outputRange[2*n+1] {
					return nil, fmt.Errorf("invalid tint function range")
				}
			}
		}
	default:
		return nil, &UnsupportedError{Feature: "tint function type"}
	}
	return f, nil
}

// color 计算指定浓度在备用空间中的颜色分量
// 入参: tint 分色浓度
// 返回: []float64 备用空间分量
func (f *tintFunction) color(tint float64) []float64 {
	tint = math.Max(f.domain[0], math.Min(f.domain[1], tint))
	out := make([]float64, f.channels)
	if !f.sampled {
		t := math.Pow(tint, f.exponent)
		for n := range out {
			out[n] = f.values[2*n] + t*(f.values[2*n+1]-f.values[2*n])
			if f.outputRange != nil {
				out[n] = math.Max(f.outputRange[2*n], math.Min(f.outputRange[2*n+1], out[n]))
			}
		}
		return out
	}
	position := f.encoded[0] + (tint-f.domain[0])/(f.domain[1]-f.domain[0])*(f.encoded[1]-f.encoded[0])
	position = math.Max(0, math.Min(float64(f.size-1), position))
	low := int(math.Floor(position))
	high := min(low+1, f.size-1)
	weight := position - float64(low)
	for n := range out {
		a := f.sample(low*f.channels + n)
		b := f.sample(high*f.channels + n)
		v := a + weight*(b-a)
		v = f.decode[2*n] + v/(math.Exp2(float64(f.bits))-1)*(f.decode[2*n+1]-f.decode[2*n])
		out[n] = math.Max(f.values[2*n], math.Min(f.values[2*n+1], v))
	}
	return out
}

// sample 读取采样表中的单个高位优先分量
// 入参: index 分量索引
// 返回: float64 采样值
func (f *tintFunction) sample(index int) float64 {
	bit := index * f.bits
	var value uint32
	for n := 0; n < f.bits; n++ {
		value = value<<1 | uint32(f.samples[(bit+n)/8]>>(7-(bit+n)%8)&1)
	}
	return float64(value)
}

// paint 将分色浓度映射到备用设备颜色空间
// 入参: tint 分色浓度
// 返回: Paint 备用空间颜色, error 错误信息
func (s *separationSpace) paint(tint float64) (Paint, error) {
	if math.IsNaN(tint) || math.IsInf(tint, 0) {
		return Paint{}, fmt.Errorf("invalid Separation tint")
	}
	tint = math.Max(0, math.Min(1, tint))
	if s.name == "None" {
		return Paint{RGB: [3]float64{1, 1, 1}, Alpha: 0}, nil
	}
	if s.name == "All" {
		return Paint{CMYK: &[4]float64{tint, tint, tint, tint}}, nil
	}
	values := s.transform.color(tint)
	paint := Paint{}
	switch s.alternate {
	case "DeviceGray":
		paint.RGB = [3]float64{values[0], values[0], values[0]}
	case "DeviceRGB":
		copy(paint.RGB[:], values)
	case "DeviceCMYK":
		paint.CMYK = &[4]float64{values[0], values[1], values[2], values[3]}
	case "Lab":
		color := s.lab.color(values[0], values[1], values[2])
		paint.RGB = [3]float64{float64(color.R) / 65535, float64(color.G) / 65535, float64(color.B) / 65535}
	}
	return paint, nil
}
