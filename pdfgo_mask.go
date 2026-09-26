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

// SoftMask 保存透明度或亮度蒙版及线性传递函数，Transfer为零值和单位值的输出
type SoftMask struct {
	Subtype     Name
	ColorSpace  *ColorSpace
	Backdrop    []float64
	Transfer    [2]float64
	interpreter pageInterpreter
	stream      *Stream
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
	m := &SoftMask{Subtype: dict["S"].(Name), Transfer: [2]float64{0, 1}, interpreter: *p, stream: stream}
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
	if dict["TR"] != nil && dict["TR"] != Name("Identity") {
		v, err := p.reader.Resolve(dict["TR"])
		if err != nil {
			return nil, err
		}
		function, ok := v.(*Stream)
		if !ok || function.Dictionary["FunctionType"] != Integer(4) {
			return nil, &UnsupportedError{Feature: "soft mask transfer function"}
		}
		for _, key := range []Name{"Domain", "Range"} {
			v, err := p.reader.Resolve(function.Dictionary[key])
			if err != nil {
				return nil, err
			}
			a, ok := v.(Array)
			if !ok {
				return nil, fmt.Errorf("invalid transfer function %s", key)
			}
			n, err := numbers(a, 2)
			if err != nil || n[0] != 0 || n[1] != 1 {
				return nil, &UnsupportedError{Feature: "transfer function domain or range"}
			}
		}
		data, err := function.Decode()
		if err != nil {
			return nil, err
		}
		m.Transfer, err = linearCalculator(data)
		if err != nil {
			return nil, err
		}
	}
	return m, nil
}

// linearCalculator 解析计算器函数的仿射运算，不将非线性函数近似为直线
// 入参: data 计算器函数内容
// 返回: [2]float64 函数端点值, error 非线性或语法错误
func linearCalculator(data []byte) ([2]float64, error) {
	values, err := affineCalculator(data, 1, 1)
	if err != nil {
		return [2]float64{}, err
	}
	end := values[0][0] + values[0][1]
	if math.IsNaN(end) || math.IsInf(end, 0) {
		return [2]float64{}, fmt.Errorf("nonfinite calculator result")
	}
	return [2]float64{values[0][0], end}, nil
}
