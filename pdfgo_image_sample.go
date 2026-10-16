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

// imageNRGBAAt 直接读取八位非预乘颜色，保留全透明像素的隐藏颜色
// 入参: source 图像, x 横向坐标, y 纵向坐标
// 返回: color.NRGBA 非预乘颜色
func imageNRGBAAt(source image.Image, x, y int) color.NRGBA {
	if s, ok := source.(interface{ NRGBAAt(int, int) color.NRGBA }); ok {
		return s.NRGBAAt(x, y)
	}
	c := imageNRGBA64At(source, x, y)
	return color.NRGBA{R: uint8(c.R >> 8), G: uint8(c.G >> 8), B: uint8(c.B >> 8), A: uint8(c.A >> 8)}
}

// imageNativeAlpha 按行扫描原生透明度或调色板索引，保留十六位透明度精度
// 入参: ctx 取消上下文, source 图像, binary 是否检查含透明像素的二值透明度
// 返回: bool 分类结果, bool 是否支持原生扫描, error 取消错误
func imageNativeAlpha(ctx context.Context, source image.Image, binary bool) (bool, bool, error) {
	var pixels []byte
	var stride, step int
	wide := false
	switch s := source.(type) {
	case *pngGrayAlphaImage:
		return imageNativeAlpha(ctx, s.Image, binary)
	case *image.Paletted:
		result, err := imagePalettedAlpha(ctx, s, binary)
		return result, true, err
	case *image.NRGBA:
		pixels, stride, step = s.Pix, s.Stride, 4
	case *image.RGBA:
		pixels, stride, step = s.Pix, s.Stride, 4
	case *image.Alpha:
		pixels, stride, step = s.Pix, s.Stride, 1
	case *image.NRGBA64:
		pixels, stride, step, wide = s.Pix, s.Stride, 8, true
	case *image.RGBA64:
		pixels, stride, step, wide = s.Pix, s.Stride, 8, true
	case *image.Alpha16:
		pixels, stride, step, wide = s.Pix, s.Stride, 2, true
	case *image.NYCbCrA:
		pixels, stride, step = s.A, s.AStride, 1
	default:
		return false, false, nil
	}
	width, height := source.Bounds().Dx(), source.Bounds().Dy()
	interval := 4095
	if binary {
		interval = 255
	}
	transparent := false
	for y := 0; y < height; y++ {
		if width == 0 {
			break
		}
		row := pixels[y*stride:][:width*step]
		for x := 0; x < width; x++ {
			if x&interval == 0 {
				if err := ctx.Err(); err != nil {
					return false, true, err
				}
			}
			i := x*step + step - 1
			a := row[i]
			if wide && row[i-1] != a {
				return false, true, ctx.Err()
			}
			if a == 255 {
				continue
			}
			if !binary || a != 0 {
				return false, true, ctx.Err()
			}
			transparent = true
		}
	}
	return !binary || transparent, true, ctx.Err()
}

// imagePalettedAlpha 扫描实际使用的调色板索引，单次调用复用各索引的透明度分类
// 入参: ctx 取消上下文, source 索引图像, binary 是否检查含透明像素的二值透明度
// 返回: bool 分类结果, error 取消错误
func imagePalettedAlpha(ctx context.Context, source *image.Paletted, binary bool) (bool, error) {
	var states [256]uint8
	width, height := source.Rect.Dx(), source.Rect.Dy()
	interval := 4095
	if binary {
		interval = 255
	}
	transparent := false
	for y := 0; y < height && width != 0; y++ {
		row := source.Pix[y*source.Stride:][:width]
		for x, index := range row {
			if x&interval == 0 {
				if err := ctx.Err(); err != nil {
					return false, err
				}
			}
			state := states[index]
			if state == 0 {
				_, _, _, alpha := source.Palette[index].RGBA()
				switch alpha {
				case 65535:
					state = 1
				case 0:
					state = 2
				default:
					state = 3
				}
				states[index] = state
			}
			if state == 1 {
				continue
			}
			if !binary || state == 3 {
				return false, ctx.Err()
			}
			transparent = true
		}
	}
	return !binary || transparent, ctx.Err()
}

