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
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
)

// iccRGBSpace 保存RGB矩阵曲线配置文件到sRGB的变换
type iccRGBSpace struct {
	matrix  calRGBSpace
	curves  [3]iccToneCurve
	inverse [9]float64
}

// iccGraySpace 保存灰度曲线及XYZ或Lab连接空间到sRGB的变换
type iccGraySpace struct {
	matrix calRGBSpace
	curve  iccToneCurve
	lab    bool
}

// iccToneCurve 保存ICC采样曲线或参数曲线
type iccToneCurve struct {
	samples    []uint16
	parameters []float64
	function   uint16
	direction  int8
}

// iccColorSpace 统一灰度、RGB、CMYK与Lab配置文件的绘制颜色变换
type iccColorSpace struct {
	rgb                *iccRGBSpace
	gray               *iccGraySpace
	lut                *iccLUTSpace
	toFloat, fromFloat [4]*iccProcessElements
	processPCS         string
	absoluteScale      [3]float64
	ranges             *[8]float64
	lab                bool
	inverse            *iccInverseSpace
	signature          *[32]byte
	alternate          *patternColorSpace
	alternateBlending  *ColorSpace
	alternateCount     int
}

// iccProfileStream 解析ICC配置流及必需的分量数，支持间接引用
// 入参: object ICCBased颜色空间数组
// 返回: *Stream 配置流, Integer 分量数, error 解析或类型错误
func (r *Reader) iccProfileStream(object Array) (*Stream, Integer, error) {
	if len(object) != 2 || object[0] != Name("ICCBased") {
		return nil, 0, fmt.Errorf("invalid ICCBased color space")
	}
	value, err := r.Resolve(object[1])
	if err != nil {
		return nil, 0, err
	}
	stream, ok := value.(*Stream)
	if !ok || stream == nil {
		return nil, 0, fmt.Errorf("invalid ICC profile stream")
	}
	value, err = r.Resolve(stream.Dictionary["N"])
	if err != nil {
		return nil, 0, err
	}
	count, ok := value.(Integer)
	if !ok || count != 1 && count != 3 && count != 4 {
		return nil, 0, fmt.Errorf("invalid ICC component count")
	}
	return stream, count, nil
}

// readICCColorSpace 读取内容流使用的ICC颜色空间
// 入参: object ICCBased颜色空间数组
// 返回: *iccColorSpace 颜色变换, error 解析错误
func (r *Reader) readICCColorSpace(object Array) (*iccColorSpace, error) {
	return r.readICCSourceSpace(object, false)
}

// readICCSourceSpace 读取原始或已按资源校验的ICC源空间，后来版本使用标准备用空间
// 入参: object ICCBased数组, effective 是否已按资源校验并替换
// 返回: *iccColorSpace 源颜色变换, error 配置或备用定义错误
func (r *Reader) readICCSourceSpace(object Array, effective bool) (*iccColorSpace, error) {
	if r.colorProfileDepth >= 64 {
		return nil, fmt.Errorf("color space recursion limit exceeded")
	}
	r.colorProfileDepth++
	defer func() { r.colorProfileDepth-- }()
	stream, count, err := r.iccProfileStream(object)
	if err != nil {
		return nil, err
	}
	ranges, err := r.iccRanges(stream.Dictionary["Range"], int(count))
	if err != nil {
		return nil, err
	}
	data, err := stream.Decode()
	if err != nil {
		return nil, err
	}
	if err := validateICCSourceHeader(data, int(count)); err != nil {
		return nil, err
	}
	if data[8] > 4 {
		if _, err := iccProfileDirectory(data, string(data[16:20])); err != nil {
			return nil, err
		}
		return r.readICCAlternate(stream, int(count), ranges, effective)
	}
	key := sha256.Sum256(data)
	if cached := r.colorProfiles[key]; cached != nil {
		if cached.components() != int(count) {
			return nil, fmt.Errorf("invalid ICC component count")
		}
		return cached.withRanges(ranges), nil
	}
	model := map[Integer]string{1: "GRAY", 3: "RGB ", 4: "CMYK"}[count]
	if count == 3 && len(data) >= 20 && string(data[16:20]) == "Lab " {
		model = "Lab "
	}
	tags, err := iccProfileTags(data, model)
	if err != nil {
		return nil, err
	}
	signature := key
	space := &iccColorSpace{absoluteScale: iccAbsoluteScale(tags["wtpt"]), lab: model == "Lab ", signature: &signature}
	if tags["A2B0"] != nil {
		space.lut, err = parseICCLUTSpace(data, tags)
	} else if count == 1 {
		space.gray, err = parseICCGray(data)
	} else if count == 3 && !space.lab {
		space.rgb, err = parseICCRGB(data)
	} else {
		err = fmt.Errorf("missing ICC A2B0 transform")
	}
	if err != nil {
		return nil, err
	}
	space.processPCS = string(data[20:24])
	for n := range space.toFloat {
		name := fmt.Sprintf("D2B%d", n)
		if tag := tags[name]; tag != nil {
			transform, err := parseICCProcessElements(tag)
			var unsupported *UnsupportedError
			if errors.As(err, &unsupported) {
				continue
			}
			if err != nil {
				return nil, fmt.Errorf("ICC %s: %w", name, err)
			}
			if transform.input != int(count) || transform.output != 3 {
				return nil, fmt.Errorf("invalid ICC %s channels", name)
			}
			space.toFloat[n] = transform
		}
	}
	space.inverse = newICCInverse(tags, int(count), space.processPCS)
	if r.colorProfiles == nil {
		r.colorProfiles = make(map[[32]byte]*iccColorSpace)
	}
	r.colorProfiles[key] = space
	return space.withRanges(ranges), nil
}

