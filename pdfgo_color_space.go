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
)

// ColorSpace 保存设备或ICC颜色空间，不包含绘制或输出格式逻辑
// Model为DeviceGray、DeviceRGB或DeviceCMYK，ICCBased保留对应模型与配置文件变换
type ColorSpace struct {
	Model   Name
	profile *iccColorSpace
}

// Calibrated 判断混合空间是否含ICC校准参数
// 返回: bool 是否校准
func (s *ColorSpace) Calibrated() bool { return s.profile != nil }

// Equal 判断颜色模型和校准变换是否相同
// 入参: other 待比较空间
// 返回: bool 是否相同
func (s *ColorSpace) Equal(other *ColorSpace) bool {
	return s != nil && other != nil && s.Model == other.Model && reflect.DeepEqual(s.profile, other.profile)
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
		return s.profile.color(values, intent)
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

// Convert 将源空间分量转换到当前设备空间，相同校准空间保留原值
// DeviceRGB转DeviceCMYK采用恒等黑版生成及底色去除函数
// 入参: values 源分量, source 源空间, intent 渲染意图
// 返回: [4]float64 目标分量, error 无效分量或未支持的目标变换
func (s *ColorSpace) Convert(values []float64, source *ColorSpace, intent Name) ([4]float64, error) {
	var result [4]float64
	if err := source.validate(values); err != nil {
		return result, err
	}
	if s.Equal(source) {
		copy(result[:], values)
		return result, nil
	}
	if s.Calibrated() {
		return result, &UnsupportedError{Feature: "ICC destination color transform"}
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

// deviceCMYKRGB 按PDF设备颜色转换规则叠加黑色分量后取补色
// 入参: values CMYK单位分量
// 返回: [3]float64 RGB单位分量
func deviceCMYKRGB(values []float64) [3]float64 {
	return [3]float64{1 - math.Min(1, values[0]+values[3]), 1 - math.Min(1, values[1]+values[3]), 1 - math.Min(1, values[2]+values[3])}
}

// Luminosity 按PDF蒙版规则计算亮度，ICC使用连接空间的Y分量
// 入参: values 已合成的颜色分量, intent 渲染意图
// 返回: float64 单位亮度, error 分量或变换错误
func (s *ColorSpace) Luminosity(values []float64, intent Name) (float64, error) {
	if err := s.validate(values); err != nil {
		return 0, err
	}
	if p := s.profile; p != nil {
		if p.lut != nil {
			xyz, err := p.lut.xyz(values, intent)
			return math.Max(0, math.Min(1, xyz[1])), err
		}
		if p.gray != nil {
			return p.gray.curve.evaluate(values[0]), nil
		}
		y := 0.0
		for i, curve := range p.rgb.curves {
			y += p.rgb.matrix.matrix[3*i+1] * curve.evaluate(values[i])
		}
		return math.Max(0, math.Min(1, y)), nil
	}
	if s.Model == "DeviceCMYK" {
		return (.3*(1-values[0]) + .59*(1-values[1]) + .11*(1-values[2])) * (1 - values[3]), nil
	}
	rgb, err := s.RGB(values, intent)
	return .3*rgb[0] + .59*rgb[1] + .11*rgb[2], err
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
	if s.profile != nil && s.profile.lut != nil && s.profile.lut.fromPCS[0] == nil {
		return nil, fmt.Errorf("missing ICC B2A0 blending transform")
	}
	return s, nil
}

// readColorSpace 解析设备或ICC颜色空间，普通着色不要求逆变换
// 入参: value 颜色空间定义
// 返回: *ColorSpace 颜色空间, error 格式或能力错误
func (r *Reader) readColorSpace(value Object) (*ColorSpace, error) {
	value, err := r.Resolve(value)
	if err != nil {
		return nil, err
	}
	if name, ok := value.(Name); ok {
		if name == "DeviceGray" || name == "DeviceRGB" || name == "DeviceCMYK" {
			return &ColorSpace{Model: name}, nil
		}
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
