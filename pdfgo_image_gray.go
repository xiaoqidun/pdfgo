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
	"math"
)

// decodeGrayImage 在独立灰度样本缓冲上应用Decode，不扩展为RGBA
// 入参: samples 解码器创建的样本, lower 映射起点, upper 映射终点
// 返回: image.Image 保留精度的灰度图像，不支持的样本类型为空, error 尺寸错误
func decodeGrayImage(samples image.Image, lower, upper float64) (image.Image, error) {
	bounds := samples.Bounds()
	if source, ok := samples.(*image.Gray16); ok {
		if lower != 0 || upper != 1 {
			for y := 0; y < bounds.Dy(); y++ {
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
	var out8 *image.Gray
	var out16 *image.Gray16
	if byteExact {
		out8 = source
		if out8 == nil {
			if _, _, err := imageBufferSize(bounds.Dx(), bounds.Dy(), 1); err != nil {
				return nil, err
			}
			out8 = image.NewGray(bounds)
		}
	} else {
		if _, _, err := imageBufferSize(bounds.Dx(), bounds.Dy(), 2); err != nil {
			return nil, err
		}
		out16 = image.NewGray16(bounds)
	}
	for y := 0; y < bounds.Dy(); y++ {
		var row, output []byte
		if source != nil {
			row = source.Pix[y*source.Stride:]
		} else {
			row = packed.data[y*packed.stride:]
		}
		if out8 != nil {
			output = out8.Pix[y*out8.Stride:]
		} else {
			output = out16.Pix[y*out16.Stride:]
		}
		for x := 0; x < bounds.Dx(); x++ {
			var index uint16
			if packed != nil {
				index = packedSample(row, x, packed.depth)
			} else {
				index = uint16(row[x])
			}
			value := lookup[index]
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
