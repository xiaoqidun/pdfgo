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
	"math/big"
)

// resizeJPEGExif 更新Exif主图尺寸和像素相关字段，保留其他应用标记
// 入参: ctx 取消上下文, metadata 已校验的JPEG元数据段, before 原尺寸, after 新尺寸
// 返回: []byte 更新后的元数据, bool 是否可安全更新, error 取消错误
func resizeJPEGExif(ctx context.Context, metadata []byte, before, after image.Point) ([]byte, bool, error) {
	if before == after {
		return metadata, true, ctx.Err()
	}
	var output []byte
	copied, found := 0, false
	for start := 0; start < len(metadata); {
		if err := ctx.Err(); err != nil {
			return nil, false, err
		}
		marker := start
		for marker < len(metadata) && metadata[marker] == 255 {
			marker++
		}
		if marker == start || marker+3 > len(metadata) {
			return nil, false, nil
		}
		size := int(binary.BigEndian.Uint16(metadata[marker+1:]))
		end := marker + 1 + size
		if size < 2 || end > len(metadata) {
			return nil, false, nil
		}
		payload := metadata[marker+3 : end]
		if metadata[marker] == 0xe1 && bytes.HasPrefix(payload, []byte("Exif\x00\x00")) {
			if found {
				return nil, false, nil
			}
			found = true
			exif, ok := resizeExifGeometry(ctx, payload[6:], before, after)
			if !ok || len(exif)+8 > 65535 {
				return nil, false, ctx.Err()
			}
			output = append(output, metadata[copied:marker+1]...)
			output = binary.BigEndian.AppendUint16(output, uint16(len(exif)+8))
			output = append(output, "Exif\x00\x00"...)
			output = append(output, exif...)
			copied = end
		}
		start = end
	}
	if output == nil {
		return metadata, true, ctx.Err()
	}
	return append(output, metadata[copied:]...), true, ctx.Err()
}

