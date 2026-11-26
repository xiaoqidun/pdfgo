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
	"fmt"
	"math"
	"runtime"
)

// imageBufferLimit 取得缓冲长度上限，WASM的线性内存范围独立于整数位宽
// 返回: int 平台可表达的缓冲字节数
func imageBufferLimit() int {
	maximum := uint64(^uint(0) >> 1)
	if runtime.GOARCH == "wasm" {
		maximum = min(maximum, uint64(1<<32-1))
	}
	return int(maximum)
}

// imageCompositeBudget 根据流解压上限派生合成图像输出缓冲预算，零值表示不额外封顶
// 入参: maxBytes 流解压字节上限
// 返回: int64 合成输出字节预算
func imageCompositeBudget(maxBytes int64) int64 {
	if maxBytes <= 0 {
		return 0
	}
	const softFloor int64 = 64 << 20
	quad := int64(math.MaxInt64)
	if maxBytes <= math.MaxInt64/4 {
		quad = maxBytes * 4
	}
	oct := int64(math.MaxInt64)
	if maxBytes <= math.MaxInt64/8 {
		oct = maxBytes * 8
	}
	return min(oct, max(quad, softFloor))
}

// imageCompositeBufferSize 校验合成图像的流预算与平台缓冲上限，拦截异尺寸遮罩膨胀
// 入参: width 输出宽度, height 输出高度, pixelBytes 单像素字节数, maxBytes 流解压字节上限
// 返回: int 行字节数, int 缓冲字节数, error 超限或平台尺寸错误
func imageCompositeBufferSize(width, height, pixelBytes int, maxBytes int64) (int, int, error) {
	if width <= 0 || height <= 0 || pixelBytes <= 0 {
		return 0, 0, fmt.Errorf("image buffer size exceeds platform buffer range")
	}
	if budget := imageCompositeBudget(maxBytes); budget > 0 {
		if int64(width) > budget/int64(pixelBytes)/int64(height) {
			return 0, 0, fmt.Errorf("%w: composite image buffer exceeds limit", ErrStreamLimitExceeded)
		}
	}
	return imageBufferSize(width, height, pixelBytes)
}

// imageBufferSize 按实际像素字节数计算缓冲，乘法前校验平台缓冲范围
// 入参: width 图像宽度, height 图像高度, pixelBytes 像素字节数
// 返回: int 行字节数, int 缓冲字节数, error 尺寸错误
func imageBufferSize(width, height, pixelBytes int) (int, int, error) {
	if width <= 0 || height <= 0 || pixelBytes <= 0 || width > imageBufferLimit()/pixelBytes/height {
		return 0, 0, fmt.Errorf("image buffer size exceeds platform buffer range")
	}
	stride := width * pixelBytes
	return stride, stride * height, nil
}

// imageSampleSize 按分量位深计算逐行对齐的原始样本尺寸
// 入参: width 图像宽度, height 图像高度, components 分量数, depth 分量位深
// 返回: int 行字节数, int 样本字节数, error 尺寸或位深错误
func imageSampleSize(width, height, components, depth int) (int, int, error) {
	if depth == 8 || depth == 16 {
		if components <= 0 || components > int(^uint(0)>>1)/(depth/8) {
			return 0, 0, fmt.Errorf("image sample size exceeds platform buffer range")
		}
		return imageBufferSize(width, height, components*(depth/8))
	}
	if depth != 1 && depth != 2 && depth != 4 {
		return 0, 0, fmt.Errorf("invalid image component depth")
	}
	if width <= 0 || height <= 0 || components <= 0 || uint64(width) > uint64(1<<63-1)/uint64(components) {
		return 0, 0, fmt.Errorf("image sample size exceeds platform buffer range")
	}
	samples := int64(width) * int64(components)
	perByte := int64(8 / depth)
	stride := samples/perByte + min(1, samples%perByte)
	if stride > int64(imageBufferLimit()/height) {
		return 0, 0, fmt.Errorf("image sample size exceeds platform buffer range")
	}
	return int(stride), int(stride) * height, nil
}
