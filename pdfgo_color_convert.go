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
	"sort"
)

// xyz 将颜色分量转换到D50连接空间，避免通过sRGB中转截断色域
// 入参: values 已校验的颜色分量, intent 渲染意图
// 返回: [3]float64 D50色度, error 变换错误
func (s *ColorSpace) xyz(values []float64, intent Name) ([3]float64, error) {
	if p := s.profile; p != nil {
		return p.xyz(values, intent)
	}
	rgb, err := s.RGB(values, intent)
	if err != nil {
		return [3]float64{}, err
	}
	for i, v := range rgb {
		if v <= .04045 {
			rgb[i] = v / 12.92
		} else {
			rgb[i] = math.Pow((v+.055)/1.055, 2.4)
		}
	}
	r, g, b := rgb[0], rgb[1], rgb[2]
	cone := bradford([3]float64{.4124564*r + .3575761*g + .1804375*b, .2126729*r + .7151522*g + .0721750*b, .0193339*r + .1191920*g + .9503041*b})
	source, target := bradford([3]float64{.95047, 1, 1.08883}), bradford([3]float64{.9642, 1, .8249})
	for i := range cone {
		cone[i] *= target[i] / source[i]
	}
	return [3]float64{.9869929*cone[0] - .1470543*cone[1] + .1599627*cone[2], .4323053*cone[0] + .5183603*cone[1] + .0492912*cone[2], -.0085287*cone[0] + .0400428*cone[1] + .9684867*cone[2]}, nil
}

// xyz 按渲染意图取得ICC连接空间色度，传统绝对色度由相对色度与媒体白点换算
// 入参: values 单位分量, intent 渲染意图
// 返回: [3]float64 色度, error 变换错误
func (s *iccColorSpace) xyz(values []float64, intent Name) ([3]float64, error) {
	if s.alternate != nil {
		var input [4]float64
		copy(input[:], values)
		return s.alternateXYZ(input, intent)
	}
	device := s.deviceValues(values)
	values = device[:len(values)]
	if xyz, used, err := s.processXYZ(values, intent); used || err != nil {
		return xyz, err
	}
	if s.lab {
		device[0] /= 100
		device[1], device[2] = (device[1]+128)/255, (device[2]+128)/255
	}
	legacy := intent
	if legacy == "AbsoluteColorimetric" {
		legacy = "RelativeColorimetric"
	}
	var xyz [3]float64
	if s.lut != nil {
		var err error
		xyz, err = s.lut.xyz(values, legacy)
		if err != nil {
			return xyz, err
		}
	} else {
		if _, err := iccIntentIndex(legacy); err != nil {
			return xyz, err
		}
		if s.gray != nil {
			y := s.gray.curve.evaluate(values[0])
			if s.gray.lab {
				xyz = iccFloatLabXYZ([3]float64{100 * y, 0, 0})
			} else {
				xyz = s.gray.matrix.linearXYZ(y, y, y)
			}
		} else {
			xyz = s.rgb.matrix.offset
			for c, curve := range s.rgb.curves {
				v := curve.evaluate(values[c])
				for i := range xyz {
					xyz[i] += s.rgb.matrix.matrix[3*c+i] * v
				}
			}
		}
	}
	if intent == "AbsoluteColorimetric" {
		return s.absoluteXYZ(xyz, false)
	}
	return xyz, nil
}

// absoluteXYZ 按ICC.1第6.3.2.2节在媒体相对色度与绝对色度间换算，不重复应用色适应矩阵
// 入参: xyz 连接空间色度, inverse 是否由绝对色度恢复相对色度
// 返回: [3]float64 换算色度, error 缺少或无效的媒体白点
func (s *iccColorSpace) absoluteXYZ(xyz [3]float64, inverse bool) ([3]float64, error) {
	for i, scale := range s.absoluteScale {
		if scale == 0 {
			return [3]float64{}, fmt.Errorf("invalid ICC media white point")
		}
		if inverse {
			xyz[i] /= scale
		} else {
			xyz[i] *= scale
		}
	}
	return xyz, nil
}

// fromXYZ 将D50连接空间转换到ICC合成分量
// 入参: xyz D50色度, intent 渲染意图
// 返回: [4]float64 合成分量, error 不可逆或未支持的变换
func (s *iccColorSpace) fromXYZ(xyz [3]float64, intent Name) ([4]float64, error) {
	if s.alternate != nil {
		return s.alternateFromXYZ(xyz, intent)
	}
	values, err := s.fromDeviceXYZ(xyz, intent)
	if err != nil || s.ranges == nil && !s.lab {
		return values, err
	}
	return s.normalize(values[:s.components()]), nil
}

