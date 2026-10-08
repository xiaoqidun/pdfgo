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

// ColorantSpace 返回轴向渐变的只读原始色料定义
// 返回: *ColorantSpace 色料定义，普通颜色空间为nil
func (g *AxialGradient) ColorantSpace() *ColorantSpace {
	if g == nil || g.function == nil {
		return nil
	}
	return g.function.colorants
}

// ColorantSpace 返回径向渐变的只读原始色料定义
// 返回: *ColorantSpace 色料定义，普通颜色空间为nil
func (g *RadialGradient) ColorantSpace() *ColorantSpace {
	if g == nil || g.function == nil {
		return nil
	}
	return g.function.colorants
}

// ColorantSpace 返回函数着色的只读原始色料定义
// 返回: *ColorantSpace 色料定义，普通颜色空间为nil
func (g *FunctionGradient) ColorantSpace() *ColorantSpace {
	if g == nil || g.tint == nil {
		return nil
	}
	return g.tint.colorants
}

// ColorantSpace 返回网格着色的只读原始色料定义
// 返回: *ColorantSpace 色料定义，普通颜色空间为nil
func (g *MeshGradient) ColorantSpace() *ColorantSpace {
	if g == nil || g.tint == nil {
		return nil
	}
	return g.tint.colorants
}

// ColorantValuesAt 将轴向渐变的原始浓度写入调用方缓冲
// 入参: position 归一化轴位置, out 与色料数等长的输出缓冲
// 返回: error 非色料渐变、参数或函数错误，错误时不保证缓冲内容
func (g *AxialGradient) ColorantValuesAt(position float64, out []float64) error {
	if g == nil {
		return fmt.Errorf("invalid gradient colorant evaluation")
	}
	return gradientColorantValues(g.function, g.domain, position, out)
}

// ColorantValuesAt 将径向渐变的原始浓度写入调用方缓冲
// 入参: position 归一化双圆插值位置, out 与色料数等长的输出缓冲
// 返回: error 非色料渐变、参数或函数错误，错误时不保证缓冲内容
func (g *RadialGradient) ColorantValuesAt(position float64, out []float64) error {
	if g == nil {
		return fmt.Errorf("invalid gradient colorant evaluation")
	}
	return gradientColorantValues(g.function, g.domain, position, out)
}

// ColorantValuesAt 求值二维源函数并裁切浓度，不经过备用色变换
// 入参: point 定义域坐标, out 与色料数等长的输出缓冲
// 返回: error 非色料渐变、参数或函数错误，错误时不保证缓冲内容
func (g *FunctionGradient) ColorantValuesAt(point Point, out []float64) error {
	space := g.ColorantSpace()
	if space == nil || len(out) != len(space.Names) || g.function == nil || len(out) != g.function.outputs || math.IsNaN(point.X) || math.IsNaN(point.Y) || math.IsInf(point.X, 0) || math.IsInf(point.Y, 0) {
		return fmt.Errorf("invalid function colorant evaluation")
	}
	point.X = math.Max(g.Domain.XMin, math.Min(g.Domain.XMax, point.X))
	point.Y = math.Max(g.Domain.YMin, math.Min(g.Domain.YMax, point.Y))
	if err := g.function.evaluate([2]float64{point.X, point.Y}, out); err != nil {
		return err
	}
	return clampColorantValues(out)
}

// ColorantValuesAt 插值网格源分量并求值函数，不分配浓度快照
// 入参: patch 网格序号, u 横向参数, v 纵向参数, out 与色料数等长的输出缓冲
// 返回: error 非色料渐变、参数或函数错误，错误时不保证缓冲内容
func (g *MeshGradient) ColorantValuesAt(patch int, u, v float64, out []float64) error {
	space := g.ColorantSpace()
	if space == nil || len(out) != len(space.Names) {
		return fmt.Errorf("invalid mesh colorant component count")
	}
	weights, colors, err := g.sourceColorsAt(patch, u, v)
	if err != nil {
		return err
	}
	components := len(colors[0])
	if g.function != nil {
		components = len(g.function.parts)
	}
	if components != len(out) {
		return fmt.Errorf("invalid mesh colorant component count")
	}
	clear(out)
	if err := g.sourceValues(weights, colors, out); err != nil {
		return err
	}
	return clampColorantValues(out)
}

