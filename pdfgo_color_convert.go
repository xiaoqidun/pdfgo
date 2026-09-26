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
		if p.lut != nil {
			return p.lut.xyz(values, intent)
		}
		if _, err := iccIntentIndex(intent); err != nil {
			return [3]float64{}, err
		}
		if p.gray != nil {
			y := p.gray.curve.evaluate(values[0])
			return [3]float64{.9642 * y, y, .8249 * y}, nil
		}
		var xyz [3]float64
		for c, curve := range p.rgb.curves {
			v := curve.evaluate(values[c])
			for i := range xyz {
				xyz[i] += p.rgb.matrix.matrix[3*c+i] * v
			}
		}
		return xyz, nil
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

// fromXYZ 将D50连接空间转换到ICC设备分量
// 入参: xyz D50色度, intent 渲染意图
// 返回: [4]float64 设备分量, error 不可逆或未支持的变换
func (s *iccColorSpace) fromXYZ(xyz [3]float64, intent Name) ([4]float64, error) {
	if s.lut != nil {
		return s.lut.fromXYZ(xyz, intent)
	}
	var result [4]float64
	if _, err := iccIntentIndex(intent); err != nil {
		return result, err
	}
	if s.gray != nil {
		value, err := s.gray.curve.inverse(xyz[1])
		result[0] = value
		return result, err
	}
	m := s.rgb.matrix.matrix
	cofactor := [9]float64{m[4]*m[8] - m[5]*m[7], m[5]*m[6] - m[3]*m[8], m[3]*m[7] - m[4]*m[6], m[2]*m[7] - m[1]*m[8], m[0]*m[8] - m[2]*m[6], m[1]*m[6] - m[0]*m[7], m[1]*m[5] - m[2]*m[4], m[2]*m[3] - m[0]*m[5], m[0]*m[4] - m[1]*m[3]}
	determinant := m[0]*cofactor[0] + m[1]*cofactor[1] + m[2]*cofactor[2]
	if determinant == 0 {
		return result, fmt.Errorf("singular ICC colorant matrix")
	}
	for i, curve := range s.rgb.curves {
		linear := (cofactor[3*i]*xyz[0] + cofactor[3*i+1]*xyz[1] + cofactor[3*i+2]*xyz[2]) / determinant
		v, err := curve.inverse(linear)
		if err != nil {
			return result, err
		}
		result[i] = v
	}
	return result, nil
}

// inverse 反求单调ICC曲线，对超出曲线范围的值使用端点
// 入参: value 线性分量
// 返回: float64 编码分量, error 不可逆的参数曲线
func (c iccToneCurve) inverse(value float64) (float64, error) {
	if len(c.parameters) > 0 {
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
		v := value * 65535
		i := sort.Search(len(c.samples), func(i int) bool { return float64(c.samples[i]) >= v })
		if i == 0 {
			return 0, nil
		}
		if i == len(c.samples) {
			return 1, nil
		}
		fraction := (v - float64(c.samples[i-1])) / float64(c.samples[i]-c.samples[i-1])
		return (float64(i-1) + fraction) / float64(len(c.samples)-1), nil
	}
	return math.Max(0, math.Min(1, value)), nil
}
