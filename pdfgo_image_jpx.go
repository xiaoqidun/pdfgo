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
	"math"
	"sort"

	"github.com/mububoki/jpeg2000/j2k"
)

// jpxSample 保存JPEG2000颜色分量与独立透明度
type jpxSample struct {
	values cmykSample
	alpha  uint16
	cmyk   bool
}

// jpxMapping 保存容器输出通道到码流分量或调色板列的映射
type jpxMapping struct {
	component int
	precision int
	palette   []uint16
}

// jpxSampleImage 保留原始分量平面，按通道定义和采样间隔读取
type jpxSampleImage struct {
	planes   []j2k.Component
	channels []int
	mapping  []jpxMapping
	alpha    int
	bounds   image.Rectangle
	x0, y0   int
	cmyk     bool
	ycc      bool
	chroma   [2]float64
}

// jpxMaskMode 读取JPEG2000内嵌软遮罩模式
// 返回: int 遮罩模式, error 参数错误
func (i *Image) jpxMaskMode() (int, error) {
	filters, _, err := i.Stream.filterChain(i.reader)
	if err != nil {
		return 0, err
	}
	if len(filters) == 0 || filters[len(filters)-1] != Name("JPXDecode") {
		return 0, nil
	}
	value, err := i.reader.Resolve(i.Stream.Dictionary["SMaskInData"])
	if err != nil || value == nil {
		return 0, err
	}
	mode, ok := value.(Integer)
	if !ok || mode < 0 || mode > 2 {
		return 0, fmt.Errorf("invalid JPEG2000 SMaskInData")
	}
	if mode != 0 {
		mask, err := i.reader.Resolve(i.SoftMask)
		if err != nil {
			return 0, err
		}
		if mask != nil {
			return 0, fmt.Errorf("JPEG2000 SMaskInData conflicts with SMask")
		}
	}
	return int(mode), nil
}

// ColorModel 返回非预乘颜色模型
// 返回: color.Model 颜色模型
func (s *jpxSampleImage) ColorModel() color.Model { return color.NRGBA64Model }

// Bounds 返回JPEG2000图像边界
// 返回: image.Rectangle 图像边界
func (s *jpxSampleImage) Bounds() image.Rectangle { return s.bounds }

// Opaque 仅检查透明度通道，不执行逐像素颜色变换
// 返回: bool 是否完全不透明
func (s *jpxSampleImage) Opaque() bool {
	if s.alpha < 0 {
		return true
	}
	for y := s.bounds.Min.Y; y < s.bounds.Max.Y; y++ {
		for x := s.bounds.Min.X; x < s.bounds.Max.X; x++ {
			if s.sample(s.alpha, x, y) != 65535 {
				return false
			}
		}
	}
	return true
}

// At 读取映射后的颜色和透明度，不应用PDF色彩管理
// 入参: x 横向坐标, y 纵向坐标
// 返回: color.Color 样本颜色
func (s *jpxSampleImage) At(x, y int) color.Color {
	if !image.Pt(x, y).In(s.bounds) {
		return jpxSample{}
	}
	pixel := jpxSample{alpha: 65535, cmyk: s.cmyk}
	for c, index := range s.channels {
		pixel.values[c] = s.sample(index, x, y)
	}
	if len(s.channels) == 1 {
		pixel.values[1], pixel.values[2] = pixel.values[0], pixel.values[0]
	}
	if s.ycc {
		cb, cr := float64(pixel.values[1])-s.chroma[0], float64(pixel.values[2])-s.chroma[1]
		y := float64(pixel.values[0])
		pixel.values[0] = uint16(math.Round(math.Max(0, math.Min(65535, y+1.402*cr))))
		pixel.values[1] = uint16(math.Round(math.Max(0, math.Min(65535, y-.344136*cb-.714136*cr))))
		pixel.values[2] = uint16(math.Round(math.Max(0, math.Min(65535, y+1.772*cb))))
	}
	if s.alpha >= 0 {
		pixel.alpha = s.sample(s.alpha, x, y)
	}
	return pixel
}

