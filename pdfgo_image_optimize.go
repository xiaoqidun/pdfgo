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
	"compress/zlib"
	"context"
	"encoding/binary"
	"fmt"
	"hash/crc32"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"io"
	"math"
)

// optimizationBufferLimit 限制图片压缩候选的缓冲大小，单位为字节
const optimizationBufferLimit = 64 << 20

// pngOptimizationChunk 引用已校验的原PNG块，不复制编码内容
type pngOptimizationChunk struct {
	kind string
	data []byte
}

// pngIDATReader 顺序读取分散的IDAT内容，不合并完整压缩缓冲
type pngIDATReader struct {
	chunks []pngOptimizationChunk
	index  int
	offset int
}

// OptimizeImage 优化PNG/JPEG且仅采用更小结果（未知格式原样返回），无损保留像素与元数据，有损可按像素需求等比缩小（不放大，未知尺寸传零）并降精度/质量
// 不修改输入，返回值可能共享输入内存须视为只读
// 入参: ctx 取消上下文, data 图片数据, options 压缩配置, size 像素需求
// 返回: []byte 图片数据, error 编码错误
func OptimizeImage(ctx context.Context, data []byte, options CompressionOptions, size image.Point) ([]byte, error) {
	if ctx == nil {
		return nil, fmt.Errorf("invalid image context")
	}
	if err := options.Validate(); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if options.Mode == CompressionUnchanged {
		return data, nil
	}
	if bytes.HasPrefix(data, []byte("\x89PNG\r\n\x1a\n")) {
		return optimizePNG(ctx, data, options, size, false)
	}
	if !bytes.HasPrefix(data, []byte{255, 216}) {
		return data, nil
	}
	best, err := OptimizeJPEG(ctx, data)
	if err != nil {
		return nil, err
	}
	if options.Mode != CompressionLossy {
		return best, nil
	}
	config, err := jpeg.DecodeConfig(&contextInput{ctx: ctx, reader: bytes.NewReader(data)})
	if err != nil {
		return nil, err
	}
	cmykSource := config.ColorModel == color.CMYKModel
	metadata, profileData, frame, adobe, tagged, iccValid, err := jpegMetadataProfileContext(ctx, data, cmykSource)
	if err != nil {
		return nil, err
	}
	if !cmykSource && frame.components == 3 && tagged && adobe == 0 || cmykSource && !iccValid || len(metadata) >= optimizationBufferLimit {
		return best, nil
	}
	if !cmykSource && (size.X <= 0 || size.Y <= 0 || size.X >= config.Width || size.Y >= config.Height) {
		sufficient, err := jpegQualitySufficient(ctx, data, options.ImageQuality())
		if err != nil {
			return nil, err
		}
		if sufficient {
			return best, nil
		}
	}
	if int64(config.Width)*int64(config.Height) > optimizationBufferLimit/4 {
		return best, nil
	}
	img, err := jpeg.Decode(&contextInput{ctx: ctx, reader: bytes.NewReader(data)})
	if err != nil {
		return nil, err
	}
	if cmykImg, ok := img.(*image.CMYK); ok {
		converted, ok, err := convertCMYKJPEGToRGBA(ctx, cmykImg, profileData)
		if err != nil {
			return nil, err
		}
		if !ok {
			return best, nil
		}
		img = converted
	}
	img, err = ResizeImage(ctx, img, size)
	if err != nil {
		return nil, err
	}
	if len(metadata) != 0 {
		valid, err := resizeJPEGPhysical(ctx, metadata, image.Pt(config.Width, config.Height), img.Bounds().Size())
		if err != nil {
			return nil, err
		}
		if !valid {
			return best, ctx.Err()
		}
		metadata, valid, err = resizeJPEGExif(ctx, metadata, image.Pt(config.Width, config.Height), img.Bounds().Size())
		if err != nil {
			return nil, err
		}
		if !valid {
			return best, ctx.Err()
		}
	}
	img, err = jpegEncodingImage(ctx, img)
	if err != nil {
		return nil, err
	}
	var encoded bytes.Buffer
	limit := &optimizationBuffer{buffer: &encoded, limit: optimizationBufferLimit - len(metadata)}
	if err := jpeg.Encode(&pdfOutput{ctx: ctx, writer: limit}, img, &jpeg.Options{Quality: options.ImageQuality()}); err != nil {
		if limit.exceeded {
			return best, ctx.Err()
		}
		return nil, err
	}
	candidate, err := OptimizeJPEG(ctx, encoded.Bytes())
	if err != nil {
		return nil, err
	}
	if len(candidate)+len(metadata) < len(best) {
		best = insertJPEGSegments(candidate, metadata)
	}
	return best, ctx.Err()
}