// iccRanges 读取PDF声明的源分量范围，缺省或单位范围不分配配置副本
// 入参: object 范围数组, count 源分量数
// 返回: *[8]float64 分量范围，单位范围为nil, error 无效范围
func (r *Reader) iccRanges(object Object, count int) (*[8]float64, error) {
	object, err := r.Resolve(object)
	if err != nil || object == nil {
		return nil, err
	}
	values, err := r.numberArray(object, count*2)
	if err != nil {
		return nil, err
	}
	var ranges [8]float64
	unit := true
	for i := range count {
		low, high := values[2*i], values[2*i+1]
		if math.IsNaN(low) || math.IsNaN(high) || math.IsInf(low, 0) || math.IsInf(high, 0) || low > high {
			return nil, fmt.Errorf("invalid ICC component range")
		}
		ranges[2*i], ranges[2*i+1] = low, high
		unit = unit && low == 0 && high == 1
	}
	if unit {
		return nil, nil
	}
	return &ranges, nil
}

// withRanges 复用不可变ICC变换，为当前PDF定义保留独立分量范围
// 入参: ranges 源分量范围
// 返回: *iccColorSpace 当前定义的颜色空间
func (s *iccColorSpace) withRanges(ranges *[8]float64) *iccColorSpace {
	if ranges == nil {
		return s
	}
	copy := *s
	copy.ranges = ranges
	return &copy
}

// sourceRanges 返回PDF源分量范围，不使用PCS编码范围替代
// 返回: [8]float64 每个源分量的最小与最大值
func (s *iccColorSpace) sourceRanges() [8]float64 {
	if s.ranges != nil {
		return *s.ranges
	}
	return [8]float64{0, 1, 0, 1, 0, 1, 0, 1}
}

// normalize 将实际源分量裁切到PDF范围并映射为合成使用的单位分量
// 入参: values 实际源分量
// 返回: [4]float64 单位分量
func (s *iccColorSpace) normalize(values []float64) [4]float64 {
	var result [4]float64
	ranges := s.sourceRanges()
	for i, value := range values {
		low, high := ranges[2*i], ranges[2*i+1]
		if low != high {
			result[i] = functionPosition(math.Max(low, math.Min(high, value)), low, high)
		}
	}
	return result
}

// deviceValues 将合成分量恢复到ICC设备单位，Lab浮点标签使用真实Lab数值
// 入参: values 单位分量
// 返回: [4]float64 ICC设备分量
func (s *iccColorSpace) deviceValues(values []float64) [4]float64 {
	var result [4]float64
	if s.ranges == nil {
		copy(result[:], values)
		return result
	}
	for i, value := range values {
		result[i] = functionValue(value, s.ranges[2*i], s.ranges[2*i+1])
	}
	return result
}

// components 返回配置文件的颜色分量数
// 返回: int 分量数
func (s *iccColorSpace) components() int {
	if s.alternate != nil {
		return s.alternateCount
	}
	if s.lut != nil {
		return s.lut.components
	}
	if s.gray != nil {
		return 1
	}
	return 3
}

