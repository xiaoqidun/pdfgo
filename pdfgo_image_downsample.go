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
	"errors"
	"fmt"
	"image"
	"image/color"
	"io"
	"math"
	"math/bits"

	"github.com/andybalholm/brotli"
)

// runLengthStreamReader 按需流式解码RunLengthDecode字节流，避免为巨幅图像分配完整解压缓冲
type runLengthStreamReader struct {
	ctx        context.Context
	data       []byte
	pos        int
	literal    int
	repeat     int
	checkpoint int
	value      byte
	done       bool
	warning    func(Diagnostic)
}

// Read 从游程流解码下一批字节并响应取消
// 入参: p 目标缓冲区
// 返回: int 写入字节数, error 结束、格式或取消错误
func (r *runLengthStreamReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	if r.done {
		return 0, io.EOF
	}
	written := 0
	for written < len(p) {
		if r.checkpoint >= 4096 {
			r.checkpoint = 0
			if err := r.ctx.Err(); err != nil {
				return written, err
			}
		}
		if r.literal > 0 {
			n := min(r.literal, len(p)-written)
			copy(p[written:written+n], r.data[r.pos:r.pos+n])
			r.pos += n
			r.literal -= n
			written += n
			r.checkpoint += n
			continue
		}
		if r.repeat > 0 {
			n := min(r.repeat, len(p)-written)
			for i := 0; i < n; i++ {
				p[written+i] = r.value
			}
			r.repeat -= n
			written += n
			r.checkpoint += n
			continue
		}
		if r.pos >= len(r.data) {
			r.done = true
			if r.warning != nil {
				r.warning(Diagnostic{Offset: int64(len(r.data)), Message: "RunLength terminator missing; encoded stream boundary retained"})
				if written > 0 {
					return written, nil
				}
				return 0, io.EOF
			}
			return written, fmt.Errorf("missing RunLength terminator")
		}
		b := int(r.data[r.pos])
		r.pos++
		r.checkpoint++
		if b == 128 {
			r.done = true
			if written > 0 {
				return written, nil
			}
			return 0, io.EOF
		}
		if b < 128 {
			count := b + 1
			if count > len(r.data)-r.pos {
				r.done = true
				return written, io.ErrUnexpectedEOF
			}
			r.literal = count
		} else {
			if r.pos >= len(r.data) {
				r.done = true
				return written, io.ErrUnexpectedEOF
			}
			r.repeat = 257 - b
			r.value = r.data[r.pos]
			r.pos++
		}
	}
	return written, nil
}

// scaledImageSize 按ResizeImage的等比两轴需求计算目标缩小尺寸，无需缩小时返回原尺寸
// 入参: width 原宽, height 原高, size 两轴像素需求
// 返回: image.Point 目标尺寸
func scaledImageSize(width, height int, size image.Point) image.Point {
	if size.X <= 0 || size.Y <= 0 || width <= 0 || height <= 0 {
		return image.Point{X: width, Y: height}
	}
	scale := math.Max(float64(size.X)/float64(width), float64(size.Y)/float64(height))
	if scale >= 1 {
		return image.Point{X: width, Y: height}
	}
	return image.Point{X: max(1, int(math.Ceil(float64(width)*scale))), Y: max(1, int(math.Ceil(float64(height)*scale)))}
}

// boundedSampleSize 将单层样本两轴收敛至不超过目标合成尺寸且不放大
// 入参: width 源宽, height 源高, target 目标尺寸
// 返回: image.Point 有效样本尺寸
func boundedSampleSize(width, height int, target image.Point) image.Point {
	if target.X <= 0 || target.Y <= 0 || width <= 0 || height <= 0 {
		return image.Point{X: width, Y: height}
	}
	return image.Point{X: min(width, target.X), Y: min(height, target.Y)}
}

// downsampleBounds 预计算盒式降采样的源像素列区间起止表
// 入参: srcW 源宽度, dstW 目标宽度
// 返回: []int 长度为dstW+1的区间边界表
func downsampleBounds(srcW, dstW int) []int {
	starts := make([]int, dstW+1)
	for dx := 0; dx <= dstW; dx++ {
		starts[dx] = int(int64(dx) * int64(srcW) / int64(dstW))
	}
	return starts
}

// countBitsRange 统计MSB优先位图中半开区间内置位像素数量
// 入参: line 行字节数据, start 起始像素列, end 结束像素列
// 返回: uint64 置位像素数
func countBitsRange(line []byte, start, end int) uint64 {
	b0, o0 := start>>3, uint(start&7)
	b1, o1 := (end-1)>>3, uint((end-1)&7)
	if b0 == b1 {
		mask := byte(0xff>>o0) & byte(0xff<<(7-o1))
		return uint64(bits.OnesCount8(line[b0] & mask))
	}
	sum := uint64(bits.OnesCount8(line[b0] & byte(0xff>>o0)))
	for b := b0 + 1; b < b1; b++ {
		sum += uint64(bits.OnesCount8(line[b]))
	}
	sum += uint64(bits.OnesCount8(line[b1] & byte(0xff<<(7-o1))))
	return sum
}

// readSampleRow 完整读取单行原始样本，并在末行容忍可控校验和或尾部多余输入错误
// 入参: reader 数据源, line 行缓冲, lastRow 是否最后一行
// 返回: error 读取或延迟校验错误
func readSampleRow(reader io.Reader, line []byte, lastRow bool) error {
	filled := 0
	for filled < len(line) {
		n, err := reader.Read(line[filled:])
		filled += n
		if err != nil {
			if filled == len(line) && (err == io.EOF || lastRow && (errors.Is(err, zlib.ErrChecksum) || err == io.ErrUnexpectedEOF || err.Error() == "brotli: excessive input")) {
				if err == io.EOF {
					return nil
				}
				return err
			}
			if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
				return fmt.Errorf("image sample size mismatch")
			}
			return err
		}
	}
	return nil
}

// checkStreamTrailingSamples 检查原始图像流读满声明行后的尾部多余字节
// 入参: ctx 取消上下文, reader 剩余数据源, warning 可恢复诊断, width 图像宽, height 图像高, limited 可选流字节限流器
// 返回: int64 多余字节总数, error 多余非零数据、超限、读取或取消错误
func checkStreamTrailingSamples(ctx context.Context, reader io.Reader, warning func(Diagnostic), width, height int, limited *io.LimitedReader) (int64, error) {
	var buf [4096]byte
	var excess int64
	nonZero := false
	for {
		if err := ctx.Err(); err != nil {
			return excess, err
		}
		n, err := reader.Read(buf[:])
		if n > 0 {
			excess += int64(n)
			if limited != nil && limited.N == 0 {
				return excess, ErrStreamLimitExceeded
			}
			if !nonZero {
				for _, b := range buf[:n] {
					if b != 0 {
						nonZero = true
						break
					}
				}
			}
			if nonZero && warning == nil {
				return excess, fmt.Errorf("image sample size mismatch")
			}
		}
		if err != nil {
			if err == io.EOF {
				if limited != nil && limited.N == 0 {
					return excess, ErrStreamLimitExceeded
				}
				break
			}
			if nonZero && warning != nil {
				warning(Diagnostic{Message: fmt.Sprintf("image has %d excess sample bytes; decoded using declared dimensions %dx%d", excess, width, height)})
			}
			return excess, err
		}
	}
	if nonZero && warning != nil {
		warning(Diagnostic{Message: fmt.Sprintf("image has %d excess sample bytes; decoded using declared dimensions %dx%d", excess, width, height)})
	}
	return excess, ctx.Err()
}

