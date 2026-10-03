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

// selectColorSpace 设置已解析的颜色空间及初始颜色，保留透明度
// 入参: object 有效颜色空间, fill 是否填充色
// 返回: error 定义或初始颜色错误
func (p *pageInterpreter) selectColorSpace(object Object, fill bool) error {
	name := colorSpaceFamily(object)
	var profile *iccColorSpace
	var separation *separationSpace
	var deviceN *deviceNSpace
	var patternBase *patternColorSpace
	var calibrated *graphicsColorSpace
	array, _ := object.(Array)
	var err error
	switch name {
	case "DeviceGray", "DeviceRGB", "DeviceCMYK":
		if array != nil && len(array) != 1 {
			return fmt.Errorf("invalid device color space")
		}
	case "Pattern":
		if len(array) == 2 {
			patternBase, err = p.reader.readPatternColorSpace(array[1], true)
		} else if array != nil && len(array) != 1 {
			return fmt.Errorf("invalid Pattern color space")
		}
	case "Lab", "CalRGB", "CalGray", "Indexed":
		calibrated = &graphicsColorSpace{displaySpace: ColorSpace{Model: "DeviceRGB", mapped: true}}
		switch name {
		case "Lab":
			calibrated.lab, err = p.reader.readLab(array)
		case "CalRGB":
			calibrated.calRGB, err = p.reader.readCalRGB(array)
		case "CalGray":
			calibrated.gray = true
			calibrated.calRGB, err = p.reader.readCalGray(array)
		case "Indexed":
			image := &Image{reader: p.reader, ColorSpace: array, Stream: &Stream{Dictionary: Dictionary{"Intent": p.state.style.RenderingIntent}}, effectiveColorSpace: true}
			calibrated.palette, err = image.palette()
		}
	case "DeviceN":
		deviceN, err = p.reader.readDeviceNSpace(array, true, 0)
	case "Separation":
		separation, err = p.reader.readSeparationSpace(array, true, 0)
	case "ICCBased":
		profile, err = p.reader.readICCColorSpace(array)
	default:
		return &UnsupportedError{Feature: "color space " + string(name)}
	}
	if err != nil {
		return err
	}
	paint := Paint{SourceSpace: name}
	switch {
	case name == "DeviceCMYK":
		paint.CMYK = &[4]float64{0, 0, 0, 1}
	case calibrated != nil:
		values := []float64{0, 0, 0}
		if calibrated.palette != nil || calibrated.gray {
			values = values[:1]
		}
		paint, err = calibrated.paint(values)
	case profile != nil:
		paint, err = profile.paint(make([]float64, profile.components()), p.state.style.RenderingIntent)
	case deviceN != nil:
		values := make([]float64, deviceN.components)
		for i := range values {
			values[i] = 1
		}
		paint, err = deviceN.paint(values, p.state.style.RenderingIntent)
	case separation != nil:
		paint, err = separation.paint(1, p.state.style.RenderingIntent)
	}
	if err != nil {
		return err
	}
	if fill {
		paint.Alpha = p.state.style.Fill.Alpha
		p.state.fillSpace, p.state.fillPatternBase, p.state.fillICC = name, patternBase, profile
		p.state.fillSeparation, p.state.fillDeviceN, p.state.fillColor = separation, deviceN, calibrated
		p.state.style.Fill = paint
	} else {
		paint.Alpha = p.state.style.Stroke.Alpha
		p.state.strokeSpace, p.state.strokePatternBase, p.state.strokeICC = name, patternBase, profile
		p.state.strokeSeparation, p.state.strokeDeviceN, p.state.strokeColor = separation, deviceN, calibrated
		p.state.style.Stroke = paint
	}
	return nil
}

// deviceColor 设置已选设备空间的颜色，不重复选择当前资源中的默认空间
// 入参: operands 原始设备分量, fill 是否填充色
// 返回: error 分量错误
func (p *pageInterpreter) deviceColor(operands []Object, fill bool) error {
	name := p.state.fillSpace
	if !fill {
		name = p.state.strokeSpace
	}
	count := (&ColorSpace{Model: name}).Components()
	values, err := numbers(operands, count)
	if err != nil {
		return err
	}
	return p.setDeviceColor(values, name, fill)
}

// setDeviceColor 写入有限设备分量并清除先前颜色空间状态
// 入参: values 设备分量, name 设备空间, fill 是否填充色
// 返回: error 设备空间错误
func (p *pageInterpreter) setDeviceColor(values []float64, name Name, fill bool) error {
	alpha := p.state.style.Fill.Alpha
	if !fill {
		alpha = p.state.style.Stroke.Alpha
	}
	for i := range values {
		values[i] = math.Max(0, math.Min(1, values[i]))
	}
	paint := Paint{SourceSpace: name, Alpha: alpha}
	switch len(values) {
	case 1:
		paint.RGB = [3]float64{values[0], values[0], values[0]}
	case 3:
		copy(paint.RGB[:], values)
	case 4:
		paint.CMYK = &[4]float64{values[0], values[1], values[2], values[3]}
	default:
		return fmt.Errorf("invalid device color space")
	}
	if fill {
		p.state.fillSpace, p.state.fillPatternBase, p.state.fillICC = name, nil, nil
		p.state.fillSeparation, p.state.fillDeviceN, p.state.fillColor = nil, nil, nil
		p.state.style.Fill = paint
	} else {
		p.state.strokeSpace, p.state.strokePatternBase, p.state.strokeICC = name, nil, nil
		p.state.strokeSeparation, p.state.strokeDeviceN, p.state.strokeColor = nil, nil, nil
		p.state.style.Stroke = paint
	}
	return nil
}
