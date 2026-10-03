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

// ReplaceImagesTo 无损替换间接图像，保留页面布局、其他对象及原加密配置
// 非预乘颜色和透明度保留八位或十六位精度；不修改源对象，不替换遮罩图像或签名文档
// 出错时应丢弃本次输出，Reader及图片须在调用期间保持可读
// 入参: ctx 取消上下文, writer 输出流, images 图像引用与替换像素
// 返回: OptimizeReport 写出结果, error 参数、编码或写入错误
func (r *Reader) ReplaceImagesTo(ctx context.Context, writer io.Writer, images map[Reference]image.Image) (OptimizeReport, error) {
	if r.closed {
		return OptimizeReport{}, os.ErrClosed
	}
	if err := ctx.Err(); err != nil {
		return OptimizeReport{}, err
	}
	if writer == nil {
		return OptimizeReport{}, fmt.Errorf("missing output writer")
	}
	maximum := int64(0)
	for number := range r.xref {
		maximum = max(maximum, number)
	}
	if maximum > math.MaxInt32-int64(len(images))-1024 {
		return OptimizeReport{}, fmt.Errorf("PDF object number exceeds output limit")
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
	replacements := make(map[Reference]Object, len(images)*2)
	for _, ref := range refs {
		if err := ctx.Err(); err != nil {
			return OptimizeReport{}, err
		}
		if ref.Number <= 0 || ref.Generation < 0 || ref.Generation > 65535 {
			return OptimizeReport{}, fmt.Errorf("invalid image reference")
		}
		original, err := r.ReadImage(ref)
		if err != nil {
			return OptimizeReport{}, err
		}
		if original.ImageMask {
			return OptimizeReport{}, fmt.Errorf("cannot replace a stencil image")
		}
		stream, mask, err := encodePDFImage(ctx, images[ref])
		if err != nil {
			return OptimizeReport{}, err
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
	return r.rewriteTo(ctx, writer, OptimizeOptions{}, replacements)
}

// encodePDFImage 逐行编码设备色像素和独立软遮罩，不分配整图样本缓冲
// 入参: ctx 取消上下文, img 像素图像
// 返回: *Stream 颜色流, *Stream 软遮罩或nil, error 编码错误
func encodePDFImage(ctx context.Context, img image.Image) (*Stream, *Stream, error) {
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
	opaque := false
	if image, ok := img.(interface{ Opaque() bool }); ok {
		opaque = image.Opaque()
	}
	if img.ColorModel() == color.RGBAModel && opaque {
		bits = 8
	}
	var pixels, alpha bytes.Buffer
	colorWriter := zlib.NewWriter(&pixels)
	defer colorWriter.Close()
	var alphaWriter *zlib.Writer
	if !opaque {
		alphaWriter = zlib.NewWriter(&alpha)
		defer alphaWriter.Close()
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
	}
	return stream, mask, nil
}
