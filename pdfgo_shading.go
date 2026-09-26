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
type AxialGradient struct {
	Start, End Point
	Extend     [2]bool
	Stops      []GradientStop
	Space      *ColorSpace
	Intent     Name
}

// RadialGradient 保存页面坐标中的双圆径向渐变及两端延伸方式
type RadialGradient struct {
	Start, End             Point
	StartRadius, EndRadius float64
	Extend                 [2]bool
	Stops                  []GradientStop
	Space                  *ColorSpace
	Intent                 Name
}

// ColorAt 在源颜色空间内插值，再按渲染意图转换为sRGB
// 入参: position 归一化轴位置，范围外使用端点颜色
// 返回: [3]float64 sRGB颜色, error 无效渐变或颜色变换错误
func (g *AxialGradient) ColorAt(position float64) ([3]float64, error) {
	return gradientRGB(g.Stops, g.Space, g.Intent, position)
}

// ColorAt 在源颜色空间内插值，再按渲染意图转换为sRGB
// 入参: position 归一化双圆插值位置，范围外使用端点颜色
// 返回: [3]float64 sRGB颜色, error 无效渐变或颜色变换错误
func (g *RadialGradient) ColorAt(position float64) ([3]float64, error) {
	return gradientRGB(g.Stops, g.Space, g.Intent, position)
}

// gradientRGB 保留源空间的插值语义，不在显示颜色之间近似
// 入参: stops 颜色分段, space 源空间, intent 渲染意图, position 归一化位置
// 返回: [3]float64 sRGB颜色, error 参数或变换错误
func gradientRGB(stops []GradientStop, space *ColorSpace, intent Name, position float64) ([3]float64, error) {
	if len(stops) == 0 || space == nil || math.IsNaN(position) || math.IsInf(position, 0) {
		return [3]float64{}, fmt.Errorf("invalid gradient evaluation")
	}
	values := gradientValue(stops, position)
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
	stops, space, err := p.shadingStops(shading["ColorSpace"], shading["Function"], domain)
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
	} else {
		paint.Radial.Stops, paint.Radial.Space = stops, space
		paint.Radial.Intent = p.state.style.RenderingIntent
	}
	return paint, nil
}

// shadingStops 解析渐变源空间，DeviceN按仿射着色函数精确映射到备用空间
// 入参: object 颜色空间, function 渐变函数, domain 输入区间
// 返回: []GradientStop 分段, *ColorSpace 插值空间, error 解析错误
func (p *pageInterpreter) shadingStops(object, function Object, domain [2]float64) ([]GradientStop, *ColorSpace, error) {
	object, err := p.reader.Resolve(object)
	if err != nil {
		return nil, nil, err
	}
	if name, ok := object.(Name); ok && name != "DeviceGray" && name != "DeviceRGB" && name != "DeviceCMYK" {
		object, err = p.resource("ColorSpace", name)
		if err != nil {
			return nil, nil, err
		}
		object, err = p.reader.Resolve(object)
		if err != nil {
			return nil, nil, err
		}
	}
	array, ok := object.(Array)
	if ok && len(array) > 0 && array[0] == Name("DeviceN") {
		return p.reader.deviceNGradient(array, function, domain)
	}
	space, err := p.reader.readColorSpace(object)
	if err != nil {
		return nil, nil, err
	}
	stops, err := p.reader.linearGradientStops(function, domain, space.Components(), 0)
	return stops, space, err
}

