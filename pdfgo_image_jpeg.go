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
)

// jpegFrame 保存采样布局和独立显示元数据状态
type jpegFrame struct {
	components      int
	width, height   int
	progressive     bool
	sampling        [4][2]int
	displayMetadata bool
}

// jpegEncodingImage 为不透明八位图像复用预乘视图，避免编码器逐像素接口分配
// 入参: ctx 取消上下文, source 待编码图像
// 返回: image.Image 只读编码视图, error 取消错误
func jpegEncodingImage(ctx context.Context, source image.Image) (image.Image, error) {
	s, ok := source.(*image.NRGBA)
	if !ok {
		return source, ctx.Err()
	}
	opaque, err := imageOpaqueContext(ctx, s)
	if err != nil {
		return nil, err
	}
	if !opaque {
		return source, nil
	}
	return &image.RGBA{Pix: s.Pix, Stride: s.Stride, Rect: s.Rect}, nil
}

// JPEGFile 返回可脱离PDF字典直接显示的JPEG，移除PDF忽略的方向和配置文件
// 返回: []byte 未重编码的JPEG或空值, error 解码参数错误
func (i *Image) JPEGFile() ([]byte, error) {
	return i.JPEGFileContext(context.Background())
}

// JPEGFileContext 返回可直接复用的JPEG，移除显示元数据期间检查取消
// 入参: ctx 取消上下文
// 返回: []byte 未重编码的JPEG或空值, error 参数、编码或取消错误
func (i *Image) JPEGFileContext(ctx context.Context) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if i.ImageMask || len(i.Decode) != 0 || i.ColorSpace != Name("DeviceGray") && i.ColorSpace != Name("DeviceRGB") {
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
	frame, transform, _, err := i.jpegTransformContext(ctx, data, params)
	if err != nil {
		return nil, err
	}
	config, err := jpeg.DecodeConfig(&contextInput{ctx: ctx, reader: bytes.NewReader(data)})
	if err != nil {
		return nil, err
	}
	if config.Width != i.Width || config.Height != i.Height {
		return nil, fmt.Errorf("JPEG dimensions differ from image dictionary")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if frame.components == 1 && i.ColorSpace == Name("DeviceGray") || frame.components == 3 && i.ColorSpace == Name("DeviceRGB") && transform == (config.ColorModel == color.YCbCrModel) {
		if frame.displayMetadata {
			return jpegDisplayFileContext(ctx, data)
		}
		return data, nil
	}
	return nil, nil
}

// jpegTransform 解析PDF的DCT颜色规则，Adobe标记优先于解码字典
// 入参: data JPEG数据, params DCT参数
// 返回: jpegFrame 采样布局, bool 是否进行颜色变换, bool 是否有Adobe标记, error 参数错误
func (i *Image) jpegTransform(data []byte, params Dictionary) (jpegFrame, bool, bool, error) {
	return i.jpegTransformContext(context.Background(), data, params)
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
		value, err := i.reader.Resolve(params["ColorTransform"])
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

// jpegSamples 按PDF的DCT颜色规则解码，Adobe标记优先于解码字典
// 入参: data JPEG数据, params DCT解码参数
// 返回: image.Image 原始颜色样本, error 参数或解码错误
func (i *Image) jpegSamples(data []byte, params Dictionary) (image.Image, error) {
	return i.jpegSamplesContext(context.Background(), data, params)
}

// jpegSamplesContext 按PDF的DCT颜色规则解码，在读取和逐行变换间检查取消
// 入参: ctx 取消上下文, data JPEG数据, params DCT解码参数
// 返回: image.Image 原始颜色样本, error 参数、解码或取消错误
func (i *Image) jpegSamplesContext(ctx context.Context, data []byte, params Dictionary) (image.Image, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	frame, transform, tagged, err := i.jpegTransformContext(ctx, data, params)
	if err != nil {
		return nil, err
	}
	components := frame.components
	reader := func() io.Reader { return bytes.NewReader(data) }
	if components == 4 && !tagged {
		marker := []byte{0xff, 0xee, 0, 14, 'A', 'd', 'o', 'b', 'e', 0, 100, 0, 0, 0, 0, 0}
		if transform {
			marker[15] = 2
		}
		reader = func() io.Reader {
			return io.MultiReader(bytes.NewReader(data[:2]), bytes.NewReader(marker), bytes.NewReader(data[2:]))
		}
	}
	config, err := jpeg.DecodeConfig(&contextInput{ctx: ctx, reader: reader()})
	if err != nil {
		return nil, err
	}
	if config.Width != i.Width || config.Height != i.Height {
		return nil, fmt.Errorf("JPEG dimensions differ from image dictionary")
	}
	if err := frame.sampleSize(); err != nil {
		return nil, err
	}
	if components == 4 || components == 3 && (config.ColorModel != color.YCbCrModel || !transform) {
		if _, _, err := imageBufferSize(i.Width, i.Height, 4); err != nil {
			return nil, err
		}
	}
	result, err := jpeg.Decode(&contextInput{ctx: ctx, reader: reader()})
	if err != nil {
		return nil, err
	}
	if cmyk, ok := result.(*image.CMYK); ok {
		for n := range cmyk.Pix {
			if n&4095 == 0 {
				if err := ctx.Err(); err != nil {
					return nil, err
				}
			}
			cmyk.Pix[n] = 255 - cmyk.Pix[n]
		}
		return cmyk, nil
	}
	if components != 3 {
		return result, nil
	}
	ycbcr, rawYCbCr := result.(*image.YCbCr)
	if transform == rawYCbCr {
		return result, nil
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

// jpegColorInfo 读取分量数与Adobe变换标记，正确跨过渐进扫描中的转义及重启标记
// 入参: data JPEG数据
// 返回: jpegFrame 采样布局, byte Adobe变换值, bool 是否有Adobe标记, error 格式错误
func jpegColorInfo(data []byte) (jpegFrame, byte, bool, error) {
	return jpegColorInfoContext(context.Background(), data)
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
				return jpegFrame{}, 0, false, fmt.Errorf("invalid JPEG marker")
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

// jpegMetadata 保留扫描前后的应用与注释段，识别Adobe颜色变换标记
// 入参: data JPEG数据
// 返回: []byte 元数据段, bool 是否包含Adobe标记, error 解析错误
func jpegMetadata(data []byte) ([]byte, bool, error) {
	return jpegMetadataContext(context.Background(), data)
}

// jpegMetadataContext 扫描完整编码并按原顺序保留元数据，分段检查取消
// 入参: ctx 取消上下文, data JPEG数据
// 返回: []byte 元数据段, bool 是否包含Adobe标记, error 解析或取消错误
func jpegMetadataContext(ctx context.Context, data []byte) ([]byte, bool, error) {
	var metadata []byte
	_, _, adobe, err := jpegColorSegmentsContext(ctx, data, func(marker byte, start, end int) error {
		if marker < 0xe0 || marker > 0xef && marker != 0xfe {
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
		return nil, false, err
	}
	return metadata, adobe, ctx.Err()
}

// sampleSize 校验JPEG解码器的采样平面及渐进系数缓冲
// 返回: error 不可分配的缓冲尺寸
func (f jpegFrame) sampleSize() error {
	if f.components != 1 && f.components != 3 && f.components != 4 {
		return fmt.Errorf("invalid JPEG component count")
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
	columns := (f.width + 8*maxH - 1) / (8 * maxH)
	rows := (f.height + 8*maxV - 1) / (8 * maxV)
	width, height := columns*8*maxH, rows*8*maxV
	_, size, err := imageBufferSize(width, height, 1)
	if err != nil {
		return err
	}
	if f.components > 1 {
		chromaWidth, chromaHeight := width, height
		if f.sampling[1] == f.sampling[2] && f.sampling[0] == [2]int{maxH, maxV} {
			chromaWidth = columns * 8 * f.sampling[1][0]
			chromaHeight = rows * 8 * f.sampling[1][1]
		}
		_, chromaSize, err := imageBufferSize(chromaWidth, chromaHeight, 1)
		if err != nil {
			return err
		}
		if chromaSize > (imageBufferLimit()-size)/2 {
			return fmt.Errorf("JPEG sample size exceeds platform buffer range")
		}
	}
	if f.progressive {
		for n := 0; n < f.components; n++ {
			if _, _, err := imageBufferSize(columns*8*f.sampling[n][0], rows*8*f.sampling[n][1], 4); err != nil {
				return fmt.Errorf("JPEG progressive coefficient size exceeds platform buffer range")
			}
		}
	}
	return nil
}
