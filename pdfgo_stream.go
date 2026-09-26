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
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"io"
)

// Stream 保存流字典及未经解码的数据
// 阅读器返回的流解码需与所属Reader串行调用，并保持数据源可读
type Stream struct {
	Dictionary Dictionary
	Data       []byte
	reader     *Reader
	decrypted  bool
}

func (*Stream) pdfObject() {}

// filterChain 解析过滤器及解码参数的间接引用，不修改源字典
// 入参: reader 关联阅读器，nil表示不解析间接引用
// 返回: Array 过滤器, Array 对应参数, error 错误信息
func (s *Stream) filterChain(reader *Reader) (Array, Array, error) {
	resolve := func(value Object) (Object, error) {
		if reader != nil {
			return reader.Resolve(value)
		}
		return value, nil
	}
	filter, err := resolve(s.Dictionary["Filter"])
	if err != nil {
		return nil, nil, err
	}
	parameters, err := resolve(s.Dictionary["DecodeParms"])
	if err != nil {
		return nil, nil, err
	}
	var filters Array
	switch v := filter.(type) {
	case nil:
	case Name:
		filters = Array{v}
	case Array:
		filters = append(Array(nil), v...)
	default:
		return nil, nil, fmt.Errorf("invalid stream filter")
	}
	params := make(Array, len(filters))
	switch v := parameters.(type) {
	case nil:
	case Dictionary:
		if len(filters) != 1 {
			return nil, nil, fmt.Errorf("invalid filter parameters")
		}
		params[0] = v
	case Array:
		if len(v) != len(filters) {
			return nil, nil, fmt.Errorf("filter parameter count mismatch")
		}
		copy(params, v)
	default:
		return nil, nil, fmt.Errorf("invalid filter parameters")
	}
	for i, filter := range filters {
		filters[i], err = resolve(filter)
		if err != nil {
			return nil, nil, err
		}
		if _, ok := filters[i].(Name); !ok {
			return nil, nil, fmt.Errorf("filter is not a name")
		}
		params[i], err = resolve(params[i])
		if err != nil {
			return nil, nil, err
		}
		if params[i] != nil {
			dict, ok := params[i].(Dictionary)
			if !ok {
				return nil, nil, fmt.Errorf("invalid decode parameters")
			}
			if reader != nil {
				resolved := make(Dictionary, len(dict))
				for key, value := range dict {
					resolved[key], err = resolve(value)
					if err != nil {
						return nil, nil, err
					}
				}
				params[i] = resolved
			}
		}
	}
	if s.decrypted && len(filters) > 0 && filters[0] == Name("Crypt") {
		return filters[1:], params[1:], nil
	}
	return filters, params, nil
}

