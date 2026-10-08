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

// ColorantProcess 保存完全原生的过程通道映射，不包含备用色或独立专色
// 创建后可并发复用，不持有调用方浓度缓冲
type ColorantProcess struct {
	inputs     [4]int
	components int
	complement bool
	alternate  bool
}

// PrepareProcess 编译无需备用变换的过程通道，不能等价映射时返回nil
// 入参: device 输出设备, group 组混合空间，nil继承设备, softMask 是否用于软蒙版
// 返回: *ColorantProcess 原生映射，需备用色或独立专色时为nil
func (s *ColorantSpace) PrepareProcess(device *ColorantDevice, group *ColorSpace, softMask bool) *ColorantProcess {
	if s == nil || device == nil || device.Space == nil || !device.Space.Device() || softMask || len(s.Names) == 0 {
		return nil
	}
	if group == nil {
		group = device.Space
	}
	if !group.Device() || !group.Equal(device.Space) {
		return nil
	}
	p := &ColorantProcess{inputs: [4]int{-1, -1, -1, -1}, components: len(s.Names)}
	if s.separation != nil && s.Names[0] == "All" {
		if len(device.Spots) != 0 {
			return nil
		}
		for i := range group.Components() {
			p.inputs[i] = 0
		}
		p.complement = group.Model != "DeviceCMYK"
		p.alternate = p.matchesAlternate(s, group)
		return p
	}
	if s.NChannel {
		if s.Process == nil || s.lab != nil || !group.Equal(s.Process.Space) || len(s.Process.Channels) != len(s.Names) {
			return nil
		}
		for i, channel := range s.Process.Channels {
			if channel < 0 || channel >= group.Components() || p.inputs[channel] >= 0 {
				return nil
			}
			p.inputs[channel] = i
		}
		p.alternate = p.matchesAlternate(s, group)
		return p
	}
	if group.Model != "DeviceCMYK" || s.separation == nil && s.tint == nil {
		return nil
	}
	for i, name := range s.Names {
		if name == "None" {
			continue
		}
		channel := nChannelCMYKComponent(name)
		if channel < 0 || p.inputs[channel] >= 0 {
			return nil
		}
		p.inputs[channel] = i
	}
	p.alternate = p.matchesAlternate(s, group)
	return p
}

// AlternateEquivalent 判断是否已证明备用变换与原生过程分量完全等价
// 返回: bool 同空间且变换等价，未能证明时为false，不代表一定存在色差
func (p *ColorantProcess) AlternateEquivalent() bool {
	return p != nil && p.alternate
}

// matchesAlternate 按已编译的仿射系数或精确线性分段核对映射，不采用取样近似
// 入参: source 源色料定义, group 原生过程空间
// 返回: bool 是否证明整个浓度区间内等价
func (p *ColorantProcess) matchesAlternate(source *ColorantSpace, group *ColorSpace) bool {
	var function *gradientFunction
	if s := source.separation; s != nil {
		if !group.Equal(s.space) || !s.space.Device() || s.nested != nil || s.lab != nil || s.none {
			return false
		}
		function = s.transform
	} else if s := source.tint; s != nil {
		if !group.Equal(s.alternate) || !s.alternate.Device() || s.nested != nil || s.lab != nil || s.none || s.sampled != nil || s.program != nil {
			return false
		}
		function = s.transform
		if function == nil {
			if len(s.expressions) != group.Components() || len(s.input) != p.components*2 || len(s.output) != group.Components()*2 {
				return false
			}
			for i := range p.components {
				if s.input[2*i] > 0 || s.input[2*i+1] < 1 {
					return false
				}
			}
			for c, expression := range s.expressions {
				if len(expression) != p.components+1 || expression[0] != 0 || s.output[2*c] > 0 || s.output[2*c+1] < 1 {
					return false
				}
				for i, coefficient := range expression[1:] {
					want := 0.0
					if p.inputs[c] == i {
						want = 1
					}
					if coefficient != want {
						return false
					}
				}
			}
			return !p.complement
		}
	}
	if p.components != 1 || function == nil || function.linear == nil {
		return false
	}
	stops := clipGradientValues(function.linear([2]float64{0, 1}), gradientUnitBounds(group.Components()))
	if len(stops) < 2 || stops[0].Position != 0 || stops[len(stops)-1].Position != 1 {
		return false
	}
	for _, stop := range stops {
		values, err := p.Convert([]float64{stop.Position})
		if err != nil {
			return false
		}
		for c := range group.Components() {
			if values[c] != math.Max(0, math.Min(1, stop.Values[c])) {
				return false
			}
		}
	}
	return true
}

// Mask 返回显式指定的过程通道，零浓度仍视为已指定
// 返回: [4]bool 过程通道标记
func (p *ColorantProcess) Mask() [4]bool {
	var mask [4]bool
	if p != nil && p.components > 0 {
		for i, input := range p.inputs {
			mask[i] = input >= 0
		}
	}
	return mask
}

// Convert 将原始浓度写入原生过程分量，不执行备用颜色反推
// 入参: tints 源浓度，与色料定义等长
// 返回: [4]float64 过程空间分量, error 分量或非有限值错误
func (p *ColorantProcess) Convert(tints []float64) ([4]float64, error) {
	var values [4]float64
	if p == nil || p.components < 1 || len(tints) != p.components {
		return values, fmt.Errorf("invalid process colorant component count")
	}
	for _, value := range tints {
		if math.IsNaN(value) || math.IsInf(value, 0) {
			return values, fmt.Errorf("invalid process colorant tint")
		}
	}
	for i, input := range p.inputs {
		if input >= 0 {
			values[i] = math.Max(0, math.Min(1, tints[input]))
			if p.complement {
				values[i] = 1 - values[i]
			}
		}
	}
	return values, nil
}
