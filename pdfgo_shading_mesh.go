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
)

// MeshPatch 保存双三次曲面控制点及四角分量，Points按u、v索引
// Colors依次对应(0,0)、(0,1)、(1,1)、(1,0)，有着色函数时仅首分量为函数输入
type MeshPatch struct {
	Points [4][4]Point
	Colors [4][]float64
}

// MeshTriangle 保存Gouraud三角形顶点及对应颜色分量
type MeshTriangle struct {
	Points [3]Point
	Colors [3][]float64
}

// MeshGradient 保存曲面或三角网格，各切片保持绘制顺序，Matrix将网格坐标映射到页面
// Background和Bounds仅用于着色图案，直接着色的边界由裁剪路径表达
type MeshGradient struct {
	Patches    []MeshPatch
	Triangles  []MeshTriangle
	Matrix     Matrix
	Space      *ColorSpace
	Intent     Name
	Background *[4]float64
	Bounds     *Rectangle
	function   *gradientVector
	tint       *deviceNSpace
	lab        *labSpace
}

// meshBits 按高位优先读取曲面网格的紧凑数值
type meshBits struct {
	data []byte
	pos  int
}

// UsesFunction 判断颜色分量是否需在插值后经过函数变换
// 返回: bool 是否使用颜色函数
func (g *MeshGradient) UsesFunction() bool {
	return g.function != nil || g.tint != nil || g.lab != nil || g.Space != nil && g.Space.profile != nil && (g.Space.profile.ranges != nil || g.Space.profile.alternate != nil)
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

// ValuesAt 插值源分量后依次执行着色函数和专色变换，三角形编号接在曲面之后
// 入参: patch 网格序号, u 横向参数或第二顶点权重, v 纵向参数或第三顶点权重
// 返回: [4]float64 Space颜色空间分量, error 参数或颜色错误
func (g *MeshGradient) ValuesAt(patch int, u, v float64) ([4]float64, error) {
	weights, colors, err := g.sourceColorsAt(patch, u, v)
	if err != nil {
		return [4]float64{}, err
	}
	components := len(colors[0])
	if g.function != nil {
		x, err := meshFunctionInput(weights, colors)
		if err != nil {
			return [4]float64{}, err
		}
		if g.function.scalar != nil {
			values, err := g.function.scalar.evaluate(x)
			if err != nil {
				return values, err
			}
			return g.colorValues(values[:len(g.function.parts)])
		}
		return g.functionValues(x)
	}
	var buffer [32]float64
	input := buffer[:min(components, len(buffer))]
	if components > len(buffer) {
		input = make([]float64, components)
	}
	meshInterpolate(weights, colors, input)
	return g.colorValues(input)
}

// functionValues 求值高通道网格函数，普通分量使用独立无分配路径
// 入参: x 插值后的函数输入
// 返回: [4]float64 备用空间分量, error 函数或颜色错误
func (g *MeshGradient) functionValues(x float64) ([4]float64, error) {
	components := len(g.function.parts)
	var buffer [32]float64
	input := buffer[:min(components, len(buffer))]
	if components > len(buffer) {
		input = make([]float64, components)
	}
	if err := g.function.calculate(x, input); err != nil {
		return [4]float64{}, err
	}
	return g.colorValues(input)
}

// colorValues 将插值后的源分量转换为备用空间，保持专色和Lab变换顺序
// 入参: input 源空间颜色分量
// 返回: [4]float64 备用空间分量, error 颜色错误
func (g *MeshGradient) colorValues(input []float64) ([4]float64, error) {
	return shadingColorValues(g.Space, g.tint, g.lab, input)
}

// meshBernstein 计算三次伯恩斯坦基函数
// 入参: t 参数
// 返回: [4]float64 基函数值
func meshBernstein(t float64) [4]float64 {
	s := 1 - t
	return [4]float64{s * s * s, 3 * t * s * s, 3 * t * t * s, t * t * t}
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

// meshPaint 解码三角形、Coons及张量积网格，保留源颜色分量和共享边
// 入参: stream 着色流, matrix 网格到页面的变换
// 返回: Paint 曲面画刷, error 格式或能力错误
func (p *pageInterpreter) meshPaint(stream *Stream, matrix Matrix) (Paint, error) {
	d := stream.Dictionary
	kind, ok := d["ShadingType"].(Integer)
	if !ok || kind < 4 || kind > 7 {
		return Paint{}, &UnsupportedError{Feature: "mesh shading type"}
	}
	depths := [3]int{}
	object, err := p.shadingColorSpace(d["ColorSpace"])
	if err != nil {
		return Paint{}, err
	}
	visible, err := p.colorSpaceVisible(object)
	if err != nil {
		return Paint{}, err
	}
	if !visible {
		return Paint{None: true}, nil
	}
	for i, key := range []Name{"BitsPerCoordinate", "BitsPerComponent", "BitsPerFlag"} {
		if kind == 5 && i == 2 {
			continue
		}
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
	g := &MeshGradient{Intent: p.state.style.RenderingIntent, Matrix: matrix}
	var components int
	g.Space, g.tint, g.lab, components, err = p.reader.readShadingColorSpace(object, true)
	if err != nil {
		return Paint{}, err
	}
	if d["Function"] != nil {
		g.function, err = p.reader.readGradientVector(d["Function"], components, 0)
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
	if kind == 4 || kind == 5 {
		g.Triangles, err = p.meshTriangleData(d, data, depths, ranges, components, kind == 5)
		if err != nil {
			return Paint{}, err
		}
		paint := Paint{Mesh: g}
		if g.tint != nil {
			paint.Process = g.tint.process
		}
		return paint, nil
	}
	bits := meshBits{data: data}
	pointCount := 12
	if kind == 7 {
		pointCount = 16
	}
	var previous [16]Point
	var colors [4][]float64
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
			colors[i] = make([]float64, components)
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
	paint := Paint{Mesh: g}
	if g.tint != nil {
		paint.Process = g.tint.process
	}
	return paint, nil
}

// meshTriangleData 按边标志或格点行顺序解码三角网格，保留绘制顺序
// 入参: dict 着色字典, data 解码数据, depths 数值位宽, ranges 解码区间, components 分量数, lattice 是否为格点网格
// 返回: []MeshTriangle 三角形, error 结构或取消错误
func (p *pageInterpreter) meshTriangleData(dict Dictionary, data []byte, depths [3]int, ranges []float64, components int, lattice bool) ([]MeshTriangle, error) {
	type vertex struct {
		point Point
		color []float64
	}
	stride := (depths[2] + depths[0]*2 + depths[1]*components + 7) / 8
	if len(data)%stride != 0 {
		return nil, io.ErrUnexpectedEOF
	}
	count := len(data) / stride
	bits := meshBits{data: data}
	read := func() (vertex, uint32, error) {
		v := vertex{color: make([]float64, components)}
		flag, err := bits.read(depths[2])
		if err != nil {
			return v, 0, err
		}
		for index := 0; index < 2+components; index++ {
			depth := depths[1]
			if index < 2 {
				depth = depths[0]
			}
			n, err := bits.read(depth)
			if err != nil {
				return v, 0, err
			}
			value := ranges[index*2] + float64(n)/float64(uint64(1)<<uint(depth)-1)*(ranges[index*2+1]-ranges[index*2])
			switch index {
			case 0:
				v.point.X = value
			case 1:
				v.point.Y = value
			default:
				v.color[index-2] = value
			}
		}
		bits.pos = (bits.pos + 7) / 8 * 8
		return v, flag & 3, nil
	}
	var result []MeshTriangle
	appendTriangle := func(a, b, c vertex) {
		result = append(result, MeshTriangle{Points: [3]Point{a.point, b.point, c.point}, Colors: [3][]float64{a.color, b.color, c.color}})
	}
	if lattice {
		value, err := p.reader.Resolve(dict["VerticesPerRow"])
		if err != nil {
			return nil, err
		}
		columns, ok := value.(Integer)
		if !ok || columns < 2 || columns > Integer(count/2) || Integer(count)%columns != 0 {
			return nil, fmt.Errorf("invalid mesh lattice dimensions")
		}
		row := make([]vertex, int(columns))
		var aboveLeft, left vertex
		for index := 0; index < count; index++ {
			if err := p.ctx.Err(); err != nil {
				return nil, err
			}
			v, _, err := read()
			if err != nil {
				return nil, err
			}
			column := index % len(row)
			above := row[column]
			if index >= len(row) && column != 0 {
				appendTriangle(aboveLeft, above, left)
				appendTriangle(above, left, v)
			}
			row[column], aboveLeft, left = v, above, v
		}
	} else {
		var previous [3]vertex
		for bits.pos < len(data)*8 {
			if err := p.ctx.Err(); err != nil {
				return nil, err
			}
			v, flag, err := read()
			if err != nil {
				return nil, err
			}
			switch flag {
			case 0:
				previous[0] = v
				for i := 1; i < 3; i++ {
					previous[i], _, err = read()
					if err != nil {
						return nil, err
					}
				}
			case 1, 2:
				if len(result) == 0 {
					return nil, fmt.Errorf("mesh shared edge without previous triangle")
				}
				if flag == 1 {
					previous[0] = previous[1]
				}
				previous[1], previous[2] = previous[2], v
			default:
				return nil, fmt.Errorf("invalid mesh triangle edge flag")
			}
			appendTriangle(previous[0], previous[1], previous[2])
		}
	}
	if len(result) == 0 {
		return nil, fmt.Errorf("empty mesh shading")
	}
	return result, nil
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
