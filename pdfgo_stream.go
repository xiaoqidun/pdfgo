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

// contextInput 在分块读取之间检查取消
type contextInput struct {
	ctx    context.Context
	reader io.Reader
}

// Decode 解码通用流过滤器，图像专用过滤器由图像接口处理
// 阅读器返回的流按需解析参数引用，手工构造的流需提供直接参数
// 返回: []byte 解码数据, error 错误信息
func (s *Stream) Decode() ([]byte, error) {
	return s.DecodeContext(context.Background())
}

// DecodeContext 解码通用流过滤器，在读取及样本处理间检查取消
// 阅读器返回的流需与所属Reader串行调用，并保持数据源可读
// 入参: ctx 取消上下文
// 返回: []byte 独立解码数据, error 解码或取消错误
func (s *Stream) DecodeContext(ctx context.Context) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	filters, params, err := s.filterChain(s.reader)
	if err != nil {
		return nil, err
	}
	data := s.Data
	for i, filter := range filters {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
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
				data, err = io.ReadAll(&contextInput{ctx: ctx, reader: reader})
				closeErr := reader.Close()
				if err == nil {
					err = closeErr
				}
			}
		case "LZWDecode":
			var early int64
			early, err = integerDefault(dict, "EarlyChange", 1)
			if err == nil {
				data, _, err = decodeLZWBytesContext(ctx, data, early)
			}
		case "ASCIIHexDecode":
			data, err = decodeASCIIHexContext(ctx, data)
		case "ASCII85Decode":
			end := bytes.Index(data, []byte("~>"))
			if end < 0 {
				return nil, fmt.Errorf("missing ASCII85 terminator")
			}
			encoded, validateErr := validateASCII85(ctx, data[:end])
			if validateErr != nil {
				return nil, validateErr
			}
			data, err = io.ReadAll(&contextInput{ctx: ctx, reader: ascii85.NewDecoder(bytes.NewReader(encoded))})
		case "RunLengthDecode":
			data, err = decodeRunLength(ctx, data)
		default:
			return nil, &UnsupportedError{Feature: "stream filter " + string(name)}
		}
		if err != nil {
			return nil, err
		}
		if name == "FlateDecode" || name == "LZWDecode" {
			data, err = decodePredictorContext(ctx, data, dict)
			if err != nil {
				return nil, err
			}
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if len(filters) == 0 || len(filters) == 1 && filters[0] == Name("Crypt") {
		data = bytes.Clone(data)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return data, nil
}

func (*Stream) pdfObject() {}

// decodeASCIIHex 解码十六进制流，忽略空白并为末尾单个半字节补零
func decodeASCIIHex(data []byte) ([]byte, error) {
	return decodeASCIIHexContext(context.Background(), data)
}

// decodeASCIIHexContext 解码十六进制流，分段检查取消
// 入参: ctx 取消上下文, data 编码数据
// 返回: []byte 解码数据, error 编码或取消错误
func decodeASCIIHexContext(ctx context.Context, data []byte) ([]byte, error) {
	end := bytes.IndexByte(data, '>')
	if end < 0 {
		return nil, fmt.Errorf("missing ASCIIHex terminator")
	}
	data = data[:end]
	count := 0
	for n, b := range data {
		if n&4095 == 0 {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
		}
		if !isSpace(b) {
			count++
		}
	}
	out := make([]byte, count/2+count%2)
	index := 0
	for n, b := range data {
		if n&4095 == 0 {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
		}
		var value byte
		switch {
		case '0' <= b && b <= '9':
			value = b - '0'
		case 'a' <= b && b <= 'f':
			value = b - 'a' + 10
		case 'A' <= b && b <= 'F':
			value = b - 'A' + 10
		case isSpace(b):
			continue
		default:
			return nil, hex.InvalidByteError(b)
		}
		if index%2 == 0 {
			out[index/2] = value << 4
		} else {
			out[index/2] |= value
		}
		index++
	}
	return out, nil
}

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
	if filter == nil {
		return nil, nil, nil
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

// decodeLZW 解码高位优先的PDF变长字典编码，支持两种码宽增长规则
// 入参: data 压缩数据, early 提前增长标志
// 返回: []byte 解码数据, error 错误信息
func decodeLZW(data []byte, early int64) ([]byte, error) {
	out, _, err := decodeLZWBytes(data, early)
	return out, err
}

// decodeLZWBytes 解码LZW并返回结束码占用的字节数，供内联图像界定数据边界
// 入参: data 压缩数据, early 提前增长标志
// 返回: []byte 解码数据, int 已消费字节数, error 编码错误
func decodeLZWBytes(data []byte, early int64) ([]byte, int, error) {
	return decodeLZWBytesContext(context.Background(), data, early)
}

// decodeLZWBytesContext 解码LZW并返回消费长度，分段检查取消
// 入参: ctx 取消上下文, data 编码数据, early 提前增长标志
// 返回: []byte 解码数据, int 消费字节数, error 编码或取消错误
func decodeLZWBytesContext(ctx context.Context, data []byte, early int64) ([]byte, int, error) {
	if early != 0 && early != 1 {
		return nil, 0, fmt.Errorf("invalid LZW EarlyChange")
	}
	var prefixes [4096]uint16
	var suffixes, stack [4096]byte
	var out []byte
	var bits uint32
	available, position := uint(0), 0
	width, next, previous := uint(9), 258, -1
	for steps := 0; ; steps++ {
		if steps&255 == 0 {
			if err := ctx.Err(); err != nil {
				return nil, 0, err
			}
		}
		for available < width {
			if position == len(data) {
				return nil, 0, io.ErrUnexpectedEOF
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
			return out, position, nil
		}
		if code > next || code >= 4096 || previous < 0 && code >= 256 {
			return nil, 0, fmt.Errorf("invalid LZW code %d", code)
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

// validateASCII85 检查编码分组并移除空白，分段检查取消
// 入参: ctx 取消上下文, data 编码数据
// 返回: []byte 有效编码数据, error 编码或取消错误
func validateASCII85(ctx context.Context, data []byte) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	encoded := make([]byte, 0, len(data))
	var value uint64
	digits := 0
	for n, b := range data {
		if n&4095 == 0 {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
		}
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

// decodeRunLength 解码游程压缩，分段检查取消
// 入参: ctx 取消上下文, data 编码数据
// 返回: []byte 解码数据, error 编码或取消错误
func decodeRunLength(ctx context.Context, data []byte) ([]byte, error) {
	var out []byte
	checkpoint := 0
	for i := 0; i < len(data); {
		if i >= checkpoint {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			checkpoint = i + min(4096, len(data)-i)
		}
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
	return decodePredictorContext(context.Background(), data, params)
}

// decodePredictorContext 还原TIFF及PNG预测样本，逐行检查取消
// 入参: ctx 取消上下文, data 预测样本, params 预测参数
// 返回: []byte 还原数据, error 参数或取消错误
func decodePredictorContext(ctx context.Context, data []byte, params Dictionary) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
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
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			line := out[start : start+row]
			if bits == 8 {
				for x := int(colors); x < len(line); x++ {
					line[x] += line[x-int(colors)]
				}
				continue
			}
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
	var previous []byte
	for y := int64(0); y < rows; y++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		filter := data[y*(row+1)]
		if filter > 4 {
			return nil, fmt.Errorf("invalid PNG predictor")
		}
		line := out[y*row : (y+1)*row]
		copy(line, data[y*(row+1)+1:(y+1)*(row+1)])
		if y == 0 {
			switch filter {
			case 2:
				filter = 0
			case 3:
				for x := int(bpp); x < len(line); x++ {
					line[x] += line[x-int(bpp)] / 2
				}
				filter = 0
			case 4:
				filter = 1
			}
		}
		switch filter {
		case 1:
			for x := int(bpp); x < len(line); x++ {
				line[x] += line[x-int(bpp)]
			}
		case 2:
			for x := range line {
				line[x] += previous[x]
			}
		case 3:
			for x := range line {
				var left byte
				if x >= int(bpp) {
					left = line[x-int(bpp)]
				}
				line[x] += byte((int(left) + int(previous[x])) / 2)
			}
		case 4:
			for x := range line {
				var left, corner byte
				if x >= int(bpp) {
					left, corner = line[x-int(bpp)], previous[x-int(bpp)]
				}
				line[x] += paeth(left, previous[x], corner)
			}
		}
		previous = line
	}
	return out, nil
}

// Read 检查取消状态后读取数据
// 入参: p 数据缓冲区
// 返回: int 已读字节数, error 读取错误
func (r *contextInput) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.reader.Read(p)
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

// paeth 按相邻端点区间计算PNG预测值，保持等距离时的标准选择顺序
// 入参: a 左侧样本, b 上方样本, c 左上样本
// 返回: byte 预测值
func paeth(a, b, c byte) byte {
	left, right := min(int(a), int(b)), max(int(a), int(b))
	sum, triple := int(a)+int(b), 3*int(c)
	result := int(c)
	if triple <= sum+left {
		result = right
	}
	if triple >= sum+right {
		result = left
	}
	return byte(result)
}
