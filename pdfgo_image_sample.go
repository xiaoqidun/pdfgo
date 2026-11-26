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
	"image"
	"image/color"
	"math"
)

// imageUnwrapper 允许包装图像暴露底层原生图像以启用快速路径
type imageUnwrapper interface {
	Unwrap() image.Image
}

// unwrapImage 解包包装图像直至得到最内层有效图像
// 入参: source 源图像
// 返回: image.Image 解包后的图像
func unwrapImage(source image.Image) image.Image {
	for range 16 {
		wrapper, ok := source.(imageUnwrapper)
		if !ok {
			return source
		}
		inner := wrapper.Unwrap()
		if inner == nil {
			return source
		}
		source = inner
	}
	return source
}

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
// 入参: ctx 取消上下文, source 图像
// 返回: bool 是否完全不透明, bool 是否支持原生扫描, error 取消或调色板错误
func imageNativeAlpha(ctx context.Context, source image.Image) (bool, bool, error) {
	var pixels []byte
	var stride, step int
	wide := false
	switch s := source.(type) {
	case *pngGrayAlphaImage:
		return imageNativeAlpha(ctx, s.Image)
	case *image.Paletted:
		result, err := imagePalettedAlpha(ctx, s)
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
	bounds := source.Bounds()
	width, height := bounds.Dx(), bounds.Dy()
	if bounds.Empty() || width <= 0 || height <= 0 {
		return true, true, ctx.Err()
	}
	if err := validateNativePixelBuffer(pixels, stride, width, height, step); err != nil {
		return false, true, err
	}
	for y := 0; y < height; y++ {
		row := pixels[y*stride:][:width*step]
		for x := 0; x < width; x++ {
			if x&4095 == 0 {
				if err := ctx.Err(); err != nil {
					return false, true, err
				}
			}
			i := x*step + step - 1
			if row[i] != 255 || (wide && row[i-1] != 255) {
				return false, true, ctx.Err()
			}
		}
	}
	return true, true, ctx.Err()
}

// validateSourceImage 沿解包链逐层校验源图像边界、原生像素缓冲及调色板，拒绝空图或越界缓冲
// 入参: ctx 取消上下文, source 待校验源图像
// 返回: error 图像非法、缓冲越界、调色板非法或取消错误
func validateSourceImage(ctx context.Context, source image.Image) error {
	return validateImageChain(ctx, source, false)
}

// validateImageChain 沿解包链最多16层校验边界、原生像素缓冲和调色板
// 入参: ctx 取消上下文, source 待校验图像, allowEmpty 是否允许合法空矩形
// 返回: error 参数非法、越界或取消错误
func validateImageChain(ctx context.Context, source image.Image, allowEmpty bool) error {
	img := source
	for step := 0; ; step++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := validateSourceLayer(ctx, img, allowEmpty); err != nil {
			return err
		}
		if step >= 16 {
			break
		}
		wrapper, ok := img.(imageUnwrapper)
		if !ok {
			break
		}
		inner := wrapper.Unwrap()
		if inner == nil {
			break
		}
		img = inner
	}
	return ctx.Err()
}

// validateNativePixelBuffer 校验原生像素图像的行跨度与缓冲长度，杜绝切片越界与整数溢出
// 入参: pix 像素切片, stride 行字节跨度, width 图像宽度, height 图像高度, pixelBytes 单像素字节数
// 返回: error 像素缓冲不合法错误
func validateNativePixelBuffer(pix []byte, stride, width, height, pixelBytes int) error {
	rowBytes, _, err := imageBufferSize(width, 1, pixelBytes)
	if err != nil || stride < rowBytes || len(pix) < rowBytes {
		return fmt.Errorf("invalid image pixel buffer")
	}
	if height > 1 && height-1 > (len(pix)-rowBytes)/stride {
		return fmt.Errorf("invalid image pixel buffer")
	}
	return nil
}

