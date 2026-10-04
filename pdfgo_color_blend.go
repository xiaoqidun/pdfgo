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
	"sync/atomic"
)

// Blend 按PDF标准混合同空间单位分量，四色在加色补色中计算，不执行空间转换
// 入参: backdrop 背景分量, source 源分量, mode 标准混合模式
// 返回: [4]float64 混合分量, error 空间、分量或模式错误
func (s *ColorSpace) Blend(backdrop, source []float64, mode Name) ([4]float64, error) {
	if err := s.validateBlend(mode); err != nil {
		return [4]float64{}, err
	}
	if err := s.validate(backdrop); err != nil {
		return [4]float64{}, err
	}
	if err := s.validate(source); err != nil {
		return [4]float64{}, err
	}
	var b, c [4]float64
	copy(b[:], backdrop)
	copy(c[:], source)
	return s.blend(b, c, mode), nil
}

// Composite 按PDF基本透明公式合成非预乘分量，透明颜色不参与求值，不处理组挖空或套印
// 入参: backdrop 背景分量, source 源分量, backdropAlpha 背景透明度, sourceAlpha 源透明度, mode 标准混合模式
// 返回: [4]float64 结果分量, float64 结果透明度, error 空间、分量、透明度或模式错误
func (s *ColorSpace) Composite(backdrop, source []float64, backdropAlpha, sourceAlpha float64, mode Name) ([4]float64, float64, error) {
	return s.composite(backdrop, source, backdropAlpha, sourceAlpha, mode, nil)
}

// CompositeOverprint 按兼容套印的隐式组规则合成基本对象，通道选择由源空间和套印模式决定
// 入参: backdrop 背景分量, source 源分量, backdropAlpha 背景透明度, sourceAlpha 源透明度, mode 当前混合模式, marked 已按套印规则选定的通道
// 返回: [4]float64 结果分量, float64 结果透明度, error 空间、分量、透明度、模式或通道错误
func (s *ColorSpace) CompositeOverprint(backdrop, source []float64, backdropAlpha, sourceAlpha float64, mode Name, marked []bool) ([4]float64, float64, error) {
	if s == nil || len(marked) != s.Components() {
		return [4]float64{}, 0, fmt.Errorf("invalid overprint component mask")
	}
	return s.composite(backdrop, source, backdropAlpha, sourceAlpha, mode, marked)
}

// composite 统一基本透明合成和兼容套印，不将未标记通道的值作为有效源颜色
// 入参: backdrop 背景分量, source 源分量, backdropAlpha 背景透明度, sourceAlpha 源透明度, mode 混合模式, marked 套印通道，nil执行普通合成
// 返回: [4]float64 结果分量, float64 结果透明度, error 定义或数值错误
func (s *ColorSpace) composite(backdrop, source []float64, backdropAlpha, sourceAlpha float64, mode Name, marked []bool) ([4]float64, float64, error) {
	var b, c [4]float64
	if err := s.validateBlend(mode); err != nil {
		return b, 0, err
	}
	if len(backdrop) != s.Components() || len(source) != s.Components() {
		return b, 0, fmt.Errorf("invalid compositing component count")
	}
	for _, alpha := range [...]float64{backdropAlpha, sourceAlpha} {
		if math.IsNaN(alpha) || math.IsInf(alpha, 0) || alpha < 0 || alpha > 1 {
			return b, 0, fmt.Errorf("invalid compositing alpha")
		}
	}
	if backdropAlpha != 0 {
		if err := s.validate(backdrop); err != nil {
			return b, 0, err
		}
		copy(b[:], backdrop)
	}
	if sourceAlpha != 0 {
		copy(c[:], source)
		if marked != nil {
			white := 1.0
			if s.Model == "DeviceCMYK" {
				white = 0
			}
			for i, mark := range marked {
				if !mark {
					c[i] = overprintComponent(b[i], backdropAlpha, white)
				}
			}
		}
		if err := s.validate(c[:s.Components()]); err != nil {
			return [4]float64{}, 0, err
		}
	}
	if sourceAlpha == 0 {
		return b, backdropAlpha, nil
	}
	values, alpha := s.compositeValues(b, c, backdropAlpha, sourceAlpha, mode)
	return values, alpha, nil
}