// downsampleRawStreamContext 对常见单层通用流执行流式逐行盒式降采样，不分配全尺寸原始样本缓冲
// 入参: ctx 取消上下文, filters 生效过滤器, parameters 对应参数, target 目标尺寸
// 返回: image.Image 降采样图像, bool 是否已由流式路径处理, error 解码或取消错误
func (i *Image) downsampleRawStreamContext(ctx context.Context, filters, parameters Array, target image.Point) (image.Image, bool, error) {
	if target.X <= 0 || target.Y <= 0 {
		return nil, false, nil
	}
	target = boundedSampleSize(i.Width, i.Height, target)
	if target.X >= i.Width && target.Y >= i.Height {
		return nil, false, nil
	}
	components, err := i.rawSampleComponents()
	if err != nil {
		return nil, false, err
	}
	stride, _, err := imageSampleSize(i.Width, i.Height, components, i.BitsPerComponent)
	if err != nil {
		return nil, false, err
	}
	filterName := Name("")
	var filterParams Dictionary
	if len(filters) == 1 {
		filterName = filters[0].(Name)
		filterParams, _ = parameters[0].(Dictionary)
	} else if len(filters) > 1 {
		return nil, false, nil
	}
	maxBytes, _ := effectiveStreamLimits(i.reader, i.Stream)
	declaredBytes := int64(stride) * int64(i.Height)
	var reader io.Reader
	var flateCloser io.ReadCloser
	var brotliReader *brotli.Reader
	var limited *io.LimitedReader
	flateClosed := false
	isFlate := false
	isBrotli := false
	brotliFailed := true
	switch filterName {
	case "", "Crypt":
		if filterName == "Crypt" && filterParams["Name"] != nil && filterParams["Name"] != Name("Identity") {
			return nil, false, &UnsupportedError{Feature: "stream crypt filter"}
		}
		if maxBytes > 0 && (declaredBytes > maxBytes || int64(len(i.Stream.Data)) > maxBytes) {
			return nil, true, ErrStreamLimitExceeded
		}
		reader = bytes.NewReader(i.Stream.Data)
	case "FlateDecode":
		predictor, err := integerDefault(filterParams, "Predictor", 1)
		if err != nil || predictor != 1 {
			return nil, false, nil
		}
		if maxBytes > 0 && declaredBytes > maxBytes {
			return nil, true, ErrStreamLimitExceeded
		}
		flateCloser, err = acquireZlibReader(bytes.NewReader(i.Stream.Data))
		if err != nil {
			return nil, false, nil
		}
		defer func() {
			if !flateClosed {
				_ = flateCloser.Close()
			}
			releaseZlibReader(flateCloser)
		}()
		reader = &contextInput{ctx: ctx, reader: flateCloser}
		isFlate = true
	case "BrotliDecode":
		predictor, err := integerDefault(filterParams, "Predictor", 1)
		if err != nil || predictor != 1 {
			return nil, false, nil
		}
		if maxBytes > 0 && declaredBytes > maxBytes {
			return nil, true, ErrStreamLimitExceeded
		}
		brotliReader = acquireBrotliReader(&contextInput{ctx: ctx, reader: bytes.NewReader(i.Stream.Data)})
		reader = &contextInput{ctx: ctx, reader: brotliReader}
		isBrotli = true
	case "RunLengthDecode":
		if maxBytes > 0 && declaredBytes > maxBytes {
			return nil, true, ErrStreamLimitExceeded
		}
		reader = &runLengthStreamReader{ctx: ctx, data: i.Stream.Data, warning: i.Warning}
	default:
		return nil, false, nil
	}
	if maxBytes > 0 && maxBytes < math.MaxInt64 {
		limited = &io.LimitedReader{R: reader, N: maxBytes + 1}
		reader = limited
	}
	if isBrotli {
		defer func() {
			decodedBytes := declaredBytes
			if limited != nil {
				decodedBytes = maxBytes + 1 - limited.N
			}
			releaseBrotliReader(brotliReader, decodedBytes, brotliFailed)
		}()
	}
	result, trailingErr, err := i.downsampleRawReaderContext(ctx, reader, components, stride, target, isFlate || isBrotli)
	if canceled := ctx.Err(); canceled != nil {
		return nil, true, canceled
	}
	if limited != nil && limited.N == 0 {
		return nil, true, ErrStreamLimitExceeded
	}
	if err != nil {
		return nil, true, err
	}
	var excessBytes int64
	if trailingErr == nil {
		excessBytes, trailingErr = checkStreamTrailingSamples(ctx, reader, i.Warning, i.Width, i.Height, limited)
	}
	if limited != nil && limited.N == 0 {
		return nil, true, ErrStreamLimitExceeded
	}
	if isFlate {
		closeErr := flateCloser.Close()
		flateClosed = true
		if trailingErr == nil {
			trailingErr = closeErr
		}
		if trailingErr != nil && i.Warning != nil && ctx.Err() == nil && (errors.Is(trailingErr, zlib.ErrChecksum) || trailingErr == io.ErrUnexpectedEOF && completeFlatePayload(ctx, i.Stream.Data, declaredBytes+excessBytes)) {
			message := "Flate checksum truncated; complete compressed data retained"
			if errors.Is(trailingErr, zlib.ErrChecksum) {
				message = "Flate checksum mismatch; complete compressed data retained"
			}
			i.Warning(Diagnostic{Message: message})
			trailingErr = nil
		}
	}
	if isBrotli && trailingErr != nil && i.Warning != nil && ctx.Err() == nil && trailingErr.Error() == "brotli: excessive input" {
		i.Warning(Diagnostic{Message: "Brotli stream has trailing bytes; complete compressed data retained"})
		trailingErr = nil
	}
	if trailingErr != nil {
		return nil, true, trailingErr
	}
	brotliFailed = false
	return result, true, ctx.Err()
}

