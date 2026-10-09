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
	"errors"
	"fmt"
	"image"
	"image/color"
	"math"

	"github.com/xiaoqidun/j2kgo"
)

// jpxRowMemoryLimit 设置条带样本与小波重建缓冲的估算内存目标
const jpxRowMemoryLimit = 8 << 20

// imageRowContext 管理一次图像导出使用的区域解码器
type imageRowContext struct {
	sources []*jpxSampleImage
}

// jpxRowReader 保存完整图像信息及当前已解码的行范围
type jpxRowReader struct {
	decoder *j2kgo.Decoder
	info    j2kgo.Info
	planes  []image.Rectangle
	ready   image.Rectangle
	margin  int
	height  int
}

// rowGrayImage 按需映射灰度样本，避免展开整幅遮罩
type rowGrayImage struct {
	source    *jpxSampleImage
	lower     float64
	upper     float64
	byteExact bool
}

// close 释放解码器及保留的行样本，可重复调用
func (s *imageRowContext) close() {
	for _, source := range s.sources {
		source.rows.decoder.Close()
		source.rows.decoder = nil
		source.rows.planes = nil
		clear(source.planes)
		source.planes = nil
	}
	s.sources = nil
}

// newJPXRowReader 按分量宽度估算条带高度，保留原始网格与采样邻域
// 入参: decoder 区域解码器
// 返回: *jpxRowReader 行读取器
func newJPXRowReader(decoder *j2kgo.Decoder) *jpxRowReader {
	s := &jpxRowReader{decoder: decoder, info: decoder.Info()}
	s.planes = make([]image.Rectangle, len(s.info.Components))
	bounds := s.info.Bounds
	var memory uint64
	for n, component := range s.info.Components {
		s.margin = max(s.margin, int(component.YStep))
		x, y := int64(component.XStep), int64(component.YStep)
		s.planes[n] = image.Rect(int((int64(bounds.Min.X)+x-1)/x), int((int64(bounds.Min.Y)+y-1)/y), int((int64(bounds.Max.X)+x-1)/x), int((int64(bounds.Max.Y)+y-1)/y))
		memory += uint64(s.planes[n].Dx()) * (8 + uint64((component.Precision+7)/8))
	}
	s.height = max(1, int(min(256, jpxRowMemoryLimit/max(memory, 1)))-2*s.margin)
	return s
}

// read 解码覆盖指定行及采样邻域的横向条带，预算不足时缩小预读范围
// 入参: ctx 取消上下文, first 起始行, last 结束行，不包含该行
// 返回: *j2kgo.Raster 原始行样本, error 解码或取消错误
func (s *jpxRowReader) read(ctx context.Context, first, last int) (*j2kgo.Raster, error) {
	height := s.info.Bounds.Dy()
	if s.decoder == nil || first < 0 || last <= first || last > height {
		return nil, fmt.Errorf("invalid JPEG2000 row request")
	}
	start := max(0, first-s.margin)
	span := min(s.height, height-first)
	for {
		end := max(last, first+span)
		end += min(s.margin, height-end)
		bounds := image.Rect(s.info.Bounds.Min.X, s.info.Bounds.Min.Y+start, s.info.Bounds.Max.X, s.info.Bounds.Min.Y+end)
		raster, err := s.decoder.DecodeRegion(ctx, bounds)
		if err == nil {
			s.ready = image.Rect(0, first, s.info.Bounds.Dx(), end)
			return raster, nil
		}
		var limit *j2kgo.LimitError
		if first+span <= last || !errors.As(err, &limit) {
			return nil, err
		}
		span = max(1, span/2)
		s.height = span
	}
}

