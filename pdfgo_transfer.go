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
	"os"
)

// TransferFunction 保存红、绿、蓝及灰度的传递函数，可独立于阅读器并发求值
// 输入输出均按加色分量解释，空值表示恒等设备默认值
type TransferFunction struct {
	functions [4]*ColorFunction
}

// ReadTransferFunction 解析单函数或四通道数组，恒等函数返回空值
// 入参: object 传递函数定义或Identity
// 返回: *TransferFunction 已解析函数, error 引用或函数错误
func (r *Reader) ReadTransferFunction(object Object) (*TransferFunction, error) {
	if r.closed {
		return nil, os.ErrClosed
	}
	value, err := r.Resolve(object)
	if err != nil {
		return nil, err
	}
	if value == nil || value == Name("Identity") {
		return nil, nil
	}
	objects := Array{value}
	if array, ok := value.(Array); ok {
		if len(array) != 4 {
			return nil, fmt.Errorf("invalid transfer function array")
		}
		objects = array
	}
	var result TransferFunction
	for i, object := range objects {
		object, err := r.Resolve(object)
		if err != nil {
			return nil, err
		}
		if object == Name("Identity") {
			continue
		}
		function, err := r.ReadColorFunction(object)
		if err != nil {
			return nil, err
		}
		if function.function.linear != nil {
			stops := function.function.linear([2]float64{0, 1})
			identity := len(stops) >= 2 && stops[0].Position == 0 && stops[len(stops)-1].Position == 1
			for _, stop := range stops {
				identity = identity && stop.Values[0] == stop.Position
			}
			if identity {
				continue
			}
		}
		result.functions[i] = function
	}
	if len(objects) == 1 {
		for i := 1; i < len(result.functions); i++ {
			result.functions[i] = result.functions[0]
		}
	}
	if result.functions == ([4]*ColorFunction{}) {
		return nil, nil
	}
	return &result, nil
}

// DeviceTransfer 合并原生设备通道的网屏传递函数，网屏定义优先于图形状态
// 入参: fallback 图形状态函数, halftone 网屏定义, model 原生设备颜色空间
// 返回: *TransferFunction 有效函数, error 通道或函数错误
func (r *Reader) DeviceTransfer(fallback *TransferFunction, halftone *Halftone, model Name) (*TransferFunction, error) {
	if r.closed {
		return nil, os.ErrClosed
	}
	var colorants [4]Name
	switch model {
	case "DeviceRGB":
		colorants = [4]Name{"Red", "Green", "Blue"}
	case "DeviceCMYK":
		colorants = [4]Name{"Cyan", "Magenta", "Yellow", "Black"}
	case "DeviceGray":
		colorants[3] = "Gray"
	default:
		return nil, fmt.Errorf("invalid transfer device space %q", model)
	}
	if halftone == nil || halftone.Type == 0 {
		return fallback.deviceChannels(colorants), nil
	}
	var result *TransferFunction
	functions := make(map[*Halftone]*TransferFunction)
	for channel, colorant := range colorants {
		if colorant == "" {
			continue
		}
		part, err := halftone.component(colorant)
		if err != nil {
			return nil, err
		}
		if part.Type == 0 || part.TransferFunction == nil {
			continue
		}
		function, found := functions[part]
		if !found {
			var defined bool
			function, defined, err = r.halftoneTransfer(part)
			if err != nil {
				return nil, err
			}
			if !defined {
				continue
			}
			functions[part] = function
		}
		if result == nil {
			result = &TransferFunction{}
			if fallback != nil {
				*result = *fallback
			}
		}
		result.functions[channel] = nil
		if function != nil {
			result.functions[channel] = function.functions[0]
		}
	}
	if result == nil {
		result = fallback
	}
	return result.deviceChannels(colorants), nil
}

// deviceChannels 排除设备未使用的传递通道，有效函数保持原对象
// 入参: colorants 标准设备分量及对应通道
// 返回: *TransferFunction 有效设备函数，空值为恒等
func (f *TransferFunction) deviceChannels(colorants [4]Name) *TransferFunction {
	if f != nil {
		for channel, colorant := range colorants {
			if colorant != "" && f.functions[channel] != nil {
				return f
			}
		}
	}
	return nil
}

// ColorantTransfer 选择原生色料的传递函数，非主色使用灰度回退
// 分量按加色形式求值，专色浓度在求值前后取补值
// 入参: fallback 图形状态函数, halftone 网屏定义, colorant 原生分量名称
// 返回: *TransferFunction 各通道相同的独立函数，空值为恒等, error 分量或函数错误
func (r *Reader) ColorantTransfer(fallback *TransferFunction, halftone *Halftone, colorant Name) (*TransferFunction, error) {
	if r.closed {
		return nil, os.ErrClosed
	}
	if colorant == "" || colorant == "All" || colorant == "None" || colorant == "Default" {
		return nil, fmt.Errorf("invalid transfer colorant %q", colorant)
	}
	channel, primary := transferChannel(colorant)
	part, err := halftone.component(colorant)
	if err != nil {
		return nil, err
	}
	if part != nil && part.Type != 0 {
		if halftone.Type == 5 && !primary {
			if err := r.defaultColorantTransfer(halftone, nil); err != nil {
				return nil, err
			}
		}
		function, defined, err := r.halftoneTransfer(part)
		if err != nil {
			return nil, err
		}
		if halftone.Type == 5 && !primary && !defined {
			return nil, fmt.Errorf("missing nonprimary halftone transfer function")
		}
		if defined {
			return function, nil
		}
	}
	if fallback == nil {
		return nil, nil
	}
	function := fallback.functions[channel]
	if function == nil {
		return nil, nil
	}
	return &TransferFunction{functions: [4]*ColorFunction{function, function, function, function}}, nil
}