// downsampleRawReaderContext 按目标网格逐行读取原始样本并做盒式区域平均
// 入参: ctx 取消上下文, reader 原始样本流, components 分量数, stride 源行字节数, target 目标尺寸, allowChecksum 是否延迟处理末行校验和错误
// 返回: image.Image 降采样样本图, error 末行校验和延迟错误, error 解码、内存或取消错误
func (i *Image) downsampleRawReaderContext(ctx context.Context, reader io.Reader, components, stride int, target image.Point, allowChecksum bool) (image.Image, error, error) {
	srcW, srcH := i.Width, i.Height
	target = boundedSampleSize(srcW, srcH, target)
	dstW, dstH := target.X, target.Y
	depth := i.BitsPerComponent
	xStarts := downsampleBounds(srcW, dstW)
	var outGray *image.Gray
	var outGray16 *image.Gray16
	var outNRGBA *image.NRGBA
	var outNRGBA64 *image.NRGBA64
	var outCMYK *packedCMYKImage
	var outDeviceN *packedDeviceNImage
	rect := image.Rect(0, 0, dstW, dstH)
	switch {
	case components == 1 && depth <= 8:
		if _, _, err := imageBufferSize(dstW, dstH, 1); err != nil {
			return nil, nil, err
		}
		outGray = image.NewGray(rect)
	case components == 1 && depth == 16:
		if _, _, err := imageBufferSize(dstW, dstH, 2); err != nil {
			return nil, nil, err
		}
		outGray16 = image.NewGray16(rect)
	case components == 3 && depth == 8:
		if _, _, err := imageBufferSize(dstW, dstH, 4); err != nil {
			return nil, nil, err
		}
		outNRGBA = image.NewNRGBA(rect)
	case components == 2 || components == 3:
		if _, _, err := imageBufferSize(dstW, dstH, 8); err != nil {
			return nil, nil, err
		}
		outNRGBA64 = image.NewNRGBA64(rect)
	case components == 4:
		outDepth := 16
		if depth == 8 {
			outDepth = 8
		}
		outStride, outBytes, err := imageSampleSize(dstW, dstH, 4, outDepth)
		if err != nil {
			return nil, nil, err
		}
		outCMYK = &packedCMYKImage{data: make([]byte, outBytes), rect: rect, stride: outStride, depth: outDepth}
	default:
		outStride, outBytes, err := imageSampleSize(dstW, dstH, components, 16)
		if err != nil {
			return nil, nil, err
		}
		outDeviceN = &packedDeviceNImage{data: make([]byte, outBytes), rect: rect, stride: outStride, depth: 16, channels: components}
	}
	if _, _, err := imageBufferSize(dstW, components, 8); err != nil {
		return nil, nil, err
	}
	line := make([]byte, stride)
	acc := make([]uint64, dstW*components)
	maximum := uint64(1)<<depth - 1
	scale16 := uint64(65535) / maximum
	var trailingErr error
	for dy := 0; dy < dstH; dy++ {
		if err := ctx.Err(); err != nil {
			return nil, nil, err
		}
		y0 := int(int64(dy) * int64(srcH) / int64(dstH))
		y1 := int(int64(dy+1) * int64(srcH) / int64(dstH))
		clear(acc)
		for sy := y0; sy < y1; sy++ {
			if sy&63 == 0 {
				if err := ctx.Err(); err != nil {
					return nil, nil, err
				}
			}
			err := readSampleRow(reader, line, allowChecksum && sy == srcH-1)
			if err != nil {
				if allowChecksum && sy == srcH-1 && (errors.Is(err, zlib.ErrChecksum) || err == io.ErrUnexpectedEOF || err.Error() == "brotli: excessive input") {
					trailingErr = err
				} else {
					return nil, nil, err
				}
			}
			switch {
			case components == 1 && depth == 1:
				for dx := 0; dx < dstW; dx++ {
					acc[dx] += countBitsRange(line, xStarts[dx], xStarts[dx+1])
				}
			case components == 1 && depth == 8:
				for dx := 0; dx < dstW; dx++ {
					var sum uint64
					for _, v := range line[xStarts[dx]:xStarts[dx+1]] {
						sum += uint64(v)
					}
					acc[dx] += sum
				}
			case components == 3 && depth == 8:
				for dx := 0; dx < dstW; dx++ {
					var r, g, b uint64
					seg := line[xStarts[dx]*3 : xStarts[dx+1]*3]
					for pos := 0; pos < len(seg); pos += 3 {
						r += uint64(seg[pos])
						g += uint64(seg[pos+1])
						b += uint64(seg[pos+2])
					}
					base := dx * 3
					acc[base] += r
					acc[base+1] += g
					acc[base+2] += b
				}
			case components == 1 && depth == 16:
				for dx := 0; dx < dstW; dx++ {
					var sum uint64
					for sx := xStarts[dx]; sx < xStarts[dx+1]; sx++ {
						sum += uint64(binary.BigEndian.Uint16(line[sx*2:]))
					}
					acc[dx] += sum
				}
			default:
				for dx := 0; dx < dstW; dx++ {
					base := dx * components
					for sx := xStarts[dx]; sx < xStarts[dx+1]; sx++ {
						offset := int64(sx) * int64(components)
						for c := 0; c < components; c++ {
							acc[base+c] += uint64(packedSample(line, offset+int64(c), depth))
						}
					}
				}
			}
		}
		rowsCount := uint64(y1 - y0)
		switch {
		case outGray != nil:
			row := outGray.Pix[dy*outGray.Stride:]
			for dx := 0; dx < dstW; dx++ {
				count := uint64(xStarts[dx+1]-xStarts[dx]) * rowsCount
				if depth == 8 {
					row[dx] = uint8((acc[dx] + count/2) / count)
				} else {
					denom := count * maximum
					row[dx] = uint8((acc[dx]*255 + denom/2) / denom)
				}
			}
		case outGray16 != nil:
			row := outGray16.Pix[dy*outGray16.Stride:]
			for dx := 0; dx < dstW; dx++ {
				count := uint64(xStarts[dx+1]-xStarts[dx]) * rowsCount
				binary.BigEndian.PutUint16(row[dx*2:], uint16((acc[dx]+count/2)/count))
			}
		case outNRGBA != nil:
			row := outNRGBA.Pix[dy*outNRGBA.Stride:]
			for dx := 0; dx < dstW; dx++ {
				count := uint64(xStarts[dx+1]-xStarts[dx]) * rowsCount
				base := dx * 3
				row[dx*4] = uint8((acc[base] + count/2) / count)
				row[dx*4+1] = uint8((acc[base+1] + count/2) / count)
				row[dx*4+2] = uint8((acc[base+2] + count/2) / count)
				row[dx*4+3] = 255
			}
		case outNRGBA64 != nil:
			for dx := 0; dx < dstW; dx++ {
				count := uint64(xStarts[dx+1]-xStarts[dx]) * rowsCount
				base := dx * components
				var vals [3]uint16
				for c := 0; c < components; c++ {
					vals[c] = uint16((acc[base+c]*scale16 + count/2) / count)
				}
				outNRGBA64.SetNRGBA64(dx, dy, color.NRGBA64{R: vals[0], G: vals[1], B: vals[2], A: 65535})
			}
		case outCMYK != nil:
			row := outCMYK.data[dy*outCMYK.stride:]
			for dx := 0; dx < dstW; dx++ {
				count := uint64(xStarts[dx+1]-xStarts[dx]) * rowsCount
				base := dx * 4
				if outCMYK.depth == 8 {
					for c := 0; c < 4; c++ {
						row[dx*4+c] = uint8((acc[base+c] + count/2) / count)
					}
				} else {
					for c := 0; c < 4; c++ {
						binary.BigEndian.PutUint16(row[dx*8+c*2:], uint16((acc[base+c]*scale16+count/2)/count))
					}
				}
			}
		case outDeviceN != nil:
			row := outDeviceN.data[dy*outDeviceN.stride:]
			for dx := 0; dx < dstW; dx++ {
				count := uint64(xStarts[dx+1]-xStarts[dx]) * rowsCount
				base := dx * components
				for c := 0; c < components; c++ {
					binary.BigEndian.PutUint16(row[(dx*components+c)*2:], uint16((acc[base+c]*scale16+count/2)/count))
				}
			}
		}
	}
	switch {
	case outGray != nil:
		return outGray, trailingErr, nil
	case outGray16 != nil:
		return outGray16, trailingErr, nil
	case outNRGBA != nil:
		return outNRGBA, trailingErr, nil
	case outNRGBA64 != nil:
		return outNRGBA64, trailingErr, nil
	case outCMYK != nil:
		return outCMYK, trailingErr, nil
	default:
		return outDeviceN, trailingErr, nil
	}
}

// downsampleYCbCr 直接在YCbCr采样平面执行盒式区域平均，并在需要时转换为RGB
// 入参: ctx 取消上下文, src YCbCr图像, target 目标尺寸, convertRGB 是否执行YCbCr到RGB转换
// 返回: *image.NRGBA 降采样后的三通道图像, error 内存或取消错误
func downsampleYCbCr(ctx context.Context, src *image.YCbCr, target image.Point, convertRGB bool) (*image.NRGBA, error) {
	b := src.Bounds()
	srcW, srcH := b.Dx(), b.Dy()
	target = boundedSampleSize(srcW, srcH, target)
	dstW, dstH := target.X, target.Y
	if _, _, err := imageBufferSize(dstW, dstH, 4); err != nil {
		return nil, err
	}
	out := image.NewNRGBA(image.Rect(0, 0, dstW, dstH))
	xStarts := downsampleBounds(srcW, dstW)
	shiftX, shiftY := uint(0), uint(0)
	switch src.SubsampleRatio {
	case image.YCbCrSubsampleRatio422:
		shiftX = 1
	case image.YCbCrSubsampleRatio420:
		shiftX, shiftY = 1, 1
	case image.YCbCrSubsampleRatio440:
		shiftY = 1
	case image.YCbCrSubsampleRatio411:
		shiftX = 2
	case image.YCbCrSubsampleRatio410:
		shiftX, shiftY = 2, 1
	}
	for dy := 0; dy < dstH; dy++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		y0 := int(int64(dy) * int64(srcH) / int64(dstH))
		y1 := int(int64(dy+1) * int64(srcH) / int64(dstH))
		cy0 := (b.Min.Y + y0) >> shiftY
		cy1 := (b.Min.Y + y1 - 1) >> shiftY
		outRow := out.Pix[dy*out.Stride:]
		for dx := 0; dx < dstW; dx++ {
			x0, x1 := xStarts[dx], xStarts[dx+1]
			var sumY uint64
			for sy := y0; sy < y1; sy++ {
				rowOff := src.YOffset(b.Min.X+x0, b.Min.Y+sy)
				for _, v := range src.Y[rowOff : rowOff+(x1-x0)] {
					sumY += uint64(v)
				}
			}
			yCount := uint64(x1-x0) * uint64(y1-y0)
			avgY := uint8((sumY + yCount/2) / yCount)
			cx0 := (b.Min.X + x0) >> shiftX
			cx1 := (b.Min.X + x1 - 1) >> shiftX
			var sumCb, sumCr uint64
			for cy := cy0; cy <= cy1; cy++ {
				cRow := (cy-(b.Min.Y>>shiftY))*src.CStride + (cx0 - (b.Min.X >> shiftX))
				n := cx1 - cx0 + 1
				for _, v := range src.Cb[cRow : cRow+n] {
					sumCb += uint64(v)
				}
				for _, v := range src.Cr[cRow : cRow+n] {
					sumCr += uint64(v)
				}
			}
			cCount := uint64(cx1-cx0+1) * uint64(cy1-cy0+1)
			avgCb := uint8((sumCb + cCount/2) / cCount)
			avgCr := uint8((sumCr + cCount/2) / cCount)
			var r, g, bl uint8
			if convertRGB {
				r, g, bl = color.YCbCrToRGB(avgY, avgCb, avgCr)
			} else {
				r, g, bl = avgY, avgCb, avgCr
			}
			outRow[dx*4] = r
			outRow[dx*4+1] = g
			outRow[dx*4+2] = bl
			outRow[dx*4+3] = 255
		}
	}
	return out, ctx.Err()
}