// paint 保存ICC合成分量与显示颜色，避免混合时从已截断的sRGB反推颜色
// 入参: values PDF源分量, intent 渲染意图
// 返回: Paint 源空间与显示颜色, error 颜色变换错误
func (s *iccColorSpace) paint(values []float64, intent Name) (Paint, error) {
	if s.alternate != nil {
		if len(values) != s.components() {
			return Paint{}, fmt.Errorf("invalid ICC alternate input count")
		}
		var input [4]float64
		copy(input[:], values)
		return s.alternatePaint(input, intent)
	}
	model := map[int]Name{1: "DeviceGray", 3: "DeviceRGB", 4: "DeviceCMYK"}[s.components()]
	paint := Paint{SourceSpace: "ICCBased", Space: &ColorSpace{Model: model, profile: s}}
	paint.Values = s.normalize(values)
	var err error
	paint.RGB, err = s.color(paint.Values[:s.components()], intent)
	return paint, err
}

// color 将ICC合成分量转换为sRGB
// 入参: values 单位分量, intent 渲染意图
// 返回: [3]float64 sRGB分量, error 不支持的变换
func (s *iccColorSpace) color(values []float64, intent Name) ([3]float64, error) {
	if s.alternate != nil {
		var input [4]float64
		copy(input[:], values)
		return s.alternateColor(input, intent)
	}
	xyz, err := s.xyz(values, intent)
	return iccXYZRGB(xyz), err
}

// validateRGBGroupSpace 检查混合空间与sRGB的偏差是否在8位量化精度内
// 入参: object 组颜色空间
// 返回: error 不支持的混合空间或解析错误
func (r *Reader) validateRGBGroupSpace(object Object) error {
	s, err := r.readBlendingSpace(object)
	if err != nil {
		return err
	}
	if !s.SRGBEquivalent() {
		return &UnsupportedError{Feature: "non-sRGB group color space"}
	}
	return nil
}

// parseICCRGB 校验ICC标签边界并读取D50矩阵和色调曲线
// 入参: data 配置文件数据
// 返回: *iccRGBSpace 颜色变换, error 错误信息
func parseICCRGB(data []byte) (*iccRGBSpace, error) {
	tags, err := iccTags(data, "RGB ")
	if err != nil {
		return nil, err
	}
	s := &iccRGBSpace{matrix: calRGBSpace{gamma: [3]float64{1, 1, 1}}}
	source, target := bradford([3]float64{0.9642, 1, 0.8249}), bradford([3]float64{0.95047, 1, 1.08883})
	for i, channel := range []string{"r", "g", "b"} {
		s.matrix.adapt[i] = target[i] / source[i]
		xyz := tags[channel+"XYZ"]
		if len(xyz) != 20 || string(xyz[:4]) != "XYZ " {
			return nil, &UnsupportedError{Feature: "ICC colorant matrix"}
		}
		for j := 0; j < 3; j++ {
			s.matrix.matrix[i*3+j] = float64(int32(binary.BigEndian.Uint32(xyz[8+4*j:]))) / 65536
		}
		s.curves[i], err = iccCurve(tags[channel+"TRC"])
		if err != nil {
			return nil, err
		}
	}
	s.inverse = iccInverseMatrix(s.matrix.matrix)
	return s, nil
}

// parseICCGray 读取灰度曲线，使用ICC定义的D50连接空间
// 入参: data 配置文件数据
// 返回: *iccGraySpace 灰度颜色变换, error 错误信息
func parseICCGray(data []byte) (*iccGraySpace, error) {
	tags, err := iccTags(data, "GRAY")
	if err != nil {
		return nil, err
	}
	white := tags["wtpt"]
	if len(white) != 20 || string(white[:4]) != "XYZ " {
		return nil, &UnsupportedError{Feature: "ICC gray white point"}
	}
	curve, err := iccCurve(tags["kTRC"])
	if err != nil {
		return nil, err
	}
	s := &iccGraySpace{curve: curve, lab: string(data[20:24]) == "Lab ", matrix: calRGBSpace{gamma: [3]float64{1, 1, 1}, matrix: [9]float64{0.9642, 0, 0, 0, 1, 0, 0, 0, 0.8249}}}
	source, target := bradford([3]float64{0.9642, 1, 0.8249}), bradford([3]float64{0.95047, 1, 1.08883})
	for i := range s.matrix.adapt {
		s.matrix.adapt[i] = target[i] / source[i]
	}
	return s, nil
}

