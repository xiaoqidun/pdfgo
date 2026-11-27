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
	"fmt"
	"image"
	"image/color"
	"math"
	"os"

	"golang.org/x/image/draw"
)

// imageScaleSource 为延迟图像补齐预乘采样接口，避免缩放器跳过非预乘目标
type imageScaleSource struct{ image.Image }

// catmullRomSpan 保存目标轴上单个输出坐标对应的源采样权重区间
type catmullRomSpan struct {
	start              int
	end                int
	invTotalWeight     float64
	invTotalWeightFFFF float64
}

// catmullRomContrib 保存单个源坐标对目标像素的卷积权重
type catmullRomContrib struct {
	coord  int
	weight float64
}

// catmullRomDistrib 保存一维Catmull-Rom缩放权重分布
type catmullRomDistrib struct {
	spans    []catmullRomSpan
	contribs []catmullRomContrib
}

// ResizeImage 按两轴像素需求等比缩小图片，保留灰度和透明通道，不修改输入
// 需求非正或无需缩小时返回原图；超出工作内存预算时保持原尺寸
// 入参: ctx 取消上下文, src 原图, size 像素需求
// 返回: image.Image 输出图片, error 参数或取消错误
func ResizeImage(ctx context.Context, src image.Image, size image.Point) (image.Image, error) {
	if ctx == nil || src == nil {
		return nil, fmt.Errorf("missing image resize context or source")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := validateImageChain(ctx, src, true); err != nil {
		return nil, err
	}
	bounds := src.Bounds()
	if size.X <= 0 || size.Y <= 0 || bounds.Empty() {
		return src, nil
	}
	scaled := scaledImageSize(bounds.Dx(), bounds.Dy(), size)
	width, height := scaled.X, scaled.Y
	if width >= bounds.Dx() && height >= bounds.Dy() {
		return src, nil
	}
	if int64(width) > optimizationBufferLimit/4/int64(height) {
		return src, nil
	}
	rect := image.Rect(0, 0, width, height)
	var dst draw.Image
	if _, ok := src.(*image.Gray); ok {
		dst = image.NewGray(rect)
	} else {
		dst = image.NewNRGBA(rect)
	}
	switch src.(type) {
	case *image.CMYK, *image.Paletted:
		src = imageScaleSource{Image: src}
	default:
		if _, ok := src.(image.RGBA64Image); !ok {
			src = imageScaleSource{Image: src}
		}
	}
	if int64(width) > optimizationBufferLimit/32/int64(bounds.Dy()) || int64(bounds.Dx()) > optimizationBufferLimit/32 || int64(bounds.Dy()) > optimizationBufferLimit/32 {
		rows := max(1, min(32, 65536/width))
		cols := 65536
		sub := dst.(interface {
			SubImage(image.Rectangle) image.Image
		})
		for y := 0; y < height; y += rows {
			endY := min(height, y+rows)
			for x := 0; x < width; x += cols {
				if err := ctx.Err(); err != nil {
					return nil, err
				}
				stripe := sub.SubImage(image.Rect(x, y, min(width, x+cols), endY))
				draw.ApproxBiLinear.Scale(stripe.(draw.Image), rect, src, bounds, draw.Src, nil)
			}
		}
	} else if err := scaleCatmullRom(ctx, dst, src.(image.RGBA64Image), bounds, width, height); err != nil {
		return nil, err
	}
	return dst, ctx.Err()
}

// newCatmullRomDistrib 构建一维Catmull-Rom核权重表并响应上下文取消
// 入参: ctx 取消上下文, dstSize 目标长度, srcSize 源长度
// 返回: catmullRomDistrib 权重分布, error 取消错误
func newCatmullRomDistrib(ctx context.Context, dstSize, srcSize int) (catmullRomDistrib, error) {
	scale := float64(srcSize) / float64(dstSize)
	halfWidth, argScale := 2.0, 1.0
	if scale > 1 {
		halfWidth *= scale
		argScale = 1 / scale
	}
	spans := make([]catmullRomSpan, dstSize)
	total := 0
	for x := range spans {
		if x&4095 == 0 {
			if err := ctx.Err(); err != nil {
				return catmullRomDistrib{}, err
			}
		}
		center := (float64(x)+0.5)*scale - 0.5
		lo := max(0, int(math.Floor(center-halfWidth)))
		hi := min(srcSize, max(lo, int(math.Ceil(center+halfWidth))))
		spans[x] = catmullRomSpan{start: lo, end: hi, invTotalWeight: center}
		total += hi - lo
	}
	contribs := make([]catmullRomContrib, 0, total)
	for k, span := range spans {
		if k&4095 == 0 {
			if err := ctx.Err(); err != nil {
				return catmullRomDistrib{}, err
			}
		}
		sum := 0.0
		base := len(contribs)
		for coord := span.start; coord < span.end; coord++ {
			if (coord-span.start)&255 == 255 {
				if err := ctx.Err(); err != nil {
					return catmullRomDistrib{}, err
				}
			}
			t := math.Abs((span.invTotalWeight - float64(coord)) * argScale)
			if t >= 2 {
				continue
			}
			var weight float64
			if t < 1 {
				weight = (1.5*t-2.5)*t*t + 1
			} else {
				weight = ((-0.5*t+2.5)*t-4)*t + 2
			}
			if weight == 0 {
				continue
			}
			sum += weight
			contribs = append(contribs, catmullRomContrib{coord: coord, weight: weight})
		}
		inv := 1 / sum
		spans[k] = catmullRomSpan{start: base, end: len(contribs), invTotalWeight: inv, invTotalWeightFFFF: inv / 0xffff}
	}
	return catmullRomDistrib{spans: spans, contribs: contribs}, ctx.Err()
}

// catmullRomUint16 将归一化浮点样本舍入并钳位为十六位无符号分量
// 入参: value 归一化分量值
// 返回: uint16 十六位无符号分量
func catmullRomUint16(value float64) uint16 {
	v := int32(0xffff*value + 0.5)
	if v > 0xffff {
		return 0xffff
	}
	if v > 0 {
		return uint16(v)
	}
	return 0
}

// scaleCatmullRom 执行两遍可分离Catmull-Rom缩放，逐行及长行分段检查上下文取消
// 入参: ctx 取消上下文, dst 目标图像, src 支持十六位预乘采样的源图像, bounds 源图像边界, width 目标宽度, height 目标高度
// 返回: error 取消错误
func scaleCatmullRom(ctx context.Context, dst draw.Image, src image.RGBA64Image, bounds image.Rectangle, width, height int) error {
	srcH := bounds.Dy()
	horiz, err := newCatmullRomDistrib(ctx, width, bounds.Dx())
	if err != nil {
		return err
	}
	vert, err := newCatmullRomDistrib(ctx, height, srcH)
	if err != nil {
		return err
	}
	if grayDst, ok := dst.(*image.Gray); ok {
		graySrc := src.(*image.Gray)
		tmp := make([]float64, width*srcH)
		for sy := 0; sy < srcH; sy++ {
			if err := ctx.Err(); err != nil {
				return err
			}
			srcRow := graySrc.Pix[sy*graySrc.Stride:]
			base := sy * width
			for dx, span := range horiz.spans {
				if dx&255 == 255 {
					if err := ctx.Err(); err != nil {
						return err
					}
				}
				var sum float64
				for i, c := range horiz.contribs[span.start:span.end] {
					if i&255 == 255 {
						if err := ctx.Err(); err != nil {
							return err
						}
					}
					sum += float64(uint16(srcRow[c.coord])*257) * c.weight
				}
				tmp[base+dx] = sum * span.invTotalWeightFFFF
			}
		}
		for dy, span := range vert.spans {
			if err := ctx.Err(); err != nil {
				return err
			}
			dstRow := grayDst.Pix[dy*grayDst.Stride:]
			contribs := vert.contribs[span.start:span.end]
			for dx := 0; dx < width; dx++ {
				if dx&255 == 255 {
					if err := ctx.Err(); err != nil {
						return err
					}
				}
				var sum float64
				for i, c := range contribs {
					if i&255 == 255 {
						if err := ctx.Err(); err != nil {
							return err
						}
					}
					sum += tmp[c.coord*width+dx] * c.weight
				}
				dstRow[dx] = uint8(catmullRomUint16(sum*span.invTotalWeight) >> 8)
			}
		}
		return ctx.Err()
	}
	nrgbaDst := dst.(*image.NRGBA)
	tmp := make([][4]float64, width*srcH)
	for sy := 0; sy < srcH; sy++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		y := bounds.Min.Y + sy
		base := sy * width
		for dx, span := range horiz.spans {
			if dx&255 == 255 {
				if err := ctx.Err(); err != nil {
					return err
				}
			}
			var pr, pg, pb, pa float64
			for i, c := range horiz.contribs[span.start:span.end] {
				if i&255 == 255 {
					if err := ctx.Err(); err != nil {
						return err
					}
				}
				pu := src.RGBA64At(bounds.Min.X+c.coord, y)
				pr += float64(pu.R) * c.weight
				pg += float64(pu.G) * c.weight
				pb += float64(pu.B) * c.weight
				pa += float64(pu.A) * c.weight
			}
			tmp[base+dx] = [4]float64{
				pr * span.invTotalWeightFFFF,
				pg * span.invTotalWeightFFFF,
				pb * span.invTotalWeightFFFF,
				pa * span.invTotalWeightFFFF,
			}
		}
	}
	for dy, span := range vert.spans {
		if err := ctx.Err(); err != nil {
			return err
		}
		contribs := vert.contribs[span.start:span.end]
		for dx := 0; dx < width; dx++ {
			if dx&255 == 255 {
				if err := ctx.Err(); err != nil {
					return err
				}
			}
			var pr, pg, pb, pa float64
			for i, c := range contribs {
				if i&255 == 255 {
					if err := ctx.Err(); err != nil {
						return err
					}
				}
				p := &tmp[c.coord*width+dx]
				pr += p[0] * c.weight
				pg += p[1] * c.weight
				pb += p[2] * c.weight
				pa += p[3] * c.weight
			}
			if pr > pa {
				pr = pa
			}
			if pg > pa {
				pg = pa
			}
			if pb > pa {
				pb = pa
			}
			nrgbaDst.SetRGBA64(dx, dy, color.RGBA64{
				R: catmullRomUint16(pr * span.invTotalWeight),
				G: catmullRomUint16(pg * span.invTotalWeight),
				B: catmullRomUint16(pb * span.invTotalWeight),
				A: catmullRomUint16(pa * span.invTotalWeight),
			})
		}
	}
	return ctx.Err()
}

