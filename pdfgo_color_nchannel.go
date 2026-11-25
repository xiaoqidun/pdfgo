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

// ProcessColorants 保存过程空间及源分量对应的过程通道，专色通道为-1，Channels切片只读
type ProcessColorants struct {
	Space    *ColorSpace
	Channels []int
}

// nChannelCMYKComponent 返回保留过程色的标准分量位置
// 入参: name 色料名称
// 返回: int 分量序号，非保留名称为-1
func nChannelCMYKComponent(name Name) int {
	switch name {
	case "Cyan":
		return 0
	case "Magenta":
		return 1
	case "Yellow":
		return 2
	case "Black":
		return 3
	}
	return -1
}

// CMYKMask 返回可直接在设备四色空间绘制的过程通道
// 返回: [4]bool 参与绘制的四色通道, bool 是否为未校准设备四色
func (p *ProcessColorants) CMYKMask() ([4]bool, bool) {
	var mask [4]bool
	if p == nil || p.Space == nil || p.Space.Model != "DeviceCMYK" || p.Space.Calibrated() {
		return mask, false
	}
	for _, channel := range p.Channels {
		if channel == -1 {
			continue
		}
		if channel < 0 || channel >= len(mask) {
			return [4]bool{}, false
		}
		mask[channel] = true
	}
	return mask, true
}

// applyNChannelProcess 按过程字典映射可直接输出的过程分量，保留备用着色函数校验
// 入参: ctx 取消上下文, space 多色定义, tint 已解析的备用变换, effective 是否已按资源校验并替换, depth 嵌套深度
// 返回: error 过程定义、预算或取消错误
func (r *Reader) applyNChannelProcess(ctx context.Context, space Array, tint *deviceNSpace, effective bool, depth int) error {
	ctx, _, err := consumeColorParseNode(ctx)
	if err != nil {
		return err
	}
	if len(space) != 5 {
		return nil
	}
	value, err := r.Resolve(space[4])
	if err != nil {
		return err
	}
	attrs := value.(Dictionary)
	subtype, err := r.Resolve(attrs["Subtype"])
	if err != nil || subtype != Name("NChannel") {
		return err
	}
	tint.colorants.NChannel = true
	names, _, err := r.deviceNColorants(space)
	if err != nil {
		return err
	}
	value, err = r.Resolve(attrs["Process"])
	if err != nil {
		return err
	}
	if value == nil {
		for _, name := range names {
			if nChannelCMYKComponent(name) >= 0 {
				return fmt.Errorf("missing NChannel process dictionary")
			}
		}
		tint.colorants.spots, err = r.readNChannelSpots(ctx, attrs, names, nil, effective, depth+1)
		return err
	}
	process, ok := value.(Dictionary)
	if !ok {
		return fmt.Errorf("invalid NChannel process dictionary")
	}
	output, lab, err := r.readDeviceNAlternateContext(ctx, process["ColorSpace"])
	if err != nil {
		return err
	}
	value, err = r.Resolve(process["Components"])
	if err != nil {
		return err
	}
	components, ok := value.(Array)
	if !ok || len(components) != output.Components() {
		return fmt.Errorf("invalid NChannel process components")
	}
	channels := make(map[Name]int, len(components)+4)
	for i, component := range components {
		value, err := r.Resolve(component)
		if err != nil {
			return err
		}
		name, ok := value.(Name)
		if _, exists := channels[name]; !ok || exists {
			return fmt.Errorf("invalid NChannel process component name")
		}
		if output.Model == "DeviceCMYK" {
			if index := nChannelCMYKComponent(name); index >= 0 && index != i {
				return fmt.Errorf("invalid NChannel CMYK component order")
			}
		}
		channels[name] = i
	}
	if output.Model == "DeviceCMYK" {
		for i, name := range [...]Name{"Cyan", "Magenta", "Yellow", "Black"} {
			channels[name] = i
		}
	}
	indices := make([]int, output.Components())
	for i := range indices {
		indices[i] = -1
	}
	spots, first := false, -1
	for i, name := range names {
		if output.Model != "DeviceCMYK" && nChannelCMYKComponent(name) >= 0 {
			return fmt.Errorf("multiple NChannel process color spaces")
		}
		channel, ok := channels[name]
		if !ok {
			spots = true
			continue
		}
		if indices[channel] >= 0 {
			return fmt.Errorf("duplicate NChannel process component")
		}
		indices[channel] = i
		if first < 0 {
			first = i
		}
	}
	if output.Model != "DeviceCMYK" && first >= 0 {
		for channel, index := range indices {
			if index != first+channel {
				return fmt.Errorf("incomplete or unordered NChannel process components")
			}
		}
	}
	tint.colorants.spots, err = r.readNChannelSpots(ctx, attrs, names, channels, effective, depth+1)
	if err != nil {
		return err
	}
	tint.colorants.Process = &ProcessColorants{Space: output, Channels: make([]int, len(names))}
	tint.colorants.lab = lab
	for i, name := range names {
		tint.colorants.Process.Channels[i] = -1
		if channel, ok := channels[name]; ok {
			tint.colorants.Process.Channels[i] = channel
		}
	}
	if spots {
		return nil
	}
	tint.alternate, tint.lab = output, lab
	tint.nested, tint.none = nil, false
	tint.process = &ProcessColorants{Space: output, Channels: make([]int, tint.components)}
	tint.input, tint.output = gradientUnitBounds(tint.components), gradientUnitBounds(output.Components())
	if lab != nil {
		tint.output = []float64{0, 100, lab.rangeAB[0], lab.rangeAB[1], lab.rangeAB[2], lab.rangeAB[3]}
	} else if output.profile != nil && output.profile.ranges != nil {
		ranges := output.profile.sourceRanges()
		tint.output = append([]float64(nil), ranges[:output.Components()*2]...)
	}
	tint.transform, tint.sampled, tint.program = nil, nil, nil
	tint.processInputs = nil
	expressions := make([]affineValue, output.Components())
	overflow := false
	for channel, input := range indices {
		low, high := tint.output[2*channel], tint.output[2*channel+1]
		expressions[channel] = make(affineValue, tint.components+1)
		expressions[channel][0] = low
		if input >= 0 {
			span := high - low
			if math.IsInf(span, 0) || math.IsNaN(span) {
				overflow = true
			} else {
				expressions[channel][input+1] = span
			}
			tint.process.Channels[input] = channel
		}
	}
	if overflow {
		tint.expressions = nil
		tint.processInputs = indices
	} else {
		tint.expressions = expressions
	}
	return nil
}

