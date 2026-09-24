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
	"encoding/ascii85"
	"encoding/hex"
	"fmt"
	"io"
)

// Stream 保存流字典及未经解码的数据
type Stream struct {
	Dictionary Dictionary
	Data       []byte
}

func (*Stream) pdfObject() {}

// Decode 解码通用流过滤器，图像专用过滤器由图像接口处理
// 返回: []byte 解码数据, error 错误信息
func (s *Stream) Decode() ([]byte, error) {
	var filters Array
	switch v := s.Dictionary["Filter"].(type) {
	case nil:
	case Name:
		filters = Array{v}
	case Array:
		filters = v
	default:
		return nil, fmt.Errorf("invalid stream filter")
	}
	params := make(Array, len(filters))
	switch v := s.Dictionary["DecodeParms"].(type) {
	case nil:
	case Dictionary:
		if len(filters) != 1 {
			return nil, fmt.Errorf("invalid filter parameters")
		}
		params[0] = v
	case Array:
		if len(v) != len(filters) {
			return nil, fmt.Errorf("filter parameter count mismatch")
		}
		copy(params, v)
	default:
		return nil, fmt.Errorf("invalid filter parameters")
	}
	data := s.Data
	for i, filter := range filters {
		name, ok := filter.(Name)
		if !ok {
			return nil, fmt.Errorf("filter is not a name")
		}
		var err error
		switch name {
		case "FlateDecode":
			var reader io.ReadCloser
			reader, err = zlib.NewReader(bytes.NewReader(data))
			if err == nil {
				data, err = io.ReadAll(reader)
				closeErr := reader.Close()
				if err == nil {
					err = closeErr
				}
			}
		case "ASCIIHexDecode":
			end := bytes.IndexByte(data, '>')
			if end < 0 {
				return nil, fmt.Errorf("missing ASCIIHex terminator")
			}
			var digits []byte
			for _, b := range data[:end] {
				if !isSpace(b) {
					digits = append(digits, b)
				}
			}
			if len(digits)%2 != 0 {
				digits = append(digits, '0')
			}
			data = make([]byte, len(digits)/2)
			_, err = hex.Decode(data, digits)
		case "ASCII85Decode":
			end := bytes.Index(data, []byte("~>"))
			if end < 0 {
				return nil, fmt.Errorf("missing ASCII85 terminator")
			}
			encoded, validateErr := validateASCII85(data[:end])
			if validateErr != nil {
				return nil, validateErr
			}
			data, err = io.ReadAll(ascii85.NewDecoder(bytes.NewReader(encoded)))
		case "RunLengthDecode":
			data, err = decodeRunLength(data)
		default:
			return nil, &UnsupportedError{Feature: "stream filter " + string(name)}
		}
		if err != nil {
			return nil, err
		}
		if params[i] != nil {
			dict, ok := params[i].(Dictionary)
			if !ok {
				return nil, fmt.Errorf("invalid decode parameters")
			}
			if name == "FlateDecode" {
				data, err = decodePredictor(data, dict)
				if err != nil {
					return nil, err
				}
			}
		}
	}
	return bytes.Clone(data), nil
}

// validateASCII85 检查编码分组溢出并移除PDF空白
func validateASCII85(data []byte) ([]byte, error) {
	encoded := make([]byte, 0, len(data))
	var value uint64
	digits := 0
	for _, b := range data {
		if isSpace(b) {
			continue
		}
		encoded = append(encoded, b)
		if b == 'z' {
			if digits != 0 {
				return nil, fmt.Errorf("misplaced ASCII85 zero group")
			}
			continue
		}
		if b < '!' || b > 'u' {
			return nil, fmt.Errorf("invalid ASCII85 character")
		}
		value = value*85 + uint64(b-'!')
		digits++
		if digits == 5 {
			if value > 1<<32-1 {
				return nil, fmt.Errorf("ASCII85 group overflow")
			}
			digits, value = 0, 0
		}
	}
	if digits == 1 {
		return nil, fmt.Errorf("incomplete ASCII85 group")
	}
	if digits > 1 {
		for ; digits < 5; digits++ {
			value = value*85 + 84
		}
		if value > 1<<32-1 {
			return nil, fmt.Errorf("ASCII85 group overflow")
		}
	}
	return encoded, nil
}

