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
	"image"
	"image/color"
	"image/png"
	"io"
	"math"
)

// pngRowCacheLimit 限制单个重采样器的临时缓存，超限仍按原图像采样
const pngRowCacheLimit = 8 << 20

// pngContextWriter 在分块写入前检查取消及短写
type pngContextWriter struct {
	ctx    context.Context
	writer io.Writer
}

// pngChunkWriter 将压缩流分为定长IDAT块，不累积整幅编码数据
type pngChunkWriter struct {
	writer pngContextWriter
	data   []byte
}

// pngImageEncoder 复用单行过滤及压缩缓冲，取消或写入失败后可独立释放
type pngImageEncoder struct {
	output     pngContextWriter
	chunks     pngChunkWriter
	compressed *zlib.Writer
	optimized  bool
	row        []byte
	previous   []byte
	filters    [2][]byte
	pixelBytes int
}

// pngRowSampler 独立复用颜色及遮罩的重采样行，不展开整幅图像
type pngRowSampler struct {
	source image.Image
	device *deviceSampleImage
	colors *pngResampleRows
	mask   *pngResampleRows
}

// pngResampleRows 保存两行预乘样本及像素中心的横向映射
type pngResampleRows struct {
	resample *imageResample
	x        []pngSampleColumn
	rows     [2][]color.RGBA64
	y        [2]int
	valid    [2]bool
	weightY  float64
}

// pngSampleColumn 保存像素中心映射及相邻样本的插值比例
type pngSampleColumn struct {
	left   int
	right  int
	weight float64
}