// fromDeviceXYZ 将连接空间转换为ICC设备单位，不混淆Lab设备值与PCS编码
// 入参: xyz D50色度, intent 渲染意图
// 返回: [4]float64 ICC设备分量, error 不可逆或未支持的变换
func (s *iccColorSpace) fromDeviceXYZ(xyz [3]float64, intent Name) ([4]float64, error) {
	lut, floating, err := s.inverseTransforms()
	if err != nil {
		return [4]float64{}, err
	}
	index, err := iccProcessIntentIndex(intent)
	if err != nil {
		return [4]float64{}, err
	}
	transform := floating[index]
	if intent == "AbsoluteColorimetric" && transform == nil {
		xyz, err = s.absoluteXYZ(xyz, true)
		if err != nil {
			return [4]float64{}, err
		}
		intent = "RelativeColorimetric"
		transform = floating[1]
	}
	if transform != nil {
		if s.processPCS == "Lab " {
			xyz = iccFloatXYZLab(xyz)
		}
		return transform.evaluate(xyz[:])
	}
	legacyIndex, err := iccIntentIndex(intent)
	if err != nil {
		return [4]float64{}, err
	}
	if s.lut != nil || lut != nil && (lut.fromPCS[legacyIndex] != nil || lut.fromPCS[0] != nil) {
		values, err := lut.fromXYZ(xyz, intent)
		if s.lab {
			values[0] *= 100
			values[1], values[2] = values[1]*255-128, values[2]*255-128
		}
		return values, err
	}
	var result [4]float64
	if s.gray != nil {
		if s.gray.lab {
			value, err := s.gray.curve.inverse(iccFloatXYZLab(xyz)[0] / 100)
			result[0] = value
			return result, err
		}
		m := s.gray.matrix
		scale := m.matrix[1] + m.matrix[4] + m.matrix[7]
		if scale == 0 {
			return result, fmt.Errorf("singular ICC gray matrix")
		}
		value, err := s.gray.curve.inverse((xyz[1] - m.offset[1]) / scale)
		result[0] = value
		return result, err
	}
	m := s.rgb.matrix.matrix
	for n := range xyz {
		xyz[n] -= s.rgb.matrix.offset[n]
	}
	inverse := s.rgb.inverse
	if inverse == ([9]float64{}) {
		inverse = iccInverseMatrix(m)
	}
	if inverse == ([9]float64{}) {
		return result, fmt.Errorf("singular ICC colorant matrix")
	}
	for i, curve := range s.rgb.curves {
		linear := inverse[3*i]*xyz[0] + inverse[3*i+1]*xyz[1] + inverse[3*i+2]*xyz[2]
		v, err := curve.inverse(linear)
		if err != nil {
			return result, err
		}
		result[i] = v
	}
	return result, nil
}

// iccInverseMatrix 预计算RGB矩阵的逆向系数，不拒绝只能用于源绘制的配置
// 入参: matrix 正向色度矩阵
// 返回: [9]float64 逆向系数，不可逆时为零
func iccInverseMatrix(matrix [9]float64) [9]float64 {
	m := matrix
	inverse := [9]float64{m[4]*m[8] - m[5]*m[7], m[5]*m[6] - m[3]*m[8], m[3]*m[7] - m[4]*m[6], m[2]*m[7] - m[1]*m[8], m[0]*m[8] - m[2]*m[6], m[1]*m[6] - m[0]*m[7], m[1]*m[5] - m[2]*m[4], m[2]*m[3] - m[0]*m[5], m[0]*m[4] - m[1]*m[3]}
	determinant := m[0]*inverse[0] + m[1]*inverse[1] + m[2]*inverse[2]
	if determinant == 0 || math.IsNaN(determinant) || math.IsInf(determinant, 0) {
		return [9]float64{}
	}
	for i := range inverse {
		inverse[i] /= determinant
		if math.IsNaN(inverse[i]) || math.IsInf(inverse[i], 0) {
			return [9]float64{}
		}
	}
	return inverse
}

// iccProcessIntentIndex 选择浮点变换的渲染意图，绝对色度使用独立标签
// 入参: intent 渲染意图
// 返回: int 标签序号, error 未知意图
func iccProcessIntentIndex(intent Name) (int, error) {
	if intent == "AbsoluteColorimetric" {
		return 3, nil
	}
	return iccIntentIndex(intent)
}

// processXYZ 按对应浮点标签取得连接空间色度，无标签时保留原变换
// 入参: values 设备分量, intent 渲染意图
// 返回: [3]float64 色度, bool 是否使用浮点标签, error 求值错误
func (s *iccColorSpace) processXYZ(values []float64, intent Name) ([3]float64, bool, error) {
	index, err := iccProcessIntentIndex(intent)
	if err != nil {
		return [3]float64{}, false, err
	}
	transform := s.toFloat[index]
	absolute := intent == "AbsoluteColorimetric" && transform == nil
	if absolute {
		transform = s.toFloat[1]
	}
	if transform == nil {
		return [3]float64{}, false, nil
	}
	v, err := transform.evaluate(values)
	if err != nil {
		return [3]float64{}, true, err
	}
	xyz := [3]float64{v[0], v[1], v[2]}
	if s.processPCS == "Lab " {
		xyz = iccFloatLabXYZ(xyz)
	}
	if absolute {
		xyz, err = s.absoluteXYZ(xyz, false)
		return xyz, true, err
	}
	return xyz, true, nil
}

