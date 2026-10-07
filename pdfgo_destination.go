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

// DestinationTarget 保存左上原点、向右向下坐标中的目标，不依赖窗口或界面
// KeepLeft、KeepTop和KeepZoom保留目标中的空值语义，坐标单位由变换矩阵确定
type DestinationTarget struct {
	Mode                           Name
	Left, Top, Right, Bottom, Zoom float64
	KeepLeft, KeepTop, KeepZoom    bool
}

// DestinationView 保存视口左上位置和缩放率，位置使用未缩放的页面坐标
type DestinationView struct {
	Left, Top, Zoom float64
}

// DestinationViewport 提供目标计算所需的窗口尺寸、当前视图和页面范围
// Width和Height与目标坐标采用相同单位，Zoom为1时不缩放；不包含工具栏或页间距
// PageBounds为变换后的CropBox，不使用MediaBox或界面外框代替
// ContentBounds须由调用方度量，使用变换后的页面坐标，FitB系列不以整页冒充内容范围
type DestinationViewport struct {
	Width, Height float64
	Current       DestinationView
	PageBounds    Rectangle
	ContentBounds *Rectangle
}

// Transform 将PDF目标转换到左上原点坐标，支持缩放、平移和直角旋转，保留空值
// 入参: matrix 从PDF默认用户空间到目标页面坐标的轴对齐可逆变换
// 返回: DestinationTarget 变换后的目标, error 模式、参数或变换错误
func (d Destination) Transform(matrix Matrix) (DestinationTarget, error) {
	count, ok := destinationParameterCount(d.Mode)
	if !ok || len(d.Parameters) != count {
		return DestinationTarget{}, fmt.Errorf("%w: mode or parameter count", ErrInvalidDestination)
	}
	for _, value := range matrix {
		if !destinationFinite(value) {
			return DestinationTarget{}, fmt.Errorf("%w: nonfinite transform", ErrInvalidDestination)
		}
	}
	swapped := matrix[0] == 0 && matrix[3] == 0 && matrix[1] != 0 && matrix[2] != 0
	if !swapped && !(matrix[1] == 0 && matrix[2] == 0 && matrix[0] != 0 && matrix[3] != 0) {
		return DestinationTarget{}, fmt.Errorf("%w: nonorthogonal transform", ErrInvalidDestination)
	}
	var values [4]float64
	var retained [4]bool
	for index, object := range d.Parameters {
		switch value := object.(type) {
		case nil:
			if d.Mode == "FitR" {
				return DestinationTarget{}, fmt.Errorf("%w: null rectangle parameter", ErrInvalidDestination)
			}
			retained[index] = true
		case Integer:
			values[index] = float64(value)
		case Real:
			values[index] = float64(value)
		default:
			return DestinationTarget{}, fmt.Errorf("%w: numeric parameter", ErrInvalidDestination)
		}
		if !destinationFinite(values[index]) || d.Mode == "XYZ" && index == 2 && values[index] < 0 {
			return DestinationTarget{}, fmt.Errorf("%w: parameter range", ErrInvalidDestination)
		}
	}
	result := DestinationTarget{Mode: d.Mode}
	switch d.Mode {
	case "XYZ":
		point := matrix.Apply(Point{X: values[0], Y: values[1]})
		result.Left, result.Top, result.Zoom = point.X, point.Y, values[2]
		result.KeepLeft, result.KeepTop, result.KeepZoom = retained[0], retained[1], retained[2] || values[2] == 0
		if swapped {
			result.KeepLeft, result.KeepTop = retained[1], retained[0]
		}
	case "FitH", "FitV", "FitBH", "FitBV":
		horizontal := d.Mode == "FitH" || d.Mode == "FitBH"
		point := Point{X: values[0]}
		if horizontal {
			point = Point{Y: values[0]}
		}
		point = matrix.Apply(point)
		if swapped {
			switch d.Mode {
			case "FitH":
				result.Mode = "FitV"
			case "FitV":
				result.Mode = "FitH"
			case "FitBH":
				result.Mode = "FitBV"
			case "FitBV":
				result.Mode = "FitBH"
			}
			horizontal = !horizontal
		}
		if horizontal {
			result.Top, result.KeepTop = point.Y, retained[0]
		} else {
			result.Left, result.KeepLeft = point.X, retained[0]
		}
	case "FitR":
		a := matrix.Apply(Point{X: values[0], Y: values[1]})
		b := matrix.Apply(Point{X: values[2], Y: values[3]})
		result.Left, result.Top = math.Min(a.X, b.X), math.Min(a.Y, b.Y)
		result.Right, result.Bottom = math.Max(a.X, b.X), math.Max(a.Y, b.Y)
	}
	for _, value := range []float64{result.Left, result.Top, result.Right, result.Bottom} {
		if !destinationFinite(value) {
			return DestinationTarget{}, fmt.Errorf("%w: transformed coordinate overflow", ErrInvalidDestination)
		}
	}
	return result, nil
}

