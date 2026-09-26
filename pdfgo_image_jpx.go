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
	for _, c := range components {
		if c.Precision != 8 || c.W != i.Width || c.H != i.Height || c.XRsiz != 1 || c.YRsiz != 1 {
			return nil, &UnsupportedError{Feature: "JPEG2000 CMYK component precision or subsampling"}
		}
	}
	out := image.NewCMYK(image.Rect(0, 0, i.Width, i.Height))
	for c, component := range components {
		for n, value := range component.Samples {
			out.Pix[n*4+c] = uint8(value + 128)
		}
	}
	return out, nil
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
