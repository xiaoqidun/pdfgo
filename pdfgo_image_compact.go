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
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"io"
	"math"
)

// optimizationBuffer 限制候选编码缓冲，超过上限时终止当前编码
type optimizationBuffer struct {
	buffer   *bytes.Buffer
	limit    int
	exceeded bool
}

// pngOptimizationBuffers 在单次图片优化中复用编码缓冲，不跨调用保留像素
type pngOptimizationBuffers struct {
	buffer *png.EncoderBuffer
}

// pngCompactImage 将灰度或索引色图像作为八位RGB候选编码，不展开像素缓冲
type pngCompactImage struct {
	image.Image
	opaque bool
}

// pngCompactGrayImage 复用已确认灰度且不透明的八位像素，避免另建灰度缓冲
type pngCompactGrayImage struct {
	image.Image
}

// pngCompactPaletteImage 按扫描顺序映射已有像素，不分配整幅索引缓冲
type pngCompactPaletteImage struct {
	image.Image
	palette color.Palette
	indices map[color.NRGBA]uint8
}

// jpegOriginImage 将非零起点图像平移至原点，规避标准库JPEG分块步进溢出
type jpegOriginImage struct {
	source image.Image
	origin image.Point
	bounds image.Rectangle
}

// OptimizeImageResource 优化资源图片，有损模式允许无元数据不透明照片改用JPEG（调用方须更新格式声明，固定格式请用OptimizeImage），不修改输入且返回值只读
// 入参: ctx 取消上下文, data 图片数据, options 压缩配置, size 像素需求
// 返回: []byte 更小的图片数据, error 编码错误
func OptimizeImageResource(ctx context.Context, data []byte, options CompressionOptions, size image.Point) ([]byte, error) {
	if ctx == nil {
		return nil, fmt.Errorf("invalid image context")
	}
	if err := options.Validate(); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if options.Mode == CompressionLossy && bytes.HasPrefix(data, []byte("\x89PNG\r\n\x1a\n")) {
		return optimizePNG(ctx, data, options, size, true)
	}
	return OptimizeImage(ctx, data, options, size)
}

// OptimizeImageFromPixels 从内存像素单次编码最优PNG（不修改输入、不降采样、不附加元数据），候选超64MiB返回io.ErrShortBuffer
// 入参: ctx 取消上下文, source 已解码图像, options 压缩配置
// 返回: []byte 最优PNG数据, error 参数、编码或取消错误
func OptimizeImageFromPixels(ctx context.Context, source image.Image, options CompressionOptions) ([]byte, error) {
	return encodeImage(ctx, source, options, false)
}

// EncodePNGWithOptions 基于内存像素单次编码优化PNG（不修改输入、不降采样、不附加元数据），候选超64MiB返回io.ErrShortBuffer，失败时输出可能不完整
// 入参: ctx 取消上下文, writer 输出流, source 已解码图像, options 压缩配置
// 返回: error 参数、编码、取消或写入错误
func EncodePNGWithOptions(ctx context.Context, writer io.Writer, source image.Image, options CompressionOptions) error {
	if writer == nil {
		return fmt.Errorf("missing PNG writer")
	}
	data, err := encodeImage(ctx, source, options, false)
	if err != nil {
		return err
	}
	_, err = (pngContextWriter{ctx: ctx, writer: writer}).Write(data)
	return err
}

// EncodeImageResource 基于内存像素编码优化资源图片，有损模式不透明照片可用JPEG其余保留PNG（须按返回数据设格式，不降采样不附元数据），超64MiB返回io.ErrShortBuffer
// 入参: ctx 取消上下文, source 已解码图像, options 压缩配置
// 返回: []byte 图片编码, error 参数、编码或取消错误
func EncodeImageResource(ctx context.Context, source image.Image, options CompressionOptions) ([]byte, error) {
	return encodeImage(ctx, source, options, true)
}

