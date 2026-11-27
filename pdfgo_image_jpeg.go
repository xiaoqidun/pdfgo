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
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"io"
	"math"
	"os"
)

// jpegRecoveryPixelLimit 限制异常JPEG恢复验证的像素预算，不按损坏帧头扩大分配
const jpegRecoveryPixelLimit = 128 << 20

// jpegFrame 保存采样布局和独立显示元数据状态
type jpegFrame struct {
	components      int
	width, height   int
	progressive     bool
	sampling        [4][2]int
	displayMetadata bool
}

// jpegCMYKImage 为超大CMYK图像提供零拷贝印刷色彩视图，避免超预算缓冲分配
type jpegCMYKImage struct {
	source *image.CMYK
	origin image.Point
	bounds image.Rectangle
}

// ColorModel 返回十六位非预乘sRGB颜色模型
// 返回: color.Model 颜色模型
func (j *jpegCMYKImage) ColorModel() color.Model {
	return color.NRGBA64Model
}

// Bounds 返回原点归一化后的图像边界
// 返回: image.Rectangle 图像边界
func (j *jpegCMYKImage) Bounds() image.Rectangle {
	return j.bounds
}

// At 按内置印刷配置读取像素sRGB颜色
// 入参: x 横向坐标, y 纵向坐标
// 返回: color.Color 十六位非预乘颜色
func (j *jpegCMYKImage) At(x, y int) color.Color {
	return imageNRGBA64At(j.source, j.origin.X+x, j.origin.Y+y)
}

// RGBA64At 按内置印刷配置读取像素十六位预乘颜色
// 入参: x 横向坐标, y 纵向坐标
// 返回: color.RGBA64 十六位预乘颜色
func (j *jpegCMYKImage) RGBA64At(x, y int) color.RGBA64 {
	return imageRGBA64At(j.source, j.origin.X+x, j.origin.Y+y)
}

// jpegEncodingImage 为不透明八位及CMYK图像生成一致的JPEG编码视图，避免朴素CMYK公式与逐像素接口分配
// 入参: ctx 取消上下文, source 待编码图像
// 返回: image.Image 只读编码视图, error 缓冲或取消错误
func jpegEncodingImage(ctx context.Context, source image.Image) (image.Image, error) {
	switch s := source.(type) {
	case *image.NRGBA:
		opaque, err := imageOpaqueContext(ctx, s)
		if err != nil {
			return nil, err
		}
		if !opaque {
			return source, nil
		}
		return &image.RGBA{Pix: s.Pix, Stride: s.Stride, Rect: s.Rect}, nil
	case *image.CMYK:
		return jpegCMYKEncodingImage(ctx, s)
	default:
		return source, ctx.Err()
	}
}

// jpegCMYKEncodingImage 按内置印刷配置将CMYK图像转换为原点对齐的不透明RGBA，超预算时退回零拷贝视图
// 入参: ctx 取消上下文, src 四色像素图像
// 返回: image.Image sRGB编码视图, error 缓冲或取消错误
func jpegCMYKEncodingImage(ctx context.Context, src *image.CMYK) (image.Image, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	bounds := src.Bounds()
	w, h := bounds.Dx(), bounds.Dy()
	if bounds.Empty() || w <= 0 || h <= 0 {
		return src, nil
	}
	if err := validateNativePixelBuffer(src.Pix, src.Stride, w, h, 4); err != nil {
		return nil, err
	}
	if !pixelsWithinBudget(int64(w), int64(h), optimizationBufferLimit/4) {
		return &jpegCMYKImage{source: src, origin: bounds.Min, bounds: image.Rect(0, 0, w, h)}, nil
	}
	dst := image.NewRGBA(image.Rect(0, 0, w, h))
	type cmykCacheEntry struct {
		key   uint32
		rgb   [3]uint8
		valid bool
	}
	var cache [4096]cmykCacheEntry
	for y := 0; y < h; y++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		srcRow := src.Pix[src.PixOffset(bounds.Min.X, bounds.Min.Y+y):]
		dstRow := dst.Pix[y*dst.Stride:]
		for x := 0; x < w; x++ {
			if x&4095 == 4095 {
				if err := ctx.Err(); err != nil {
					return nil, err
				}
			}
			off := x * 4
			c, m, yv, k := srcRow[off], srcRow[off+1], srcRow[off+2], srcRow[off+3]
			key := uint32(c) | uint32(m)<<8 | uint32(yv)<<16 | uint32(k)<<24
			slot := (key * 2654435761) >> 20
			entry := &cache[slot]
			var rgb [3]uint8
			if entry.valid && entry.key == key {
				rgb = entry.rgb
			} else {
				pixel := imageNRGBASample(color.CMYK{C: c, M: m, Y: yv, K: k})
				rgb = [3]uint8{
					uint8((uint32(pixel.R) + 128) / 257),
					uint8((uint32(pixel.G) + 128) / 257),
					uint8((uint32(pixel.B) + 128) / 257),
				}
				entry.key, entry.rgb, entry.valid = key, rgb, true
			}
			dstRow[off], dstRow[off+1], dstRow[off+2], dstRow[off+3] = rgb[0], rgb[1], rgb[2], 255
		}
	}
	return dst, ctx.Err()
}

