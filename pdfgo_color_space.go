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
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"math"
	"slices"
	"sync/atomic"
)

// ColorSpace 保存设备或校准颜色空间，区分源分量、空间转换与sRGB显示
// Model为DeviceGray、DeviceRGB或DeviceCMYK，校准色保留对应模型与颜色变换
type ColorSpace struct {
	Model     Name
	profile   *iccColorSpace
	srgbState uint32
	blending  uint32
	mapped    bool
}

// NewDeviceColorSpace 创建标准设备颜色空间
// 入参: model 设备颜色模型，仅接受DeviceGray、DeviceRGB或DeviceCMYK
// 返回: *ColorSpace 设备颜色空间, error 不支持的模型错误
func NewDeviceColorSpace(model Name) (*ColorSpace, error) {
	switch model {
	case "DeviceGray", "DeviceRGB", "DeviceCMYK":
		return &ColorSpace{Model: model}, nil
	default:
		return nil, fmt.Errorf("invalid device color space %q", model)
	}
}

// NewCalRGBColorSpace 以校准参数建立独立源空间，不生成或声明ICC配置
// 入参: white XYZ白点且Y为1, black XYZ黑点, gamma 各通道解码指数, matrix 按列排列的RGB到XYZ矩阵
// 返回: *ColorSpace 校准空间, error 参数错误
func NewCalRGBColorSpace(white, black, gamma [3]float64, matrix [9]float64) (*ColorSpace, error) {
	dict := Dictionary{}
	for _, field := range []struct {
		key    Name
		values []float64
	}{{"WhitePoint", white[:]}, {"BlackPoint", black[:]}, {"Gamma", gamma[:]}, {"Matrix", matrix[:]}} {
		values := make(Array, len(field.values))
		for n, value := range field.values {
			values[n] = Real(value)
		}
		dict[field.key] = values
	}
	return (&Reader{}).readCalibratedSpace(Array{Name("CalRGB"), dict})
}

// deviceCMYKRGB 按内置印刷配置文件将未标定CMYK分量转换为sRGB
// 入参: values CMYK单位分量
// 返回: [3]float64 RGB单位分量
func deviceCMYKRGB(values []float64) [3]float64 {
	return DeviceCMYKToSRGB(values[0], values[1], values[2], values[3])
}

// Device 判断分量是否属于原始设备空间，不将校准色的RGB显示结果视为设备源色
// 返回: bool 是否原始设备空间
func (s *ColorSpace) Device() bool {
	return s != nil && s.profile == nil && !s.mapped && s.Components() != 0
}

// Calibrated 判断颜色空间是否含校准参数
// 返回: bool 是否校准
func (s *ColorSpace) Calibrated() bool { return s != nil && s.profile != nil }

// DefaultIntent 返回ICC头建议的渲染意图，不替代PDF内容流指定的意图
// 返回: Name 配置默认意图，设备及其他校准空间为相对色度
func (s *ColorSpace) DefaultIntent() Name {
	if s != nil && s.profile != nil {
		return normalizeRenderingIntent(s.profile.intent)
	}
	return "RelativeColorimetric"
}

// Equal 判断颜色模型、配置定义与分量范围是否相同，仅比较不可变定义字段且不访问懒加载状态
// 入参: other 待比较空间
// 返回: bool 是否相同
func (s *ColorSpace) Equal(other *ColorSpace) bool {
	if s == nil || other == nil || s.Model != other.Model || s.mapped != other.mapped {
		return false
	}
	return equalICCProfile(s.profile, other.profile)
}

// equalICCProfile 按不可变定义字段比较两个ICC或校准颜色配置，不访问逆向懒加载与函数字段
// 入参: a 首个配置, b 对比配置
// 返回: bool 是否相同
func equalICCProfile(a, b *iccColorSpace) bool {
	if a == b {
		return true
	}
	if a == nil || b == nil {
		return false
	}
	if a.lab != b.lab || a.intent != b.intent || a.absoluteScale != b.absoluteScale || !equalICCRanges(a.ranges, b.ranges) {
		return false
	}
	if (a.signature != nil) != (b.signature != nil) {
		return false
	}
	if a.signature != nil {
		return *a.signature == *b.signature && bytes.Equal(a.data, b.data)
	}
	if (a.alternate != nil) != (b.alternate != nil) {
		return false
	}
	if a.alternate != nil {
		if a.alternateCount != b.alternateCount || a.alternate.components != b.alternate.components || a.alternate.invisible != b.alternate.invisible {
			return false
		}
		if a.alternateBlending != nil || b.alternateBlending != nil {
			return a.alternateBlending.Equal(b.alternateBlending)
		}
		return a.hasAlternateDigest && b.hasAlternateDigest && a.alternateDigest == b.alternateDigest
	}
	return equalICCGray(a.gray, b.gray) && equalICCRGB(a.rgb, b.rgb)
}