// encodeImage 直接基于内存像素生成最优图像编码，避免先编再解的冗余PNG回路
// 入参: ctx 取消上下文, source 已解码图像, options 压缩配置, resource 是否允许更换格式
// 返回: []byte 图片编码, error 参数、编码或取消错误
func encodeImage(ctx context.Context, source image.Image, options CompressionOptions, resource bool) ([]byte, error) {
	if ctx == nil {
		return nil, fmt.Errorf("invalid image context")
	}
	if err := options.Validate(); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if source == nil {
		return nil, fmt.Errorf("missing PNG image")
	}
	bounds := source.Bounds()
	if bounds.Empty() || bounds.Dx() <= 0 || bounds.Dy() <= 0 || int64(bounds.Dx()) > 1<<31-1 || int64(bounds.Dy()) > 1<<31-1 {
		return nil, fmt.Errorf("invalid PNG dimensions")
	}
	if options.Mode == CompressionUnchanged {
		var output bytes.Buffer
		limit := &optimizationBuffer{buffer: &output, limit: optimizationBufferLimit}
		if err := EncodePNG(ctx, limit, source); err != nil {
			return nil, err
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		return output.Bytes(), nil
	}
	if err := validateSourceImage(ctx, source); err != nil {
		return nil, err
	}
	var jpegBest []byte
	if resource && options.Mode == CompressionLossy {
		photo, err := photographicImage(ctx, source)
		if err != nil {
			return nil, err
		}
		if photo {
			encoded, err := encodePhotoJPEG(ctx, source, options.ImageQuality(), optimizationBufferLimit)
			if err != nil {
				return nil, err
			}
			jpegBest = encoded
		}
	}
	data, err := encodeOptimalPNGFromPixels(ctx, source, jpegBest, options)
	if err != nil {
		return nil, err
	}
	return data, ctx.Err()
}

// encodeOptimalPNGFromPixels 单遍分析内存像素特征并直通最优PNG编码，跳过中间PNG往返
// 入参: ctx 取消上下文, source 源像素图像, best 可选已有更优候选, options 压缩配置
// 返回: []byte 最优图像编码, error 编码或取消错误
func encodeOptimalPNGFromPixels(ctx context.Context, source image.Image, best []byte, options CompressionOptions) ([]byte, error) {
	var buffers pngOptimizationBuffers
	bounds := source.Bounds()
	img := source
	if _, _, err := imageBufferSize(bounds.Dx(), bounds.Dy(), 4); err != nil || !pixelsWithinBudget(int64(bounds.Dx()), int64(bounds.Dy()), optimizationBufferLimit/4) {
		var out bytes.Buffer
		maxLen := optimizationBufferLimit
		if len(best) > 0 {
			maxLen = min(len(best), optimizationBufferLimit)
		}
		limit := &optimizationBuffer{buffer: &out, limit: maxLen}
		if err := EncodePNG(ctx, limit, img); err != nil {
			if limit.exceeded && len(best) > 0 {
				return best, ctx.Err()
			}
			return nil, err
		}
		if len(best) == 0 || out.Len() < len(best) {
			best = out.Bytes()
		}
		return best, ctx.Err()
	}
	depth16 := sourceNeeds16BitPNG(ctx, img)
	if depth16 && options.Mode != CompressionLossy {
		var byteExact bool
		var err error
		img, byteExact, err = compactPNGPrecision(ctx, img)
		if err != nil {
			return nil, err
		}
		if !byteExact {
			if _, gray := img.(*pngGrayAlphaImage); gray {
				return compactPNGGray(ctx, img, 16, true, best)
			}
			if _, gray := img.(*image.Gray16); gray {
				return compactPNGImage(ctx, img, best, &buffers)
			}
			return encodeBestPNG16(ctx, img, best)
		}
	}
	quantize := options.Mode == CompressionLossy && options.ImageQuality() < 100
	var pixels image.Image
	var writable *image.NRGBA
	reused := false
	if !quantize {
		switch src := img.(type) {
		case *image.NRGBA:
			pixels, reused = src, true
		case *image.RGBA:
			pixels, reused = src, true
		case *image.Gray, *image.Paletted:
			pixels, reused = compactPNGView(src, false), true
		}
	}
	if pixels == nil {
		writable = image.NewNRGBA(bounds)
		pixels = writable
	}
	palette := make(color.Palette, 0, 16)
	indices := make(map[color.NRGBA]uint8, 16)
	gray, opaque := true, true
	levels := (1 << uint(4+options.ImageQuality()*4/100)) - 1
	quant := func(v uint8) uint8 { return uint8(((int(v)*levels + 127) / 255) * 255 / levels) }
	w := bounds.Dx()
	rgbaSrc, isRGBA := img.(*image.RGBA)
	nrgbaSrc, isNRGBA := img.(*image.NRGBA)
	graySrc, isGray := img.(*image.Gray)
	scanSource := img
	if writable == nil && !isRGBA && !isNRGBA && !isGray {
		scanSource = pixels
	}
	for y := bounds.Min.Y; y < bounds.Max.Y; y++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		switch {
		case isRGBA && !quantize:
			row := rgbaSrc.Pix[rgbaSrc.PixOffset(bounds.Min.X, y):]
			for x := 0; x < w; x++ {
				if x&4095 == 4095 {
					if err := ctx.Err(); err != nil {
						return nil, err
					}
				}
				off := x * 4
				a := row[off+3]
				var c color.NRGBA
				if a == 255 {
					c = color.NRGBA{R: row[off], G: row[off+1], B: row[off+2], A: 255}
				} else if a == 0 {
					opaque = false
				} else {
					opaque = false
					a32 := uint32(a)
					c = color.NRGBA{
						R: uint8(min(uint32(row[off])*65535/a32, 65535) >> 8),
						G: uint8(min(uint32(row[off+1])*65535/a32, 65535) >> 8),
						B: uint8(min(uint32(row[off+2])*65535/a32, 65535) >> 8),
						A: a,
					}
				}
				if gray && (c.R != c.G || c.G != c.B) {
					gray = false
				}
				if len(palette) <= 256 {
					if _, ok := indices[c]; !ok {
						indices[c] = uint8(len(palette))
						palette = append(palette, c)
					}
				}
			}
		case isNRGBA && !quantize:
			row := nrgbaSrc.Pix[nrgbaSrc.PixOffset(bounds.Min.X, y):]
			for x := 0; x < w; x++ {
				if x&4095 == 4095 {
					if err := ctx.Err(); err != nil {
						return nil, err
					}
				}
				off := x * 4
				c := color.NRGBA{R: row[off], G: row[off+1], B: row[off+2], A: row[off+3]}
				if c.A != 255 {
					opaque = false
				}
				if gray && (c.R != c.G || c.G != c.B) {
					gray = false
				}
				if len(palette) <= 256 {
					if _, ok := indices[c]; !ok {
						indices[c] = uint8(len(palette))
						palette = append(palette, c)
					}
				}
			}
		case isGray && !quantize:
			row := graySrc.Pix[graySrc.PixOffset(bounds.Min.X, y):]
			for x := 0; x < w; x++ {
				if x&4095 == 4095 {
					if err := ctx.Err(); err != nil {
						return nil, err
					}
				}
				v := row[x]
				c := color.NRGBA{R: v, G: v, B: v, A: 255}
				if len(palette) <= 256 {
					if _, ok := indices[c]; !ok {
						indices[c] = uint8(len(palette))
						palette = append(palette, c)
					}
				}
			}
		default:
			var dstRow []byte
			if writable != nil {
				dstRow = writable.Pix[writable.PixOffset(bounds.Min.X, y):]
			}
			for x := 0; x < w; x++ {
				if x&4095 == 4095 {
					if err := ctx.Err(); err != nil {
						return nil, err
					}
				}
				c := imageNRGBAAt(scanSource, bounds.Min.X+x, y)
				if quantize {
					c.R, c.G, c.B = quant(c.R), quant(c.G), quant(c.B)
				}
				if writable != nil {
					off := x * 4
					dstRow[off], dstRow[off+1], dstRow[off+2], dstRow[off+3] = c.R, c.G, c.B, c.A
				}
				if gray && (c.R != c.G || c.G != c.B) {
					gray = false
				}
				if opaque && c.A != 255 {
					opaque = false
				}
				if len(palette) <= 256 {
					if _, ok := indices[c]; !ok {
						indices[c] = uint8(len(palette))
						palette = append(palette, c)
					}
				}
			}
		}
		if reused && !gray && !opaque && len(palette) > 256 {
			break
		}
	}
	var err error
	if gray && opaque {
		depth := 1
		if len(palette) > 16 {
			depth = 8
		} else {
			for _, entry := range palette {
				value := entry.(color.NRGBA).R
				if value%17 != 0 {
					depth = 8
					break
				}
				if value%85 != 0 {
					depth = 4
				} else if value%255 != 0 {
					depth = max(depth, 2)
				}
			}
		}
		if depth < 8 {
			return compactPNGGray(ctx, pixels, depth, false, best)
		}
		var g image.Image = &pngCompactGrayImage{Image: pixels}
		if isGray && !quantize {
			g = graySrc
		}
		return compactPNGImage(ctx, g, best, &buffers)
	}
	if gray && !opaque {
		best, err = compactPNGGray(ctx, pixels, 8, true, best)
		if err != nil {
			return nil, err
		}
		if len(palette) <= 256 {
			p := &pngCompactPaletteImage{Image: pixels, palette: palette, indices: indices}
			best, err = compactPNGImage(ctx, p, best, &buffers)
			if err != nil {
				return nil, err
			}
		}
		return best, ctx.Err()
	}
	if len(palette) <= 256 {
		p := &pngCompactPaletteImage{Image: pixels, palette: palette, indices: indices}
		best, err = compactPNGImage(ctx, p, best, &buffers)
		if err != nil {
			return nil, err
		}
		if len(palette) <= 64 {
			return best, ctx.Err()
		}
	}
	switch view := pixels.(type) {
	case *pngCompactImage:
		view.opaque = opaque
	case *image.RGBA:
		pixels = &pngCompactImage{Image: view, opaque: opaque}
	case *image.NRGBA:
		if opaque {
			pixels = &pngCompactImage{Image: view, opaque: true}
		}
	}
	return compactPNGImage(ctx, pixels, best, &buffers)
}