// jpegAdobeMarker 构建四通道CMYK或YCCK的Adobe APP14标记
// 入参: transform 是否采用YCCK颜色变换
// 返回: []byte 16字节APP14标记段
func jpegAdobeMarker(transform bool) []byte {
	marker := []byte{0xff, 0xee, 0, 14, 'A', 'd', 'o', 'b', 'e', 0, 100, 0, 0, 0, 0, 0}
	if transform {
		marker[15] = 2
	}
	return marker
}

// jpegColorInfoContext 分段扫描JPEG标记及渐进数据，限制取消检查间隔
// 入参: ctx 取消上下文, data JPEG数据
// 返回: jpegFrame 采样布局, byte Adobe变换, bool Adobe标记, error 格式或取消错误
func jpegColorInfoContext(ctx context.Context, data []byte) (jpegFrame, byte, bool, error) {
	return jpegColorSegmentsContext(ctx, data, nil)
}

// jpegColorSegmentsContext 解析颜色标记并访问扫描前后的完整片段
// 入参: ctx 取消上下文, data JPEG数据, visit 可选片段处理函数
// 返回: jpegFrame 采样布局, byte Adobe变换, bool Adobe标记, error 格式、处理或取消错误
func jpegColorSegmentsContext(ctx context.Context, data []byte, visit func(byte, int, int) error) (jpegFrame, byte, bool, error) {
	return jpegReadSegmentsContext(ctx, data, visit, nil)
}

// jpegReadSegmentsContext 检查JPEG标记，恢复扫描时只移除扫描外的多余字节
// 入参: ctx 取消上下文, data 编码数据, visit 片段访问函数, skip 多余字节处理函数
// 返回: jpegFrame 采样布局, byte Adobe变换, bool Adobe标记, error 格式或取消错误
func jpegReadSegmentsContext(ctx context.Context, data []byte, visit func(byte, int, int) error, skip func(int, int) error) (jpegFrame, byte, bool, error) {
	if err := ctx.Err(); err != nil {
		return jpegFrame{}, 0, false, err
	}
	if len(data) < 2 || data[0] != 0xff || data[1] != 0xd8 {
		return jpegFrame{}, 0, false, fmt.Errorf("invalid JPEG start marker")
	}
	var frame jpegFrame
	adobe, tagged, entropy := byte(0), false, false
	for pos := 2; pos < len(data); {
		if err := ctx.Err(); err != nil {
			return jpegFrame{}, 0, false, err
		}
		if data[pos] != 0xff {
			if !entropy {
				if skip == nil {
					return jpegFrame{}, 0, false, fmt.Errorf("invalid JPEG marker")
				}
				end := pos + min(65536, len(data)-pos)
				n := bytes.IndexByte(data[pos:end], 0xff)
				if n >= 0 {
					end = pos + n
				}
				if err := skip(pos, end); err != nil {
					return jpegFrame{}, 0, false, err
				}
				pos = end
				continue
			}
			end := pos + min(65536, len(data)-pos)
			n := bytes.IndexByte(data[pos:end], 0xff)
			if n < 0 {
				if end == len(data) {
					return jpegFrame{}, 0, false, fmt.Errorf("truncated JPEG scan")
				}
				pos = end
				continue
			}
			pos += n
		}
		start := pos
		for pos < len(data) && data[pos] == 0xff {
			if pos&4095 == 0 {
				if err := ctx.Err(); err != nil {
					return jpegFrame{}, 0, false, err
				}
			}
			pos++
		}
		if pos == len(data) {
			break
		}
		marker := data[pos]
		pos++
		if entropy && (marker == 0 || marker >= 0xd0 && marker <= 0xd7) {
			continue
		}
		if skip != nil && marker == 0 {
			if err := skip(start, pos); err != nil {
				return jpegFrame{}, 0, false, err
			}
			continue
		}
		entropy = false
		if marker == 0xd9 {
			return frame, adobe, tagged, nil
		}
		if marker == 1 {
			continue
		}
		if marker == 0 || marker == 0xd8 || len(data)-pos < 2 {
			return jpegFrame{}, 0, false, fmt.Errorf("invalid JPEG marker")
		}
		size := int(binary.BigEndian.Uint16(data[pos:]))
		if size < 2 || size > len(data)-pos {
			return jpegFrame{}, 0, false, fmt.Errorf("invalid JPEG segment length")
		}
		segment := data[pos+2 : pos+size]
		if marker == 0xe1 && bytes.HasPrefix(segment, []byte("Exif\x00\x00")) || marker == 0xe2 && bytes.HasPrefix(segment, []byte("ICC_PROFILE\x00")) {
			frame.displayMetadata = true
		}
		if visit != nil {
			if err := visit(marker, start, pos+size); err != nil {
				return jpegFrame{}, 0, false, err
			}
		}
		switch marker {
		case 0xc0, 0xc1, 0xc2:
			if len(segment) < 6 {
				return jpegFrame{}, 0, false, fmt.Errorf("invalid JPEG frame header")
			}
			frame.components = int(segment[5])
			frame.width = int(binary.BigEndian.Uint16(segment[3:5]))
			frame.height = int(binary.BigEndian.Uint16(segment[1:3]))
			frame.progressive = marker == 0xc2
			if len(segment) != 6+3*frame.components {
				return jpegFrame{}, 0, false, fmt.Errorf("invalid JPEG frame header")
			}
			for n := 0; n < min(frame.components, len(frame.sampling)); n++ {
				frame.sampling[n] = [2]int{int(segment[7+3*n] >> 4), int(segment[7+3*n] & 15)}
			}
		case 0xee:
			if len(segment) >= 12 && string(segment[:5]) == "Adobe" {
				adobe, tagged = segment[11], true
			}
		case 0xda:
			entropy = true
		}
		pos += size
	}
	return jpegFrame{}, 0, false, fmt.Errorf("missing JPEG end marker")
}