// sample 将原始有符号域样本归一化为十六位分量
// 入参: index 平面索引, x 横向坐标, y 纵向坐标
// 返回: uint16 样本值
func (s *jpxSampleImage) sample(index, x, y int) uint16 {
	var palette []uint16
	if len(s.mapping) != 0 {
		palette = s.mapping[index].palette
		index = s.mapping[index].component
	}
	c := s.planes[index]
	sx := min(c.W-1, max(0, (x+s.x0)/c.XRsiz-(s.x0+c.XRsiz-1)/c.XRsiz))
	sy := min(c.H-1, max(0, (y+s.y0)/c.YRsiz-(s.y0+c.YRsiz-1)/c.YRsiz))
	value := uint64(int64(c.Samples[sy*c.W+sx]) + int64(1)<<(c.Precision-1))
	if palette != nil {
		return palette[min(value, uint64(len(palette)-1))]
	}
	maximum := uint64(1)<<c.Precision - 1
	return uint16((value*65535 + maximum/2) / maximum)
}

// precision 返回容器输出通道的有效位深
// 入参: index 输出通道
// 返回: int 分量位深
func (s *jpxSampleImage) precision(index int) int {
	if len(s.mapping) != 0 {
		return s.mapping[index].precision
	}
	return s.planes[index].Precision
}

// RGBA 返回预乘设备色预览，原始分量由图像解码流程解释
// 返回: uint32 红、绿、蓝及透明度
func (s jpxSample) RGBA() (uint32, uint32, uint32, uint32) {
	r, g, b := uint32(s.values[0]), uint32(s.values[1]), uint32(s.values[2])
	if s.cmyk {
		r, g, b, _ = s.values.RGBA()
	}
	a := uint32(s.alpha)
	return r * a / 65535, g * a / 65535, b * a / 65535, a
}

// jpxDefaults 读取JPEG2000码流位深及容器颜色声明，按PDF标准选择回退空间
// 入参: stream 图像流
// 返回: int 位深, Object 颜色空间, error 容器或颜色错误
func (r *Reader) jpxDefaults(stream *Stream) (int, Object, error) {
	filters, parameters, err := stream.filterChain(r)
	if err != nil || len(filters) == 0 || filters[len(filters)-1] != Name("JPXDecode") {
		return 0, nil, err
	}
	data, err := (&Stream{Dictionary: Dictionary{"Filter": filters[:len(filters)-1], "DecodeParms": parameters[:len(parameters)-1]}, Data: stream.Data}).Decode()
	if err != nil {
		return 0, nil, err
	}
	code := data
	if len(data) >= 12 && bytes.Equal(data[:12], []byte{0, 0, 0, 12, 'j', 'P', ' ', ' ', 13, 10, 135, 10}) {
		code = nil
		err = walkJPXBoxes(data, func(name string, payload []byte) error {
			if name == "jp2c" {
				if code != nil {
					return &UnsupportedError{Feature: "multiple JPEG2000 codestreams"}
				}
				code = payload
			}
			return nil
		})
		if err != nil {
			return 0, nil, err
		}
	}
	if len(code) < 42 || !bytes.Equal(code[:4], []byte{255, 79, 255, 81}) {
		return 0, nil, fmt.Errorf("invalid JPEG2000 size marker")
	}
	count := int(binary.BigEndian.Uint16(code[40:42]))
	if count == 0 || len(code) < 42+3*count || int(binary.BigEndian.Uint16(code[4:6])) != 38+3*count {
		return 0, nil, fmt.Errorf("invalid JPEG2000 component descriptors")
	}
	depth := int(code[42]&127) + 1
	for n := 0; n < count; n++ {
		bits := int(code[42+3*n]&127) + 1
		if bits > 16 {
			return 0, nil, &UnsupportedError{Feature: "JPEG2000 component precision"}
		}
		if bits != depth {
			depth = 16
		}
	}
	if depth != 1 && depth != 2 && depth != 4 && depth != 8 {
		depth = 16
	}
	space, _, err := r.jpxColorSpace(data, stream.Dictionary, count)
	return depth, space, err
}

