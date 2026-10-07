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

// OptimizeImageResource 优化资源图片，有损模式允许无元数据的不透明照片改用JPEG
// 调用方须按返回数据更新格式声明；固定格式导出应使用OptimizeImageSize
// 不修改输入；返回值可能引用输入，调用方应视为只读
// 入参: ctx 取消上下文, data 图片数据, options 压缩配置, size 像素需求
// 返回: []byte 更小的图片数据, error 编码错误
func OptimizeImageResource(ctx context.Context, data []byte, options CompressionOptions, size image.Point) ([]byte, error) {
	if err := options.Validate(); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if options.Mode == CompressionLossy && bytes.HasPrefix(data, []byte("\x89PNG\r\n\x1a\n")) {
		return optimizePNG(ctx, data, options, size, true)
	}
	return OptimizeImageSize(ctx, data, options, size)
}

// photographicImage 保守识别连续色调图片，扫描透明度及颜色期间检查取消
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
	colors := make(map[color.NRGBA]bool)
	count, edges := 0, 0
	for y := b.Min.Y; y < b.Max.Y; y += max(1, b.Dy()/128) {
		if err := ctx.Err(); err != nil {
			return false, err
		}
		for x := b.Min.X; x < b.Max.X; x += max(1, b.Dx()/128) {
			c := imageNRGBAAt(img, x, y)
			if len(colors) <= 256 {
				colors[c] = true
			}
			if x+1 < b.Max.X {
				d := imageNRGBAAt(img, x+1, y)
				if math.Abs(float64(c.R)-float64(d.R))+math.Abs(float64(c.G)-float64(d.G))+math.Abs(float64(c.B)-float64(d.B)) > 120 {
					edges++
				}
				count++
			}
		}
	}
	return len(colors) > 256 && edges*20 < count, ctx.Err()
}

// compactPNG 共用一次解码和降采样，优先比较JPEG、索引色和灰度候选
// 入参: ctx 取消上下文, data 无附加块的PNG, best 已有候选, options 压缩配置, size 像素需求, resource 是否允许更换格式
// 返回: []byte 更小的图片编码, error 编码错误
func compactPNG(ctx context.Context, data, best []byte, options CompressionOptions, size image.Point, resource bool) ([]byte, error) {
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
	img, err := png.Decode(&contextInput{ctx: ctx, reader: bytes.NewReader(data)})
	if err != nil {
		return nil, err
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
	if depth16 {
		var byteExact bool
		img, byteExact, err = compactPNGPrecision(ctx, img)
		if err != nil {
			return nil, err
		}
		if !byteExact {
			if _, gray := img.(*image.Gray16); gray || img.Bounds().Dx() != config.Width || img.Bounds().Dy() != config.Height {
				return compactPNGImage(ctx, img, best, &buffers)
			}
			return best, ctx.Err()
		}
	}
	b := img.Bounds()
	pixels, reused, err := compactPNGStorage(ctx, img)
	if err != nil {
		return nil, err
	}
	palette := color.Palette{}
	indices := make(map[color.NRGBA]uint8)
	gray := true
	levels := (1 << uint(4+options.ImageQuality()*4/100)) - 1
	quant := func(v uint8) uint8 { return uint8(((int(v)*levels + 127) / 255) * 255 / levels) }
	for y := b.Min.Y; y < b.Max.Y; y++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		for x := b.Min.X; x < b.Max.X; x++ {
			if (x-b.Min.X)&4095 == 0 {
				if err := ctx.Err(); err != nil {
					return nil, err
				}
			}
			c := imageNRGBAAt(img, x, y)
			if options.Mode == CompressionLossy {
				c.R, c.G, c.B = quant(c.R), quant(c.G), quant(c.B)
			}
			if !reused || options.Mode == CompressionLossy {
				pixels.SetNRGBA(x, y, c)
			}
			gray = gray && c.R == c.G && c.G == c.B && c.A == 255
			if len(palette) <= 256 {
				if _, ok := indices[c]; !ok {
					indices[c] = uint8(len(palette))
					palette = append(palette, c)
				}
			}
		}
		if reused && options.Mode != CompressionLossy && !gray && len(palette) > 256 {
			break
		}
	}
	if len(palette) <= 256 {
		p := image.NewPaletted(b, palette)
		for y := b.Min.Y; y < b.Max.Y; y++ {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			for x := b.Min.X; x < b.Max.X; x++ {
				p.SetColorIndex(x, y, indices[pixels.NRGBAAt(x, y)])
			}
		}
		best, err = compactPNGImage(ctx, p, best, &buffers)
		if err != nil {
			return nil, err
		}
	}
	if gray {
		g, ok := img.(*image.Gray)
		if !ok || options.Mode == CompressionLossy {
			g = image.NewGray(b)
			for y := b.Min.Y; y < b.Max.Y; y++ {
				if err := ctx.Err(); err != nil {
					return nil, err
				}
				for x := b.Min.X; x < b.Max.X; x++ {
					g.SetGray(x, y, color.Gray{Y: pixels.NRGBAAt(x, y).R})
				}
			}
		}
		best, err = compactPNGImage(ctx, g, best, &buffers)
		if err != nil {
			return nil, err
		}
	}
	best, err = compactPNGImage(ctx, pixels, best, &buffers)
	if err != nil {
		return nil, err
	}
	return best, ctx.Err()
}