// At 返回与十六位预乘采样一致的颜色，避免通用缩放回退绕过色彩转换
// 入参: x 横向坐标, y 纵向坐标
// 返回: color.Color 预乘颜色
func (s imageScaleSource) At(x, y int) color.Color {
	return s.RGBA64At(x, y)
}

// RGBA64At 保留源图像的采样精度，按缩放器要求返回预乘颜色
// 入参: x 横向坐标, y 纵向坐标
// 返回: color.RGBA64 预乘颜色
func (s imageScaleSource) RGBA64At(x, y int) color.RGBA64 {
	return imageRGBA64At(s.Image, x, y)
}

// DecodeImageSize 按显式像素需求解码并等比缩小，不改变原始图像描述
// 入参: size 两轴像素需求
// 返回: image.Image 显示图像, error 参数或解码错误
func (i *Image) DecodeImageSize(size image.Point) (image.Image, error) {
	return i.DecodeImageSizeContext(context.Background(), size)
}

// DecodeImageSizeContext 按显式像素需求解码并等比缩小，不改变原始图像描述
// 支持对JPEG、JPEG2000、通用流、外部遮罩及调色板、专色图像分级降采样
// 零值或非正需求保持完整尺寸，缩小结果不用于无损输出
// 入参: ctx 取消上下文, size 两轴像素需求
// 返回: image.Image 显示图像, error 参数、解码或取消错误
func (i *Image) DecodeImageSizeContext(ctx context.Context, size image.Point) (image.Image, error) {
	if ctx == nil || i == nil || i.Stream == nil {
		return nil, fmt.Errorf("missing image decode context or source")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if i.reader != nil && i.reader.isClosed() {
		return nil, os.ErrClosed
	}
	decoded, err := i.decodeImageSize(ctx, nil, false, size)
	if err != nil {
		return nil, err
	}
	return ResizeImage(ctx, decoded, size)
}