// jpxColorSpace 按颜色声明优先级和近似程度选择空间，未支持时按通道数回退
// 入参: data 容器数据, dict 图像字典, count 码流通道数
// 返回: Object 颜色空间, bool 是否使用sYCC, error 容器错误
func (r *Reader) jpxColorSpace(data []byte, dict Dictionary, count int) (Object, bool, error) {
	space, err := r.Resolve(dict["ColorSpace"])
	if err != nil {
		return nil, false, err
	}
	if space != nil {
		return nil, false, nil
	}
	var specifications [][]byte
	var definitions []byte
	if len(data) >= 12 && bytes.Equal(data[:12], []byte{0, 0, 0, 12, 'j', 'P', ' ', ' ', 13, 10, 135, 10}) {
		err := walkJPXBoxes(data, func(name string, payload []byte) error {
			if name != "jp2h" {
				return nil
			}
			return walkJPXBoxes(payload, func(name string, payload []byte) error {
				switch name {
				case "colr":
					if len(payload) < 3 {
						return fmt.Errorf("invalid JPEG2000 color specification")
					}
					specifications = append(specifications, payload)
				case "cmap":
					if len(payload) == 0 || len(payload)%4 != 0 {
						return fmt.Errorf("invalid JPEG2000 component mapping")
					}
					count = len(payload) / 4
				case "cdef":
					if len(payload) < 2 || len(payload) != 2+6*int(binary.BigEndian.Uint16(payload)) {
						return fmt.Errorf("invalid JPEG2000 channel definition")
					}
					definitions = payload[2:]
				}
				return nil
			})
		})
		if err != nil {
			return nil, false, err
		}
	}
	sort.SliceStable(specifications, func(a, b int) bool {
		x, y := specifications[a], specifications[b]
		if x[1] != y[1] {
			return int8(x[1]) > int8(y[1])
		}
		return x[2] < y[2]
	})
	for _, specification := range specifications {
		switch specification[0] {
		case 1:
			if len(specification) < 7 {
				return nil, false, fmt.Errorf("invalid JPEG2000 enumerated color space")
			}
			code := binary.BigEndian.Uint32(specification[3:])
			switch code {
			case 16, 18:
				return Name("DeviceRGB"), code == 18, nil
			case 17:
				return Name("DeviceGray"), false, nil
			case 12:
				return Name("DeviceCMYK"), false, nil
			}
		case 2, 3:
			profile := specification[3:]
			if len(profile) < 128 {
				return nil, false, fmt.Errorf("invalid JPEG2000 ICC profile")
			}
			components := map[string]Integer{"GRAY": 1, "RGB ": 3, "CMYK": 4}[string(profile[16:20])]
			if components == 0 {
				continue
			}
			space := Array{Name("ICCBased"), &Stream{Dictionary: Dictionary{"N": components}, Data: profile, reader: r}}
			if _, err := r.readICCColorSpace(space); err == nil {
				return space, false, nil
			}
		}
	}
	if definitions != nil {
		opacity := make(map[uint16]bool)
		for entry := definitions; len(entry) != 0; entry = entry[6:] {
			kind := binary.BigEndian.Uint16(entry[2:])
			if kind == 1 || kind == 2 {
				opacity[binary.BigEndian.Uint16(entry)] = true
			}
		}
		count -= len(opacity)
	} else {
		value, err := r.Resolve(dict["SMaskInData"])
		if err != nil {
			return nil, false, err
		}
		if mode, ok := value.(Integer); ok && mode != 0 {
			count--
		}
	}
	fallback := map[int]Name{1: "DeviceGray", 3: "DeviceRGB", 4: "DeviceCMYK"}[count]
	if fallback == "" {
		return nil, false, &UnsupportedError{Feature: "JPEG2000 color channel count"}
	}
	return fallback, false, nil
}

