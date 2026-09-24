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

// GradientStop 保存轴向渐变的归一化位置与sRGB颜色
type GradientStop struct {
	Position float64
	RGB      [3]float64
}

// AxialGradient 保存页面坐标中的渐变轴及两端延伸方式
type AxialGradient struct {
	Start, End Point
	Extend     [2]bool
	Stops      []GradientStop
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

// axialPattern 解析着色图案中的轴向线性渐变
// 入参: name 图案资源名
// 返回: *AxialGradient 渐变信息, error 不支持的图案或解析错误
func (p *pageInterpreter) axialPattern(name Name) (*AxialGradient, error) {
	object, err := p.resource("Pattern", name)
	if err != nil {
		return nil, err
	}
	v, err := p.reader.Resolve(object)
	if err != nil {
		return nil, err
	}
	dict, ok := v.(Dictionary)
	if !ok || dict["PatternType"] != Integer(2) || dict["ExtGState"] != nil {
		return nil, &UnsupportedError{Feature: "shading pattern type or graphics state"}
	}
	m := p.patternMatrix
	if dict["Matrix"] != nil {
		v, err := p.reader.numberArray(dict["Matrix"], 6)
		if err != nil {
			return nil, err
		}
		m = m.Mul(Matrix(v))
	}
	sx, sy := math.Hypot(m[0], m[1]), math.Hypot(m[2], m[3])
	if sx == 0 || math.Abs(sx-sy) > 1e-8*math.Max(1, sx) || math.Abs(m[0]*m[2]+m[1]*m[3]) > 1e-8*math.Max(1, sx*sy) {
		return nil, &UnsupportedError{Feature: "anisotropic axial shading pattern"}
	}
	v, err = p.reader.Resolve(dict["Shading"])
	if err != nil {
		return nil, err
	}
	shading, ok := v.(Dictionary)
	if !ok || shading["ShadingType"] != Integer(2) || shading["Background"] != nil || shading["BBox"] != nil {
		return nil, &UnsupportedError{Feature: "axial shading dictionary"}
	}
	if shading["Domain"] != nil {
		d, err := p.reader.numberArray(shading["Domain"], 2)
		if err != nil {
			return nil, err
		}
		if d[0] != 0 || d[1] != 1 {
			return nil, &UnsupportedError{Feature: "shading domain"}
		}
	}
	coords, err := p.reader.numberArray(shading["Coords"], 4)
	if err != nil {
		return nil, err
	}
	start, end := Point{coords[0], coords[1]}, Point{coords[2], coords[3]}
	if start == end {
		return nil, fmt.Errorf("degenerate shading axis")
	}
	gradient := &AxialGradient{Start: m.Apply(start), End: m.Apply(end)}
	if shading["Extend"] != nil {
		v, err := p.reader.Resolve(shading["Extend"])
		if err != nil {
			return nil, err
		}
		a, ok := v.(Array)
		if !ok || len(a) != 2 {
			return nil, fmt.Errorf("invalid shading extension")
		}
		for i, v := range a {
			b, ok := v.(Boolean)
			if !ok {
				return nil, fmt.Errorf("invalid shading extension")
			}
			gradient.Extend[i] = bool(b)
		}
	}
	gradient.Stops, err = p.reader.linearGradientStops(shading["Function"], 0)
	if err != nil {
		return nil, err
	}
	space, err := p.reader.Resolve(shading["ColorSpace"])
	if err != nil {
		return nil, err
	}
	if space != Name("DeviceRGB") {
		if err := p.reader.validateRGBGroupSpace(space); err != nil {
			return nil, err
		}
		profile, err := p.reader.readICCRGB(space.(Array))
		if err != nil {
			return nil, err
		}
		for i := range gradient.Stops {
			stop := &gradient.Stops[i]
			stop.RGB, err = profile.color(stop.RGB[:], p.state.style.RenderingIntent)
			if err != nil {
				return nil, err
			}
		}
	}
	return gradient, nil
}

// linearGradientStops 解析线性指数函数及线性拼接函数
// 入参: object 函数字典或引用, depth 当前嵌套深度
// 返回: []GradientStop 渐变分段, error 解析错误
func (r *Reader) linearGradientStops(object Object, depth int) ([]GradientStop, error) {
	if depth >= 32 {
		return nil, fmt.Errorf("gradient function recursion limit exceeded")
	}
	v, err := r.Resolve(object)
	if err != nil {
		return nil, err
	}
	dict, ok := v.(Dictionary)
	if !ok {
		return nil, &UnsupportedError{Feature: "gradient function type"}
	}
	domain, err := r.numberArray(dict["Domain"], 2)
	if err != nil {
		return nil, err
	}
	if domain[0] != 0 || domain[1] != 1 || dict["Range"] != nil {
		return nil, &UnsupportedError{Feature: "gradient function domain or range"}
	}
	switch dict["FunctionType"] {
	case Integer(2):
		n, err := r.number(dict["N"])
		if err != nil {
			return nil, err
		}
		if n != 1 {
			return nil, &UnsupportedError{Feature: "nonlinear gradient function"}
		}
		stops := make([]GradientStop, 2)
		for i, key := range []Name{"C0", "C1"} {
			color, err := r.numberArray(dict[key], 3)
			if err != nil {
				return nil, err
			}
			for _, c := range color {
				if c < 0 || c > 1 {
					return nil, fmt.Errorf("invalid gradient color")
				}
			}
			stops[i] = GradientStop{Position: float64(i), RGB: [3]float64(color)}
		}
		return stops, nil
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
		points := append(append([]float64{0}, bounds...), 1)
		var stops []GradientStop
		for i, function := range functions {
			if points[i] >= points[i+1] || encode[i*2] != 0 || encode[i*2+1] != 1 {
				return nil, &UnsupportedError{Feature: "gradient stitching bounds or encoding"}
			}
			part, err := r.linearGradientStops(function, depth+1)
			if err != nil {
				return nil, err
			}
			for _, stop := range part {
				stop.Position = points[i] + stop.Position*(points[i+1]-points[i])
				stops = append(stops, stop)
			}
		}
		return stops, nil
	default:
		return nil, &UnsupportedError{Feature: "gradient function type"}
	}
}