// EncodePNG 按行无损编码图像，保留颜色精度及透明度，不展开整幅延迟图像
// 图像解码由调用方完成，取消或写入失败时输出可能不完整
// 入参: ctx 取消上下文, writer 输出流, source 已解码图像
// 返回: error 图像参数、取消或写入错误
func EncodePNG(ctx context.Context, writer io.Writer, source image.Image) error {
	if ctx == nil || writer == nil {
		return fmt.Errorf("missing PNG context or writer")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if source == nil {
		return fmt.Errorf("missing PNG image")
	}
	bounds := source.Bounds()
	if bounds.Dx() <= 0 || bounds.Dy() <= 0 || int64(bounds.Dx()) > 1<<31-1 || int64(bounds.Dy()) > 1<<31-1 {
		return fmt.Errorf("invalid PNG dimensions")
	}
	output := pngContextWriter{ctx: ctx, writer: writer}
	_, direct := source.(interface{ NRGBA64At(int, int) color.NRGBA64 })
	if _, packed := source.(*packedGrayImage); !direct && !packed {
		return png.Encode(output, source)
	}
	depth, channels, kind, err := pngSampleFormat(ctx, source)
	if err != nil {
		return err
	}
	sampler, err := newPNGRowSampler(source)
	if err != nil {
		return err
	}
	gray, _ := source.(*mappedGrayImage)
	var grayBytes [256]byte
	if gray != nil && depth == gray.source.depth && depth < 8 {
		grayBytes = pngGrayByteMap(gray, depth)
	}
	encoder, err := newPNGImageEncoder(ctx, writer, bounds, depth, channels, kind)
	if err != nil {
		return err
	}
	defer encoder.release()
	for y := bounds.Min.Y; y < bounds.Max.Y; y++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := sampler.prepare(ctx, y); err != nil {
			return err
		}
		row := encoder.row
		if gray != nil {
			if err := pngGrayRow(ctx, row, gray, y, depth, &grayBytes); err != nil {
				return err
			}
		} else {
			written, err := pngJPXRow(ctx, row, source, y, depth, channels)
			if err != nil {
				return err
			}
			if !written {
				for start := bounds.Min.X; start < bounds.Max.X; {
					if err := ctx.Err(); err != nil {
						return err
					}
					end := start + min(4096, bounds.Max.X-start)
					for x := start; x < end; x++ {
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
					start = end
				}
			}
		}
		if err := encoder.writeRow(); err != nil {
			return err
		}
	}
	return encoder.finish()
}

// newPNGImageEncoder 建立标准PNG行编码器，不分配整幅彩色缓冲
// 入参: ctx 取消上下文, writer 输出流, bounds 图像边界, depth 位深, channels 通道数, kind 颜色类型
// 返回: *pngImageEncoder 行编码器, error 参数或写入错误
func newPNGImageEncoder(ctx context.Context, writer io.Writer, bounds image.Rectangle, depth, channels, kind int) (*pngImageEncoder, error) {
	return newPNGImageEncoderWithCompression(ctx, writer, bounds, depth, channels, kind, false)
}

// newPNGImageEncoderWithCompression 建立独立行编码器，按用途选择压缩缓冲池
// 入参: ctx 取消上下文, writer 输出流, bounds 图像边界, depth 位深, channels 通道数, kind 颜色类型, optimized 是否使用最高无损压缩
// 返回: *pngImageEncoder 行编码器, error 参数或写入错误
func newPNGImageEncoderWithCompression(ctx context.Context, writer io.Writer, bounds image.Rectangle, depth, channels, kind int, optimized bool) (*pngImageEncoder, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if bounds.Dx() <= 0 || bounds.Dy() <= 0 || int64(bounds.Dx()) > 1<<31-1 || int64(bounds.Dy()) > 1<<31-1 {
		return nil, fmt.Errorf("invalid PNG dimensions")
	}
	stride, _, err := imageSampleSize(bounds.Dx(), 1, channels, depth)
	if err != nil || stride >= imageBufferLimit()/4 {
		return nil, fmt.Errorf("PNG row exceeds platform buffer range")
	}
	s := &pngImageEncoder{output: pngContextWriter{ctx: ctx, writer: writer}, pixelBytes: max(1, channels*depth/8), optimized: optimized}
	s.row, s.previous = make([]byte, stride), make([]byte, stride)
	for n := range s.filters {
		s.filters[n] = make([]byte, stride+1)
	}
	if _, err := s.output.Write([]byte("\x89PNG\r\n\x1a\n")); err != nil {
		return nil, err
	}
	var header [13]byte
	binary.BigEndian.PutUint32(header[:4], uint32(bounds.Dx()))
	binary.BigEndian.PutUint32(header[4:8], uint32(bounds.Dy()))
	header[8], header[9] = byte(depth), byte(kind)
	if err := writePNGChunk(s.output, "IHDR", header[:]); err != nil {
		return nil, err
	}
	s.chunks = pngChunkWriter{writer: s.output, data: make([]byte, 0, 32768)}
	if optimized {
		s.compressed = optimizationCompressor(&s.chunks)
	} else {
		s.compressed = imageCompressor(&s.chunks)
	}
	return s, nil
}

// writeRow 写入当前样本行并交换前后行缓冲，不保留已写像素
// 返回: error 取消、过滤或写入错误
func (s *pngImageEncoder) writeRow() error {
	filtered, err := pngFilterRow(s.output.ctx, s.row, s.previous, s.pixelBytes, &s.filters)
	if err != nil {
		return err
	}
	for len(filtered) != 0 {
		if err := s.output.ctx.Err(); err != nil {
			return err
		}
		n := min(32768, len(filtered))
		if _, err := s.compressed.Write(filtered[:n]); err != nil {
			return err
		}
		filtered = filtered[n:]
	}
	s.row, s.previous = s.previous, s.row
	return s.output.ctx.Err()
}

// finish 完成压缩数据及标准结束块，错误时不补写结束标记
// 返回: error 取消或写入错误
func (s *pngImageEncoder) finish() error {
	if err := s.output.ctx.Err(); err != nil {
		return err
	}
	if err := s.compressed.Close(); err != nil {
		return err
	}
	if err := s.chunks.flush(); err != nil {
		return err
	}
	return writePNGChunk(s.output, "IEND", nil)
}

// release 解除输出引用并归还压缩器，取消或写入失败后同样释放
func (s *pngImageEncoder) release() {
	if s.compressed != nil {
		if s.optimized {
			releaseOptimizationCompressor(s.compressed)
		} else {
			releaseImageCompressor(s.compressed)
		}
		s.compressed = nil
	}
}

// pngJPXRow 直接读取等横向采样的分量行，保持通道排序、调色板及透明度精度
// 入参: ctx 取消上下文, row 输出行, source 图像, y 行坐标, depth 输出位深, channels 输出通道数
// 返回: bool 是否完成直接采样, error 读取或取消错误
func pngJPXRow(ctx context.Context, row []byte, source image.Image, y, depth, channels int) (bool, error) {
	var samples *jpxSampleImage
	embedded := true
	switch s := source.(type) {
	case *jpxSampleImage:
		samples = s
	case *deviceSampleImage:
		if s.mask != nil || s.palette != nil {
			return false, nil
		}
		samples, _ = s.source.(*jpxSampleImage)
		embedded = s.embedded
	}
	if samples == nil || samples.bounds.Min != (image.Point{}) || samples.cmyk || samples.ycc || len(samples.channels) != 1 && len(samples.channels) != 3 {
		return false, nil
	}
	indices := [4]int{samples.channels[0], samples.channels[0], samples.channels[0], -1}
	if len(samples.channels) == 3 {
		copy(indices[:3], samples.channels)
	}
	if embedded {
		indices[3] = samples.alpha
	}
	width := samples.bounds.Dx()
	for _, index := range indices[:channels] {
		if index < 0 {
			continue
		}
		if len(samples.mapping) != 0 {
			index = samples.mapping[index].component
		}
		plane := samples.planes[index]
		if plane.Info().XStep != 1 || width > plane.Bounds().Dx() {
			return false, nil
		}
	}
	stride := channels * depth / 8
	var data [4096]int64
	for channel, index := range indices[:channels] {
		var palette []uint16
		var bias int64
		var maximum, step uint64
		var origin image.Point
		if index >= 0 {
			if len(samples.mapping) != 0 {
				palette = samples.mapping[index].palette
				index = samples.mapping[index].component
			}
			plane := samples.planes[index]
			spec, bounds := plane.Info(), samples.planeBounds(index)
			ratio := int64(spec.YStep)
			sy := min(bounds.Dy()-1, max(0, int((int64(y)+samples.y0)/ratio-(samples.y0+ratio-1)/ratio)))
			origin = bounds.Min.Add(image.Pt(0, sy))
			if spec.Signed {
				bias = int64(1) << (spec.Precision - 1)
			}
			maximum = uint64(1)<<spec.Precision - 1
			if 65535%maximum == 0 {
				step = 65535 / maximum
			}
		}
		for start := 0; start < width; start += 4096 {
			if err := ctx.Err(); err != nil {
				return false, err
			}
			if index >= 0 {
				if err := samples.planes[index].ReadSamples(data[:min(4096, width-start)], origin.X+start, origin.Y); err != nil {
					return false, err
				}
			}
			for x := start; x < min(start+4096, width); x++ {
				value := uint16(65535)
				if index >= 0 {
					raw := uint64(data[x-start] + bias)
					switch {
					case palette != nil:
						value = palette[min(raw, uint64(len(palette)-1))]
					case step != 0:
						value = uint16(raw * step)
					default:
						value = uint16((raw*65535 + maximum/2) / maximum)
					}
				}
				offset := x*stride + channel*depth/8
				if depth == 8 {
					row[offset] = byte(value >> 8)
				} else {
					binary.BigEndian.PutUint16(row[offset:], value)
				}
			}
		}
	}
	return true, nil
}

// pngSampleFormat 按颜色模型选择无损位深，已知不透明时省略透明度通道
// 入参: ctx 取消上下文, source 图像
// 返回: int 位深, int 通道数, int PNG颜色类型, error 取消错误
func pngSampleFormat(ctx context.Context, source image.Image) (int, int, int, error) {
	depth := 16
	model := source.ColorModel()
	if model == color.GrayModel || model == color.NRGBAModel || model == color.RGBAModel {
		depth = 8
	}
	if model == color.GrayModel || model == color.Gray16Model {
		if gray, ok := source.(*mappedGrayImage); ok && gray.byteExact {
			for _, bits := range []int{1, 2, 4} {
				step := uint16(65535 / (1<<bits - 1))
				exact := true
				for _, value := range gray.lookup[:1<<gray.source.depth] {
					if value%step != 0 {
						exact = false
						break
					}
				}
				if exact {
					return bits, 1, 0, nil
				}
			}
		}
		return depth, 1, 0, nil
	}
	switch s := source.(type) {
	case *image.NRGBA64:
		opaque, err := imageOpaqueContext(ctx, s)
		if err != nil {
			return 0, 0, 0, err
		}
		if opaque {
			return depth, 3, 2, nil
		}
	case *deviceSampleImage:
		if s.mask == nil && !s.embedded && s.palette == nil {
			return depth, 3, 2, nil
		}
	case *jpxSampleImage:
		if s.alpha < 0 {
			return depth, 3, 2, nil
		}
	}
	return depth, 4, 6, nil
}

// pngGrayByteMap 将同位深的灰度映射合并为逐字节查找表
// 入参: source 紧凑灰度图像, depth 输出位深
// 返回: [256]byte 字节映射表
func pngGrayByteMap(source *mappedGrayImage, depth int) (lookup [256]byte) {
	maximum := uint16(1<<depth - 1)
	step := uint16(65535 / maximum)
	for n := range lookup {
		for shift := 8 - depth; shift >= 0; shift -= depth {
			value := source.lookup[uint16(n>>shift)&maximum] / step
			lookup[n] |= byte(value << shift)
		}
	}
	return lookup
}

// pngGrayRow 按输出位深直接映射样本行，不展开整幅灰度图像
// 入参: ctx 取消上下文, row 输出行, source 紧凑图像, y 源行坐标, depth 输出位深, lookup 字节映射表
// 返回: error 取消错误
func pngGrayRow(ctx context.Context, row []byte, source *mappedGrayImage, y, depth int, lookup *[256]byte) error {
	width := source.Bounds().Dx()
	input := source.source.data[(y-source.Bounds().Min.Y)*source.source.stride:]
	if depth < 8 {
		if depth == source.source.depth {
			for n := range row {
				if n&4095 == 0 {
					if err := ctx.Err(); err != nil {
						return err
					}
				}
				row[n] = lookup[input[n]]
			}
		} else {
			clear(row)
			step := uint16(65535 / (1<<depth - 1))
			for x := 0; x < width; x++ {
				if x&4095 == 0 {
					if err := ctx.Err(); err != nil {
						return err
					}
				}
				value := source.lookup[packedSample(input, x, source.source.depth)] / step
				setPackedSample(row, x, depth, value)
			}
		}
		if remainder := width % (8 / depth) * depth; remainder != 0 {
			row[len(row)-1] &= byte(0xff << (8 - remainder))
		}
		return ctx.Err()
	}
	for x := 0; x < width; x++ {
		if x&4095 == 0 {
			if err := ctx.Err(); err != nil {
				return err
			}
		}
		value := source.lookup[packedSample(input, x, source.source.depth)]
		if depth == 8 {
			row[x] = byte(value >> 8)
		} else {
			binary.BigEndian.PutUint16(row[2*x:], value)
		}
	}
	return ctx.Err()
}

// pngFilterRow 比较五种标准行过滤器，重复行直接生成零残差，保留原选择顺序
// 入参: ctx 取消上下文, row 当前行, previous 上一行, pixelBytes 像素字节数, filters 复用缓冲
// 返回: []byte 带过滤器编号的样本行, error 取消错误
func pngFilterRow(ctx context.Context, row, previous []byte, pixelBytes int, filters *[2][]byte) ([]byte, error) {
	return pngFilterRowOrder(ctx, row, previous, pixelBytes, filters, [4]byte{2, 1, 4, 3})
}

// pngFilterRowOrder 按指定顺序比较PNG预测器，得分相同时保留先选结果
// 入参: ctx 取消上下文, row 当前行, previous 上一行, pixelBytes 像素字节数, filters 复用缓冲, order 非零预测器顺序
// 返回: []byte 带预测器编号的样本行, error 取消错误
func pngFilterRowOrder(ctx context.Context, row, previous []byte, pixelBytes int, filters *[2][]byte, order [4]byte) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if bytes.Equal(row, previous) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		clear(filters[0])
		if bytes.Count(row, []byte{0}) != len(row) {
			filters[0][0] = 2
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		return filters[0], nil
	}
	filters[0][0] = 0
	copy(filters[0][1:], row)
	var score uint64
	for start := 0; start < len(row); start += 4096 {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		for _, value := range row[start:min(start+4096, len(row))] {
			score += uint64(min(int(value), 256-int(value)))
		}
	}
	for _, kind := range order {
		if score == 0 {
			break
		}
		var candidate uint64
		filters[1][0] = byte(kind)
		output := filters[1][1:]
		prefix := min(pixelBytes, len(row))
		for index := 0; index < prefix; index++ {
			prediction := previous[index]
			if kind == 1 {
				prediction = 0
			} else if kind == 3 {
				prediction /= 2
			}
			residual := row[index] - prediction
			output[index] = residual
			candidate += uint64(min(int(residual), 256-int(residual)))
		}
		for start := prefix; start < len(row) && candidate < score; start += 128 {
			if (start-prefix)&4095 == 0 {
				if err := ctx.Err(); err != nil {
					return nil, err
				}
			}
			end := min(start+128, len(row))
			current, filtered := row[start:end], output[start:end]
			switch kind {
			case 1:
				left := row[start-pixelBytes:][:len(current)]
				for index, value := range current {
					residual := value - left[index]
					filtered[index] = residual
					candidate += uint64(min(int(residual), 256-int(residual)))
				}
			case 2:
				above := previous[start:end]
				for index, value := range current {
					residual := value - above[index]
					filtered[index] = residual
					candidate += uint64(min(int(residual), 256-int(residual)))
				}
			case 3:
				left, above := row[start-pixelBytes:][:len(current)], previous[start:end]
				for index, value := range current {
					residual := value - byte((uint16(left[index])+uint16(above[index]))/2)
					filtered[index] = residual
					candidate += uint64(min(int(residual), 256-int(residual)))
				}
			case 4:
				left, above, corner := row[start-pixelBytes:][:len(current)], previous[start:end], previous[start-pixelBytes:][:len(current)]
				for index, value := range current {
					residual := value - paeth(left[index], above[index], corner[index])
					filtered[index] = residual
					candidate += uint64(min(int(residual), 256-int(residual)))
				}
			}
		}
		if candidate < score {
			filters[0], filters[1] = filters[1], filters[0]
			score = candidate
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return filters[0], nil
}

// newPNGRowSampler 为颜色及遮罩建立独立缓存，其他图像保留直接采样
// 入参: source 图像
// 返回: pngRowSampler 行采样器, error 行缓冲尺寸错误
func newPNGRowSampler(source image.Image) (pngRowSampler, error) {
	s := pngRowSampler{source: source}
	device, ok := source.(*deviceSampleImage)
	if !ok {
		return s, nil
	}
	s.device = device
	var err error
	if r, ok := device.source.(*imageResample); ok {
		s.colors, err = newPNGResampleRows(r)
		if err != nil {
			return s, err
		}
	}
	if device.mask != nil {
		s.mask, err = newPNGResampleRows(device.mask)
	}
	return s, err
}

// newPNGResampleRows 为双线性插值建立有界行缓存，不改变超限图像的精度
// 入参: r 重采样图像
// 返回: *pngResampleRows 可选缓存, error 无效源尺寸
func newPNGResampleRows(r *imageResample) (*pngResampleRows, error) {
	if !r.interpolate || r.bounds == r.source.Bounds() {
		return nil, nil
	}
	switch raw := r.source.(type) {
	case *image.CMYK, *packedCMYKImage, *packedDeviceNImage, *imageResample:
		return nil, nil
	case *jpxSampleImage:
		if raw.cmyk || len(raw.channels) > 4 {
			return nil, nil
		}
	}
	b := r.source.Bounds()
	if b.Dx() <= 0 || b.Dy() <= 0 || r.bounds.Dx() <= 0 || r.bounds.Dy() <= 0 {
		return nil, fmt.Errorf("invalid PNG resampling dimensions")
	}
	if b.Dx() > pngRowCacheLimit/16 || r.bounds.Dx() > (pngRowCacheLimit-b.Dx()*16)/24 {
		return nil, nil
	}
	s := &pngResampleRows{resample: r}
	s.rows = [2][]color.RGBA64{make([]color.RGBA64, b.Dx()), make([]color.RGBA64, b.Dx())}
	s.x = make([]pngSampleColumn, r.bounds.Dx())
	for index := range s.x {
		coordinate := (float64(index)+.5)*float64(b.Dx())/float64(r.bounds.Dx()) - .5
		left := int(math.Floor(coordinate))
		s.x[index] = pngSampleColumn{left: min(b.Dx()-1, max(0, left)), right: min(b.Dx()-1, max(0, left+1)), weight: coordinate - float64(left)}
	}
	return s, nil
}

// prepare 更新颜色及遮罩行缓存，在读取样本期间检查取消
// 入参: ctx 取消上下文, y 输出行坐标
// 返回: error 取消错误
func (s *pngRowSampler) prepare(ctx context.Context, y int) error {
	if err := prepareImageRows(ctx, s.source, y, y+1); err != nil {
		return err
	}
	if s.colors != nil {
		if err := s.colors.prepare(ctx, y); err != nil {
			return err
		}
	}
	if s.mask != nil {
		return s.mask.prepare(ctx, y)
	}
	return ctx.Err()
}

// prepare 仅在源行变化时读取原始样本，取消后不继续填充缓存
// 入参: ctx 取消上下文, y 输出行坐标
// 返回: error 取消错误
func (s *pngResampleRows) prepare(ctx context.Context, y int) error {
	b := s.resample.source.Bounds()
	coordinate := (float64(y-s.resample.bounds.Min.Y)+.5)*float64(b.Dy())/float64(s.resample.bounds.Dy()) - .5
	low := int(math.Floor(coordinate))
	s.weightY = coordinate - float64(low)
	rows := [2]int{b.Min.Y + min(b.Dy()-1, max(0, low)), b.Min.Y + min(b.Dy()-1, max(0, low+1))}
	if s.valid[1] && s.y[1] == rows[0] {
		s.rows[0], s.rows[1] = s.rows[1], s.rows[0]
		s.y[0], s.y[1] = s.y[1], s.y[0]
		s.valid[0], s.valid[1] = s.valid[1], s.valid[0]
	}
	for n, row := range rows {
		if s.valid[n] && s.y[n] == row {
			continue
		}
		s.valid[n] = false
		for x := range s.rows[n] {
			if x&4095 == 0 {
				if err := ctx.Err(); err != nil {
					return err
				}
			}
			s.rows[n][x] = imageRGBA64At(s.resample.source, b.Min.X+x, row)
		}
		s.y[n], s.valid[n] = row, true
	}
	return ctx.Err()
}

// sample 使用缓存样本执行原有预乘插值和整数舍入，再合成遮罩
// 入参: x 横向坐标, y 纵向坐标
// 返回: color.NRGBA64 非预乘颜色
func (s *pngRowSampler) sample(x, y int) color.NRGBA64 {
	if s.device == nil {
		return imageNRGBA64At(s.source, x, y)
	}
	var pixel color.NRGBA64
	if s.colors != nil {
		pixel = s.colors.sample(x)
	} else {
		pixel = imageNRGBA64At(s.device.source, x, y)
	}
	alpha := uint32(65535)
	if s.mask != nil {
		alpha = uint32(s.mask.red(x))
	} else if s.device.mask != nil {
		alpha = uint32(imageRGBA64At(s.device.mask, x, y).R)
	}
	if s.device.mask != nil && s.device.inverted {
		alpha = 65535 - alpha
	}
	return s.device.combineAlpha(pixel, alpha)
}

// sample 使用缓存执行预乘插值，已知不透明时不重复插值覆盖率
// 入参: x 输出横向坐标
// 返回: color.NRGBA64 非预乘颜色
func (s *pngResampleRows) sample(x int) color.NRGBA64 {
	column := s.x[x-s.resample.bounds.Min.X]
	a, b, c, d := s.rows[0][column.left], s.rows[0][column.right], s.rows[1][column.left], s.rows[1][column.right]
	alpha := uint16(65535)
	if a.A != 65535 || b.A != 65535 || c.A != 65535 || d.A != 65535 {
		alpha = pngInterpolate(a.A, b.A, c.A, d.A, column.weight, s.weightY)
	}
	return imageUnpremultiply(color.RGBA64{
		R: pngInterpolate(a.R, b.R, c.R, d.R, column.weight, s.weightY),
		G: pngInterpolate(a.G, b.G, c.G, d.G, column.weight, s.weightY),
		B: pngInterpolate(a.B, b.B, c.B, d.B, column.weight, s.weightY),
		A: alpha,
	})
}

// red 读取遮罩插值后的预乘灰度，不对不透明灰度执行多余颜色计算
// 入参: x 输出横向坐标
// 返回: uint16 遮罩覆盖率
func (s *pngResampleRows) red(x int) uint16 {
	column := s.x[x-s.resample.bounds.Min.X]
	a, b, c, d := s.rows[0][column.left], s.rows[0][column.right], s.rows[1][column.left], s.rows[1][column.right]
	red := pngInterpolate(a.R, b.R, c.R, d.R, column.weight, s.weightY)
	if a.A == 65535 && b.A == 65535 && c.A == 65535 && d.A == 65535 {
		return red
	}
	alpha := pngInterpolate(a.A, b.A, c.A, d.A, column.weight, s.weightY)
	pixel := imageUnpremultiply(color.RGBA64{R: red, A: alpha})
	value, _, _, _ := pixel.RGBA()
	return uint16(value)
}

// pngInterpolate 按原有累加顺序插值非负样本，保留半值向上舍入
// 入参: a 左上样本, b 右上样本, c 左下样本, d 右下样本, x 横向比例, y 纵向比例
// 返回: uint16 插值样本
func pngInterpolate(a, b, c, d uint16, x, y float64) uint16 {
	value := float64(a) * (1 - x) * (1 - y)
	value += float64(b) * x * (1 - y)
	value += float64(c) * (1 - x) * y
	value += float64(d) * x * y
	integer := uint16(value)
	if value-float64(integer) >= .5 {
		integer++
	}
	return integer
}

// Write 在写入前后检查取消，不接受未报告错误的短写
// 入参: data 输出数据
// 返回: int 已写字节数, error 取消或写入错误
func (w pngContextWriter) Write(data []byte) (int, error) {
	if err := w.ctx.Err(); err != nil {
		return 0, err
	}
	n, err := w.writer.Write(data)
	if err == nil && n != len(data) {
		err = io.ErrShortWrite
	}
	if err == nil {
		err = w.ctx.Err()
	}
	return n, err
}

// Write 将压缩字节写入定长块缓冲，填充前检查取消，满块时直接输出
// 入参: data 压缩数据
// 返回: int 已接收字节数, error 写入错误
func (w *pngChunkWriter) Write(data []byte) (int, error) {
	if err := w.writer.ctx.Err(); err != nil {
		return 0, err
	}
	written := 0
	for len(data) != 0 {
		if err := w.writer.ctx.Err(); err != nil {
			return written, err
		}
		n := min(len(data), cap(w.data)-len(w.data))
		w.data = append(w.data, data[:n]...)
		data, written = data[n:], written+n
		if len(w.data) == cap(w.data) {
			if err := w.flush(); err != nil {
				return written, err
			}
		}
	}
	return written, nil
}

// flush 写出剩余IDAT数据，保留缓冲供后续块复用
// 返回: error 写入错误
func (w *pngChunkWriter) flush() error {
	if len(w.data) == 0 {
		return nil
	}
	if err := writePNGChunk(w.writer, "IDAT", w.data); err != nil {
		return err
	}
	w.data = w.data[:0]
	return nil
}