// jpxSamples 按PDF颜色空间和容器通道定义解码分量，不混淆黑色与透明度
// 入参: data JPEG2000编码数据, cmyk 是否使用四色空间
// 返回: image.Image 采样图像, error 解码错误
func (i *Image) jpxSamples(data []byte, cmyk bool) (image.Image, error) {
	stream, err := jpxCodestream(data)
	if err != nil {
		return nil, err
	}
	config, err := j2k.DecodeConfig(bytes.NewReader(stream))
	if err != nil {
		return nil, err
	}
	if config.Width != i.Width || config.Height != i.Height {
		return nil, fmt.Errorf("JPEG2000 dimensions differ from image dictionary")
	}
	components, err := j2k.DecodeComponents(bytes.NewReader(stream), j2k.Options{})
	if err != nil {
		return nil, err
	}
	for _, c := range components {
		if c.Precision < 1 || c.Precision > 31 || c.W <= 0 || c.H <= 0 || c.XRsiz <= 0 || c.YRsiz <= 0 || len(c.Samples)/c.W != c.H {
			return nil, fmt.Errorf("invalid JPEG2000 component plane")
		}
	}
	if i.ImageMask && (len(components) != 1 || components[0].Precision != 1) {
		return nil, fmt.Errorf("JPEG2000 stencil requires one one-bit channel")
	}
	count := 3
	if cmyk {
		count = 4
	} else if i.ColorSpace == Name("DeviceGray") || i.ImageMask {
		count = 1
	} else if space, ok := i.ColorSpace.(Array); ok && len(space) > 0 {
		switch space[0] {
		case Name("CalGray"), Name("Indexed"), Name("Separation"):
			count = 1
		case Name("DeviceN"):
			definition, err := i.reader.readDeviceN(space)
			if err != nil {
				return nil, err
			}
			count = definition.components
		case Name("ICCBased"):
			if len(space) != 2 {
				return nil, fmt.Errorf("invalid ICC color space")
			}
			value, err := i.reader.Resolve(space[1])
			if err != nil {
				return nil, err
			}
			profile, ok := value.(*Stream)
			if !ok {
				return nil, fmt.Errorf("invalid ICC profile stream")
			}
			n, err := integerDefault(profile.Dictionary, "N", 0)
			if err != nil || n != 1 && n != 3 && n != 4 {
				return nil, fmt.Errorf("invalid JPEG2000 color component count")
			}
			count = int(n)
		}
	}
	mode, err := i.jpxMaskMode()
	if err != nil {
		return nil, err
	}
	channelData, planeCount := data, len(components)
	var mapping []jpxMapping
	indexed := false
	if space, ok := i.ColorSpace.(Array); ok && len(space) > 0 {
		indexed = space[0] == Name("Indexed")
	}
	if indexed && i.Stream.Dictionary["ColorSpace"] != nil {
		channelData = stream
	} else {
		mapping, err = jpxComponentMapping(data, components)
		if err != nil {
			return nil, err
		}
		if len(mapping) != 0 {
			planeCount = len(mapping)
		}
	}
	channels, alpha, err := jpxChannels(channelData, planeCount, count, mode != 0)
	if err != nil {
		return nil, err
	}
	_, ycc, err := i.reader.jpxColorSpace(data, i.Stream.Dictionary, len(components))
	if err != nil {
		return nil, err
	}
	out := &jpxSampleImage{planes: components, channels: channels, mapping: mapping, alpha: alpha, bounds: image.Rect(0, 0, i.Width, i.Height), x0: int(binary.BigEndian.Uint32(stream[14:18])), y0: int(binary.BigEndian.Uint32(stream[18:22])), cmyk: count == 4, ycc: ycc}
	if ycc {
		for n := range out.chroma {
			precision := out.precision(channels[n+1])
			out.chroma[n] = float64(uint64(1)<<(precision-1)) * 65535 / float64(uint64(1)<<precision-1)
		}
	}
	return out, nil
}