// jpegRestoreFramePrefix 保留原量化数据，仅恢复被前一片段长度吞入的帧标记前缀
// 入参: ctx 取消上下文, data 待完整解码验证的JPEG副本
// 返回: []byte 恢复标记前缀的编码或原编码, error 取消错误
func jpegRestoreFramePrefix(ctx context.Context, data []byte) ([]byte, error) {
	for position := 2; position+4 <= len(data); {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if data[position] != 255 {
			if data[position-1] == 255 && (data[position] == 192 || data[position] == 193 || data[position] == 194) {
				result := make([]byte, len(data)+1)
				copy(result, data[:position])
				result[position] = 255
				copy(result[position+1:], data[position:])
				return result, ctx.Err()
			}
			return data, nil
		}
		marker := data[position+1]
		if marker == 218 || marker == 217 {
			return data, nil
		}
		length := int(binary.BigEndian.Uint16(data[position+2:]))
		if length < 2 || length > len(data)-position-2 {
			return data, nil
		}
		position += length + 2
	}
	return data, ctx.Err()
}

// jpegDisplayFileContext 无损移除PDF忽略的Exif和ICC片段，保留颜色标记及扫描数据
// 入参: ctx 取消上下文, data JPEG数据
// 返回: []byte 独立JPEG副本, error 标记或取消错误
func jpegDisplayFileContext(ctx context.Context, data []byte) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	output := make([]byte, 0, len(data))
	pos := 0
	appendUntil := func(end int) error {
		for pos < end {
			if err := ctx.Err(); err != nil {
				return err
			}
			next := pos + min(65536, end-pos)
			output = append(output, data[pos:next]...)
			pos = next
		}
		return ctx.Err()
	}
	_, _, _, err := jpegColorSegmentsContext(ctx, data, func(marker byte, start, end int) error {
		segment := start + 1
		for data[segment] == 255 {
			segment++
		}
		payload := data[segment+3 : end]
		if !(marker == 0xe1 && bytes.HasPrefix(payload, []byte("Exif\x00\x00")) || marker == 0xe2 && bytes.HasPrefix(payload, []byte("ICC_PROFILE\x00"))) {
			return nil
		}
		if err := appendUntil(start); err != nil {
			return err
		}
		pos = end
		return nil
	})
	if err != nil {
		return nil, err
	}
	if err := appendUntil(len(data)); err != nil {
		return nil, err
	}
	return output, nil
}

// jpegMetadataProfileContext 扫描JPEG元数据并按需剥离色彩标记与重组ICC分段
// 入参: ctx 取消上下文, data JPEG数据, stripColor 是否剥离APP14与ICC_PROFILE段
// 返回: []byte 保留的元数据段, []byte 重组后的ICC数据, jpegFrame 帧头信息, byte Adobe变换值, bool 是否含Adobe标记, bool ICC分段是否有效, error 解析或取消错误
func jpegMetadataProfileContext(ctx context.Context, data []byte, stripColor bool) ([]byte, []byte, jpegFrame, byte, bool, bool, error) {
	var metadata []byte
	var parts [255][]byte
	count, found, total := 0, 0, 0
	iccValid := true
	frame, adobe, tagged, err := jpegColorSegmentsContext(ctx, data, func(marker byte, start, end int) error {
		if marker < 0xe0 || marker > 0xef && marker != 0xfe {
			return nil
		}
		segment := start + 1
		for data[segment] == 255 {
			segment++
		}
		payload := data[segment+3 : end]
		isICC := marker == 0xe2 && bytes.HasPrefix(payload, []byte("ICC_PROFILE\x00"))
		isAdobe := marker == 0xee && len(payload) >= 12 && bytes.HasPrefix(payload, []byte("Adobe"))
		if isICC && stripColor {
			if len(payload) <= 14 {
				iccValid = false
			} else {
				index, n := int(payload[12]), int(payload[13])
				if index == 0 || index > n || count != 0 && count != n || parts[index-1] != nil {
					iccValid = false
				} else {
					count, found = n, found+1
					parts[index-1] = payload[14:]
					total += len(payload) - 14
				}
			}
			return nil
		}
		if isAdobe && stripColor {
			return nil
		}
		for start < end {
			if err := ctx.Err(); err != nil {
				return err
			}
			next := start + min(65536, end-start)
			metadata = append(metadata, data[start:next]...)
			start = next
		}
		return ctx.Err()
	})
	if err != nil {
		return nil, nil, jpegFrame{}, 0, false, false, err
	}
	if !stripColor || found == 0 {
		return metadata, nil, frame, adobe, tagged, iccValid, ctx.Err()
	}
	if !iccValid || found != count {
		return metadata, nil, frame, adobe, tagged, false, ctx.Err()
	}
	profile := make([]byte, 0, total)
	for _, part := range parts[:count] {
		profile = append(profile, part...)
	}
	return metadata, profile, frame, adobe, tagged, true, ctx.Err()
}

