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

// GradientPosition 保存只读渐变几何，独立于颜色函数及着色图案边界
type GradientPosition struct {
	start      Point
	dx, dy, dr float64
	radius     float64
	scale      float64
	inverse    Matrix
	extend     [2]bool
	radial     bool
}

// PreparePosition 准备轴向参数求值，不近似颜色函数或应用BBox
// 返回: GradientPosition 只读几何, error 非有限坐标
func (g *AxialGradient) PreparePosition() (GradientPosition, error) {
	if g == nil {
		return GradientPosition{}, fmt.Errorf("invalid axial gradient geometry")
	}
	p := GradientPosition{start: g.Start, dx: g.End.X - g.Start.X, dy: g.End.Y - g.Start.Y, extend: g.Extend, inverse: Identity()}
	for _, value := range [4]float64{g.Start.X, g.Start.Y, g.End.X, g.End.Y} {
		if math.IsNaN(value) || math.IsInf(value, 0) {
			return GradientPosition{}, fmt.Errorf("invalid axial gradient coordinates")
		}
	}
	p.scale = math.Max(math.Abs(p.dx), math.Abs(p.dy))
	if math.IsInf(p.scale, 0) {
		p.scale = math.Max(math.Max(math.Abs(g.Start.X), math.Abs(g.Start.Y)), math.Max(math.Abs(g.End.X), math.Abs(g.End.Y)))
		p.dx, p.dy = g.End.X/p.scale-g.Start.X/p.scale, g.End.Y/p.scale-g.Start.Y/p.scale
	} else if p.scale != 0 {
		p.dx, p.dy = p.dx/p.scale, p.dy/p.scale
	}
	return p, nil
}

// PreparePosition 准备双圆参数求值，复用页面到渐变坐标的逆矩阵
// 返回: GradientPosition 只读几何, error 非法半径或矩阵
func (g *RadialGradient) PreparePosition() (GradientPosition, error) {
	if g == nil {
		return GradientPosition{}, fmt.Errorf("invalid radial gradient geometry")
	}
	p := GradientPosition{start: g.Start, dx: g.End.X - g.Start.X, dy: g.End.Y - g.Start.Y, dr: g.EndRadius - g.StartRadius, radius: g.StartRadius, extend: g.Extend, radial: true, inverse: Identity()}
	for _, value := range [6]float64{g.Start.X, g.Start.Y, g.End.X, g.End.Y, g.StartRadius, g.EndRadius} {
		if math.IsNaN(value) || math.IsInf(value, 0) {
			return GradientPosition{}, fmt.Errorf("invalid radial gradient coordinates")
		}
	}
	if g.StartRadius < 0 || g.EndRadius < 0 {
		return GradientPosition{}, fmt.Errorf("invalid radial gradient radius")
	}
	if g.Matrix != (Matrix{}) {
		var ok bool
		p.inverse, ok = g.Matrix.Inverse()
		if !ok {
			return GradientPosition{}, fmt.Errorf("singular radial gradient matrix")
		}
	}
	p.scale = math.Max(math.Max(math.Abs(p.dx), math.Abs(p.dy)), math.Max(g.StartRadius, g.EndRadius))
	if math.IsInf(p.scale, 0) {
		p.scale = math.Max(math.Max(math.Abs(g.Start.X), math.Abs(g.Start.Y)), math.Max(math.Abs(g.End.X), math.Abs(g.End.Y)))
		p.dx, p.dy = g.End.X/p.scale-g.Start.X/p.scale, g.End.Y/p.scale-g.Start.Y/p.scale
	} else if p.scale != 0 {
		p.dx, p.dy = p.dx/p.scale, p.dy/p.scale
	}
	if p.scale != 0 {
		p.dr, p.radius = p.dr/p.scale, p.radius/p.scale
	}
	return p, nil
}

