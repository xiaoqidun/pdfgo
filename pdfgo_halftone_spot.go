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
	"math/bits"
	"sort"
)

// SpotFunction 保存预定义或自定义的二维网点函数，编译后不依赖阅读器
type SpotFunction struct {
	value func(float64, float64) (float64, error)
}

// ReadSpotFunction 读取标准预定义名称、类型0采样或类型4计算器网点函数
// 入参: object 网点函数名称、数据流或间接引用
// 返回: *SpotFunction 网点函数, error 定义错误
func (r *Reader) ReadSpotFunction(object Object) (*SpotFunction, error) {
	value, err := r.Resolve(object)
	if err != nil {
		return nil, err
	}
	if name, ok := value.(Name); ok {
		function, err := predefinedSpot(name)
		if err != nil {
			return nil, err
		}
		return &SpotFunction{value: func(x, y float64) (float64, error) { return function(x, y), nil }}, nil
	}
	stream, ok := value.(*Stream)
	if !ok {
		return nil, fmt.Errorf("invalid spot function stream")
	}
	kind, err := r.Resolve(stream.Dictionary["FunctionType"])
	if err != nil {
		return nil, err
	}
	if kind == Integer(0) {
		function, err := r.readSampledFunction(stream, 2, 1)
		if err != nil {
			return nil, err
		}
		return &SpotFunction{value: func(x, y float64) (float64, error) {
			var result [1]float64
			function.evaluate([]float64{x, y}, result[:])
			return result[0], nil
		}}, nil
	}
	if kind != Integer(4) {
		return nil, fmt.Errorf("invalid two-input spot function type")
	}
	domain, err := r.numberArray(stream.Dictionary["Domain"], 4)
	if err != nil {
		return nil, err
	}
	rangeValues, err := r.numberArray(stream.Dictionary["Range"], 2)
	if err != nil {
		return nil, err
	}
	if domain[0] > domain[1] || domain[2] > domain[3] || rangeValues[0] > rangeValues[1] {
		return nil, fmt.Errorf("invalid spot function bounds")
	}
	data, err := stream.Decode()
	if err != nil {
		return nil, err
	}
	program, err := compileCalculator(data)
	if err != nil {
		return nil, err
	}
	return &SpotFunction{value: func(x, y float64) (float64, error) {
		input := [2]float64{math.Max(domain[0], math.Min(domain[1], x)), math.Max(domain[2], math.Min(domain[3], y))}
		var output [1]float64
		if err := evaluateCalculator(program, input[:], output[:]); err != nil {
			return 0, err
		}
		return math.Max(rangeValues[0], math.Min(rangeValues[1], output[0])), nil
	}}, nil
}

// Evaluate 求值网点函数，输入及输出限制到标准网点区间
// 入参: x 横向网点坐标, y 纵向网点坐标
// 返回: float64 网点排序值, error 非有限值或计算错误
func (f *SpotFunction) Evaluate(x, y float64) (float64, error) {
	if math.IsNaN(x) || math.IsInf(x, 0) || math.IsNaN(y) || math.IsInf(y, 0) {
		return 0, fmt.Errorf("nonfinite spot coordinates")
	}
	value, err := f.value(math.Max(-1, math.Min(1, x)), math.Max(-1, math.Min(1, y)))
	if err != nil {
		return 0, err
	}
	if math.IsNaN(value) || math.IsInf(value, 0) {
		return 0, fmt.Errorf("nonfinite spot result")
	}
	return math.Max(-1, math.Min(1, value)), nil
}

