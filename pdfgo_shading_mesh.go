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
	"io"
	"math"
)

// MeshPatch 保存双三次曲面控制点及四角分量，Points按u、v索引
// Colors依次对应(0,0)、(0,1)、(1,1)、(1,0)，有函数时仅首分量为函数输入
type MeshPatch struct {
	Points [4][4]Point
	Colors [4][4]float64
}

// MeshGradient 保存按绘制顺序排列的曲面网格，Matrix将网格坐标映射到页面
type MeshGradient struct {
	Patches  []MeshPatch
	Matrix   Matrix
	Space    *ColorSpace
	Intent   Name
	function *gradientFunction
}

// PointAt 计算单位参数域内的双三次曲面坐标
// 入参: u 横向参数, v 纵向参数
// 返回: Point 网格坐标
func (p MeshPatch) PointAt(u, v float64) Point {
	a, b := meshBernstein(u), meshBernstein(v)
	var point Point
	for i := range a {
		for j := range b {
			point.X += p.Points[i][j].X * a[i] * b[j]
			point.Y += p.Points[i][j].Y * a[i] * b[j]
		}
	}
	return point
}

// ValuesAt 在参数域中双线性插值，再执行可选颜色函数
// 入参: patch 曲面序号, u 横向参数, v 纵向参数
// 返回: [4]float64 源颜色分量, error 参数或颜色错误
func (g *MeshGradient) ValuesAt(patch int, u, v float64) ([4]float64, error) {
	if patch < 0 || patch >= len(g.Patches) || math.IsNaN(u) || math.IsNaN(v) || u < 0 || u > 1 || v < 0 || v > 1 {
		return [4]float64{}, fmt.Errorf("invalid mesh evaluation")
	}
	weights := [4]float64{(1 - u) * (1 - v), (1 - u) * v, u * v, u * (1 - v)}
	var values [4]float64
	for i, weight := range weights {
		for c, value := range g.Patches[patch].Colors[i] {
			values[c] += value * weight
		}
	}
	if g.function != nil {
		values = g.function.value(values[0])
	}
	for c, value := range values {
		if math.IsNaN(value) || math.IsInf(value, 0) {
			return values, fmt.Errorf("nonfinite mesh color")
		}
		values[c] = math.Max(0, math.Min(1, value))
	}
	return values, nil
}

// meshBernstein 计算三次伯恩斯坦基函数
// 入参: t 参数
// 返回: [4]float64 基函数值
func meshBernstein(t float64) [4]float64 {
	s := 1 - t
	return [4]float64{s * s * s, 3 * t * s * s, 3 * t * t * s, t * t * t}
}

// meshBits 按高位优先读取曲面网格的紧凑数值
type meshBits struct {
	data []byte
	pos  int
}

// read 读取指定位数的无符号整数
// 入参: count 位数
// 返回: uint32 数值, error 截断错误
func (b *meshBits) read(count int) (uint32, error) {
	if count > len(b.data)*8-b.pos {
		return 0, io.ErrUnexpectedEOF
	}
	var value uint32
	for range count {
		value = value<<1 | uint32(b.data[b.pos/8]>>uint(7-b.pos%8)&1)
		b.pos++
	}
	return value, nil
}

