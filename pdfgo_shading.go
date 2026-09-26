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
	"slices"
)

// GradientStop 保存渐变位置、源空间分量及对应sRGB颜色
// RGB仅表示断点颜色，区间颜色应通过渐变的ColorAt方法计算
type GradientStop struct {
	Position float64
	RGB      [3]float64
	Values   [4]float64
}

// AxialGradient 保存页面坐标中的渐变轴及两端延伸方式
// Stops仅在函数可精确展开为线性分段时提供，否则通过ValuesAt或ColorAt求值
type AxialGradient struct {
	Start, End Point
	Extend     [2]bool
	Stops      []GradientStop
	Space      *ColorSpace
	Intent     Name
	function   *gradientFunction
	domain     [2]float64
}

// RadialGradient 保存页面坐标中的双圆径向渐变及两端延伸方式
// Stops仅在函数可精确展开为线性分段时提供，否则通过ValuesAt或ColorAt求值
type RadialGradient struct {
	Start, End             Point
	StartRadius, EndRadius float64
	Extend                 [2]bool
	Stops                  []GradientStop
	Space                  *ColorSpace
	Intent                 Name
	function               *gradientFunction
	domain                 [2]float64
}

// ColorAt 在源颜色空间内插值，再按渲染意图转换为sRGB
// 入参: position 归一化轴位置，范围外使用端点颜色
// 返回: [3]float64 sRGB颜色, error 无效渐变或颜色变换错误
func (g *AxialGradient) ColorAt(position float64) ([3]float64, error) {
	values, err := g.ValuesAt(position)
	return gradientRGB(values, err, g.Space, g.Intent)
}

// ColorAt 在源颜色空间内插值，再按渲染意图转换为sRGB
// 入参: position 归一化双圆插值位置，范围外使用端点颜色
// 返回: [3]float64 sRGB颜色, error 无效渐变或颜色变换错误
func (g *RadialGradient) ColorAt(position float64) ([3]float64, error) {
	values, err := g.ValuesAt(position)
	return gradientRGB(values, err, g.Space, g.Intent)
}

// ValuesAt 按原始函数计算轴向渐变的源颜色分量
// 入参: position 归一化轴位置，范围外使用端点
// 返回: [4]float64 源空间分量, error 无效求值
func (g *AxialGradient) ValuesAt(position float64) ([4]float64, error) {
	return gradientValues(g.Stops, g.function, g.domain, position)
}

// ValuesAt 按原始函数计算径向渐变的源颜色分量
// 入参: position 归一化双圆插值位置，范围外使用端点
// 返回: [4]float64 源空间分量, error 无效求值
func (g *RadialGradient) ValuesAt(position float64) ([4]float64, error) {
	return gradientValues(g.Stops, g.function, g.domain, position)
}

// gradientValues 优先计算原始函数，保留非线性、定义域和边界语义
// 入参: stops 线性分段, function 原始函数, domain 着色定义域, position 归一化位置
// 返回: [4]float64 源空间分量, error 无效参数或结果
func gradientValues(stops []GradientStop, function *gradientFunction, domain [2]float64, position float64) ([4]float64, error) {
	if math.IsNaN(position) || math.IsInf(position, 0) || function == nil && len(stops) == 0 {
		return [4]float64{}, fmt.Errorf("invalid gradient evaluation")
	}
	if function == nil {
		return gradientValue(stops, position), nil
	}
	position = math.Max(0, math.Min(1, position))
	values := function.value(domain[0] + position*(domain[1]-domain[0]))
	for i, value := range values {
		if math.IsNaN(value) || math.IsInf(value, 0) {
			return [4]float64{}, fmt.Errorf("nonfinite gradient color")
		}
		values[i] = math.Max(0, math.Min(1, value))
	}
	return values, nil
}

// gradientRGB 保留源空间的插值语义，不在显示颜色之间近似
// 入参: values 源分量, err 分量求值错误, space 源空间, intent 渲染意图
// 返回: [3]float64 sRGB颜色, error 参数或变换错误
func gradientRGB(values [4]float64, err error, space *ColorSpace, intent Name) ([3]float64, error) {
	if err != nil {
		return [3]float64{}, err
	}
	if space == nil {
		return [3]float64{}, fmt.Errorf("invalid gradient evaluation")
	}
	return space.RGB(values[:space.Components()], intent)
}

