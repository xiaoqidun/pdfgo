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
	"reflect"
	"sync/atomic"
)

// ColorSpace 保存设备或校准颜色空间，不包含绘制或输出格式逻辑
// Model为DeviceGray、DeviceRGB或DeviceCMYK，校准色保留对应模型与颜色变换
type ColorSpace struct {
	Model   Name
	profile *iccColorSpace
	srgb    uint32
	mapped  bool
}

// Device 判断分量是否属于原始设备空间，不将校准色的RGB显示结果视为设备源色
// 返回: bool 是否原始设备空间
func (s *ColorSpace) Device() bool {
	return s != nil && s.profile == nil && !s.mapped && s.Components() != 0
}

// Calibrated 判断混合空间是否含校准参数
// 返回: bool 是否校准
func (s *ColorSpace) Calibrated() bool { return s.profile != nil }

// Equal 判断颜色模型、配置定义与分量范围是否相同，不受逆向缓存初始化影响
// 入参: other 待比较空间
// 返回: bool 是否相同
func (s *ColorSpace) Equal(other *ColorSpace) bool {
	if s == nil || other == nil || s.Model != other.Model {
		return false
	}
	if s.profile == other.profile {
		return true
	}
	if s.profile != nil && other.profile != nil && s.profile.signature != nil && other.profile.signature != nil {
		return *s.profile.signature == *other.profile.signature && reflect.DeepEqual(s.profile.ranges, other.profile.ranges)
	}
	return reflect.DeepEqual(s.profile, other.profile)
}

// SRGBEquivalent 检查混合空间与sRGB的偏差是否在8位量化精度内
// 返回: bool 是否等价
func (s *ColorSpace) SRGBEquivalent() bool {
	if s.Model != "DeviceRGB" {
		return false
	}
	if s.profile == nil {
		return true
	}
	if cached := atomic.LoadUint32(&s.srgb); cached != 0 {
		return cached == 2
	}
	equivalent := s.srgbEquivalent()
	cached := uint32(1)
	if equivalent {
		cached = 2
	}
	atomic.StoreUint32(&s.srgb, cached)
	return equivalent
}

// srgbEquivalent 核对不可变校准变换的轴向与三维采样，不改变颜色分量
// 返回: bool 是否在8位量化精度内等价
func (s *ColorSpace) srgbEquivalent() bool {
	if s.profile.rgb == nil {
		return false
	}
	check := func(input [3]float64) bool {
		output, err := s.RGB(input[:], "RelativeColorimetric")
		if err != nil {
			return false
		}
		for j := range output {
			if math.Abs(output[j]-input[j]) > 1.0/255 {
				return false
			}
		}
		return true
	}
	for channel := 0; channel < 3; channel++ {
		for i := 0; i <= 256; i++ {
			input := [3]float64{}
			input[channel] = float64(i) / 256
			if !check(input) {
				return false
			}
		}
	}
	for red := 0; red <= 4; red++ {
		for green := 0; green <= 4; green++ {
			for blue := 0; blue <= 4; blue++ {
				if !check([3]float64{float64(red) / 4, float64(green) / 4, float64(blue) / 4}) {
					return false
				}
			}
		}
	}
	return true
}

// Components 返回混合空间的分量数
// 返回: int 分量数
func (s *ColorSpace) Components() int {
	switch s.Model {
	case "DeviceGray":
		return 1
	case "DeviceCMYK":
		return 4
	case "DeviceRGB":
		return 3
	}
	return 0
}

// RGB 将混合空间分量转换为sRGB显示颜色
// 入参: values 混合空间分量, intent 渲染意图
// 返回: [3]float64 sRGB颜色, error 分量或变换错误
func (s *ColorSpace) RGB(values []float64, intent Name) ([3]float64, error) {
	if err := s.validate(values); err != nil {
		return [3]float64{}, err
	}
	if s.profile != nil {
		return s.profile.color(values, normalizeRenderingIntent(intent))
	}
	switch s.Model {
	case "DeviceGray":
		return [3]float64{values[0], values[0], values[0]}, nil
	case "DeviceRGB":
		return [3]float64(values), nil
	case "DeviceCMYK":
		return deviceCMYKRGB(values), nil
	}
	return [3]float64{}, &UnsupportedError{Feature: "blending color space " + string(s.Model)}
}