// compositeValues 合成已校验的非预乘颜色，不重复校验组内逐像素分量
// 入参: backdrop 背景值, source 源值, backdropAlpha 背景透明度, sourceAlpha 源透明度, mode 已校验模式
// 返回: [4]float64 结果分量, float64 结果透明度
func (s *ColorSpace) compositeValues(backdrop, source [4]float64, backdropAlpha, sourceAlpha float64, mode Name) ([4]float64, float64) {
	if backdropAlpha == 0 {
		return source, sourceAlpha
	}
	blend := s.blend(backdrop, source, mode)
	alpha := sourceAlpha + backdropAlpha*(1-sourceAlpha)
	for i := 0; i < s.Components(); i++ {
		backdrop[i] = compositeComponent(backdrop[i], source[i], blend[i], backdropAlpha, sourceAlpha, alpha)
	}
	return backdrop, alpha
}

// overprintComponent 移除兼容套印隐式非隔离组的背景贡献，保留未标记通道
// 入参: backdrop 背景分量, alpha 背景透明度, white 无色料值
// 返回: float64 供当前混合模式使用的组分量
func overprintComponent(backdrop, alpha, white float64) float64 {
	if alpha == 0 {
		return white
	}
	if alpha == 1 {
		return backdrop
	}
	return alpha*backdrop + (1-alpha)*white
}

// validateBlend 校验标准模式和混合模型，不将源Lab或特殊备用色作为混合空间
// 入参: mode 混合模式
// 返回: error 空间或模式错误
func (s *ColorSpace) validateBlend(mode Name) error {
	if s == nil || s.Components() == 0 {
		return fmt.Errorf("invalid blending color space")
	}
	if s.profile != nil && atomic.LoadUint32(&s.blending) == 0 {
		if err := s.profile.validateBlending(); err != nil {
			return err
		}
		atomic.StoreUint32(&s.blending, 1)
	}
	switch mode {
	case "", "Normal", "Compatible", "Multiply", "Screen", "Overlay", "Darken", "Lighten", "ColorDodge", "ColorBurn", "HardLight", "SoftLight", "Difference", "Exclusion", "Hue", "Saturation", "Color", "Luminosity":
		return nil
	}
	return &UnsupportedError{Feature: "blend mode " + string(mode)}
}

// blend 计算已校验分量的混合值，灰度和四色保留标准非分离规则
// 入参: backdrop 背景分量, source 源分量, mode 混合模式
// 返回: [4]float64 混合分量
func (s *ColorSpace) blend(backdrop, source [4]float64, mode Name) [4]float64 {
	if mode == "" || mode == "Normal" || mode == "Compatible" {
		return source
	}
	nonseparable := mode == "Hue" || mode == "Saturation" || mode == "Color" || mode == "Luminosity"
	if nonseparable {
		if s.Model == "DeviceGray" {
			if mode == "Luminosity" {
				return source
			}
			return backdrop
		}
		b, c := backdrop, source
		if s.Model == "DeviceCMYK" {
			for i := 0; i < 3; i++ {
				b[i], c[i] = 1-b[i], 1-c[i]
			}
		}
		result := b
		switch mode {
		case "Hue":
			result = blendSetLuminosity(blendSetSaturation(c, blendSaturation(b)), blendLuminosity(b))
		case "Saturation":
			result = blendSetLuminosity(blendSetSaturation(b, blendSaturation(c)), blendLuminosity(b))
		case "Color":
			result = blendSetLuminosity(c, blendLuminosity(b))
		case "Luminosity":
			result = blendSetLuminosity(b, blendLuminosity(c))
		}
		if s.Model == "DeviceCMYK" {
			for i := 0; i < 3; i++ {
				result[i] = 1 - result[i]
			}
			result[3] = backdrop[3]
			if mode == "Luminosity" {
				result[3] = source[3]
			}
		}
		return result
	}
	var result [4]float64
	for i := 0; i < s.Components(); i++ {
		b, c := backdrop[i], source[i]
		if s.Model == "DeviceCMYK" {
			b, c = 1-b, 1-c
		}
		result[i] = separableBlend(b, c, mode)
		if s.Model == "DeviceCMYK" {
			result[i] = 1 - result[i]
		}
	}
	return result
}