// insertJPEGSegments 在JPEG起始标记后插入元数据段，无附加段时直接返回原数据
// 入参: data JPEG数据, segment 待插入应用段
// 返回: []byte 拼接后的JPEG数据
func insertJPEGSegments(data, segment []byte) []byte {
	if len(segment) == 0 {
		return data
	}
	joined := make([]byte, len(data)+len(segment))
	copy(joined, data[:2])
	copy(joined[2:], segment)
	copy(joined[2+len(segment):], data[2:])
	return joined
}

// convertCMYKJPEGToRGBA 按内嵌CMYK配置或内置印刷配置将四色JPEG像素转为不透明RGBA
// 入参: ctx 取消上下文, src 四色像素, profileData 可选内嵌ICC配置
// 返回: *image.RGBA 转换结果, bool 配置是否可用, error 取消错误
func convertCMYKJPEGToRGBA(ctx context.Context, src *image.CMYK, profileData []byte) (*image.RGBA, bool, error) {
	var space *ColorSpace
	var intent Name
	if len(profileData) != 0 {
		parsed, err := NewICCColorSpace(profileData)
		if err != nil || parsed.Model != "DeviceCMYK" {
			return nil, false, ctx.Err()
		}
		space, intent = parsed, parsed.DefaultIntent()
	}
	bounds := src.Bounds()
	dst := image.NewRGBA(bounds)
	type cmykCacheEntry struct {
		key   uint32
		rgb   color.RGBA
		valid bool
	}
	var cache [4096]cmykCacheEntry
	for y := bounds.Min.Y; y < bounds.Max.Y; y++ {
		if y&15 == 0 {
			if err := ctx.Err(); err != nil {
				return nil, false, err
			}
		}
		srcRow := src.Pix[src.PixOffset(bounds.Min.X, y):]
		dstRow := dst.Pix[dst.PixOffset(bounds.Min.X, y):]
		for x := 0; x < bounds.Dx(); x++ {
			off := x * 4
			c, m, yv, k := srcRow[off], srcRow[off+1], srcRow[off+2], srcRow[off+3]
			key := uint32(c) | uint32(m)<<8 | uint32(yv)<<16 | uint32(k)<<24
			slot := (key * 2654435761) >> 20
			entry := &cache[slot]
			var out color.RGBA
			if entry.valid && entry.key == key {
				out = entry.rgb
			} else {
				var rgb [3]float64
				if space != nil {
					values := [4]float64{float64(c) / 255, float64(m) / 255, float64(yv) / 255, float64(k) / 255}
					var err error
					rgb, err = space.SRGB(values[:], intent)
					if err != nil {
						return nil, false, ctx.Err()
					}
				} else {
					rgb = DeviceCMYKToSRGB(float64(c)/255, float64(m)/255, float64(yv)/255, float64(k)/255)
				}
				out = color.RGBA{
					R: uint8(math.Round(math.Max(0, math.Min(1, rgb[0])) * 255)),
					G: uint8(math.Round(math.Max(0, math.Min(1, rgb[1])) * 255)),
					B: uint8(math.Round(math.Max(0, math.Min(1, rgb[2])) * 255)),
					A: 255,
				}
				entry.key, entry.rgb, entry.valid = key, out, true
			}
			dstRow[off], dstRow[off+1], dstRow[off+2], dstRow[off+3] = out.R, out.G, out.B, 255
		}
	}
	return dst, true, ctx.Err()
}

// optimizePNG 比较图像表示并限制IDAT重压缩体积，按块复制规则保留附加信息
// 入参: ctx 取消上下文, data PNG数据, options 压缩配置, size 像素上限, resource 是否允许更换格式
// 返回: []byte 优化数据, error 编码错误
func optimizePNG(ctx context.Context, data []byte, options CompressionOptions, size image.Point, resource bool) ([]byte, error) {
	return optimizePNGSource(ctx, data, options, size, resource, nil)
}