// PositionAt 按页面点求参数，双圆多解取半径非负且符合Extend的最大参数
// 入参: point 页面坐标
// 返回: float64 归一化参数, bool 是否处于着色范围
func (g GradientPosition) PositionAt(point Point) (float64, bool) {
	point = g.inverse.Apply(point)
	if g.scale == 0 || math.IsNaN(point.X) || math.IsNaN(point.Y) || math.IsInf(point.X, 0) || math.IsInf(point.Y, 0) {
		return 0, false
	}
	px, py := gradientCoordinate(point.X, g.start.X, g.scale), gradientCoordinate(point.Y, g.start.Y, g.scale)
	if !g.radial {
		var t float64
		if g.dy == 0 {
			t = px / g.dx
		} else if g.dx == 0 {
			t = py / g.dy
		} else {
			t = (px*g.dx + py*g.dy) / (g.dx*g.dx + g.dy*g.dy)
			if math.IsNaN(t) || math.IsInf(t, 0) {
				scale := math.Max(math.Max(math.Abs(point.X), math.Abs(point.Y)), math.Max(math.Abs(g.start.X), math.Abs(g.start.Y)))
				px, py = gradientCoordinate(point.X, g.start.X, scale), gradientCoordinate(point.Y, g.start.Y, scale)
				t = gradientRatio((px*g.dx+py*g.dy)/(g.dx*g.dx+g.dy*g.dy), scale, g.scale)
			}
		}
		t = gradientFinite(t)
		return t, g.accept(t)
	}
	scale := math.Max(1, math.Max(math.Abs(px), math.Abs(py)))
	numerator, denominator := scale, 1.0
	if math.IsInf(scale, 0) {
		scale = math.Max(math.Max(math.Abs(point.X), math.Abs(point.Y)), math.Max(math.Abs(g.start.X), math.Abs(g.start.Y)))
		px, py = gradientCoordinate(point.X, g.start.X, scale), gradientCoordinate(point.Y, g.start.Y, scale)
		numerator, denominator = scale, g.scale
	} else {
		px, py = px/scale, py/scale
	}
	radius := gradientRatio(g.radius, denominator, numerator)
	if g.dx == 0 && g.dy == 0 && g.dr != 0 {
		t := gradientRatio((math.Hypot(px, py)-radius)/g.dr, numerator, denominator)
		return t, g.accept(t)
	}
	dx, dy, dr := g.dx, g.dy, g.dr
	a, b, c := dx*dx+dy*dy-dr*dr, -2*(px*dx+py*dy+radius*dr), px*px+py*py-radius*radius
	roots := [2]float64{math.NaN(), math.NaN()}
	if a == 0 {
		if b != 0 {
			roots[0] = -c / b
		}
	} else if discriminant := b*b - 4*a*c; discriminant >= 0 {
		q := -.5 * (b + math.Copysign(math.Sqrt(discriminant), b))
		if q == 0 {
			roots[0] = -b / (2 * a)
		} else {
			roots[0], roots[1] = q/a, c/q
		}
	}
	t := math.Inf(-1)
	for _, root := range roots {
		parameter := gradientRatio(root, numerator, denominator)
		if g.accept(parameter) && radius+root*dr >= 0 && parameter > t {
			t = parameter
		}
	}
	return t, g.accept(t)
}

// gradientRatio 缩放参数，避免中间乘除溢出，超出有限范围时保留端点方向
// 入参: value 参数, numerator 分子尺度, denominator 非零分母尺度
// 返回: float64 有限参数，非法参数仍返回NaN
func gradientRatio(value, numerator, denominator float64) float64 {
	if numerator == denominator {
		return gradientFinite(value)
	}
	product := value * numerator
	if product == 0 && value == 0 {
		return value
	}
	if product != 0 && !math.IsInf(product, 0) {
		if result := product / denominator; result != 0 && !math.IsInf(result, 0) {
			return result
		}
	}
	v, ve := math.Frexp(value)
	n, ne := math.Frexp(numerator)
	d, de := math.Frexp(denominator)
	return gradientFinite(math.Ldexp(v*n/d, ve+ne-de))
}

// gradientFinite 保留参数符号，确保延伸颜色可以按有限端点求值
// 入参: value 参数
// 返回: float64 参数，溢出时取同方向的最大有限值
func gradientFinite(value float64) float64 {
	if math.IsInf(value, 0) {
		return math.Copysign(math.MaxFloat64, value)
	}
	return value
}

// accept 检查有限参数及两端延伸，不裁切有效参数
// 入参: position 归一化参数
// 返回: bool 是否可着色
func (g GradientPosition) accept(position float64) bool {
	return !math.IsNaN(position) && !math.IsInf(position, 0) && (position >= 0 || g.extend[0]) && (position <= 1 || g.extend[1])
}

// gradientCoordinate 缩放坐标差，跨有限端点溢出时先缩放端点
// 入参: value 坐标, origin 原点, scale 非零尺度
// 返回: float64 缩放后的坐标差
func gradientCoordinate(value, origin, scale float64) float64 {
	difference := value - origin
	if math.IsInf(difference, 0) {
		return value/scale - origin/scale
	}
	return difference / scale
}
