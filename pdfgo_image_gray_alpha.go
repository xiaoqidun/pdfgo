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
	"context"
	"encoding/binary"
	"image"
	"image/color"
)

// pngGrayAlphaImage 标记已经验证的等值灰度通道，保留原始非预乘透明样本
type pngGrayAlphaImage struct {
	image.Image
}

// NRGBA64At 读取原始非预乘样本，不经透明度往返换算
// 入参: x 横坐标, y 纵坐标
// 返回: color.NRGBA64 原始颜色及透明度
func (p *pngGrayAlphaImage) NRGBA64At(x, y int) color.NRGBA64 {
	return imageNRGBA64At(p.Image, x, y)
}

// compactPNGGrayAlpha 按PNG颜色类型4编码灰度及透明度，仅保留更小候选
// 入参: ctx 取消上下文, img 已验证灰度图像, depth 原始位深, best 已有候选
// 返回: []byte 更小的编码, error 编码或取消错误
func compactPNGGrayAlpha(ctx context.Context, img image.Image, depth int, best []byte) ([]byte, error) {
	var out bytes.Buffer
	limit := &optimizationBuffer{buffer: &out, limit: min(len(best), optimizationBufferLimit)}
	encoder, err := newPNGImageEncoderWithCompression(ctx, limit, img.Bounds(), depth, 2, 4, true)
	if err != nil {
		if limit.exceeded {
			return best, ctx.Err()
		}
		return nil, err
	}
	defer encoder.release()
	bounds := img.Bounds()
	for y := bounds.Min.Y; y < bounds.Max.Y; y++ {
		for x := bounds.Min.X; x < bounds.Max.X; x++ {
			if (x-bounds.Min.X)&4095 == 0 {
				if err := ctx.Err(); err != nil {
					return nil, err
				}
			}
			c := imageNRGBA64At(img, x, y)
			offset := (x - bounds.Min.X) * depth / 4
			if depth == 8 {
				encoder.row[offset], encoder.row[offset+1] = byte(c.R>>8), byte(c.A>>8)
			} else {
				binary.BigEndian.PutUint16(encoder.row[offset:], c.R)
				binary.BigEndian.PutUint16(encoder.row[offset+2:], c.A)
			}
		}
		if err := encoder.writeRow(); err != nil {
			if limit.exceeded {
				return best, ctx.Err()
			}
			return nil, err
		}
	}
	if err := encoder.finish(); err != nil {
		if limit.exceeded {
			return best, ctx.Err()
		}
		return nil, err
	}
	if out.Len() < len(best) {
		best = out.Bytes()
	}
	return best, ctx.Err()
}