// prepareRows 更新区域样本，解码错误直接返回调用方
// 入参: ctx 取消上下文, first 起始行, last 结束行，不包含该行
// 返回: error 解码或取消错误
func (s *jpxSampleImage) prepareRows(ctx context.Context, first, last int) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if s.rows != nil && s.rows.decoder == nil {
		return fmt.Errorf("JPEG2000 row decoder is closed")
	}
	if s.rows == nil || first >= s.rows.ready.Min.Y && last <= s.rows.ready.Max.Y {
		return nil
	}
	clear(s.planes)
	s.rows.ready = image.Rectangle{}
	raster, err := s.rows.read(ctx, first, last)
	if err != nil {
		return err
	}
	for n := range s.planes {
		s.planes[n] = raster.Component(n)
	}
	return nil
}

// planeBounds 返回完整分量网格，不随当前解码窗口变化
// 入参: index 原始分量索引
// 返回: image.Rectangle 完整分量边界
func (s *jpxSampleImage) planeBounds(index int) image.Rectangle {
	if s.rows == nil {
		return s.planes[index].Bounds()
	}
	return s.rows.planes[index]
}

// prepareImageRows 在颜色取样前加载原始行及重采样邻域
// 入参: ctx 取消上下文, source 图像, first 起始行, last 结束行，不包含该行
// 返回: error 解码或取消错误
func prepareImageRows(ctx context.Context, source image.Image, first, last int) error {
	switch s := source.(type) {
	case *jpxSampleImage:
		return s.prepareRows(ctx, first, last)
	case *rowGrayImage:
		return prepareImageRows(ctx, s.source, first, last)
	case *deviceSampleImage:
		if err := prepareImageRows(ctx, s.source, first, last); err != nil {
			return err
		}
		if s.mask != nil {
			return prepareImageRows(ctx, s.mask, first, last)
		}
	case *imageResample:
		b := s.source.Bounds()
		if b == s.bounds {
			return prepareImageRows(ctx, s.source, first, last)
		}
		low := (float64(first-s.bounds.Min.Y)+.5)*float64(b.Dy())/float64(s.bounds.Dy()) - .5
		high := (float64(last-1-s.bounds.Min.Y)+.5)*float64(b.Dy())/float64(s.bounds.Dy()) - .5
		var start, end int
		if s.interpolate {
			start, end = int(math.Floor(low)), int(math.Floor(high))+1
		} else {
			start, end = int(math.Floor(low+.5)), int(math.Floor(high+.5))
		}
		start, end = max(0, min(b.Dy()-1, start)), max(0, min(b.Dy()-1, end))
		return prepareImageRows(ctx, s.source, b.Min.Y+start, b.Min.Y+end+1)
	}
	return ctx.Err()
}

// ColorModel 返回可无损表达映射灰度的颜色模型
// 返回: color.Model 灰度模型
func (s *rowGrayImage) ColorModel() color.Model {
	if s.byteExact {
		return color.GrayModel
	}
	return color.Gray16Model
}

// Bounds 返回原始图像边界
// 返回: image.Rectangle 图像边界
func (s *rowGrayImage) Bounds() image.Rectangle { return s.source.Bounds() }

// At 读取已准备行中的灰度，不执行解码操作
// 入参: x 横向坐标, y 纵向坐标
// 返回: color.Color 灰度颜色
func (s *rowGrayImage) At(x, y int) color.Color {
	return color.Gray16{Y: s.NRGBA64At(x, y).R}
}

// NRGBA64At 应用灰度映射，不修改原始样本
// 入参: x 横向坐标, y 纵向坐标
// 返回: color.NRGBA64 非预乘颜色
func (s *rowGrayImage) NRGBA64At(x, y int) color.NRGBA64 {
	if !image.Pt(x, y).In(s.Bounds()) {
		return color.NRGBA64{}
	}
	value := s.source.NRGBA64At(x, y).R
	value = uint16(math.Round(math.Max(0, math.Min(1, functionValue(float64(value)/65535, s.lower, s.upper))) * 65535))
	return color.NRGBA64{R: value, G: value, B: value, A: 65535}
}