// Decode 解码通用流过滤器，图像专用过滤器由图像接口处理
// 阅读器返回的流按需解析参数引用，手工构造的流需提供直接参数
// 返回: []byte 解码数据, error 错误信息
func (s *Stream) Decode() ([]byte, error) {
	filters, params, err := s.filterChain(s.reader)
	if err != nil {
		return nil, err
	}
	data := s.Data
	for i, filter := range filters {
		name := filter.(Name)
		dict, _ := params[i].(Dictionary)
		var err error
		switch name {
		case "Crypt":
			if i != 0 || dict["Name"] != nil && dict["Name"] != Name("Identity") {
				return nil, &UnsupportedError{Feature: "stream crypt filter"}
			}
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
		case "LZWDecode":
			var early int64
			early, err = integerDefault(dict, "EarlyChange", 1)
			if err == nil {
				data, err = decodeLZW(data, early)
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
		if name == "FlateDecode" || name == "LZWDecode" {
			data, err = decodePredictor(data, dict)
			if err != nil {
				return nil, err
			}
		}
	}
	if len(filters) == 0 {
		return bytes.Clone(data), nil
	}
	return data, nil
}

// decodeLZW 解码高位优先的PDF变长字典编码，支持两种码宽增长规则
// 入参: data 压缩数据, early 提前增长标志
// 返回: []byte 解码数据, error 错误信息
func decodeLZW(data []byte, early int64) ([]byte, error) {
	if early != 0 && early != 1 {
		return nil, fmt.Errorf("invalid LZW EarlyChange")
	}
	var prefixes [4096]uint16
	var suffixes, stack [4096]byte
	var out []byte
	var bits uint32
	available, position := uint(0), 0
	width, next, previous := uint(9), 258, -1
	for {
		for available < width {
			if position == len(data) {
				return nil, io.ErrUnexpectedEOF
			}
			bits = bits<<8 | uint32(data[position])
			available += 8
			position++
		}
		available -= width
		code := int(bits >> available & (1<<width - 1))
		if code == 256 {
			width, next, previous = 9, 258, -1
			continue
		}
		if code == 257 {
			return out, nil
		}
		if code > next || code >= 4096 || previous < 0 && code >= 256 {
			return nil, fmt.Errorf("invalid LZW code %d", code)
		}
		current, start := code, len(stack)
		if code == next {
			current = previous
		}
		for current >= 258 {
			start--
			stack[start] = suffixes[current]
			current = int(prefixes[current])
		}
		start--
		stack[start] = byte(current)
		out = append(out, stack[start:]...)
		if code == next {
			out = append(out, byte(current))
		}
		if previous >= 0 && next < 4096 {
			prefixes[next], suffixes[next] = uint16(previous), byte(current)
			next++
			if width < 12 && next+int(early) == 1<<width {
				width++
			}
		}
		previous = code
	}
}

// validateASCII85 检查编码分组溢出并移除PDF空白
// 入参: data 编码数据
// 返回: []byte 有效编码数据, error 错误信息
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
// 入参: data 压缩数据
// 返回: []byte 解码数据, error 错误信息
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
// 入参: dict 属性字典, key 属性名称, fallback 缺省值
// 返回: int64 属性值, error 错误信息
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
// 入参: data 预测后的数据, params 预测参数
// 返回: []byte 还原的样本数据, error 错误信息
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
	if columns <= 0 || colors <= 0 || (bits != 1 && bits != 2 && bits != 4 && bits != 8 && bits != 16) || uint64(columns) > uint64(1<<63-8)/uint64(colors)/uint64(bits) {
		return nil, fmt.Errorf("invalid predictor dimensions")
	}
	row := (columns*colors*bits + 7) / 8
	bpp := (colors*bits + 7) / 8
	if row > int64(len(data)) {
		return nil, io.ErrUnexpectedEOF
	}
	if predictor == 2 {
		if int64(len(data))%row != 0 {
			return nil, io.ErrUnexpectedEOF
		}
		out := bytes.Clone(data)
		for start := int64(0); start < int64(len(out)); start += row {
			line := out[start : start+row]
			for x := int(colors); x < int(columns*colors); x++ {
				value := packedSample(line, x, int(bits)) + packedSample(line, x-int(colors), int(bits))
				setPackedSample(line, x, int(bits), value)
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

// packedSample 读取按高位优先排列的图像分量
// 入参: data 行数据, index 分量索引, bits 分量位深
// 返回: uint16 分量值
func packedSample(data []byte, index, bits int) uint16 {
	if bits == 16 {
		return binary.BigEndian.Uint16(data[index*2:])
	}
	perByte := 8 / bits
	shift := uint(8 - bits - index%perByte*bits)
	return uint16(data[index/perByte]>>shift) & uint16((1<<bits)-1)
}

// setPackedSample 写入图像分量，保留同字节其他分量和行尾填充
// 入参: data 行数据, index 分量索引, bits 分量位深, value 分量值
func setPackedSample(data []byte, index, bits int, value uint16) {
	if bits == 16 {
		binary.BigEndian.PutUint16(data[index*2:], value)
		return
	}
	perByte := 8 / bits
	shift := uint(8 - bits - index%perByte*bits)
	mask := byte((1<<bits)-1) << shift
	data[index/perByte] = data[index/perByte]&^mask | byte(value<<shift)&mask
}

// paeth 计算PNG预测样本
// 入参: a 左侧样本, b 上方样本, c 左上样本
// 返回: byte 预测值
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
