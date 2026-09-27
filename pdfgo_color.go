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

// labSpace 保存Lab白点和分量范围
type labSpace struct {
	white   [3]float64
	rangeAB [4]float64
	adapt   [3]float64
}

// graphicsColorSpace 保存页面着色使用的校准色或索引色参数
type graphicsColorSpace struct {
	lab     *labSpace
	calRGB  *calRGBSpace
	gray    bool
	palette *imagePalette
}

// paint 将页面颜色操作数转换为画刷，索引色保留基色空间分量
// 入参: values 颜色分量
// 返回: Paint 画刷, error 无效分量
func (s *graphicsColorSpace) paint(values []float64) (Paint, error) {
	count := 3
	if s.palette != nil || s.gray {
		count = 1
	}
	if len(values) != count {
		return Paint{}, fmt.Errorf("invalid color component count")
	}
	for _, v := range values {
		if math.IsNaN(v) || math.IsInf(v, 0) {
			return Paint{}, fmt.Errorf("invalid color component")
		}
	}
	var c color.NRGBA64
	paint := Paint{}
	if s.palette != nil {
		index := int(math.Round(math.Max(0, math.Min(float64(len(s.palette.colors)-1), values[0]))))
		c = s.palette.colors[index]
		paint.Space, paint.Values = s.palette.space, s.palette.values[index]
	} else if s.lab != nil {
		c = s.lab.color(values[0], values[1], values[2])
	} else if s.gray {
		v := math.Max(0, math.Min(1, values[0]))
		c = s.calRGB.color(v, v, v)
	} else {
		c = s.calRGB.color(math.Max(0, math.Min(1, values[0])), math.Max(0, math.Min(1, values[1])), math.Max(0, math.Min(1, values[2])))
	}
	paint.RGB = [3]float64{float64(c.R) / 65535, float64(c.G) / 65535, float64(c.B) / 65535}
	return paint, nil
}

// readLab 读取Lab颜色空间定义
// 入参: object Lab颜色空间数组
// 返回: *labSpace 颜色变换参数, error 错误信息
func (r *Reader) readLab(object Object) (*labSpace, error) {
	a, ok := object.(Array)
	if !ok || len(a) != 2 || a[0] != Name("Lab") {
		return nil, fmt.Errorf("invalid Lab color space")
	}
	resolved, err := r.Resolve(a[1])
	if err != nil {
		return nil, err
	}
	dict, ok := resolved.(Dictionary)
	if !ok {
		return nil, fmt.Errorf("invalid Lab dictionary")
	}
	s := &labSpace{rangeAB: [4]float64{-100, 100, -100, 100}}
	for _, field := range []struct {
		key    Name
		values []float64
	}{{"WhitePoint", s.white[:]}, {"Range", s.rangeAB[:]}} {
		value, err := r.Resolve(dict[field.key])
		if err != nil {
			return nil, err
		}
		if value == nil && field.key == "Range" {
			continue
		}
		values, ok := value.(Array)
		if !ok || len(values) != len(field.values) {
			return nil, fmt.Errorf("invalid Lab %s", field.key)
		}
		for n, item := range values {
			field.values[n], err = r.number(item)
			if err != nil || math.IsNaN(field.values[n]) || math.IsInf(field.values[n], 0) {
				return nil, fmt.Errorf("invalid Lab %s", field.key)
			}
		}
	}
	if s.white[0] <= 0 || s.white[1] != 1 || s.white[2] <= 0 || s.rangeAB[0] > s.rangeAB[1] || s.rangeAB[2] > s.rangeAB[3] {
		return nil, fmt.Errorf("invalid Lab white point or range")
	}
	black, err := r.Resolve(dict["BlackPoint"])
	if err != nil {
		return nil, err
	}
	if black != nil {
		values, err := r.numberArray(black, 3)
		if err != nil || values[0] != 0 || values[1] != 0 || values[2] != 0 {
			return nil, &UnsupportedError{Feature: "Lab nonzero black point"}
		}
	}
	source := bradford(s.white)
	target := bradford([3]float64{0.95047, 1, 1.08883})
	for n := range s.adapt {
		if source[n] <= 0 {
			return nil, fmt.Errorf("invalid Lab white point")
		}
		s.adapt[n] = target[n] / source[n]
	}
	return s, nil
}

