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

	"golang.org/x/image/draw"
)

// imageScaleSource 为延迟图像补齐预乘采样接口，避免缩放器跳过非预乘目标
type imageScaleSource struct{ image.Image }

// RGBA64At 保留源图像的采样精度，按缩放器要求返回预乘颜色
// 入参: x 横向坐标, y 纵向坐标
// 返回: color.RGBA64 预乘颜色
func (s imageScaleSource) RGBA64At(x, y int) color.RGBA64 {
	return imageRGBA64At(s.Image, x, y)
}

// DecodeImageSizeContext 按显式像素需求解码并等比缩小，不改变原始图像描述
// JPEG2000设备色及校准色可提前裁减高分辨率层；外部遮罩及索引色先按完整样本解释
// 零值或非正需求保持完整尺寸，缩小结果不用于无损输出
// 入参: ctx 取消上下文, size 两轴像素需求
// 返回: image.Image 显示图像, error 参数、解码或取消错误
func (i *Image) DecodeImageSizeContext(ctx context.Context, size image.Point) (image.Image, error) {
	if ctx == nil || i == nil || i.Stream == nil {
		return nil, fmt.Errorf("missing image decode context or source")
	}
	decoded, err := i.decodeImageSize(ctx, nil, false, size)
	if err != nil {
		return nil, err
	}
	return ResizeImage(ctx, decoded, size)
}

// ResizeImage 按两轴像素需求等比缩小图片，保留灰度和透明通道，不修改输入
// 需求非正或无需缩小时返回原图；超出工作内存预算时保持原尺寸
// 入参: ctx 取消上下文, src 原图, size 像素需求
// 返回: image.Image 输出图片, error 取消错误
func ResizeImage(ctx context.Context, src image.Image, size image.Point) (image.Image, error) {
	if ctx == nil || src == nil {
		return nil, fmt.Errorf("missing image resize context or source")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	bounds := src.Bounds()
	if size.X <= 0 || size.Y <= 0 || bounds.Empty() {
		return src, nil
	}
	scale := math.Max(float64(size.X)/float64(bounds.Dx()), float64(size.Y)/float64(bounds.Dy()))
	if scale >= 1 {
		return src, nil
	}
	width := max(1, int(math.Ceil(float64(bounds.Dx())*scale)))
	height := max(1, int(math.Ceil(float64(bounds.Dy())*scale)))
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
	if _, ok := src.(image.RGBA64Image); !ok {
		src = imageScaleSource{Image: src}
	}
	if int64(width) > optimizationBufferLimit/32/int64(bounds.Dy()) {
		rows := max(1, min(32, 65536/width))
		for y := 0; y < height; y += rows {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			stripe := dst.(interface {
				SubImage(image.Rectangle) image.Image
			}).SubImage(image.Rect(0, y, width, min(height, y+rows)))
			draw.ApproxBiLinear.Scale(stripe.(draw.Image), rect, src, bounds, draw.Src, nil)
		}
	} else {
		scaler := draw.CatmullRom.NewScaler(width, height, bounds.Dx(), bounds.Dy())
		scaler.Scale(dst, rect, src, bounds, draw.Src, nil)
	}
	return dst, ctx.Err()
}
