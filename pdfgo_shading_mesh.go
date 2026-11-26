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
	pos  int64
}

// meshBernstein 计算三次伯恩斯坦基函数
// 入参: t 参数
// 返回: [4]float64 基函数值
func meshBernstein(t float64) [4]float64 {
	s := 1 - t
	return [4]float64{s * s * s, 3 * t * s * s, 3 * t * t * s, t * t * t}
}

// UsesFunction 判断颜色分量是否需在插值后经过函数变换
// 返回: bool 是否使用颜色函数
func (g *MeshGradient) UsesFunction() bool {
	return g != nil && (g.function != nil || g.tint != nil || g.lab != nil || g.Space != nil && g.Space.profile != nil && (g.Space.profile.ranges != nil || g.Space.profile.alternate != nil))
}

// LinearColors 返回可直接用于线性插值的三角形顶点颜色，不改变源网格
// 入参: index 三角形索引，不计曲面数量
// 返回: [3][4]float64 Space颜色空间分量, error 参数错误或无法确认等价转换的UnsupportedError
func (g *MeshGradient) LinearColors(index int) ([3][4]float64, error) {
	var result [3][4]float64
	if g == nil || index < 0 || index >= len(g.Triangles) || g.Space == nil {
		return result, fmt.Errorf("invalid mesh triangle")
	}
	if g.lab != nil || g.Space.profile != nil && (g.Space.profile.ranges != nil || g.Space.profile.alternate != nil) {
		return result, &UnsupportedError{Feature: "nonlinear mesh color transform"}
	}
	components := g.Space.Components()
	if components < 1 || components > 4 {
		return result, fmt.Errorf("invalid mesh color component count")
	}
	inputs := components
	if g.tint != nil {
		inputs = g.tint.components
	}
	if g.function != nil {
		inputs = 1
	}
	triangle := g.Triangles[index]
	for i, values := range triangle.Colors {
		if len(values) != inputs {
			return result, fmt.Errorf("invalid mesh vertex color")
		}
		for c, value := range values {
			if math.IsNaN(value) || math.IsInf(value, 0) {
				return result, fmt.Errorf("nonfinite mesh vertex color")
			}
			if g.tint == nil {
				result[i][c] = math.Max(0, math.Min(1, value))
			}
		}
	}
	if g.function == nil && g.tint != nil && g.tint.components != 1 {
		return g.linearTintColors(triangle)
	}
	if g.function != nil || g.tint != nil {
		lower := min(triangle.Colors[0][0], triangle.Colors[1][0], triangle.Colors[2][0])
		upper := max(triangle.Colors[0][0], triangle.Colors[1][0], triangle.Colors[2][0])
		if lower != upper {
			function := g.linearFunction()
			if function == nil || function.linear == nil {
				return result, &UnsupportedError{Feature: "nonlinear mesh function"}
			}
			stops := clipGradientValues(function.linear([2]float64{lower, upper}), gradientUnitBounds(components))
			if len(stops) != 2 || stops[0].Position != 0 || stops[1].Position != 1 {
				return result, &UnsupportedError{Feature: "piecewise mesh function"}
			}
		}
	} else {
		for c := range components {
			lower := min(triangle.Colors[0][c], triangle.Colors[1][c], triangle.Colors[2][c])
			upper := max(triangle.Colors[0][c], triangle.Colors[1][c], triangle.Colors[2][c])
			if lower < 0 && upper > 0 || lower < 1 && upper > 1 {
				return result, &UnsupportedError{Feature: "clipped mesh color"}
			}
		}
		return result, nil
	}
	for i, values := range triangle.Colors {
		var err error
		if g.function != nil {
			result[i], err = g.functionValues(values[0])
		} else {
			result[i], err = g.colorValues(values)
		}
		if err != nil {
			return [3][4]float64{}, err
		}
	}
	return result, nil
}

// PointAt 计算单位参数域内的双三次曲面坐标
// 入参: u 横向参数, v 纵向参数
// 返回: Point 网格坐标
func (p MeshPatch) PointAt(u, v float64) Point {
	a, b := meshBernstein(u), meshBernstein(v)
	scaled := false
	for i := range p.Points {
		for j := range p.Points[i] {
			if math.Abs(p.Points[i][j].X) > math.MaxFloat64/16 || math.Abs(p.Points[i][j].Y) > math.MaxFloat64/16 {
				scaled = true
				break
			}
		}
		if scaled {
			break
		}
	}
	var point Point
	if scaled {
		for i := range a {
			for j := range b {
				w := a[i] * b[j]
				point.X += p.Points[i][j].X * 0.0625 * w
				point.Y += p.Points[i][j].Y * 0.0625 * w
			}
		}
		return Point{X: point.X * 16, Y: point.Y * 16}
	}
	for i := range a {
		for j := range b {
			w := a[i] * b[j]
			point.X += p.Points[i][j].X * w
			point.Y += p.Points[i][j].Y * w
		}
	}
	return point
}

