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
	if m == nil || math.IsNaN(value) || math.IsInf(value, 0) {
		return 0, fmt.Errorf("invalid mask transfer input")
	}
	value = math.Max(0, math.Min(1, value))
	if m.transfer == nil {
		return value, nil
	}
	values, err := m.transfer.evaluate(value)
	if err != nil {
		return 0, err
	}
	value = values[0]
	if math.IsNaN(value) || math.IsInf(value, 0) {
		return 0, fmt.Errorf("invalid mask transfer result")
	}
	return math.Max(0, math.Min(1, value)), nil
}

// Walk 访问蒙版图元，坐标与引用蒙版的页面一致
// 入参: visitor 蒙版图元访问器
// 返回: error 解析或访问错误
func (m *SoftMask) Walk(visitor Visitor) error {
	if m == nil {
		return fmt.Errorf("invalid soft mask")
	}
	ctx := m.interpreter.ctx
	if ctx == nil {
		ctx = context.Background()
	}
	return m.WalkContext(ctx, visitor)
}

// WalkContext 使用本次取消上下文访问蒙版图元，不修改原始解析状态
// 入参: ctx 取消上下文, visitor 蒙版图元访问器
// 返回: error 解析、访问或取消错误
func (m *SoftMask) WalkContext(ctx context.Context, visitor Visitor) error {
	if ctx == nil {
		return fmt.Errorf("invalid soft mask context")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if m == nil || m.stream == nil {
		return fmt.Errorf("invalid soft mask group")
	}
	p := m.interpreter
	p.ctx = ctx
	p.forms, p.content = nil, nil
	if visitor.Reference == nil {
		visitor.Reference = p.visitor.Reference
	}
	if visitor.Halftones == nil {
		visitor.Halftones = p.visitor.Halftones
	}
	p.visitor = visitor
	p.maskGroup = m.stream
	return p.form(m.stream)
}

// readSoftMask 读取蒙版颜色空间、背景及图形，不执行页面合成
// 入参: value 蒙版字典
// 返回: *SoftMask 蒙版信息, error 错误信息
func (p *pageInterpreter) readSoftMask(value Object) (*SoftMask, error) {
	dict, ok := value.(Dictionary)
	if !ok {
		return nil, &UnsupportedError{Feature: "soft mask subtype"}
	}
	v, err := p.reader.Resolve(dict["S"])
	if err != nil {
		return nil, err
	}
	subtype, ok := v.(Name)
	if !ok || subtype != Name("Luminosity") && subtype != Name("Alpha") {
		return nil, &UnsupportedError{Feature: "soft mask subtype"}
	}
	v, err = p.reader.Resolve(dict["G"])
	if err != nil {
		return nil, err
	}
	stream, ok := v.(*Stream)
	if !ok || stream == nil {
		return nil, fmt.Errorf("invalid soft mask group")
	}
	v, err = p.reader.Resolve(stream.Dictionary["Subtype"])
	if err != nil {
		return nil, err
	}
	if v != Name("Form") {
		return nil, fmt.Errorf("invalid soft mask group")
	}
	v, err = p.reader.Resolve(stream.Dictionary["Group"])
	if err != nil {
		return nil, err
	}
	group, ok := v.(Dictionary)
	if !ok {
		return nil, fmt.Errorf("invalid soft mask transparency group")
	}
	v, err = p.reader.Resolve(group["S"])
	if err != nil {
		return nil, err
	}
	if v != Name("Transparency") {
		return nil, fmt.Errorf("invalid soft mask transparency group")
	}
	m := &SoftMask{Subtype: subtype, interpreter: *p, stream: stream}
	m.interpreter.forms, m.interpreter.content = nil, nil
	v, err = p.reader.Resolve(group["CS"])
	if err != nil {
		return nil, err
	}
	if v != nil {
		resources := p.resources
		object, err := p.reader.Resolve(stream.Dictionary["Resources"])
		if err != nil {
			return nil, err
		}
		if object != nil {
			var ok bool
			resources, ok = object.(Dictionary)
			if !ok {
				return nil, fmt.Errorf("invalid soft mask resources")
			}
		}
		m.ColorSpace, err = p.reader.resourceBlendingSpace(v, resources)
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
		if profile := m.ColorSpace.profile; profile != nil {
			values := profile.normalize(m.Backdrop)
			copy(m.Backdrop, values[:])
		}
	}
	m.interpreter.state.style.SoftMask = nil
	m.interpreter.state.style.BlendMode = "Normal"
	m.interpreter.state.style.Clips = nil
	m.interpreter.state.style.Fill.Alpha, m.interpreter.state.style.Stroke.Alpha = 1, 1
	if m.Subtype == "Luminosity" {
		v, err := p.reader.Resolve(dict["BC"])
		if err != nil {
			return nil, err
		}
		if v != nil {
			n, err := p.reader.numberArray(v, m.ColorSpace.Components())
			if err != nil {
				return nil, err
			}
			if profile := m.ColorSpace.profile; profile != nil {
				ranges := profile.sourceRanges()
				for i, value := range n {
					if math.IsNaN(value) || math.IsInf(value, 0) || value < ranges[2*i] || value > ranges[2*i+1] {
						return nil, fmt.Errorf("invalid soft mask backdrop")
					}
				}
				values := profile.normalize(n)
				copy(n, values[:])
			}
			if m.ColorSpace.validate(n) != nil {
				return nil, fmt.Errorf("invalid soft mask backdrop")
			}
			m.Backdrop = n
		}
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