// ColorantAt 求值轴向渐变的源色料浓度，不使用备用色标反推浓度
// 入参: position 归一化轴位置，范围外使用端点
// 返回: *ColorantPaint 色料快照，非色料渐变为nil, error 参数或函数错误
func (g *AxialGradient) ColorantAt(position float64) (*ColorantPaint, error) {
	return gradientColorantAt(g.function, g.domain, position)
}

// ColorantAt 求值径向渐变的源色料浓度，不使用备用空间分量替代源浓度
// 入参: position 归一化双圆插值位置，范围外使用端点
// 返回: *ColorantPaint 色料快照，非色料渐变为nil, error 参数或函数错误
func (g *RadialGradient) ColorantAt(position float64) (*ColorantPaint, error) {
	return gradientColorantAt(g.function, g.domain, position)
}

// ColorantAt 求值二维函数渐变的原始色料，不预先转换到组混合空间
// 入参: point 定义域坐标，不含Matrix变换
// 返回: *ColorantPaint 色料快照，非色料渐变为nil, error 参数或函数错误
func (g *FunctionGradient) ColorantAt(point Point) (*ColorantPaint, error) {
	if g.tint == nil || g.tint.colorants == nil {
		return nil, nil
	}
	components := len(g.tint.colorants.Names)
	var buffer [32]float64
	values := buffer[:min(components, len(buffer))]
	if components > len(buffer) {
		values = make([]float64, components)
	}
	if err := g.ColorantValuesAt(point, values); err != nil {
		return nil, err
	}
	return colorantSnapshot(g.tint.colorants, values)
}

// ColorantAt 先插值网格源分量并求值函数，再保留色料浓度快照
// 入参: patch 网格序号, u 横向参数或第二顶点权重, v 纵向参数或第三顶点权重
// 返回: *ColorantPaint 色料快照，非色料网格为nil, error 参数或函数错误
func (g *MeshGradient) ColorantAt(patch int, u, v float64) (*ColorantPaint, error) {
	if g.tint == nil || g.tint.colorants == nil {
		return nil, nil
	}
	components := len(g.tint.colorants.Names)
	var buffer [32]float64
	values := buffer[:min(components, len(buffer))]
	if components > len(buffer) {
		values = make([]float64, components)
	}
	if err := g.ColorantValuesAt(patch, u, v, values); err != nil {
		return nil, err
	}
	return colorantSnapshot(g.tint.colorants, values)
}

// gradientColorantFunction 复用已编译源函数，将常见分量写入调用方缓冲
// 入参: source 源函数, components 源分量数
// 返回: func(float64, []float64) error 原始色料求值函数
func gradientColorantFunction(source *gradientFunction, components int) func(float64, []float64) error {
	return func(x float64, out []float64) error {
		values, err := source.evaluate(x)
		copy(out, values[:components])
		return err
	}
}

// gradientColorantAt 求值原始函数，保留定义域和源浓度裁切，不反向转换备用色
// 入参: function 映射后的渐变函数, domain 定义域, position 归一化位置
// 返回: *ColorantPaint 色料快照，非色料渐变为nil, error 参数或函数错误
func gradientColorantAt(function *gradientFunction, domain [2]float64, position float64) (*ColorantPaint, error) {
	if function == nil || function.colorants == nil {
		return nil, nil
	}
	components := len(function.colorants.Names)
	var buffer [32]float64
	values := buffer[:min(components, len(buffer))]
	if components > len(buffer) {
		values = make([]float64, components)
	}
	if err := gradientColorantValues(function, domain, position, values); err != nil {
		return nil, err
	}
	return colorantSnapshot(function.colorants, values)
}

// gradientColorantValues 在源定义域求值并裁切色料浓度，不使用备用函数区间
// 入参: function 渐变函数, domain 定义域, position 归一化位置, out 浓度缓冲
// 返回: error 定义、分量或函数错误
func gradientColorantValues(function *gradientFunction, domain [2]float64, position float64, out []float64) error {
	if function == nil || function.colorants == nil || function.colorant == nil || len(out) != len(function.colorants.Names) || math.IsNaN(position) || math.IsInf(position, 0) {
		return fmt.Errorf("invalid gradient colorant evaluation")
	}
	position = math.Max(0, math.Min(1, position))
	if err := function.colorant(functionValue(position, domain[0], domain[1]), out); err != nil {
		return err
	}
	return clampColorantValues(out)
}