// packedCMYKAImage 保留十六位四色分量及独立透明度，供带内嵌遮罩的四色降采样使用
type packedCMYKAImage struct {
	data   []byte
	rect   image.Rectangle
	stride int
}

// packedDeviceNAImage 按行保留十六位专色分量及独立透明度，供带内嵌遮罩的专色降采样使用
type packedDeviceNAImage struct {
	data             []byte
	rect             image.Rectangle
	stride, channels int
}

// packedDeviceNASample 按需读取带独立透明度的十六位专色像素
type packedDeviceNASample struct {
	source *packedDeviceNAImage
	x, y   int
}

// ColorModel 返回非预乘颜色模型
// 返回: color.Model 颜色模型
func (s *packedCMYKAImage) ColorModel() color.Model { return color.NRGBA64Model }

// Bounds 返回带透明度的四色图像边界
// 返回: image.Rectangle 图像边界
func (s *packedCMYKAImage) Bounds() image.Rectangle { return s.rect }

// At 读取十六位四色分量及独立透明度，边界外返回零值样本
// 入参: x 横向坐标, y 纵向坐标
// 返回: color.Color 四色透明样本
func (s *packedCMYKAImage) At(x, y int) color.Color {
	if !image.Pt(x, y).In(s.rect) {
		return jpxSample{}
	}
	return s.sample(x, y)
}

// sample 读取已校验边界内的四色分量及独立透明度
// 入参: x 横向坐标, y 纵向坐标
// 返回: jpxSample 四色透明样本
func (s *packedCMYKAImage) sample(x, y int) jpxSample {
	off := (y-s.rect.Min.Y)*s.stride + (x-s.rect.Min.X)*10
	return jpxSample{
		values: cmykSample{
			binary.BigEndian.Uint16(s.data[off:]),
			binary.BigEndian.Uint16(s.data[off+2:]),
			binary.BigEndian.Uint16(s.data[off+4:]),
			binary.BigEndian.Uint16(s.data[off+6:]),
		},
		alpha: binary.BigEndian.Uint16(s.data[off+8:]),
		cmyk:  true,
	}
}

// ColorModel 返回非预乘颜色模型
// 返回: color.Model 颜色模型
func (s *packedDeviceNAImage) ColorModel() color.Model { return color.NRGBA64Model }

// Bounds 返回带透明度的专色图像边界
// 返回: image.Rectangle 图像边界
func (s *packedDeviceNAImage) Bounds() image.Rectangle { return s.rect }

// At 读取带独立透明度的专色样本，边界外返回透明色
// 入参: x 横向坐标, y 纵向坐标
// 返回: color.Color 专色透明样本
func (s *packedDeviceNAImage) At(x, y int) color.Color {
	if !image.Pt(x, y).In(s.rect) {
		return color.NRGBA64{}
	}
	return packedDeviceNASample{source: s, x: x, y: y}
}

// component 读取指定通道的十六位专色或透明度样本
// 入参: channel 通道索引
// 返回: uint16 样本值
func (s packedDeviceNASample) component(channel int) uint16 {
	off := (s.y-s.source.rect.Min.Y)*s.source.stride + ((s.x-s.source.rect.Min.X)*(s.source.channels+1)+channel)*2
	return binary.BigEndian.Uint16(s.source.data[off:])
}

// RGBA 返回前三个专色分量的预乘预览及独立透明度
// 返回: uint32 红、绿、蓝及透明度
func (s packedDeviceNASample) RGBA() (uint32, uint32, uint32, uint32) {
	alpha := uint32(s.component(s.source.channels))
	return uint32(s.component(0)) * alpha / 65535, uint32(s.component(1)) * alpha / 65535, uint32(s.component(2)) * alpha / 65535, alpha
}

// downsampleDecodedImage 对已解码图像按目标尺寸执行盒式区域平均降采样，保留原始通道布局
// 入参: ctx 取消上下文, src 源图像, target 目标尺寸
// 返回: image.Image 降采样后的图像, error 缓冲或取消错误
func downsampleDecodedImage(ctx context.Context, src image.Image, target image.Point) (image.Image, error) {
	return downsampleDecodedImageMode(ctx, src, target, false)
}