// optimizePNGSource 比较PNG编码候选，内部编码入口可复用与编码一致的只读像素
// 入参: ctx 取消上下文, data PNG数据, options 压缩配置, size 像素上限, resource 是否允许更换格式, pixels 已编码像素或空值
// 返回: []byte 优化数据, error 编码错误
func optimizePNGSource(ctx context.Context, data []byte, options CompressionOptions, size image.Point, resource bool, pixels image.Image) ([]byte, error) {
	var chunks []pngOptimizationChunk
	plain := true
	unsafeMetadata := false
	ended := false
	var density, iccpChunk, gamaChunk, chrmChunk []byte
	srgbChunk, hdrColor, hasExif := false, false, false
	imageData := false
	imageDataEnded := false
	palette := false
	for pos := 8; pos < len(data); {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if len(data)-pos < 12 {
			return nil, fmt.Errorf("truncated PNG chunk")
		}
		n := int64(binary.BigEndian.Uint32(data[pos:]))
		if n > int64(len(data)-pos-12) {
			return nil, fmt.Errorf("invalid PNG length")
		}
		raw := data[pos : pos+12+int(n)]
		kind := string(raw[4:8])
		for _, letter := range raw[4:8] {
			if letter < 'A' || letter > 'Z' && letter < 'a' || letter > 'z' {
				return nil, fmt.Errorf("invalid PNG chunk type")
			}
		}
		if raw[6]&32 != 0 || pos == 8 && kind != "IHDR" {
			return nil, fmt.Errorf("invalid PNG chunk type or order")
		}
		if crc32.ChecksumIEEE(raw[4:len(raw)-4]) != binary.BigEndian.Uint32(raw[len(raw)-4:]) {
			return nil, fmt.Errorf("invalid PNG checksum")
		}
		switch kind {
		case "IHDR":
			if pos != 8 || n != 13 {
				return nil, fmt.Errorf("invalid PNG header")
			}
		case "PLTE":
			if palette || imageData || n == 0 || n > 768 || n%3 != 0 || data[25] == 0 || data[25] == 4 || data[25] == 3 && n/3 > 1<<data[24] {
				return nil, fmt.Errorf("invalid PNG palette")
			}
			palette = true
		case "IDAT":
			if imageDataEnded || data[25] == 3 && !palette {
				return nil, fmt.Errorf("invalid PNG image data order")
			}
		case "IEND":
			if !imageData || n != 0 {
				return nil, fmt.Errorf("invalid PNG end")
			}
		default:
			if raw[4]&32 == 0 {
				return nil, fmt.Errorf("unsupported PNG critical chunk %s", kind)
			}
		}
		chunks = append(chunks, pngOptimizationChunk{kind, raw})
		if kind != "IHDR" && kind != "IDAT" && kind != "IEND" && kind != "PLTE" && kind != "tRNS" && kind != "pHYs" {
			plain = false
		}
		switch kind {
		case "pHYs":
			if n != 9 || density != nil || imageData || raw[16] > 1 || binary.BigEndian.Uint32(raw[8:]) > math.MaxInt32 || binary.BigEndian.Uint32(raw[12:]) > math.MaxInt32 {
				return nil, fmt.Errorf("invalid PNG physical dimensions")
			}
			density = raw
		case "iCCP":
			iccpChunk = raw[8 : len(raw)-4]
		case "sRGB":
			srgbChunk = true
		case "gAMA":
			gamaChunk = raw[8 : len(raw)-4]
		case "cHRM":
			chrmChunk = raw[8 : len(raw)-4]
		case "cICP", "mDCV", "cLLI":
			hdrColor = true
		case "eXIf":
			hasExif = true
		}
		imageDataEnded = imageDataEnded || imageData && kind != "IDAT"
		imageData = imageData || kind == "IDAT"
		switch kind {
		case "IHDR", "IDAT", "IEND", "PLTE", "tRNS", "hIST", "sBIT", "cHRM", "gAMA", "iCCP", "sRGB", "cICP", "mDCV", "cLLI", "bKGD", "pHYs", "tIME", "tEXt", "zTXt", "iTXt", "eXIf":
		case "acTL", "fcTL", "fdAT":
			unsafeMetadata = true
		default:
			unsafeMetadata = unsafeMetadata || raw[7]&32 == 0
		}
		pos += len(raw)
		if kind == "IEND" {
			ended = true
			if pos != len(data) {
				return data, nil
			}
			break
		}
	}
	if !ended {
		return nil, fmt.Errorf("missing PNG end")
	}
	if unsafeMetadata {
		return data, ctx.Err()
	}
	best := data
	if plain {
		var candidate []byte
		var err error
		if density == nil {
			candidate, err = compactPNGSource(ctx, data, best, options, size, resource, pixels)
		} else {
			candidate, err = compactPNGPhysical(ctx, data, best, density, options, size, resource)
		}
		if err != nil {
			return nil, err
		}
		if len(candidate) < len(best) {
			best = candidate
		}
	}
	var reused [][]byte
	if len(best) < len(data) && len(best) >= 33 && (data[25] == 2 || data[25] == 6) && bytes.Equal(data[:33], best[:33]) {
		var err error
		reused, err = reusePNGImageData(ctx, chunks, best)
		if err != nil {
			return nil, err
		}
	}
	if reused == nil {
		input, err := zlib.NewReader(&contextInput{ctx: ctx, reader: &pngIDATReader{chunks: chunks}})
		if err != nil {
			return nil, err
		}
		var packed bytes.Buffer
		budget := min(len(best)-1, optimizationBufferLimit) - 8 - 12
		for _, chunk := range chunks {
			if chunk.kind != "IDAT" {
				budget -= len(chunk.data)
			}
		}
		output := &optimizationBuffer{buffer: &packed, limit: max(0, budget)}
		encoder := optimizationCompressor(output)
		defer releaseOptimizationCompressor(encoder)
		source := &io.LimitedReader{R: &contextInput{ctx: ctx, reader: input}, N: 512<<20 + 1}
		_, err = io.Copy(encoder, source)
		if output.exceeded {
			_, err = io.Copy(io.Discard, source)
		}
		input.Close()
		closeErr := encoder.Close()
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if source.N == 0 {
			return data, nil
		}
		if err != nil {
			return nil, err
		}
		if closeErr != nil && !output.exceeded {
			return nil, closeErr
		}
		if !output.exceeded {
			reused = [][]byte{packed.Bytes()}
		}
	}
	if reused != nil {
		var out bytes.Buffer
		length := uint64(8 + 12)
		for _, part := range reused {
			length += uint64(len(part))
		}
		for _, c := range chunks {
			if c.kind != "IDAT" {
				length += uint64(len(c.data))
			}
		}
		if length < uint64(len(best)) && length <= optimizationBufferLimit {
			out.Grow(int(length))
			out.Write(data[:8])
			written := false
			for _, c := range chunks {
				if c.kind == "IDAT" {
					if !written {
						writePNGChunk(&out, "IDAT", reused...)
						written = true
					}
				} else {
					out.Write(c.data)
				}
			}
			best = out.Bytes()
		}
	}
	if !plain && options.Mode == CompressionLossy && (resource || options.ImageQuality() < 100 || size.X > 0 && size.Y > 0 || data[24] == 16) {
		config, err := png.DecodeConfig(&contextInput{ctx: ctx, reader: bytes.NewReader(data)})
		if err != nil {
			return nil, err
		}
		pixelSize := int64(4)
		if data[24] == 16 {
			pixelSize = 8
		}
		if int64(config.Width)*int64(config.Height) > optimizationBufferLimit/pixelSize {
			return best, ctx.Err()
		}
		img, err := png.Decode(&contextInput{ctx: ctx, reader: bytes.NewReader(data)})
		if err != nil {
			return nil, err
		}
		before := image.Pt(config.Width, config.Height)
		if resource && !hdrColor && !hasExif {
			photo, err := photographicImage(ctx, img)
			if err != nil {
				return nil, err
			}
			if photo {
				srgbImg, ok, err := pngImageToSRGB(ctx, img, iccpChunk, srgbChunk, gamaChunk, chrmChunk)
				if err != nil {
					return nil, err
				}
				if ok {
					resized, err := ResizeImage(ctx, srgbImg, size)
					if err != nil {
						return nil, err
					}
					encoded, err := encodePhotoJPEG(ctx, resized, options.ImageQuality(), min(len(best), optimizationBufferLimit))
					if err != nil {
						return nil, err
					}
					if len(encoded) != 0 {
						var jfif []byte
						densityOK := true
						if density != nil {
							jfif, densityOK = jfifSegmentFromPNGPhysical(density, before, resized.Bounds().Size())
						}
						if densityOK && len(encoded)+len(jfif) < len(best) && len(encoded)+len(jfif) <= optimizationBufferLimit {
							best = insertJPEGSegments(encoded, jfif)
						}
					}
				}
			}
		}
		allowGray, allowColor := true, true
		if len(iccpChunk) != 0 {
			if data[25] == 0 || data[25] == 4 {
				allowColor = false
			} else {
				allowGray = false
			}
		}
		if hdrColor {
			allowGray = false
		}
		candidate, err := compactPNGSourceMode(ctx, data, best, options, size, false, img, allowGray, allowColor)
		if err != nil {
			return nil, err
		}
		if &candidate[0] != &best[0] {
			assembled, err := assembleLossyPNGChunks(ctx, data, chunks, candidate, len(best))
			if err != nil {
				return nil, err
			}
			if len(assembled) != 0 && len(assembled) < len(best) {
				best = assembled
			}
		}
	}
	return best, ctx.Err()
}

