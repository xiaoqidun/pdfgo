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

// EncodePNGWithOptions 编码并优化固定PNG格式，共用已解码像素，不修改输入
// 不降采样，不附加密度或色彩配置；失败时输出可能不完整
// 编码候选超过64MiB时返回io.ErrShortBuffer
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

// EncodeImageResource 编码并优化资源图片，有损模式允许不透明照片采用JPEG，其余保留PNG
// 不修改输入，不降采样，不附加密度或色彩配置；调用方须按返回数据设置格式声明
// 编码候选超过64MiB时返回io.ErrShortBuffer，失败时不返回编码数据
// 入参: ctx 取消上下文, source 已解码图像, options 压缩配置
// 返回: []byte 图片编码, error 参数、编码或取消错误
func EncodeImageResource(ctx context.Context, source image.Image, options CompressionOptions) ([]byte, error) {
	return encodeImage(ctx, source, options, true)
}

// encodeImage 生成PNG基准候选，安全复用非预乘像素并比较压缩结果
// 入参: ctx 取消上下文, source 已解码图像, options 压缩配置, resource 是否允许更换格式
// 返回: []byte 图片编码, error 参数、编码或取消错误
func encodeImage(ctx context.Context, source image.Image, options CompressionOptions, resource bool) ([]byte, error) {
	if err := options.Validate(); err != nil {
		return nil, err
	}
	var output bytes.Buffer
	limit := &optimizationBuffer{buffer: &output, limit: optimizationBufferLimit}
	if err := EncodePNG(ctx, limit, source); err != nil {
		return nil, err
	}
	if options.Mode == CompressionUnchanged {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		return output.Bytes(), nil
	}
	var pixels image.Image
	switch source.(type) {
	case *image.NRGBA, *image.NRGBA64, *image.Gray, *image.Gray16:
		pixels = source
	}
	data, err := optimizePNGSource(ctx, output.Bytes(), options, image.Point{}, resource, pixels)
	if err != nil {
		return nil, err
	}
	return data, nil
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
// 入参: ctx 取消上下文, data 待重编码的PNG, best 已有候选, options 压缩配置, size 像素需求, resource 是否允许更换格式
// 返回: []byte 更小的图片编码, error 编码错误
func compactPNG(ctx context.Context, data, best []byte, options CompressionOptions, size image.Point, resource bool) ([]byte, error) {
	return compactPNGSource(ctx, data, best, options, size, resource, nil)
}

// compactPNGSource 比较颜色表示候选，复用指定只读像素或自行解码
// 入参: ctx 取消上下文, data PNG数据, best 已有候选, options 压缩配置, size 像素需求, resource 是否允许更换格式, source 与编码一致的像素或空值
// 返回: []byte 更小的图片编码, error 编码错误
func compactPNGSource(ctx context.Context, data, best []byte, options CompressionOptions, size image.Point, resource bool, source image.Image) ([]byte, error) {
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
	if depth16 {
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
			if (x-b.Min.X)&4095 == 0 {
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
		if reused && options.Mode != CompressionLossy && !gray && len(palette) > 256 {
			break
		}
	}
	if len(palette) <= 256 {
		p := &pngCompactPaletteImage{Image: pixels, palette: palette, indices: indices}
		best, err = compactPNGImage(ctx, p, best, &buffers)
		if err != nil {
			return nil, err
		}
	}
	if gray && !opaque {
		best, err = compactPNGGray(ctx, pixels, 8, true, best)
		if err != nil {
			return nil, err
		}
	}
	if gray && opaque {
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
	if writable == nil {
		pixels.(*pngCompactImage).opaque = opaque
	}
	best, err = compactPNGImage(ctx, pixels, best, &buffers)
	if err != nil {
		return nil, err
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

// compactPNGStorage 复用非预乘像素，其余颜色模型分配独立缓冲
// 输入来自调用方时，复用结果仅可读取，不得用于原地量化
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