// validateYCbCrBuffer 校验YCbCr图像亮度与色度平面的行跨度和缓冲长度
// 入参: img 待校验YCbCr图像
// 返回: error 像素缓冲不合法错误
func validateYCbCrBuffer(img *image.YCbCr) error {
	b := img.Bounds()
	w, h := b.Dx(), b.Dy()
	if err := validateNativePixelBuffer(img.Y, img.YStride, w, h, 1); err != nil {
		return err
	}
	cw, ch := w, h
	switch img.SubsampleRatio {
	case image.YCbCrSubsampleRatio422:
		cw = (b.Max.X-1)/2 - b.Min.X/2 + 1
	case image.YCbCrSubsampleRatio420:
		cw = (b.Max.X-1)/2 - b.Min.X/2 + 1
		ch = (b.Max.Y-1)/2 - b.Min.Y/2 + 1
	case image.YCbCrSubsampleRatio440:
		ch = (b.Max.Y-1)/2 - b.Min.Y/2 + 1
	case image.YCbCrSubsampleRatio411:
		cw = (b.Max.X-1)/4 - b.Min.X/4 + 1
	case image.YCbCrSubsampleRatio410:
		cw = (b.Max.X-1)/4 - b.Min.X/4 + 1
		ch = (b.Max.Y-1)/2 - b.Min.Y/2 + 1
	}
	if err := validateNativePixelBuffer(img.Cb, img.CStride, cw, ch, 1); err != nil {
		return err
	}
	return validateNativePixelBuffer(img.Cr, img.CStride, cw, ch, 1)
}

// validateSourceLayer 校验单层图像的边界、色板条目及标准库原生像素缓冲
// 入参: ctx 取消上下文, img 单层图像, allowEmpty 是否允许空矩形
// 返回: error 图像非法、越界或取消错误
func validateSourceLayer(ctx context.Context, img image.Image, allowEmpty bool) error {
	p, isPaletted := img.(*image.Paletted)
	var palette color.Palette
	hasPalette := isPaletted
	if isPaletted {
		palette = p.Palette
	} else {
		palette, hasPalette = img.ColorModel().(color.Palette)
	}
	if hasPalette {
		if len(palette) == 0 {
			return fmt.Errorf("empty palette")
		}
		for _, entry := range palette {
			if err := ctx.Err(); err != nil {
				return err
			}
			if entry == nil {
				return fmt.Errorf("invalid palette color")
			}
		}
	}
	bounds := img.Bounds()
	if bounds.Empty() {
		if allowEmpty {
			return ctx.Err()
		}
		return fmt.Errorf("invalid image dimensions")
	}
	width, height := bounds.Dx(), bounds.Dy()
	if width <= 0 || height <= 0 {
		return fmt.Errorf("invalid image dimensions")
	}
	switch s := img.(type) {
	case *image.Paletted:
		if err := validateNativePixelBuffer(s.Pix, s.Stride, width, height, 1); err != nil {
			return fmt.Errorf("invalid palette pixel buffer")
		}
		if len(s.Palette) >= 256 {
			return ctx.Err()
		}
		for dy := 0; dy < height; dy++ {
			if err := ctx.Err(); err != nil {
				return err
			}
			offset := dy * s.Stride
			row := s.Pix[offset : offset+width]
			for x, index := range row {
				if x&4095 == 4095 {
					if err := ctx.Err(); err != nil {
						return err
					}
				}
				if int(index) >= len(s.Palette) {
					return fmt.Errorf("palette index out of range")
				}
			}
		}
	case *image.NRGBA:
		return validateNativePixelBuffer(s.Pix, s.Stride, width, height, 4)
	case *image.RGBA:
		return validateNativePixelBuffer(s.Pix, s.Stride, width, height, 4)
	case *image.NRGBA64:
		return validateNativePixelBuffer(s.Pix, s.Stride, width, height, 8)
	case *image.RGBA64:
		return validateNativePixelBuffer(s.Pix, s.Stride, width, height, 8)
	case *image.Gray:
		return validateNativePixelBuffer(s.Pix, s.Stride, width, height, 1)
	case *image.Gray16:
		return validateNativePixelBuffer(s.Pix, s.Stride, width, height, 2)
	case *image.Alpha:
		return validateNativePixelBuffer(s.Pix, s.Stride, width, height, 1)
	case *image.Alpha16:
		return validateNativePixelBuffer(s.Pix, s.Stride, width, height, 2)
	case *image.CMYK:
		return validateNativePixelBuffer(s.Pix, s.Stride, width, height, 4)
	case *image.YCbCr:
		return validateYCbCrBuffer(s)
	case *image.NYCbCrA:
		if err := validateYCbCrBuffer(&s.YCbCr); err != nil {
			return err
		}
		return validateNativePixelBuffer(s.A, s.AStride, width, height, 1)
	}
	return ctx.Err()
}

