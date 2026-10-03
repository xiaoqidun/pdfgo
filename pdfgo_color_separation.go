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
	"encoding/binary"
	"fmt"
	"math"
)

// separationSpace 保存分色名、备用设备空间及着色变换
type separationSpace struct {
	name      Name
	alternate Name
	space     *ColorSpace
	lab       *labSpace
	icc       *iccColorSpace
	calRGB    *calRGBSpace
	gray      bool
	transform *gradientFunction
}

// tintFunction 保存单输入函数的定义及采样数据
type tintFunction struct {
	domain, values, decode, outputRange []float64
	samples                             []byte
	size, bits, channels, order         int
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
	colorant, err := r.Resolve(space[1])
	if err != nil {
		return nil, err
	}
	name, ok := colorant.(Name)
	if !ok {
		return nil, fmt.Errorf("invalid Separation colorant")
	}
	if name == "All" || name == "None" {
		if err := r.ignoredTintSpace(space); err != nil {
			return nil, err
		}
		model := Name("DeviceRGB")
		var transform *gradientFunction
		if name == "All" {
			model = "DeviceCMYK"
			transform, err = r.readGradientFunction(Dictionary{"FunctionType": Integer(2), "Domain": Array{Integer(0), Integer(1)}, "C0": Array{Integer(0), Integer(0), Integer(0), Integer(0)}, "C1": Array{Integer(1), Integer(1), Integer(1), Integer(1)}, "N": Integer(1)}, 4, 0)
			if err != nil {
				return nil, err
			}
		}
		return &separationSpace{name: name, alternate: model, space: &ColorSpace{Model: model}, transform: transform}, nil
	}
	object, err := r.resolveColorSpace(space[2])
	if err != nil {
		return nil, err
	}
	alternate, ok := object.(Name)
	var lab *labSpace
	var icc *iccColorSpace
	var calibrated *calRGBSpace
	gray := false
	if !ok {
		array, arrayOK := object.(Array)
		if !arrayOK || len(array) != 2 {
			return nil, &UnsupportedError{Feature: "Separation alternate color space"}
		}
		switch array[0] {
		case Name("Lab"):
			lab, err = r.readLab(array)
			alternate = "Lab"
		case Name("ICCBased"):
			icc, err = r.readICCColorSpace(array)
			alternate = "ICCBased"
		case Name("CalRGB"):
			calibrated, err = r.readCalRGB(array)
			alternate = "CalRGB"
		case Name("CalGray"):
			calibrated, err = r.readCalGray(array)
			alternate, gray = "CalGray", true
		default:
			return nil, &UnsupportedError{Feature: "Separation alternate color space"}
		}
		if err != nil {
			return nil, err
		}
	}
	channels := 0
	switch alternate {
	case "DeviceGray", "CalGray":
		channels = 1
	case "DeviceRGB":
		channels = 3
	case "ICCBased":
		channels = icc.components()
	case "Lab", "CalRGB":
		channels = 3
	case "DeviceCMYK":
		channels = 4
	default:
		return nil, &UnsupportedError{Feature: "Separation alternate color space"}
	}
	transform, err := r.readGradientFunction(space[3], channels, 0)
	if err != nil {
		return nil, err
	}
	source, _, err := r.readDeviceNAlternate(object)
	if err != nil {
		return nil, err
	}
	return &separationSpace{name: name, alternate: alternate, space: source, lab: lab, icc: icc, calRGB: calibrated, gray: gray, transform: transform}, nil
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
	if err != nil || domain[0] > domain[1] {
		return nil, fmt.Errorf("invalid tint function domain")
	}
	f := &tintFunction{domain: domain, channels: channels}
	kind, err := r.Resolve(dict["FunctionType"])
	if err != nil {
		return nil, err
	}
	switch kind {
	case Integer(0):
		if stream == nil {
			return nil, fmt.Errorf("missing sampled tint data")
		}
		sampled, err := r.readSampledFunction(stream, 1, channels)
		if err != nil {
			return nil, err
		}
		f.domain, f.decode, f.values = sampled.domain, sampled.decode, sampled.bounds
		f.encoded = [2]float64{sampled.encode[0], sampled.encode[1]}
		f.size, f.bits, f.samples = sampled.sizes[0], sampled.table.bits, sampled.table.samples
		f.order, f.sampled = sampled.order, true
	case Integer(2):
		f.exponent, err = r.number(dict["N"])
		if err != nil || math.IsNaN(f.exponent) || math.IsInf(f.exponent, 0) {
			return nil, fmt.Errorf("invalid tint exponent")
		}
		if math.Trunc(f.exponent) != f.exponent && domain[0] < 0 || f.exponent < 0 && domain[0] <= 0 && domain[1] >= 0 {
			return nil, fmt.Errorf("undefined tint function domain")
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
				if channels != 1 {
					return nil, fmt.Errorf("invalid default tint component count")
				}
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
	out := make([]float64, f.channels)
	f.colorInto(tint, out)
	return out
}

// colorInto 将着色函数结果写入已有分量缓冲区
// 入参: tint 输入浓度, out 输出分量缓冲区
func (f *tintFunction) colorInto(tint float64, out []float64) {
	tint = math.Max(f.domain[0], math.Min(f.domain[1], tint))
	if !f.sampled {
		t := math.Pow(tint, f.exponent)
		for n := range out {
			out[n] = f.values[2*n] + t*(f.values[2*n+1]-f.values[2*n])
			if f.outputRange != nil {
				out[n] = math.Max(f.outputRange[2*n], math.Min(f.outputRange[2*n+1], out[n]))
			}
		}
		return
	}
	position := functionValue(functionPosition(tint, f.domain[0], f.domain[1]), f.encoded[0], f.encoded[1])
	position = math.Max(0, math.Min(float64(f.size-1), position))
	if f.order == 3 && f.size >= 4 {
		indices, weights := sampledSplineSpan(position, f.size)
		for n := range out {
			var value float64
			for i, weight := range weights {
				if weight != 0 {
					value += weight * f.sample(indices[i]*f.channels+n)
				}
			}
			value = functionValue(value/(math.Exp2(float64(f.bits))-1), f.decode[2*n], f.decode[2*n+1])
			out[n] = math.Max(f.values[2*n], math.Min(f.values[2*n+1], value))
		}
		return
	}
	low := int(math.Floor(position))
	high := min(low+1, f.size-1)
	weight := position - float64(low)
	for n := range out {
		a := f.sample(low*f.channels + n)
		b := f.sample(high*f.channels + n)
		v := a + weight*(b-a)
		v = functionValue(v/(math.Exp2(float64(f.bits))-1), f.decode[2*n], f.decode[2*n+1])
		out[n] = math.Max(f.values[2*n], math.Min(f.values[2*n+1], v))
	}
}

// sample 读取采样表中的单个高位优先分量
// 入参: index 分量索引
// 返回: float64 采样值
func (f *tintFunction) sample(index int) float64 {
	bit := index * f.bits
	data := f.samples[bit/8:]
	switch f.bits {
	case 1, 2, 4:
		return float64(data[0] >> (8 - f.bits - bit%8) & byte((1<<f.bits)-1))
	case 8:
		return float64(data[0])
	case 12:
		return float64(binary.BigEndian.Uint16(data) >> (4 - bit%8) & 4095)
	case 16:
		return float64(binary.BigEndian.Uint16(data))
	case 24:
		return float64(uint32(data[0])<<16 | uint32(data[1])<<8 | uint32(data[2]))
	case 32:
		return float64(binary.BigEndian.Uint32(data))
	}
	var value uint32
	for n := 0; n < f.bits; n++ {
		value = value<<1 | uint32(f.samples[(bit+n)/8]>>(7-(bit+n)%8)&1)
	}
	return float64(value)
}

// paint 将分色浓度映射到替代颜色空间
// 入参: tint 分色浓度, intent 渲染意图
// 返回: Paint 备用空间颜色, error 错误信息
func (s *separationSpace) paint(tint float64, intent Name) (Paint, error) {
	values, err := s.values(tint)
	if err != nil {
		return Paint{}, err
	}
	if s.name == "None" {
		return Paint{SourceSpace: "Separation", None: true}, nil
	}
	if s.name == "All" {
		return Paint{SourceSpace: "Separation", CMYK: &values}, nil
	}
	paint := Paint{SourceSpace: "Separation", Space: s.space, Values: values}
	switch s.alternate {
	case "DeviceGray":
		paint.RGB = [3]float64{values[0], values[0], values[0]}
	case "DeviceRGB":
		copy(paint.RGB[:], values[:])
	case "DeviceCMYK":
		paint.CMYK = &[4]float64{values[0], values[1], values[2], values[3]}
	case "Lab":
		copy(paint.RGB[:], values[:])
	case "ICCBased":
		paint.RGB, err = s.icc.color(values[:s.icc.components()], intent)
		if err != nil {
			return Paint{}, err
		}
	case "CalRGB", "CalGray":
		if s.gray {
			values[1], values[2] = values[0], values[0]
		}
		color := s.calRGB.color(values[0], values[1], values[2])
		paint.RGB = [3]float64{float64(color.R) / 65535, float64(color.G) / 65535, float64(color.B) / 65535}
	}
	return paint, nil
}

// values 将专色浓度映射到备用空间，Lab沿用显示RGB映射
// 入参: tint 专色浓度
// 返回: [4]float64 备用空间分量, error 着色函数错误
func (s *separationSpace) values(tint float64) ([4]float64, error) {
	if math.IsNaN(tint) || math.IsInf(tint, 0) {
		return [4]float64{}, fmt.Errorf("invalid Separation tint")
	}
	tint = math.Max(0, math.Min(1, tint))
	if s.name == "All" {
		return [4]float64{tint, tint, tint, tint}, nil
	}
	if s.name == "None" {
		return [4]float64{1, 1, 1}, nil
	}
	values, err := s.transform.evaluate(tint)
	if err != nil {
		return [4]float64{}, err
	}
	if s.lab != nil {
		pixel := s.lab.color(values[0], values[1], values[2])
		return [4]float64{float64(pixel.R) / 65535, float64(pixel.G) / 65535, float64(pixel.B) / 65535}, nil
	}
	if s.icc != nil && s.icc.ranges != nil {
		return s.icc.normalize(values[:s.icc.components()]), nil
	}
	for i := range values {
		values[i] = math.Max(0, math.Min(1, values[i]))
	}
	return values, nil
}