// pngImageToSRGB 按PNG色彩块将图像转换到sRGB，保留纯灰度特征以支持单通道JPEG编码
// 入参: ctx 取消上下文, img 解码图像, iccp iCCP块载荷, hasSRGB 是否存在sRGB块, gama gAMA块载荷, chrm cHRM块载荷
// 返回: image.Image sRGB图像, bool 色彩配置是否可安全转换, error 取消错误
func pngImageToSRGB(ctx context.Context, img image.Image, iccp []byte, hasSRGB bool, gama, chrm []byte) (image.Image, bool, error) {
	if len(iccp) == 0 && (hasSRGB || len(gama) == 0 && len(chrm) == 0) {
		return img, true, ctx.Err()
	}
	var space *ColorSpace
	var intent Name
	var gammaOnly float64
	if len(iccp) != 0 {
		parsed, ok, err := pngICCColorSpace(ctx, iccp)
		if err != nil || !ok {
			return nil, false, err
		}
		space, intent = parsed, parsed.DefaultIntent()
	} else if len(chrm) != 0 {
		parsed, ok := pngCalRGBColorSpace(gama, chrm)
		if !ok {
			return nil, false, ctx.Err()
		}
		space, intent = parsed, "RelativeColorimetric"
	} else if len(gama) == 4 {
		raw := binary.BigEndian.Uint32(gama)
		if raw == 0 {
			return nil, false, ctx.Err()
		}
		if raw >= 44955 && raw <= 45955 {
			return img, true, ctx.Err()
		}
		gammaOnly = 100000.0 / float64(raw)
	} else {
		return nil, false, ctx.Err()
	}
	bounds := img.Bounds()
	if space != nil && space.Model == "DeviceGray" {
		dst := image.NewGray(image.Rect(0, 0, bounds.Dx(), bounds.Dy()))
		var cache [256]uint8
		var cached [256]bool
		for y := 0; y < bounds.Dy(); y++ {
			if err := ctx.Err(); err != nil {
				return nil, false, err
			}
			for x := 0; x < bounds.Dx(); x++ {
				if x&4095 == 4095 {
					if err := ctx.Err(); err != nil {
						return nil, false, err
					}
				}
				c := imageNRGBA64At(img, bounds.Min.X+x, bounds.Min.Y+y)
				if c.R%257 == 0 && cached[c.R>>8] {
					dst.Pix[y*dst.Stride+x] = cache[c.R>>8]
					continue
				}
				rgb, err := space.SRGB([]float64{float64(c.R) / 65535}, intent)
				if err != nil {
					return nil, false, ctx.Err()
				}
				v := uint8(math.Round(math.Max(0, math.Min(1, rgb[0])) * 255))
				if c.R%257 == 0 {
					cache[c.R>>8], cached[c.R>>8] = v, true
				}
				dst.Pix[y*dst.Stride+x] = v
			}
		}
		return dst, true, ctx.Err()
	}
	if space != nil && space.Model != "DeviceRGB" {
		return nil, false, ctx.Err()
	}
	var gammaLUT [256]uint8
	if gammaOnly > 0 {
		for i := range gammaLUT {
			gammaLUT[i] = uint8(srgbComponent(math.Pow(float64(i)/255, gammaOnly)) >> 8)
		}
	}
	_, isGray := img.(*image.Gray)
	_, isGray16 := img.(*image.Gray16)
	if (isGray || isGray16) && gammaOnly > 0 {
		dst := image.NewGray(image.Rect(0, 0, bounds.Dx(), bounds.Dy()))
		for y := 0; y < bounds.Dy(); y++ {
			if err := ctx.Err(); err != nil {
				return nil, false, err
			}
			for x := 0; x < bounds.Dx(); x++ {
				if x&4095 == 4095 {
					if err := ctx.Err(); err != nil {
						return nil, false, err
					}
				}
				c := imageNRGBA64At(img, bounds.Min.X+x, bounds.Min.Y+y)
				if isGray {
					dst.Pix[y*dst.Stride+x] = gammaLUT[c.R>>8]
				} else {
					dst.Pix[y*dst.Stride+x] = uint8(srgbComponent(math.Pow(float64(c.R)/65535, gammaOnly)) >> 8)
				}
			}
		}
		return dst, true, ctx.Err()
	}
	dst := image.NewRGBA(image.Rect(0, 0, bounds.Dx(), bounds.Dy()))
	var prev color.NRGBA64
	var prevRGB color.RGBA
	hasPrev := false
	for y := 0; y < bounds.Dy(); y++ {
		if err := ctx.Err(); err != nil {
			return nil, false, err
		}
		for x := 0; x < bounds.Dx(); x++ {
			if x&4095 == 4095 {
				if err := ctx.Err(); err != nil {
					return nil, false, err
				}
			}
			c := imageNRGBA64At(img, bounds.Min.X+x, bounds.Min.Y+y)
			if hasPrev && c == prev {
				dst.SetRGBA(x, y, prevRGB)
				continue
			}
			var out color.RGBA
			if gammaOnly > 0 {
				if c.R>>8 == c.R&0xff && c.G>>8 == c.G&0xff && c.B>>8 == c.B&0xff {
					out = color.RGBA{
						R: gammaLUT[c.R>>8],
						G: gammaLUT[c.G>>8],
						B: gammaLUT[c.B>>8],
						A: 255,
					}
				} else {
					out = color.RGBA{
						R: uint8(srgbComponent(math.Pow(float64(c.R)/65535, gammaOnly)) >> 8),
						G: uint8(srgbComponent(math.Pow(float64(c.G)/65535, gammaOnly)) >> 8),
						B: uint8(srgbComponent(math.Pow(float64(c.B)/65535, gammaOnly)) >> 8),
						A: 255,
					}
				}
			} else {
				rgb, err := space.SRGB([]float64{float64(c.R) / 65535, float64(c.G) / 65535, float64(c.B) / 65535}, intent)
				if err != nil {
					return nil, false, ctx.Err()
				}
				out = color.RGBA{
					R: uint8(math.Round(math.Max(0, math.Min(1, rgb[0])) * 255)),
					G: uint8(math.Round(math.Max(0, math.Min(1, rgb[1])) * 255)),
					B: uint8(math.Round(math.Max(0, math.Min(1, rgb[2])) * 255)),
					A: 255,
				}
			}
			prev, prevRGB, hasPrev = c, out, true
			dst.SetRGBA(x, y, out)
		}
	}
	return dst, true, ctx.Err()
}