// encodedImageDimensions 在取消上下文内只读取单层JPEG或JPEG2000图像头，不为无效字典推测尺寸
// 入参: ctx 取消上下文, stream 图像编码流
// 返回: int 编码宽度, int 编码高度, error 不可确定的尺寸、编码或取消错误
func (r *Reader) encodedImageDimensions(ctx context.Context, stream *Stream) (int, int, error) {
	if err := ctx.Err(); err != nil {
		return 0, 0, err
	}
	filters, _, err := stream.filterChain(r)
	if err != nil {
		return 0, 0, err
	}
	if err := ctx.Err(); err != nil {
		return 0, 0, err
	}
	if len(filters) == 1 {
		switch filters[0] {
		case Name("DCTDecode"):
			config, err := jpeg.DecodeConfig(&contextInput{ctx: ctx, reader: bytes.NewReader(stream.Data)})
			if err := ctx.Err(); err != nil {
				return 0, 0, err
			}
			if err == nil && (config.Height <= 0 || config.Width <= 0 || config.Width > jpegRecoveryPixelLimit/config.Height) {
				return 0, 0, fmt.Errorf("JPEG dimension recovery exceeds sample budget")
			}
			return config.Width, config.Height, err
		case Name("JPXDecode"):
			data, err := jpxCodestream(stream.Data)
			if err != nil {
				return 0, 0, err
			}
			if err := ctx.Err(); err != nil {
				return 0, 0, err
			}
			width, height, _, err := jpxSize(data)
			if err == nil && (height <= 0 || width <= 0 || width > jpegRecoveryPixelLimit/height) {
				return 0, 0, fmt.Errorf("JPEG2000 dimension recovery exceeds sample budget")
			}
			return width, height, err
		}
	}
	return 0, 0, fmt.Errorf("image encoding has no independent dimensions")
}

// JPEGFile 返回可脱离PDF字典直接显示的JPEG，移除PDF忽略的方向和配置文件
// 返回: []byte 未重编码的JPEG或空值, error 解码参数错误
func (i *Image) JPEGFile() ([]byte, error) {
	return i.JPEGFileContext(context.Background())
}

