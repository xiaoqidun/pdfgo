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
	"image"
	"math"

	"golang.org/x/image/draw"
)

// ResizeImage 按两轴像素需求等比缩小图片，保留灰度和透明通道，不修改输入
// 需求非正或无需缩小时返回原图；超出工作内存预算时保持原尺寸
// 入参: ctx 取消上下文, src 原图, size 像素需求
// 返回: image.Image 输出图片, error 取消错误
func ResizeImage(ctx context.Context, src image.Image, size image.Point) (image.Image, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	bounds := src.Bounds()
	if size.X <= 0 || size.Y <= 0 || bounds.Empty() {
		return src, nil
	}
	scale := math.Max(float64(size.X)/float64(bounds.Dx()), float64(size.Y)/float64(bounds.Dy()))
	if scale >= 1 || int64(bounds.Dx()) > optimizationBufferLimit/4/int64(bounds.Dy()) {
		return src, nil
	}
	width := max(1, int(math.Ceil(float64(bounds.Dx())*scale)))
	height := max(1, int(math.Ceil(float64(bounds.Dy())*scale)))
	rect := image.Rect(0, 0, width, height)
	var dst draw.Image
	if _, ok := src.(*image.Gray); ok {
		dst = image.NewGray(rect)
	} else {
		dst = image.NewNRGBA(rect)
	}
	if int64(width)*int64(bounds.Dy()) > optimizationBufferLimit/32 {
		draw.ApproxBiLinear.Scale(dst, rect, src, bounds, draw.Src, nil)
	} else {
		scaler := draw.CatmullRom.NewScaler(width, height, bounds.Dx(), bounds.Dy())
		scaler.Scale(dst, rect, src, bounds, draw.Src, nil)
	}
	return dst, ctx.Err()
}