// numberArray 解析指定长度的数值数组
// 入参: object 数组或引用, count 数组长度
// 返回: []float64 数值, error 解析错误
func (r *Reader) numberArray(object Object, count int) ([]float64, error) {
	v, err := r.Resolve(object)
	if err != nil {
		return nil, err
	}
	a, ok := v.(Array)
	if !ok {
		return nil, fmt.Errorf("expected numeric array")
	}
	return numbers(a, count)
}

// shadingPattern 解析着色图案中的轴向或径向渐变
// 入参: name 图案资源名
// 返回: Paint 渐变画刷, error 不支持的图案或解析错误
func (p *pageInterpreter) shadingPattern(name Name) (Paint, error) {
	object, err := p.resource("Pattern", name)
	if err != nil {
		return Paint{}, err
	}
	v, err := p.reader.Resolve(object)
	if err != nil {
		return Paint{}, err
	}
	dict, ok := v.(Dictionary)
	if !ok || dict["PatternType"] != Integer(2) || dict["ExtGState"] != nil {
		return Paint{}, &UnsupportedError{Feature: "shading pattern type or graphics state"}
	}
	m := p.patternMatrix
	if dict["Matrix"] != nil {
		v, err := p.reader.numberArray(dict["Matrix"], 6)
		if err != nil {
			return Paint{}, err
		}
		m = m.Mul(Matrix(v))
	}
	v, err = p.reader.Resolve(dict["Shading"])
	if err != nil {
		return Paint{}, err
	}
	shading, ok := v.(Dictionary)
	if !ok || shading["Background"] != nil || shading["BBox"] != nil {
		return Paint{}, &UnsupportedError{Feature: "shading dictionary"}
	}
	return p.shadingPaint(shading, m)
}

// shadingFill 按当前裁剪和图形状态直接绘制着色资源，不改变当前路径
// 入参: operands 着色资源名
// 返回: error 解析或绘制错误
func (p *pageInterpreter) shadingFill(operands []Object) error {
	if len(operands) != 1 {
		return fmt.Errorf("invalid shading operands")
	}
	name, ok := operands[0].(Name)
	if !ok {
		return fmt.Errorf("invalid shading name")
	}
	object, err := p.resource("Shading", name)
	if err != nil {
		return err
	}
	value, err := p.reader.Resolve(object)
	if err != nil {
		return err
	}
	dict, ok := value.(Dictionary)
	if !ok {
		return &UnsupportedError{Feature: "shading dictionary"}
	}
	paint, err := p.shadingPaint(dict, p.state.matrix)
	if err != nil {
		return err
	}
	style := p.state.style
	paint.Alpha = style.Fill.Alpha
	style.Fill = paint
	if dict["BBox"] != nil {
		box, err := p.reader.rectangle(dict["BBox"])
		if err != nil {
			return err
		}
		clip := shadingRectangle(box, p.state.matrix)
		style.Clips = append(append([]Path(nil), style.Clips...), clip)
	}
	if p.visitor.Path == nil {
		return fmt.Errorf("path visitor missing")
	}
	if p.bounds.XMax <= p.bounds.XMin || p.bounds.YMax <= p.bounds.YMin {
		return fmt.Errorf("missing shading bounds")
	}
	return p.visitor.Path(PathMark{Path: shadingRectangle(p.bounds, Identity()), Style: style, Fill: true})
}

// shadingRectangle 将着色边界转换为页面坐标的闭合路径
// 入参: box 矩形边界, matrix 坐标变换
// 返回: Path 矩形路径
func shadingRectangle(box Rectangle, matrix Matrix) Path {
	return Path{Segments: []Segment{
		{"M", []Point{matrix.Apply(Point{box.XMin, box.YMin})}},
		{"L", []Point{matrix.Apply(Point{box.XMax, box.YMin})}},
		{"L", []Point{matrix.Apply(Point{box.XMax, box.YMax})}},
		{"L", []Point{matrix.Apply(Point{box.XMin, box.YMax})}},
		{"C", nil},
	}}
}