// compactPNGStorage 复用本次解码的非预乘像素，其余颜色模型分配独立缓冲
// 入参: ctx 取消上下文, img 内部独占图像，不得传入调用方图像
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

// compactPNGPrecision 检查16位分量能否精确降位，或以不透明灰度减少通道
// 入参: ctx 取消上下文, img 图像
// 返回: image.Image 等价图像, bool 是否能精确表示为8位, error 取消错误
func compactPNGPrecision(ctx context.Context, img image.Image) (image.Image, bool, error) {
	bounds := img.Bounds()
	byteExact, gray := true, true
	for y := bounds.Min.Y; y < bounds.Max.Y; y++ {
		for x := bounds.Min.X; x < bounds.Max.X; x++ {
			if (x-bounds.Min.X)&4095 == 0 {
				if err := ctx.Err(); err != nil {
					return nil, false, err
				}
			}
			c := imageNRGBA64At(img, x, y)
			byteExact = byteExact && c.R%257 == 0 && c.G%257 == 0 && c.B%257 == 0 && c.A%257 == 0
			gray = gray && c.R == c.G && c.G == c.B && c.A == 65535
			if !byteExact && !gray {
				return img, false, ctx.Err()
			}
		}
	}
	if byteExact {
		return img, true, ctx.Err()
	}
	if _, ok := img.(*image.Gray16); ok {
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
	encoder := png.Encoder{CompressionLevel: png.BestCompression, BufferPool: buffers}
	limit := &optimizationBuffer{buffer: &out, limit: min(len(best), optimizationBufferLimit)}
	if err := encoder.Encode(&pdfOutput{ctx: ctx, writer: limit}, img); err != nil {
		if limit.exceeded {
			return best, ctx.Err()
		}
		return nil, err
	}
	if out.Len() < len(best) {
		best = out.Bytes()
	}
	return best, ctx.Err()
}

// compactJPEG 编码不透明照片并优化熵编码，只采用优于已有候选的结果
// 入参: ctx 取消上下文, img 图像, best 已有候选, quality 有损质量
// 返回: []byte 更小的编码, error 编码或取消错误
func compactJPEG(ctx context.Context, img image.Image, best []byte, quality int) ([]byte, error) {
	img, err := jpegEncodingImage(ctx, img)
	if err != nil {
		return nil, err
	}
	var out bytes.Buffer
	limit := &optimizationBuffer{buffer: &out, limit: optimizationBufferLimit}
	if err := jpeg.Encode(&pdfOutput{ctx: ctx, writer: limit}, img, &jpeg.Options{Quality: quality}); err != nil {
		if limit.exceeded {
			return best, ctx.Err()
		}
		return nil, err
	}
	encoded, err := OptimizeJPEG(ctx, out.Bytes())
	if err != nil {
		return nil, err
	}
	if len(encoded) < len(best) {
		best = encoded
	}
	return best, ctx.Err()
}