// deviceNGradient 精确展开多色渐变的仿射着色与范围截断
// 入参: space 多色定义, function 渐变函数, domain 输入区间
// 返回: []GradientStop 备用空间分段, *ColorSpace 备用空间, error 能力或格式错误
func (r *Reader) deviceNGradient(space Array, function Object, domain [2]float64) ([]GradientStop, *ColorSpace, error) {
	if len(space) != 4 && len(space) != 5 {
		return nil, nil, fmt.Errorf("invalid DeviceN color space")
	}
	value, err := r.Resolve(space[1])
	if err != nil {
		return nil, nil, err
	}
	names, ok := value.(Array)
	if !ok || len(names) == 0 {
		return nil, nil, fmt.Errorf("invalid DeviceN colorants")
	}
	if len(names) > 4 {
		return nil, nil, &UnsupportedError{Feature: "DeviceN gradient component count"}
	}
	seen := map[Name]bool{}
	for _, value := range names {
		name, ok := value.(Name)
		if !ok || seen[name] || name == "All" {
			return nil, nil, fmt.Errorf("invalid DeviceN colorant")
		}
		if name == "None" {
			return nil, nil, &UnsupportedError{Feature: "DeviceN gradient None colorant"}
		}
		seen[name] = true
	}
	alternate, err := r.readColorSpace(space[2])
	if err != nil {
		return nil, nil, err
	}
	value, err = r.Resolve(space[3])
	if err != nil {
		return nil, nil, err
	}
	stream, ok := value.(*Stream)
	if !ok || stream.Dictionary["FunctionType"] != Integer(4) {
		return nil, nil, &UnsupportedError{Feature: "DeviceN gradient tint function"}
	}
	input, err := r.numberArray(stream.Dictionary["Domain"], 2*len(names))
	if err != nil {
		return nil, nil, err
	}
	output, err := r.numberArray(stream.Dictionary["Range"], 2*alternate.Components())
	if err != nil {
		return nil, nil, err
	}
	for index, bounds := range [][]float64{input, output} {
		for i := 0; i < len(bounds); i += 2 {
			if bounds[i] > bounds[i+1] || index == 0 && bounds[i] == bounds[i+1] {
				return nil, nil, fmt.Errorf("invalid DeviceN tint bounds")
			}
		}
	}
	data, err := stream.Decode()
	if err != nil {
		return nil, nil, err
	}
	expressions, err := affineCalculator(data, len(names), alternate.Components())
	if err != nil {
		return nil, nil, err
	}
	stops, err := r.linearGradientStops(function, domain, len(names), 0)
	if err != nil {
		return nil, nil, err
	}
	stops = clipGradientValues(stops, input)
	for i := range stops {
		values := stops[i].Values
		stops[i].Values = [4]float64{}
		for c, expression := range expressions {
			v := expression[0]
			for j, coefficient := range expression[1:] {
				v += coefficient * values[j]
			}
			if math.IsNaN(v) || math.IsInf(v, 0) {
				return nil, nil, fmt.Errorf("nonfinite DeviceN tint result")
			}
			stops[i].Values[c] = v
		}
	}
	for i := range output {
		output[i] = math.Max(0, math.Min(1, output[i]))
	}
	return clipGradientValues(stops, output), alternate, nil
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

// linearGradientStops 解析线性指数函数及线性拼接函数
// 入参: object 函数字典或引用, interval 实际输入区间, channels 分量数, depth 当前嵌套深度
// 返回: []GradientStop 渐变分段, error 解析错误
func (r *Reader) linearGradientStops(object Object, interval [2]float64, channels, depth int) ([]GradientStop, error) {
	if depth >= 32 {
		return nil, fmt.Errorf("gradient function recursion limit exceeded")
	}
	v, err := r.Resolve(object)
	if err != nil {
		return nil, err
	}
	if array, ok := v.(Array); ok {
		return r.gradientFunctionArray(array, interval, channels, depth)
	}
	dict, ok := v.(Dictionary)
	if stream, streamOK := v.(*Stream); streamOK {
		dict, ok = stream.Dictionary, true
	}
	if !ok {
		return nil, &UnsupportedError{Feature: "gradient function type"}
	}
	if dict["FunctionType"] == Integer(0) {
		return r.sampledGradientStops(v, interval, channels)
	}
	domain, err := r.numberArray(dict["Domain"], 2)
	if err != nil {
		return nil, err
	}
	if domain[0] >= domain[1] {
		return nil, fmt.Errorf("invalid gradient function domain")
	}
	if dict["Range"] != nil {
		return nil, &UnsupportedError{Feature: "gradient function range"}
	}
	var stops []GradientStop
	switch dict["FunctionType"] {
	case Integer(2):
		n, err := r.number(dict["N"])
		if err != nil {
			return nil, err
		}
		if n != 1 {
			return nil, &UnsupportedError{Feature: "nonlinear gradient function"}
		}
		var colors [2][4]float64
		for i, key := range []Name{"C0", "C1"} {
			color, err := r.numberArray(dict[key], channels)
			if err != nil {
				return nil, err
			}
			for _, c := range color {
				if c < 0 || c > 1 {
					return nil, fmt.Errorf("invalid gradient color")
				}
			}
			copy(colors[i][:], color)
		}
		positions := []float64{domain[0], domain[1]}
		for c, start := range colors[0] {
			change := colors[1][c] - start
			if change == 0 {
				continue
			}
			for _, limit := range []float64{0, 1} {
				x := (limit - start) / change
				if x > domain[0] && x < domain[1] {
					positions = append(positions, x)
				}
			}
		}
		slices.Sort(positions)
		for _, x := range slices.Compact(positions) {
			stop := GradientStop{Position: (x - domain[0]) / (domain[1] - domain[0])}
			for c, start := range colors[0] {
				stop.Values[c] = math.Max(0, math.Min(1, start+x*(colors[1][c]-start)))
			}
			stops = append(stops, stop)
		}
	case Integer(3):
		v, err := r.Resolve(dict["Functions"])
		if err != nil {
			return nil, err
		}
		functions, ok := v.(Array)
		if !ok || len(functions) == 0 {
			return nil, fmt.Errorf("invalid stitching functions")
		}
		bounds, err := r.numberArray(dict["Bounds"], len(functions)-1)
		if err != nil {
			return nil, err
		}
		encode, err := r.numberArray(dict["Encode"], len(functions)*2)
		if err != nil {
			return nil, err
		}
		points := append(append([]float64{domain[0]}, bounds...), domain[1])
		for i, function := range functions {
			if points[i] > points[i+1] {
				return nil, fmt.Errorf("invalid gradient stitching bounds")
			}
			if points[i] == points[i+1] {
				continue
			}
			part, err := r.linearGradientStops(function, [2]float64{encode[i*2], encode[i*2+1]}, channels, depth+1)
			if err != nil {
				return nil, err
			}
			for _, stop := range part {
				stop.Position = (points[i] + stop.Position*(points[i+1]-points[i]) - domain[0]) / (domain[1] - domain[0])
				stops = append(stops, stop)
			}
		}
	default:
		return nil, &UnsupportedError{Feature: "gradient function type"}
	}
	return gradientInterval(stops, (interval[0]-domain[0])/(domain[1]-domain[0]), (interval[1]-domain[0])/(domain[1]-domain[0])), nil
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

// gradientFunctionArray 合并独立分量函数的断点，保留各分量的不连续边界
// 入参: functions 分量函数, interval 输入区间, channels 分量数, depth 嵌套深度
// 返回: []GradientStop 合并分段, error 函数错误
func (r *Reader) gradientFunctionArray(functions Array, interval [2]float64, channels, depth int) ([]GradientStop, error) {
	if len(functions) != channels {
		return nil, fmt.Errorf("invalid gradient function count")
	}
	parts := make([][]GradientStop, channels)
	positions := []float64{0, 1}
	for i, function := range functions {
		var err error
		parts[i], err = r.linearGradientStops(function, interval, 1, depth+1)
		if err != nil {
			return nil, err
		}
		for _, stop := range parts[i] {
			positions = append(positions, stop.Position)
		}
	}
	slices.Sort(positions)
	var stops []GradientStop
	for _, position := range slices.Compact(positions) {
		for _, left := range []bool{true, false} {
			stop := GradientStop{Position: position}
			for c, part := range parts {
				stop.Values[c] = gradientValueSide(part, position, left)[0]
			}
			stops = append(stops, stop)
		}
	}
	return slices.Compact(stops), nil
}

// sampledGradientStops 精确展开一维线性采样与范围截断，不以固定步长近似
// 入参: object 采样函数, interval 输入区间, channels 分量数
// 返回: []GradientStop 线性分段, error 函数错误
func (r *Reader) sampledGradientStops(object Object, interval [2]float64, channels int) ([]GradientStop, error) {
	f, err := r.readTintFunction(object, channels)
	if err != nil {
		return nil, err
	}
	positions := []float64{f.domain[0], f.domain[1]}
	if f.encoded[0] != f.encoded[1] {
		for i := 0; i < f.size; i++ {
			x := f.domain[0] + (float64(i)-f.encoded[0])/(f.encoded[1]-f.encoded[0])*(f.domain[1]-f.domain[0])
			if x > f.domain[0] && x < f.domain[1] {
				positions = append(positions, x)
			}
		}
	}
	slices.Sort(positions)
	positions = slices.Compact(positions)
	raw := *f
	raw.values = make([]float64, channels*2)
	for i := 0; i < channels; i++ {
		raw.values[2*i], raw.values[2*i+1] = math.Inf(-1), math.Inf(1)
	}
	knots := append([]float64(nil), positions...)
	for i := 1; i < len(positions); i++ {
		a, b := raw.color(positions[i-1]), raw.color(positions[i])
		for c := range a {
			if a[c] == b[c] {
				continue
			}
			for _, limit := range []float64{0, 1, f.values[2*c], f.values[2*c+1]} {
				t := (limit - a[c]) / (b[c] - a[c])
				if t > 0 && t < 1 {
					knots = append(knots, positions[i-1]+t*(positions[i]-positions[i-1]))
				}
			}
		}
	}
	slices.Sort(knots)
	stops := make([]GradientStop, 0, len(knots))
	for _, x := range slices.Compact(knots) {
		stop := GradientStop{Position: (x - f.domain[0]) / (f.domain[1] - f.domain[0])}
		for i, value := range f.color(x) {
			stop.Values[i] = math.Max(0, math.Min(1, value))
		}
		stops = append(stops, stop)
	}
	return gradientInterval(stops, (interval[0]-f.domain[0])/(f.domain[1]-f.domain[0]), (interval[1]-f.domain[0])/(f.domain[1]-f.domain[0])), nil
}
