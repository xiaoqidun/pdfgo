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

// ColorantGroup 保存只读组混合空间和输出设备，过程色转换不改变原生专色身份
type ColorantGroup struct {
	Space  *ColorSpace
	Device *ColorantDevice
}

// ColorantGroupCompositor 保存只读组合成参数，复用空间校验和通道布局
type ColorantGroupCompositor struct {
	source, target               ColorantGroup
	components, targetComponents int
	spots                        int
	sameSpace                    bool
	mode, intent                 Name
	conversion                   ColorConversion
}

// ColorantPixel 保存非预乘过程色及专色，所有通道共用Alpha、Shape和Effect
// Effect为不含初始背景的累计透明度，隔离组使用零透明度初值
type ColorantPixel struct {
	Values []float64
	Alpha  float64
	Shape  float64
	Effect float64
}

// NewColorantGroup 建立组混合定义，不将设备专色转换为组过程色
// 入参: space 组空间，nil继承设备或使用设备RGB, device 输出设备，nil仅处理过程色
// 返回: *ColorantGroup 组定义, error 空间或设备错误
func NewColorantGroup(space *ColorSpace, device *ColorantDevice) (*ColorantGroup, error) {
	if space == nil {
		space = &ColorSpace{Model: "DeviceRGB"}
		if device != nil {
			space = device.Space
		}
	}
	group := &ColorantGroup{Space: space, Device: device}
	if err := group.validate("Normal"); err != nil {
		return nil, err
	}
	return group, nil
}

// Components 返回组过程色及设备专色的总分量数
// 返回: int 分量数，无效定义为零
func (g *ColorantGroup) Components() int {
	if g == nil || g.Space == nil {
		return 0
	}
	return g.Space.Components() + len(g.spots())
}

// Composite 在组空间合成过程色和独立专色，未指定通道按无色料计算
// 入参: out 输出缓冲，可与backdrop相同, backdrop 非预乘背景, source 已在组空间求值的色料, backdropAlpha 背景透明度, sourceAlpha 源透明度, mode 混合模式
// 返回: float64 结果透明度, error 定义或数值错误，错误时不修改输出
func (g *ColorantGroup) Composite(out, backdrop []float64, source ColorantResult, backdropAlpha, sourceAlpha float64, mode Name) (float64, error) {
	return g.composite(out, backdrop, source, backdropAlpha, sourceAlpha, mode, false)
}

// CompositeOverprint 在组空间应用基本对象兼容套印，不将此规则用于整组结果
// 入参: out 输出缓冲，可与backdrop相同, backdrop 非预乘背景, source 已选择套印通道的色料, backdropAlpha 背景透明度, sourceAlpha 源透明度, mode 混合模式
// 返回: float64 结果透明度, error 定义或数值错误，错误时不修改输出
func (g *ColorantGroup) CompositeOverprint(out, backdrop []float64, source ColorantResult, backdropAlpha, sourceAlpha float64, mode Name) (float64, error) {
	return g.composite(out, backdrop, source, backdropAlpha, sourceAlpha, mode, true)
}

// CompositeGroup 移除组初始背景后合成到父组，专色只参与混合而不参与空间转换
// 入参: target 父组累计结果, initial 源组初始背景, result 源组累计结果, source 源组定义, opacity 组不透明度及蒙版乘积, mode 混合模式, intent 渲染意图, conversion 设备转换函数
// 返回: error 定义或数值错误，错误时不修改输出；非隔离组须继承父空间
func (g *ColorantGroup) CompositeGroup(target *ColorantPixel, initial, result ColorantPixel, source *ColorantGroup, opacity float64, mode, intent Name, conversion ColorConversion) error {
	compositor, err := g.PrepareGroup(source, mode, intent, conversion)
	if err != nil {
		return err
	}
	return compositor.Composite(target, initial, result, opacity)
}

// PrepareGroup 校验跨组混合定义，逐像素复用过程空间和原生专色布局
// 入参: source 源组定义, mode 混合模式, intent 渲染意图, conversion 设备转换函数
// 返回: ColorantGroupCompositor 只读合成器, error 空间、模式或设备错误
func (g *ColorantGroup) PrepareGroup(source *ColorantGroup, mode, intent Name, conversion ColorConversion) (ColorantGroupCompositor, error) {
	if err := g.validate(mode); err != nil {
		return ColorantGroupCompositor{}, err
	}
	if err := source.validate("Normal"); err != nil {
		return ColorantGroupCompositor{}, err
	}
	if !g.sameDevice(source) {
		return ColorantGroupCompositor{}, fmt.Errorf("mismatched group spot colorants")
	}
	return ColorantGroupCompositor{source: *source, target: *g, components: source.Space.Components(), targetComponents: g.Space.Components(), spots: len(g.spots()), sameSpace: g.Space.Equal(source.Space), mode: mode, intent: intent, conversion: conversion}, nil
}

