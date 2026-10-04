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
	"context"
	"encoding/binary"
	"fmt"
	"image"
	"math"
	"math/bits"
	"os"
)

// HalftoneOptions 保存设备网屏参数，分量值采用黑零白一的加色形式
// Origin指定设备像素原点，不自动换算用户坐标或分辨率
type HalftoneOptions struct {
	Resolution    float64
	Bits          int
	Colorant      Name
	Transfer      Object
	Named         map[string]*Halftone
	MaxCellPixels int
	Origin        image.Point
}

// HalftoneScreen 保存独立于阅读器的设备网屏，编译后可并发读取
type HalftoneScreen struct {
	width, height int
	width2        int
	height2       int
	xsign, ysign  int
	originX       uint64
	originY       uint64
	originKey     [2]uint64
	levels        uint64
	thresholds    []uint16
	tiled         map[[2]uint64]uint16
	transfer      []uint16
}

// CompileHalftone 编译类型1、5、6、10和16网屏，名称优先匹配设备定义
// 入参: ctx 取消上下文, h 网屏定义, options 设备参数，默认单比特及百万像素网屏上限
// 返回: *HalftoneScreen 设备网屏，默认网屏返回空值, error 参数或函数错误
func (r *Reader) CompileHalftone(ctx context.Context, h *Halftone, options HalftoneOptions) (*HalftoneScreen, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if r.closed {
		return nil, os.ErrClosed
	}
	if h == nil {
		return nil, nil
	}
	if named := options.Named[string(h.Name)]; h.Name != nil && named != nil {
		h = named
	}
	if h.Type == 0 {
		return nil, nil
	}
	compound := h.Type == 5
	_, primary := transferChannel(options.Colorant)
	requireTransfer := compound && !primary && options.Colorant != ""
	if requireTransfer {
		if err := r.defaultColorantTransfer(h, options.Named); err != nil {
			return nil, err
		}
	}
	var err error
	h, err = h.component(options.Colorant)
	if err != nil {
		return nil, err
	}
	if compound {
		if named := options.Named[string(h.Name)]; h.Name != nil && named != nil {
			h = named
		}
	}
	if requireTransfer && h.Type != 0 && h.TransferFunction == nil {
		return nil, fmt.Errorf("missing nonprimary halftone transfer function")
	}
	transferReader := r
	if h.reader != nil {
		r = h.reader
	}
	if r.closed {
		return nil, os.ErrClosed
	}
	return r.compileHalftone(ctx, h, options, transferReader, requireTransfer)
}