// sourceNeeds16BitPNG 判断源图像是否包含超出8位精度的高位深像素
// 入参: ctx 取消上下文, img 源图像
// 返回: bool 是否可能需要16位PNG编码
func sourceNeeds16BitPNG(ctx context.Context, img image.Image) bool {
	switch img.(type) {
	case *image.RGBA, *image.NRGBA, *image.Gray, *image.Alpha, *packedGrayImage:
		return false
	case *image.Paletted:
		return !palettedByteExact(img.(*image.Paletted).Palette)
	case *image.RGBA64, *image.NRGBA64, *image.Gray16, *image.Alpha16, *image.CMYK, *image.YCbCr:
		return true
	}
	if palette, ok := img.ColorModel().(color.Palette); ok {
		for _, entry := range palette {
			if ctx.Err() != nil {
				return false
			}
			if entry == nil {
				return true
			}
			c := imageNRGBASample(entry)
			if c.R%257 != 0 || c.G%257 != 0 || c.B%257 != 0 || c.A%257 != 0 {
				return true
			}
		}
		return false
	}
	depth, _, _, err := pngSampleFormat(ctx, img)
	return err == nil && depth == 16
}

// encodeBestPNG16 以最高无损压缩等级单次编码16位彩色PNG，保留非预乘透明度
// 入参: ctx 取消上下文, img 16位图像, best 可选已有候选
// 返回: []byte 更小编码, error 编码或取消错误
func encodeBestPNG16(ctx context.Context, img image.Image, best []byte) ([]byte, error) {
	var out bytes.Buffer
	maxLen := optimizationBufferLimit
	if len(best) > 0 {
		maxLen = min(len(best), optimizationBufferLimit)
	}
	limit := &optimizationBuffer{buffer: &out, limit: maxLen}
	depth, channels, kind, err := pngSampleFormat(ctx, img)
	if err != nil {
		return nil, err
	}
	sampler, err := newPNGRowSampler(img)
	if err != nil {
		return nil, err
	}
	bounds := img.Bounds()
	encoder, err := newPNGImageEncoderWithCompression(ctx, limit, bounds, depth, channels, kind, true)
	if err != nil {
		if limit.exceeded {
			if len(best) == 0 {
				return nil, io.ErrShortBuffer
			}
			return best, ctx.Err()
		}
		return nil, err
	}
	defer encoder.release()
	for y := bounds.Min.Y; y < bounds.Max.Y; y++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if err := sampler.prepare(ctx, y); err != nil {
			return nil, err
		}
		row := encoder.row
		for x := bounds.Min.X; x < bounds.Max.X; x++ {
			if (x-bounds.Min.X)&4095 == 4095 {
				if err := ctx.Err(); err != nil {
					return nil, err
				}
			}
			pixel := sampler.sample(x, y)
			values := [4]uint16{pixel.R, pixel.G, pixel.B, pixel.A}
			for c := 0; c < channels; c++ {
				offset := (x-bounds.Min.X)*encoder.pixelBytes + c*depth/8
				if depth == 8 {
					row[offset] = byte(values[c] >> 8)
				} else {
					binary.BigEndian.PutUint16(row[offset:], values[c])
				}
			}
		}
		if err := encoder.writeRow(); err != nil {
			if limit.exceeded {
				if len(best) == 0 {
					return nil, io.ErrShortBuffer
				}
				return best, ctx.Err()
			}
			return nil, err
		}
	}
	if err := encoder.finish(); err != nil {
		if limit.exceeded {
			if len(best) == 0 {
				return nil, io.ErrShortBuffer
			}
			return best, ctx.Err()
		}
		return nil, err
	}
	if len(best) == 0 || out.Len() < len(best) {
		best = out.Bytes()
	}
	return best, ctx.Err()
}

