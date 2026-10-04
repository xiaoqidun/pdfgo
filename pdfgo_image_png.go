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

// pngRowSampler 在逐行编码中复用双线性插值的源行和横向坐标
type pngRowSampler struct {
	source   image.Image
	device   *deviceSampleImage
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
	depth, channels, kind := pngSampleFormat(source)
	pixelBytes := max(1, channels*depth/8)
	stride, _, err := imageSampleSize(bounds.Dx(), 1, channels, depth)
	if err != nil || stride >= imageBufferLimit()/4 {
		return fmt.Errorf("PNG row exceeds platform buffer range")
	}
	row, previous := make([]byte, stride), make([]byte, stride)
	sampler, err := newPNGRowSampler(source)
	if err != nil {
		return err
	}
	gray, _ := source.(*mappedGrayImage)
	var grayBytes [256]byte
	if gray != nil && depth == gray.source.depth && depth < 8 {
		grayBytes = pngGrayByteMap(gray, depth)
	}
	var filters [2][]byte
	for n := range filters {
		filters[n] = make([]byte, stride+1)
	}
	if _, err := output.Write([]byte("\x89PNG\r\n\x1a\n")); err != nil {
		return err
	}
	var header [13]byte
	binary.BigEndian.PutUint32(header[:4], uint32(bounds.Dx()))
	binary.BigEndian.PutUint32(header[4:8], uint32(bounds.Dy()))
	header[8], header[9] = byte(depth), byte(kind)
	if err := writePNGChunk(output, "IHDR", header[:]); err != nil {
		return err
	}
	chunks := pngChunkWriter{writer: output, data: make([]byte, 0, 32768)}
	compressed := zlib.NewWriter(&chunks)
	for y := bounds.Min.Y; y < bounds.Max.Y; y++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		if gray != nil {
			pngGrayRow(row, gray, y, depth, &grayBytes)
		} else {
			sampler.prepare(y)
			for x := bounds.Min.X; x < bounds.Max.X; x++ {
				pixel := sampler.sample(x, y)
				values := [4]uint16{pixel.R, pixel.G, pixel.B, pixel.A}
				for c := 0; c < channels; c++ {
					offset := (x-bounds.Min.X)*pixelBytes + c*depth/8
					if depth == 8 {
						row[offset] = byte(values[c] >> 8)
					} else {
						binary.BigEndian.PutUint16(row[offset:], values[c])
					}
				}
			}
		}
		filtered := pngFilterRow(row, previous, pixelBytes, &filters)
		if _, err := compressed.Write(filtered); err != nil {
			return err
		}
		row, previous = previous, row
	}
	if err := compressed.Close(); err != nil {
		return err
	}
	if err := chunks.flush(); err != nil {
		return err
	}
	return writePNGChunk(output, "IEND", nil)
}

// pngSampleFormat 按颜色模型选择无损位深，已知不透明时省略透明度通道
// 入参: source 图像
// 返回: int 位深, int 通道数, int PNG颜色类型
func pngSampleFormat(source image.Image) (int, int, int) {
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
					return bits, 1, 0
				}
			}
		}
		return depth, 1, 0
	}
	switch s := source.(type) {
	case *image.NRGBA64:
		if s.Opaque() {
			return depth, 3, 2
		}
	case *deviceSampleImage:
		if s.mask == nil && !s.embedded && s.palette == nil {
			return depth, 3, 2
		}
	case *jpxSampleImage:
		if s.alpha < 0 {
			return depth, 3, 2
		}
	}
	return depth, 4, 6
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
// 入参: row 输出行, source 紧凑图像, y 源行坐标, depth 输出位深, lookup 字节映射表
func pngGrayRow(row []byte, source *mappedGrayImage, y, depth int, lookup *[256]byte) {
	width := source.Bounds().Dx()
	input := source.source.data[(y-source.Bounds().Min.Y)*source.source.stride:]
	if depth < 8 {
		if depth == source.source.depth {
			for n := range row {
				row[n] = lookup[input[n]]
			}
		} else {
			clear(row)
			step := uint16(65535 / (1<<depth - 1))
			for x := 0; x < width; x++ {
				value := source.lookup[packedSample(input, x, source.source.depth)] / step
				setPackedSample(row, x, depth, value)
			}
		}
		if remainder := width % (8 / depth) * depth; remainder != 0 {
			row[len(row)-1] &= byte(0xff << (8 - remainder))
		}
		return
	}
	for x := 0; x < width; x++ {
		value := source.lookup[packedSample(input, x, source.source.depth)]
		if depth == 8 {
			row[x] = byte(value >> 8)
		} else {
			binary.BigEndian.PutUint16(row[2*x:], value)
		}
	}
}