// Composite 将已准备的源组结果合成到父组，错误时不修改当前输出
// 入参: target 父组结果, initial 源组初始背景, result 源组累计结果, opacity 组不透明度及蒙版乘积
// 返回: error 分量、透明度或空间转换错误
func (c *ColorantGroupCompositor) Composite(target *ColorantPixel, initial, result ColorantPixel, opacity float64) error {
	if c == nil || c.source.Space == nil || c.target.Space == nil {
		return fmt.Errorf("invalid group compositor")
	}
	source, g := c.source, c.target
	if !colorantUnit(opacity) || target == nil {
		return fmt.Errorf("invalid group opacity or target")
	}
	if err := target.validate(c.targetComponents + c.spots); err != nil {
		return err
	}
	if err := initial.validateColor(c.components + c.spots); err != nil {
		return err
	}
	if err := result.validate(c.components + c.spots); err != nil {
		return err
	}
	if initial.Alpha != 0 && !c.sameSpace {
		return fmt.Errorf("non-isolated colorant group must inherit parent space")
	}
	alpha := result.Effect * opacity
	if alpha == 0 {
		target.Shape += result.Shape * (1 - target.Shape)
		return nil
	}
	components, parentComponents := c.components, c.targetComponents
	var process [4]float64
	for c := 0; c < components; c++ {
		process[c] = colorantGroupComponent(initial, result, c)
	}
	if !c.sameSpace {
		var err error
		process, err = g.Space.ConvertWith(process[:components], source.Space, c.intent, c.conversion)
		if err != nil {
			return err
		}
		if err := g.Space.validate(process[:parentComponents]); err != nil {
			return err
		}
	}
	var backdrop [4]float64
	if target.Alpha != 0 {
		copy(backdrop[:], target.Values[:parentComponents])
	}
	values, combined := g.Space.compositeValues(backdrop, process, target.Alpha, alpha, c.mode)
	spots := c.spots
	for j := range spots {
		index := j
		if parentComponents > components {
			index = spots - j - 1
		}
		b := 0.0
		if target.Alpha != 0 {
			b = target.Values[parentComponents+index]
		}
		s := colorantGroupComponent(initial, result, components+index)
		target.Values[parentComponents+index] = compositeComponent(b, s, colorantSpotBlend(b, s, c.mode), target.Alpha, alpha, combined)
	}
	copy(target.Values[:parentComponents], values[:parentComponents])
	target.Alpha = combined
	target.Effect = alpha + target.Effect*(1-alpha)
	target.Shape += result.Shape * (1 - target.Shape)
	return nil
}

// Interpolate 按形状覆盖混合预乘颜色和累计透明度，形状由调用方独立累计
// 入参: source 候选结果, shape 覆盖率
// 返回: error 分量或数值错误，错误时不修改输出
func (p *ColorantPixel) Interpolate(source ColorantPixel, shape float64) error {
	if p == nil || !colorantUnit(shape) {
		return fmt.Errorf("invalid colorant interpolation")
	}
	if err := p.validate(len(source.Values)); err != nil {
		return err
	}
	if err := source.validate(len(p.Values)); err != nil {
		return err
	}
	alpha := p.Alpha*(1-shape) + source.Alpha*shape
	if alpha != 0 {
		for c := range p.Values {
			p.Values[c] = (colorantPremultiplied(*p, c)*(1-shape) + colorantPremultiplied(source, c)*shape) / alpha
		}
	}
	p.Alpha = alpha
	p.Effect = p.Effect*(1-shape) + source.Effect*shape
	return nil
}

// Knockout 按对象形状移除之前的贡献，不将透明度当作覆盖率
// 入参: initial 组初始背景, source 已与初始背景合成的候选结果
// 返回: error 分量或数值错误，错误时不修改输出
func (p *ColorantPixel) Knockout(initial, source ColorantPixel) error {
	if p == nil {
		return fmt.Errorf("invalid knockout target")
	}
	for _, pixel := range [...]ColorantPixel{*p, initial, source} {
		if err := pixel.validate(len(p.Values)); err != nil {
			return err
		}
	}
	f := source.Shape
	if f == 0 {
		return nil
	}
	alpha := p.Alpha*(1-f) + source.Alpha - initial.Alpha*(1-f)
	if alpha > 0 {
		for c := range p.Values {
			p.Values[c] = math.Max(0, math.Min(1, (colorantPremultiplied(*p, c)*(1-f)+colorantPremultiplied(source, c)-colorantPremultiplied(initial, c)*(1-f))/alpha))
		}
	}
	p.Alpha = math.Max(0, math.Min(1, alpha))
	p.Effect = p.Effect*(1-f) + source.Effect
	p.Shape += f * (1 - p.Shape)
	return nil
}

