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

import "fmt"

// ColorConverter 保存颜色空间及转换参数的只读快照，可用于并发分量转换
type ColorConverter struct {
	destination, source ColorSpace
	intent              Name
	conversion          ColorConversion
	same                bool
	ready               bool
}

// PrepareConversion 准备源空间到当前空间的转换，不近似配置文件或颜色函数
// 入参: source 源空间, intent 渲染意图, conversion 设备转换函数
// 返回: ColorConverter 只读转换器, error 无效空间
func (s *ColorSpace) PrepareConversion(source *ColorSpace, intent Name, conversion ColorConversion) (ColorConverter, error) {
	if s.Components() == 0 || source.Components() == 0 {
		return ColorConverter{}, fmt.Errorf("invalid color conversion space")
	}
	return ColorConverter{
		destination: ColorSpace{Model: s.Model, profile: s.profile, mapped: s.mapped},
		source:      ColorSpace{Model: source.Model, profile: source.profile, mapped: source.mapped},
		intent:      normalizeRenderingIntent(intent),
		conversion:  conversion,
		same:        s.Equal(source),
		ready:       true,
	}, nil
}

// Convert 验证源分量并使用已准备的空间转换，不保留输入或共享求值缓冲
// 入参: values 源颜色分量
// 返回: [4]float64 目标分量, error 分量或变换错误
func (c *ColorConverter) Convert(values []float64) ([4]float64, error) {
	if c == nil || !c.ready {
		return [4]float64{}, fmt.Errorf("unprepared color converter")
	}
	if err := c.source.validate(values); err != nil {
		return [4]float64{}, err
	}
	return c.destination.convert(values, &c.source, c.intent, c.conversion, c.same)
}