// resizeExifGeometry 更新主图及Exif目录的尺寸、密度和主体坐标，保留缩略图目录
// 新数值单独存放，避免改写与缩略图、厂商数据或其他字段共享的原始值
// 入参: ctx 取消上下文, data TIFF结构的Exif数据, before 原尺寸, after 新尺寸
// 返回: []byte 更新后的Exif, bool 已识别字段是否可安全更新
func resizeExifGeometry(ctx context.Context, data []byte, before, after image.Point) ([]byte, bool) {
	if len(data) < 8 || before.X <= 0 || before.Y <= 0 || after.X <= 0 || after.Y <= 0 || max(before.X, before.Y, after.X, after.Y) > 65535 {
		return nil, false
	}
	var order binary.ByteOrder
	switch string(data[:2]) {
	case "II":
		order = binary.LittleEndian
	case "MM":
		order = binary.BigEndian
	default:
		return nil, false
	}
	if order.Uint16(data[2:]) != 42 {
		return nil, false
	}
	output := bytes.Clone(data)
	offset := uint64(order.Uint32(data[4:]))
	var primaryStart, primaryEnd uint64
	var resolution [2]*big.Rat
	for directory := 0; directory < 2 && offset != 0; directory++ {
		if offset < 8 || offset%2 != 0 || offset+2 > uint64(len(data)) {
			return nil, false
		}
		count := uint64(order.Uint16(data[offset:]))
		end := offset + 6 + count*12
		if end > uint64(len(data)) || directory == 1 && offset < primaryEnd && end > primaryStart {
			return nil, false
		}
		if directory == 0 {
			primaryStart, primaryEnd = offset, end
		}
		next, previous := uint64(0), -1
		for i := uint64(0); i < count; i++ {
			if ctx.Err() != nil {
				return nil, false
			}
			at := offset + 2 + i*12
			entry := data[at : at+12]
			tag, kind, number := int(order.Uint16(entry)), order.Uint16(entry[2:]), order.Uint32(entry[4:])
			if tag <= previous {
				return nil, false
			}
			previous = tag
			if directory == 0 && tag == 0x128 {
				unit := order.Uint16(entry[8:])
				if kind != 3 || number != 1 || unit < 2 || unit > 3 {
					return nil, false
				}
			}
			if directory == 0 && tag == 0x8769 {
				if kind != 4 || number != 1 {
					return nil, false
				}
				next = uint64(order.Uint32(entry[8:]))
				continue
			}
			axis, rational := -1, false
			if directory == 0 {
				switch tag {
				case 0x100, 0x101:
					axis = tag - 0x100
				case 0x11a, 0x11b:
					axis, rational = tag-0x11a, true
				}
			} else {
				switch tag {
				case 0xa002, 0xa003:
					axis = tag - 0xa002
				case 0xa20e, 0xa20f:
					axis, rational = tag-0xa20e, true
				case 0x9214, 0xa214:
					if kind != 3 || number < 2 || number > 4 || tag == 0xa214 && number != 2 {
						return nil, false
					}
					position := at + 8
					if number > 2 {
						position = uint64(order.Uint32(entry[8:]))
						if position < 8 || position%2 != 0 {
							return nil, false
						}
					}
					if position+uint64(number)*2 > uint64(len(data)) {
						return nil, false
					}
					value := bytes.Clone(data[position : position+uint64(number)*2])
					if !resizeExifSubject(order, value, before, after) {
						return nil, false
					}
					if number == 2 {
						copy(output[at+8:at+12], value)
					} else {
						output = appendExifValue(order, output, at, value)
					}
				}
			}
			if axis < 0 {
				continue
			}
			oldSize, newSize := before.X, after.X
			if axis == 1 {
				oldSize, newSize = before.Y, after.Y
			}
			if number != 1 {
				return nil, false
			}
			if rational {
				position := uint64(order.Uint32(entry[8:]))
				if kind != 5 || position < 8 || position%2 != 0 || position+8 > uint64(len(data)) || order.Uint32(data[position+4:]) == 0 {
					return nil, false
				}
				n, d := order.Uint32(data[position:]), order.Uint32(data[position+4:])
				if n == 0 {
					return nil, false
				}
				ratio := new(big.Rat).SetFrac64(int64(n)*int64(newSize), int64(d)*int64(oldSize))
				if ratio.Num().BitLen() > 32 || ratio.Denom().BitLen() > 32 {
					return nil, false
				}
				value := make([]byte, 8)
				order.PutUint32(value, uint32(ratio.Num().Uint64()))
				order.PutUint32(value[4:], uint32(ratio.Denom().Uint64()))
				output = appendExifValue(order, output, at, value)
				if directory == 0 {
					resolution[axis] = ratio
				}
			} else {
				value := uint64(order.Uint32(entry[8:]))
				if kind == 3 {
					value = uint64(order.Uint16(entry[8:]))
				} else if kind != 4 {
					return nil, false
				}
				if value == 0 || value > uint64(oldSize) {
					return nil, false
				}
				value = (value*uint64(newSize) + uint64(oldSize) - 1) / uint64(oldSize)
				if kind == 3 {
					order.PutUint16(output[at+8:], uint16(value))
				} else {
					order.PutUint32(output[at+8:], uint32(value))
				}
			}
		}
		if directory == 0 && (resolution[0] != nil || resolution[1] != nil) {
			if resolution[0] == nil || resolution[1] == nil || resolution[0].Cmp(resolution[1]) != 0 {
				return nil, false
			}
		}
		offset = next
	}
	return output, primaryEnd != 0 && ctx.Err() == nil
}

// appendExifValue 追加按双字节对齐的字段值并更新目录偏移
// 入参: order 字节序, data 输出数据, entry 目录项偏移, value 字段值
// 返回: []byte 更新后的数据
func appendExifValue(order binary.ByteOrder, data []byte, entry uint64, value []byte) []byte {
	if len(data)%2 != 0 {
		data = append(data, 0)
	}
	order.PutUint32(data[entry+8:], uint32(len(data)))
	return append(data, value...)
}

// resizeExifSubject 缩放旋转前的主体坐标和区域，非等比缩放时不将圆形误写为圆形
// 入参: order 字节序, data 独占的SHORT数组, before 原尺寸, after 新尺寸
// 返回: bool 主体几何是否可表示
func resizeExifSubject(order binary.ByteOrder, data []byte, before, after image.Point) bool {
	if len(data) == 6 && int64(before.X)*int64(after.Y) != int64(before.Y)*int64(after.X) {
		return false
	}
	for i := 0; i < len(data)/2; i++ {
		oldSize, newSize := before.X, after.X
		if i%2 == 1 {
			oldSize, newSize = before.Y, after.Y
		}
		value := uint64(order.Uint16(data[i*2:]))
		if i < 2 && value >= uint64(oldSize) || i >= 2 && (value == 0 || value > uint64(oldSize)) {
			return false
		}
		if i < 2 {
			value = value * uint64(newSize) / uint64(oldSize)
		} else {
			value = max((value*uint64(newSize)+uint64(oldSize)/2)/uint64(oldSize), 1)
		}
		order.PutUint16(data[i*2:], uint16(value))
	}
	return true
}