// decodeRunLength 解码游程压缩数据
func decodeRunLength(data []byte) ([]byte, error) {
	var out []byte
	for i := 0; i < len(data); {
		n := int(data[i])
		i++
		if n == 128 {
			return out, nil
		}
		count := n + 1
		if n > 128 {
			count = 257 - n
		}
		if n < 128 {
			if count > len(data)-i {
				return nil, io.ErrUnexpectedEOF
			}
			out = append(out, data[i:i+count]...)
			i += count
		} else {
			if i == len(data) {
				return nil, io.ErrUnexpectedEOF
			}
			for range count {
				out = append(out, data[i])
			}
			i++
		}
	}
	return nil, fmt.Errorf("missing RunLength terminator")
}

// integerDefault 读取整数属性或使用缺省值
func integerDefault(dict Dictionary, key Name, fallback int64) (int64, error) {
	v := dict[key]
	if v == nil {
		return fallback, nil
	}
	n, ok := v.(Integer)
	if !ok {
		return 0, fmt.Errorf("%s is not an integer", key)
	}
	return int64(n), nil
}

// decodePredictor 还原TIFF或PNG预测后的样本字节
func decodePredictor(data []byte, params Dictionary) ([]byte, error) {
	predictor, err := integerDefault(params, "Predictor", 1)
	if err != nil {
		return nil, err
	}
	if predictor == 1 {
		return data, nil
	}
	columns, err := integerDefault(params, "Columns", 1)
	if err != nil {
		return nil, err
	}
	colors, err := integerDefault(params, "Colors", 1)
	if err != nil {
		return nil, err
	}
	bits, err := integerDefault(params, "BitsPerComponent", 8)
	if err != nil {
		return nil, err
	}
	if columns <= 0 || colors <= 0 || colors > 32 || columns > int64(len(data))*8 || (bits != 1 && bits != 2 && bits != 4 && bits != 8 && bits != 16) {
		return nil, fmt.Errorf("invalid predictor dimensions")
	}
	row := (columns*colors*bits + 7) / 8
	bpp := (colors*bits + 7) / 8
	if row > int64(len(data)) {
		return nil, io.ErrUnexpectedEOF
	}
	if predictor == 2 {
		if bits != 8 {
			return nil, &UnsupportedError{Feature: "TIFF predictor with non-8-bit components"}
		}
		if int64(len(data))%row != 0 {
			return nil, io.ErrUnexpectedEOF
		}
		out := bytes.Clone(data)
		for start := int64(0); start < int64(len(out)); start += row {
			for x := bpp; x < row; x++ {
				out[start+x] += out[start+x-bpp]
			}
		}
		return out, nil
	}
	if predictor < 10 || predictor > 15 {
		return nil, fmt.Errorf("invalid predictor")
	}
	if int64(len(data))%(row+1) != 0 {
		return nil, io.ErrUnexpectedEOF
	}
	rows := int64(len(data)) / (row + 1)
	out := make([]byte, rows*row)
	for y := int64(0); y < rows; y++ {
		filter := data[y*(row+1)]
		if filter > 4 {
			return nil, fmt.Errorf("invalid PNG predictor")
		}
		for x := int64(0); x < row; x++ {
			var left, up, corner byte
			if x >= bpp {
				left = out[y*row+x-bpp]
			}
			if y > 0 {
				up = out[(y-1)*row+x]
				if x >= bpp {
					corner = out[(y-1)*row+x-bpp]
				}
			}
			v := data[y*(row+1)+1+x]
			switch filter {
			case 1:
				v += left
			case 2:
				v += up
			case 3:
				v += byte((int(left) + int(up)) / 2)
			case 4:
				v += paeth(left, up, corner)
			}
			out[y*row+x] = v
		}
	}
	return out, nil
}

// paeth 计算PNG预测样本
func paeth(a, b, c byte) byte {
	p := int(a) + int(b) - int(c)
	abs := func(v int) int {
		if v < 0 {
			return -v
		}
		return v
	}
	pa, pb, pc := abs(p-int(a)), abs(p-int(b)), abs(p-int(c))
	if pa <= pb && pa <= pc {
		return a
	}
	if pb <= pc {
		return b
	}
	return c
}
