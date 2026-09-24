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
	"fmt"
	"image"
	"image/color"
	"image/jpeg"

	"github.com/xiaoqidun/jbig2"
)

// Image 保存图像原始属性，颜色空间和遮罩保持为PDF对象
type Image struct {
	Width            int
	Height           int
	BitsPerComponent int
	ColorSpace       Object
	Decode           Array
	ImageMask        bool
	Mask             Object
	SoftMask         Object
	Stream           *Stream
	reader           *Reader
}

// ReadImage 读取图像XObject描述，不隐式转换颜色或丢弃遮罩
// 入参: object 图像对象或引用
// 返回: *Image 图像描述, error 错误信息
func (r *Reader) ReadImage(object Object) (*Image, error) {
	resolved, err := r.Resolve(object)
	if err != nil {
		return nil, err
	}
	stream, ok := resolved.(*Stream)
	if !ok || stream.Dictionary["Subtype"] != Name("Image") {
		return nil, fmt.Errorf("expected image XObject")
	}
	dict := stream.Dictionary
	readInteger := func(key Name) (int64, error) {
		value, err := r.Resolve(dict[key])
		if err != nil {
			return 0, err
		}
		n, ok := value.(Integer)
		if !ok {
			return 0, fmt.Errorf("invalid image %s", key)
		}
		return int64(n), nil
	}
	w, err := readInteger("Width")
	if err != nil {
		return nil, err
	}
	h, err := readInteger("Height")
	if err != nil {
		return nil, err
	}
	if w <= 0 || h <= 0 || uint64(w) > uint64(^uint(0)>>1)/8/uint64(h) {
		return nil, fmt.Errorf("invalid image dimensions or platform integer overflow")
	}
	maskObject, err := r.Resolve(dict["ImageMask"])
	if err != nil {
		return nil, err
	}
	mask := false
	if maskObject != nil {
		b, ok := maskObject.(Boolean)
		if !ok {
			return nil, fmt.Errorf("invalid image mask flag")
		}
		mask = bool(b)
	}
	bits := int64(1)
	if !mask || dict["BitsPerComponent"] != nil {
		bits, err = readInteger("BitsPerComponent")
		if err != nil {
			return nil, err
		}
	}
	if bits != 1 && bits != 2 && bits != 4 && bits != 8 && bits != 16 {
		return nil, fmt.Errorf("invalid image component depth")
	}
	if mask && bits != 1 {
		return nil, fmt.Errorf("invalid stencil component depth")
	}
	space, err := r.Resolve(dict["ColorSpace"])
	if err != nil {
		return nil, err
	}
	decodeObject, err := r.Resolve(dict["Decode"])
	if err != nil {
		return nil, err
	}
	decode, ok := decodeObject.(Array)
	if decodeObject != nil && !ok {
		return nil, fmt.Errorf("invalid image decode array")
	}
	return &Image{Width: int(w), Height: int(h), BitsPerComponent: int(bits), ColorSpace: space, Decode: decode, ImageMask: mask, Mask: dict["Mask"], SoftMask: dict["SMask"], Stream: stream, reader: r}, nil
}