// equalICCRanges 比较两个可选分量范围指针的内容是否相等
// 入参: a 首个范围, b 对比范围
// 返回: bool 是否相等
func equalICCRanges(a, b *[8]float64) bool {
	if a == b {
		return true
	}
	if a == nil || b == nil {
		return false
	}
	return *a == *b
}

// equalICCGray 比较两个不可变灰度校准配置是否相同
// 入参: a 首个配置, b 对比配置
// 返回: bool 是否相同
func equalICCGray(a, b *iccGraySpace) bool {
	if a == b {
		return true
	}
	if a == nil || b == nil {
		return false
	}
	return a.lab == b.lab && a.matrix == b.matrix && equalICCToneCurve(a.curve, b.curve)
}

// equalICCRGB 比较两个不可变RGB校准配置是否相同
// 入参: a 首个配置, b 对比配置
// 返回: bool 是否相同
func equalICCRGB(a, b *iccRGBSpace) bool {
	if a == b {
		return true
	}
	if a == nil || b == nil {
		return false
	}
	if a.matrix != b.matrix || a.inverse != b.inverse {
		return false
	}
	for i := range a.curves {
		if !equalICCToneCurve(a.curves[i], b.curves[i]) {
			return false
		}
	}
	return true
}

// equalICCToneCurve 比较两条不可变色调响应曲线定义是否相同
// 入参: a 首条曲线, b 对比曲线
// 返回: bool 是否相同
func equalICCToneCurve(a, b iccToneCurve) bool {
	return a.function == b.function && a.direction == b.direction && slices.Equal(a.samples, b.samples) && slices.Equal(a.parameters, b.parameters)
}

// Digest 返回颜色空间配置的确定性摘要，仅规范模型及含原始签名或配置数据的空间返回true，无原始数据的CalGray/CalRGB与复合空间返回false，nil接收者返回零值与true
// 返回: [32]byte 内容摘要, bool 是否生成确定性摘要
func (s *ColorSpace) Digest() ([32]byte, bool) {
	var zero [32]byte
	if s == nil {
		return zero, true
	}
	h := sha256.New()
	var buf [8]byte
	writeU64 := func(v uint64) {
		binary.LittleEndian.PutUint64(buf[:], v)
		h.Write(buf[:])
	}
	writeFloat := func(v float64) {
		if v == 0 {
			v = 0
		}
		writeU64(math.Float64bits(v))
	}
	writeU64(uint64(len(s.Model)))
	h.Write([]byte(s.Model))
	if s.mapped {
		h.Write([]byte{1})
	} else {
		h.Write([]byte{0})
	}
	if s.profile == nil {
		h.Write([]byte{0})
		var sum [32]byte
		h.Sum(sum[:0])
		return sum, true
	}
	if s.profile.alternate != nil || s.profile.alternateBlending != nil {
		return zero, false
	}
	h.Write([]byte{1})
	if s.profile.signature != nil {
		h.Write([]byte{1})
		h.Write(s.profile.signature[:])
	} else if len(s.profile.data) != 0 {
		h.Write([]byte{2})
		writeU64(uint64(len(s.profile.data)))
		h.Write(s.profile.data)
	} else {
		return zero, false
	}
	if s.profile.lab {
		h.Write([]byte{1})
	} else {
		h.Write([]byte{0})
	}
	writeU64(uint64(len(s.profile.intent)))
	h.Write([]byte(s.profile.intent))
	if s.profile.ranges != nil {
		h.Write([]byte{1})
		for _, v := range *s.profile.ranges {
			writeFloat(v)
		}
	} else {
		h.Write([]byte{0})
	}
	var sum [32]byte
	h.Sum(sum[:0])
	return sum, true
}

// SRGBEquivalent 检查混合空间与sRGB的偏差是否在8位量化精度内
// 返回: bool 是否等价
func (s *ColorSpace) SRGBEquivalent() bool {
	if s == nil || s.Model != "DeviceRGB" {
		return false
	}
	if s.profile == nil {
		return true
	}
	if cached := atomic.LoadUint32(&s.srgbState); cached != 0 {
		return cached == 2
	}
	equivalent := s.srgbEquivalent()
	cached := uint32(1)
	if equivalent {
		cached = 2
	}
	atomic.StoreUint32(&s.srgbState, cached)
	return equivalent
}