// defaultColorantTransfer 检查非主色设备必需的类型5默认传递函数
// 入参: h 分量网屏, named 设备命名网屏
// 返回: error 默认网屏或函数错误
func (r *Reader) defaultColorantTransfer(h *Halftone, named map[string]*Halftone) error {
	part := h.Components["Default"]
	if part == nil {
		return fmt.Errorf("missing default halftone component")
	}
	if replacement := named[string(part.Name)]; part.Name != nil && replacement != nil {
		part = replacement
	}
	if part.Type == 5 {
		return fmt.Errorf("invalid default halftone component")
	}
	if part.Type == 0 {
		return nil
	}
	_, defined, err := r.halftoneTransfer(part)
	if err != nil {
		return err
	}
	if !defined {
		return fmt.Errorf("missing nonprimary default transfer function")
	}
	return nil
}

// halftoneTransfer 读取网屏的单通道覆盖函数，不接受图形状态的函数数组
// 入参: h 网屏定义
// 返回: *TransferFunction 覆盖函数, bool 是否显式定义, error 引用或函数错误
func (r *Reader) halftoneTransfer(h *Halftone) (*TransferFunction, bool, error) {
	if h == nil || h.Type == 0 || h.TransferFunction == nil {
		return nil, false, nil
	}
	if h.reader != nil {
		r = h.reader
	}
	value, err := r.Resolve(h.TransferFunction)
	if err != nil || value == nil {
		return nil, false, err
	}
	if _, array := value.(Array); array {
		return nil, false, fmt.Errorf("invalid halftone transfer function")
	}
	function, err := r.ReadTransferFunction(value)
	return function, true, err
}

// transferChannel 区分标准主色通道及非主色的灰度回退
// 入参: colorant 原生分量名称
// 返回: int 传递通道, bool 是否标准主色
func transferChannel(colorant Name) (int, bool) {
	switch colorant {
	case "Red", "Cyan":
		return 0, true
	case "Green", "Magenta":
		return 1, true
	case "Blue", "Yellow":
		return 2, true
	case "Gray", "Black":
		return 3, true
	}
	return 3, false
}

// SelectChannels 保留不透明通道的函数，其余通道使用恒等设备默认值
// 入参: channels 红绿蓝灰通道选择，四色设备依次对应青品黄黑
// 返回: *TransferFunction 只读函数，未改变时复用原对象，全恒等时为空
func (f *TransferFunction) SelectChannels(channels [4]bool) *TransferFunction {
	if f == nil {
		return nil
	}
	var result TransferFunction
	for i, selected := range channels {
		if selected {
			result.functions[i] = f.functions[i]
		}
	}
	if result.functions == ([4]*ColorFunction{}) {
		return nil
	}
	if result.functions == f.functions {
		return f
	}
	return &result
}

// Evaluate 计算指定通道的加色分量，输出裁切至单位范围
// 入参: value 单位输入值, channel 红绿蓝灰通道序号0至3
// 返回: float64 加色输出值, error 输入或求值错误
func (f *TransferFunction) Evaluate(value float64, channel int) (float64, error) {
	if channel < 0 || channel > 3 || math.IsNaN(value) || math.IsInf(value, 0) || value < 0 || value > 1 {
		return 0, fmt.Errorf("invalid transfer function input")
	}
	if f == nil {
		return value, nil
	}
	value, err := f.functions[channel].Evaluate(value)
	if err != nil {
		return 0, err
	}
	return math.Max(0, math.Min(1, value)), nil
}

// Apply 对原生设备分量应用传递函数，减色分量在求值前后取补值
// 入参: values 颜色转换后的单位分量, model DeviceGray、DeviceRGB或DeviceCMYK, source 原始颜色空间族
// 返回: [4]float64 原生设备输出分量, error 分量或求值错误
func (f *TransferFunction) Apply(values []float64, model, source Name) ([4]float64, error) {
	var result [4]float64
	components := 0
	switch model {
	case "DeviceGray":
		components = 1
	case "DeviceRGB":
		components = 3
	case "DeviceCMYK":
		components = 4
	default:
		return result, fmt.Errorf("invalid transfer device space %q", model)
	}
	if len(values) != components {
		return result, fmt.Errorf("invalid transfer component count")
	}
	for i, value := range values {
		if math.IsNaN(value) || math.IsInf(value, 0) || value < 0 || value > 1 {
			return [4]float64{}, fmt.Errorf("invalid transfer device component")
		}
		result[i] = value
		if model == "DeviceCMYK" && source == "DeviceGray" && i < 3 {
			continue
		}
		channel := i
		if model == "DeviceGray" {
			channel = 3
		}
		if model == "DeviceCMYK" {
			value = 1 - value
		}
		value, err := f.Evaluate(value, channel)
		if err != nil {
			return [4]float64{}, err
		}
		if model == "DeviceCMYK" {
			value = 1 - value
		}
		result[i] = value
	}
	return result, nil
}