// readNChannelSpots 编译所用专色的独立分色定义，忽略过程分量的同名定义
// 入参: ctx 取消上下文, attrs 属性字典, names 色料名称, process 过程分量映射, effective 是否已按资源校验并替换, depth 嵌套深度
// 返回: []*separationSpace 按源分量保存的专色变换, error 专色定义、预算或取消错误
func (r *Reader) readNChannelSpots(ctx context.Context, attrs Dictionary, names []Name, process map[Name]int, effective bool, depth int) ([]*separationSpace, error) {
	ctx, state, err := consumeColorParseNode(ctx)
	if err != nil {
		return nil, err
	}
	var colorants Dictionary
	var spots []*separationSpace
	loaded := false
	for i, name := range names {
		if _, ok := process[name]; ok {
			continue
		}
		if !loaded {
			value, err := r.Resolve(attrs["Colorants"])
			if err != nil {
				return nil, err
			}
			var ok bool
			colorants, ok = value.(Dictionary)
			if !ok {
				return nil, fmt.Errorf("missing NChannel spot colorants")
			}
			loaded = true
			spots = make([]*separationSpace, len(names))
		}
		raw := colorants[name]
		ref, isRef := raw.(Reference)
		key := separationCacheKey{ref: ref, effective: effective}
		if isRef && state.separations != nil {
			if cached := state.separations[key]; cached != nil {
				if cached.name != name {
					return nil, fmt.Errorf("mismatched NChannel spot colorant %q", name)
				}
				spots[i] = cached
				continue
			}
		}
		value, err := r.resolveColorSpace(raw)
		if err != nil {
			return nil, err
		}
		definition, ok := value.(Array)
		if !ok || len(definition) != 4 || definition[0] != Name("Separation") {
			return nil, fmt.Errorf("invalid NChannel spot colorant %q", name)
		}
		value, err = r.Resolve(definition[1])
		if err != nil {
			return nil, err
		}
		if value != name {
			return nil, fmt.Errorf("mismatched NChannel spot colorant %q", name)
		}
		spots[i], err = r.readSeparationSpaceContext(ctx, definition, effective, depth)
		if err != nil {
			return nil, fmt.Errorf("NChannel spot colorant %q: %w", name, err)
		}
		if isRef {
			if state.separations == nil {
				state.separations = make(map[separationCacheKey]*separationSpace)
			}
			state.separations[key] = spots[i]
		}
	}
	return spots, nil
}