// clampColorantValues 检查非有限结果并裁切到单位浓度区间
// 入参: values 原始浓度及输出
// 返回: error 非有限颜色错误
func clampColorantValues(values []float64) error {
	for i, value := range values {
		if math.IsNaN(value) || math.IsInf(value, 0) {
			return fmt.Errorf("nonfinite gradient color")
		}
		values[i] = math.Max(0, math.Min(1, value))
	}
	return nil
}

// colorantSnapshot 核验渐变源浓度后建立独立快照，不掩盖非有限函数结果
// 入参: space 色料定义, values 原始函数或网格分量
// 返回: *ColorantPaint 浓度快照, error 分量错误
func colorantSnapshot(space *ColorantSpace, values []float64) (*ColorantPaint, error) {
	if len(values) != len(space.Names) {
		return nil, fmt.Errorf("invalid colorant component count")
	}
	for _, value := range values {
		if math.IsNaN(value) || math.IsInf(value, 0) {
			return nil, fmt.Errorf("nonfinite gradient color")
		}
	}
	return newColorantPaint(space, values), nil
}

// sourceColorsAt 核验网格参数及参与插值的源分量，保持三角形与曲面角点顺序
// 入参: patch 网格序号, u 横向参数或第二顶点权重, v 纵向参数或第三顶点权重
// 返回: [4]float64 插值权重, [4][]float64 源分量, error 参数或分量错误
func (g *MeshGradient) sourceColorsAt(patch int, u, v float64) ([4]float64, [4][]float64, error) {
	weights, colors := [4]float64{}, [4][]float64{}
	if patch < 0 || patch >= len(g.Patches)+len(g.Triangles) || math.IsNaN(u) || math.IsNaN(v) || u < 0 || u > 1 || v < 0 || v > 1 {
		return weights, colors, fmt.Errorf("invalid mesh evaluation")
	}
	if patch < len(g.Patches) {
		weights = [4]float64{(1 - u) * (1 - v), (1 - u) * v, u * v, u * (1 - v)}
		colors = g.Patches[patch].Colors
	} else {
		if u+v > 1+1e-12 {
			return weights, colors, fmt.Errorf("invalid triangle evaluation")
		}
		weights = [4]float64{math.Max(0, 1-u-v), u, v, 0}
		copy(colors[:], g.Triangles[patch-len(g.Patches)].Colors[:])
	}
	if len(colors[0]) == 0 {
		return weights, colors, fmt.Errorf("missing mesh color components")
	}
	for i, weight := range weights {
		if weight != 0 && len(colors[i]) != len(colors[0]) {
			return weights, colors, fmt.Errorf("inconsistent mesh color components")
		}
	}
	return weights, colors, nil
}

// sourceValues 插值源色料或函数输入，不执行备用色及校准变换
// 入参: weights 网格权重, colors 已校验的源分量, out 色料缓冲
// 返回: error 函数或分量错误
func (g *MeshGradient) sourceValues(weights [4]float64, colors [4][]float64, out []float64) error {
	if g.function != nil {
		x, err := meshFunctionInput(weights, colors)
		if err != nil {
			return err
		}
		return g.function.calculate(x, out)
	}
	meshInterpolate(weights, colors, out)
	return nil
}

// meshFunctionInput 插值网格函数的单输入，拒绝混用颜色分量
// 入参: weights 网格权重, colors 已校验的源分量
// 返回: float64 函数输入, error 分量错误
func meshFunctionInput(weights [4]float64, colors [4][]float64) (float64, error) {
	if len(colors[0]) != 1 {
		return 0, fmt.Errorf("invalid mesh function input count")
	}
	x := 0.0
	for i, weight := range weights {
		if weight != 0 {
			x += colors[i][0] * weight
		}
	}
	return x, nil
}

// meshInterpolate 将已校验的源分量写入零值缓冲，不产生逐像素分配
// 入参: weights 网格权重, colors 源分量, out 插值缓冲
func meshInterpolate(weights [4]float64, colors [4][]float64, out []float64) {
	for i, weight := range weights {
		if weight != 0 {
			for c, value := range colors[i] {
				out[c] += value * weight
			}
		}
	}
}