// encodePhotoJPEG 将不透明照片直接编码为优化后的JPEG，灰度图使用单通道编码
// 入参: ctx 取消上下文, img 图像, quality 有损质量, maxBytes 编码缓冲上限
// 返回: []byte JPEG编码，超限或超尺寸时为空, error 编码或取消错误
func encodePhotoJPEG(ctx context.Context, img image.Image, quality, maxBytes int) ([]byte, error) {
	bounds := img.Bounds()
	w, h := bounds.Dx(), bounds.Dy()
	if bounds.Empty() || w <= 0 || h <= 0 || w > 65535 || h > 65535 {
		return nil, nil
	}
	if _, total, err := imageBufferSize(w, h, 1); err != nil || total > optimizationBufferLimit {
		return nil, nil
	}
	gray, err := imageAllGray(ctx, img)
	if err != nil {
		return nil, err
	}
	if gray {
		img, err = imageToGray8(ctx, img)
	} else {
		img, err = jpegEncodingImage(ctx, img)
	}
	if err != nil {
		return nil, err
	}
	img = jpegZeroOrigin(img)
	var out bytes.Buffer
	limit := &optimizationBuffer{buffer: &out, limit: maxBytes}
	if err := jpeg.Encode(&pdfOutput{ctx: ctx, writer: limit}, img, &jpeg.Options{Quality: quality}); err != nil {
		if limit.exceeded {
			return nil, ctx.Err()
		}
		return nil, err
	}
	return OptimizeJPEG(ctx, out.Bytes())
}

