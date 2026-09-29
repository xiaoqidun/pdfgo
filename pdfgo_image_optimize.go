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

const optimizationBufferLimit = 64 << 20

// OptimizeImage 优化PNG或JPEG编码，只采用更小的结果，不改变尺寸和透明度
// 无损保留像素与元数据，有损可降低RGB精度或JPEG质量；未知格式原样返回
// 返回值可能引用输入数据；不修改输入，调用方应将结果视为只读
// 入参: ctx 取消上下文, data 图片数据, options 压缩配置
// 返回: []byte 图片数据, error 编码错误
func OptimizeImage(ctx context.Context, data []byte, options CompressionOptions) ([]byte, error) {
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
		return optimizePNG(ctx, data, options)
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
	metadata, adobe, err := jpegMetadata(data)
	if err != nil {
		return nil, err
	}
	if adobe {
		return best, nil
	}
	config, err := jpeg.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	if int64(config.Width)*int64(config.Height) > optimizationBufferLimit/4 {
		return best, nil
	}
	img, err := jpeg.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	if img.ColorModel() == color.CMYKModel {
		return best, nil
	}
	var encoded bytes.Buffer
	if err := jpeg.Encode(&encoded, img, &jpeg.Options{Quality: options.ImageQuality()}); err != nil {
		return nil, err
	}
	candidate, err := OptimizeJPEG(ctx, encoded.Bytes())
	if err != nil {
		return nil, err
	}
	candidate = append(append(append([]byte(nil), candidate[:2]...), metadata...), candidate[2:]...)
	if len(candidate) < len(best) {
		best = candidate
	}
	return best, ctx.Err()
}

// jpegMetadata 保留应用与注释段，识别需要保留原颜色变换的Adobe标记
// 入参: data JPEG数据
// 返回: []byte 元数据段, bool 是否包含Adobe标记, error 解析错误
func jpegMetadata(data []byte) ([]byte, bool, error) {
	var metadata []byte
	adobe := false
	for pos := 2; pos < len(data); {
		start := pos
		if data[pos] != 255 {
			return nil, false, fmt.Errorf("invalid JPEG marker")
		}
		for pos < len(data) && data[pos] == 255 {
			pos++
		}
		if pos >= len(data) {
			break
		}
		marker := data[pos]
		pos++
		if marker == 0xda || marker == 0xd9 {
			return metadata, adobe, nil
		}
		if pos+2 > len(data) {
			break
		}
		n := int(binary.BigEndian.Uint16(data[pos:]))
		if n < 2 || n > len(data)-pos {
			break
		}
		if marker >= 0xe0 && marker <= 0xef || marker == 0xfe {
			metadata = append(metadata, data[start:pos+n]...)
		}
		if marker == 0xee {
			adobe = true
		}
		pos += n
	}
	return nil, false, fmt.Errorf("truncated JPEG header")
}

// optimizePNG 保留块内容并重压缩IDAT，有损模式仅量化八位真彩色图片
// 入参: ctx 取消上下文, data PNG数据, options 压缩配置
// 返回: []byte 优化数据, error 编码错误
func optimizePNG(ctx context.Context, data []byte, options CompressionOptions) ([]byte, error) {
	type chunk struct {
		kind string
		data []byte
	}
	var chunks []chunk
	var compressed bytes.Buffer
	eligible := false
	unsafeMetadata := false
	ended := false
	for pos := 8; pos < len(data); {
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
		chunks = append(chunks, chunk{kind, raw})
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
		if kind == "IDAT" {
			compressed.Write(raw[8 : len(raw)-4])
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
	input, err := zlib.NewReader(bytes.NewReader(compressed.Bytes()))
	if err != nil {
		return nil, err
	}
	raw, err := io.ReadAll(io.LimitReader(&contextInput{ctx: ctx, reader: input}, optimizationBufferLimit+1))
	input.Close()
	if err != nil {
		return nil, err
	}
	if len(raw) > optimizationBufferLimit {
		return data, nil
	}
	encoded, err := compressPDFBytes(ctx, raw)
	if err != nil {
		return nil, err
	}
	assemble := func(idat, header []byte, lossy bool) []byte {
		var out bytes.Buffer
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
					writePNGChunk(&out, "IDAT", idat)
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
	best := data
	if candidate := assemble(encoded, nil, false); len(candidate) < len(best) {
		best = candidate
	}
	if options.Mode == CompressionLossy && eligible && !unsafeMetadata && options.ImageQuality() < 100 {
		config, err := png.DecodeConfig(bytes.NewReader(data))
		if err != nil {
			return nil, err
		}
		if int64(config.Width)*int64(config.Height) > optimizationBufferLimit/4 {
			return best, nil
		}
		img, err := png.Decode(bytes.NewReader(data))
		if err != nil {
			return nil, err
		}
		bounds := img.Bounds()
		pixels := image.NewNRGBA(bounds)
		bits := uint(4 + options.ImageQuality()*4/100)
		levels := (1 << bits) - 1
		quant := func(v uint8) uint8 { return uint8(((int(v)*levels + 127) / 255) * 255 / levels) }
		for y := bounds.Min.Y; y < bounds.Max.Y; y++ {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			for x := bounds.Min.X; x < bounds.Max.X; x++ {
				c := color.NRGBAModel.Convert(img.At(x, y)).(color.NRGBA)
				c.R, c.G, c.B = quant(c.R), quant(c.G), quant(c.B)
				pixels.SetNRGBA(x, y, c)
			}
		}
		var b bytes.Buffer
		encoder := png.Encoder{CompressionLevel: png.BestCompression}
		if err := encoder.Encode(&b, pixels); err != nil {
			return nil, err
		}
		candidate := b.Bytes()
		var ids bytes.Buffer
		for pos := 33; pos < len(candidate); {
			n := int(binary.BigEndian.Uint32(candidate[pos:]))
			if string(candidate[pos+4:pos+8]) == "IDAT" {
				ids.Write(candidate[pos+8 : pos+8+n])
			}
			pos += n + 12
		}
		if result := assemble(ids.Bytes(), candidate[8:33], true); len(result) < len(best) {
			best = result
		}
	}
	return best, ctx.Err()
}

// writePNGChunk 写入带校验的PNG块
// 入参: w 内存输出, kind 块类型, data 块内容
func writePNGChunk(w *bytes.Buffer, kind string, data []byte) {
	var header [8]byte
	binary.BigEndian.PutUint32(header[:4], uint32(len(data)))
	copy(header[4:], kind)
	w.Write(header[:])
	w.Write(data)
	hash := crc32.NewIEEE()
	hash.Write(header[4:])
	hash.Write(data)
	var checksum [4]byte
	binary.BigEndian.PutUint32(checksum[:], hash.Sum32())
	w.Write(checksum[:])
}

// contextInput 在分块读取之间检查取消
type contextInput struct {
	ctx    context.Context
	reader io.Reader
}

// Read 检查取消状态后读取数据
// 入参: p 数据缓冲区
// 返回: int 已读字节数, error 读取错误
func (r *contextInput) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.reader.Read(p)
}