// View 按PDF目标语义计算定位与缩放，不限制滚动范围或执行跳转
// 入参: viewport 当前窗口、视图和内容度量
// 返回: DestinationView 新视图, error 无效范围、缺少内容度量或计算溢出
func (d DestinationTarget) View(viewport DestinationViewport) (DestinationView, error) {
	if _, ok := destinationParameterCount(d.Mode); !ok {
		return DestinationView{}, fmt.Errorf("%w: unknown mode", ErrInvalidDestination)
	}
	for _, value := range []float64{viewport.Width, viewport.Height, viewport.Current.Left, viewport.Current.Top, viewport.Current.Zoom, d.Left, d.Top, d.Right, d.Bottom, d.Zoom} {
		if !destinationFinite(value) {
			return DestinationView{}, fmt.Errorf("%w: nonfinite viewport", ErrInvalidDestination)
		}
	}
	if viewport.Width <= 0 || viewport.Height <= 0 || viewport.Current.Zoom <= 0 || d.Zoom < 0 || !destinationRectangle(viewport.PageBounds) {
		return DestinationView{}, fmt.Errorf("%w: viewport range", ErrInvalidDestination)
	}
	box := viewport.PageBounds
	if d.Mode == "FitB" || d.Mode == "FitBH" || d.Mode == "FitBV" {
		if viewport.ContentBounds == nil || !destinationRectangle(*viewport.ContentBounds) {
			return DestinationView{}, fmt.Errorf("%w: content bounds required", ErrInvalidDestination)
		}
		content := *viewport.ContentBounds
		box = Rectangle{math.Max(box.XMin, content.XMin), math.Max(box.YMin, content.YMin), math.Min(box.XMax, content.XMax), math.Min(box.YMax, content.YMax)}
		if !destinationRectangle(box) {
			return DestinationView{}, fmt.Errorf("%w: empty visible content bounds", ErrInvalidDestination)
		}
	} else if d.Mode == "FitR" {
		box = Rectangle{d.Left, d.Top, d.Right, d.Bottom}
		if !destinationRectangle(box) {
			return DestinationView{}, fmt.Errorf("%w: empty target rectangle", ErrInvalidDestination)
		}
	}
	result := viewport.Current
	switch d.Mode {
	case "XYZ":
		if !d.KeepLeft {
			result.Left = d.Left
		}
		if !d.KeepTop {
			result.Top = d.Top
		}
		if !d.KeepZoom && d.Zoom != 0 {
			result.Zoom = d.Zoom
		}
	case "FitH", "FitBH":
		result.Zoom, result.Left = viewport.Width/(box.XMax-box.XMin), box.XMin
		if !d.KeepTop {
			result.Top = d.Top
		}
	case "FitV", "FitBV":
		result.Zoom, result.Top = viewport.Height/(box.YMax-box.YMin), box.YMin
		if !d.KeepLeft {
			result.Left = d.Left
		}
	default:
		result.Zoom = math.Min(viewport.Width/(box.XMax-box.XMin), viewport.Height/(box.YMax-box.YMin))
		result.Left = box.XMin + ((box.XMax-box.XMin)-viewport.Width/result.Zoom)/2
		result.Top = box.YMin + ((box.YMax-box.YMin)-viewport.Height/result.Zoom)/2
	}
	if !destinationFinite(result.Left) || !destinationFinite(result.Top) || !destinationFinite(result.Zoom) || result.Zoom <= 0 {
		return DestinationView{}, fmt.Errorf("%w: viewport calculation overflow", ErrInvalidDestination)
	}
	return result, nil
}

// destinationParameterCount 返回标准显式目标的参数数量
// 入参: mode 目标模式
// 返回: int 参数数量, bool 是否为标准模式
func destinationParameterCount(mode Name) (int, bool) {
	switch mode {
	case "Fit", "FitB":
		return 0, true
	case "FitH", "FitV", "FitBH", "FitBV":
		return 1, true
	case "XYZ":
		return 3, true
	case "FitR":
		return 4, true
	}
	return 0, false
}

// destinationRectangle 检查目标矩形的有限坐标和正面积
// 入参: box 目标矩形
// 返回: bool 范围是否有效
func destinationRectangle(box Rectangle) bool {
	return destinationFinite(box.XMin) && destinationFinite(box.YMin) && destinationFinite(box.XMax) && destinationFinite(box.YMax) && box.XMax > box.XMin && box.YMax > box.YMin && destinationFinite(box.XMax-box.XMin) && destinationFinite(box.YMax-box.YMin)
}

// destinationFinite 检查目标计算数值是否有限
// 入参: value 数值
// 返回: bool 是否为有限值
func destinationFinite(value float64) bool {
	return !math.IsNaN(value) && !math.IsInf(value, 0)
}