// DecodeSamples 解码原始图像样本，不应用Decode数组、遮罩或色彩管理
// 返回的灰度值用于样本解释，不代表图像已完成页面合成
// CMYK JPEG还原DCT分量，取消通用JPEG解码器的Adobe反相处理
// 返回: image.Image 样本图像, error 错误信息
func (i *Image) DecodeSamples() (image.Image, error) {
	dict := i.Stream.Dictionary
	filter, err := i.reader.Resolve(dict["Filter"])
	if err != nil {
		return nil, err
	}
	params, err := i.reader.Resolve(dict["DecodeParms"])
	if err != nil {
		return nil, err
	}
	filters := Array{}
	if filter != nil {
		if a, ok := filter.(Array); ok {
			filters = a
		} else {
			filters = Array{filter}
		}
	}
	parameters := make(Array, len(filters))
	if params != nil {
		if a, ok := params.(Array); ok {
			if len(a) != len(filters) {
				return nil, fmt.Errorf("image filter parameter count mismatch")
			}
			copy(parameters, a)
		} else {
			if len(filters) != 1 {
				return nil, fmt.Errorf("invalid image filter parameters")
			}
			parameters[0] = params
		}
	}
	for n := range filters {
		filters[n], err = i.reader.Resolve(filters[n])
		if err != nil {
			return nil, err
		}
		parameters[n], err = i.reader.Resolve(parameters[n])
		if err != nil {
			return nil, err
		}
	}
	terminal := Name("")
	var terminalParams Dictionary
	if len(filters) > 0 {
		last := filters[len(filters)-1]
		if last == Name("DCTDecode") || last == Name("JBIG2Decode") {
			terminal = last.(Name)
			if parameters[len(filters)-1] != nil {
				var ok bool
				terminalParams, ok = parameters[len(filters)-1].(Dictionary)
				if !ok {
					return nil, fmt.Errorf("invalid image decode parameters")
				}
			}
			filters = filters[:len(filters)-1]
			parameters = parameters[:len(parameters)-1]
		}
	}
	data, err := (&Stream{Dictionary: Dictionary{"Filter": filters, "DecodeParms": parameters}, Data: i.Stream.Data}).Decode()
	if err != nil {
		return nil, err
	}
	var result image.Image
	switch terminal {
	case "DCTDecode":
		if terminalParams["ColorTransform"] != nil {
			return nil, &UnsupportedError{Feature: "explicit JPEG ColorTransform"}
		}
		config, err := jpeg.DecodeConfig(bytes.NewReader(data))
		if err != nil {
			return nil, err
		}
		if config.Width != i.Width || config.Height != i.Height {
			return nil, fmt.Errorf("JPEG dimensions differ from image dictionary")
		}
		result, err = jpeg.Decode(bytes.NewReader(data))
		if err != nil {
			return nil, err
		}
		if cmyk, ok := result.(*image.CMYK); ok {
			for n := range cmyk.Pix {
				cmyk.Pix[n] = 255 - cmyk.Pix[n]
			}
		}
	case "JBIG2Decode":
		if i.BitsPerComponent != 1 {
			return nil, fmt.Errorf("invalid JBIG2 component depth")
		}
		var globals []byte
		if terminalParams["JBIG2Globals"] != nil {
			object, err := i.reader.Resolve(terminalParams["JBIG2Globals"])
			if err != nil {
				return nil, err
			}
			stream, ok := object.(*Stream)
			if !ok {
				return nil, fmt.Errorf("invalid JBIG2 globals")
			}
			globals, err = stream.Decode()
			if err != nil {
				return nil, err
			}
		}
		decoder, err := jbig2.NewDecoderWithGlobals(bytes.NewReader(data), globals)
		if err != nil {
			return nil, err
		}
		result, err = decoder.Decode()
		if err != nil {
			return nil, err
		}
	default:
		return i.rawSamples(data)
	}
	if result.Bounds().Dx() != i.Width || result.Bounds().Dy() != i.Height {
		return nil, fmt.Errorf("decoded image dimensions differ from dictionary")
	}
	return result, nil
}

// rawSamples 将已解压的设备色彩样本展开为图像
func (i *Image) rawSamples(data []byte) (image.Image, error) {
	components := 0
	switch i.ColorSpace {
	case Name("DeviceGray"):
		components = 1
	case Name("DeviceRGB"):
		components = 3
	case Name("DeviceCMYK"):
		components = 4
	}
	if i.ImageMask {
		components = 1
	}
	if components == 0 {
		return nil, &UnsupportedError{Feature: "raw image color space"}
	}
	if i.BitsPerComponent != 8 && components != 1 {
		return nil, &UnsupportedError{Feature: "non-8-bit multicomponent image samples"}
	}
	stride := (i.Width*components*i.BitsPerComponent + 7) / 8
	if len(data) != stride*i.Height {
		return nil, fmt.Errorf("image sample size mismatch")
	}
	if components == 4 {
		return &image.CMYK{Pix: bytes.Clone(data), Stride: stride, Rect: image.Rect(0, 0, i.Width, i.Height)}, nil
	}
	if components == 3 {
		out := image.NewNRGBA(image.Rect(0, 0, i.Width, i.Height))
		for n := 0; n < i.Width*i.Height; n++ {
			copy(out.Pix[n*4:n*4+3], data[n*3:n*3+3])
			out.Pix[n*4+3] = 255
		}
		return out, nil
	}
	out := image.NewGray16(image.Rect(0, 0, i.Width, i.Height))
	maximum := (uint32(1) << i.BitsPerComponent) - 1
	for y := 0; y < i.Height; y++ {
		for x := 0; x < i.Width; x++ {
			bit := x * i.BitsPerComponent
			var sample uint32
			for b := 0; b < i.BitsPerComponent; b++ {
				sample = sample<<1 | uint32((data[y*stride+(bit+b)/8]>>uint(7-(bit+b)%8))&1)
			}
			out.SetGray16(x, y, color.Gray16{Y: uint16(sample * 65535 / maximum)})
		}
	}
	return out, nil
}