// composite 合成组过程色与原生专色，兼容套印先计算未标记通道的隐式组贡献
// 入参: out 输出缓冲, backdrop 背景分量, source 源色料, backdropAlpha 背景透明度, sourceAlpha 源透明度, mode 混合模式, overprint 是否兼容套印
// 返回: float64 结果透明度, error 定义或数值错误
func (g *ColorantGroup) composite(out, backdrop []float64, source ColorantResult, backdropAlpha, sourceAlpha float64, mode Name, overprint bool) (float64, error) {
	components, marked, err := g.validateComposite(out, backdrop, source, backdropAlpha, sourceAlpha, overprint)
	if err != nil {
		return 0, err
	}
	if err := g.Space.validateBlend(mode); err != nil {
		return 0, err
	}
	if !marked && (!overprint || source.Process == nil) || sourceAlpha == 0 {
		if backdropAlpha == 0 {
			clear(out)
		} else {
			copy(out, backdrop)
		}
		return backdropAlpha, nil
	}
	var process [4]float64
	for c := 0; c < components; c++ {
		if g.Space.Model != "DeviceCMYK" {
			process[c] = 1
		}
		if sourceAlpha != 0 && source.ProcessMask[c] {
			process[c] = source.Process.Values[c]
		}
	}
	var values [4]float64
	var alpha float64
	if overprint {
		values, alpha, err = g.Space.CompositeOverprint(backdrop[:components], process[:components], backdropAlpha, sourceAlpha, mode, source.ProcessMask[:components])
	} else {
		values, alpha, err = g.Space.Composite(backdrop[:components], process[:components], backdropAlpha, sourceAlpha, mode)
	}
	if err != nil {
		return 0, err
	}
	for c := range g.spots() {
		b, s := 0.0, 0.0
		if backdropAlpha != 0 {
			b = backdrop[components+c]
		}
		if len(source.SpotMask) != 0 && source.SpotMask[c] {
			s = source.Spots[c]
		} else if overprint {
			s = overprintComponent(b, backdropAlpha, 0)
		}
		out[components+c] = compositeComponent(b, s, colorantSpotBlend(b, s, mode), backdropAlpha, sourceAlpha, alpha)
	}
	copy(out[:components], values[:components])
	return alpha, nil
}

// validateComposite 校验组输出和独立通道，零透明度不读取未定义颜色
// 入参: out 输出缓冲, backdrop 背景分量, source 源色料, backdropAlpha 背景透明度, sourceAlpha 源透明度, overprint 是否忽略未标记源分量
// 返回: int 过程分量数, bool 是否指定色料, error 定义或数值错误
func (g *ColorantGroup) validateComposite(out, backdrop []float64, source ColorantResult, backdropAlpha, sourceAlpha float64, overprint bool) (int, bool, error) {
	if g == nil || g.Space == nil || g.Device != nil && (g.Device.Space == nil || !g.Device.Space.Device() || len(g.Device.Spots) != 0 && g.Device.Space.Model != "DeviceCMYK") {
		return 0, false, fmt.Errorf("invalid colorant group")
	}
	components := g.Space.Components()
	if components == 0 || len(out) != components+len(g.spots()) || len(backdrop) != len(out) {
		return 0, false, fmt.Errorf("invalid device colorant buffer")
	}
	if len(source.Alternates) != 0 {
		return 0, false, &UnsupportedError{Feature: "colorant alternate compositing"}
	}
	for _, alpha := range [...]float64{backdropAlpha, sourceAlpha} {
		if math.IsNaN(alpha) || math.IsInf(alpha, 0) || alpha < 0 || alpha > 1 {
			return 0, false, fmt.Errorf("invalid compositing alpha")
		}
	}
	if backdropAlpha != 0 {
		for _, value := range backdrop {
			if math.IsNaN(value) || math.IsInf(value, 0) || value < 0 || value > 1 {
				return 0, false, fmt.Errorf("invalid device colorant backdrop")
			}
		}
	}
	if source.Process != nil {
		if !g.Space.Equal(source.Process.Space) {
			return 0, false, fmt.Errorf("mismatched native process color space")
		}
		if sourceAlpha != 0 {
			values := source.Process.Values
			if overprint {
				for c := 0; c < components; c++ {
					if !source.ProcessMask[c] {
						values[c] = 0
					}
				}
			}
			if err := g.Space.validate(values[:components]); err != nil {
				return 0, false, err
			}
		}
	}
	if len(source.Spots) != len(source.SpotMask) || len(source.Spots) != 0 && len(source.Spots) != len(g.spots()) {
		return 0, false, fmt.Errorf("invalid native spot colorants")
	}
	marked := false
	for c, mask := range source.ProcessMask {
		if mask && (c >= components || source.Process == nil) {
			return 0, false, fmt.Errorf("invalid native process mask")
		}
		marked = marked || mask
	}
	for c, value := range source.Spots {
		if sourceAlpha != 0 && (!overprint || source.SpotMask[c]) && (math.IsNaN(value) || math.IsInf(value, 0) || value < 0 || value > 1) {
			return 0, false, fmt.Errorf("invalid native spot tint")
		}
		marked = marked || source.SpotMask[c]
	}
	return components, marked, nil
}