// meshPaint 解码Coons及张量积曲面，保留源颜色分量和共享边
// 入参: stream 着色流, matrix 网格到页面的变换
// 返回: Paint 曲面画刷, error 格式或能力错误
func (p *pageInterpreter) meshPaint(stream *Stream, matrix Matrix) (Paint, error) {
	d := stream.Dictionary
	kind, ok := d["ShadingType"].(Integer)
	if !ok || kind != 6 && kind != 7 {
		return Paint{}, &UnsupportedError{Feature: "mesh shading type"}
	}
	if d["Background"] != nil {
		return Paint{}, &UnsupportedError{Feature: "mesh shading background"}
	}
	depths := [3]int{}
	for i, key := range []Name{"BitsPerCoordinate", "BitsPerComponent", "BitsPerFlag"} {
		v, err := p.reader.Resolve(d[key])
		if err != nil {
			return Paint{}, err
		}
		n, ok := v.(Integer)
		valid := ok && (n == 1 || n == 2 || n == 4 || n == 8 || n == 12 || n == 16 || i == 0 && (n == 24 || n == 32))
		if i == 2 {
			valid = ok && (n == 2 || n == 4 || n == 8)
		}
		if !valid {
			return Paint{}, fmt.Errorf("invalid mesh %s", key)
		}
		depths[i] = int(n)
	}
	space, err := p.reader.readColorSpace(d["ColorSpace"])
	if err != nil {
		return Paint{}, err
	}
	g := &MeshGradient{Space: space, Intent: p.state.style.RenderingIntent, Matrix: matrix}
	components := space.Components()
	if d["Function"] != nil {
		g.function, err = p.reader.readGradientFunction(d["Function"], components, 0)
		if err != nil {
			return Paint{}, err
		}
		components = 1
	}
	ranges, err := p.reader.numberArray(d["Decode"], 4+components*2)
	if err != nil {
		return Paint{}, err
	}
	data, err := stream.Decode()
	if err != nil {
		return Paint{}, err
	}
	bits := meshBits{data: data}
	pointCount := 12
	if kind == 7 {
		pointCount = 16
	}
	var previous [16]Point
	var colors [4][4]float64
	decode := func(depth, index int) (float64, error) {
		value, err := bits.read(depth)
		return ranges[index] + float64(value)/float64(uint64(1)<<uint(depth)-1)*(ranges[index+1]-ranges[index]), err
	}
	for bits.pos < len(data)*8 {
		if err := p.ctx.Err(); err != nil {
			return Paint{}, err
		}
		flag, err := bits.read(depths[2])
		if err != nil {
			return Paint{}, err
		}
		flag &= 3
		var points [16]Point
		startPoint, startColor := 0, 0
		if flag != 0 {
			if len(g.Patches) == 0 {
				return Paint{}, fmt.Errorf("mesh shared edge without previous patch")
			}
			for i := 0; i < 4; i++ {
				points[i] = previous[(int(flag)*3+i)%12]
			}
			colors[0], colors[1] = colors[flag], colors[(flag+1)%4]
			startPoint, startColor = 4, 2
		}
		for i := startPoint; i < pointCount; i++ {
			points[i].X, err = decode(depths[0], 0)
			if err != nil {
				return Paint{}, err
			}
			points[i].Y, err = decode(depths[0], 2)
			if err != nil {
				return Paint{}, err
			}
		}
		for i := startColor; i < 4; i++ {
			for c := 0; c < components; c++ {
				colors[i][c], err = decode(depths[1], 4+2*c)
				if err != nil {
					return Paint{}, err
				}
			}
		}
		bits.pos = (bits.pos + 7) / 8 * 8
		patch := MeshPatch{Colors: colors}
		indices := [16][2]int{{0, 0}, {0, 1}, {0, 2}, {0, 3}, {1, 3}, {2, 3}, {3, 3}, {3, 2}, {3, 1}, {3, 0}, {2, 0}, {1, 0}, {1, 1}, {1, 2}, {2, 2}, {2, 1}}
		for i, index := range indices {
			if i < pointCount {
				patch.Points[index[0]][index[1]] = points[i]
			}
		}
		if kind == 6 {
			patch.completeCoons()
		}
		g.Patches = append(g.Patches, patch)
		previous = points
	}
	if len(g.Patches) == 0 {
		return Paint{}, fmt.Errorf("empty mesh shading")
	}
	return Paint{Mesh: g}, nil
}

// completeCoons 按双线性边界混合补齐Coons曲面的内部控制点
func (p *MeshPatch) completeCoons() {
	for i := 1; i < 3; i++ {
		for j := 1; j < 3; j++ {
			u, v := float64(i)/3, float64(j)/3
			weights := [8]float64{1 - v, v, 1 - u, u, -(1 - u) * (1 - v), -(1 - u) * v, -u * v, -u * (1 - v)}
			points := [8]Point{p.Points[i][0], p.Points[i][3], p.Points[0][j], p.Points[3][j], p.Points[0][0], p.Points[0][3], p.Points[3][3], p.Points[3][0]}
			for k, point := range points {
				p.Points[i][j].X += point.X * weights[k]
				p.Points[i][j].Y += point.Y * weights[k]
			}
		}
	}
}