// jpxComponentMapping 解析JP2调色板和直接通道映射，保留原始采样精度
// 入参: data 容器数据, components 码流分量
// 返回: []jpxMapping 输出通道映射, error 容器错误
func jpxComponentMapping(data []byte, components []j2k.Component) ([]jpxMapping, error) {
	if len(data) >= 2 && data[0] == 255 && data[1] == 79 {
		return nil, nil
	}
	var palette, channelMap []byte
	err := walkJPXBoxes(data, func(name string, payload []byte) error {
		if name != "jp2h" {
			return nil
		}
		return walkJPXBoxes(payload, func(name string, payload []byte) error {
			switch name {
			case "pclr":
				if palette != nil {
					return fmt.Errorf("duplicate JPEG2000 palette")
				}
				palette = payload
			case "cmap":
				if channelMap != nil {
					return fmt.Errorf("duplicate JPEG2000 component mapping")
				}
				channelMap = payload
			}
			return nil
		})
	})
	if err != nil {
		return nil, err
	}
	if palette == nil && channelMap == nil {
		return nil, nil
	}
	if len(channelMap) == 0 || len(channelMap)%4 != 0 {
		return nil, fmt.Errorf("invalid JPEG2000 component mapping")
	}
	var columns [][]uint16
	var depths []byte
	if palette != nil {
		if len(palette) < 3 {
			return nil, fmt.Errorf("invalid JPEG2000 palette header")
		}
		entries, count := int(binary.BigEndian.Uint16(palette)), int(palette[2])
		if entries == 0 || count == 0 || len(palette) < 3+count {
			return nil, fmt.Errorf("invalid JPEG2000 palette dimensions")
		}
		depths = palette[3 : 3+count]
		stride := 0
		for _, depth := range depths {
			bits := int(depth&127) + 1
			if bits > 31 {
				return nil, &UnsupportedError{Feature: "JPEG2000 palette precision"}
			}
			stride += (bits + 7) / 8
		}
		values := palette[3+count:]
		if len(values) != entries*stride {
			return nil, fmt.Errorf("invalid JPEG2000 palette data length")
		}
		columns = make([][]uint16, count)
		for n := range columns {
			columns[n] = make([]uint16, entries)
		}
		for row := 0; row < entries; row++ {
			for n, depth := range depths {
				bits := uint(depth&127) + 1
				width := (bits + 7) / 8
				v := uint64(0)
				for _, b := range values[:width] {
					v = v<<8 | uint64(b)
				}
				maximum := uint64(1)<<bits - 1
				v &= maximum
				if depth&128 != 0 {
					v = (v + uint64(1)<<(bits-1)) & maximum
				}
				columns[n][row] = uint16((v*65535 + maximum/2) / maximum)
				values = values[width:]
			}
		}
	}
	mapping := make([]jpxMapping, len(channelMap)/4)
	for n := range mapping {
		entry := channelMap[4*n:]
		component, kind, column := int(binary.BigEndian.Uint16(entry)), entry[2], int(entry[3])
		if component >= len(components) || kind > 1 || kind == 0 && column != 0 || kind == 1 && column >= len(columns) {
			return nil, fmt.Errorf("invalid JPEG2000 mapped channel")
		}
		mapping[n] = jpxMapping{component: component, precision: components[component].Precision}
		if kind == 1 {
			mapping[n].palette = columns[column]
			mapping[n].precision = int(depths[column]&127) + 1
		}
	}
	return mapping, nil
}

