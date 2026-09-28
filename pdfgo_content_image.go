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
	"fmt"
	"io"
)

// inlineByteReader 限制解码器预读，避免读取内联图像后的内容操作符
type inlineByteReader struct {
	data []byte
	pos  int
}

// Read 每次提供一个编码字节，并记录已消费长度
// 入参: p 目标缓冲区
// 返回: int 字节数, error 读取错误
func (r *inlineByteReader) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	if r.pos == len(r.data) {
		return 0, io.EOF
	}
	p[0] = r.data[r.pos]
	r.pos++
	return 1, nil
}

// inlineImage 读取内联图像字典并按样本长度或过滤器结束标记定位数据
// 入参: resolve 当前作用域颜色空间解析器
// 返回: *Stream 展开缩写后的图像流, error 字典或数据边界错误
func (p *objectParser) inlineImage(resolve func(Object) (Object, error)) (*Stream, error) {
	dict := Dictionary{"Subtype": Name("Image")}
	keys := map[Name]Name{"BPC": "BitsPerComponent", "CS": "ColorSpace", "D": "Decode", "DP": "DecodeParms", "F": "Filter", "H": "Height", "IM": "ImageMask", "I": "Interpolate", "W": "Width"}
	for {
		p.skipSpace()
		if p.pos == len(p.data) {
			return nil, io.ErrUnexpectedEOF
		}
		if p.data[p.pos] != '/' {
			if p.token() != "ID" {
				return nil, p.fail("expected inline image data")
			}
			break
		}
		key, err := p.object()
		if err != nil {
			return nil, err
		}
		name := key.(Name)
		if full, ok := keys[name]; ok {
			name = full
		}
		value, err := p.object()
		if err != nil {
			return nil, err
		}
		dict[name] = value
	}
	if p.pos == len(p.data) || !isSpace(p.data[p.pos]) {
		return nil, p.fail("missing inline image data separator")
	}
	if p.data[p.pos] == '\r' && p.pos+1 < len(p.data) && p.data[p.pos+1] == '\n' {
		p.pos++
	}
	p.pos++
	aliases := map[Name]Name{"G": "DeviceGray", "RGB": "DeviceRGB", "CMYK": "DeviceCMYK", "I": "Indexed", "AHx": "ASCIIHexDecode", "A85": "ASCII85Decode", "LZW": "LZWDecode", "Fl": "FlateDecode", "RL": "RunLengthDecode", "CCF": "CCITTFaxDecode", "DCT": "DCTDecode"}
	expand := func(value Object) Object {
		if name, ok := value.(Name); ok {
			if full, ok := aliases[name]; ok {
				return full
			}
		}
		return value
	}
	for _, key := range []Name{"ColorSpace", "Filter"} {
		if array, ok := dict[key].(Array); ok {
			for i := range array {
				array[i] = expand(array[i])
			}
		} else {
			dict[key] = expand(dict[key])
		}
	}
	if resolve != nil && dict["ColorSpace"] != nil {
		value, err := resolve(dict["ColorSpace"])
		if err != nil {
			return nil, err
		}
		dict["ColorSpace"] = value
		if array, ok := value.(Array); ok && len(array) == 2 && array[0] == Name("ICCBased") {
			profile, err := resolve(array[1])
			if err != nil {
				return nil, err
			}
			dict["ColorSpace"] = Array{array[0], profile}
		}
	}
	stream := &Stream{Dictionary: dict}
	filters, params, err := stream.filterChain(nil)
	if err != nil {
		return nil, err
	}
	data := p.data[p.pos:]
	length := 0
	if len(filters) == 0 {
		length, err = inlineSampleLength(dict)
	} else if filters[0] == Name("CCITTFaxDecode") {
		length, err = inlineCCITTLength(data, dict, params[0])
	} else {
		length, err = inlineFilterLength(data, filters[0].(Name), params[0])
	}
	if err != nil {
		return nil, err
	}
	if length > len(data) {
		return nil, io.ErrUnexpectedEOF
	}
	stream.Data = data[:length]
	p.pos += length
	if p.pos == len(p.data) || !isSpace(p.data[p.pos]) {
		return nil, p.fail("missing inline image end separator")
	}
	p.skipSpace()
	if p.token() != "EI" {
		return nil, p.fail("missing inline image end")
	}
	return stream, nil
}

// inlineCCITTLength 按声明行数或块结束标记解码并确定内联数据边界
// 入参: data 内容流余下数据, dict 图像字典, params 解码参数
// 返回: int 编码字节数, error 图像或编码错误
func inlineCCITTLength(data []byte, dict Dictionary, params Object) (int, error) {
	length, err := inlineSampleLength(dict)
	if err != nil {
		return 0, err
	}
	depth := dict["BitsPerComponent"]
	if depth != Integer(1) && !(depth == nil && dict["ImageMask"] == Boolean(true)) {
		return 0, fmt.Errorf("invalid CCITT component depth")
	}
	source := &inlineByteReader{data: data}
	parameters, _ := params.(Dictionary)
	reader, endOfBlock, err := ccittImageReader(source, int(dict["Width"].(Integer)), int(dict["Height"].(Integer)), parameters)
	if err != nil {
		return 0, err
	}
	if _, err := io.CopyN(io.Discard, reader, int64(length)); err != nil {
		return 0, err
	}
	if endOfBlock {
		var extra [1]byte
		n, err := reader.Read(extra[:])
		if n != 0 {
			return 0, fmt.Errorf("CCITT dimensions differ from image dictionary")
		}
		if err != io.EOF {
			return 0, err
		}
	}
	return source.pos, nil
}

