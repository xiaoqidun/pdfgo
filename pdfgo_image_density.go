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
	"hash/crc32"
	"image"
	"image/png"
	"math"
	"math/big"
)

// compactPNGPhysical 优化带像素密度的PNG，保持格式并同步缩放后的密度
// 入参: ctx 取消上下文, data 原PNG, best 已有候选, density 已校验的pHYs块, options 压缩配置, size 像素需求
// 返回: []byte 更小的图片编码, error 编码或取消错误
func compactPNGPhysical(ctx context.Context, data, best, density []byte, options CompressionOptions, size image.Point) ([]byte, error) {
	candidate, err := compactPNG(ctx, data, best, options, size, false)
	if err != nil || len(candidate)+len(density) >= len(best) || len(candidate)+len(density) > optimizationBufferLimit {
		return best, err
	}
	config, err := png.DecodeConfig(&contextInput{ctx: ctx, reader: bytes.NewReader(candidate)})
	if err != nil {
		return nil, err
	}
	before := image.Pt(int(binary.BigEndian.Uint32(data[16:])), int(binary.BigEndian.Uint32(data[20:])))
	chunk, ok := resizePNGPhysical(density, before, image.Pt(config.Width, config.Height))
	if !ok {
		return best, ctx.Err()
	}
	result := make([]byte, len(candidate)+len(chunk))
	copy(result, candidate[:33])
	copy(result[33:], chunk)
	copy(result[33+len(chunk):], candidate[33:])
	return result, ctx.Err()
}

// resizePNGPhysical 按缩放后的像素数更新密度，相对单位保留精确比例
// 入参: chunk 已校验的pHYs块, before 原尺寸, after 新尺寸
// 返回: []byte 密度块，可能引用输入, bool 是否可在PNG整数范围内表示
func resizePNGPhysical(chunk []byte, before, after image.Point) ([]byte, bool) {
	if before == after {
		return chunk, true
	}
	x, y := uint64(binary.BigEndian.Uint32(chunk[8:])), uint64(binary.BigEndian.Uint32(chunk[12:]))
	if chunk[16] == 0 {
		if x == 0 || y == 0 {
			return nil, false
		}
		ratio := new(big.Rat).SetFrac64(int64(x)*int64(after.X), int64(y)*int64(after.Y))
		ratio.Mul(ratio, new(big.Rat).SetFrac64(int64(before.Y), int64(before.X)))
		if ratio.Num().BitLen() > 31 || ratio.Denom().BitLen() > 31 {
			return nil, false
		}
		x, y = ratio.Num().Uint64(), ratio.Denom().Uint64()
	} else {
		x = (x*uint64(after.X) + uint64(before.X)/2) / uint64(before.X)
		y = (y*uint64(after.Y) + uint64(before.Y)/2) / uint64(before.Y)
		if x > math.MaxInt32 || y > math.MaxInt32 || x == 0 && binary.BigEndian.Uint32(chunk[8:]) != 0 || y == 0 && binary.BigEndian.Uint32(chunk[12:]) != 0 {
			return nil, false
		}
	}
	result := bytes.Clone(chunk)
	binary.BigEndian.PutUint32(result[8:], uint32(x))
	binary.BigEndian.PutUint32(result[12:], uint32(y))
	binary.BigEndian.PutUint32(result[17:], crc32.ChecksumIEEE(result[4:17]))
	return result, true
}

// resizeJPEGPhysical 同步JPEG降采样后的JFIF密度，其他元数据及缩略图保持原样
// 入参: ctx 取消上下文, data 原始JPEG, metadata 独占的元数据缓冲, before 原尺寸, after 新尺寸
// 返回: bool 密度是否可表示, error JPEG解析或取消错误
func resizeJPEGPhysical(ctx context.Context, data, metadata []byte, before, after image.Point) (bool, error) {
	if before == after {
		return true, ctx.Err()
	}
	offset := 0
	valid, found := true, false
	_, _, _, err := jpegColorSegmentsContext(ctx, data, func(marker byte, start, end int) error {
		if marker < 0xe0 || marker > 0xef && marker != 0xfe {
			return nil
		}
		position := start + 1
		for data[position] == 255 {
			position++
		}
		payload := data[position+3 : end]
		if marker == 0xe0 && bytes.HasPrefix(payload, []byte("JFIF\x00")) {
			if found || !resizeJFIFDensity(metadata[offset+position+3-start:offset+end-start], before, after) {
				valid = false
			}
			found = true
		}
		offset += end - start
		return ctx.Err()
	})
	return valid, err
}

// resizeJFIFDensity 更新JFIF有效载荷中的密度，绝对单位四舍五入，相对单位保留精确比例
// 入参: payload 独占的JFIF有效载荷, before 原尺寸, after 新尺寸
// 返回: bool 是否可在非零16位密度范围内表示
func resizeJFIFDensity(payload []byte, before, after image.Point) bool {
	if len(payload) < 14 || len(payload) != 14+3*int(payload[12])*int(payload[13]) || payload[7] > 2 {
		return false
	}
	x, y := uint64(binary.BigEndian.Uint16(payload[8:])), uint64(binary.BigEndian.Uint16(payload[10:]))
	if x == 0 || y == 0 || before.X <= 0 || before.Y <= 0 || after.X <= 0 || after.Y <= 0 || before.X > 65535 || before.Y > 65535 || after.X > 65535 || after.Y > 65535 {
		return false
	}
	if payload[7] == 0 {
		ratio := new(big.Rat).SetFrac64(int64(x)*int64(after.X), int64(y)*int64(after.Y))
		ratio.Mul(ratio, new(big.Rat).SetFrac64(int64(before.Y), int64(before.X)))
		x, y = ratio.Num().Uint64(), ratio.Denom().Uint64()
	} else {
		x = (x*uint64(after.X) + uint64(before.X)/2) / uint64(before.X)
		y = (y*uint64(after.Y) + uint64(before.Y)/2) / uint64(before.Y)
	}
	if x == 0 || y == 0 || x > 65535 || y > 65535 {
		return false
	}
	binary.BigEndian.PutUint16(payload[8:], uint16(x))
	binary.BigEndian.PutUint16(payload[10:], uint16(y))
	return true
}
