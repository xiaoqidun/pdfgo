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
	"image"
	"math"
)

// imageComponentView 在独立原始样本上应用设备色映射，不展开整幅十六位缓冲
type imageComponentView struct {
	source   image.Image
	mask     *imageResample
	channels int
	ranges   []float64
	keys     []float64
	matte    []float64
	inverted bool
	embedded bool
	maximum  float64
}

// DecodeComponentsView 读取原生分量的只读视图，设备色和模板图像保持按需访问
// Pix可为空，使用ValuesAt采样；其他空间保留备用、过程和专色分量
// 返回: *ImageComponents 只读颜色分量及透明度, error 解码错误
func (i *Image) DecodeComponentsView() (*ImageComponents, error) {
	return i.DecodeComponentsViewContext(context.Background())
}

// DecodeComponentsViewContext 解码原始样本并按需读取设备色，不保留取消上下文
// 入参: ctx 解码取消上下文
// 返回: *ImageComponents 只读颜色分量及透明度, error 解码或取消错误
func (i *Image) DecodeComponentsViewContext(ctx context.Context) (*ImageComponents, error) {
	result := &ImageComponents{}
	if _, err := i.decodeImage(ctx, result, true); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return result, nil
}

// valuesAt 应用Decode、色键与遮罩，保留与完整分量缓冲相同的十六位舍入
// 入参: x 横坐标, y 纵坐标
// 返回: [4]float64 原生非预乘颜色, float64 透明度
func (s *imageComponentView) valuesAt(x, y int) ([4]float64, float64) {
	var values [4]float64
	alpha := uint16(65535)
	if s.mask != nil {
		value := imageRGBA64At(s.mask, x, y).R
		if s.inverted {
			value = 65535 - value
		}
		alpha = value
	}
	if s.channels == 4 {
		pixel, _ := imageCMYKAt(s.source, x, y)
		for c, value := range pixel {
			values[c] = float64(value) / 65535
		}
		if s.embedded {
			_, _, _, value := s.source.At(x, y).RGBA()
			alpha = uint16(value)
		}
	} else {
		pixel := imageNRGBA64At(s.source, x, y)
		values = [4]float64{float64(pixel.R) / 65535, float64(pixel.G) / 65535, float64(pixel.B) / 65535}
		if s.embedded {
			alpha = pixel.A
		}
	}
	transparent := len(s.keys) != 0
	for c := 0; c < s.channels; c++ {
		if transparent {
			sample := math.Round(values[c] * s.maximum)
			transparent = sample >= s.keys[2*c] && sample <= s.keys[2*c+1]
		}
		value := math.Max(0, math.Min(1, functionValue(values[c], s.ranges[2*c], s.ranges[2*c+1])))
		if len(s.matte) != 0 {
			if alpha == 0 {
				value = s.matte[c]
			} else {
				value = math.Max(0, math.Min(1, s.matte[c]+(value-s.matte[c])*65535/float64(alpha)))
			}
		}
		values[c] = float64(uint16(math.Round(value*65535))) / 65535
	}
	clear(values[s.channels:])
	if transparent {
		alpha = 0
	}
	return values, float64(alpha) / 65535
}
