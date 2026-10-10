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
	"image"
	"image/color"
	"math"
)

// imageComponentSample 保留超过四色的原始图像分量
type imageComponentSample interface {
	color.Color
	component(int) uint16
}

// packedDeviceNImage 按行保留任意数量专色的紧凑样本
type packedDeviceNImage struct {
	data                    []byte
	rect                    image.Rectangle
	stride, depth, channels int
}

// packedDeviceNSample 按需读取专色像素，不截断原始分量
type packedDeviceNSample struct {
	source *packedDeviceNImage
	x, y   int
}

// jpxDeviceNSample 按需读取JPEG2000专色及独立透明度
type jpxDeviceNSample struct {
	source *jpxSampleImage
	x, y   int
}

// resampledDeviceNSample 保留重采样后的全部专色分量及透明度
type resampledDeviceNSample struct {
	samples [4]imageComponentSample
	weights [4]float64
}

// ColorModel 返回原始分量预览使用的颜色模型
// 返回: color.Model 颜色模型
func (s *packedDeviceNImage) ColorModel() color.Model { return color.NRGBA64Model }

// Bounds 返回专色图像边界
// 返回: image.Rectangle 图像边界
func (s *packedDeviceNImage) Bounds() image.Rectangle { return s.rect }

// At 读取原始专色样本，区域外返回透明色
// 入参: x 横向坐标, y 纵向坐标
// 返回: color.Color 样本颜色
func (s *packedDeviceNImage) At(x, y int) color.Color {
	if !image.Pt(x, y).In(s.rect) {
		return color.NRGBA64{}
	}
	return packedDeviceNSample{source: s, x: x, y: y}
}

// component 将紧凑样本归一化为十六位分量
// 入参: channel 分量索引
// 返回: uint16 样本值
func (s packedDeviceNSample) component(channel int) uint16 {
	line := s.source.data[(s.y-s.source.rect.Min.Y)*s.source.stride:]
	value := packedSample(line, int64(s.x-s.source.rect.Min.X)*int64(s.source.channels)+int64(channel), s.source.depth)
	return uint16(uint32(value) * 65535 / ((uint32(1) << s.source.depth) - 1))
}

// RGBA 返回前三个原始分量预览，不应用专色着色函数
// 返回: uint32 红、绿、蓝及透明度
func (s packedDeviceNSample) RGBA() (uint32, uint32, uint32, uint32) {
	return uint32(s.component(0)), uint32(s.component(1)), uint32(s.component(2)), 65535
}

// component 读取容器映射后的专色分量
// 入参: channel 分量索引
// 返回: uint16 样本值
func (s jpxDeviceNSample) component(channel int) uint16 {
	return s.source.sample(s.source.channels[channel], s.x, s.y)
}

// RGBA 返回原始分量预览及独立透明度
// 返回: uint32 红、绿、蓝及透明度
func (s jpxDeviceNSample) RGBA() (uint32, uint32, uint32, uint32) {
	alpha := uint32(65535)
	if s.source.alpha >= 0 {
		alpha = uint32(s.source.sample(s.source.alpha, s.x, s.y))
	}
	return uint32(s.component(0)) * alpha / 65535, uint32(s.component(1)) * alpha / 65535, uint32(s.component(2)) * alpha / 65535, alpha
}

// component 对原始专色浓度执行双线性插值
// 入参: channel 分量索引
// 返回: uint16 插值样本
func (s resampledDeviceNSample) component(channel int) uint16 {
	value := 0.0
	for n, sample := range s.samples {
		value += float64(sample.component(channel)) * s.weights[n]
	}
	return uint16(math.Round(value))
}

// RGBA 对预乘预览和透明度执行双线性插值
// 返回: uint32 红、绿、蓝及透明度
func (s resampledDeviceNSample) RGBA() (uint32, uint32, uint32, uint32) {
	var values [4]float64
	for n, sample := range s.samples {
		r, g, b, a := sample.RGBA()
		for c, value := range [4]uint32{r, g, b, a} {
			values[c] += float64(value) * s.weights[n]
		}
	}
	return uint32(math.Round(values[0])), uint32(math.Round(values[1])), uint32(math.Round(values[2])), uint32(math.Round(values[3]))
}
