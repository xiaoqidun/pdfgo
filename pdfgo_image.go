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
	"math"

	_ "github.com/mububoki/jpeg2000/j2k"
	_ "github.com/mububoki/jpeg2000/jp2"
	"github.com/xiaoqidun/jbig2"
)

var jbig2FileHeader = []byte{0x97, 0x4a, 0x42, 0x32, 0x0d, 0x0a, 0x1a, 0x0a, 3}

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

// JBIG2File 无损封装可直接复用的单页JBIG2图像及全局段
// 不适合直接复用的图像返回nil，调用DecodeImage可得到已应用颜色与遮罩的像素
// 返回: []byte 完整JBIG2文件, error 解码参数或资源错误
func (i *Image) JBIG2File() ([]byte, error) {
	if i.ImageMask || i.BitsPerComponent != 1 || i.ColorSpace != Name("DeviceGray") || len(i.Decode) != 0 || i.Mask != nil || i.SoftMask != nil || i.Stream.Dictionary["SMaskInData"] != nil || i.Stream.Dictionary["Matte"] != nil {
		return nil, nil
	}
	filter, err := i.reader.Resolve(i.Stream.Dictionary["Filter"])
	if err != nil {
		return nil, err
	}
	if filter != Name("JBIG2Decode") {
		return nil, nil
	}
	var globals []byte
	if i.Stream.Dictionary["DecodeParms"] != nil {
		value, err := i.reader.Resolve(i.Stream.Dictionary["DecodeParms"])
		if err != nil {
			return nil, err
		}
		params, ok := value.(Dictionary)
		if !ok {
			return nil, fmt.Errorf("invalid JBIG2 decode parameters")
		}
		if params["JBIG2Globals"] != nil {
			value, err := i.reader.Resolve(params["JBIG2Globals"])
			if err != nil {
				return nil, err
			}
			stream, ok := value.(*Stream)
			if !ok {
				return nil, fmt.Errorf("invalid JBIG2 globals")
			}
			globals, err = stream.Decode()
			if err != nil {
				return nil, err
			}
		}
	}
	data := make([]byte, 0, len(jbig2FileHeader)+len(globals)+len(i.Stream.Data))
	data = append(data, jbig2FileHeader...)
	data = append(data, globals...)
	data = append(data, i.Stream.Data...)
	config, err := jbig2.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	if config.Width != i.Width || config.Height != i.Height {
		return nil, fmt.Errorf("JBIG2 dimensions differ from image dictionary")
	}
	return data, nil
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

// DecodeImage 应用Decode映射及颜色空间变换，合成同尺寸遮罩后返回非预乘透明图像
// 模板图像返回映射后的灰度样本，当前填充色由页面使用方应用
// 不支持的色彩管理和遮罩重采样返回明确错误
// 返回: image.Image 解码图像, error 错误信息
func (i *Image) DecodeImage() (image.Image, error) {
	palette, err := i.palette()
	if err != nil {
		return nil, err
	}
	components := 0
	var calibrated *calRGBSpace
	var iccGray *iccGraySpace
	var iccRGB *iccRGBSpace
	var separation *separationSpace
	switch i.ColorSpace {
	case Name("DeviceGray"):
		components = 1
	case Name("DeviceRGB"):
		components = 3
	case Name("DeviceCMYK"):
		components = 4
	}
	if space, ok := i.ColorSpace.(Array); ok && len(space) == 2 && space[0] == Name("CalRGB") {
		calibrated, err = i.reader.readCalRGB(space)
		if err != nil {
			return nil, err
		}
		components = 3
	}
	if space, ok := i.ColorSpace.(Array); ok && len(space) == 2 && space[0] == Name("ICCBased") {
		profile, err := i.reader.Resolve(space[1])
		if err != nil {
			return nil, err
		}
		stream, ok := profile.(*Stream)
		if !ok {
			return nil, fmt.Errorf("invalid ICC profile stream")
		}
		switch stream.Dictionary["N"] {
		case Integer(1):
			iccGray, err = i.reader.readICCGray(space)
			components = 1
		case Integer(3):
			iccRGB, err = i.reader.readICCRGB(space)
			components = 3
		default:
			return nil, &UnsupportedError{Feature: "image ICC component count"}
		}
		if err != nil {
			return nil, err
		}
	}
	if space, ok := i.ColorSpace.(Array); ok && len(space) == 4 && space[0] == Name("Separation") {
		separation, err = i.reader.readSeparation(space)
		if err != nil {
			return nil, err
		}
		components = 1
	}
	if i.ImageMask {
		components = 1
	}
	if palette != nil {
		components = 1
	}
	if components == 0 {
		return nil, &UnsupportedError{Feature: "image color space"}
	}
	if i.Stream.Dictionary["SMaskInData"] != nil {
		return nil, &UnsupportedError{Feature: "image field SMaskInData"}
	}
	if intent := i.Stream.Dictionary["Intent"]; intent != nil && intent != Name("Perceptual") && intent != Name("RelativeColorimetric") {
		return nil, &UnsupportedError{Feature: "image rendering intent"}
	}
	ranges := make([]float64, components*2)
	for c := 0; c < components; c++ {
		ranges[c*2+1] = 1
	}
	if palette != nil {
		ranges[1] = float64((uint32(1) << i.BitsPerComponent) - 1)
	}
	if len(i.Decode) != 0 {
		if len(i.Decode) != len(ranges) {
			return nil, fmt.Errorf("invalid image Decode array")
		}
		for n, value := range i.Decode {
			v, err := i.reader.number(value)
			if err != nil || math.IsNaN(v) || math.IsInf(v, 0) {
				return nil, fmt.Errorf("invalid image Decode value")
			}
			ranges[n] = v
		}
	}
	samples, err := i.DecodeSamples()
	if err != nil {
		return nil, err
	}
	mask, keys, inverted, matte, err := i.decodeMask(components)
	if err != nil {
		return nil, err
	}
	out := image.NewNRGBA64(samples.Bounds())
	intent, _ := i.Stream.Dictionary["Intent"].(Name)
	maximum := float64((uint32(1) << i.BitsPerComponent) - 1)
	for y := 0; y < i.Height; y++ {
		for x := 0; x < i.Width; x++ {
			alpha := uint16(65535)
			if mask != nil {
				value, _, _, _ := mask.At(x, y).RGBA()
				if inverted {
					value = 65535 - value
				}
				alpha = uint16(value)
			}
			r, g, b, _ := samples.At(x, y).RGBA()
			values := [4]float64{float64(r) / 65535, float64(g) / 65535, float64(b) / 65535}
			if components == 4 {
				cmyk, ok := samples.At(x, y).(color.CMYK)
				if !ok {
					return nil, fmt.Errorf("invalid CMYK image samples")
				}
				values = [4]float64{float64(cmyk.C) / 255, float64(cmyk.M) / 255, float64(cmyk.Y) / 255, float64(cmyk.K) / 255}
			}
			transparent := len(keys) != 0
			for c := 0; c < components; c++ {
				if transparent {
					sample := math.Round(values[c] * maximum)
					transparent = sample >= keys[c*2] && sample <= keys[c*2+1]
				}
				values[c] = ranges[c*2] + values[c]*(ranges[c*2+1]-ranges[c*2])
				if palette == nil {
					values[c] = math.Max(0, math.Min(1, values[c]))
				}
				if len(matte) != 0 {
					if alpha == 0 {
						values[c] = matte[c]
					} else {
						values[c] = math.Max(0, math.Min(1, matte[c]+(values[c]-matte[c])*65535/float64(alpha)))
					}
				}
			}
			if components == 1 {
				values[1], values[2] = values[0], values[0]
			}
			var pixel color.NRGBA64
			if palette != nil {
				index := int(math.Max(0, math.Min(float64(len(palette)-1), math.Round(values[0]))))
				pixel = palette[index]
			} else if separation != nil {
				paint, err := separation.paint(values[0])
				if err != nil {
					return nil, err
				}
				if paint.CMYK != nil {
					v := paint.CMYK
					pixel = color.NRGBA64Model.Convert(color.CMYK{C: uint8(math.Round(v[0] * 255)), M: uint8(math.Round(v[1] * 255)), Y: uint8(math.Round(v[2] * 255)), K: uint8(math.Round(v[3] * 255))}).(color.NRGBA64)
				} else {
					pixel = color.NRGBA64{R: uint16(math.Round(paint.RGB[0] * 65535)), G: uint16(math.Round(paint.RGB[1] * 65535)), B: uint16(math.Round(paint.RGB[2] * 65535)), A: 65535}
				}
				if separation.name == "None" {
					pixel.A = 0
				}
			} else if iccGray != nil {
				pixel = iccGray.color(values[0])
			} else if iccRGB != nil {
				converted, err := iccRGB.color(values[:3], intent)
				if err != nil {
					return nil, err
				}
				pixel = color.NRGBA64{R: uint16(math.Round(converted[0] * 65535)), G: uint16(math.Round(converted[1] * 65535)), B: uint16(math.Round(converted[2] * 65535)), A: 65535}
			} else if calibrated != nil {
				pixel = calibrated.color(values[0], values[1], values[2])
			} else if components == 4 {
				cmyk := color.CMYK{C: uint8(math.Round(values[0] * 255)), M: uint8(math.Round(values[1] * 255)), Y: uint8(math.Round(values[2] * 255)), K: uint8(math.Round(values[3] * 255))}
				pixel = color.NRGBA64Model.Convert(cmyk).(color.NRGBA64)
			} else {
				pixel = color.NRGBA64{R: uint16(math.Round(values[0] * 65535)), G: uint16(math.Round(values[1] * 65535)), B: uint16(math.Round(values[2] * 65535)), A: 65535}
			}
			if transparent {
				pixel.A = 0
			}
			if mask != nil {
				pixel.A = alpha
			}
			out.SetNRGBA64(x, y, pixel)
		}
	}
	return out, nil
}

// palette 解析索引色查找表，保留原始索引样本供Decode和色键遮罩使用
// 返回: []color.NRGBA64 调色板，非索引色时为空, error 错误信息
func (i *Image) palette() ([]color.NRGBA64, error) {
	array, ok := i.ColorSpace.(Array)
	if !ok {
		return nil, nil
	}
	if len(array) == 2 && (array[0] == Name("CalRGB") || array[0] == Name("ICCBased")) || len(array) == 4 && array[0] == Name("Separation") {
		return nil, nil
	}
	if len(array) != 4 || array[0] != Name("Indexed") {
		return nil, &UnsupportedError{Feature: "image color space"}
	}
	base, err := i.reader.Resolve(array[1])
	if err != nil {
		return nil, err
	}
	components := 0
	var calibrated *calRGBSpace
	switch base {
	case Name("DeviceGray"):
		components = 1
	case Name("DeviceRGB"):
		components = 3
	case Name("DeviceCMYK"):
		components = 4
	default:
		calibrated, err = i.reader.readCalRGB(base)
		if err != nil {
			return nil, err
		}
		components = 3
	}
	high, err := i.reader.Resolve(array[2])
	if err != nil {
		return nil, err
	}
	n, ok := high.(Integer)
	if !ok || n < 0 || n > 255 {
		return nil, fmt.Errorf("invalid Indexed high value")
	}
	lookup, err := i.reader.Resolve(array[3])
	if err != nil {
		return nil, err
	}
	var data []byte
	switch value := lookup.(type) {
	case String:
		data = value
	case *Stream:
		data, err = value.Decode()
		if err != nil {
			return nil, err
		}
	default:
		return nil, fmt.Errorf("invalid Indexed lookup")
	}
	if len(data) != int(n+1)*components {
		return nil, fmt.Errorf("invalid Indexed lookup length")
	}
	palette := make([]color.NRGBA64, int(n+1))
	for index := range palette {
		v := data[index*components:]
		switch components {
		case 1:
			palette[index] = color.NRGBA64{R: uint16(v[0]) * 257, G: uint16(v[0]) * 257, B: uint16(v[0]) * 257, A: 65535}
		case 3:
			palette[index] = color.NRGBA64{R: uint16(v[0]) * 257, G: uint16(v[1]) * 257, B: uint16(v[2]) * 257, A: 65535}
			if calibrated != nil {
				palette[index] = calibrated.color(float64(v[0])/255, float64(v[1])/255, float64(v[2])/255)
			}
		case 4:
			palette[index] = color.NRGBA64Model.Convert(color.CMYK{C: v[0], M: v[1], Y: v[2], K: v[3]}).(color.NRGBA64)
		}
	}
	return palette, nil
}

// decodeMask 读取有效遮罩，软遮罩优先于显式遮罩和色键遮罩
// 入参: components 原始图像分量数
// 返回: image.Image 遮罩图像, []float64 色键范围, bool 是否反转遮罩灰度, []float64 预混合底色, error 错误信息
func (i *Image) decodeMask(components int) (image.Image, []float64, bool, []float64, error) {
	object, err := i.reader.Resolve(i.SoftMask)
	if err != nil {
		return nil, nil, false, nil, err
	}
	soft := object != nil && object != Name("None")
	if !soft {
		object, err = i.reader.Resolve(i.Mask)
		if err != nil {
			return nil, nil, false, nil, err
		}
	}
	if object == nil {
		return nil, nil, false, nil, nil
	}
	if i.ImageMask {
		return nil, nil, false, nil, fmt.Errorf("stencil image cannot have a mask")
	}
	if keys, ok := object.(Array); ok && !soft {
		if len(keys) != components*2 {
			return nil, nil, false, nil, fmt.Errorf("invalid color key mask")
		}
		values := make([]float64, len(keys))
		maximum := (int64(1) << i.BitsPerComponent) - 1
		for n, key := range keys {
			value, err := i.reader.Resolve(key)
			if err != nil {
				return nil, nil, false, nil, err
			}
			v, ok := value.(Integer)
			if !ok || v < 0 || int64(v) > maximum || n%2 == 1 && float64(v) < values[n-1] {
				return nil, nil, false, nil, fmt.Errorf("invalid color key range")
			}
			values[n] = float64(v)
		}
		return nil, values, false, nil, nil
	}
	mask, err := i.reader.ReadImage(object)
	if err != nil {
		return nil, nil, false, nil, err
	}
	if mask.Mask != nil || mask.SoftMask != nil || soft && (mask.ImageMask || mask.ColorSpace != Name("DeviceGray")) || !soft && !mask.ImageMask {
		return nil, nil, false, nil, fmt.Errorf("invalid image mask dictionary")
	}
	if mask.Width != i.Width || mask.Height != i.Height {
		return nil, nil, false, nil, &UnsupportedError{Feature: "image mask resampling"}
	}
	var matte []float64
	if value := mask.Stream.Dictionary["Matte"]; value != nil {
		if !soft {
			return nil, nil, false, nil, fmt.Errorf("invalid image Matte array")
		}
		resolved, err := i.reader.Resolve(value)
		if err != nil {
			return nil, nil, false, nil, err
		}
		array, ok := resolved.(Array)
		if !ok || len(array) != components {
			return nil, nil, false, nil, fmt.Errorf("invalid image Matte array")
		}
		matte = make([]float64, len(array))
		for index, component := range array {
			matte[index], err = i.reader.number(component)
			if err != nil || math.IsNaN(matte[index]) || math.IsInf(matte[index], 0) {
				return nil, nil, false, nil, fmt.Errorf("invalid image Matte value")
			}
		}
	}
	decoded, err := mask.DecodeImage()
	return decoded, nil, !soft, matte, err
}

// DecodeSamples 解码原始图像样本，不应用Decode数组、遮罩或色彩管理
// 返回的灰度值用于样本解释，不代表图像已完成页面合成
// CMYK图像还原DCT分量，取消通用JPEG解码器的Adobe反相处理
// 返回: image.Image 样本图像, error 错误信息
func (i *Image) DecodeSamples() (image.Image, error) {
	filters, parameters, err := i.Stream.filterChain(i.reader)
	if err != nil {
		return nil, err
	}
	terminal := Name("")
	var terminalParams Dictionary
	if len(filters) > 0 {
		last := filters[len(filters)-1]
		if last == Name("DCTDecode") || last == Name("JBIG2Decode") || last == Name("JPXDecode") {
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
	case "JPXDecode":
		result, _, err = image.Decode(bytes.NewReader(data))
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

// rawSamples 将已解压的设备色彩样本或颜色索引展开为图像
// 入参: data 原始样本字节
// 返回: image.Image 样本图像, error 错误信息
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
	if space, ok := i.ColorSpace.(Array); ok && len(space) == 4 && space[0] == Name("Indexed") {
		components = 1
	}
	if space, ok := i.ColorSpace.(Array); ok && len(space) == 4 && space[0] == Name("Separation") {
		components = 1
	}
	if space, ok := i.ColorSpace.(Array); ok && len(space) == 2 && space[0] == Name("CalRGB") {
		components = 3
	}
	if space, ok := i.ColorSpace.(Array); ok && len(space) == 2 && space[0] == Name("ICCBased") {
		profile, err := i.reader.Resolve(space[1])
		if err != nil {
			return nil, err
		}
		stream, ok := profile.(*Stream)
		if !ok {
			return nil, fmt.Errorf("invalid ICC profile stream")
		}
		switch stream.Dictionary["N"] {
		case Integer(1), Integer(3):
			components = int(stream.Dictionary["N"].(Integer))
		default:
			return nil, &UnsupportedError{Feature: "image ICC component count"}
		}
	}
	if components == 0 {
		return nil, &UnsupportedError{Feature: "raw image color space"}
	}
	if i.BitsPerComponent != 8 && components == 4 {
		return nil, &UnsupportedError{Feature: "non-8-bit CMYK image samples"}
	}
	rowBits := uint64(i.Width) * uint64(components) * uint64(i.BitsPerComponent)
	rowBytes := (rowBits + 7) / 8
	expected := rowBytes * uint64(i.Height)
	if rowBytes > uint64(len(data))/uint64(i.Height) || expected > uint64(len(data)) {
		return nil, fmt.Errorf("image sample size mismatch")
	}
	if expected < uint64(len(data)) {
		for _, value := range data[expected:] {
			if value != 0 {
				return nil, fmt.Errorf("image sample size mismatch")
			}
		}
		data = data[:expected]
	}
	stride := int(rowBytes)
	if components == 4 {
		return &image.CMYK{Pix: bytes.Clone(data), Stride: stride, Rect: image.Rect(0, 0, i.Width, i.Height)}, nil
	}
	if components == 3 && i.BitsPerComponent == 8 {
		out := image.NewNRGBA(image.Rect(0, 0, i.Width, i.Height))
		for n := 0; n < i.Width*i.Height; n++ {
			copy(out.Pix[n*4:n*4+3], data[n*3:n*3+3])
			out.Pix[n*4+3] = 255
		}
		return out, nil
	}
	maximum := (uint32(1) << i.BitsPerComponent) - 1
	if components == 3 {
		out := image.NewNRGBA64(image.Rect(0, 0, i.Width, i.Height))
		for y := 0; y < i.Height; y++ {
			line := data[y*stride : (y+1)*stride]
			for x := 0; x < i.Width; x++ {
				var values [3]uint16
				for c := range values {
					values[c] = uint16(uint32(packedSample(line, x*3+c, i.BitsPerComponent)) * 65535 / maximum)
				}
				out.SetNRGBA64(x, y, color.NRGBA64{R: values[0], G: values[1], B: values[2], A: 65535})
			}
		}
		return out, nil
	}
	out := image.NewGray16(image.Rect(0, 0, i.Width, i.Height))
	for y := 0; y < i.Height; y++ {
		line := data[y*stride : (y+1)*stride]
		for x := 0; x < i.Width; x++ {
			sample := uint32(packedSample(line, x, i.BitsPerComponent))
			out.SetGray16(x, y, color.Gray16{Y: uint16(sample * 65535 / maximum)})
		}
	}
	return out, nil
}