// inlineSampleLength 计算未压缩图像逐行字节对齐后的数据长度
// 入参: dict 内联图像字典
// 返回: int 字节数, error 无效尺寸、色空间或整数溢出
func inlineSampleLength(dict Dictionary) (int, error) {
	w, wok := dict["Width"].(Integer)
	h, hok := dict["Height"].(Integer)
	b, bok := dict["BitsPerComponent"].(Integer)
	components := 0
	switch dict["ColorSpace"] {
	case Name("DeviceGray"):
		components = 1
	case Name("DeviceRGB"):
		components = 3
	case Name("DeviceCMYK"):
		components = 4
	}
	if array, ok := dict["ColorSpace"].(Array); ok && len(array) > 0 {
		switch array[0] {
		case Name("Indexed"), Name("Separation"), Name("CalGray"):
			components = 1
		case Name("CalRGB"), Name("Lab"):
			components = 3
		case Name("ICCBased"):
			if len(array) == 2 {
				if s, ok := array[1].(*Stream); ok {
					if n, ok := s.Dictionary["N"].(Integer); ok && (n == 1 || n == 3 || n == 4) {
						components = int(n)
					}
				}
			}
		case Name("DeviceN"):
			if len(array) >= 2 {
				if names, ok := array[1].(Array); ok {
					components = len(names)
				}
			}
		}
	}
	if dict["ImageMask"] == Boolean(true) {
		components = 1
		if !bok {
			b, bok = 1, true
		}
	}
	if !wok || !hok || !bok || w <= 0 || h <= 0 || b != 1 && b != 2 && b != 4 && b != 8 && b != 16 {
		return 0, fmt.Errorf("invalid inline image dimensions or depth")
	}
	if components == 0 {
		return 0, &UnsupportedError{Feature: "inline image color space"}
	}
	limit := uint64(^uint(0) >> 1)
	if uint64(w) > (limit-7)/uint64(components)/uint64(b) {
		return 0, fmt.Errorf("inline image size overflow")
	}
	row := (uint64(w)*uint64(components)*uint64(b) + 7) / 8
	if uint64(h) > limit/row {
		return 0, fmt.Errorf("inline image size overflow")
	}
	return int(row * uint64(h)), nil
}

// inlineFilterLength 根据首层过滤器的结束标记定位压缩数据末尾
// 入参: data 内容流余下数据, filter 过滤器名称, params 解码参数
// 返回: int 压缩字节数, error 无效或未支持的编码
func inlineFilterLength(data []byte, filter Name, params Object) (int, error) {
	switch filter {
	case "FlateDecode":
		r := bytes.NewReader(data)
		z, err := zlib.NewReader(r)
		if err != nil {
			return 0, err
		}
		_, err = io.Copy(io.Discard, z)
		closeErr := z.Close()
		if err != nil {
			return 0, err
		}
		return len(data) - r.Len(), closeErr
	case "LZWDecode":
		early := int64(1)
		if d, ok := params.(Dictionary); ok && d["EarlyChange"] != nil {
			n, ok := d["EarlyChange"].(Integer)
			if !ok {
				return 0, fmt.Errorf("invalid LZW EarlyChange")
			}
			early = int64(n)
		}
		_, n, err := decodeLZWBytes(data, early)
		return n, err
	case "ASCIIHexDecode":
		if n := bytes.IndexByte(data, '>'); n >= 0 {
			return n + 1, nil
		}
		return 0, io.ErrUnexpectedEOF
	case "ASCII85Decode":
		if n := bytes.Index(data, []byte("~>")); n >= 0 {
			return n + 2, nil
		}
		return 0, io.ErrUnexpectedEOF
	case "RunLengthDecode":
		for n := 0; n < len(data); {
			b := int(data[n])
			n++
			if b == 128 {
				return n, nil
			}
			if b < 128 {
				n += b + 1
			} else {
				n++
			}
		}
		return 0, io.ErrUnexpectedEOF
	case "DCTDecode":
		return inlineJPEGLength(data)
	}
	return 0, &UnsupportedError{Feature: "inline image filter " + string(filter)}
}

// inlineJPEGLength 沿JPEG段长度与熵编码转义定位EOI，不搜索任意二进制结束串
// 入参: data JPEG及后续内容数据
// 返回: int JPEG字节数, error 无效段或缺失结束标记
func inlineJPEGLength(data []byte) (int, error) {
	if len(data) < 2 || data[0] != 0xff || data[1] != 0xd8 {
		return 0, fmt.Errorf("invalid inline JPEG header")
	}
	position := 2
	for position < len(data) {
		if data[position] != 0xff {
			return 0, fmt.Errorf("invalid inline JPEG marker")
		}
		for position < len(data) && data[position] == 0xff {
			position++
		}
		if position == len(data) {
			break
		}
		marker := data[position]
		position++
		if marker == 0xd9 {
			return position, nil
		}
		if marker == 0 || marker == 0xd8 || marker >= 0xd0 && marker <= 0xd7 {
			return 0, fmt.Errorf("invalid inline JPEG segment")
		}
		if marker == 1 {
			continue
		}
		if len(data)-position < 2 {
			break
		}
		length := int(data[position])<<8 | int(data[position+1])
		if length < 2 || length > len(data)-position {
			return 0, fmt.Errorf("invalid inline JPEG segment length")
		}
		position += length
		if marker != 0xda {
			continue
		}
		for position < len(data) {
			if data[position] != 0xff {
				position++
				continue
			}
			start := position
			for position < len(data) && data[position] == 0xff {
				position++
			}
			if position == len(data) {
				break
			}
			if data[position] == 0 || data[position] >= 0xd0 && data[position] <= 0xd7 {
				position++
				continue
			}
			position = start
			break
		}
	}
	return 0, io.ErrUnexpectedEOF
}
