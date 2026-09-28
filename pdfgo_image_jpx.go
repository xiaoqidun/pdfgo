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

	"github.com/mububoki/jpeg2000/j2k"
)

// jpxDefaults 读取JPEG2000码流位深及容器颜色声明，不推测缺失的颜色空间
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
	var space Object
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
			if name != "jp2h" {
				return nil
			}
			return walkJPXBoxes(payload, func(name string, payload []byte) error {
				if name != "colr" || space != nil || stream.Dictionary["ColorSpace"] != nil {
					return nil
				}
				if len(payload) < 3 {
					return fmt.Errorf("invalid JPEG2000 color specification")
				}
				switch payload[0] {
				case 1:
					if len(payload) < 7 {
						return fmt.Errorf("invalid JPEG2000 enumerated color space")
					}
					switch binary.BigEndian.Uint32(payload[3:]) {
					case 16, 18:
						space = Name("DeviceRGB")
					case 17:
						space = Name("DeviceGray")
					case 12:
						space = Name("DeviceCMYK")
					}
				case 2, 3:
					profile := payload[3:]
					if len(profile) < 128 {
						return fmt.Errorf("invalid JPEG2000 ICC profile")
					}
					components := map[string]Integer{"GRAY": 1, "RGB ": 3, "CMYK": 4}[string(profile[16:20])]
					if components == 0 {
						return &UnsupportedError{Feature: "JPEG2000 ICC color space"}
					}
					space = Array{Name("ICCBased"), &Stream{Dictionary: Dictionary{"N": components}, Data: profile, reader: r}}
				}
				return nil
			})
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
	return depth, space, nil
}

// jpxCMYKSamples 按PDF指定的四色空间解码原始分量，不将黑色分量解释为透明度
// 入参: data JPEG2000编码数据
// 返回: image.Image 四色采样图像, error 解码错误
func (i *Image) jpxCMYKSamples(data []byte) (image.Image, error) {
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
	if len(components) != 4 {
		return nil, fmt.Errorf("JPEG2000 components differ from CMYK color space")
	}
	depth := 8
	for _, c := range components {
		if c.Precision < 1 || c.Precision > 16 || c.W != i.Width || c.H != i.Height || c.XRsiz != 1 || c.YRsiz != 1 {
			return nil, &UnsupportedError{Feature: "JPEG2000 CMYK component precision or subsampling"}
		}
		if c.Precision != 8 {
			depth = 16
		}
	}
	data = make([]byte, i.Width*i.Height*4*(depth/8))
	for c, component := range components {
		maximum := uint32(1)<<component.Precision - 1
		for n, value := range component.Samples {
			sample := uint32(value + int32(1<<(component.Precision-1)))
			if depth == 8 {
				data[n*4+c] = uint8(sample)
			} else {
				binary.BigEndian.PutUint16(data[(n*4+c)*2:], uint16((sample*65535+maximum/2)/maximum))
			}
		}
	}
	if depth == 8 {
		return &image.CMYK{Pix: data, Stride: i.Width * 4, Rect: image.Rect(0, 0, i.Width, i.Height)}, nil
	}
	return &packedCMYKImage{data: data, rect: image.Rect(0, 0, i.Width, i.Height), stride: i.Width * 8, depth: 16}, nil
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
				case "pclr", "cmap":
					return &UnsupportedError{Feature: "JPEG2000 palette component mapping"}
				case "cdef":
					if len(payload) < 2 {
						return fmt.Errorf("invalid JPEG2000 channel definition")
					}
					n := int(binary.BigEndian.Uint16(payload))
					if len(payload) != 2+6*n {
						return fmt.Errorf("invalid JPEG2000 channel definition")
					}
					for j := 0; j < n; j++ {
						entry := payload[2+6*j:]
						channel, kind, association := binary.BigEndian.Uint16(entry), binary.BigEndian.Uint16(entry[2:]), binary.BigEndian.Uint16(entry[4:])
						if channel >= 4 || kind != 0 || association != channel+1 {
							return &UnsupportedError{Feature: "JPEG2000 channel association"}
						}
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