// pngFilterRow 比较五种标准行过滤器，使用有符号残差较小的结果
// 入参: row 当前行, previous 上一行, pixelBytes 像素字节数, filters 复用缓冲
// 返回: []byte 带过滤器编号的样本行
func pngFilterRow(row, previous []byte, pixelBytes int, filters *[2][]byte) []byte {
	filters[0][0] = 0
	copy(filters[0][1:], row)
	var score uint64
	for _, value := range row {
		score += uint64(min(int(value), 256-int(value)))
	}
	for _, kind := range [4]int{2, 1, 4, 3} {
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
			end := min(start+128, len(row))
			switch kind {
			case 1:
				for index := start; index < end; index++ {
					residual := row[index] - row[index-pixelBytes]
					output[index] = residual
					candidate += uint64(min(int(residual), 256-int(residual)))
				}
			case 2:
				for index := start; index < end; index++ {
					residual := row[index] - previous[index]
					output[index] = residual
					candidate += uint64(min(int(residual), 256-int(residual)))
				}
			case 3:
				for index := start; index < end; index++ {
					residual := row[index] - byte((uint16(row[index-pixelBytes])+uint16(previous[index]))/2)
					output[index] = residual
					candidate += uint64(min(int(residual), 256-int(residual)))
				}
			case 4:
				for index := start; index < end; index++ {
					residual := row[index] - paeth(row[index-pixelBytes], previous[index], previous[index-pixelBytes])
					output[index] = residual
					candidate += uint64(min(int(residual), 256-int(residual)))
				}
			}
		}
		if candidate < score {
			filters[0], filters[1] = filters[1], filters[0]
			score = candidate
		}
	}
	return filters[0]
}

// newPNGRowSampler 为设备色插值建立两行缓存，其他图像保留直接采样
// 入参: source 图像
// 返回: pngRowSampler 行采样器, error 行缓冲尺寸错误
func newPNGRowSampler(source image.Image) (pngRowSampler, error) {
	s := pngRowSampler{source: source}
	device, ok := source.(*deviceSampleImage)
	if !ok {
		return s, nil
	}
	r, ok := device.source.(*imageResample)
	if !ok || !r.interpolate || r.bounds == r.source.Bounds() {
		return s, nil
	}
	switch raw := r.source.(type) {
	case *image.CMYK, *packedCMYKImage, *packedDeviceNImage, *imageResample:
		return s, nil
	case *jpxSampleImage:
		if raw.cmyk || len(raw.channels) > 4 {
			return s, nil
		}
	}
	b := r.source.Bounds()
	if _, _, err := imageBufferSize(b.Dx(), 2, 8); err != nil {
		return s, err
	}
	if _, _, err := imageBufferSize(r.bounds.Dx(), 1, 24); err != nil {
		return s, err
	}
	s.device, s.resample = device, r
	s.rows = [2][]color.RGBA64{make([]color.RGBA64, b.Dx()), make([]color.RGBA64, b.Dx())}
	s.x = make([]pngSampleColumn, r.bounds.Dx())
	for index := range s.x {
		coordinate := (float64(index)+.5)*float64(b.Dx())/float64(r.bounds.Dx()) - .5
		left := int(math.Floor(coordinate))
		s.x[index] = pngSampleColumn{left: min(b.Dx()-1, max(0, left)), right: min(b.Dx()-1, max(0, left+1)), weight: coordinate - float64(left)}
	}
	return s, nil
}

// prepare 仅在源行变化时读取原始样本，不累积输出行
// 入参: y 输出行坐标
func (s *pngRowSampler) prepare(y int) {
	if s.resample == nil {
		return
	}
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
		for x := range s.rows[n] {
			s.rows[n][x] = imageRGBA64At(s.resample.source, b.Min.X+x, row)
		}
		s.y[n], s.valid[n] = row, true
	}
}

// sample 使用缓存样本执行原有预乘插值和整数舍入，再合成遮罩
// 入参: x 横向坐标, y 纵向坐标
// 返回: color.NRGBA64 非预乘颜色
func (s *pngRowSampler) sample(x, y int) color.NRGBA64 {
	if s.resample == nil {
		return imageNRGBA64At(s.source, x, y)
	}
	column := s.x[x-s.resample.bounds.Min.X]
	a, b, c, d := s.rows[0][column.left], s.rows[0][column.right], s.rows[1][column.left], s.rows[1][column.right]
	result := imageUnpremultiply(color.RGBA64{
		R: pngInterpolate(a.R, b.R, c.R, d.R, column.weight, s.weightY),
		G: pngInterpolate(a.G, b.G, c.G, d.G, column.weight, s.weightY),
		B: pngInterpolate(a.B, b.B, c.B, d.B, column.weight, s.weightY),
		A: pngInterpolate(a.A, b.A, c.A, d.A, column.weight, s.weightY),
	})
	return s.device.combine(result, x, y)
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

// Write 检查取消状态后写入数据，不接受未报告错误的短写
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
	return n, err
}

// Write 将压缩字节写入定长块缓冲，满块时直接输出
// 入参: data 压缩数据
// 返回: int 已接收字节数, error 写入错误
func (w *pngChunkWriter) Write(data []byte) (int, error) {
	written := 0
	for len(data) != 0 {
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