// ValuesAt 插值源分量后依次执行着色函数和专色变换，三角形编号接在曲面之后
// 入参: patch 网格序号, u 横向参数或第二顶点权重, v 纵向参数或第三顶点权重
// 返回: [4]float64 Space颜色空间分量, error 参数或颜色错误
func (g *MeshGradient) ValuesAt(patch int, u, v float64) ([4]float64, error) {
	if g == nil {
		return [4]float64{}, fmt.Errorf("invalid mesh gradient")
	}
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
	if g.function.scalar != nil {
		values, err := g.function.scalar.evaluate(x)
		if err != nil {
			return values, err
		}
		return g.colorValues(values[:len(g.function.parts)])
	}
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

// read 读取指定位数的无符号整数
// 入参: count 位数
// 返回: uint32 数值, error 截断错误
func (b *meshBits) read(count int) (uint32, error) {
	if int64(count) > int64(len(b.data))*8-b.pos {
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
	kindValue, err := p.reader.Resolve(d["ShadingType"])
	if err != nil {
		return Paint{}, err
	}
	kind, ok := kindValue.(Integer)
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
	g.Space, g.tint, g.lab, components, err = p.reader.readShadingColorSpaceContext(p.ctx, object, true)
	if err != nil {
		return Paint{}, err
	}
	fnValue, err := p.reader.Resolve(d["Function"])
	if err != nil {
		return Paint{}, err
	}
	if fnValue != nil {
		g.function, err = p.reader.readGradientVectorContext(p.ctx, d["Function"], components, 0)
		if err != nil {
			return Paint{}, err
		}
		components = 1
	}
	colorBounds := g.decodeColorBounds(components)
	ranges, err := p.reader.numberArray(d["Decode"], 4+components*2)
	if err != nil {
		return Paint{}, err
	}
	data, err := p.reader.DecodeStreamContext(p.ctx, stream)
	if err != nil {
		return Paint{}, err
	}
	maxBytes, _ := effectiveStreamLimits(p.reader, stream)
	var maxTriangles, maxPatches int64
	if maxBytes > 0 {
		maxMeshBudget := int64(math.MaxInt64)
		if maxBytes <= math.MaxInt64/4 {
			maxMeshBudget = maxBytes * 4
		}
		maxTriangles = maxMeshBudget / (144 + int64(components)*24)
		maxPatches = maxMeshBudget / (400 + int64(components)*32)
		if (kind == 4 || kind == 5) && maxTriangles == 0 || (kind == 6 || kind == 7) && maxPatches == 0 {
			return Paint{}, fmt.Errorf("%w: mesh shading element limit exceeded", ErrStreamLimitExceeded)
		}
	}
	if kind == 4 || kind == 5 {
		g.Triangles, err = p.meshTriangleData(d, data, depths, ranges, colorBounds, components, kind == 5, maxTriangles)
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
		return functionValue(float64(value)/float64(uint64(1)<<uint(depth)-1), ranges[index], ranges[index+1]), err
	}
	warnedFlag := false
	for bits.pos < int64(len(data))*8 {
		if err := p.ctx.Err(); err != nil {
			return Paint{}, err
		}
		if maxPatches > 0 && int64(len(g.Patches)) >= maxPatches {
			return Paint{}, fmt.Errorf("%w: mesh shading element limit exceeded", ErrStreamLimitExceeded)
		}
		flag, err := bits.read(depths[2])
		if err != nil {
			return Paint{}, err
		}
		if flag > 3 {
			if !warnedFlag && p.reader.warning != nil {
				warnedFlag = true
				p.reader.warning(Diagnostic{Message: "mesh shading flag high bits ignored"})
			}
			flag &= 3
		}
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
				if colorBounds != nil {
					colors[i][c] = max(colorBounds[2*c], min(colorBounds[2*c+1], colors[i][c]))
				}
			}
		}
		bits.pos = (bits.pos + 7) &^ 7
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
		for i := range patch.Points {
			for j := range patch.Points[i] {
				if !pointFinite(patch.Points[i][j]) {
					return Paint{}, fmt.Errorf("nonfinite mesh patch coordinate")
				}
			}
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
// 入参: dict 着色字典, data 解码数据, depths 数值位宽, ranges 解码区间, colorBounds 顶点颜色范围，函数参数不设此范围, components 分量数, lattice 是否为格点网格, maxTriangles 三角形数量预算，零值不限制
// 返回: []MeshTriangle 三角形, error 结构、预算或取消错误
func (p *pageInterpreter) meshTriangleData(dict Dictionary, data []byte, depths [3]int, ranges, colorBounds []float64, components int, lattice bool, maxTriangles int64) ([]MeshTriangle, error) {
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
	warnedFlag := false
	read := func() (vertex, uint32, error) {
		v := vertex{color: make([]float64, components)}
		flag, err := bits.read(depths[2])
		if err != nil {
			return v, 0, err
		}
		if flag > 3 {
			if !warnedFlag && p.reader.warning != nil {
				warnedFlag = true
				p.reader.warning(Diagnostic{Message: "mesh shading flag high bits ignored"})
			}
			flag &= 3
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
			value := functionValue(float64(n)/float64(uint64(1)<<uint(depth)-1), ranges[index*2], ranges[index*2+1])
			switch index {
			case 0:
				v.point.X = value
			case 1:
				v.point.Y = value
			default:
				if colorBounds != nil {
					c := index - 2
					value = max(colorBounds[2*c], min(colorBounds[2*c+1], value))
				}
				v.color[index-2] = value
			}
		}
		bits.pos = (bits.pos + 7) &^ 7
		return v, flag, nil
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
		rows := int64(count) / int64(columns)
		cols := int64(columns)
		if maxTriangles > 0 && (rows-1 > maxTriangles/2/(cols-1) || (rows-1)*(cols-1)*2 > maxTriangles) {
			return nil, fmt.Errorf("%w: mesh shading element limit exceeded", ErrStreamLimitExceeded)
		}
		if rows-1 > int64(math.MaxInt)/2/(cols-1) {
			return nil, fmt.Errorf("%w: mesh shading element limit exceeded", ErrStreamLimitExceeded)
		}
		result = make([]MeshTriangle, 0, int((rows-1)*(cols-1)*2))
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
		for bits.pos < int64(len(data))*8 {
			if err := p.ctx.Err(); err != nil {
				return nil, err
			}
			if maxTriangles > 0 && int64(len(result)) >= maxTriangles {
				return nil, fmt.Errorf("%w: mesh shading element limit exceeded", ErrStreamLimitExceeded)
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

// decodeColorBounds 返回网格顶点解码后的颜色范围，着色函数的输入参数不按颜色裁切
// 入参: components 源分量数
// 返回: []float64 各分量上下界，函数参数返回nil
func (g *MeshGradient) decodeColorBounds(components int) []float64 {
	if g.function != nil {
		return nil
	}
	bounds := gradientUnitBounds(components)
	if g.tint != nil {
		return bounds
	}
	if g.lab != nil {
		bounds[1] = 100
		copy(bounds[2:], g.lab.rangeAB[:])
	} else if g.Space != nil && g.Space.profile != nil {
		ranges := g.Space.profile.sourceRanges()
		copy(bounds, ranges[:])
	}
	return bounds
}

// coonsBlend 计算双线性边界混合值，大坐标采用二次幂缩放避免可避免的中间溢出
// 入参: ru 沿u方向线性插值, rv 沿v方向线性插值, rb 角点双线性插值
// 返回: float64 混合坐标
func coonsBlend(ru, rv, rb float64) float64 {
	if math.Max(math.Abs(ru), math.Max(math.Abs(rv), math.Abs(rb))) > math.MaxFloat64/4 {
		return (0.25*ru + 0.25*rv - 0.25*rb) * 4
	}
	return ru + rv - rb
}

// completeCoons 按双线性边界混合补齐Coons曲面的内部控制点
func (p *MeshPatch) completeCoons() {
	for i := 1; i < 3; i++ {
		u0, u1 := 2.0/3, 1.0/3
		if i == 2 {
			u0, u1 = 1.0/3, 2.0/3
		}
		for j := 1; j < 3; j++ {
			v0, v1 := 2.0/3, 1.0/3
			if j == 2 {
				v0, v1 = 1.0/3, 2.0/3
			}
			ruX := u0*p.Points[0][j].X + u1*p.Points[3][j].X
			rvX := v0*p.Points[i][0].X + v1*p.Points[i][3].X
			rbX := u0*(v0*p.Points[0][0].X+v1*p.Points[0][3].X) + u1*(v0*p.Points[3][0].X+v1*p.Points[3][3].X)
			ruY := u0*p.Points[0][j].Y + u1*p.Points[3][j].Y
			rvY := v0*p.Points[i][0].Y + v1*p.Points[i][3].Y
			rbY := u0*(v0*p.Points[0][0].Y+v1*p.Points[0][3].Y) + u1*(v0*p.Points[3][0].Y+v1*p.Points[3][3].Y)
			p.Points[i][j] = Point{X: coonsBlend(ruX, rvX, rbX), Y: coonsBlend(ruY, rvY, rbY)}
		}
	}
}
