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
	"context"
	"errors"
	"fmt"
	"math"
)

// meshLinearVertex 保存三角形裁切顶点及归一化函数输入
type meshLinearVertex struct {
	point Point
	value float64
}

// LinearTriangles 将连续分段线性颜色函数及专色变换展开为三角形，不改变源网格
// 入参: index 三角形索引，不计曲面数量
// 返回: []MeshTriangle 使用Space颜色分量且无需再求值函数的三角形, error 参数错误或无法等价展开的UnsupportedError
func (g *MeshGradient) LinearTriangles(index int) ([]MeshTriangle, error) {
	return g.LinearTrianglesContext(context.Background(), index)
}

// LinearTrianglesContext 展开连续分段线性网格，取消或失败时不返回部分结果
// 入参: ctx 取消上下文, index 三角形索引，不计曲面数量
// 返回: []MeshTriangle 使用Space颜色分量的独立三角形, error 参数、转换或取消错误
func (g *MeshGradient) LinearTrianglesContext(ctx context.Context, index int) ([]MeshTriangle, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	colors, err := g.LinearColors(index)
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	var unsupported *UnsupportedError
	if err != nil && (!errors.As(err, &unsupported) || g.lab != nil || g.Space.profile != nil && (g.Space.profile.ranges != nil || g.Space.profile.alternate != nil)) {
		return nil, err
	}
	triangle := g.Triangles[index]
	for _, point := range triangle.Points {
		if math.IsNaN(point.X) || math.IsNaN(point.Y) || math.IsInf(point.X, 0) || math.IsInf(point.Y, 0) {
			return nil, fmt.Errorf("nonfinite mesh vertex")
		}
	}
	components := g.Space.Components()
	if err == nil {
		for i := range triangle.Colors {
			triangle.Colors[i] = colors[i][:components:components]
		}
		return []MeshTriangle{triangle}, nil
	}
	if g.function == nil && g.tint != nil && g.tint.components > 1 && g.tint.expressions != nil && g.tint.lab == nil && g.tint.nested == nil {
		return g.linearTintTriangles(ctx, triangle)
	}
	function := g.linearFunction()
	if function == nil || function.linear == nil {
		return nil, err
	}
	lower := min(triangle.Colors[0][0], triangle.Colors[1][0], triangle.Colors[2][0])
	upper := max(triangle.Colors[0][0], triangle.Colors[1][0], triangle.Colors[2][0])
	stops := clipGradientValues(function.linear([2]float64{lower, upper}), gradientUnitBounds(components))
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if len(stops) < 2 || stops[0].Position != 0 || stops[len(stops)-1].Position != 1 {
		return nil, err
	}
	for i, stop := range stops {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if math.IsNaN(stop.Position) || math.IsInf(stop.Position, 0) || i > 0 && stop.Position <= stops[i-1].Position {
			return nil, &UnsupportedError{Feature: "discontinuous mesh function"}
		}
		for _, value := range stop.Values[:components] {
			if math.IsNaN(value) || math.IsInf(value, 0) {
				return nil, fmt.Errorf("nonfinite mesh vertex color")
			}
		}
	}
	var source [3]meshLinearVertex
	for i, point := range triangle.Points {
		source[i] = meshLinearVertex{point, functionPosition(triangle.Colors[i][0], lower, upper)}
	}
	var result []MeshTriangle
	for i := 1; i < len(stops); i++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		a, b := stops[i-1], stops[i]
		var first, second [6]meshLinearVertex
		polygon := clipMeshLinear(first[:0], source[:], a.Position, true)
		polygon = clipMeshLinear(second[:0], polygon, b.Position, false)
		var values [6][4]float64
		for j, vertex := range polygon {
			t := functionPosition(vertex.value, a.Position, b.Position)
			for c := range components {
				values[j][c] = functionValue(t, a.Values[c], b.Values[c])
			}
		}
		for j := 2; j < len(polygon); j++ {
			p, q, r := polygon[0].point, polygon[j-1].point, polygon[j].point
			if (q.X-p.X)*(r.Y-p.Y)-(r.X-p.X)*(q.Y-p.Y) == 0 {
				continue
			}
			result = append(result, MeshTriangle{
				Points: [3]Point{p, q, r},
				Colors: [3][]float64{values[0][:components:components], values[j-1][:components:components], values[j][:components:components]},
			})
		}
	}
	if len(result) == 0 {
		return nil, &UnsupportedError{Feature: "degenerate piecewise mesh triangle"}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return result, nil
}