// pngICCColorSpace 解压并解析PNG的iCCP色彩配置文件
// 入参: ctx 取消上下文, payload iCCP块载荷
// 返回: *ColorSpace 色彩空间, bool 是否解析有效, error 取消错误
func pngICCColorSpace(ctx context.Context, payload []byte) (*ColorSpace, bool, error) {
	sep := bytes.IndexByte(payload, 0)
	if sep < 1 || sep > 79 || sep+2 >= len(payload) || payload[sep+1] != 0 {
		return nil, false, ctx.Err()
	}
	reader, err := zlib.NewReader(bytes.NewReader(payload[sep+2:]))
	if err != nil {
		return nil, false, ctx.Err()
	}
	profile, err := io.ReadAll(io.LimitReader(&contextInput{ctx: ctx, reader: reader}, 16<<20+1))
	closeErr := reader.Close()
	if ctx.Err() != nil {
		return nil, false, ctx.Err()
	}
	if err != nil || closeErr != nil || len(profile) > 16<<20 {
		return nil, false, nil
	}
	space, err := NewICCColorSpace(profile)
	if err != nil {
		return nil, false, nil
	}
	return space, true, nil
}

// pngCalRGBColorSpace 由PNG的cHRM与gAMA块构造CalRGB色彩空间
// 入参: gama 可选gAMA块载荷, chrm cHRM块载荷
// 返回: *ColorSpace 校准RGB空间, bool 参数是否有效
func pngCalRGBColorSpace(gama, chrm []byte) (*ColorSpace, bool) {
	if len(chrm) != 32 {
		return nil, false
	}
	decodeGamma := 2.2
	if len(gama) == 4 {
		raw := binary.BigEndian.Uint32(gama)
		if raw == 0 {
			return nil, false
		}
		decodeGamma = 100000.0 / float64(raw)
	}
	var vals [8]float64
	for i := range vals {
		vals[i] = float64(binary.BigEndian.Uint32(chrm[i*4:])) / 100000.0
	}
	wx, wy, rx, ry, gx, gy, bx, by := vals[0], vals[1], vals[2], vals[3], vals[4], vals[5], vals[6], vals[7]
	if wy <= 0 || ry <= 0 || gy <= 0 || by <= 0 {
		return nil, false
	}
	xw, zw := wx/wy, (1-wx-wy)/wy
	xr, zr := rx/ry, (1-rx-ry)/ry
	xg, zg := gx/gy, (1-gx-gy)/gy
	xb, zb := bx/by, (1-bx-by)/by
	det := xr*(zb-zg) + xg*(zr-zb) + xb*(zg-zr)
	if det == 0 || math.IsNaN(det) || math.IsInf(det, 0) {
		return nil, false
	}
	sr := (xw*(zb-zg) + xg*(zw-zb) + xb*(zg-zw)) / det
	sg := (xr*(zb-zw) + xw*(zr-zb) + xb*(zw-zr)) / det
	sb := (xr*(zw-zg) + xg*(zr-zw) + xw*(zg-zr)) / det
	if sr <= 0 || sg <= 0 || sb <= 0 {
		return nil, false
	}
	space, err := NewCalRGBColorSpace(
		[3]float64{xw, 1, zw},
		[3]float64{0, 0, 0},
		[3]float64{decodeGamma, decodeGamma, decodeGamma},
		[9]float64{sr * xr, sr, sr * zr, sg * xg, sg, sg * zg, sb * xb, sb, sb * zb},
	)
	if err != nil {
		return nil, false
	}
	return space, true
}