// predefinedSpot 按ISO32000-1表128定义预置网点，不引入资源文件
// 入参: name 标准网点名称
// 返回: func(float64, float64) float64 网点函数, error 未知名称
func predefinedSpot(name Name) (func(float64, float64) float64, error) {
	switch name {
	case "SimpleDot":
		return func(x, y float64) float64 { return 1 - x*x - y*y }, nil
	case "InvertedSimpleDot":
		return func(x, y float64) float64 { return x*x + y*y - 1 }, nil
	case "DoubleDot":
		return func(x, y float64) float64 { return (math.Sin(2*math.Pi*x) + math.Sin(2*math.Pi*y)) / 2 }, nil
	case "InvertedDoubleDot":
		return func(x, y float64) float64 { return -(math.Sin(2*math.Pi*x) + math.Sin(2*math.Pi*y)) / 2 }, nil
	case "CosineDot":
		return func(x, y float64) float64 { return (math.Cos(math.Pi*x) + math.Cos(math.Pi*y)) / 2 }, nil
	case "Double":
		return func(x, y float64) float64 { return (math.Sin(math.Pi*x) + math.Sin(2*math.Pi*y)) / 2 }, nil
	case "InvertedDouble":
		return func(x, y float64) float64 { return -(math.Sin(math.Pi*x) + math.Sin(2*math.Pi*y)) / 2 }, nil
	case "Line":
		return func(x, y float64) float64 { return -math.Abs(y) }, nil
	case "LineX":
		return func(x, y float64) float64 { return x }, nil
	case "LineY":
		return func(x, y float64) float64 { return y }, nil
	case "Round":
		return func(x, y float64) float64 {
			x, y = math.Abs(x), math.Abs(y)
			if x+y <= 1 {
				return 1 - x*x - y*y
			}
			return (x-1)*(x-1) + (y-1)*(y-1) - 1
		}, nil
	case "Ellipse":
		return func(x, y float64) float64 {
			x, y = math.Abs(x), math.Abs(y)
			w := 3*x + 4*y - 3
			if w < 0 {
				return 1 - (x*x+y*y/(0.75*0.75))/4
			}
			if w > 1 {
				return ((1-x)*(1-x)+(1-y)*(1-y)/(0.75*0.75))/4 - 1
			}
			return 0.5 - w
		}, nil
	case "EllipseA":
		return func(x, y float64) float64 { return 1 - x*x - 0.9*y*y }, nil
	case "InvertedEllipseA":
		return func(x, y float64) float64 { return x*x + 0.9*y*y - 1 }, nil
	case "EllipseB":
		return func(x, y float64) float64 { return 1 - math.Sqrt(x*x+5*y*y/8) }, nil
	case "EllipseC":
		return func(x, y float64) float64 { return 1 - 0.9*x*x - y*y }, nil
	case "InvertedEllipseC":
		return func(x, y float64) float64 { return 0.9*x*x + y*y - 1 }, nil
	case "Square":
		return func(x, y float64) float64 { return -math.Max(math.Abs(x), math.Abs(y)) }, nil
	case "Cross":
		return func(x, y float64) float64 { return -math.Min(math.Abs(x), math.Abs(y)) }, nil
	case "Rhomboid":
		return func(x, y float64) float64 { return (0.9*math.Abs(x) + math.Abs(y)) / 2 }, nil
	case "Diamond":
		return func(x, y float64) float64 {
			x, y = math.Abs(x), math.Abs(y)
			if x+y <= 0.75 {
				return 1 - x*x - y*y
			}
			if x+y <= 1.23 {
				return 1 - 0.85*x - y
			}
			return (x-1)*(x-1) + (y-1)*(y-1) - 1
		}, nil
	}
	return nil, &UnsupportedError{Feature: "spot function " + string(name)}
}

// spotThresholds 按网点函数排序设备像素，精确模式使用16倍边长的超网格
// 入参: ctx 取消上下文, s 输出网屏, h 网点定义, resolution 设备分辨率, limit 像素上限
// 返回: error 设备参数、容量或函数错误
func (r *Reader) spotThresholds(ctx context.Context, s *HalftoneScreen, h *Halftone, resolution float64, limit int) error {
	if resolution <= 0 || math.IsInf(resolution, 0) || math.IsNaN(resolution) || h.Frequency <= 0 || math.IsInf(h.Frequency, 0) || math.IsNaN(h.Frequency) || math.IsInf(h.Angle, 0) || math.IsNaN(h.Angle) {
		return fmt.Errorf("invalid spot screen device parameters")
	}
	factor := 1.0
	if h.AccurateScreens {
		factor = 16
	}
	angle := math.Mod(h.Angle, 360) * math.Pi / 180
	length := resolution / h.Frequency * factor
	a, b := math.Round(length*math.Cos(angle)), math.Round(length*math.Sin(angle))
	if math.IsInf(length, 0) || math.Abs(a) > math.Sqrt(float64(limit)) || math.Abs(b) > math.Sqrt(float64(limit)) {
		return fmt.Errorf("oversized spot screen cell")
	}
	s.width, s.height = int(math.Abs(a)), int(math.Abs(a))
	s.width2, s.height2 = int(math.Abs(b)), int(math.Abs(b))
	count, err := s.cellPixels(limit)
	if err != nil {
		return err
	}
	if a < 0 || a == 0 && b > 0 {
		s.xsign = -1
	}
	if b > 0 || b == 0 && a < 0 {
		s.ysign = -1
	}
	function, err := r.ReadSpotFunction(h.SpotFunction)
	if err != nil {
		return err
	}
	samples := make([]struct {
		index int
		value float64
	}, 0, count)
	for _, rect := range [][3]int{{s.width, s.height, 0}, {s.width2, s.height2, s.height}} {
		for y := 0; y < rect[1]; y++ {
			if err := ctx.Err(); err != nil {
				return err
			}
			for x := 0; x < rect[0]; x++ {
				px, py := float64(x)+0.5, float64(y+rect[2])+0.5
				u := (float64(s.width)*px - float64(s.width2)*py) / float64(count) * factor
				v := (float64(s.width2)*px + float64(s.width)*py) / float64(count) * factor * float64(s.xsign*s.ysign)
				u, v = u-math.Floor(u), v-math.Floor(v)
				value, err := function.Evaluate(2*u-1, 2*v-1)
				if err != nil {
					return err
				}
				samples = append(samples, struct {
					index int
					value float64
				}{len(samples), value})
			}
		}
	}
	sort.Slice(samples, func(i, j int) bool {
		if samples[i].value == samples[j].value {
			return samples[i].index < samples[j].index
		}
		return samples[i].value < samples[j].value
	})
	s.thresholds = make([]uint16, count)
	for rank, sample := range samples {
		hi, lo := bits.Mul64(uint64(rank+1), 65535)
		value, remainder := bits.Div64(hi, lo, uint64(count))
		if remainder != 0 {
			value++
		}
		s.thresholds[sample.index] = uint16(value)
	}
	return nil
}