// downsampleDecodedImageMode 按预乘模式对已解码图像执行盒式区域平均降采样，保留原始通道布局
// 入参: ctx 取消上下文, src 源图像, target 目标尺寸, premultiplied 内嵌透明样本是否已预乘
// 返回: image.Image 降采样后的图像, error 缓冲或取消错误
func downsampleDecodedImageMode(ctx context.Context, src image.Image, target image.Point, premultiplied bool) (image.Image, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	b := src.Bounds()
	srcW, srcH := b.Dx(), b.Dy()
	if target.X <= 0 || target.Y <= 0 {
		return src, nil
	}
	target = boundedSampleSize(srcW, srcH, target)
	dstW, dstH := target.X, target.Y
	if dstW >= srcW && dstH >= srcH {
		return src, nil
	}
	xStarts := downsampleBounds(srcW, dstW)
	rect := image.Rect(0, 0, dstW, dstH)
	switch s := src.(type) {
	case *image.YCbCr:
		return downsampleYCbCr(ctx, s, target, true)
	case *image.Gray:
		if _, _, err := imageBufferSize(dstW, dstH, 1); err != nil {
			return nil, err
		}
		out := image.NewGray(rect)
		for dy := 0; dy < dstH; dy++ {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			y0, y1 := int(int64(dy)*int64(srcH)/int64(dstH)), int(int64(dy+1)*int64(srcH)/int64(dstH))
			outRow := out.Pix[dy*out.Stride:]
			for dx := 0; dx < dstW; dx++ {
				x0, x1 := xStarts[dx], xStarts[dx+1]
				var sum uint64
				for sy := y0; sy < y1; sy++ {
					off := s.PixOffset(b.Min.X+x0, b.Min.Y+sy)
					for _, v := range s.Pix[off : off+(x1-x0)] {
						sum += uint64(v)
					}
				}
				count := uint64(x1-x0) * uint64(y1-y0)
				outRow[dx] = uint8((sum + count/2) / count)
			}
		}
		return out, nil
	case *image.Gray16:
		if _, _, err := imageBufferSize(dstW, dstH, 2); err != nil {
			return nil, err
		}
		out := image.NewGray16(rect)
		for dy := 0; dy < dstH; dy++ {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			y0, y1 := int(int64(dy)*int64(srcH)/int64(dstH)), int(int64(dy+1)*int64(srcH)/int64(dstH))
			outRow := out.Pix[dy*out.Stride:]
			for dx := 0; dx < dstW; dx++ {
				x0, x1 := xStarts[dx], xStarts[dx+1]
				var sum uint64
				for sy := y0; sy < y1; sy++ {
					off := s.PixOffset(b.Min.X+x0, b.Min.Y+sy)
					for sx := 0; sx < x1-x0; sx++ {
						sum += uint64(binary.BigEndian.Uint16(s.Pix[off+sx*2:]))
					}
				}
				count := uint64(x1-x0) * uint64(y1-y0)
				binary.BigEndian.PutUint16(outRow[dx*2:], uint16((sum+count/2)/count))
			}
		}
		return out, nil
	case *packedGrayImage:
		if _, _, err := imageBufferSize(dstW, dstH, 1); err != nil {
			return nil, err
		}
		out := image.NewGray(rect)
		maximum := uint64(1)<<s.depth - 1
		for dy := 0; dy < dstH; dy++ {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			y0, y1 := int(int64(dy)*int64(srcH)/int64(dstH)), int(int64(dy+1)*int64(srcH)/int64(dstH))
			outRow := out.Pix[dy*out.Stride:]
			for dx := 0; dx < dstW; dx++ {
				x0, x1 := xStarts[dx], xStarts[dx+1]
				var sum uint64
				for sy := y0; sy < y1; sy++ {
					line := s.data[sy*s.stride:]
					if s.depth == 1 {
						sum += countBitsRange(line, x0, x1)
					} else {
						for sx := x0; sx < x1; sx++ {
							sum += uint64(packedSample(line, int64(sx), s.depth))
						}
					}
				}
				denom := uint64(x1-x0) * uint64(y1-y0) * maximum
				outRow[dx] = uint8((sum*255 + denom/2) / denom)
			}
		}
		return out, nil
	case *mappedGrayImage:
		if s.byteExact {
			if _, _, err := imageBufferSize(dstW, dstH, 1); err != nil {
				return nil, err
			}
			out := image.NewGray(rect)
			for dy := 0; dy < dstH; dy++ {
				if err := ctx.Err(); err != nil {
					return nil, err
				}
				y0, y1 := int(int64(dy)*int64(srcH)/int64(dstH)), int(int64(dy+1)*int64(srcH)/int64(dstH))
				outRow := out.Pix[dy*out.Stride:]
				for dx := 0; dx < dstW; dx++ {
					x0, x1 := xStarts[dx], xStarts[dx+1]
					var sum uint64
					for sy := y0; sy < y1; sy++ {
						line := s.source.data[sy*s.source.stride:]
						for sx := x0; sx < x1; sx++ {
							sum += uint64(s.lookup[packedSample(line, int64(sx), s.source.depth)] >> 8)
						}
					}
					count := uint64(x1-x0) * uint64(y1-y0)
					outRow[dx] = uint8((sum + count/2) / count)
				}
			}
			return out, nil
		}
		if _, _, err := imageBufferSize(dstW, dstH, 2); err != nil {
			return nil, err
		}
		out := image.NewGray16(rect)
		for dy := 0; dy < dstH; dy++ {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			y0, y1 := int(int64(dy)*int64(srcH)/int64(dstH)), int(int64(dy+1)*int64(srcH)/int64(dstH))
			outRow := out.Pix[dy*out.Stride:]
			for dx := 0; dx < dstW; dx++ {
				x0, x1 := xStarts[dx], xStarts[dx+1]
				var sum uint64
				for sy := y0; sy < y1; sy++ {
					line := s.source.data[sy*s.source.stride:]
					for sx := x0; sx < x1; sx++ {
						sum += uint64(s.lookup[packedSample(line, int64(sx), s.source.depth)])
					}
				}
				count := uint64(x1-x0) * uint64(y1-y0)
				binary.BigEndian.PutUint16(outRow[dx*2:], uint16((sum+count/2)/count))
			}
		}
		return out, nil
	case *image.NRGBA:
		if _, _, err := imageBufferSize(dstW, dstH, 4); err != nil {
			return nil, err
		}
		out := image.NewNRGBA(rect)
		for dy := 0; dy < dstH; dy++ {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			y0, y1 := int(int64(dy)*int64(srcH)/int64(dstH)), int(int64(dy+1)*int64(srcH)/int64(dstH))
			outRow := out.Pix[dy*out.Stride:]
			for dx := 0; dx < dstW; dx++ {
				x0, x1 := xStarts[dx], xStarts[dx+1]
				var sumR, sumG, sumB, sumA uint64
				for sy := y0; sy < y1; sy++ {
					off := s.PixOffset(b.Min.X+x0, b.Min.Y+sy)
					for pos := off; pos < off+(x1-x0)*4; pos += 4 {
						a := uint64(s.Pix[pos+3])
						sumR += uint64(s.Pix[pos]) * a
						sumG += uint64(s.Pix[pos+1]) * a
						sumB += uint64(s.Pix[pos+2]) * a
						sumA += a
					}
				}
				count := uint64(x1-x0) * uint64(y1-y0)
				if sumA == 0 {
					continue
				}
				outRow[dx*4] = uint8((sumR + sumA/2) / sumA)
				outRow[dx*4+1] = uint8((sumG + sumA/2) / sumA)
				outRow[dx*4+2] = uint8((sumB + sumA/2) / sumA)
				outRow[dx*4+3] = uint8((sumA + count/2) / count)
			}
		}
		return out, nil
	case *image.RGBA:
		if _, _, err := imageBufferSize(dstW, dstH, 4); err != nil {
			return nil, err
		}
		out := image.NewNRGBA(rect)
		for dy := 0; dy < dstH; dy++ {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			y0, y1 := int(int64(dy)*int64(srcH)/int64(dstH)), int(int64(dy+1)*int64(srcH)/int64(dstH))
			outRow := out.Pix[dy*out.Stride:]
			for dx := 0; dx < dstW; dx++ {
				x0, x1 := xStarts[dx], xStarts[dx+1]
				var sumR, sumG, sumB, sumA uint64
				for sy := y0; sy < y1; sy++ {
					off := s.PixOffset(b.Min.X+x0, b.Min.Y+sy)
					for pos := off; pos < off+(x1-x0)*4; pos += 4 {
						sumR += uint64(s.Pix[pos])
						sumG += uint64(s.Pix[pos+1])
						sumB += uint64(s.Pix[pos+2])
						sumA += uint64(s.Pix[pos+3])
					}
				}
				count := uint64(x1-x0) * uint64(y1-y0)
				if sumA == 0 {
					continue
				}
				outRow[dx*4] = uint8(min(255, (sumR*255+sumA/2)/sumA))
				outRow[dx*4+1] = uint8(min(255, (sumG*255+sumA/2)/sumA))
				outRow[dx*4+2] = uint8(min(255, (sumB*255+sumA/2)/sumA))
				outRow[dx*4+3] = uint8((sumA + count/2) / count)
			}
		}
		return out, nil
	case *packedCMYKImage:
		outDepth := 16
		if s.depth == 8 {
			outDepth = 8
		}
		outStride, outBytes, err := imageSampleSize(dstW, dstH, 4, outDepth)
		if err != nil {
			return nil, err
		}
		out := &packedCMYKImage{data: make([]byte, outBytes), rect: rect, stride: outStride, depth: outDepth}
		for dy := 0; dy < dstH; dy++ {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			y0, y1 := int(int64(dy)*int64(srcH)/int64(dstH)), int(int64(dy+1)*int64(srcH)/int64(dstH))
			outRow := out.data[dy*out.stride:]
			for dx := 0; dx < dstW; dx++ {
				x0, x1 := xStarts[dx], xStarts[dx+1]
				var sum [4]uint64
				for sy := y0; sy < y1; sy++ {
					if s.depth == 8 {
						row := s.data[sy*s.stride:]
						for pos := x0 * 4; pos < x1*4; pos += 4 {
							sum[0] += uint64(row[pos])
							sum[1] += uint64(row[pos+1])
							sum[2] += uint64(row[pos+2])
							sum[3] += uint64(row[pos+3])
						}
					} else {
						for sx := x0; sx < x1; sx++ {
							sample := s.sample(b.Min.X+sx, b.Min.Y+sy)
							for c := 0; c < 4; c++ {
								sum[c] += uint64(sample[c])
							}
						}
					}
				}
				count := uint64(x1-x0) * uint64(y1-y0)
				if outDepth == 8 {
					for c := 0; c < 4; c++ {
						outRow[dx*4+c] = uint8((sum[c] + count/2) / count)
					}
				} else {
					for c := 0; c < 4; c++ {
						binary.BigEndian.PutUint16(outRow[dx*8+c*2:], uint16((sum[c]+count/2)/count))
					}
				}
			}
		}
		return out, nil
	case *packedDeviceNImage:
		outStride, outBytes, err := imageSampleSize(dstW, dstH, s.channels, 16)
		if err != nil {
			return nil, err
		}
		out := &packedDeviceNImage{data: make([]byte, outBytes), rect: rect, stride: outStride, depth: 16, channels: s.channels}
		sum := make([]uint64, s.channels)
		for dy := 0; dy < dstH; dy++ {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			y0, y1 := int(int64(dy)*int64(srcH)/int64(dstH)), int(int64(dy+1)*int64(srcH)/int64(dstH))
			outRow := out.data[dy*out.stride:]
			for dx := 0; dx < dstW; dx++ {
				x0, x1 := xStarts[dx], xStarts[dx+1]
				clear(sum)
				for sy := y0; sy < y1; sy++ {
					for sx := x0; sx < x1; sx++ {
						sample := packedDeviceNSample{source: s, x: b.Min.X + sx, y: b.Min.Y + sy}
						for c := 0; c < s.channels; c++ {
							sum[c] += uint64(sample.component(c))
						}
					}
				}
				count := uint64(x1-x0) * uint64(y1-y0)
				for c := 0; c < s.channels; c++ {
					binary.BigEndian.PutUint16(outRow[(dx*s.channels+c)*2:], uint16((sum[c]+count/2)/count))
				}
			}
		}
		return out, nil
	case *jpxSampleImage:
		return downsampleJPXImage(ctx, s, target, xStarts, premultiplied)
	default:
		if _, _, err := imageBufferSize(dstW, dstH, 8); err != nil {
			return nil, err
		}
		out := image.NewNRGBA64(rect)
		for dy := 0; dy < dstH; dy++ {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			y0, y1 := int(int64(dy)*int64(srcH)/int64(dstH)), int(int64(dy+1)*int64(srcH)/int64(dstH))
			for dx := 0; dx < dstW; dx++ {
				x0, x1 := xStarts[dx], xStarts[dx+1]
				var sumR, sumG, sumB, sumA uint64
				for sy := y0; sy < y1; sy++ {
					for sx := x0; sx < x1; sx++ {
						p := imageNRGBA64At(src, b.Min.X+sx, b.Min.Y+sy)
						a := uint64(p.A)
						sumR += uint64(p.R) * a
						sumG += uint64(p.G) * a
						sumB += uint64(p.B) * a
						sumA += a
					}
				}
				count := uint64(x1-x0) * uint64(y1-y0)
				if sumA == 0 {
					continue
				}
				out.SetNRGBA64(dx, dy, color.NRGBA64{
					R: uint16(min(65535, (sumR+sumA/2)/sumA)),
					G: uint16(min(65535, (sumG+sumA/2)/sumA)),
					B: uint16(min(65535, (sumB+sumA/2)/sumA)),
					A: uint16((sumA + count/2) / count),
				})
			}
		}
		return out, nil
	}
}

