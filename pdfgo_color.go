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
	"image/color"
	"math"
)

// calRGBSpace 保存校准RGB到D65白点的颜色变换参数
type calRGBSpace struct {
	gamma  [3]float64
	matrix [9]float64
	adapt  [3]float64
}

// readCalRGB 读取校准RGB参数，未支持的非零黑点不作近似处理
// 入参: object CalRGB颜色空间数组
// 返回: *calRGBSpace 颜色变换参数, error 错误信息
func (r *Reader) readCalRGB(object Object) (*calRGBSpace, error) {
	a, ok := object.(Array)
	if !ok || len(a) != 2 || a[0] != Name("CalRGB") {
		return nil, &UnsupportedError{Feature: "indexed base color space"}
	}
	resolved, err := r.Resolve(a[1])
	if err != nil {
		return nil, err
	}
	dict, ok := resolved.(Dictionary)
	if !ok {
		return nil, fmt.Errorf("invalid CalRGB dictionary")
	}
	s := &calRGBSpace{gamma: [3]float64{1, 1, 1}, matrix: [9]float64{1, 0, 0, 0, 1, 0, 0, 0, 1}}
	white := [3]float64{}
	black := [3]float64{}
	for _, field := range []struct {
		key    Name
		values []float64
	}{{"WhitePoint", white[:]}, {"BlackPoint", black[:]}, {"Gamma", s.gamma[:]}, {"Matrix", s.matrix[:]}} {
		value, err := r.Resolve(dict[field.key])
		if err != nil {
			return nil, err
		}
		if value == nil && field.key != "WhitePoint" {
			continue
		}
		values, ok := value.(Array)
		if !ok || len(values) != len(field.values) {
			return nil, fmt.Errorf("invalid CalRGB %s", field.key)
		}
		for n, v := range values {
			number, err := r.number(v)
			if err != nil || math.IsNaN(number) || math.IsInf(number, 0) {
				return nil, fmt.Errorf("invalid CalRGB %s", field.key)
			}
			field.values[n] = number
		}
	}
	if white[0] <= 0 || white[1] != 1 || white[2] <= 0 || s.gamma[0] <= 0 || s.gamma[1] <= 0 || s.gamma[2] <= 0 {
		return nil, fmt.Errorf("invalid CalRGB white point or gamma")
	}
	if black != [3]float64{} {
		return nil, &UnsupportedError{Feature: "CalRGB nonzero black point"}
	}
	source := bradford(white)
	target := bradford([3]float64{0.95047, 1, 1.08883})
	for n := range s.adapt {
		if source[n] <= 0 {
			return nil, fmt.Errorf("invalid CalRGB white point")
		}
		s.adapt[n] = target[n] / source[n]
	}
	return s, nil
}

// bradford 将XYZ变换到用于白点适应的锥体响应空间
// 入参: v XYZ分量
// 返回: [3]float64 锥体响应分量
func bradford(v [3]float64) [3]float64 {
	return [3]float64{0.8951*v[0] + 0.2664*v[1] - 0.1614*v[2], -0.7502*v[0] + 1.7135*v[1] + 0.0367*v[2], 0.0389*v[0] - 0.0685*v[1] + 1.0296*v[2]}
}

// color 应用CalRGB矩阵及白点适应，输出sRGB颜色
// 入参: a 第一分量, b 第二分量, c 第三分量，各分量范围为0至1
// 返回: color.NRGBA64 非预乘sRGB颜色
func (s *calRGBSpace) color(a, b, c float64) color.NRGBA64 {
	a, b, c = math.Pow(a, s.gamma[0]), math.Pow(b, s.gamma[1]), math.Pow(c, s.gamma[2])
	m := s.matrix
	cone := bradford([3]float64{m[0]*a + m[3]*b + m[6]*c, m[1]*a + m[4]*b + m[7]*c, m[2]*a + m[5]*b + m[8]*c})
	for n := range cone {
		cone[n] *= s.adapt[n]
	}
	x := 0.9869929*cone[0] - 0.1470543*cone[1] + 0.1599627*cone[2]
	y := 0.4323053*cone[0] + 0.5183603*cone[1] + 0.0492912*cone[2]
	z := -0.0085287*cone[0] + 0.0400428*cone[1] + 0.9684867*cone[2]
	return color.NRGBA64{R: srgbComponent(3.2404542*x - 1.5371385*y - 0.4985314*z), G: srgbComponent(-0.969266*x + 1.8760108*y + 0.041556*z), B: srgbComponent(0.0556434*x - 0.2040259*y + 1.0572252*z), A: 65535}
}

// srgbComponent 将线性分量映射到sRGB编码范围
// 入参: v 线性颜色分量
// 返回: uint16 16位sRGB分量
func srgbComponent(v float64) uint16 {
	v = math.Max(0, math.Min(1, v))
	if v <= 0.0031308 {
		v *= 12.92
	} else {
		v = 1.055*math.Pow(v, 1/2.4) - 0.055
	}
	return uint16(math.Round(v * 65535))
}