// Convert 将源空间分量转换到当前颜色空间，相同校准空间保留原值
// DeviceRGB转DeviceCMYK采用恒等黑版生成及底色去除函数
// 入参: values 源分量, source 源空间, intent 渲染意图
// 返回: [4]float64 目标分量, error 无效分量或未支持的目标变换
func (s *ColorSpace) Convert(values []float64, source *ColorSpace, intent Name) ([4]float64, error) {
	return s.ConvertWith(values, source, intent, ColorConversion{})
}

// ConvertWith 在设备RGB转设备四色时应用指定函数，不改变灰度、校准色及同空间转换
// 入参: values 源分量, source 源空间, intent 渲染意图, conversion 设备转换函数
// 返回: [4]float64 目标分量, error 无效分量或变换错误
func (s *ColorSpace) ConvertWith(values []float64, source *ColorSpace, intent Name, conversion ColorConversion) ([4]float64, error) {
	var result [4]float64
	if err := source.validate(values); err != nil {
		return result, err
	}
	if s.Equal(source) {
		copy(result[:], values)
		return result, nil
	}
	intent = normalizeRenderingIntent(intent)
	if s.Calibrated() {
		xyz, err := source.xyz(values, intent)
		if err != nil {
			return result, err
		}
		return s.profile.fromXYZ(xyz, intent)
	}
	rgb, err := source.RGB(values, intent)
	if err != nil {
		return result, err
	}
	switch s.Model {
	case "DeviceGray":
		result[0] = .3*rgb[0] + .59*rgb[1] + .11*rgb[2]
	case "DeviceRGB":
		copy(result[:], rgb[:])
	case "DeviceCMYK":
		if source.Model == "DeviceRGB" && source.Device() {
			return conversion.RGBToCMYK(rgb)
		}
		black := 1 - math.Max(rgb[0], math.Max(rgb[1], rgb[2]))
		for c := range rgb {
			result[c] = math.Max(0, 1-rgb[c]-black)
		}
		result[3] = black
	default:
		return result, &UnsupportedError{Feature: "destination color space " + string(s.Model)}
	}
	return result, nil
}

// Luminosity 按PDF蒙版规则计算亮度，ICC使用连接空间的Y分量
// 入参: values 已合成的颜色分量, intent 渲染意图
// 返回: float64 单位亮度, error 分量或变换错误
func (s *ColorSpace) Luminosity(values []float64, intent Name) (float64, error) {
	if err := s.validate(values); err != nil {
		return 0, err
	}
	if p := s.profile; p != nil {
		xyz, err := s.xyz(values, normalizeRenderingIntent(intent))
		return math.Max(0, math.Min(1, xyz[1])), err
	}
	if s.Model == "DeviceCMYK" {
		return (.3*(1-values[0]) + .59*(1-values[1]) + .11*(1-values[2])) * (1 - values[3]), nil
	}
	rgb, err := s.RGB(values, intent)
	return .3*rgb[0] + .59*rgb[1] + .11*rgb[2], err
}

// deviceCMYKRGB 按PDF设备颜色转换规则叠加黑色分量后取补色
// 入参: values CMYK单位分量
// 返回: [3]float64 RGB单位分量
func deviceCMYKRGB(values []float64) [3]float64 {
	return [3]float64{1 - math.Min(1, values[0]+values[3]), 1 - math.Min(1, values[1]+values[3]), 1 - math.Min(1, values[2]+values[3])}
}

// validate 检查混合分量数与单位范围
// 入参: values 颜色分量
// 返回: error 无效颜色
func (s *ColorSpace) validate(values []float64) error {
	if s.Components() == 0 || len(values) != s.Components() {
		return fmt.Errorf("invalid blending color component count")
	}
	for _, v := range values {
		if math.IsNaN(v) || math.IsInf(v, 0) || v < 0 || v > 1 {
			return fmt.Errorf("invalid blending color component")
		}
	}
	return nil
}

// readBlendingSpace 解析设备或可双向变换的ICC混合空间
// 入参: value 颜色空间定义
// 返回: *ColorSpace 混合空间, error 格式或能力错误
func (r *Reader) readBlendingSpace(value Object) (*ColorSpace, error) {
	s, err := r.readColorSpace(value)
	if err != nil {
		return nil, err
	}
	if s.profile != nil {
		if err := s.profile.validateBlending(); err != nil {
			return nil, err
		}
	}
	return s, nil
}