// jpegZeroOrigin 将非零起点图像无拷贝平移至原点，防止JPEG编码器按块步进时坐标溢出
// 入参: img 待编码图像
// 返回: image.Image 原点归一化后的只读图像
func jpegZeroOrigin(img image.Image) image.Image {
	b := img.Bounds()
	if b.Min == (image.Point{}) {
		return img
	}
	rect := image.Rect(0, 0, b.Dx(), b.Dy())
	switch s := img.(type) {
	case *image.Gray:
		return &image.Gray{Pix: s.Pix, Stride: s.Stride, Rect: rect}
	case *image.RGBA:
		return &image.RGBA{Pix: s.Pix, Stride: s.Stride, Rect: rect}
	case *image.NRGBA:
		return &image.NRGBA{Pix: s.Pix, Stride: s.Stride, Rect: rect}
	default:
		return &jpegOriginImage{source: img, origin: b.Min, bounds: rect}
	}
}

// imageToGray8 将图像转换为8位单通道灰度图像，已有8位灰度直接复用
// 入参: ctx 取消上下文, src 源图像
// 返回: *image.Gray 8位灰度图像, error 缓冲或取消错误
func imageToGray8(ctx context.Context, src image.Image) (*image.Gray, error) {
	if gray, ok := src.(*image.Gray); ok {
		return gray, ctx.Err()
	}
	b := src.Bounds()
	if _, _, err := imageBufferSize(b.Dx(), b.Dy(), 1); err != nil {
		return nil, err
	}
	dst := image.NewGray(image.Rect(0, 0, b.Dx(), b.Dy()))
	for y := 0; y < b.Dy(); y++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		for x := 0; x < b.Dx(); x++ {
			if x&4095 == 4095 {
				if err := ctx.Err(); err != nil {
					return nil, err
				}
			}
			dst.Pix[y*dst.Stride+x] = imageNRGBAAt(src, b.Min.X+x, b.Min.Y+y).R
		}
	}
	return dst, ctx.Err()
}

// photographicImage 保守识别连续色调图片，支持彩色与灰度照片并检查取消
// 入参: ctx 取消上下文, img 图片
// 返回: bool 是否适合JPEG, error 取消错误
func photographicImage(ctx context.Context, img image.Image) (bool, error) {
	if _, ok := img.(interface{ Opaque() bool }); !ok {
		return false, ctx.Err()
	}
	opaque, err := imageOpaqueContext(ctx, img)
	if err != nil || !opaque {
		return false, err
	}
	b := img.Bounds()
	w, h := b.Dx(), b.Dy()
	if b.Empty() || w <= 0 || h <= 0 {
		return false, ctx.Err()
	}
	stepX, stepY := max(1, w/128), max(1, h/128)
	colors := make(map[color.NRGBA]bool)
	gray := true
	count, edges := 0, 0
	for dy := 0; ; {
		if err := ctx.Err(); err != nil {
			return false, err
		}
		y := b.Min.Y + dy
		for dx := 0; ; {
			x := b.Min.X + dx
			c := imageNRGBAAt(img, x, y)
			if gray && (c.R != c.G || c.G != c.B) {
				gray = false
			}
			if len(colors) <= 256 {
				colors[c] = true
			}
			if dx < w-1 {
				d := imageNRGBAAt(img, x+1, y)
				if math.Abs(float64(c.R)-float64(d.R))+math.Abs(float64(c.G)-float64(d.G))+math.Abs(float64(c.B)-float64(d.B)) > 120 {
					edges++
				}
				count++
			}
			if dx >= w-stepX {
				break
			}
			dx += stepX
		}
		if dy >= h-stepY {
			break
		}
		dy += stepY
	}
	if edges*20 >= count {
		return false, ctx.Err()
	}
	if len(colors) > 256 {
		return true, ctx.Err()
	}
	if gray && len(colors) > 32 {
		return imageAllGray(ctx, img)
	}
	return false, ctx.Err()
}

// compactPNG 共用一次解码和降采样，优先比较JPEG、索引色和灰度候选
// 入参: ctx 取消上下文, data 待重编码的PNG, best 已有候选, options 压缩配置, size 像素需求, resource 是否允许更换格式
// 返回: []byte 更小的图片编码, error 编码错误
func compactPNG(ctx context.Context, data, best []byte, options CompressionOptions, size image.Point, resource bool) ([]byte, error) {
	return compactPNGSource(ctx, data, best, options, size, resource, nil)
}

// compactPNGSource 比较颜色表示候选，复用指定只读像素或自行解码
// 入参: ctx 取消上下文, data PNG数据, best 已有候选, options 压缩配置, size 像素需求, resource 是否允许更换格式, source 与编码一致的像素或空值
// 返回: []byte 更小的图片编码, error 编码错误
func compactPNGSource(ctx context.Context, data, best []byte, options CompressionOptions, size image.Point, resource bool, source image.Image) ([]byte, error) {
	return compactPNGSourceMode(ctx, data, best, options, size, resource, source, true, true)
}

