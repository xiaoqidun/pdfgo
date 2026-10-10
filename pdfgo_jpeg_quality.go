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
	"image/jpeg"
)

// jpegQualityComponent 关联顺序JPEG分量与量化表
type jpegQualityComponent struct{ id, table byte }

// jpegQualitySufficient 判断现有量化是否已达到目标，避免重复有损编码
// 仅识别单扫描灰度及标准分量编号的YCbCr，不推断自定义颜色分量
// 入参: ctx 取消上下文, data JPEG数据, quality 目标画质
// 返回: bool 无需继续量化, error 扫描头或取消错误
func jpegQualitySufficient(ctx context.Context, data []byte, quality int) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	scan, err := readJPEGScan(ctx, data)
	if err != nil || scan == nil {
		return false, err
	}
	tables, components, ok := jpegQualityTables(ctx, data[:scan.sos])
	if !ok || len(components) != 1 && len(components) != 3 {
		return false, ctx.Err()
	}
	var encoded bytes.Buffer
	if err := jpeg.Encode(&encoded, image.NewYCbCr(image.Rect(0, 0, 1, 1), image.YCbCrSubsampleRatio420), &jpeg.Options{Quality: quality}); err != nil {
		return false, err
	}
	target, _, ok := jpegQualityTables(ctx, encoded.Bytes())
	if !ok {
		return false, ctx.Err()
	}
	var ids byte
	for _, component := range components {
		table := 0
		if len(components) == 3 {
			if component.id < 1 || component.id > 3 || ids&(1<<component.id) != 0 {
				return false, nil
			}
			ids |= 1 << component.id
			if component.id != 1 {
				table = 1
			}
		}
		for index, value := range tables[component.table] {
			if value == 0 || target[table][index] == 0 || value < target[table][index] {
				return false, nil
			}
		}
	}
	return true, ctx.Err()
}

// jpegQualityTables 读取扫描前的量化表与基线分量，保留表内原始顺序
// 入参: ctx 取消上下文, data JPEG数据或扫描头
// 返回: [4][64]uint16 量化表, []jpegQualityComponent 分量, bool 可安全比较
func jpegQualityTables(ctx context.Context, data []byte) ([4][64]uint16, []jpegQualityComponent, bool) {
	var tables [4][64]uint16
	var components []jpegQualityComponent
	if len(data) < 2 || data[0] != 255 || data[1] != 216 {
		return tables, nil, false
	}
	for pos := 2; pos < len(data); {
		if ctx.Err() != nil {
			return tables, nil, false
		}
		if data[pos] != 255 {
			return tables, nil, false
		}
		for pos < len(data) && data[pos] == 255 {
			if pos&4095 == 0 && ctx.Err() != nil {
				return tables, nil, false
			}
			pos++
		}
		if len(data)-pos < 3 {
			return tables, nil, false
		}
		marker := data[pos]
		length := int(binary.BigEndian.Uint16(data[pos+1:]))
		if length < 2 || length > len(data)-pos-1 {
			return tables, nil, false
		}
		value := data[pos+3 : pos+1+length]
		switch marker {
		case 0xdb:
			for len(value) != 0 {
				if len(value) < 65 || value[0] > 3 {
					return tables, nil, false
				}
				for index, q := range value[1:65] {
					if q == 0 {
						return tables, nil, false
					}
					tables[value[0]][index] = uint16(q)
				}
				value = value[65:]
			}
		case 0xc0:
			if components != nil || len(value) < 6 || value[0] != 8 || len(value) != 6+3*int(value[5]) {
				return tables, nil, false
			}
			for index := 6; index < len(value); index += 3 {
				if value[index+2] > 3 {
					return tables, nil, false
				}
				components = append(components, jpegQualityComponent{value[index], value[index+2]})
			}
		case 0xda:
			return tables, components, len(components) != 0
		}
		pos += 1 + length
	}
	return tables, components, len(components) != 0
}