// assembleLossyPNGChunks 按PNG标准块顺序合并新图像数据与原安全辅助块，同步缩放pHYs和eXIf
// 入参: ctx 取消上下文, data 原PNG, chunks 原PNG块, candidate 新编码PNG, limit 目标体积上限
// 返回: []byte 合并后的PNG或空值, error 取消错误
func assembleLossyPNGChunks(ctx context.Context, data []byte, chunks []pngOptimizationChunk, candidate []byte, limit int) ([]byte, error) {
	if len(candidate) < 33 {
		return nil, ctx.Err()
	}
	header := candidate[8:33]
	before := image.Pt(int(binary.BigEndian.Uint32(data[16:])), int(binary.BigEndian.Uint32(data[20:])))
	after := image.Pt(int(binary.BigEndian.Uint32(header[8:])), int(binary.BigEndian.Uint32(header[12:])))
	var newPLTE, newTRNS []byte
	var newIDAT [][]byte
	for pos := 33; pos < len(candidate); {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if len(candidate)-pos < 12 {
			return nil, nil
		}
		n := int(binary.BigEndian.Uint32(candidate[pos:]))
		if n < 0 || pos+12+n > len(candidate) {
			return nil, nil
		}
		raw := candidate[pos : pos+12+n]
		switch string(raw[4:8]) {
		case "PLTE":
			newPLTE = raw
		case "tRNS":
			newTRNS = raw
		case "IDAT":
			newIDAT = append(newIDAT, raw[8:8+n])
		}
		pos += 12 + n
	}
	if len(newIDAT) == 0 {
		return nil, ctx.Err()
	}
	sameFormat := header[16] == data[24] && header[17] == data[25] && newPLTE == nil
	var beforePLTE, afterPLTE, afterIDAT [][]byte
	for _, c := range chunks {
		switch c.kind {
		case "cHRM", "gAMA", "iCCP", "sRGB", "cICP", "mDCV", "cLLI":
			beforePLTE = append(beforePLTE, c.data)
		case "pHYs":
			chunk, ok := resizePNGPhysical(c.data, before, after)
			if !ok {
				return nil, ctx.Err()
			}
			beforePLTE = append(beforePLTE, chunk)
		case "bKGD":
			if sameFormat {
				afterPLTE = append(afterPLTE, c.data)
			}
		case "eXIf":
			if before == after {
				afterIDAT = append(afterIDAT, c.data)
			} else {
				updated, ok := resizeExifGeometry(ctx, c.data[8:len(c.data)-4], before, after)
				if !ok {
					return nil, ctx.Err()
				}
				var buf bytes.Buffer
				_ = writePNGChunk(&buf, "eXIf", updated)
				afterIDAT = append(afterIDAT, buf.Bytes())
			}
		case "tEXt", "zTXt", "iTXt", "tIME":
			afterIDAT = append(afterIDAT, c.data)
		}
	}
	var out bytes.Buffer
	buf := &optimizationBuffer{buffer: &out, limit: min(limit-1, optimizationBufferLimit)}
	writeRaw := func(raw []byte) bool {
		_, err := buf.Write(raw)
		return err == nil
	}
	if !writeRaw(data[:8]) || !writeRaw(header) {
		return nil, ctx.Err()
	}
	for _, raw := range beforePLTE {
		if !writeRaw(raw) {
			return nil, ctx.Err()
		}
	}
	if newPLTE != nil && !writeRaw(newPLTE) {
		return nil, ctx.Err()
	}
	if newTRNS != nil && !writeRaw(newTRNS) {
		return nil, ctx.Err()
	}
	for _, raw := range afterPLTE {
		if !writeRaw(raw) {
			return nil, ctx.Err()
		}
	}
	if err := writePNGChunk(buf, "IDAT", newIDAT...); err != nil {
		return nil, ctx.Err()
	}
	for _, raw := range afterIDAT {
		if !writeRaw(raw) {
			return nil, ctx.Err()
		}
	}
	if err := writePNGChunk(buf, "IEND", nil); err != nil {
		return nil, ctx.Err()
	}
	return out.Bytes(), ctx.Err()
}