// compactPNGSourceMode 按允许的颜色类型比较压缩候选，有损模式支持16位降8位量化与调色板优化
// 入参: ctx 取消上下文, data PNG数据, best 已有候选, options 压缩配置, size 像素需求, resource 是否允许更换格式, source 已解码像素或空值, allowGray 允许输出灰度, allowColor 允许输出彩色或调色板
// 返回: []byte 更小的图片编码, error 编码错误
func compactPNGSourceMode(ctx context.Context, data, best []byte, options CompressionOptions, size image.Point, resource bool, source image.Image, allowGray, allowColor bool) ([]byte, error) {
	var buffers pngOptimizationBuffers
	config, err := png.DecodeConfig(&contextInput{ctx: ctx, reader: bytes.NewReader(data)})
	if err != nil {
		return nil, err
	}
	depth16 := data[24] == 16
	pixelSize := int64(4)
	if depth16 {
		pixelSize = 8
	}
	if int64(config.Width)*int64(config.Height) > optimizationBufferLimit/pixelSize {
		return best, ctx.Err()
	}
	img := source
	if img == nil {
		img, err = png.Decode(&contextInput{ctx: ctx, reader: bytes.NewReader(data)})
		if err != nil {
			return nil, err
		}
	}
	photo := false
	if resource && options.Mode == CompressionLossy {
		photo, err = photographicImage(ctx, img)
		if err != nil {
			return nil, err
		}
	}
	if options.Mode == CompressionLossy {
		img, err = ResizeImage(ctx, img, size)
		if err != nil {
			return nil, err
		}
	}
	if photo {
		best, err = compactJPEG(ctx, img, best, options.ImageQuality())
		if err != nil {
			return nil, err
		}
	}
	if depth16 && options.Mode != CompressionLossy {
		var byteExact bool
		img, byteExact, err = compactPNGPrecision(ctx, img)
		if err != nil {
			return nil, err
		}
		if !byteExact {
			if _, gray := img.(*pngGrayAlphaImage); gray {
				return compactPNGGray(ctx, img, 16, true, best)
			}
			if _, gray := img.(*image.Gray16); gray || img.Bounds().Dx() != config.Width || img.Bounds().Dy() != config.Height {
				return compactPNGImage(ctx, img, best, &buffers)
			}
			return best, ctx.Err()
		}
	}
	b := img.Bounds()
	var pixels image.Image
	var writable *image.NRGBA
	reused := true
	quantize := options.Mode == CompressionLossy && options.ImageQuality() < 100
	if !quantize {
		switch img.(type) {
		case *image.Gray, *image.Paletted:
			pixels = compactPNGView(img, false)
		}
	}
	if pixels == nil {
		if source != nil && quantize {
			writable, reused = image.NewNRGBA(b), false
		} else {
			writable, reused, err = compactPNGStorage(ctx, img)
			if err != nil {
				return nil, err
			}
		}
		pixels = writable
	}
	source = img
	if writable == nil {
		source = pixels
	}
	palette := color.Palette{}
	indices := make(map[color.NRGBA]uint8)
	gray, opaque := true, true
	levels := (1 << uint(4+options.ImageQuality()*4/100)) - 1
	quant := func(v uint8) uint8 { return uint8(((int(v)*levels + 127) / 255) * 255 / levels) }
	for y := b.Min.Y; y < b.Max.Y; y++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		for x := b.Min.X; x < b.Max.X; x++ {
			if (x-b.Min.X)&4095 == 4095 {
				if err := ctx.Err(); err != nil {
					return nil, err
				}
			}
			c := imageNRGBAAt(source, x, y)
			if quantize {
				c.R, c.G, c.B = quant(c.R), quant(c.G), quant(c.B)
			}
			if !reused || quantize {
				writable.SetNRGBA(x, y, c)
			}
			gray = gray && c.R == c.G && c.G == c.B
			opaque = opaque && c.A == 255
			if len(palette) <= 256 {
				if _, ok := indices[c]; !ok {
					indices[c] = uint8(len(palette))
					palette = append(palette, c)
				}
			}
		}
		if reused && options.Mode != CompressionLossy && !gray && !opaque && len(palette) > 256 {
			break
		}
	}
	if allowColor && len(palette) <= 256 {
		p := &pngCompactPaletteImage{Image: pixels, palette: palette, indices: indices}
		best, err = compactPNGImage(ctx, p, best, &buffers)
		if err != nil {
			return nil, err
		}
	}
	if allowGray && gray && !opaque {
		best, err = compactPNGGray(ctx, pixels, 8, true, best)
		if err != nil {
			return nil, err
		}
	}
	if allowGray && gray && opaque {
		depth := 1
		for _, entry := range palette {
			value := entry.(color.NRGBA).R
			if value%17 != 0 {
				depth = 8
				break
			}
			if value%85 != 0 {
				depth = 4
			} else if value%255 != 0 {
				depth = max(depth, 2)
			}
		}
		if depth < 8 {
			best, err = compactPNGGray(ctx, pixels, depth, false, best)
			if err != nil {
				return nil, err
			}
		}
		var g image.Image = &pngCompactGrayImage{Image: pixels}
		if source, ok := img.(*image.Gray); ok && !quantize {
			g = source
		}
		best, err = compactPNGImage(ctx, g, best, &buffers)
		if err != nil {
			return nil, err
		}
	}
	if allowColor {
		if writable == nil {
			pixels.(*pngCompactImage).opaque = opaque
		}
		best, err = compactPNGImage(ctx, pixels, best, &buffers)
		if err != nil {
			return nil, err
		}
	}
	return best, ctx.Err()
}

