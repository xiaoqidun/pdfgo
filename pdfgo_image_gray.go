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
	"image/color"
	"math"
)

// mappedGrayImage 在紧凑灰度样本上按需应用Decode映射
type mappedGrayImage struct {
	source    *packedGrayImage
	lookup    [256]uint16
	byteExact bool
}

// ColorModel 返回映射结果可无损表达的灰度模型
// 返回: color.Model 颜色模型
func (s *mappedGrayImage) ColorModel() color.Model {
	if s.byteExact {
		return color.GrayModel
	}
	return color.Gray16Model
}

// Bounds 返回原始采样边界
// 返回: image.Rectangle 图像边界
func (s *mappedGrayImage) Bounds() image.Rectangle { return s.source.rect }

// Opaque 返回灰度图像的不透明状态
// 返回: bool 是否不透明
func (s *mappedGrayImage) Opaque() bool { return true }

// At 读取映射后的灰度，边界外返回零值
// 入参: x 横向坐标, y 纵向坐标
// 返回: color.Color 灰度颜色
func (s *mappedGrayImage) At(x, y int) color.Color {
	value := s.Gray16At(x, y)
	if s.byteExact {
		return color.Gray{Y: byte(value.Y >> 8)}
	}
	return value
}

// Gray16At 直接读取映射后的十六位灰度
// 入参: x 横向坐标, y 纵向坐标
// 返回: color.Gray16 灰度颜色
func (s *mappedGrayImage) Gray16At(x, y int) color.Gray16 {
	if !image.Pt(x, y).In(s.Bounds()) {
		return color.Gray16{}
	}
	line := s.source.data[(y-s.source.rect.Min.Y)*s.source.stride:]
	return color.Gray16{Y: s.lookup[packedSample(line, x-s.source.rect.Min.X, s.source.depth)]}
}

// NRGBA64At 直接读取非预乘颜色，避免逐像素颜色接口转换
// 入参: x 横向坐标, y 纵向坐标
// 返回: color.NRGBA64 非预乘颜色
func (s *mappedGrayImage) NRGBA64At(x, y int) color.NRGBA64 {
	value := s.Gray16At(x, y).Y
	return color.NRGBA64{R: value, G: value, B: value, A: 65535}
}

// RGBA64At 直接读取预乘颜色，保留灰度精度
// 入参: x 横向坐标, y 纵向坐标
// 返回: color.RGBA64 预乘颜色
func (s *mappedGrayImage) RGBA64At(x, y int) color.RGBA64 {
	value := s.Gray16At(x, y).Y
	return color.RGBA64{R: value, G: value, B: value, A: 65535}
}

// decodeGrayImageContext 应用灰度映射，紧凑样本保持延迟展开
// 入参: ctx 取消上下文, samples 独立样本, lower 映射起点, upper 映射终点
// 返回: image.Image 灰度图像，不支持的样本类型为空, error 尺寸或取消错误
func decodeGrayImageContext(ctx context.Context, samples image.Image, lower, upper float64) (image.Image, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if source, ok := samples.(*jpxSampleImage); ok && source.rows != nil {
		return &rowGrayImage{source: source, lower: lower, upper: upper, byteExact: imageByteExact(source) && (lower == 0 && upper == 1 || lower == 1 && upper == 0)}, nil
	}
	bounds := samples.Bounds()
	if source, ok := samples.(*image.Gray16); ok {
		if lower != 0 || upper != 1 {
			for y := 0; y < bounds.Dy(); y++ {
				if err := ctx.Err(); err != nil {
					return nil, err
				}
				row := source.Pix[y*source.Stride:]
				for x := 0; x < bounds.Dx(); x++ {
					value := uint16(row[2*x])<<8 | uint16(row[2*x+1])
					value = uint16(math.Round(math.Max(0, math.Min(1, functionValue(float64(value)/65535, lower, upper))) * 65535))
					row[2*x], row[2*x+1] = byte(value>>8), byte(value)
				}
			}
		}
		return source, nil
	}
	var source *image.Gray
	var packed *packedGrayImage
	maximum := 255
	switch value := samples.(type) {
	case *image.Gray:
		if lower == 0 && upper == 1 {
			return value, nil
		}
		source = value
	case *packedGrayImage:
		if value.depth > 8 {
			return nil, nil
		}
		packed = value
		maximum = 1<<value.depth - 1
	default:
		return nil, nil
	}
	var lookup [256]uint16
	byteExact := true
	for n := 0; n <= maximum; n++ {
		value := float64(uint32(n)*65535/uint32(maximum)) / 65535
		lookup[n] = uint16(math.Round(math.Max(0, math.Min(1, functionValue(value, lower, upper))) * 65535))
		byteExact = byteExact && lookup[n]%257 == 0
	}
	if packed != nil {
		pixelBytes := 2
		if byteExact {
			pixelBytes = 1
		}
		if _, _, err := imageBufferSize(bounds.Dx(), bounds.Dy(), pixelBytes); err != nil {
			return nil, err
		}
		return &mappedGrayImage{source: packed, lookup: lookup, byteExact: byteExact}, nil
	}
	var out8 *image.Gray
	var out16 *image.Gray16
	if byteExact {
		out8 = source
	} else {
		if _, _, err := imageBufferSize(bounds.Dx(), bounds.Dy(), 2); err != nil {
			return nil, err
		}
		out16 = image.NewGray16(bounds)
	}
	for y := 0; y < bounds.Dy(); y++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		row := source.Pix[y*source.Stride:]
		var output []byte
		if out8 != nil {
			output = out8.Pix[y*out8.Stride:]
		} else {
			output = out16.Pix[y*out16.Stride:]
		}
		for x := 0; x < bounds.Dx(); x++ {
			value := lookup[row[x]]
			if out8 != nil {
				output[x] = byte(value >> 8)
			} else {
				output[2*x], output[2*x+1] = byte(value>>8), byte(value)
			}
		}
	}
	if out8 != nil {
		return out8, nil
	}
	return out16, nil
}
