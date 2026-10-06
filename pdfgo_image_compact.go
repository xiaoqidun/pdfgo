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

// compactPNG 共用一次解码和降采样，比较原色、灰度、索引色及可选JPEG
// 入参: ctx 取消上下文, data 无附加块的PNG, best 已有候选, options 压缩配置, size 像素需求, resource 是否允许更换格式
// 返回: []byte 更小的图片编码, error 编码错误
func compactPNG(ctx context.Context, data, best []byte, options CompressionOptions, size image.Point, resource bool) ([]byte, error) {
	config, err := png.DecodeConfig(&contextInput{ctx: ctx, reader: bytes.NewReader(data)})
	if err != nil {
		return nil, err
	}
	depth16 := data[24] == 16
	pixelSize := int64(4)
	if depth16 {
		pixelSize = 8
	}
	if int64(config.Width)*int64(config.Height) > optimizationBufferLimit/pixelSize || depth16 && (!resource || options.Mode != CompressionLossy) {
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
	if depth16 && !photo {
		return best, ctx.Err()
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
		return best, ctx.Err()
	}
	b := img.Bounds()
	pixels := image.NewNRGBA(b)
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
			pixels.SetNRGBA(x, y, c)
			gray = gray && c.R == c.G && c.G == c.B && c.A == 255
			if _, ok := indices[c]; !ok && len(palette) <= 256 {
				indices[c] = uint8(len(palette))
				palette = append(palette, c)
			}
		}
	}
	encode := func(img image.Image) error {
		var out bytes.Buffer
		encoder := png.Encoder{CompressionLevel: png.BestCompression}
		limit := &optimizationBuffer{buffer: &out, limit: min(len(best), optimizationBufferLimit)}
		output := &pdfOutput{ctx: ctx, writer: limit}
		if err := encoder.Encode(output, img); err != nil {
			if limit.exceeded {
				return ctx.Err()
			}
			return err
		}
		if out.Len() < len(best) {
			best = out.Bytes()
		}
		return ctx.Err()
	}
	if err := encode(pixels); err != nil {
		return nil, err
	}
	if gray {
		g := image.NewGray(b)
		for y := b.Min.Y; y < b.Max.Y; y++ {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			for x := b.Min.X; x < b.Max.X; x++ {
				g.SetGray(x, y, color.Gray{Y: pixels.NRGBAAt(x, y).R})
			}
		}
		if err := encode(g); err != nil {
			return nil, err
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
		if err := encode(p); err != nil {
			return nil, err
		}
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