// reusePNGImageData 逐块核对过滤后的扫描行，复用完全相同的已压缩图像流
// 入参: ctx 取消上下文, original 原始PNG块, candidate 已生成的PNG候选
// 返回: [][]byte 可复用的IDAT片段，不一致时为空, error 读取或取消错误
func reusePNGImageData(ctx context.Context, original []pngOptimizationChunk, candidate []byte) ([][]byte, error) {
	var chunks []pngOptimizationChunk
	var parts [][]byte
	for pos := 8; pos < len(candidate); {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if len(candidate)-pos < 12 {
			return nil, fmt.Errorf("truncated PNG chunk")
		}
		size := uint64(binary.BigEndian.Uint32(candidate[pos:]))
		if size > uint64(len(candidate)-pos-12) {
			return nil, fmt.Errorf("invalid PNG length")
		}
		raw := candidate[pos : pos+12+int(size)]
		if string(raw[4:8]) == "IDAT" {
			chunks = append(chunks, pngOptimizationChunk{kind: "IDAT", data: raw})
			parts = append(parts, raw[8:len(raw)-4])
		}
		pos += len(raw)
	}
	left, err := zlib.NewReader(&contextInput{ctx: ctx, reader: &pngIDATReader{chunks: original}})
	if err != nil {
		return nil, err
	}
	defer left.Close()
	right, err := zlib.NewReader(&contextInput{ctx: ctx, reader: &pngIDATReader{chunks: chunks}})
	if err != nil {
		return nil, err
	}
	defer right.Close()
	a, b := make([]byte, 32<<10), make([]byte, 32<<10)
	for total := 0; total <= 512<<20; {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		n, leftErr := io.ReadFull(left, a)
		m, rightErr := io.ReadFull(right, b)
		if leftErr != nil && leftErr != io.EOF && leftErr != io.ErrUnexpectedEOF {
			return nil, leftErr
		}
		if rightErr != nil && rightErr != io.EOF && rightErr != io.ErrUnexpectedEOF {
			return nil, rightErr
		}
		if n != m || !bytes.Equal(a[:n], b[:m]) {
			return nil, nil
		}
		total += n
		if leftErr != nil || rightErr != nil {
			if leftErr == rightErr && total <= 512<<20 {
				return parts, nil
			}
			return nil, nil
		}
	}
	return nil, nil
}