// validate 检查组混合空间、原生设备和当前混合模式
// 入参: mode 混合模式
// 返回: error 定义错误
func (g *ColorantGroup) validate(mode Name) error {
	if g == nil || g.Device != nil && (g.Device.Space == nil || !g.Device.Space.Device() || len(g.Device.Spots) != 0 && g.Device.Space.Model != "DeviceCMYK") {
		return fmt.Errorf("invalid colorant group")
	}
	return g.Space.validateBlend(mode)
}

// validate 检查共享透明参数，零透明度不读取未定义颜色
// 入参: components 期望分量数
// 返回: error 分量或数值错误
func (p ColorantPixel) validate(components int) error {
	if !colorantUnit(p.Shape) || !colorantUnit(p.Effect) {
		return fmt.Errorf("invalid colorant pixel")
	}
	return p.validateColor(components)
}

// validateColor 校验有效颜色，不读取不参与背景移除的形状和累计透明度
// 入参: components 期望分量数
// 返回: error 分量或数值错误
func (p ColorantPixel) validateColor(components int) error {
	if components == 0 || len(p.Values) != components || !colorantUnit(p.Alpha) {
		return fmt.Errorf("invalid colorant pixel")
	}
	if p.Alpha != 0 {
		for _, value := range p.Values {
			if !colorantUnit(value) {
				return fmt.Errorf("invalid colorant pixel component")
			}
		}
	}
	return nil
}

// spots 返回只读设备专色顺序，不为过程色组分配额外列表
// 返回: []Name 设备专色
func (g *ColorantGroup) spots() []Name {
	if g.Device == nil {
		return nil
	}
	return g.Device.Spots
}

// sameDevice 检查跨组专色身份和顺序，允许没有专色的独立过程空间
// 入参: source 源组定义
// 返回: bool 专色是否匹配
func (g *ColorantGroup) sameDevice(source *ColorantGroup) bool {
	names, other := g.spots(), source.spots()
	if len(names) != len(other) {
		return false
	}
	if len(names) == 0 || g.Device == source.Device {
		return true
	}
	if !g.Device.Space.Equal(source.Device.Space) {
		return false
	}
	for c, name := range names {
		if name != other[c] {
			return false
		}
	}
	return true
}

// colorantUnit 判断透明参数或色料值是否在有限单位范围
// 入参: value 待校验值
// 返回: bool 是否有效
func colorantUnit(value float64) bool {
	return value >= 0 && value <= 1
}

// colorantPremultiplied 读取预乘分量，透明颜色不参与计算
// 入参: pixel 像素, component 通道下标
// 返回: float64 预乘分量
func colorantPremultiplied(pixel ColorantPixel, component int) float64 {
	if pixel.Alpha == 0 {
		return 0
	}
	return pixel.Values[component] * pixel.Alpha
}

// colorantGroupComponent 移除初始背景贡献并还原单位组颜色
// 入参: initial 初始背景, result 累计结果, component 通道下标
// 返回: float64 组分量
func colorantGroupComponent(initial, result ColorantPixel, component int) float64 {
	if initial.Alpha == 0 && result.Alpha == result.Effect {
		return result.Values[component]
	}
	return math.Max(0, math.Min(1, (colorantPremultiplied(result, component)-colorantPremultiplied(initial, component)*(1-result.Effect))/result.Effect))
}

// colorantSpotBlend 按减色规则混合专色，指定模式按标准替换为Normal
// 入参: backdrop 背景浓度, source 源浓度, mode 过程色混合模式
// 返回: float64 专色混合浓度
func colorantSpotBlend(backdrop, source float64, mode Name) float64 {
	switch mode {
	case "", "Normal", "Compatible", "Difference", "Exclusion", "Hue", "Saturation", "Color", "Luminosity":
		return source
	}
	return 1 - separableBlend(1-backdrop, 1-source, mode)
}