// jpxChannels 解析颜色排序及全通道软遮罩，未指定软遮罩时忽略透明度
// 入参: data 容器数据, planes 分量数, colors 颜色数, masked 是否使用软遮罩
// 返回: []int 颜色平面, int 透明平面, error 通道错误
func jpxChannels(data []byte, planes, colors int, masked bool) ([]int, int, error) {
	channels := make([]int, colors)
	for c := range channels {
		channels[c] = c
	}
	alpha, defined, coverage := -1, false, 0
	if len(data) >= 2 && data[0] == 0xff && data[1] == 0x4f {
		if planes == colors+1 {
			alpha = colors
		}
	} else {
		err := walkJPXBoxes(data, func(name string, payload []byte) error {
			if name != "jp2h" {
				return nil
			}
			return walkJPXBoxes(payload, func(name string, payload []byte) error {
				if name != "cdef" {
					return nil
				}
				defined = true
				if len(payload) < 2 || len(payload) != 2+6*int(binary.BigEndian.Uint16(payload)) {
					return fmt.Errorf("invalid JPEG2000 channel definition")
				}
				for c := range channels {
					channels[c] = -1
				}
				for entry := payload[2:]; len(entry) > 0; entry = entry[6:] {
					channel, kind, association := int(binary.BigEndian.Uint16(entry)), binary.BigEndian.Uint16(entry[2:]), int(binary.BigEndian.Uint16(entry[4:]))
					if channel >= planes {
						return fmt.Errorf("invalid JPEG2000 channel index")
					}
					switch kind {
					case 0:
						if association < 1 || association > colors || channels[association-1] != -1 {
							return fmt.Errorf("invalid JPEG2000 color association")
						}
						channels[association-1] = channel
					case 1, 2:
						if masked && alpha != -1 && alpha != channel {
							return fmt.Errorf("JPEG2000 soft mask requires one opacity channel")
						}
						alpha = channel
						if association == 0 {
							coverage = 1<<colors - 1
						} else if association <= colors {
							coverage |= 1 << (association - 1)
						} else {
							return fmt.Errorf("invalid JPEG2000 opacity association")
						}
					case 65535:
					default:
						return fmt.Errorf("invalid JPEG2000 channel type")
					}
				}
				return nil
			})
		})
		if err != nil {
			return nil, -1, err
		}
	}
	if !defined && planes == colors+1 {
		alpha = colors
		coverage = 1<<colors - 1
	}
	for _, c := range channels {
		if c < 0 || c >= planes || masked && c == alpha {
			return nil, -1, fmt.Errorf("JPEG2000 color channels differ from color space")
		}
	}
	if masked && alpha < 0 {
		return nil, -1, fmt.Errorf("missing JPEG2000 opacity channel")
	}
	if masked && coverage != 1<<colors-1 {
		return nil, -1, fmt.Errorf("JPEG2000 soft mask must cover all color channels")
	}
	if !masked {
		alpha = -1
	}
	return channels, alpha, nil
}

// jpxCodestream 读取单码流容器，颜色空间由PDF字典确定
// 入参: data JPEG2000编码数据
// 返回: []byte 原始码流, error 容器错误
func jpxCodestream(data []byte) ([]byte, error) {
	if len(data) >= 2 && data[0] == 0xff && data[1] == 0x4f {
		return data, nil
	}
	if len(data) < 12 || !bytes.Equal(data[:12], []byte{0, 0, 0, 12, 'j', 'P', ' ', ' ', 13, 10, 135, 10}) {
		return nil, fmt.Errorf("invalid JPEG2000 container signature")
	}
	var stream []byte
	err := walkJPXBoxes(data, func(name string, payload []byte) error {
		switch name {
		case "jp2c":
			if stream != nil {
				return &UnsupportedError{Feature: "multiple JPEG2000 codestreams"}
			}
			stream = payload
		case "jp2h":
			return walkJPXBoxes(payload, func(name string, payload []byte) error {
				switch name {
				case "cdef":
					if len(payload) < 2 {
						return fmt.Errorf("invalid JPEG2000 channel definition")
					}
					n := int(binary.BigEndian.Uint16(payload))
					if len(payload) != 2+6*n {
						return fmt.Errorf("invalid JPEG2000 channel definition")
					}
				}
				return nil
			})
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if len(stream) == 0 {
		return nil, fmt.Errorf("missing JPEG2000 codestream")
	}
	return stream, nil
}

// walkJPXBoxes 访问单层JPEG2000数据盒，支持扩展长度和末尾数据盒
// 入参: data 数据盒序列, visit 数据盒访问函数
// 返回: error 边界或访问错误
func walkJPXBoxes(data []byte, visit func(string, []byte) error) error {
	for len(data) > 0 {
		if len(data) < 8 {
			return fmt.Errorf("truncated JPEG2000 box")
		}
		length, header := uint64(binary.BigEndian.Uint32(data)), uint64(8)
		if length == 1 {
			if len(data) < 16 {
				return fmt.Errorf("truncated JPEG2000 box length")
			}
			length, header = binary.BigEndian.Uint64(data[8:]), 16
		} else if length == 0 {
			length = uint64(len(data))
		}
		if length < header || length > uint64(len(data)) {
			return fmt.Errorf("invalid JPEG2000 box length")
		}
		if err := visit(string(data[4:8]), data[header:length]); err != nil {
			return err
		}
		data = data[length:]
	}
	return nil
}