// JPEGFileContext 返回可直接复用的JPEG，移除显示元数据期间检查取消
// 支持灰度、RGB及符合Adobe反相约定的四通道CMYK图像直通，不重编码像素
// 入参: ctx 取消上下文
// 返回: []byte 未重编码的JPEG或空值, error 参数、编码或取消错误
func (i *Image) JPEGFileContext(ctx context.Context) ([]byte, error) {
	if ctx == nil || i == nil || i.Stream == nil {
		return nil, fmt.Errorf("invalid image context")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if i.reader != nil && i.reader.closed {
		return nil, os.ErrClosed
	}
	colorSpace := normalizeDeviceColorSpace(i.ColorSpace)
	invertedCMYK := colorSpace == Name("DeviceCMYK") && i.isInvertedCMYKDecode()
	if i.ImageMask || !invertedCMYK && (len(i.Decode) != 0 || colorSpace != Name("DeviceGray") && colorSpace != Name("DeviceRGB")) {
		return nil, nil
	}
	masked, err := i.HasMask()
	if err != nil || masked {
		return nil, err
	}
	data, params, err := i.encodedFileContext(ctx, "DCTDecode")
	if err != nil {
		return nil, err
	}
	if data == nil {
		return nil, nil
	}
	data, frame, transform, tagged, err := i.jpegReadTransformContext(ctx, data, params)
	if err != nil {
		return nil, err
	}
	reader := func() io.Reader { return bytes.NewReader(data) }
	if invertedCMYK && frame.components == 4 && !tagged {
		marker := jpegAdobeMarker(transform)
		reader = func() io.Reader {
			return io.MultiReader(bytes.NewReader(data[:2]), bytes.NewReader(marker), bytes.NewReader(data[2:]))
		}
	}
	config, err := jpeg.DecodeConfig(&contextInput{ctx: ctx, reader: reader()})
	if err != nil {
		return nil, err
	}
	if config.Width != i.Width || config.Height != i.Height {
		if i.Warning == nil || !jpegDimensionRecoveryAllowed(i.Width, i.Height, config.Width, config.Height) {
			return nil, fmt.Errorf("JPEG dimensions differ from image dictionary")
		}
		i.Warning(Diagnostic{Message: fmt.Sprintf("JPEG dimensions differ from image dictionary; using complete image data dimensions %dx%d", config.Width, config.Height)})
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if frame.components == 1 && colorSpace == Name("DeviceGray") || frame.components == 3 && colorSpace == Name("DeviceRGB") && transform == (config.ColorModel == color.YCbCrModel) {
		if frame.displayMetadata {
			return jpegDisplayFileContext(ctx, data)
		}
		return bytes.Clone(data), nil
	}
	if invertedCMYK && frame.components == 4 && config.ColorModel == color.CMYKModel {
		if frame.displayMetadata {
			data, err = jpegDisplayFileContext(ctx, data)
			if err != nil {
				return nil, err
			}
		}
		if !tagged {
			marker := jpegAdobeMarker(transform)
			out := make([]byte, len(data)+len(marker))
			copy(out, data[:2])
			copy(out[2:], marker)
			copy(out[2+len(marker):], data[2:])
			return out, nil
		}
		return bytes.Clone(data), nil
	}
	return nil, nil
}

// isInvertedCMYKDecode 判断解码数组是否为标准四通道反相映射
// 返回: bool 是否为反相四通道解码
func (i *Image) isInvertedCMYKDecode() bool {
	if len(i.Decode) != 8 {
		return false
	}
	values, ok, err := i.optionalNumberArray(i.Decode, 8)
	if err != nil || !ok {
		return false
	}
	return values[0] == 1 && values[1] == 0 && values[2] == 1 && values[3] == 0 && values[4] == 1 && values[5] == 0 && values[6] == 1 && values[7] == 0
}

// jpegTransformContext 解析DCT颜色规则并检查标记扫描取消
// 入参: ctx 取消上下文, data JPEG数据, params DCT参数
// 返回: jpegFrame 采样布局, bool 颜色变换, bool Adobe标记, error 参数或取消错误
func (i *Image) jpegTransformContext(ctx context.Context, data []byte, params Dictionary) (jpegFrame, bool, bool, error) {
	frame, adobe, tagged, err := jpegColorInfoContext(ctx, data)
	if err != nil {
		return jpegFrame{}, false, false, err
	}
	components := frame.components
	transform := components == 3
	if tagged {
		if components == 3 && adobe != 0 && adobe != 1 || components == 4 && adobe != 0 && adobe != 2 {
			return jpegFrame{}, false, false, fmt.Errorf("invalid Adobe JPEG color transform")
		}
		transform = adobe != 0
	} else if (components == 3 || components == 4) && params["ColorTransform"] != nil {
		value, err := i.reader.resolveOptional(params["ColorTransform"])
		if err != nil {
			return jpegFrame{}, false, false, err
		}
		if value != Integer(0) && value != Integer(1) {
			return jpegFrame{}, false, false, fmt.Errorf("invalid JPEG ColorTransform")
		}
		transform = value == Integer(1)
	}
	return frame, transform, tagged, nil
}

// jpegSamplesContext 按PDF的DCT颜色规则解码，在读取和逐行变换间检查取消
// 入参: ctx 取消上下文, data JPEG数据, params DCT解码参数
// 返回: image.Image 原始颜色样本, error 参数、解码或取消错误
func (i *Image) jpegSamplesContext(ctx context.Context, data []byte, params Dictionary) (image.Image, error) {
	return i.jpegSamplesSizeContext(ctx, data, params, image.Point{})
}

// jpegSamplesSizeContext 按PDF的DCT颜色规则解码，并在存在显式缩小需求时直接盒式降采样
// 入参: ctx 取消上下文, data JPEG数据, params DCT解码参数, size 目标尺寸限制
// 返回: image.Image 原始或降采样后的颜色样本, error 参数、解码或取消错误
func (i *Image) jpegSamplesSizeContext(ctx context.Context, data []byte, params Dictionary, size image.Point) (image.Image, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	data, frame, transform, tagged, err := i.jpegReadTransformContext(ctx, data, params)
	if err != nil {
		return nil, err
	}
	components := frame.components
	reader := func() io.Reader { return bytes.NewReader(data) }
	if components == 4 && !tagged {
		marker := jpegAdobeMarker(transform)
		reader = func() io.Reader {
			return io.MultiReader(bytes.NewReader(data[:2]), bytes.NewReader(marker), bytes.NewReader(data[2:]))
		}
	}
	config, err := jpeg.DecodeConfig(&contextInput{ctx: ctx, reader: reader()})
	if err != nil {
		return nil, err
	}
	if config.Width != i.Width || config.Height != i.Height {
		if i.Warning == nil || !jpegDimensionRecoveryAllowed(i.Width, i.Height, config.Width, config.Height) {
			return nil, fmt.Errorf("JPEG dimensions differ from image dictionary")
		}
		i.Warning(Diagnostic{Message: fmt.Sprintf("JPEG dimensions differ from image dictionary; using image data dimensions %dx%d", config.Width, config.Height)})
	}
	maxBytes, _ := effectiveStreamLimits(i.reader, i.Stream)
	if err := frame.sampleSize(maxBytes); err != nil {
		return nil, err
	}
	dst := boundedSampleSize(config.Width, config.Height, size)
	downscale := size.X > 0 && size.Y > 0 && (dst.X < config.Width || dst.Y < config.Height)
	if !downscale && (components == 4 || components == 3 && (config.ColorModel != color.YCbCrModel || !transform)) {
		if _, _, err := imageCompositeBufferSize(config.Width, config.Height, 4, maxBytes); err != nil {
			return nil, err
		}
	}
	result, err := jpeg.Decode(&contextInput{ctx: ctx, reader: reader()})
	if err != nil {
		return nil, err
	}
	if cmyk, ok := result.(*image.CMYK); ok {
		out := &packedCMYKImage{data: cmyk.Pix, rect: cmyk.Rect, stride: cmyk.Stride, depth: 8}
		if downscale {
			scaled, err := downsampleDecodedImage(ctx, out, dst, maxBytes)
			if err != nil {
				return nil, err
			}
			out = scaled.(*packedCMYKImage)
		}
		for n := range out.data {
			if n&4095 == 0 {
				if err := ctx.Err(); err != nil {
					return nil, err
				}
			}
			out.data[n] = 255 - out.data[n]
		}
		return out, nil
	}
	if components != 3 {
		if downscale {
			return downsampleDecodedImage(ctx, result, dst, maxBytes)
		}
		return result, nil
	}
	ycbcr, rawYCbCr := result.(*image.YCbCr)
	if downscale && rawYCbCr {
		return downsampleYCbCr(ctx, ycbcr, dst, transform, maxBytes)
	}
	if transform == rawYCbCr {
		if downscale {
			return downsampleDecodedImage(ctx, result, dst, maxBytes)
		}
		return result, nil
	}
	if downscale {
		result, err = downsampleDecodedImage(ctx, result, dst, maxBytes)
		if err != nil {
			return nil, err
		}
	}
	out := image.NewRGBA(result.Bounds())
	for y := out.Rect.Min.Y; y < out.Rect.Max.Y; y++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		for x := out.Rect.Min.X; x < out.Rect.Max.X; x++ {
			var red, green, blue uint8
			if rawYCbCr {
				value := ycbcr.YCbCrAt(x, y)
				red, green, blue = value.Y, value.Cb, value.Cr
			} else {
				r, g, b, _ := result.At(x, y).RGBA()
				red, green, blue = color.YCbCrToRGB(uint8(r>>8), uint8(g>>8), uint8(b>>8))
			}
			out.SetRGBA(x, y, color.RGBA{R: red, G: green, B: blue, A: 255})
		}
	}
	return out, nil
}

// jpegReadTransformContext 仅在完整样本可严格解码时恢复结束标记及扫描外冗余数据
// 入参: ctx 取消上下文, data JPEG数据, params DCT参数
// 返回: []byte 有效编码, jpegFrame 采样布局, bool 颜色变换, bool Adobe标记, error 解码或取消错误
func (i *Image) jpegReadTransformContext(ctx context.Context, data []byte, params Dictionary) ([]byte, jpegFrame, bool, bool, error) {
	frame, transform, tagged, err := i.jpegTransformContext(ctx, data, params)
	if err == nil && i.Warning != nil && (frame.width != i.Width || frame.height != i.Height) {
		data, err = i.jpegDimensionFileContext(ctx, data, frame, params)
		if err == nil {
			frame, transform, tagged, err = i.jpegTransformContext(ctx, data, params)
		}
		return data, frame, transform, tagged, err
	}
	if err == nil || i.Warning == nil || ctx.Err() != nil {
		return data, frame, transform, tagged, err
	}
	originalErr := err
	if len(data) < 2 || data[0] != 255 || data[1] != 216 || len(data) > imageBufferLimit()-2 {
		return nil, jpegFrame{}, false, false, originalErr
	}
	candidate := make([]byte, 0, len(data)+2)
	for position := 0; position < len(data); {
		if err := ctx.Err(); err != nil {
			return nil, jpegFrame{}, false, false, err
		}
		end := position + min(65536, len(data)-position)
		candidate = append(candidate, data[position:end]...)
		position = end
	}
	if data[len(data)-2] != 255 || data[len(data)-1] != 217 {
		candidate = append(candidate, 255, 217)
	}
	candidate, err = jpegRestoreFramePrefix(ctx, candidate)
	if err != nil {
		return nil, jpegFrame{}, false, false, err
	}
	var normalized []byte
	position := 0
	frame, _, _, err = jpegReadSegmentsContext(ctx, candidate, nil, func(start, end int) error {
		normalized = append(normalized, candidate[position:start]...)
		position = end
		return ctx.Err()
	})
	if err != nil {
		return nil, jpegFrame{}, false, false, err
	}
	if position != 0 {
		normalized = append(normalized, candidate[position:]...)
		candidate = normalized
	}
	maxBytes, _ := effectiveStreamLimits(i.reader, i.Stream)
	if err := frame.sampleSize(maxBytes); err != nil {
		return nil, jpegFrame{}, false, false, err
	}
	if frame.height <= 0 || frame.width <= 0 || frame.width > jpegRecoveryPixelLimit/frame.height {
		return nil, jpegFrame{}, false, false, fmt.Errorf("JPEG framing recovery exceeds sample budget")
	}
	validation := *i
	validation.Width, validation.Height, validation.Warning = frame.width, frame.height, nil
	if _, err := validation.jpegSamplesContext(ctx, candidate, params); err != nil {
		return nil, jpegFrame{}, false, false, err
	}
	frame, transform, tagged, err = i.jpegTransformContext(ctx, candidate, params)
	if err != nil {
		return nil, jpegFrame{}, false, false, err
	}
	i.Warning(Diagnostic{Message: fmt.Sprintf("recovered JPEG framing after complete sample validation: %v", originalErr)})
	return candidate, frame, transform, tagged, ctx.Err()
}

// jpegDimensionRecoveryAllowed 判断宽松模式下JPEG与字典的尺寸差异是否属于可恢复的单向偏差
// 入参: dictWidth 字典宽度, dictHeight 字典高度, frameWidth 编码宽度, frameHeight 编码高度
// 返回: bool 是否允许按编码尺寸恢复
func jpegDimensionRecoveryAllowed(dictWidth, dictHeight, frameWidth, frameHeight int) bool {
	if frameWidth <= 0 || frameHeight <= 0 {
		return false
	}
	return frameWidth == dictWidth || frameHeight == dictHeight
}

// jpegDimensionFileContext 用完整扫描验证字典高度候选，禁止截去额外MCU行或按损坏高度分配巨幅图像
// 入参: ctx 取消上下文, data JPEG数据, frame 原编码布局, params DCT参数
// 返回: []byte 已验证的编码副本或原编码, error 不完整扫描、预算或取消错误
func (i *Image) jpegDimensionFileContext(ctx context.Context, data []byte, frame jpegFrame, params Dictionary) ([]byte, error) {
	if !jpegDimensionRecoveryAllowed(i.Width, i.Height, frame.width, frame.height) {
		return nil, fmt.Errorf("JPEG dimensions differ from image dictionary")
	}
	validate := func(encoded []byte, width, height int) error {
		if width <= 0 || height <= 0 || width > jpegRecoveryPixelLimit/height {
			return fmt.Errorf("JPEG dimension recovery exceeds sample budget")
		}
		validation := *i
		validation.Width, validation.Height, validation.Warning = width, height, nil
		_, err := validation.jpegSamplesContext(ctx, encoded, params)
		return err
	}
	maxH, maxV := 1, 1
	for n := 0; n < min(frame.components, len(frame.sampling)); n++ {
		maxH = max(maxH, frame.sampling[n][0])
		maxV = max(maxV, frame.sampling[n][1])
	}
	if !frame.progressive && frame.width == i.Width && i.Height > 0 && i.Height < frame.height && (frame.width+8*maxH-1)/(8*maxH) > 8 {
		frameStart, scanStart, scanCount := 0, 0, 0
		_, _, _, err := jpegColorSegmentsContext(ctx, data, func(marker byte, start, end int) error {
			switch marker {
			case 0xc0, 0xc1:
				if frameStart != 0 {
					return fmt.Errorf("multiple JPEG frames cannot recover dimensions")
				}
				for data[start+1] == 255 {
					start++
				}
				frameStart = start + 5
			case 0xda:
				scanCount++
				for data[start+1] == 255 {
					start++
				}
				if start+4 < end && int(data[start+4]) == frame.components {
					scanStart = end
				}
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
		if frameStart != 0 && scanStart != 0 && scanCount == 1 {
			end := scanStart
			for end+1 < len(data) {
				if end&4095 == 0 {
					if err := ctx.Err(); err != nil {
						return nil, err
					}
				}
				if data[end] != 255 {
					end++
					continue
				}
				marker := data[end+1]
				if marker == 0 || marker >= 0xd0 && marker <= 0xd7 {
					end += 2
					continue
				}
				if marker == 255 {
					end++
					continue
				}
				break
			}
			if end > scanStart && end+1 < len(data) && data[end+1] == 217 && data[end-1] != 0 {
				unit := 8 * maxV
				firstHeight := (i.Height + unit - 1) / unit * unit
				for _, height := range [2]int{firstHeight, firstHeight + unit} {
					if height >= frame.height || height > 65535 {
						continue
					}
					candidate := bytes.Clone(data)
					binary.BigEndian.PutUint16(candidate[frameStart:], uint16(height))
					if err := validate(candidate, frame.width, height); err == nil {
						probe := make([]byte, end+1)
						copy(probe, candidate[:end-1])
						probe[end-1], probe[end] = 255, 217
						if err := validate(probe, frame.width, height); err != nil && ctx.Err() == nil {
							i.Warning(Diagnostic{Message: fmt.Sprintf("JPEG frame height %d disagrees with complete scan; recovered verified height %d", frame.height, height)})
							return candidate, nil
						}
					}
				}
			}
		}
	}
	if err := validate(data, frame.width, frame.height); err != nil {
		return nil, err
	}
	return data, ctx.Err()
}

// sampleSize 校验JPEG解码器的采样平面、黑色通道、渐进系数及流内存预算
// 入参: maxBytes 流解压字节上限，非正数表示仅校验平台缓冲上限
// 返回: error 不可分配的缓冲尺寸或超限错误
func (f jpegFrame) sampleSize(maxBytes int64) error {
	if f.components != 1 && f.components != 3 && f.components != 4 {
		return fmt.Errorf("invalid JPEG component count")
	}
	if f.width <= 0 || f.height <= 0 {
		return fmt.Errorf("invalid JPEG dimensions")
	}
	if f.components == 1 {
		f.sampling[0] = [2]int{1, 1}
	}
	maxH, maxV := 1, 1
	for n := 0; n < f.components; n++ {
		h, v := f.sampling[n][0], f.sampling[n][1]
		if h < 1 || h > 4 || v < 1 || v > 4 {
			return fmt.Errorf("invalid JPEG sampling factors")
		}
		maxH, maxV = max(maxH, h), max(maxV, v)
	}
	budget := imageCompositeBudget(maxBytes)
	limit := int64(imageBufferLimit())
	overflow := func(msg string) error {
		if budget > 0 {
			return fmt.Errorf("%w: JPEG sample buffer exceeds limit", ErrStreamLimitExceeded)
		}
		return fmt.Errorf("%s", msg)
	}
	if int64(f.width) > math.MaxInt64-int64(8*maxH) || int64(f.height) > math.MaxInt64-int64(8*maxV) {
		return overflow("JPEG sample size exceeds platform buffer range")
	}
	columns := (int64(f.width) + int64(8*maxH) - 1) / int64(8*maxH)
	rows := (int64(f.height) + int64(8*maxV) - 1) / int64(8*maxV)
	width, height := columns*int64(8*maxH), rows*int64(8*maxV)
	if width > math.MaxInt64/height {
		return overflow("JPEG sample size exceeds platform buffer range")
	}
	size := width * height
	imgBytes := size
	if budget > 0 && imgBytes > budget {
		return fmt.Errorf("%w: JPEG sample buffer exceeds limit", ErrStreamLimitExceeded)
	}
	sampleOverLimit := size > limit
	if f.components > 1 {
		chromaWidth, chromaHeight := width, height
		if f.sampling[1] == f.sampling[2] && f.sampling[0] == [2]int{maxH, maxV} {
			chromaWidth = columns * int64(8*f.sampling[1][0])
			chromaHeight = rows * int64(8*f.sampling[1][1])
		}
		chromaSize := chromaWidth * chromaHeight
		if f.components == 4 {
			if size > math.MaxInt64-imgBytes {
				return overflow("JPEG sample size exceeds platform buffer range")
			}
			imgBytes += size
		}
		if chromaSize > (math.MaxInt64-imgBytes)/2 {
			return overflow("JPEG sample size exceeds platform buffer range")
		}
		imgBytes += 2 * chromaSize
		if budget > 0 && imgBytes > budget {
			return fmt.Errorf("%w: JPEG sample buffer exceeds limit", ErrStreamLimitExceeded)
		}
		if imgBytes > limit {
			sampleOverLimit = true
		}
	}
	total := imgBytes
	progOverLimit := false
	if f.progressive {
		for n := 0; n < f.components; n++ {
			pw, ph := columns*int64(8*f.sampling[n][0]), rows*int64(8*f.sampling[n][1])
			if pw > math.MaxInt64/4/ph {
				return overflow("JPEG progressive coefficient size exceeds platform buffer range")
			}
			progSize := pw * ph * 4
			if progSize > math.MaxInt64-total {
				return overflow("JPEG progressive coefficient size exceeds platform buffer range")
			}
			total += progSize
			if budget > 0 && total > budget {
				return fmt.Errorf("%w: JPEG sample buffer exceeds limit", ErrStreamLimitExceeded)
			}
			if total > limit {
				progOverLimit = true
			}
		}
	}
	if sampleOverLimit {
		return fmt.Errorf("JPEG sample size exceeds platform buffer range")
	}
	if progOverLimit {
		return fmt.Errorf("JPEG progressive coefficient size exceeds platform buffer range")
	}
	return nil
}