// iccFloatLabXYZ 将浮点Lab色度转换为D50的XYZ，不截断负值
// 入参: lab Lab色度
// 返回: [3]float64 XYZ色度
func iccFloatLabXYZ(lab [3]float64) [3]float64 {
	y := (lab[0] + 16) / 116
	inverse := func(v float64) float64 {
		if v > 6.0/29 {
			return v * v * v
		}
		return 3 * (6.0 / 29) * (6.0 / 29) * (v - 4.0/29)
	}
	return [3]float64{.9642 * inverse(y+lab[1]/500), inverse(y), .8249 * inverse(y-lab[2]/200)}
}

// iccFloatXYZLab 将D50的XYZ转换为浮点Lab色度，不截断负值
// 入参: xyz XYZ色度
// 返回: [3]float64 Lab色度
func iccFloatXYZLab(xyz [3]float64) [3]float64 {
	f := func(v float64) float64 {
		if v > (6.0/29)*(6.0/29)*(6.0/29) {
			return math.Cbrt(v)
		}
		return v/(3*(6.0/29)*(6.0/29)) + 4.0/29
	}
	x, y, z := f(xyz[0]/.9642), f(xyz[1]), f(xyz[2]/.8249)
	return [3]float64{116*y - 16, 500 * (x - y), 200 * (y - z)}
}

// inverse 反求单调ICC曲线，对超出曲线范围的值使用端点
// 入参: value 线性分量
// 返回: float64 编码分量, error 不可逆的参数曲线
func (c iccToneCurve) inverse(value float64) (float64, error) {
	if len(c.parameters) > 0 {
		if c.direction == 2 || c.direction == 0 && c.evaluate(0) == c.evaluate(1) {
			return 0, fmt.Errorf("noninvertible ICC constant curve")
		}
		p := c.parameters
		if c.function >= 3 && (p[1] <= 0 || p[3] < 0) {
			return 0, fmt.Errorf("nonmonotonic ICC parametric curve")
		}
		v := value
		if c.function == 0 {
			return math.Pow(math.Max(0, math.Min(1, v)), 1/p[0]), nil
		}
		if c.function <= 2 {
			if c.function == 2 {
				v -= p[3]
			}
			v = (math.Pow(math.Max(0, v), 1/p[0]) - p[2]) / p[1]
		} else {
			offset, lowOffset := 0.0, 0.0
			if c.function == 4 {
				offset, lowOffset = p[5], p[6]
			}
			boundary := math.Inf(1)
			if p[4] <= 1 {
				boundary = math.Pow(p[1]*math.Max(0, p[4])+p[2], p[0]) + offset
			}
			lower := p[3]*p[4] + lowOffset
			if p[4] > 0 && p[4] < 1 && boundary < lower-1.0/65536 {
				return 0, fmt.Errorf("nonmonotonic ICC parametric curve")
			}
			if v < boundary && p[4] > 0 {
				if p[3] == 0 {
					v = 0
				} else {
					v = math.Min(p[4], (v-lowOffset)/p[3])
				}
			} else {
				v = (math.Pow(math.Max(0, v-offset), 1/p[0]) - p[2]) / p[1]
			}
		}
		return math.Max(0, math.Min(1, v)), nil
	}
	if len(c.samples) > 1 {
		direction := c.direction
		if direction == 0 {
			direction = iccCurveDirection(c.samples)
		}
		if direction == 2 {
			return 0, fmt.Errorf("noninvertible ICC sampled curve")
		}
		sign := float64(direction)
		v := value * 65535 * sign
		i := sort.Search(len(c.samples), func(i int) bool { return float64(c.samples[i])*sign > v })
		if i == 0 {
			return 0, nil
		}
		if i == len(c.samples) {
			if v == float64(c.samples[i-1])*sign {
				i = sort.Search(len(c.samples), func(i int) bool { return float64(c.samples[i])*sign >= v })
				return float64(i) / float64(len(c.samples)-1), nil
			}
			return 1, nil
		}
		fraction := (v - float64(c.samples[i-1])*sign) / ((float64(c.samples[i]) - float64(c.samples[i-1])) * sign)
		return (float64(i-1) + fraction) / float64(len(c.samples)-1), nil
	}
	return math.Max(0, math.Min(1, value)), nil
}