// compactPNGView 复用八位像素并转换调色板，避免RGB编码时逐像素分配颜色
// 入参: img 灰度或索引色图像, opaque 已检查的不透明状态
// 返回: image.Image 只读RGB候选
func compactPNGView(img image.Image, opaque bool) image.Image {
	if source, ok := img.(*image.Paletted); ok {
		view := *source
		view.Palette = make(color.Palette, len(source.Palette))
		for i, entry := range source.Palette {
			c := imageNRGBASample(entry)
			view.Palette[i] = color.NRGBA{R: uint8(c.R >> 8), G: uint8(c.G >> 8), B: uint8(c.B >> 8), A: uint8(c.A >> 8)}
		}
		img = &view
	}
	return &pngCompactImage{Image: img, opaque: opaque}
}

// compactPNGStorage 复用非预乘像素或分配独立缓冲，复用的外部输入仅可读取不可原地量化
// 入参: ctx 取消上下文, img 图像
// 返回: *image.NRGBA 可写像素, bool 是否复用原像素, error 取消错误
func compactPNGStorage(ctx context.Context, img image.Image) (*image.NRGBA, bool, error) {
	if err := ctx.Err(); err != nil {
		return nil, false, err
	}
	switch source := img.(type) {
	case *image.NRGBA:
		return source, true, nil
	case *image.RGBA:
		opaque, err := imageOpaqueContext(ctx, source)
		if err != nil {
			return nil, false, err
		}
		if opaque {
			return &image.NRGBA{Pix: source.Pix, Stride: source.Stride, Rect: source.Rect}, true, nil
		}
	}
	return image.NewNRGBA(img.Bounds()), false, nil
}

// compactPNGPrecision 检查16位分量能否精确降位，或以灰度减少通道
// 入参: ctx 取消上下文, img 图像
// 返回: image.Image 等价图像, bool 是否能精确表示为8位, error 取消错误
func compactPNGPrecision(ctx context.Context, img image.Image) (image.Image, bool, error) {
	bounds := img.Bounds()
	byteExact, gray, opaque := true, true, true
	for y := bounds.Min.Y; y < bounds.Max.Y; y++ {
		for x := bounds.Min.X; x < bounds.Max.X; x++ {
			if (x-bounds.Min.X)&4095 == 0 {
				if err := ctx.Err(); err != nil {
					return nil, false, err
				}
			}
			c := imageNRGBA64At(img, x, y)
			byteExact = byteExact && c.R%257 == 0 && c.G%257 == 0 && c.B%257 == 0 && c.A%257 == 0
			gray = gray && c.R == c.G && c.G == c.B
			opaque = opaque && c.A == 65535
			if !byteExact && !gray {
				return img, false, ctx.Err()
			}
		}
	}
	if byteExact {
		return img, true, ctx.Err()
	}
	if !opaque {
		return &pngGrayAlphaImage{img}, false, ctx.Err()
	}
	if _, ok := img.(*image.Gray16); ok {
		return img, false, ctx.Err()
	}
	if _, total, err := imageBufferSize(bounds.Dx(), bounds.Dy(), 2); err != nil || total > optimizationBufferLimit {
		return img, false, ctx.Err()
	}
	result := image.NewGray16(bounds)
	for y := bounds.Min.Y; y < bounds.Max.Y; y++ {
		for x := bounds.Min.X; x < bounds.Max.X; x++ {
			if (x-bounds.Min.X)&4095 == 0 {
				if err := ctx.Err(); err != nil {
					return nil, false, err
				}
			}
			result.SetGray16(x, y, color.Gray16{Y: imageNRGBA64At(img, x, y).R})
		}
	}
	return result, false, ctx.Err()
}

// compactPNGImage 限制PNG候选体积，只采用更小的编码
// 入参: ctx 取消上下文, img 图像, best 已有候选, buffers 本次编码缓冲
// 返回: []byte 更小的编码, error 编码或取消错误
func compactPNGImage(ctx context.Context, img image.Image, best []byte, buffers *pngOptimizationBuffers) ([]byte, error) {
	var out bytes.Buffer
	maxLen := optimizationBufferLimit
	if len(best) > 0 {
		maxLen = min(len(best), optimizationBufferLimit)
	}
	encoder := png.Encoder{CompressionLevel: png.BestCompression, BufferPool: buffers}
	limit := &optimizationBuffer{buffer: &out, limit: maxLen}
	if err := encoder.Encode(&pdfOutput{ctx: ctx, writer: limit}, img); err != nil {
		if limit.exceeded {
			if len(best) == 0 {
				return nil, io.ErrShortBuffer
			}
			return best, ctx.Err()
		}
		return nil, err
	}
	if len(best) == 0 || out.Len() < len(best) {
		best = out.Bytes()
	}
	return best, ctx.Err()
}

