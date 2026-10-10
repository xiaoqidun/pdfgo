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

// OptimizeImage 优化PNG或JPEG编码，只采用更小的结果，不改变尺寸和透明度
// 无损保留像素与元数据，有损可降低RGB精度或JPEG质量；未知格式原样返回
// 返回值可能引用输入数据；不修改输入，调用方应将结果视为只读
// 入参: ctx 取消上下文, data 图片数据, options 压缩配置
// 返回: []byte 图片数据, error 编码错误
func OptimizeImage(ctx context.Context, data []byte, options CompressionOptions) ([]byte, error) {
	return OptimizeImageSize(ctx, data, options, image.Point{})
}

// OptimizeImageSize 优化图片编码，有损模式按两轴像素需求等比缩小，只采用更小的结果
// 未知显示尺寸时传零值，不降采样；不会放大图片或修改输入数据
// 入参: ctx 取消上下文, data 图片数据, options 压缩配置, size 像素需求
// 返回: []byte 图片数据, error 编码错误
func OptimizeImageSize(ctx context.Context, data []byte, options CompressionOptions, size image.Point) ([]byte, error) {
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
	metadata, adobe, err := jpegMetadataContext(ctx, data)
	if err != nil {
		return nil, err
	}
	if adobe || len(metadata) >= optimizationBufferLimit {
		return best, nil
	}
	config, err := jpeg.DecodeConfig(&contextInput{ctx: ctx, reader: bytes.NewReader(data)})
	if err != nil {
		return nil, err
	}
	if size.X <= 0 || size.Y <= 0 || size.X >= config.Width || size.Y >= config.Height {
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
	if img.ColorModel() == color.CMYKModel {
		return best, nil
	}
	img, err = ResizeImage(ctx, img, size)
	if err != nil {
		return nil, err
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
		if len(metadata) == 0 {
			best = candidate
		} else {
			joined := make([]byte, len(candidate)+len(metadata))
			copy(joined, candidate[:2])
			copy(joined[2:], metadata)
			copy(joined[2+len(metadata):], candidate[2:])
			best = joined
		}
	}
	return best, ctx.Err()
}

// optimizePNG 比较图像表示并按整图体积限制IDAT重压缩，保留原数据校验
// 入参: ctx 取消上下文, data PNG数据, options 压缩配置, size 像素上限, resource 是否允许更换格式
// 返回: []byte 优化数据, error 编码错误
func optimizePNG(ctx context.Context, data []byte, options CompressionOptions, size image.Point, resource bool) ([]byte, error) {
	var chunks []pngOptimizationChunk
	eligible := false
	plain := true
	unsafeMetadata := false
	ended := false
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
		if crc32.ChecksumIEEE(raw[4:len(raw)-4]) != binary.BigEndian.Uint32(raw[len(raw)-4:]) {
			return nil, fmt.Errorf("invalid PNG checksum")
		}
		chunks = append(chunks, pngOptimizationChunk{kind, raw})
		if kind != "IHDR" && kind != "IDAT" && kind != "IEND" && kind != "PLTE" && kind != "tRNS" {
			plain = false
		}
		switch kind {
		case "IHDR", "IDAT", "IEND", "PLTE", "tRNS", "hIST", "sBIT", "cHRM", "gAMA", "iCCP", "sRGB", "cICP", "mDCV", "cLLI", "bKGD", "pHYs", "tIME", "tEXt", "zTXt", "iTXt", "eXIf":
		default:
			unsafeMetadata = unsafeMetadata || raw[4]&32 == 0 || raw[7]&32 == 0
		}
		if kind == "IHDR" {
			eligible = n == 13 && raw[16] == 8 && (raw[17] == 2 || raw[17] == 6)
		}
		if kind == "acTL" {
			return data, nil
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
	best := data
	if plain {
		candidate, err := compactPNG(ctx, data, best, options, size, resource)
		if err != nil {
			return nil, err
		}
		if len(candidate) < len(best) {
			best = candidate
		}
	}
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
	encoded := packed.Bytes()
	assemble := func(idat [][]byte, header []byte, lossy bool) []byte {
		var out bytes.Buffer
		length := uint64(8 + 12)
		for _, part := range idat {
			length += uint64(len(part))
		}
		for _, c := range chunks {
			if c.kind != "IDAT" && !(lossy && (c.kind == "PLTE" || c.kind == "tRNS" || c.kind == "hIST" || c.kind == "sBIT")) {
				length += uint64(len(c.data))
			}
		}
		if length >= uint64(len(best)) || length > optimizationBufferLimit {
			return best
		}
		out.Grow(int(length))
		out.Write(data[:8])
		written := false
		for _, c := range chunks {
			switch c.kind {
			case "IHDR":
				if header != nil {
					out.Write(header)
				} else {
					out.Write(c.data)
				}
			case "IDAT":
				if !written {
					writePNGChunk(&out, "IDAT", idat...)
					written = true
				}
			case "PLTE", "tRNS", "hIST", "sBIT":
				if !lossy {
					out.Write(c.data)
				}
			default:
				out.Write(c.data)
			}
		}
		return out.Bytes()
	}
	if !output.exceeded {
		if candidate := assemble([][]byte{encoded}, nil, false); len(candidate) < len(best) {
			best = candidate
		}
	}
	if !plain && options.Mode == CompressionLossy && eligible && !unsafeMetadata && (options.ImageQuality() < 100 || size.X > 0 && size.Y > 0) {
		config, err := png.DecodeConfig(&contextInput{ctx: ctx, reader: bytes.NewReader(data)})
		if err != nil {
			return nil, err
		}
		if int64(config.Width)*int64(config.Height) > optimizationBufferLimit/4 {
			return best, nil
		}
		img, err := png.Decode(&contextInput{ctx: ctx, reader: bytes.NewReader(data)})
		if err != nil {
			return nil, err
		}
		img, err = ResizeImage(ctx, img, size)
		if err != nil {
			return nil, err
		}
		bounds := img.Bounds()
		pixels, _, err := compactPNGStorage(ctx, img)
		if err != nil {
			return nil, err
		}
		bits := uint(4 + options.ImageQuality()*4/100)
		levels := (1 << bits) - 1
		quant := func(v uint8) uint8 { return uint8(((int(v)*levels + 127) / 255) * 255 / levels) }
		for y := bounds.Min.Y; y < bounds.Max.Y; y++ {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			for x := bounds.Min.X; x < bounds.Max.X; x++ {
				if (x-bounds.Min.X)&4095 == 0 {
					if err := ctx.Err(); err != nil {
						return nil, err
					}
				}
				c := imageNRGBAAt(img, x, y)
				c.R, c.G, c.B = quant(c.R), quant(c.G), quant(c.B)
				pixels.SetNRGBA(x, y, c)
			}
		}
		var b bytes.Buffer
		encoder := png.Encoder{CompressionLevel: png.BestCompression}
		limit := &optimizationBuffer{buffer: &b, limit: min(len(best), optimizationBufferLimit)}
		if err := encoder.Encode(&pdfOutput{ctx: ctx, writer: limit}, pixels); err != nil {
			if limit.exceeded {
				return best, ctx.Err()
			}
			return nil, err
		}
		candidate := b.Bytes()
		var idat [][]byte
		for pos := 33; pos < len(candidate); {
			n := int(binary.BigEndian.Uint32(candidate[pos:]))
			if string(candidate[pos+4:pos+8]) == "IDAT" {
				idat = append(idat, candidate[pos+8:pos+8+n])
			}
			pos += n + 12
		}
		if result := assemble(idat, candidate[8:33], true); len(result) < len(best) {
			best = result
		}
	}
	return best, ctx.Err()
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