// compileHalftone 编译单分量阈值及传递函数，不保留阅读器或函数数据流
// 入参: ctx 取消上下文, h 单分量网屏, options 设备参数, transferReader 图形状态函数所属阅读器, requireTransfer 是否必需传递函数
// 返回: *HalftoneScreen 网屏, error 定义或计算错误
func (r *Reader) compileHalftone(ctx context.Context, h *Halftone, options HalftoneOptions, transferReader *Reader, requireTransfer bool) (*HalftoneScreen, error) {
	if h == nil || h.Type == 0 {
		return nil, nil
	}
	depth := options.Bits
	if depth == 0 {
		depth = 1
	}
	if depth < 1 || depth > 16 {
		return nil, fmt.Errorf("invalid halftone output depth")
	}
	limit := options.MaxCellPixels
	if limit == 0 {
		limit = 1 << 20
	}
	if limit < 1 {
		return nil, fmt.Errorf("invalid halftone cell limit")
	}
	s := &HalftoneScreen{width: h.Width, height: h.Height, width2: h.Width2, height2: h.Height2, xsign: 1, ysign: 1, levels: (1 << depth) - 1}
	if h.Type == 1 {
		if err := r.spotThresholds(ctx, s, h, options.Resolution, limit); err != nil {
			return nil, err
		}
	} else {
		if h.Type == 10 {
			s.width, s.height, s.width2, s.height2 = h.Xsquare, h.Xsquare, h.Ysquare, h.Ysquare
		} else if h.Type != 6 && h.Type != 16 {
			return nil, &UnsupportedError{Feature: fmt.Sprintf("halftone screen type %d", h.Type)}
		}
		if s.width < 1 || s.height < 1 {
			return nil, fmt.Errorf("invalid halftone first rectangle")
		}
		count, err := s.cellPixels(limit)
		if err != nil {
			return nil, err
		}
		if count != len(h.Thresholds) {
			return nil, fmt.Errorf("invalid halftone threshold count")
		}
		s.thresholds = append([]uint16(nil), h.Thresholds...)
	}
	for i, threshold := range s.thresholds {
		if threshold == 0 {
			s.thresholds[i] = 1
			if h.Type == 6 || h.Type == 10 {
				s.thresholds[i] = 257
			}
		}
	}
	if s.width2 != 0 {
		s.tiled = make(map[[2]uint64]uint16, len(s.thresholds))
		index := 0
		for _, rect := range [][3]int{{s.width, s.height, 0}, {s.width2, s.height2, s.height}} {
			for y := 0; y < rect[1]; y++ {
				if err := ctx.Err(); err != nil {
					return nil, err
				}
				for x := 0; x < rect[0]; x++ {
					s.tiled[s.tileKey(x, y+rect[2])] = s.thresholds[index]
					index++
				}
			}
		}
		if len(s.tiled) != len(s.thresholds) {
			return nil, fmt.Errorf("invalid halftone tiling")
		}
		s.thresholds = nil
	}
	if s.tiled != nil {
		n := uint64(s.width*s.height + s.width2*s.height2)
		x, y := halftoneModulo(options.Origin.X, n), halftoneModulo(options.Origin.Y, n)
		if s.xsign < 0 {
			x = halftonePhase(0, x, n)
		}
		if s.ysign < 0 {
			y = halftonePhase(0, y, n)
		}
		s.originKey = s.tileResidues(x, y)
	} else {
		s.originX, s.originY = halftoneModulo(options.Origin.X, uint64(s.width)), halftoneModulo(options.Origin.Y, uint64(s.height))
		if s.xsign < 0 {
			s.originX = halftonePhase(0, s.originX, uint64(s.width))
		}
		if s.ysign < 0 {
			s.originY = halftonePhase(0, s.originY, uint64(s.height))
		}
	}
	var value Object
	var err error
	overridden := false
	if h.TransferFunction != nil {
		value, err = r.Resolve(h.TransferFunction)
		if err != nil {
			return nil, err
		}
		overridden = value != nil
	}
	if !overridden && requireTransfer {
		return nil, fmt.Errorf("missing nonprimary halftone transfer function")
	}
	if overridden {
		transferReader = r
	} else {
		value, err = transferReader.Resolve(options.Transfer)
		if err != nil {
			return nil, err
		}
	}
	if functions, ok := value.(Array); ok {
		if overridden {
			return nil, fmt.Errorf("invalid halftone transfer function")
		}
		if len(functions) != 4 {
			return nil, fmt.Errorf("invalid transfer function count")
		}
		channel, _ := transferChannel(options.Colorant)
		value, err = transferReader.Resolve(functions[channel])
		if err != nil {
			return nil, err
		}
		if value == nil || value == Name("Default") {
			return nil, fmt.Errorf("invalid transfer function component")
		}
		if _, ok := value.(Array); ok {
			return nil, fmt.Errorf("invalid transfer function component")
		}
	}
	if value == Name("Default") && !overridden {
		value = nil
	}
	function, err := transferReader.ReadTransferFunction(value)
	if err != nil {
		return nil, err
	}
	if function != nil {
		s.transfer = make([]uint16, 1<<16)
		for i := range s.transfer {
			if i%256 == 0 {
				if err := ctx.Err(); err != nil {
					return nil, err
				}
			}
			value, err := function.Evaluate(float64(i)/65535, 0)
			if err != nil {
				return nil, fmt.Errorf("invalid halftone transfer result: %v", err)
			}
			s.transfer[i] = uint16(math.Round(value * 65535))
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return s, nil
}

// cellPixels 验证双矩形尺寸并在乘法前检查设备容量
// 入参: limit 最大像素数
// 返回: int 像素数, error 尺寸或容量错误
func (s *HalftoneScreen) cellPixels(limit int) (int, error) {
	count := 0
	for _, rect := range [][2]int{{s.width, s.height}, {s.width2, s.height2}} {
		if rect == [2]int{} {
			continue
		}
		if rect[0] < 1 || rect[1] < 1 || rect[0] > (limit-count)/rect[1] {
			return 0, fmt.Errorf("invalid or oversized halftone cell")
		}
		count += rect[0] * rect[1]
	}
	if count == 0 {
		return 0, fmt.Errorf("empty halftone cell")
	}
	return count, nil
}

// tileKey 将设备坐标映射到双矩形网格的整数格点商，避免斜角和负坐标接缝
// 入参: x 横向设备坐标, y 纵向设备坐标
// 返回: [2]uint64 格点标识
func (s *HalftoneScreen) tileKey(x, y int) [2]uint64 {
	n := uint64(s.width*s.height + s.width2*s.height2)
	xm, ym := halftoneModulo(x, n), halftoneModulo(y, n)
	return s.tileResidues(xm, ym)
}

// tileResidues 将坐标余数映射到双矩形格点商
// 入参: xm 横向余数, ym 纵向余数
// 返回: [2]uint64 格点标识
func (s *HalftoneScreen) tileResidues(xm, ym uint64) [2]uint64 {
	n := uint64(s.width*s.height + s.width2*s.height2)
	a := halftoneProduct(uint64(s.height)%n, xm, n)
	b := halftoneProduct(uint64(s.width2)%n, ym, n)
	u := (a + n - b) % n
	v := (halftoneProduct(uint64(s.height2)%n, xm, n) + halftoneProduct(uint64(s.width)%n, ym, n)) % n
	return [2]uint64{u, v}
}

// halftonePhase 从坐标余数扣除原点相位，避免有符号坐标相减溢出
// 入参: value 坐标余数, origin 原点余数, period 网格周期
// 返回: uint64 平移后的非负余数
func halftonePhase(value, origin, period uint64) uint64 {
	if value < origin {
		return value + period - origin
	}
	return value - origin
}

// halftoneModulo 计算有符号设备坐标的非负余数
// 入参: value 设备坐标, modulus 网格周期
// 返回: uint64 非负余数
func halftoneModulo(value int, modulus uint64) uint64 {
	v := value % int(modulus)
	if v < 0 {
		v += int(modulus)
	}
	return uint64(v)
}

// halftoneProduct 计算格点乘积的余数，不截断大坐标乘积
// 入参: a 第一因子, b 第二因子, modulus 周期
// 返回: uint64 乘积余数
func halftoneProduct(a, b, modulus uint64) uint64 {
	hi, lo := bits.Mul64(a, b)
	_, remainder := bits.Div64(hi, lo, modulus)
	return remainder
}

// Quantize 将16位连续分量量化为设备等级，坐标始终处于设备空间
// 入参: value 黑零白满的分量值, x 横向设备坐标, y 纵向设备坐标
// 返回: uint16 归一化后的设备分量值
func (s *HalftoneScreen) Quantize(value uint16, x, y int) uint16 {
	if s.xsign < 0 {
		x = ^x
	}
	if s.ysign < 0 {
		y = ^y
	}
	var threshold uint16
	if s.tiled != nil {
		key := s.tileKey(x, y)
		if s.originKey != [2]uint64{} {
			n := uint64(s.width*s.height + s.width2*s.height2)
			key[0], key[1] = halftonePhase(key[0], s.originKey[0], n), halftonePhase(key[1], s.originKey[1], n)
		}
		threshold = s.tiled[key]
	} else {
		xm, ym := halftoneModulo(x, uint64(s.width)), halftoneModulo(y, uint64(s.height))
		xm, ym = halftonePhase(xm, s.originX, uint64(s.width)), halftonePhase(ym, s.originY, uint64(s.height))
		threshold = s.thresholds[int(ym)*s.width+int(xm)]
	}
	return s.quantize(value, threshold)
}

// quantize 应用传递函数并按指定阈值选择设备等级
// 入参: value 加色分量, threshold 设备像素阈值
// 返回: uint16 归一化设备分量
func (s *HalftoneScreen) quantize(value, threshold uint16) uint16 {
	if s.transfer != nil {
		value = s.transfer[value]
	}
	scaled := uint64(value) * s.levels
	level, fraction := scaled/65535, scaled%65535
	if fraction >= uint64(threshold) {
		level++
	}
	return uint16((level*65535 + s.levels/2) / s.levels)
}

// QuantizeRow 量化一行加色分量，支持重叠缓冲区且不分配像素内存
// 入参: ctx 取消上下文, out 输出分量, values 源分量, x 起始设备横坐标, y 设备纵坐标
// 返回: error 取消、尺寸或坐标错误，取消时输出可能已部分更新
func (s *HalftoneScreen) QuantizeRow(ctx context.Context, out, values []uint16, x, y int) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if s == nil || s.levels == 0 {
		return fmt.Errorf("missing halftone screen")
	}
	if len(out) != len(values) {
		return fmt.Errorf("invalid halftone row length")
	}
	if len(out) > 0 && x > math.MaxInt-(len(out)-1) {
		return fmt.Errorf("halftone row coordinate overflow")
	}
	copy(out, values)
	if s.tiled != nil {
		for i, value := range out {
			if i%256 == 0 {
				if err := ctx.Err(); err != nil {
					return err
				}
			}
			out[i] = s.Quantize(value, x+i, y)
		}
		return ctx.Err()
	}
	if s.xsign < 0 {
		x = ^x
	}
	if s.ysign < 0 {
		y = ^y
	}
	xm := int(halftonePhase(halftoneModulo(x, uint64(s.width)), s.originX, uint64(s.width)))
	ym := int(halftonePhase(halftoneModulo(y, uint64(s.height)), s.originY, uint64(s.height)))
	row := s.thresholds[ym*s.width : (ym+1)*s.width]
	for i, value := range out {
		if i%256 == 0 {
			if err := ctx.Err(); err != nil {
				return err
			}
		}
		out[i] = s.quantize(value, row[xm])
		xm += s.xsign
		if xm < 0 {
			xm = s.width - 1
		} else if xm == s.width {
			xm = 0
		}
	}
	return ctx.Err()
}

// Apply 对单个加色分量进行网屏化，保持原始设备坐标及子图边界
// 入参: ctx 取消上下文, source 16位连续分量图像
// 返回: *image.Gray16 网屏化图像, error 取消或空图像错误
func (s *HalftoneScreen) Apply(ctx context.Context, source *image.Gray16) (*image.Gray16, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if source == nil {
		return nil, fmt.Errorf("missing halftone source")
	}
	if s == nil || s.levels == 0 {
		return nil, fmt.Errorf("missing halftone screen")
	}
	width, height := source.Rect.Dx(), source.Rect.Dy()
	if source.Rect.Max.X < source.Rect.Min.X || source.Rect.Max.Y < source.Rect.Min.Y || width < 0 || height < 0 || width > math.MaxInt/2 || source.Stride < 0 || source.Stride < width*2 {
		return nil, fmt.Errorf("invalid halftone source layout")
	}
	if width > 0 && height > 0 && (len(source.Pix) < width*2 || height-1 > (len(source.Pix)-width*2)/source.Stride) {
		return nil, fmt.Errorf("truncated halftone source pixels")
	}
	result := image.NewGray16(source.Bounds())
	for y := source.Rect.Min.Y; y < source.Rect.Max.Y; y++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		sourceOffset, outputOffset := source.PixOffset(source.Rect.Min.X, y), result.PixOffset(result.Rect.Min.X, y)
		for x := source.Rect.Min.X; x < source.Rect.Max.X; x++ {
			value := binary.BigEndian.Uint16(source.Pix[sourceOffset:])
			binary.BigEndian.PutUint16(result.Pix[outputOffset:], s.Quantize(value, x, y))
			sourceOffset += 2
			outputOffset += 2
		}
	}
	return result, nil
}