// srgbEquivalent 核对不可变校准变换的轴向与三维采样，不改变颜色分量
// 返回: bool 是否在8位量化精度内等价
func (s *ColorSpace) srgbEquivalent() bool {
	if s.profile.rgb == nil {
		return false
	}
	check := func(input [3]float64) bool {
		output, err := s.SRGB(input[:], "RelativeColorimetric")
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

// Components 返回颜色空间的分量数
// 返回: int 分量数
func (s *ColorSpace) Components() int {
	if s == nil {
		return 0
	}
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

// SRGB 将颜色空间分量转换为sRGB显示颜色
// 入参: values 单位分量, intent 渲染意图
// 返回: [3]float64 sRGB颜色, error 分量或变换错误
func (s *ColorSpace) SRGB(values []float64, intent Name) ([3]float64, error) {
	if err := s.validate(values); err != nil {
		return [3]float64{}, err
	}
	return s.srgb(values, intent)
}

// srgb 转换已验证的源分量，不重复检查分量数量及范围
// 入参: values 源分量, intent 渲染意图
// 返回: [3]float64 sRGB颜色, error 变换错误
func (s *ColorSpace) srgb(values []float64, intent Name) ([3]float64, error) {
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
	return [3]float64{}, &UnsupportedError{Feature: "source color space " + string(s.Model)}
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
	if s == nil {
		return result, fmt.Errorf("invalid destination color space")
	}
	if err := source.validate(values); err != nil {
		return result, err
	}
	return s.convert(values, source, normalizeRenderingIntent(intent), conversion, s.Equal(source))
}

// convert 转换已验证的分量，复用相同空间判定
// 入参: values 源分量, source 源空间, intent 渲染意图, conversion 设备转换函数, same 是否同空间
// 返回: [4]float64 目标分量, error 变换错误
func (s *ColorSpace) convert(values []float64, source *ColorSpace, intent Name, conversion ColorConversion, same bool) ([4]float64, error) {
	var result [4]float64
	if same {
		copy(result[:], values)
		return result, nil
	}
	if s.Calibrated() {
		xyz, err := source.xyz(values, intent)
		if err != nil {
			return result, err
		}
		return s.profile.fromXYZ(xyz, intent)
	}
	rgb, err := source.srgb(values, intent)
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
	switch s.Model {
	case "DeviceGray":
		return values[0], nil
	case "DeviceRGB":
		return .3*values[0] + .59*values[1] + .11*values[2], nil
	case "DeviceCMYK":
		return (.3*(1-values[0]) + .59*(1-values[1]) + .11*(1-values[2])) * (1 - values[3]), nil
	}
	return 0, &UnsupportedError{Feature: "luminosity color space " + string(s.Model)}
}

// validate 检查颜色分量数与单位范围
// 入参: values 颜色分量
// 返回: error 无效颜色
func (s *ColorSpace) validate(values []float64) error {
	components := s.Components()
	if components == 0 || len(values) != components {
		return fmt.Errorf("invalid color component count")
	}
	for _, v := range values {
		if math.IsNaN(v) || math.IsInf(v, 0) || v < 0 || v > 1 {
			return fmt.Errorf("invalid color component")
		}
	}
	return nil
}

// readBlendingSpaceContext 在取消上下文内解析设备或可双向变换的ICC混合空间
// 入参: ctx 取消上下文, value 颜色空间定义
// 返回: *ColorSpace 混合空间, error 格式、能力或取消错误
func (r *Reader) readBlendingSpaceContext(ctx context.Context, value Object) (*ColorSpace, error) {
	s, err := r.readEffectiveColorSpaceContext(ctx, value, false)
	if err != nil {
		return nil, err
	}
	for s.profile != nil {
		if err := s.profile.validateBlending(); err != nil {
			return nil, err
		}
		if s.profile.alternate != nil {
			s = s.profile.alternateBlending
			continue
		}
		break
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
	return r.readEffectiveColorSpace(value, false)
}

// readEffectiveColorSpace 读取原始或已按资源校验的设备及校准空间
// 入参: value 颜色定义, effective 是否已按资源校验并替换
// 返回: *ColorSpace 源空间, error 定义或配置错误
func (r *Reader) readEffectiveColorSpace(value Object, effective bool) (*ColorSpace, error) {
	return r.readEffectiveColorSpaceContext(context.Background(), value, effective)
}

// readEffectiveColorSpaceContext 在取消上下文内读取原始或已按资源校验的设备及校准空间
// 入参: ctx 取消上下文, value 颜色定义, effective 是否已按资源校验并替换
// 返回: *ColorSpace 源空间, error 定义、配置或取消错误
func (r *Reader) readEffectiveColorSpaceContext(ctx context.Context, value Object, effective bool) (*ColorSpace, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
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
		profile, err := r.readICCSourceSpaceContext(ctx, array, effective)
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
	profile := &iccColorSpace{absoluteScale: [3]float64{1, 1, 1}}
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