// iccAbsoluteScale 预计算媒体白点相对于D50的比率，缺失或无效时不提供传统绝对色度变换
// 入参: data 媒体白点标签
// 返回: [3]float64 换算比率，无效时为零
func iccAbsoluteScale(data []byte) [3]float64 {
	var scale [3]float64
	if len(data) != 20 || string(data[:4]) != "XYZ " {
		return scale
	}
	for i, illuminant := range [...]float64{.9642, 1, .8249} {
		white := float64(int32(binary.BigEndian.Uint32(data[8+4*i:]))) / 65536
		if white <= 0 {
			return [3]float64{}
		}
		scale[i] = white / illuminant
	}
	return scale
}

// iccTags 校验ICC标签边界并读取指定模型的配置文件标签
// 入参: data 配置文件数据, model 颜色模型
// 返回: map[string][]byte 配置文件标签, error 错误信息
func iccTags(data []byte, model string) (map[string][]byte, error) {
	tags, err := iccProfileTags(data, model)
	if err != nil {
		return nil, err
	}
	if string(data[20:24]) != "XYZ " && (model != "GRAY" || string(data[20:24]) != "Lab ") {
		return nil, &UnsupportedError{Feature: "ICC profile connection space"}
	}
	for _, name := range []string{"A2B0", "A2B1", "A2B2"} {
		if tags[name] != nil {
			return nil, &UnsupportedError{Feature: "ICC lookup table transform"}
		}
	}
	return tags, nil
}

// iccProfileTags 校验ICC头和标签边界，不预设颜色变换类型
// 入参: data 配置文件数据, model 颜色模型
// 返回: map[string][]byte 标签数据, error 格式错误
func iccProfileTags(data []byte, model string) (map[string][]byte, error) {
	tags, err := iccProfileDirectory(data, model)
	if err != nil {
		return nil, err
	}
	if data[8] != 2 && data[8] != 4 {
		return nil, &UnsupportedError{Feature: "ICC profile version"}
	}
	return tags, nil
}

// iccProfileDirectory 校验ICC头和标签目录，不将未知版本与格式损坏混为一类
// 入参: data 配置数据, model 预期源模型
// 返回: map[string][]byte 标签数据, error 头或目录错误
func iccProfileDirectory(data []byte, model string) (map[string][]byte, error) {
	if len(data) < 132 || string(data[36:40]) != "acsp" || uint64(binary.BigEndian.Uint32(data)) != uint64(len(data)) {
		return nil, fmt.Errorf("invalid ICC profile header")
	}
	if string(data[16:20]) != model || string(data[20:24]) != "XYZ " && string(data[20:24]) != "Lab " {
		return nil, fmt.Errorf("invalid ICC profile model")
	}
	count := uint64(binary.BigEndian.Uint32(data[128:]))
	if count > uint64(len(data)-132)/12 {
		return nil, fmt.Errorf("invalid ICC tag table")
	}
	tags := make(map[string][]byte, int(count))
	for i := 0; i < int(count); i++ {
		entry := data[132+12*i:]
		offset, size := uint64(binary.BigEndian.Uint32(entry[4:])), uint64(binary.BigEndian.Uint32(entry[8:]))
		if offset < 132+12*count || offset > uint64(len(data)) || size > uint64(len(data))-offset {
			return nil, fmt.Errorf("invalid ICC tag bounds")
		}
		name := string(entry[:4])
		if tags[name] != nil {
			return nil, fmt.Errorf("duplicate ICC tag %q", name)
		}
		tags[name] = data[offset : offset+size]
	}
	return tags, nil
}