// writePNGChunk 将分段数据写为一个带校验的PNG块，不复制合并数据
// 入参: w 输出流, kind 块类型, data 按序排列的块内容片段
// 返回: error 写入错误
func writePNGChunk(w io.Writer, kind string, data ...[]byte) error {
	var length uint64
	for _, part := range data {
		length += uint64(len(part))
		if length > 1<<31-1 {
			return fmt.Errorf("invalid PNG chunk length")
		}
	}
	var header [8]byte
	binary.BigEndian.PutUint32(header[:4], uint32(length))
	copy(header[4:], kind)
	hash := crc32.Update(0, crc32.IEEETable, header[4:])
	for _, part := range data {
		hash = crc32.Update(hash, crc32.IEEETable, part)
	}
	var checksum [4]byte
	binary.BigEndian.PutUint32(checksum[:], hash)
	write := func(part []byte) error {
		n, err := w.Write(part)
		if err != nil {
			return err
		}
		if n != len(part) {
			return io.ErrShortWrite
		}
		return nil
	}
	if err := write(header[:]); err != nil {
		return err
	}
	for _, part := range data {
		if err := write(part); err != nil {
			return err
		}
	}
	return write(checksum[:])
}

// Read 读取下一个非空IDAT片段
// 入参: data 读取缓冲
// 返回: int 读取字节数, error 流结束标记
func (r *pngIDATReader) Read(data []byte) (int, error) {
	if len(data) == 0 {
		return 0, nil
	}
	for r.index < len(r.chunks) {
		chunk := r.chunks[r.index]
		if chunk.kind != "IDAT" || r.offset >= len(chunk.data)-12 {
			r.index++
			r.offset = 0
			continue
		}
		n := copy(data, chunk.data[8+r.offset:len(chunk.data)-4])
		r.offset += n
		return n, nil
	}
	return 0, io.EOF
}