// color 将Lab分量转换为sRGB颜色
// 入参: lightness 亮度, a 红绿分量, b 黄蓝分量
// 返回: color.NRGBA64 非预乘sRGB颜色
func (s *labSpace) color(lightness, a, b float64) color.NRGBA64 {
	lightness = math.Max(0, math.Min(100, lightness))
	a = math.Max(s.rangeAB[0], math.Min(s.rangeAB[1], a))
	b = math.Max(s.rangeAB[2], math.Min(s.rangeAB[3], b))
	fy := (lightness + 16) / 116
	fx, fz := fy+a/500, fy-b/200
	inverse := func(v float64) float64 {
		if v > 6.0/29 {
			return v * v * v
		}
		return 3 * (6.0 / 29) * (6.0 / 29) * (v - 4.0/29)
	}
	cone := bradford([3]float64{s.white[0] * inverse(fx), s.white[1] * inverse(fy), s.white[2] * inverse(fz)})
	for n := range cone {
		cone[n] *= s.adapt[n]
	}
	x := 0.9869929*cone[0] - 0.1470543*cone[1] + 0.1599627*cone[2]
	y := 0.4323053*cone[0] + 0.5183603*cone[1] + 0.0492912*cone[2]
	z := -0.0085287*cone[0] + 0.0400428*cone[1] + 0.9684867*cone[2]
	return color.NRGBA64{R: srgbComponent(3.2404542*x - 1.5371385*y - 0.4985314*z), G: srgbComponent(-0.969266*x + 1.8760108*y + 0.041556*z), B: srgbComponent(0.0556434*x - 0.2040259*y + 1.0572252*z), A: 65535}
}

// readCalGray 将单分量校准灰度展开为等价的三轴颜色变换
// 入参: object CalGray颜色空间数组
// 返回: *calRGBSpace 颜色变换参数, error 错误信息
func (r *Reader) readCalGray(object Object) (*calRGBSpace, error) {
	a, ok := object.(Array)
	if !ok || len(a) != 2 || a[0] != Name("CalGray") {
		return nil, fmt.Errorf("invalid CalGray color space")
	}
	resolved, err := r.Resolve(a[1])
	if err != nil {
		return nil, err
	}
	dict, ok := resolved.(Dictionary)
	if !ok {
		return nil, fmt.Errorf("invalid CalGray dictionary")
	}
	white, err := r.numberArray(dict["WhitePoint"], 3)
	if err != nil || white[0] <= 0 || white[1] != 1 || white[2] <= 0 {
		return nil, fmt.Errorf("invalid CalGray white point")
	}
	gamma := 1.0
	value, err := r.Resolve(dict["Gamma"])
	if err != nil {
		return nil, err
	}
	if value != nil {
		gamma, err = r.number(value)
		if err != nil || gamma <= 0 || math.IsNaN(gamma) || math.IsInf(gamma, 0) {
			return nil, fmt.Errorf("invalid CalGray gamma")
		}
	}
	black, err := r.Resolve(dict["BlackPoint"])
	if err != nil {
		return nil, err
	}
	if black != nil {
		values, err := r.numberArray(black, 3)
		if err != nil {
			return nil, fmt.Errorf("invalid CalGray black point")
		}
		for _, v := range values {
			if v < 0 {
				return nil, fmt.Errorf("invalid CalGray black point")
			}
			if v != 0 {
				return nil, &UnsupportedError{Feature: "CalGray nonzero black point"}
			}
		}
	}
	return r.readCalRGB(Array{Name("CalRGB"), Dictionary{
		"WhitePoint": Array{Real(white[0]), Real(white[1]), Real(white[2])},
		"Gamma":      Array{Real(gamma), Real(gamma), Real(gamma)},
		"Matrix":     Array{Real(white[0]), Integer(0), Integer(0), Integer(0), Real(white[1]), Integer(0), Integer(0), Integer(0), Real(white[2])},
	}})
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