// resolveColorSpace 解析颜色空间及数组首项的间接引用，不修改共享数组
// 入参: value 颜色空间对象
// 返回: Object 已解析定义, error 引用或类型错误
func (r *Reader) resolveColorSpace(value Object) (Object, error) {
	value, err := r.Resolve(value)
	if err != nil {
		return nil, err
	}
	array, ok := value.(Array)
	if !ok || len(array) == 0 {
		return value, nil
	}
	if _, ok := array[0].(Name); ok {
		return value, nil
	}
	head, err := r.Resolve(array[0])
	if err != nil {
		return nil, err
	}
	if _, ok := head.(Name); !ok {
		return nil, fmt.Errorf("invalid color space family")
	}
	array = append(Array(nil), array...)
	array[0] = head
	return array, nil
}

// readColorSpace 解析设备、校准或ICC颜色空间，普通着色不要求逆变换
// 入参: value 颜色空间定义
// 返回: *ColorSpace 颜色空间, error 格式或能力错误
func (r *Reader) readColorSpace(value Object) (*ColorSpace, error) {
	value, err := r.resolveColorSpace(value)
	if err != nil {
		return nil, err
	}
	if name, ok := value.(Name); ok {
		if name == "DeviceGray" || name == "DeviceRGB" || name == "DeviceCMYK" {
			return &ColorSpace{Model: name}, nil
		}
	}
	if array, ok := value.(Array); ok && len(array) == 1 {
		if name, ok := array[0].(Name); ok && (name == "DeviceGray" || name == "DeviceRGB" || name == "DeviceCMYK") {
			return &ColorSpace{Model: name}, nil
		}
	}
	if array, ok := value.(Array); ok && len(array) == 2 && (array[0] == Name("CalRGB") || array[0] == Name("CalGray")) {
		return r.readCalibratedSpace(array)
	}
	if array, ok := value.(Array); ok && len(array) == 2 && array[0] == Name("ICCBased") {
		profile, err := r.readICCColorSpace(array)
		if err != nil {
			return nil, err
		}
		model := map[int]Name{1: "DeviceGray", 3: "DeviceRGB", 4: "DeviceCMYK"}[profile.components()]
		return &ColorSpace{Model: model, profile: profile}, nil
	}
	return nil, &UnsupportedError{Feature: "color space"}
}

// readCalibratedSpace 将校准色转换为D50矩阵曲线，复用双向混合运算
// 入参: array 校准颜色空间
// 返回: *ColorSpace 颜色变换, error 参数错误
func (r *Reader) readCalibratedSpace(array Array) (*ColorSpace, error) {
	var calibrated *calRGBSpace
	var err error
	gray := array[0] == Name("CalGray")
	if gray {
		calibrated, err = r.readCalGray(array)
	} else {
		calibrated, err = r.readCalRGB(array)
	}
	if err != nil {
		return nil, err
	}
	d65 := bradford([3]float64{.95047, 1, 1.08883})
	d50 := bradford([3]float64{.9642, 1, .8249})
	matrix := calRGBSpace{gamma: [3]float64{1, 1, 1}}
	for i := range matrix.adapt {
		matrix.adapt[i] = d65[i] / d50[i]
	}
	for c := 0; c < 3; c++ {
		cone := bradford([3]float64(calibrated.matrix[c*3 : c*3+3]))
		for i := range cone {
			cone[i] *= calibrated.adapt[i] * d50[i] / d65[i]
		}
		matrix.matrix[c*3] = .9869929*cone[0] - .1470543*cone[1] + .1599627*cone[2]
		matrix.matrix[c*3+1] = .4323053*cone[0] + .5183603*cone[1] + .0492912*cone[2]
		matrix.matrix[c*3+2] = -.0085287*cone[0] + .0400428*cone[1] + .9684867*cone[2]
	}
	cone := bradford(calibrated.offset)
	for i := range cone {
		cone[i] *= calibrated.adapt[i] * d50[i] / d65[i]
	}
	matrix.offset = [3]float64{.9869929*cone[0] - .1470543*cone[1] + .1599627*cone[2], .4323053*cone[0] + .5183603*cone[1] + .0492912*cone[2], -.0085287*cone[0] + .0400428*cone[1] + .9684867*cone[2]}
	profile := &iccColorSpace{}
	model := Name("DeviceRGB")
	if gray {
		profile.gray = &iccGraySpace{matrix: matrix, curve: iccToneCurve{parameters: []float64{calibrated.gamma[0]}}}
		model = "DeviceGray"
	} else {
		profile.rgb = &iccRGBSpace{matrix: matrix, inverse: iccInverseMatrix(matrix.matrix)}
		for i, gamma := range calibrated.gamma {
			profile.rgb.curves[i] = iccToneCurve{parameters: []float64{gamma}}
		}
	}
	return &ColorSpace{Model: model, profile: profile}, nil
}