// imagePalettedAlpha 扫描实际使用的调色板索引，单次调用复用各索引的不透明判定
// 入参: ctx 取消上下文, source 索引图像
// 返回: bool 是否完全不透明, error 取消或调色板错误
func imagePalettedAlpha(ctx context.Context, source *image.Paletted) (bool, error) {
	var opaque [256]bool
	width, height := source.Rect.Dx(), source.Rect.Dy()
	if source.Rect.Empty() || width <= 0 || height <= 0 {
		return true, ctx.Err()
	}
	if err := validateNativePixelBuffer(source.Pix, source.Stride, width, height, 1); err != nil {
		return false, fmt.Errorf("invalid palette pixel buffer")
	}
	for y := 0; y < height; y++ {
		offset := y * source.Stride
		row := source.Pix[offset : offset+width]
		for x, index := range row {
			if x&4095 == 0 {
				if err := ctx.Err(); err != nil {
					return false, err
				}
			}
			if opaque[index] {
				continue
			}
			if int(index) >= len(source.Palette) {
				return false, fmt.Errorf("palette index out of range")
			}
			entry := source.Palette[index]
			if entry == nil {
				return false, fmt.Errorf("invalid palette color")
			}
			_, _, _, alpha := entry.RGBA()
			if alpha != 65535 {
				return false, ctx.Err()
			}
			opaque[index] = true
		}
	}
	return true, ctx.Err()
}

// imageOpaqueContext 分段检查透明度，已知无透明通道时不扫描颜色
// 入参: ctx 取消上下文, source 图像
// 返回: bool 是否完全不透明, error 取消或调色板错误
func imageOpaqueContext(ctx context.Context, source image.Image) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	source = unwrapImage(source)
	if result, native, err := imageNativeAlpha(ctx, source); native {
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
		if len(s.Palette) == 0 {
			return color.NRGBA64{}
		}
		if !image.Pt(x, y).In(s.Rect) {
			return imageNRGBASample(s.Palette[0])
		}
		offset := s.PixOffset(x, y)
		if offset < 0 || offset >= len(s.Pix) {
			return color.NRGBA64{}
		}
		idx := int(s.Pix[offset])
		if idx >= len(s.Palette) {
			return color.NRGBA64{}
		}
		return imageNRGBASample(s.Palette[idx])
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
	if s, ok := source.(*image.Paletted); ok {
		if len(s.Palette) == 0 || !image.Pt(x, y).In(s.Rect) {
			return color.RGBA64{}
		}
		offset := s.PixOffset(x, y)
		if offset < 0 || offset >= len(s.Pix) {
			return color.RGBA64{}
		}
		idx := int(s.Pix[offset])
		if idx >= len(s.Palette) || s.Palette[idx] == nil {
			return color.RGBA64{}
		}
		r, g, b, a := s.Palette[idx].RGBA()
		return color.RGBA64{R: uint16(r), G: uint16(g), B: uint16(b), A: uint16(a)}
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
	return color.NRGBA64{
		R: uint16(min(uint32(c.R)*65535/a, 65535)),
		G: uint16(min(uint32(c.G)*65535/a, 65535)),
		B: uint16(min(uint32(c.B)*65535/a, 65535)),
		A: c.A,
	}
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
