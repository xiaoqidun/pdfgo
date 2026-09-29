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
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"io"
	"math"
)

// optimizationBuffer 限制候选编码的内存，超过原文大小时终止无收益的压缩
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
	best, err := OptimizeImageSize(ctx, data, options, size)
	if err != nil || options.Mode != CompressionLossy || !plainPNG(data) {
		return best, err
	}
	config, err := png.DecodeConfig(bytes.NewReader(data))
	if err != nil || int64(config.Width)*int64(config.Height) > optimizationBufferLimit/4 {
		return best, ctx.Err()
	}
	img, err := png.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	if !photographicImage(img) {
		return best, ctx.Err()
	}
	img, err = ResizeImage(ctx, img, size)
	if err != nil {
		return nil, err
	}
	var b bytes.Buffer
	if err := jpeg.Encode(&b, img, &jpeg.Options{Quality: options.ImageQuality()}); err != nil {
		return nil, err
	}
	encoded, err := OptimizeJPEG(ctx, b.Bytes())
	if err != nil {
		return nil, err
	}
	if len(encoded) < len(best) {
		best = encoded
	}
	return best, ctx.Err()
}

// plainPNG 判断图片是否只含像素编码块，避免改写颜色配置和附加信息
// 入参: data 已校验图片
// 返回: bool 是否可以转换表示
func plainPNG(data []byte) bool {
	if !bytes.HasPrefix(data, []byte("\x89PNG\r\n\x1a\n")) {
		return false
	}
	for pos := 8; pos+12 <= len(data); {
		n := uint64(binary.BigEndian.Uint32(data[pos:]))
		if n > uint64(len(data)-pos-12) {
			return false
		}
		switch string(data[pos+4 : pos+8]) {
		case "IHDR", "IDAT", "IEND", "PLTE", "tRNS":
		default:
			return false
		}
		pos += int(n) + 12
	}
	return true
}

// photographicImage 保守识别连续色调图片，少色、透明及大量锐利边缘保留无损编码
// 入参: img 图片
// 返回: bool 是否适合JPEG
func photographicImage(img image.Image) bool {
	if opaque, ok := img.(interface{ Opaque() bool }); !ok || !opaque.Opaque() {
		return false
	}
	b := img.Bounds()
	colors := make(map[color.NRGBA]bool)
	count, edges := 0, 0
	for y := b.Min.Y; y < b.Max.Y; y += max(1, b.Dy()/128) {
		for x := b.Min.X; x < b.Max.X; x += max(1, b.Dx()/128) {
			c := color.NRGBAModel.Convert(img.At(x, y)).(color.NRGBA)
			if len(colors) <= 256 {
				colors[c] = true
			}
			if x+1 < b.Max.X {
				d := color.NRGBAModel.Convert(img.At(x+1, y)).(color.NRGBA)
				if math.Abs(float64(c.R)-float64(d.R))+math.Abs(float64(c.G)-float64(d.G))+math.Abs(float64(c.B)-float64(d.B)) > 120 {
					edges++
				}
				count++
			}
		}
	}
	return len(colors) > 256 && edges*20 < count
}

// compactPNG 比较原色、灰度和精确索引色，有损模式才降低精度及尺寸
// 入参: ctx 取消上下文, data 无附加块的PNG, options 压缩配置, size 像素需求
// 返回: []byte 更小的PNG, error 编码错误
func compactPNG(ctx context.Context, data []byte, options CompressionOptions, size image.Point) ([]byte, error) {
	config, err := png.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	if int64(config.Width)*int64(config.Height) > optimizationBufferLimit/4 || data[24] == 16 {
		return data, nil
	}
	img, err := png.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	if options.Mode == CompressionLossy {
		img, err = ResizeImage(ctx, img, size)
		if err != nil {
			return nil, err
		}
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
			c := color.NRGBAModel.Convert(img.At(x, y)).(color.NRGBA)
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
	best := data
	encode := func(img image.Image) error {
		var out bytes.Buffer
		encoder := png.Encoder{CompressionLevel: png.BestCompression}
		if err := encoder.Encode(&out, img); err != nil {
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