// imageOpaqueContext 分段检查透明度，已知无透明通道时不扫描颜色
// 入参: ctx 取消上下文, source 图像
// 返回: bool 是否完全不透明, error 取消错误
func imageOpaqueContext(ctx context.Context, source image.Image) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if result, native, err := imageNativeAlpha(ctx, source, false); native {
		return result, err
	}
	switch s := source.(type) {
	case *image.Gray, *image.Gray16, *image.CMYK, *image.YCbCr, *packedGrayImage, *mappedGrayImage, *packedCMYKImage:
		return true, nil
	case *jpxSampleImage:
		if s.alpha < 0 {
			return true, nil
		}
	case *deviceSampleImage:
		if s.mask == nil && !s.embedded && s.palette == nil {
			return true, nil
		}
	}
	bounds := source.Bounds()
	for y := bounds.Min.Y; y < bounds.Max.Y; y++ {
		for x := bounds.Min.X; x < bounds.Max.X; x++ {
			if (x-bounds.Min.X)&4095 == 0 {
				if err := ctx.Err(); err != nil {
					return false, err
				}
			}
			var alpha uint16
			switch s := source.(type) {
			case *jpxSampleImage:
				alpha = s.sample(s.alpha, x, y)
			case image.RGBA64Image:
				alpha = s.RGBA64At(x, y).A
			default:
				alpha = imageNRGBA64At(source, x, y).A
			}
			if alpha != 65535 {
				return false, ctx.Err()
			}
		}
	}
	return true, ctx.Err()
}

// imageNRGBA64At 优先使用直接采样接口，保留透明像素的隐藏颜色
// 入参: source 图像, x 横向坐标, y 纵向坐标
// 返回: color.NRGBA64 非预乘颜色
func imageNRGBA64At(source image.Image, x, y int) color.NRGBA64 {
	switch s := source.(type) {
	case interface{ NRGBA64At(int, int) color.NRGBA64 }:
		return s.NRGBA64At(x, y)
	case *image.NRGBA:
		c := s.NRGBAAt(x, y)
		return color.NRGBA64{R: uint16(c.R) * 257, G: uint16(c.G) * 257, B: uint16(c.B) * 257, A: uint16(c.A) * 257}
	case *image.CMYK:
		if !image.Pt(x, y).In(s.Bounds()) {
			return color.NRGBA64{}
		}
		return imageNRGBASample(s.CMYKAt(x, y))
	case *image.Paletted:
		return imageNRGBASample(s.At(x, y))
	case *packedGrayImage:
		if !image.Pt(x, y).In(s.rect) {
			return color.NRGBA64{}
		}
		value := uint32(packedSample(s.data[(y-s.rect.Min.Y)*s.stride:], int64(x-s.rect.Min.X), s.depth))
		v := uint16(value * 65535 / ((uint32(1) << s.depth) - 1))
		return color.NRGBA64{R: v, G: v, B: v, A: 65535}
	case image.RGBA64Image:
		c := s.RGBA64At(x, y)
		return imageUnpremultiply(c)
	default:
		return imageNRGBASample(source.At(x, y))
	}
}

// imageRGBA64At 直接取得预乘颜色，避免颜色模型转换分配
// 入参: source 图像, x 横向坐标, y 纵向坐标
// 返回: color.RGBA64 预乘颜色
func imageRGBA64At(source image.Image, x, y int) color.RGBA64 {
	if s, ok := source.(*image.CMYK); ok {
		pixel := imageNRGBA64At(s, x, y)
		return color.RGBA64{R: pixel.R, G: pixel.G, B: pixel.B, A: pixel.A}
	}
	if s, ok := source.(image.RGBA64Image); ok {
		return s.RGBA64At(x, y)
	}
	c := imageNRGBA64At(source, x, y)
	r, g, b, a := c.RGBA()
	return color.RGBA64{R: uint16(r), G: uint16(g), B: uint16(b), A: uint16(a)}
}

