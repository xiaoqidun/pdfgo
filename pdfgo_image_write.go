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
	"compress/zlib"
	"context"
	"encoding/binary"
	"fmt"
	"image"
	"image/color"
	"io"
	"maps"
	"math"
	"os"
	"slices"
)

// ImageWriteOptions 保存图像创建与替换参数
// PreblendBinary对二值透明图像采用标准Matte预混合，保留可见颜色，透明像素的隐藏颜色不保留
type ImageWriteOptions struct {
	PreblendBinary bool
}

// CreateImage 从像素创建独立PDF图像及软遮罩，不修改文档或分配间接对象编号
// 入参: ctx 取消上下文, source 像素图像, options 可选编码参数，至多一项
// 返回: *Image 图像描述及独立编码数据, error 参数、编码或关闭错误
func (r *Reader) CreateImage(ctx context.Context, source image.Image, options ...ImageWriteOptions) (*Image, error) {
	if ctx == nil || r == nil {
		return nil, fmt.Errorf("invalid image creation context")
	}
	if r.closed {
		return nil, os.ErrClosed
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if len(options) > 1 {
		return nil, fmt.Errorf("too many image write options")
	}
	var settings ImageWriteOptions
	if len(options) == 1 {
		settings = options[0]
	}
	stream, mask, err := encodePDFImage(ctx, source, settings)
	if err != nil {
		return nil, err
	}
	if mask != nil {
		stream.Dictionary["SMask"] = mask
	}
	return r.ReadImage(stream)
}

// ReplaceImagesTo 无损替换间接图像，保留页面布局、其他对象及原加密配置
// 默认保留非预乘颜色和透明度的八位或十六位精度；不修改源对象，不替换遮罩图像或签名文档
// 出错时应丢弃本次输出，Reader及图片须在调用期间保持可读
// 入参: ctx 取消上下文, writer 输出流, images 图像引用与替换像素, options 可选替换参数，至多一项
// 返回: OptimizeReport 写出结果, error 参数、编码或写入错误
func (r *Reader) ReplaceImagesTo(ctx context.Context, writer io.Writer, images map[Reference]image.Image, options ...ImageWriteOptions) (OptimizeReport, error) {
	if len(options) > 1 {
		return OptimizeReport{}, fmt.Errorf("too many image write options")
	}
	var settings ImageWriteOptions
	if len(options) == 1 {
		settings = options[0]
	}
	return r.RewriteTo(ctx, writer, RewriteOptions{Images: images, ImageOptions: settings})
}

// imageReplacements 编码替换图像并为软遮罩分配独立引用，不修改源对象
// 入参: ctx 取消上下文, images 替换图像, settings 编码参数
// 返回: map[Reference]Object 替换对象, error 参数或编码错误
func (r *Reader) imageReplacements(ctx context.Context, images map[Reference]image.Image, settings ImageWriteOptions) (map[Reference]Object, error) {
	replacements := make(map[Reference]Object, len(images)*2)
	if len(images) == 0 {
		return replacements, nil
	}
	maximum := int64(0)
	for number := range r.xref {
		maximum = max(maximum, number)
	}
	if maximum > math.MaxInt32-int64(len(images))-1024 {
		return nil, fmt.Errorf("PDF object number exceeds output limit")
	}
	refs := slices.Collect(maps.Keys(images))
	slices.SortFunc(refs, func(a, b Reference) int {
		if a.Number != b.Number {
			if a.Number < b.Number {
				return -1
			}
			return 1
		}
		if a.Generation < b.Generation {
			return -1
		}
		if a.Generation > b.Generation {
			return 1
		}
		return 0
	})
	for _, ref := range refs {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if ref.Number <= 0 || ref.Generation < 0 || ref.Generation > 65535 {
			return nil, fmt.Errorf("invalid image reference")
		}
		original, err := r.ReadImage(ref)
		if err != nil {
			return nil, err
		}
		if original.ImageMask {
			return nil, fmt.Errorf("cannot replace a stencil image")
		}
		stream, mask, err := encodePDFImage(ctx, images[ref], settings)
		if err != nil {
			return nil, err
		}
		dict := maps.Clone(original.Stream.Dictionary)
		for _, key := range []Name{"Length", "Filter", "DecodeParms", "Decode", "Mask", "SMask", "SMaskInData", "ImageMask", "Matte", "F", "FFilter", "FDecodeParms", "Alternates"} {
			delete(dict, key)
		}
		maps.Copy(dict, stream.Dictionary)
		stream.Dictionary = dict
		if mask != nil {
			mask.Dictionary["Interpolate"] = Boolean(original.Interpolate)
			maximum++
			maskRef := Reference{Number: maximum}
			stream.Dictionary["SMask"] = maskRef
			replacements[maskRef] = mask
		}
		replacements[ref] = stream
	}
	return replacements, nil
}

// encodePDFImage 逐行编码设备色像素和独立软遮罩，不分配整图样本缓冲
// 入参: ctx 取消上下文, img 像素图像, options 替换参数
// 返回: *Stream 颜色流, *Stream 软遮罩或nil, error 编码错误
func encodePDFImage(ctx context.Context, img image.Image, options ImageWriteOptions) (*Stream, *Stream, error) {
	if img == nil {
		return nil, nil, fmt.Errorf("missing replacement image")
	}
	bounds := img.Bounds()
	width, height := bounds.Dx(), bounds.Dy()
	bits, channels, space := 16, 3, Name("DeviceRGB")
	switch img.ColorModel() {
	case color.GrayModel:
		bits, channels, space = 8, 1, Name("DeviceGray")
	case color.Gray16Model:
		channels, space = 1, Name("DeviceGray")
	case color.CMYKModel:
		bits, channels, space = 8, 4, Name("DeviceCMYK")
	case color.NRGBAModel, color.AlphaModel:
		bits = 8
	}
	switch source := img.(type) {
	case *packedCMYKImage:
		channels, space = 4, Name("DeviceCMYK")
	case *jpxSampleImage:
		if source.cmyk {
			channels, space = 4, Name("DeviceCMYK")
		}
	}
	if width <= 0 || height <= 0 || width > math.MaxInt/(channels*(bits/8)) {
		return nil, nil, fmt.Errorf("invalid replacement image dimensions")
	}
	opaque, err := imageOpaqueContext(ctx, img)
	if err != nil {
		return nil, nil, err
	}
	if img.ColorModel() == color.RGBAModel && opaque {
		bits = 8
	}
	preblend := false
	if options.PreblendBinary && channels == 3 && !opaque {
		var err error
		preblend, err = imageBinaryAlpha(ctx, img)
		if err != nil {
			return nil, nil, err
		}
	}
	var pixels, alpha bytes.Buffer
	colorWriter := imageCompressor(&pixels)
	defer releaseImageCompressor(colorWriter)
	var alphaWriter *zlib.Writer
	if !opaque {
		alphaWriter = imageCompressor(&alpha)
		defer releaseImageCompressor(alphaWriter)
	}
	row := make([]byte, width*channels*(bits/8))
	var maskRow []byte
	if alphaWriter != nil {
		maskRow = make([]byte, width*(bits/8))
	}
	hasAlpha := false
	for y := bounds.Min.Y; y < bounds.Max.Y; y++ {
		if err := ctx.Err(); err != nil {
			return nil, nil, err
		}
		for x := 0; x < width; x++ {
			if x&4095 == 0 {
				if err := ctx.Err(); err != nil {
					return nil, nil, err
				}
			}
			var values [4]uint16
			a := uint16(65535)
			switch channels {
			case 1:
				values[0] = imageRGBA64At(img, bounds.Min.X+x, y).R
			case 4:
				if c, ok := imageCMYKAt(img, bounds.Min.X+x, y); ok {
					values = [4]uint16(c)
				} else {
					c := color.CMYKModel.Convert(img.At(bounds.Min.X+x, y)).(color.CMYK)
					values = [4]uint16{uint16(c.C) * 257, uint16(c.M) * 257, uint16(c.Y) * 257, uint16(c.K) * 257}
				}
				if !opaque {
					_, _, _, alpha := img.At(bounds.Min.X+x, y).RGBA()
					a = uint16(alpha)
				}
			default:
				c := imageNRGBA64At(img, bounds.Min.X+x, y)
				values[0], values[1], values[2], a = c.R, c.G, c.B, c.A
				if preblend && a == 0 {
					values[0], values[1], values[2] = 65535, 65535, 65535
				}
			}
			for c, value := range values[:channels] {
				index := (x*channels + c) * (bits / 8)
				if bits == 16 {
					binary.BigEndian.PutUint16(row[index:], value)
				} else {
					row[index] = byte(value >> 8)
				}
			}
			if alphaWriter != nil {
				if bits == 16 {
					binary.BigEndian.PutUint16(maskRow[x*2:], a)
				} else {
					maskRow[x] = byte(a >> 8)
				}
				hasAlpha = hasAlpha || a != 65535
			}
		}
		if _, err := colorWriter.Write(row); err != nil {
			return nil, nil, err
		}
		if alphaWriter != nil {
			if _, err := alphaWriter.Write(maskRow); err != nil {
				return nil, nil, err
			}
		}
	}
	if err := colorWriter.Close(); err != nil {
		return nil, nil, err
	}
	if alphaWriter != nil {
		if err := alphaWriter.Close(); err != nil {
			return nil, nil, err
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	stream := &Stream{Dictionary: Dictionary{"Type": Name("XObject"), "Subtype": Name("Image"), "Width": Integer(width), "Height": Integer(height), "ColorSpace": space, "BitsPerComponent": Integer(bits), "Filter": Name("FlateDecode")}, Data: pixels.Bytes()}
	var mask *Stream
	if hasAlpha {
		mask = &Stream{Dictionary: maps.Clone(stream.Dictionary), Data: alpha.Bytes()}
		mask.Dictionary["ColorSpace"] = Name("DeviceGray")
		if preblend {
			mask.Dictionary["Matte"] = Array{Integer(1), Integer(1), Integer(1)}
		}
	}
	return stream, mask, nil
}

// imageBinaryAlpha 判断是否具有二值透明度，不复制像素
// 入参: ctx 取消上下文, img 像素图像
// 返回: bool 存在全透明像素且无渐变透明度, error 取消错误
func imageBinaryAlpha(ctx context.Context, img image.Image) (bool, error) {
	bounds := img.Bounds()
	transparent := false
	for y := bounds.Min.Y; y < bounds.Max.Y; y++ {
		for x := bounds.Min.X; x < bounds.Max.X; x++ {
			if (x-bounds.Min.X)%256 == 0 {
				if err := ctx.Err(); err != nil {
					return false, err
				}
			}
			a := imageNRGBA64At(img, x, y).A
			if a != 0 && a != 65535 {
				return false, nil
			}
			transparent = transparent || a == 0
		}
	}
	return transparent, ctx.Err()
}
