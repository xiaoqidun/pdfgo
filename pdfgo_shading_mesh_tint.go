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
	"fmt"
	"math"
)

// linearTintTriangles 沿多专色仿射变换的输入与输出截断边界拆分三角形
// 入参: ctx 取消上下文, triangle 已校验顶点及源分量的三角形
// 返回: []MeshTriangle 使用备用空间分量的独立三角形, error 颜色、几何或取消错误
func (g *MeshGradient) linearTintTriangles(ctx context.Context, triangle MeshTriangle) ([]MeshTriangle, error) {
	tint := g.tint
	polygons := [][]Point{{{0, 0}, {1, 0}, {0, 1}}}
	input := func(point Point, channel int) float64 {
		return meshTriangleValue(point, triangle.Colors[0][channel], triangle.Colors[1][channel], triangle.Colors[2][channel])
	}
	clipped := func(point Point, channel int) float64 {
		value := math.Max(0, math.Min(1, input(point, channel)))
		return math.Max(tint.input[2*channel], math.Min(tint.input[2*channel+1], value))
	}
	for channel := range tint.components {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		used := false
		for _, expression := range tint.expressions {
			used = used || expression[channel+1] != 0
		}
		if !used {
			continue
		}
		low, high := tint.input[2*channel], tint.input[2*channel+1]
		low, high = math.Max(low, math.Min(high, 0)), math.Max(low, math.Min(high, 1))
		if low == high {
			continue
		}
		for _, boundary := range [2]float64{low, high} {
			var err error
			polygons, err = splitMeshTint(ctx, polygons, boundary, func(point Point) float64 { return input(point, channel) })
			if err != nil {
				return nil, err
			}
		}
	}
	for channel, expression := range tint.expressions {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		low := math.Max(0, math.Min(1, tint.output[2*channel]))
		high := math.Max(0, math.Min(1, tint.output[2*channel+1]))
		if low == high {
			continue
		}
		for _, boundary := range [2]float64{low, high} {
			var err error
			polygons, err = splitMeshTint(ctx, polygons, boundary, func(point Point) float64 {
				value := expression[0]
				for c, coefficient := range expression[1:] {
					value += coefficient * clipped(point, c)
				}
				return value
			})
			if err != nil {
				return nil, err
			}
		}
	}
	var result []MeshTriangle
	values := make([]float64, tint.components)
	components := g.Space.Components()
	for _, polygon := range polygons {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		points := make([]Point, len(polygon))
		colors := make([][4]float64, len(polygon))
		for i, point := range polygon {
			for c := range values {
				values[c] = input(point, c)
			}
			var err error
			colors[i], err = g.colorValues(values)
			if err != nil {
				return nil, err
			}
			a, b, c := triangle.Points[0], triangle.Points[1], triangle.Points[2]
			points[i] = Point{meshTriangleValue(point, a.X, b.X, c.X), meshTriangleValue(point, a.Y, b.Y, c.Y)}
		}
		for i := 2; i < len(points); i++ {
			a, b, c := points[0], points[i-1], points[i]
			if (b.X-a.X)*(c.Y-a.Y)-(c.X-a.X)*(b.Y-a.Y) == 0 {
				continue
			}
			result = append(result, MeshTriangle{
				Points: [3]Point{a, b, c},
				Colors: [3][]float64{colors[0][:components:components], colors[i-1][:components:components], colors[i][:components:components]},
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

// meshTriangleValue 按重心坐标插值分量，避免有限端点相减溢出
// 入参: point 第二、第三顶点权重, a 第一顶点分量, b 第二顶点分量, c 第三顶点分量
// 返回: float64 插值结果
func meshTriangleValue(point Point, a, b, c float64) float64 {
	if point.Y == 1 {
		return c
	}
	x := math.Max(0, math.Min(1, point.X/(1-point.Y)))
	return functionValue(point.Y, functionValue(x, a, b), c)
}

// splitMeshTint 按分量等值线拆分凸多边形，未跨边界的区域直接复用
// 入参: ctx 取消上下文, polygons 原区域, boundary 截断边界, value 当前区域内的仿射分量
// 返回: [][]Point 拆分后的区域, error 非有限分量或取消错误
func splitMeshTint(ctx context.Context, polygons [][]Point, boundary float64, value func(Point) float64) ([][]Point, error) {
	var result [][]Point
	var source []meshLinearVertex
	for _, polygon := range polygons {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		source = source[:0]
		low, high := math.Inf(1), math.Inf(-1)
		for _, point := range polygon {
			v := value(point)
			if math.IsNaN(v) || math.IsInf(v, 0) {
				return nil, fmt.Errorf("nonfinite mesh vertex color")
			}
			low, high = math.Min(low, v), math.Max(high, v)
			source = append(source, meshLinearVertex{point: point, value: v})
		}
		if low >= boundary || high <= boundary {
			result = append(result, polygon)
			continue
		}
		for _, lower := range [2]bool{false, true} {
			clipped := clipMeshLinear(make([]meshLinearVertex, 0, len(polygon)+1), source, boundary, lower)
			points := make([]Point, len(clipped))
			for i, vertex := range clipped {
				points[i] = vertex.point
			}
			result = append(result, points)
		}
	}
	return result, nil
}