// shadingPaint 解析轴向或径向着色的坐标及颜色函数
// 入参: shading 着色字典, m 着色坐标到页面的变换
// 返回: Paint 渐变画刷, error 解析错误
func (p *pageInterpreter) shadingPaint(shading Dictionary, m Matrix) (Paint, error) {
	sx, sy := math.Hypot(m[0], m[1]), math.Hypot(m[2], m[3])
	if shading["ShadingType"] == Integer(3) && (sx == 0 || math.Abs(sx-sy) > 1e-8*math.Max(1, sx) || math.Abs(m[0]*m[2]+m[1]*m[3]) > 1e-8*math.Max(1, sx*sy)) {
		return Paint{}, &UnsupportedError{Feature: "anisotropic shading pattern"}
	}
	domain := [2]float64{0, 1}
	if shading["Domain"] != nil {
		d, err := p.reader.numberArray(shading["Domain"], 2)
		if err != nil {
			return Paint{}, err
		}
		if d[0] >= d[1] {
			return Paint{}, fmt.Errorf("invalid shading domain")
		}
		domain = [2]float64(d)
	}
	count := 4
	if shading["ShadingType"] == Integer(3) {
		count = 6
	} else if shading["ShadingType"] != Integer(2) {
		return Paint{}, &UnsupportedError{Feature: "shading type"}
	}
	coords, err := p.reader.numberArray(shading["Coords"], count)
	if err != nil {
		return Paint{}, err
	}
	start := Point{coords[0], coords[1]}
	if count == 6 {
		if coords[2] < 0 || coords[5] <= 0 {
			return Paint{}, &UnsupportedError{Feature: "radial shading radius"}
		}
	}
	paint := Paint{}
	if count == 4 {
		end := Point{coords[2], coords[3]}
		if start == end {
			return Paint{}, fmt.Errorf("degenerate shading axis")
		}
		inverse, ok := m.Inverse()
		if !ok {
			return Paint{}, fmt.Errorf("singular shading transform")
		}
		dx, dy := end.X-start.X, end.Y-start.Y
		gx, gy := inverse[0]*dx+inverse[1]*dy, inverse[2]*dx+inverse[3]*dy
		factor := (dx*dx + dy*dy) / (gx*gx + gy*gy)
		start = m.Apply(start)
		paint.Axial = &AxialGradient{Start: start, End: Point{start.X + gx*factor, start.Y + gy*factor}}
	} else {
		end := Point{coords[3], coords[4]}
		paint.Radial = &RadialGradient{Start: m.Apply(start), End: m.Apply(end), StartRadius: coords[2] * sx, EndRadius: coords[5] * sx}
	}
	if shading["Extend"] != nil {
		v, err := p.reader.Resolve(shading["Extend"])
		if err != nil {
			return Paint{}, err
		}
		a, ok := v.(Array)
		if !ok || len(a) != 2 {
			return Paint{}, fmt.Errorf("invalid shading extension")
		}
		for i, v := range a {
			b, ok := v.(Boolean)
			if !ok {
				return Paint{}, fmt.Errorf("invalid shading extension")
			}
			if paint.Axial != nil {
				paint.Axial.Extend[i] = bool(b)
			} else {
				paint.Radial.Extend[i] = bool(b)
			}
		}
	}
	stops, space, function, err := p.shadingStops(shading["ColorSpace"], shading["Function"], domain)
	if err != nil {
		return Paint{}, err
	}
	for i := range stops {
		stop := &stops[i]
		stop.RGB, err = space.RGB(stop.Values[:space.Components()], p.state.style.RenderingIntent)
		if err != nil {
			return Paint{}, err
		}
	}
	if paint.Axial != nil {
		paint.Axial.Stops, paint.Axial.Space = stops, space
		paint.Axial.Intent = p.state.style.RenderingIntent
		paint.Axial.function, paint.Axial.domain = function, domain
	} else {
		paint.Radial.Stops, paint.Radial.Space = stops, space
		paint.Radial.Intent = p.state.style.RenderingIntent
		paint.Radial.function, paint.Radial.domain = function, domain
	}
	return paint, nil
}