// linearFunction 获取包含专色变换的网格颜色函数，不近似非线性变换
// 返回: *gradientFunction 备用颜色空间函数，无法提供单输入函数时为空
func (g *MeshGradient) linearFunction() *gradientFunction {
	var function *gradientFunction
	if g.function != nil {
		function = g.function.scalar
	} else if g.tint != nil && g.tint.components == 1 {
		function = &gradientFunction{
			value: func(x float64) [4]float64 { return [4]float64{x} },
			linear: func(interval [2]float64) []GradientStop {
				return []GradientStop{{Position: 0, Values: [4]float64{interval[0]}}, {Position: 1, Values: [4]float64{interval[1]}}}
			},
		}
	}
	if function != nil && g.tint != nil {
		function = deviceNColorFunction(function, g.tint)
	}
	return function
}

// linearTintColors 展开未跨截断边界的多专色仿射变换，不改变源分量
// 入参: triangle 已校验分量数量及有限值的三角形
// 返回: [3][4]float64 备用空间顶点颜色, error 无法等价展开的错误
func (g *MeshGradient) linearTintColors(triangle MeshTriangle) ([3][4]float64, error) {
	var result [3][4]float64
	tint := g.tint
	if tint.expressions == nil || tint.lab != nil || tint.nested != nil {
		return result, &UnsupportedError{Feature: "nonlinear mesh tint transform"}
	}
	for c, expression := range tint.expressions {
		for i := range result {
			result[i][c] = expression[0]
		}
	}
	for c := range tint.components {
		values := [3]float64{triangle.Colors[0][c], triangle.Colors[1][c], triangle.Colors[2][c]}
		values, ok := meshLinearClamp(values, 0, 1)
		if !ok {
			return result, &UnsupportedError{Feature: "clipped mesh tint input"}
		}
		values, ok = meshLinearClamp(values, tint.input[2*c], tint.input[2*c+1])
		if !ok {
			return result, &UnsupportedError{Feature: "clipped mesh tint domain"}
		}
		for channel, expression := range tint.expressions {
			for i, value := range values {
				result[i][channel] += expression[c+1] * value
			}
		}
	}
	for c := range tint.expressions {
		values := [3]float64{result[0][c], result[1][c], result[2][c]}
		for _, value := range values {
			if math.IsNaN(value) || math.IsInf(value, 0) {
				return [3][4]float64{}, fmt.Errorf("nonfinite mesh vertex color")
			}
		}
		values, ok := meshLinearClamp(values, tint.output[2*c], tint.output[2*c+1])
		if !ok {
			return result, &UnsupportedError{Feature: "clipped mesh tint range"}
		}
		values, ok = meshLinearClamp(values, 0, 1)
		if !ok {
			return result, &UnsupportedError{Feature: "clipped mesh tint color"}
		}
		for i, value := range values {
			result[i][c] = value
		}
	}
	return result, nil
}

// meshLinearClamp 裁切三个顶点的分量，仅在整个三角形内保持线性时返回成功
// 入参: values 顶点分量, low 下界, high 上界
// 返回: [3]float64 裁切后的分量, bool 是否保持线性
func meshLinearClamp(values [3]float64, low, high float64) ([3]float64, bool) {
	lower, upper := min(values[0], values[1], values[2]), max(values[0], values[1], values[2])
	if low != high && (lower < low && upper > low || lower < high && upper > high) {
		return values, false
	}
	for i, value := range values {
		values[i] = math.Max(low, math.Min(high, value))
	}
	return values, true
}

// clipMeshLinear 沿函数输入等值线裁切凸多边形，保持顶点顺序
// 入参: dst 输出缓冲, source 原多边形, boundary 裁切值, lower 是否保留下界一侧
// 返回: []meshLinearVertex 裁切后的顶点
func clipMeshLinear(dst, source []meshLinearVertex, boundary float64, lower bool) []meshLinearVertex {
	if len(source) == 0 {
		return dst
	}
	a := source[len(source)-1]
	inside := func(value float64) bool {
		return lower && value >= boundary || !lower && value <= boundary
	}
	appendVertex := func(vertex meshLinearVertex) {
		if len(dst) == 0 || dst[len(dst)-1] != vertex {
			dst = append(dst, vertex)
		}
	}
	for _, b := range source {
		if inside(a.value) != inside(b.value) {
			left, right := a, b
			if left.value > right.value {
				left, right = right, left
			}
			t := functionPosition(boundary, left.value, right.value)
			point := Point{functionValue(t, left.point.X, right.point.X), functionValue(t, left.point.Y, right.point.Y)}
			appendVertex(meshLinearVertex{point, boundary})
		}
		if inside(b.value) {
			appendVertex(b)
		}
		a = b
	}
	if len(dst) > 1 && dst[0] == dst[len(dst)-1] {
		dst = dst[:len(dst)-1]
	}
	return dst
}