// compactJPEG 编码不透明照片并优化熵编码，只采用优于已有候选的结果
// 入参: ctx 取消上下文, img 图像, best 已有候选, quality 有损质量
// 返回: []byte 更小的编码, error 编码或取消错误
func compactJPEG(ctx context.Context, img image.Image, best []byte, quality int) ([]byte, error) {
	encoded, err := encodePhotoJPEG(ctx, img, quality, optimizationBufferLimit)
	if err != nil {
		return nil, err
	}
	if len(encoded) != 0 && len(encoded) < len(best) {
		best = encoded
	}
	return best, ctx.Err()
}

// ColorModel 返回本次编码的调色板
// 返回: color.Model 索引色模型
func (p *pngCompactPaletteImage) ColorModel() color.Model {
	return p.palette
}

// ColorIndexAt 读取像素对应的调色板索引
// 入参: x 横向坐标, y 纵向坐标
// 返回: uint8 颜色索引，坐标越界时为0
func (p *pngCompactPaletteImage) ColorIndexAt(x, y int) uint8 {
	if !image.Pt(x, y).In(p.Bounds()) {
		return 0
	}
	return p.indices[imageNRGBAAt(p.Image, x, y)]
}

// ColorModel 返回八位灰度模型
// 返回: color.Model 灰度颜色模型
func (p *pngCompactGrayImage) ColorModel() color.Model {
	return color.GrayModel
}

// At 读取已确认灰度的像素分量
// 入参: x 横向坐标, y 纵向坐标
// 返回: color.Color 八位灰度颜色
func (p *pngCompactGrayImage) At(x, y int) color.Color {
	return color.Gray{Y: imageNRGBAAt(p.Image, x, y).R}
}

// ColorModel 返回候选编码的非预乘颜色模型
// 返回: color.Model 八位非预乘颜色模型
func (p *pngCompactImage) ColorModel() color.Model {
	return color.NRGBAModel
}

// Opaque 返回颜色扫描得到的不透明状态
// 返回: bool 图像是否完全不透明
func (p *pngCompactImage) Opaque() bool {
	return p.opaque
}

// At 读取八位非预乘颜色，保证标准库PNG编码复用一致的反预乘与钳位语义
// 入参: x 横向坐标, y 纵向坐标
// 返回: color.Color 八位非预乘颜色
func (p *pngCompactImage) At(x, y int) color.Color {
	return imageNRGBAAt(p.Image, x, y)
}

// NRGBAAt 直接读取原始像素，避免通过颜色接口分配临时值
// 入参: x 横向坐标, y 纵向坐标
// 返回: color.NRGBA 八位非预乘颜色
func (p *pngCompactImage) NRGBAAt(x, y int) color.NRGBA {
	return imageNRGBAAt(p.Image, x, y)
}

// Get 取出本次优化的编码缓冲
// 返回: *png.EncoderBuffer 可复用缓冲或空值
func (p *pngOptimizationBuffers) Get() *png.EncoderBuffer {
	buffer := p.buffer
	p.buffer = nil
	return buffer
}

// Put 归还本次优化的编码缓冲
// 入参: buffer 编码缓冲
func (p *pngOptimizationBuffers) Put(buffer *png.EncoderBuffer) {
	p.buffer = buffer
}

// Write 写入受限缓冲区
// 入参: data 编码数据
// 返回: int 已写字节数, error 容量错误
func (b *optimizationBuffer) Write(data []byte) (int, error) {
	if len(data) > b.limit-b.buffer.Len() {
		b.exceeded = true
		return 0, io.ErrShortBuffer
	}
	return b.buffer.Write(data)
}

// ColorModel 返回底层图像的颜色模型
// 返回: color.Model 颜色模型
func (j *jpegOriginImage) ColorModel() color.Model {
	return j.source.ColorModel()
}

// Bounds 返回平移至原点后的图像边界
// 返回: image.Rectangle 归一化边界
func (j *jpegOriginImage) Bounds() image.Rectangle {
	return j.bounds
}

// At 按原始坐标偏移读取像素颜色
// 入参: x 横向坐标, y 纵向坐标
// 返回: color.Color 像素颜色
func (j *jpegOriginImage) At(x, y int) color.Color {
	return j.source.At(j.origin.X+x, j.origin.Y+y)
}

// RGBA64At 按原始坐标偏移读取十六位预乘颜色
// 入参: x 横向坐标, y 纵向坐标
// 返回: color.RGBA64 十六位预乘颜色
func (j *jpegOriginImage) RGBA64At(x, y int) color.RGBA64 {
	return imageRGBA64At(j.source, j.origin.X+x, j.origin.Y+y)
}