// iccCurve 读取ICC采样曲线或参数曲线并校验定义域
// 入参: data 色调曲线标签数据
// 返回: iccToneCurve 色调曲线, error 错误信息
func iccCurve(data []byte) (iccToneCurve, error) {
	if len(data) < 12 {
		return iccToneCurve{}, fmt.Errorf("invalid ICC tone curve length")
	}
	if string(data[:4]) == "para" {
		curve := iccToneCurve{function: binary.BigEndian.Uint16(data[8:])}
		counts := [...]int{1, 3, 4, 5, 7}
		if int(curve.function) >= len(counts) {
			return iccToneCurve{}, &UnsupportedError{Feature: "ICC parametric curve function"}
		}
		n := counts[curve.function]
		if len(data) < 12+4*n {
			return iccToneCurve{}, fmt.Errorf("invalid ICC parametric curve length")
		}
		curve.parameters = make([]float64, n)
		for i := range curve.parameters {
			curve.parameters[i] = float64(int32(binary.BigEndian.Uint32(data[12+4*i:]))) / 65536
		}
		if curve.parameters[0] <= 0 || (curve.function == 1 || curve.function == 2) && curve.parameters[1] <= 0 {
			return iccToneCurve{}, fmt.Errorf("invalid ICC parametric curve parameters")
		}
		points := []float64{0, 1}
		if curve.function >= 3 {
			d := curve.parameters[4]
			if d >= 0 && d <= 1 {
				points = append(points, d)
			}
		}
		for _, x := range points {
			if y := curve.parametric(x); math.IsNaN(y) || math.IsInf(y, 0) {
				return iccToneCurve{}, fmt.Errorf("undefined ICC parametric curve")
			}
		}
		curve.direction = 1
		if curve.evaluate(0) == curve.evaluate(1) {
			curve.direction = 2
		}
		return curve, nil
	}
	if string(data[:4]) != "curv" {
		return iccToneCurve{}, &UnsupportedError{Feature: "ICC tone curve type"}
	}
	n := uint64(binary.BigEndian.Uint32(data[8:]))
	if n > uint64(len(data)-12)/2 {
		return iccToneCurve{}, fmt.Errorf("invalid ICC tone curve length")
	}
	curve := make([]uint16, int(n))
	for j := range curve {
		curve[j] = binary.BigEndian.Uint16(data[12+2*j:])
	}
	if n == 1 && curve[0] == 0 {
		return iccToneCurve{}, fmt.Errorf("invalid ICC curve gamma")
	}
	if n == 1 {
		return iccToneCurve{parameters: []float64{float64(curve[0]) / 256}}, nil
	}
	return iccToneCurve{samples: curve, direction: iccCurveDirection(curve)}, nil
}

// iccCurveDirection 检查采样曲线的单调方向，常量和非单调曲线不可反求
// 入参: samples 曲线采样值
// 返回: int8 递增为1、递减为-1、不可逆为2
func iccCurveDirection(samples []uint16) int8 {
	var direction int8
	for n := 1; n < len(samples); n++ {
		step := int8(0)
		if samples[n] > samples[n-1] {
			step = 1
		} else if samples[n] < samples[n-1] {
			step = -1
		}
		if step != 0 {
			if direction != 0 && direction != step {
				return 2
			}
			direction = step
		}
	}
	if direction == 0 {
		return 2
	}
	return direction
}

// parametric 按ICC函数类型求出未截断的曲线值
// 入参: x 归一化输入
// 返回: float64 曲线值
func (c iccToneCurve) parametric(x float64) float64 {
	p := c.parameters
	if c.function == 0 {
		return math.Pow(x, p[0])
	}
	if c.function <= 2 {
		y := 0.0
		if x >= -p[2]/p[1] {
			y = math.Pow(p[1]*x+p[2], p[0])
		}
		if c.function == 2 {
			y += p[3]
		}
		return y
	}
	if x < p[4] {
		y := p[3] * x
		if c.function == 4 {
			y += p[6]
		}
		return y
	}
	y := math.Pow(p[1]*x+p[2], p[0])
	if c.function == 4 {
		y += p[5]
	}
	return y
}

// evaluate 计算ICC曲线并将输入输出限制在标准单位区间
// 入参: value 编码分量
// 返回: float64 线性分量
func (c iccToneCurve) evaluate(value float64) float64 {
	v := math.Max(0, math.Min(1, value))
	if len(c.parameters) > 0 {
		return math.Max(0, math.Min(1, c.parametric(v)))
	}
	if len(c.samples) > 1 {
		position := v * float64(len(c.samples)-1)
		index := min(int(position), len(c.samples)-2)
		return (float64(c.samples[index]) + (position-float64(index))*(float64(c.samples[index+1])-float64(c.samples[index]))) / 65535
	}
	return v
}

// color 将ICC编码分量变换到sRGB，矩阵配置文件使用同一色度变换
// 入参: values RGB编码分量, intent 渲染意图
// 返回: [3]float64 sRGB分量, error 错误信息
func (s *iccRGBSpace) color(values []float64, intent Name) ([3]float64, error) {
	if intent == "AbsoluteColorimetric" {
		return [3]float64{}, &UnsupportedError{Feature: "ICC absolute colorimetric conversion"}
	}
	linear := [3]float64{}
	for i, curve := range s.curves {
		linear[i] = curve.evaluate(values[i])
	}
	c := s.matrix.color(linear[0], linear[1], linear[2])
	return [3]float64{float64(c.R) / 65535, float64(c.G) / 65535, float64(c.B) / 65535}, nil
}
