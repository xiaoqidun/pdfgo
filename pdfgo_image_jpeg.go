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
	"encoding/binary"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"io"
)

// JPEGFile 返回可脱离PDF字典直接显示的原始JPEG，需颜色映射或遮罩时返回空值
// 返回: []byte 原始JPEG数据或空值, error 解码参数错误
func (i *Image) JPEGFile() ([]byte, error) {
	filter, err := i.reader.Resolve(i.Stream.Dictionary["Filter"])
	if err != nil {
		return nil, err
	}
	if filter != Name("DCTDecode") || i.ImageMask || len(i.Decode) != 0 || i.Mask != nil || i.SoftMask != nil || i.ColorSpace != Name("DeviceGray") && i.ColorSpace != Name("DeviceRGB") {
		return nil, nil
	}
	value, err := i.reader.Resolve(i.Stream.Dictionary["DecodeParms"])
	if err != nil {
		return nil, err
	}
	params, ok := value.(Dictionary)
	if value != nil && !ok {
		return nil, fmt.Errorf("invalid JPEG decode parameters")
	}
	components, transform, _, err := i.jpegTransform(i.Stream.Data, params)
	if err != nil {
		return nil, err
	}
	config, err := jpeg.DecodeConfig(bytes.NewReader(i.Stream.Data))
	if err != nil {
		return nil, err
	}
	if config.Width != i.Width || config.Height != i.Height {
		return nil, fmt.Errorf("JPEG dimensions differ from image dictionary")
	}
	if components == 1 && i.ColorSpace == Name("DeviceGray") || components == 3 && i.ColorSpace == Name("DeviceRGB") && transform == (config.ColorModel == color.YCbCrModel) {
		return i.Stream.Data, nil
	}
	return nil, nil
}

// jpegTransform 解析PDF的DCT颜色规则，Adobe标记优先于解码字典
// 入参: data JPEG数据, params DCT参数
// 返回: int 分量数, bool 是否进行颜色变换, bool 是否有Adobe标记, error 参数错误
func (i *Image) jpegTransform(data []byte, params Dictionary) (int, bool, bool, error) {
	components, adobe, tagged, err := jpegColorInfo(data)
	if err != nil {
		return 0, false, false, err
	}
	transform := components == 3
	if tagged {
		if components == 3 && adobe != 0 && adobe != 1 || components == 4 && adobe != 0 && adobe != 2 {
			return 0, false, false, fmt.Errorf("invalid Adobe JPEG color transform")
		}
		transform = adobe != 0
	} else if (components == 3 || components == 4) && params["ColorTransform"] != nil {
		value, err := i.reader.Resolve(params["ColorTransform"])
		if err != nil {
			return 0, false, false, err
		}
		if value != Integer(0) && value != Integer(1) {
			return 0, false, false, fmt.Errorf("invalid JPEG ColorTransform")
		}
		transform = value == Integer(1)
	}
	return components, transform, tagged, nil
}

// jpegSamples 按PDF的DCT颜色规则解码，Adobe标记优先于解码字典
// 入参: data JPEG数据, params DCT解码参数
// 返回: image.Image 原始颜色样本, error 参数或解码错误
func (i *Image) jpegSamples(data []byte, params Dictionary) (image.Image, error) {
	components, transform, tagged, err := i.jpegTransform(data, params)
	if err != nil {
		return nil, err
	}
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
	config, err := jpeg.DecodeConfig(reader())
	if err != nil {
		return nil, err
	}
	if config.Width != i.Width || config.Height != i.Height {
		return nil, fmt.Errorf("JPEG dimensions differ from image dictionary")
	}
	result, err := jpeg.Decode(reader())
	if err != nil {
		return nil, err
	}
	if cmyk, ok := result.(*image.CMYK); ok {
		for n := range cmyk.Pix {
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
// 返回: int 分量数, byte Adobe变换值, bool 是否有Adobe标记, error 格式错误
func jpegColorInfo(data []byte) (int, byte, bool, error) {
	if len(data) < 2 || data[0] != 0xff || data[1] != 0xd8 {
		return 0, 0, false, fmt.Errorf("invalid JPEG start marker")
	}
	components, adobe, tagged, entropy := 0, byte(0), false, false
	for pos := 2; pos < len(data); {
		if data[pos] != 0xff {
			if !entropy {
				return 0, 0, false, fmt.Errorf("invalid JPEG marker")
			}
			n := bytes.IndexByte(data[pos:], 0xff)
			if n < 0 {
				return 0, 0, false, fmt.Errorf("truncated JPEG scan")
			}
			pos += n
		}
		for pos < len(data) && data[pos] == 0xff {
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
			return components, adobe, tagged, nil
		}
		if marker == 1 {
			continue
		}
		if marker == 0 || marker == 0xd8 || len(data)-pos < 2 {
			return 0, 0, false, fmt.Errorf("invalid JPEG marker")
		}
		size := int(binary.BigEndian.Uint16(data[pos:]))
		if size < 2 || size > len(data)-pos {
			return 0, 0, false, fmt.Errorf("invalid JPEG segment length")
		}
		segment := data[pos+2 : pos+size]
		switch marker {
		case 0xc0, 0xc1, 0xc2:
			if len(segment) < 6 {
				return 0, 0, false, fmt.Errorf("invalid JPEG frame header")
			}
			components = int(segment[5])
		case 0xee:
			if len(segment) >= 12 && string(segment[:5]) == "Adobe" {
				adobe, tagged = segment[11], true
			}
		case 0xda:
			entropy = true
		}
		pos += size
	}
	return 0, 0, false, fmt.Errorf("missing JPEG end marker")
}