// compositeComponent 按非预乘透明公式合成一个已校验分量
// 入参: backdrop 背景值, source 源值, blend 混合值, backdropAlpha 背景透明度, sourceAlpha 源透明度, alpha 结果透明度
// 返回: float64 单位结果
func compositeComponent(backdrop, source, blend, backdropAlpha, sourceAlpha, alpha float64) float64 {
	if sourceAlpha == 1 && blend == source {
		return source
	}
	return math.Max(0, math.Min(1, ((1-sourceAlpha)*backdropAlpha*backdrop+sourceAlpha*((1-backdropAlpha)*source+backdropAlpha*blend))/alpha))
}

// blendLuminosity 计算非分离混合的加色亮度
// 入参: c 加色分量
// 返回: float64 亮度
func blendLuminosity(c [4]float64) float64 { return .3*c[0] + .59*c[1] + .11*c[2] }

// blendSaturation 计算非分离混合的加色饱和度
// 入参: c 加色分量
// 返回: float64 饱和度
func blendSaturation(c [4]float64) float64 {
	return math.Max(c[0], math.Max(c[1], c[2])) - math.Min(c[0], math.Min(c[1], c[2]))
}

// blendSetSaturation 保持分量顺序并调整饱和度
// 入参: c 加色分量, saturation 目标饱和度
// 返回: [4]float64 调整后分量
func blendSetSaturation(c [4]float64, saturation float64) [4]float64 {
	minimum := math.Min(c[0], math.Min(c[1], c[2]))
	span := blendSaturation(c)
	for i := 0; i < 3; i++ {
		if span == 0 {
			c[i] = 0
		} else {
			c[i] = (c[i] - minimum) * saturation / span
		}
	}
	return c
}

// blendSetLuminosity 调整亮度并按PDF规则压缩超界分量
// 入参: c 加色分量, luminosity 目标亮度
// 返回: [4]float64 调整后分量
func blendSetLuminosity(c [4]float64, luminosity float64) [4]float64 {
	delta := luminosity - blendLuminosity(c)
	for i := 0; i < 3; i++ {
		c[i] += delta
	}
	minimum, maximum := math.Min(c[0], math.Min(c[1], c[2])), math.Max(c[0], math.Max(c[1], c[2]))
	if minimum < 0 {
		for i := 0; i < 3; i++ {
			c[i] = luminosity + (c[i]-luminosity)*luminosity/(luminosity-minimum)
		}
	}
	if maximum > 1 {
		for i := 0; i < 3; i++ {
			c[i] = luminosity + (c[i]-luminosity)*(1-luminosity)/(maximum-luminosity)
		}
	}
	return c
}

// separableBlend 计算已校验模式的标准加色混合值
// 入参: backdrop 背景分量, source 源分量, mode 混合模式
// 返回: float64 混合分量
func separableBlend(backdrop, source float64, mode Name) float64 {
	switch mode {
	case "Multiply":
		return backdrop * source
	case "Screen":
		return backdrop + source - backdrop*source
	case "Overlay":
		if backdrop <= .5 {
			return 2 * backdrop * source
		}
		return 1 - 2*(1-backdrop)*(1-source)
	case "Darken":
		return math.Min(backdrop, source)
	case "Lighten":
		return math.Max(backdrop, source)
	case "ColorDodge":
		if backdrop == 0 {
			return 0
		}
		if source == 1 {
			return 1
		}
		return math.Min(1, backdrop/(1-source))
	case "ColorBurn":
		if backdrop == 1 {
			return 1
		}
		if source == 0 {
			return 0
		}
		return 1 - math.Min(1, (1-backdrop)/source)
	case "HardLight":
		if source <= .5 {
			return 2 * backdrop * source
		}
		return 1 - 2*(1-backdrop)*(1-source)
	case "SoftLight":
		if source <= .5 {
			return backdrop - (1-2*source)*backdrop*(1-backdrop)
		}
		d := math.Sqrt(backdrop)
		if backdrop <= .25 {
			d = ((16*backdrop-12)*backdrop + 4) * backdrop
		}
		return backdrop + (2*source-1)*(d-backdrop)
	case "Difference":
		return math.Abs(backdrop - source)
	case "Exclusion":
		return backdrop + source - 2*backdrop*source
	}
	return source
}