// imageUnpremultiply 恢复十六位非预乘颜色，与标准颜色模型的整数舍入一致
// 入参: c 预乘颜色
// 返回: color.NRGBA64 非预乘颜色
func imageUnpremultiply(c color.RGBA64) color.NRGBA64 {
	if c.A == 0 {
		return color.NRGBA64{}
	}
	if c.A == 65535 {
		return color.NRGBA64{R: c.R, G: c.G, B: c.B, A: c.A}
	}
	a := uint32(c.A)
	return color.NRGBA64{R: uint16(uint32(c.R) * 65535 / a), G: uint16(uint32(c.G) * 65535 / a), B: uint16(uint32(c.B) * 65535 / a), A: c.A}
}

// NRGBA64At 直接重采样显示颜色，原始四色和专色仍按原始分量插值
// 入参: x 横向坐标, y 纵向坐标
// 返回: color.NRGBA64 非预乘颜色
func (s *imageResample) NRGBA64At(x, y int) color.NRGBA64 {
	if !image.Pt(x, y).In(s.bounds) {
		return color.NRGBA64{}
	}
	b := s.source.Bounds()
	if b == s.bounds {
		return imageNRGBA64At(s.source, x, y)
	}
	if s.interpolate {
		return imageUnpremultiply(s.RGBA64At(x, y))
	}
	sx := (float64(x-s.bounds.Min.X)+.5)*float64(b.Dx())/float64(s.bounds.Dx()) - .5
	sy := (float64(y-s.bounds.Min.Y)+.5)*float64(b.Dy())/float64(s.bounds.Dy()) - .5
	return imageNRGBA64At(s.source, b.Min.X+min(b.Dx()-1, max(0, int(math.Floor(sx+.5)))), b.Min.Y+min(b.Dy()-1, max(0, int(math.Floor(sy+.5)))))
}

// RGBA64At 直接重采样预乘颜色，不经过反预乘往返，保留原始四色和专色插值
// 入参: x 横向坐标, y 纵向坐标
// 返回: color.RGBA64 预乘颜色
func (s *imageResample) RGBA64At(x, y int) color.RGBA64 {
	if !image.Pt(x, y).In(s.bounds) {
		return color.RGBA64{}
	}
	b := s.source.Bounds()
	if b == s.bounds {
		return imageRGBA64At(s.source, x, y)
	}
	if s.interpolate {
		switch source := s.source.(type) {
		case *image.CMYK, *packedCMYKImage, *packedDeviceNImage, *imageResample:
			return color.RGBA64Model.Convert(s.At(x, y)).(color.RGBA64)
		case *jpxSampleImage:
			if source.cmyk || len(source.channels) > 4 {
				return color.RGBA64Model.Convert(s.At(x, y)).(color.RGBA64)
			}
		}
	}
	sx := (float64(x-s.bounds.Min.X)+.5)*float64(b.Dx())/float64(s.bounds.Dx()) - .5
	sy := (float64(y-s.bounds.Min.Y)+.5)*float64(b.Dy())/float64(s.bounds.Dy()) - .5
	if !s.interpolate {
		return imageRGBA64At(s.source, b.Min.X+min(b.Dx()-1, max(0, int(math.Floor(sx+.5)))), b.Min.Y+min(b.Dy()-1, max(0, int(math.Floor(sy+.5)))))
	}
	x0, y0 := int(math.Floor(sx)), int(math.Floor(sy))
	fx, fy := sx-float64(x0), sy-float64(y0)
	var values [4]float64
	for dy := 0; dy < 2; dy++ {
		for dx := 0; dx < 2; dx++ {
			wx, wy := 1-fx, 1-fy
			if dx == 1 {
				wx = fx
			}
			if dy == 1 {
				wy = fy
			}
			pixel := imageRGBA64At(s.source, b.Min.X+min(b.Dx()-1, max(0, x0+dx)), b.Min.Y+min(b.Dy()-1, max(0, y0+dy)))
			for c, value := range [4]uint16{pixel.R, pixel.G, pixel.B, pixel.A} {
				values[c] += float64(value) * wx * wy
			}
		}
	}
	return color.RGBA64{R: uint16(math.Round(values[0])), G: uint16(math.Round(values[1])), B: uint16(math.Round(values[2])), A: uint16(math.Round(values[3]))}
}
