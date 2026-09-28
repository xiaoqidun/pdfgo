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

// SoftMask 保存透明度或亮度蒙版及其传递函数
type SoftMask struct {
	Subtype     Name
	ColorSpace  *ColorSpace
	Backdrop    []float64
	transfer    *gradientFunction
	interpreter pageInterpreter
	stream      *Stream
}

// Transfer 将蒙版透明度或亮度映射到最终不透明度
// 入参: value 蒙版采样值
// 返回: float64 限定在零至一之间的不透明度, error 传递函数错误
func (m *SoftMask) Transfer(value float64) (float64, error) {
	if math.IsNaN(value) || math.IsInf(value, 0) {
		return 0, fmt.Errorf("invalid mask transfer input")
	}
	value = math.Max(0, math.Min(1, value))
	if m.transfer != nil {
		values, err := m.transfer.evaluate(value)
		if err != nil {
			return 0, err
		}
		value = values[0]
	}
	return math.Max(0, math.Min(1, value)), nil
}

// Walk 访问蒙版图元，坐标与引用蒙版的页面一致
// 入参: visitor 蒙版图元访问器
// 返回: error 解析或访问错误
func (m *SoftMask) Walk(visitor Visitor) error {
	p := m.interpreter
	p.visitor = visitor
	return p.form(m.stream)
}

// readSoftMask 读取蒙版颜色空间、背景及图形，不执行页面合成
// 入参: value 蒙版字典
// 返回: *SoftMask 蒙版信息, error 错误信息
func (p *pageInterpreter) readSoftMask(value Object) (*SoftMask, error) {
	dict, ok := value.(Dictionary)
	if !ok || dict["S"] != Name("Luminosity") && dict["S"] != Name("Alpha") {
		return nil, &UnsupportedError{Feature: "soft mask subtype"}
	}
	v, err := p.reader.Resolve(dict["G"])
	if err != nil {
		return nil, err
	}
	stream, ok := v.(*Stream)
	if !ok || stream.Dictionary["Subtype"] != Name("Form") {
		return nil, fmt.Errorf("invalid soft mask group")
	}
	v, err = p.reader.Resolve(stream.Dictionary["Group"])
	if err != nil {
		return nil, err
	}
	group, ok := v.(Dictionary)
	if !ok || group["S"] != Name("Transparency") {
		return nil, fmt.Errorf("invalid soft mask transparency group")
	}
	m := &SoftMask{Subtype: dict["S"].(Name), interpreter: *p, stream: stream}
	if group["CS"] != nil {
		m.ColorSpace, err = p.reader.readBlendingSpace(group["CS"])
		if err != nil {
			return nil, err
		}
	} else if m.Subtype == "Luminosity" {
		return nil, fmt.Errorf("missing luminosity mask color space")
	}
	if m.ColorSpace != nil {
		m.Backdrop = make([]float64, m.ColorSpace.Components())
		if m.ColorSpace.Model == "DeviceCMYK" {
			m.Backdrop[3] = 1
		}
	}
	m.interpreter.state.style.SoftMask = nil
	m.interpreter.state.style.BlendMode = "Normal"
	m.interpreter.state.style.Clips = nil
	m.interpreter.state.style.Fill.Alpha, m.interpreter.state.style.Stroke.Alpha = 1, 1
	if dict["BC"] != nil && m.Subtype == "Luminosity" {
		v, err := p.reader.Resolve(dict["BC"])
		if err != nil {
			return nil, err
		}
		a, ok := v.(Array)
		if !ok {
			return nil, fmt.Errorf("invalid soft mask backdrop")
		}
		n, err := numbers(a, m.ColorSpace.Components())
		if err != nil || m.ColorSpace.validate(n) != nil {
			return nil, fmt.Errorf("invalid soft mask backdrop")
		}
		m.Backdrop = n
	}
	transfer, err := p.reader.Resolve(dict["TR"])
	if err != nil {
		return nil, err
	}
	if transfer != nil && transfer != Name("Identity") {
		m.transfer, err = p.reader.readGradientFunction(transfer, 1, 0)
		if err != nil {
			return nil, err
		}
	}
	return m, nil
}