// downsampleJPXImage 对已解码的JPEG2000分量图像执行盒式降采样，保留原始颜色通道及预乘语义
// 入参: ctx 取消上下文, s JPEG2000样本图, target 目标尺寸, xStarts 水平区间边界, premultiplied 内嵌透明是否已预乘
// 返回: image.Image 降采样后的样本图, error 缓冲或取消错误
func downsampleJPXImage(ctx context.Context, s *jpxSampleImage, target image.Point, xStarts []int, premultiplied bool) (image.Image, error) {
	if s.rows != nil {
		return s, nil
	}
	b := s.bounds
	srcH := b.Dy()
	dstW, dstH := target.X, target.Y
	rect := image.Rect(0, 0, dstW, dstH)
	channels := len(s.channels)
	if channels > 4 {
		if s.alpha >= 0 {
			outStride, outBytes, err := imageSampleSize(dstW, dstH, channels+1, 16)
			if err != nil {
				return nil, err
			}
			out := &packedDeviceNAImage{data: make([]byte, outBytes), rect: rect, stride: outStride, channels: channels}
			sum := make([]uint64, channels)
			for dy := 0; dy < dstH; dy++ {
				if err := ctx.Err(); err != nil {
					return nil, err
				}
				y0, y1 := int(int64(dy)*int64(srcH)/int64(dstH)), int(int64(dy+1)*int64(srcH)/int64(dstH))
				outRow := out.data[dy*out.stride:]
				for dx := 0; dx < dstW; dx++ {
					x0, x1 := xStarts[dx], xStarts[dx+1]
					clear(sum)
					var sumA uint64
					for sy := y0; sy < y1; sy++ {
						for sx := x0; sx < x1; sx++ {
							a := uint64(s.sample(s.alpha, b.Min.X+sx, b.Min.Y+sy))
							if premultiplied {
								for c, idx := range s.channels {
									sum[c] += uint64(s.sample(idx, b.Min.X+sx, b.Min.Y+sy))
								}
							} else {
								for c, idx := range s.channels {
									sum[c] += uint64(s.sample(idx, b.Min.X+sx, b.Min.Y+sy)) * a
								}
							}
							sumA += a
						}
					}
					count := uint64(x1-x0) * uint64(y1-y0)
					base := dx * (channels + 1) * 2
					if premultiplied {
						for c := 0; c < channels; c++ {
							binary.BigEndian.PutUint16(outRow[base+c*2:], uint16((sum[c]+count/2)/count))
						}
						binary.BigEndian.PutUint16(outRow[base+channels*2:], uint16((sumA+count/2)/count))
						continue
					}
					if sumA == 0 {
						continue
					}
					for c := 0; c < channels; c++ {
						binary.BigEndian.PutUint16(outRow[base+c*2:], uint16(min(65535, (sum[c]+sumA/2)/sumA)))
					}
					binary.BigEndian.PutUint16(outRow[base+channels*2:], uint16((sumA+count/2)/count))
				}
			}
			return out, nil
		}
		outStride, outBytes, err := imageSampleSize(dstW, dstH, channels, 16)
		if err != nil {
			return nil, err
		}
		out := &packedDeviceNImage{data: make([]byte, outBytes), rect: rect, stride: outStride, depth: 16, channels: channels}
		sum := make([]uint64, channels)
		for dy := 0; dy < dstH; dy++ {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			y0, y1 := int(int64(dy)*int64(srcH)/int64(dstH)), int(int64(dy+1)*int64(srcH)/int64(dstH))
			outRow := out.data[dy*out.stride:]
			for dx := 0; dx < dstW; dx++ {
				x0, x1 := xStarts[dx], xStarts[dx+1]
				clear(sum)
				for sy := y0; sy < y1; sy++ {
					for sx := x0; sx < x1; sx++ {
						for c, idx := range s.channels {
							sum[c] += uint64(s.sample(idx, b.Min.X+sx, b.Min.Y+sy))
						}
					}
				}
				count := uint64(x1-x0) * uint64(y1-y0)
				for c := 0; c < channels; c++ {
					binary.BigEndian.PutUint16(outRow[(dx*channels+c)*2:], uint16((sum[c]+count/2)/count))
				}
			}
		}
		return out, nil
	}
	if s.cmyk {
		if s.alpha >= 0 {
			outStride, outBytes, err := imageSampleSize(dstW, dstH, 5, 16)
			if err != nil {
				return nil, err
			}
			out := &packedCMYKAImage{data: make([]byte, outBytes), rect: rect, stride: outStride}
			for dy := 0; dy < dstH; dy++ {
				if err := ctx.Err(); err != nil {
					return nil, err
				}
				y0, y1 := int(int64(dy)*int64(srcH)/int64(dstH)), int(int64(dy+1)*int64(srcH)/int64(dstH))
				outRow := out.data[dy*out.stride:]
				for dx := 0; dx < dstW; dx++ {
					x0, x1 := xStarts[dx], xStarts[dx+1]
					var sum [4]uint64
					var sumA uint64
					for sy := y0; sy < y1; sy++ {
						for sx := x0; sx < x1; sx++ {
							p := s.pixel(b.Min.X+sx, b.Min.Y+sy)
							if premultiplied {
								for c := 0; c < 4; c++ {
									sum[c] += uint64(p.values[c])
								}
								sumA += uint64(p.alpha)
							} else {
								a := uint64(p.alpha)
								for c := 0; c < 4; c++ {
									sum[c] += uint64(p.values[c]) * a
								}
								sumA += a
							}
						}
					}
					count := uint64(x1-x0) * uint64(y1-y0)
					if premultiplied {
						for c := 0; c < 4; c++ {
							binary.BigEndian.PutUint16(outRow[dx*10+c*2:], uint16((sum[c]+count/2)/count))
						}
						binary.BigEndian.PutUint16(outRow[dx*10+8:], uint16((sumA+count/2)/count))
						continue
					}
					if sumA == 0 {
						continue
					}
					for c := 0; c < 4; c++ {
						binary.BigEndian.PutUint16(outRow[dx*10+c*2:], uint16(min(65535, (sum[c]+sumA/2)/sumA)))
					}
					binary.BigEndian.PutUint16(outRow[dx*10+8:], uint16((sumA+count/2)/count))
				}
			}
			return out, nil
		}
		outStride, outBytes, err := imageSampleSize(dstW, dstH, 4, 16)
		if err != nil {
			return nil, err
		}
		out := &packedCMYKImage{data: make([]byte, outBytes), rect: rect, stride: outStride, depth: 16}
		for dy := 0; dy < dstH; dy++ {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			y0, y1 := int(int64(dy)*int64(srcH)/int64(dstH)), int(int64(dy+1)*int64(srcH)/int64(dstH))
			outRow := out.data[dy*out.stride:]
			for dx := 0; dx < dstW; dx++ {
				x0, x1 := xStarts[dx], xStarts[dx+1]
				var sum [4]uint64
				for sy := y0; sy < y1; sy++ {
					for sx := x0; sx < x1; sx++ {
						p := s.pixel(b.Min.X+sx, b.Min.Y+sy)
						for c := 0; c < 4; c++ {
							sum[c] += uint64(p.values[c])
						}
					}
				}
				count := uint64(x1-x0) * uint64(y1-y0)
				for c := 0; c < 4; c++ {
					binary.BigEndian.PutUint16(outRow[dx*8+c*2:], uint16((sum[c]+count/2)/count))
				}
			}
		}
		return out, nil
	}
	if channels == 1 && s.alpha < 0 {
		if imageByteExact(s) {
			if _, _, err := imageBufferSize(dstW, dstH, 1); err != nil {
				return nil, err
			}
			out := image.NewGray(rect)
			for dy := 0; dy < dstH; dy++ {
				if err := ctx.Err(); err != nil {
					return nil, err
				}
				y0, y1 := int(int64(dy)*int64(srcH)/int64(dstH)), int(int64(dy+1)*int64(srcH)/int64(dstH))
				outRow := out.Pix[dy*out.Stride:]
				for dx := 0; dx < dstW; dx++ {
					x0, x1 := xStarts[dx], xStarts[dx+1]
					var sum uint64
					for sy := y0; sy < y1; sy++ {
						for sx := x0; sx < x1; sx++ {
							sum += uint64(s.sample(s.channels[0], b.Min.X+sx, b.Min.Y+sy) >> 8)
						}
					}
					count := uint64(x1-x0) * uint64(y1-y0)
					outRow[dx] = uint8((sum + count/2) / count)
				}
			}
			return out, nil
		}
		if _, _, err := imageBufferSize(dstW, dstH, 2); err != nil {
			return nil, err
		}
		out := image.NewGray16(rect)
		for dy := 0; dy < dstH; dy++ {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			y0, y1 := int(int64(dy)*int64(srcH)/int64(dstH)), int(int64(dy+1)*int64(srcH)/int64(dstH))
			outRow := out.Pix[dy*out.Stride:]
			for dx := 0; dx < dstW; dx++ {
				x0, x1 := xStarts[dx], xStarts[dx+1]
				var sum uint64
				for sy := y0; sy < y1; sy++ {
					for sx := x0; sx < x1; sx++ {
						sum += uint64(s.sample(s.channels[0], b.Min.X+sx, b.Min.Y+sy))
					}
				}
				count := uint64(x1-x0) * uint64(y1-y0)
				binary.BigEndian.PutUint16(outRow[dx*2:], uint16((sum+count/2)/count))
			}
		}
		return out, nil
	}
	if s.alpha < 0 && imageByteExact(s) {
		if _, _, err := imageBufferSize(dstW, dstH, 4); err != nil {
			return nil, err
		}
		out := image.NewNRGBA(rect)
		for dy := 0; dy < dstH; dy++ {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			y0, y1 := int(int64(dy)*int64(srcH)/int64(dstH)), int(int64(dy+1)*int64(srcH)/int64(dstH))
			outRow := out.Pix[dy*out.Stride:]
			for dx := 0; dx < dstW; dx++ {
				x0, x1 := xStarts[dx], xStarts[dx+1]
				var sumR, sumG, sumB uint64
				for sy := y0; sy < y1; sy++ {
					for sx := x0; sx < x1; sx++ {
						p := s.pixel(b.Min.X+sx, b.Min.Y+sy)
						sumR += uint64(p.values[0] >> 8)
						sumG += uint64(p.values[1] >> 8)
						sumB += uint64(p.values[2] >> 8)
					}
				}
				count := uint64(x1-x0) * uint64(y1-y0)
				outRow[dx*4] = uint8((sumR + count/2) / count)
				outRow[dx*4+1] = uint8((sumG + count/2) / count)
				outRow[dx*4+2] = uint8((sumB + count/2) / count)
				outRow[dx*4+3] = 255
			}
		}
		return out, nil
	}
	if _, _, err := imageBufferSize(dstW, dstH, 8); err != nil {
		return nil, err
	}
	out := image.NewNRGBA64(rect)
	if s.alpha < 0 {
		for dy := 0; dy < dstH; dy++ {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			y0, y1 := int(int64(dy)*int64(srcH)/int64(dstH)), int(int64(dy+1)*int64(srcH)/int64(dstH))
			for dx := 0; dx < dstW; dx++ {
				x0, x1 := xStarts[dx], xStarts[dx+1]
				var sumR, sumG, sumB uint64
				for sy := y0; sy < y1; sy++ {
					for sx := x0; sx < x1; sx++ {
						p := s.pixel(b.Min.X+sx, b.Min.Y+sy)
						sumR += uint64(p.values[0])
						sumG += uint64(p.values[1])
						sumB += uint64(p.values[2])
					}
				}
				count := uint64(x1-x0) * uint64(y1-y0)
				out.SetNRGBA64(dx, dy, color.NRGBA64{
					R: uint16((sumR + count/2) / count),
					G: uint16((sumG + count/2) / count),
					B: uint16((sumB + count/2) / count),
					A: 65535,
				})
			}
		}
		return out, nil
	}
	if premultiplied {
		for dy := 0; dy < dstH; dy++ {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			y0, y1 := int(int64(dy)*int64(srcH)/int64(dstH)), int(int64(dy+1)*int64(srcH)/int64(dstH))
			for dx := 0; dx < dstW; dx++ {
				x0, x1 := xStarts[dx], xStarts[dx+1]
				var sumR, sumG, sumB, sumA uint64
				for sy := y0; sy < y1; sy++ {
					for sx := x0; sx < x1; sx++ {
						p := s.pixel(b.Min.X+sx, b.Min.Y+sy)
						sumR += uint64(p.values[0])
						sumG += uint64(p.values[1])
						sumB += uint64(p.values[2])
						sumA += uint64(p.alpha)
					}
				}
				count := uint64(x1-x0) * uint64(y1-y0)
				out.SetNRGBA64(dx, dy, color.NRGBA64{
					R: uint16((sumR + count/2) / count),
					G: uint16((sumG + count/2) / count),
					B: uint16((sumB + count/2) / count),
					A: uint16((sumA + count/2) / count),
				})
			}
		}
		return out, nil
	}
	for dy := 0; dy < dstH; dy++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		y0, y1 := int(int64(dy)*int64(srcH)/int64(dstH)), int(int64(dy+1)*int64(srcH)/int64(dstH))
		for dx := 0; dx < dstW; dx++ {
			x0, x1 := xStarts[dx], xStarts[dx+1]
			var sumR, sumG, sumB, sumA uint64
			for sy := y0; sy < y1; sy++ {
				for sx := x0; sx < x1; sx++ {
					p := s.pixel(b.Min.X+sx, b.Min.Y+sy)
					a := uint64(p.alpha)
					sumR += uint64(p.values[0]) * a
					sumG += uint64(p.values[1]) * a
					sumB += uint64(p.values[2]) * a
					sumA += a
				}
			}
			count := uint64(x1-x0) * uint64(y1-y0)
			if sumA == 0 {
				continue
			}
			out.SetNRGBA64(dx, dy, color.NRGBA64{
				R: uint16(min(65535, (sumR+sumA/2)/sumA)),
				G: uint16(min(65535, (sumG+sumA/2)/sumA)),
				B: uint16(min(65535, (sumB+sumA/2)/sumA)),
				A: uint16((sumA + count/2) / count),
			})
		}
	}
	return out, nil
}