// shadingStops 解析渐变源空间，DeviceN按仿射着色函数精确映射到备用空间
// 入参: object 颜色空间, function 渐变函数, domain 输入区间
// 返回: []GradientStop 分段, *ColorSpace 插值空间, *gradientFunction 原始函数, error 解析错误
func (p *pageInterpreter) shadingStops(object, function Object, domain [2]float64) ([]GradientStop, *ColorSpace, *gradientFunction, error) {
	object, err := p.reader.Resolve(object)
	if err != nil {
		return nil, nil, nil, err
	}
	if name, ok := object.(Name); ok && name != "DeviceGray" && name != "DeviceRGB" && name != "DeviceCMYK" {
		object, err = p.resource("ColorSpace", name)
		if err != nil {
			return nil, nil, nil, err
		}
		object, err = p.reader.Resolve(object)
		if err != nil {
			return nil, nil, nil, err
		}
	}
	array, ok := object.(Array)
	if ok && len(array) > 0 && array[0] == Name("DeviceN") {
		return p.reader.deviceNGradient(array, function, domain)
	}
	space, err := p.reader.readColorSpace(object)
	if err != nil {
		return nil, nil, nil, err
	}
	f, err := p.reader.readGradientFunction(function, space.Components(), 0)
	if err != nil {
		return nil, nil, nil, err
	}
	var stops []GradientStop
	if f.linear != nil {
		stops = clipGradientValues(f.linear(domain), gradientUnitBounds(space.Components()))
	}
	return stops, space, f, nil
}

// deviceNGradient 精确展开多色渐变的仿射着色与范围截断
// 入参: space 多色定义, function 渐变函数, domain 输入区间
// 返回: []GradientStop 备用空间分段, *ColorSpace 备用空间, *gradientFunction 颜色函数, error 能力或格式错误
func (r *Reader) deviceNGradient(space Array, function Object, domain [2]float64) ([]GradientStop, *ColorSpace, *gradientFunction, error) {
	if len(space) != 4 && len(space) != 5 {
		return nil, nil, nil, fmt.Errorf("invalid DeviceN color space")
	}
	value, err := r.Resolve(space[1])
	if err != nil {
		return nil, nil, nil, err
	}
	names, ok := value.(Array)
	if !ok || len(names) == 0 {
		return nil, nil, nil, fmt.Errorf("invalid DeviceN colorants")
	}
	if len(names) > 4 {
		return nil, nil, nil, &UnsupportedError{Feature: "DeviceN gradient component count"}
	}
	seen := map[Name]bool{}
	for _, value := range names {
		name, ok := value.(Name)
		if !ok || seen[name] || name == "All" {
			return nil, nil, nil, fmt.Errorf("invalid DeviceN colorant")
		}
		if name == "None" {
			return nil, nil, nil, &UnsupportedError{Feature: "DeviceN gradient None colorant"}
		}
		seen[name] = true
	}
	alternate, err := r.readColorSpace(space[2])
	if err != nil {
		return nil, nil, nil, err
	}
	value, err = r.Resolve(space[3])
	if err != nil {
		return nil, nil, nil, err
	}
	stream, ok := value.(*Stream)
	if !ok || stream.Dictionary["FunctionType"] != Integer(4) {
		return nil, nil, nil, &UnsupportedError{Feature: "DeviceN gradient tint function"}
	}
	input, err := r.numberArray(stream.Dictionary["Domain"], 2*len(names))
	if err != nil {
		return nil, nil, nil, err
	}
	output, err := r.numberArray(stream.Dictionary["Range"], 2*alternate.Components())
	if err != nil {
		return nil, nil, nil, err
	}
	for index, bounds := range [][]float64{input, output} {
		for i := 0; i < len(bounds); i += 2 {
			if bounds[i] > bounds[i+1] || index == 0 && bounds[i] == bounds[i+1] {
				return nil, nil, nil, fmt.Errorf("invalid DeviceN tint bounds")
			}
		}
	}
	data, err := stream.Decode()
	if err != nil {
		return nil, nil, nil, err
	}
	expressions, err := affineCalculator(data, len(names), alternate.Components())
	if err != nil {
		return nil, nil, nil, err
	}
	source, err := r.readGradientFunction(function, len(names), 0)
	if err != nil {
		return nil, nil, nil, err
	}
	mapped := deviceNGradientFunction(source, input, output, expressions)
	var stops []GradientStop
	if mapped.linear != nil {
		stops = clipGradientValues(mapped.linear(domain), gradientUnitBounds(alternate.Components()))
	}
	return stops, alternate, mapped, nil
}

