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

// ScaleFactor 按PDF标准表305计算投影倍率，不按模型包围盒重新适配
// 透视投影的相机坐标还需除以各自的z坐标；返回值不含视口平移
// 入参: viewBox 三维视口, annotationBox 注解区域
// 返回: float64 投影倍率, error 投影参数或尺寸错误
func (p ThreeDProjection) ScaleFactor(viewBox, annotationBox Rectangle) (float64, error) {
	var scale float64
	switch p.Subtype {
	case "P":
		if math.IsNaN(p.FieldOfView) || p.FieldOfView <= 0 || p.FieldOfView >= 180 {
			return 0, &UnsupportedError{Feature: "degenerate 3D perspective field of view"}
		}
		diameter := p.PerspectiveDiameter
		if p.PerspectiveBinding != "" {
			if p.PerspectiveBinding == "Absolute" {
				return 0, fmt.Errorf("invalid 3D perspective binding")
			}
			var err error
			diameter, err = threeDBinding(p.PerspectiveBinding, viewBox)
			if err != nil {
				return 0, err
			}
		}
		if math.IsNaN(diameter) || math.IsInf(diameter, 0) || diameter <= 0 {
			return 0, fmt.Errorf("invalid 3D perspective diameter")
		}
		scale = diameter / (2 * math.Tan(p.FieldOfView*math.Pi/360))
	case "O":
		binding, err := threeDBinding(p.OrthographicBinding, annotationBox)
		if err != nil {
			return 0, err
		}
		scale = p.OrthographicScale * binding
	default:
		return 0, fmt.Errorf("invalid 3D projection subtype")
	}
	if math.IsNaN(scale) || math.IsInf(scale, 0) || scale <= 0 {
		return 0, fmt.Errorf("invalid 3D projection scale")
	}
	return scale, nil
}

// threeDBinding 计算视口或注解区域的绑定倍率
// 入参: binding 绑定方式, box 目标区域
// 返回: float64 倍率, error 方式或尺寸错误
func threeDBinding(binding Name, box Rectangle) (float64, error) {
	if binding == "Absolute" {
		return 1, nil
	}
	w, h := box.XMax-box.XMin, box.YMax-box.YMin
	if math.IsNaN(w) || math.IsNaN(h) || math.IsInf(w, 0) || math.IsInf(h, 0) || w <= 0 || h <= 0 {
		return 0, fmt.Errorf("invalid 3D binding dimensions")
	}
	switch binding {
	case "W":
		return w, nil
	case "H":
		return h, nil
	case "Min":
		return min(w, h), nil
	case "Max":
		return max(w, h), nil
	}
	return 0, fmt.Errorf("invalid 3D binding")
}

// WalkThreeDOverlay 解释选定正交视图的叠加外观，映射到页面用户空间并裁剪到三维视口
// 调用方应在激活的静态视图上最后绘制，不用于手动导航或脚本修改后的场景
// 入参: ctx 取消上下文, page 所在页面, source 三维参数, visitor 图元访问器
// 返回: error 坐标、外观、能力或访问错误
func (r *Reader) WalkThreeDOverlay(ctx context.Context, page *Page, source ThreeD, visitor Visitor) error {
	if ctx == nil {
		return fmt.Errorf("invalid 3D overlay context")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if r == nil || r.closed || page == nil || page.reader != r {
		return fmt.Errorf("3D overlay reader unavailable")
	}
	stream := source.Presentation.Overlay
	if stream == nil {
		return nil
	}
	if stream.reader != nil && stream.reader != r {
		return fmt.Errorf("3D overlay belongs to another reader")
	}
	if source.Camera.Source != "M" && source.Camera.Source != "U3D" || source.Projection == nil {
		return fmt.Errorf("missing 3D overlay camera or projection")
	}
	if source.Projection.Subtype != "O" {
		return &UnsupportedError{Feature: "3D perspective view overlay"}
	}
	subtype, err := r.Resolve(stream.Dictionary["Subtype"])
	if err != nil {
		return err
	}
	if subtype != Name("Form") {
		return fmt.Errorf("invalid 3D view overlay subtype")
	}
	for _, box := range []Rectangle{source.AnnotationBox, source.ViewBox} {
		for _, n := range []float64{box.XMin, box.YMin, box.XMax, box.YMax} {
			if math.IsNaN(n) || math.IsInf(n, 0) {
				return fmt.Errorf("invalid 3D overlay bounds")
			}
		}
		if box.XMin >= box.XMax || box.YMin >= box.YMax {
			return ctx.Err()
		}
		if math.IsInf(box.XMax-box.XMin, 0) || math.IsInf(box.YMax-box.YMin, 0) {
			return fmt.Errorf("invalid 3D overlay dimensions")
		}
	}
	scale, err := source.Projection.ScaleFactor(source.ViewBox, source.AnnotationBox)
	if err != nil {
		return err
	}
	a, b := source.AnnotationBox, source.ViewBox
	w, h := a.XMax-a.XMin, a.YMax-a.YMin
	if b.XMin < -w/2 || b.XMax > w/2 || b.YMin < -h/2 || b.YMax > h/2 {
		return fmt.Errorf("3D overlay viewport outside annotation")
	}
	x, y := a.XMin/2+a.XMax/2, a.YMin/2+a.YMax/2
	clip := Rectangle{XMin: x + b.XMin, YMin: y + b.YMin, XMax: x + b.XMax, YMax: y + b.YMax}
	matrix := Matrix{scale, 0, 0, scale, x + b.XMin/2 + b.XMax/2, y + b.YMin/2 + b.YMax/2}
	for _, n := range []float64{clip.XMin, clip.YMin, clip.XMax, clip.YMax, matrix[4], matrix[5]} {
		if math.IsNaN(n) || math.IsInf(n, 0) {
			return fmt.Errorf("invalid 3D overlay placement")
		}
	}
	path := Path{Segments: []Segment{
		{"M", []Point{{clip.XMin, clip.YMin}}},
		{"L", []Point{{clip.XMax, clip.YMin}}},
		{"L", []Point{{clip.XMax, clip.YMax}}},
		{"L", []Point{{clip.XMin, clip.YMax}}},
		{"C", nil},
	}}
	interpreter := pageInterpreter{reader: r, resources: page.Resources, pageResources: page.Resources, visitor: visitor, ctx: ctx, bounds: clip}
	interpreter.patternMatrix = Identity()
	interpreter.state = graphicsState{matrix: matrix, hscale: 1, fillSpace: "DeviceGray", strokeSpace: "DeviceGray", style: Style{Fill: Paint{SourceSpace: "DeviceGray", Alpha: 1}, Stroke: Paint{SourceSpace: "DeviceGray", Alpha: 1}, LineWidth: 1, MiterLimit: 10, BlendMode: "Normal", Clips: []Path{path}}}
	return interpreter.form(stream)
}