// downsampleIndexedImage 先按调色板展开索引样本再在颜色空间执行盒式降采样，禁止对索引值插值
// 入参: ctx 取消上下文, src 索引样本图, palette 调色板, lower Decode起点, upper Decode终点, target 目标尺寸
// 返回: image.Image 展开并降采样后的颜色图像, bool 调色板是否包含透明像素, error 缓冲或取消错误
func downsampleIndexedImage(ctx context.Context, src image.Image, palette *imagePalette, lower, upper float64, target image.Point) (image.Image, bool, error) {
	b := src.Bounds()
	srcW, srcH := b.Dx(), b.Dy()
	target = boundedSampleSize(srcW, srcH, target)
	dstW, dstH := target.X, target.Y
	hasAlpha := false
	byteExact := true
	for _, c := range palette.colors {
		if c.A != 65535 {
			hasAlpha = true
		}
		if c.R%257 != 0 || c.G%257 != 0 || c.B%257 != 0 || c.A%257 != 0 {
			byteExact = false
		}
	}
	maxIndex := float64(len(palette.colors) - 1)
	lookupColor := func(norm float64) color.NRGBA64 {
		v := functionValue(norm, lower, upper)
		index := int(math.Max(0, math.Min(maxIndex, math.Round(v))))
		return palette.colors[index]
	}
	graySrc, isGray := src.(*image.Gray)
	packedSrc, isPacked := src.(*packedGrayImage)
	var lut [256]color.NRGBA64
	if isPacked && packedSrc.depth <= 8 {
		maxVal := (1 << packedSrc.depth) - 1
		for n := 0; n <= maxVal; n++ {
			lut[n] = lookupColor(float64(uint32(n)*65535/uint32(maxVal)) / 65535)
		}
	} else if isGray {
		for n := 0; n < 256; n++ {
			lut[n] = lookupColor(float64(n) / 255)
		}
	}
	xStarts := downsampleBounds(srcW, dstW)
	rect := image.Rect(0, 0, dstW, dstH)
	if byteExact && !hasAlpha {
		if _, _, err := imageBufferSize(dstW, dstH, 4); err != nil {
			return nil, false, err
		}
		out := image.NewNRGBA(rect)
		for dy := 0; dy < dstH; dy++ {
			if err := ctx.Err(); err != nil {
				return nil, false, err
			}
			y0, y1 := int(int64(dy)*int64(srcH)/int64(dstH)), int(int64(dy+1)*int64(srcH)/int64(dstH))
			outRow := out.Pix[dy*out.Stride:]
			for dx := 0; dx < dstW; dx++ {
				x0, x1 := xStarts[dx], xStarts[dx+1]
				var sumR, sumG, sumB uint64
				for sy := y0; sy < y1; sy++ {
					switch {
					case isGray:
						off := graySrc.PixOffset(b.Min.X+x0, b.Min.Y+sy)
						for _, idx := range graySrc.Pix[off : off+(x1-x0)] {
							c := lut[idx]
							sumR += uint64(c.R >> 8)
							sumG += uint64(c.G >> 8)
							sumB += uint64(c.B >> 8)
						}
					case isPacked && packedSrc.depth <= 8:
						line := packedSrc.data[sy*packedSrc.stride:]
						for sx := x0; sx < x1; sx++ {
							c := lut[packedSample(line, int64(sx), packedSrc.depth)]
							sumR += uint64(c.R >> 8)
							sumG += uint64(c.G >> 8)
							sumB += uint64(c.B >> 8)
						}
					default:
						for sx := x0; sx < x1; sx++ {
							c := lookupColor(float64(imageNRGBA64At(src, b.Min.X+sx, b.Min.Y+sy).R) / 65535)
							sumR += uint64(c.R >> 8)
							sumG += uint64(c.G >> 8)
							sumB += uint64(c.B >> 8)
						}
					}
				}
				count := uint64(x1-x0) * uint64(y1-y0)
				outRow[dx*4] = uint8((sumR + count/2) / count)
				outRow[dx*4+1] = uint8((sumG + count/2) / count)
				outRow[dx*4+2] = uint8((sumB + count/2) / count)
				outRow[dx*4+3] = 255
			}
		}
		return out, false, nil
	}
	if _, _, err := imageBufferSize(dstW, dstH, 8); err != nil {
		return nil, false, err
	}
	out := image.NewNRGBA64(rect)
	for dy := 0; dy < dstH; dy++ {
		if err := ctx.Err(); err != nil {
			return nil, false, err
		}
		y0, y1 := int(int64(dy)*int64(srcH)/int64(dstH)), int(int64(dy+1)*int64(srcH)/int64(dstH))
		for dx := 0; dx < dstW; dx++ {
			x0, x1 := xStarts[dx], xStarts[dx+1]
			var sumR, sumG, sumB, sumA uint64
			for sy := y0; sy < y1; sy++ {
				switch {
				case isGray:
					off := graySrc.PixOffset(b.Min.X+x0, b.Min.Y+sy)
					for _, idx := range graySrc.Pix[off : off+(x1-x0)] {
						c := lut[idx]
						a := uint64(c.A)
						sumR += uint64(c.R) * a
						sumG += uint64(c.G) * a
						sumB += uint64(c.B) * a
						sumA += a
					}
				case isPacked && packedSrc.depth <= 8:
					line := packedSrc.data[sy*packedSrc.stride:]
					for sx := x0; sx < x1; sx++ {
						c := lut[packedSample(line, int64(sx), packedSrc.depth)]
						a := uint64(c.A)
						sumR += uint64(c.R) * a
						sumG += uint64(c.G) * a
						sumB += uint64(c.B) * a
						sumA += a
					}
				default:
					for sx := x0; sx < x1; sx++ {
						c := lookupColor(float64(imageNRGBA64At(src, b.Min.X+sx, b.Min.Y+sy).R) / 65535)
						a := uint64(c.A)
						sumR += uint64(c.R) * a
						sumG += uint64(c.G) * a
						sumB += uint64(c.B) * a
						sumA += a
					}
				}
			}
			count := uint64(x1-x0) * uint64(y1-y0)
			if sumA == 0 {
				continue
			}
			out.SetNRGBA64(dx, dy, color.NRGBA64{
				R: uint16(min(65535, (sumR+sumA/2)/sumA)),
				G: uint16(min(65535, (sumG+sumA/2)/sumA)),
				B: uint16(min(65535, (sumB+sumA/2)/sumA)),
				A: uint16((sumA + count/2) / count),
			})
		}
	}
	return out, hasAlpha, nil
}