// clipGradientValues 在截断位置增加精确断点，不改变其余分段或跳变
// 入参: stops 源分段, bounds 各分量下界与上界
// 返回: []GradientStop 截断后的分段
func clipGradientValues(stops []GradientStop, bounds []float64) []GradientStop {
	var result []GradientStop
	for i, stop := range stops {
		if i > 0 && stops[i-1].Position < stop.Position {
			a := stops[i-1]
			var positions []float64
			for c := 0; c < len(bounds)/2; c++ {
				if a.Values[c] == stop.Values[c] {
					continue
				}
				for _, bound := range bounds[c*2 : c*2+2] {
					t := (bound - a.Values[c]) / (stop.Values[c] - a.Values[c])
					if t > 0 && t < 1 {
						positions = append(positions, t)
					}
				}
			}
			slices.Sort(positions)
			for _, t := range slices.Compact(positions) {
				v := GradientStop{Position: a.Position + t*(stop.Position-a.Position)}
				for c := range v.Values {
					v.Values[c] = a.Values[c] + t*(stop.Values[c]-a.Values[c])
				}
				result = append(result, v)
			}
		}
		result = append(result, stop)
	}
	for i := range result {
		for c := 0; c < len(bounds)/2; c++ {
			result[i].Values[c] = math.Max(bounds[2*c], math.Min(bounds[2*c+1], result[i].Values[c]))
		}
	}
	return slices.Compact(result)
}

// linearGradientStops 返回可精确展开的函数分段，不近似非线性函数
// 入参: object 函数对象, interval 输入区间, channels 分量数, depth 嵌套深度
// 返回: []GradientStop 精确分段, error 解析或非线性错误
func (r *Reader) linearGradientStops(object Object, interval [2]float64, channels, depth int) ([]GradientStop, error) {
	f, err := r.readGradientFunction(object, channels, depth)
	if err != nil {
		return nil, err
	}
	if f.linear == nil {
		return nil, &UnsupportedError{Feature: "nonlinear gradient function"}
	}
	return clipGradientValues(f.linear(interval), gradientUnitBounds(channels)), nil
}

// gradientUnitBounds 返回设备或ICC分量的单位区间
// 入参: channels 分量数
// 返回: []float64 分量上下界
func gradientUnitBounds(channels int) []float64 {
	bounds := make([]float64, channels*2)
	for i := 0; i < channels; i++ {
		bounds[i*2+1] = 1
	}
	return bounds
}

// gradientInterval 截取或反向映射线性分段，保留定义域外常量和不连续边界
// 入参: stops 原分段, start 起始参数, end 终止参数
// 返回: []GradientStop 归一化分段
func gradientInterval(stops []GradientStop, start, end float64) []GradientStop {
	if start > end {
		result := gradientInterval(stops, end, start)
		slices.Reverse(result)
		for i := range result {
			result[i].Position = 1 - result[i].Position
		}
		return result
	}
	result := []GradientStop{{Position: 0, Values: gradientValue(stops, start)}}
	if start != end {
		for _, stop := range stops {
			if stop.Position > start && stop.Position <= end {
				stop.Position = (stop.Position - start) / (end - start)
				result = append(result, stop)
			}
		}
	}
	return slices.Compact(append(result, GradientStop{Position: 1, Values: gradientValue(stops, end)}))
}

// gradientValue 计算线性分段在给定位置的颜色，重复位置使用右侧分段
// 入参: stops 原分段, position 归一化位置
// 返回: [4]float64 颜色分量
func gradientValue(stops []GradientStop, position float64) [4]float64 {
	return gradientValueSide(stops, position, false)
}

// gradientValueSide 按指定侧取得重复断点处的颜色
// 入参: stops 分段, position 位置, left 是否取左极限
// 返回: [4]float64 颜色分量
func gradientValueSide(stops []GradientStop, position float64, left bool) [4]float64 {
	if position < stops[0].Position {
		return stops[0].Values
	}
	for i := 1; i < len(stops); i++ {
		if position < stops[i].Position || left && position == stops[i].Position && stops[i-1].Position < position {
			a, b := stops[i-1], stops[i]
			t := (position - a.Position) / (b.Position - a.Position)
			var value [4]float64
			for c := range value {
				value[c] = a.Values[c] + t*(b.Values[c]-a.Values[c])
			}
			return value
		}
	}
	return stops[len(stops)-1].Values
}
