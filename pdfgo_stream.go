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
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"io"
	"math/bits"
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
			data, err = decodeASCII85Context(ctx, data)
		case "RunLengthDecode":
			data, err = decodeRunLength(ctx, data)
		default:
			return nil, &UnsupportedError{Feature: "stream filter " + string(name)}
		}
		if err != nil {
			return nil, err
		}
		if name == "FlateDecode" || name == "LZWDecode" {
			data, err = restorePredictorContext(ctx, data, dict)
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

// pdfObject 将流标记为PDF对象
func (*Stream) pdfObject() {}

// decodeASCIIHexContext 按校验后的长度解码十六进制流，补齐末尾半字节
// 入参: ctx 取消上下文, data 编码数据
// 返回: []byte 解码数据, error 编码或取消错误
func decodeASCIIHexContext(ctx context.Context, data []byte) ([]byte, error) {
	end, size, err := asciiHexSize(ctx, data)
	if err != nil {
		return nil, err
	}
	out := make([]byte, size)
	index := 0
	for n, b := range data[:end] {
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
		default:
			continue
		}
		if index%2 == 0 {
			out[index/2] = value << 4
		} else {
			out[index/2] |= value
		}
		index++
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// asciiHexSize 校验十六进制字符和结束标记，计算解码长度
// 入参: ctx 取消上下文, data 编码数据
// 返回: int 结束标记位置, int 解码字节数, error 编码或取消错误
func asciiHexSize(ctx context.Context, data []byte) (int, int, error) {
	if err := ctx.Err(); err != nil {
		return 0, 0, err
	}
	digits := 0
	for n, b := range data {
		if n&4095 == 0 {
			if err := ctx.Err(); err != nil {
				return 0, 0, err
			}
		}
		switch {
		case '0' <= b && b <= '9', 'a' <= b && b <= 'f', 'A' <= b && b <= 'F':
			digits++
		case b == '>':
			return n, digits/2 + digits%2, ctx.Err()
		case isSpace(b):
		default:
			return 0, 0, hex.InvalidByteError(b)
		}
	}
	return 0, 0, fmt.Errorf("missing ASCIIHex terminator")
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

// decodeASCII85Context 按校验后的长度解码ASCII85，不复制编码数据
// 入参: ctx 取消上下文, data 带结束标记的编码数据
// 返回: []byte 独立解码数据, error 编码、溢出或取消错误
func decodeASCII85Context(ctx context.Context, data []byte) ([]byte, error) {
	end, size, err := ascii85Size(ctx, data)
	if err != nil {
		return nil, err
	}
	out := make([]byte, size)
	var value uint32
	digits, position := 0, 0
	for n, b := range data[:end] {
		if n&4095 == 0 {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
		}
		if '!' <= b && b <= 'u' {
			value = value*85 + uint32(b-'!')
			digits++
			if digits == 5 {
				binary.BigEndian.PutUint32(out[position:], value)
				position += 4
				value, digits = 0, 0
			}
		} else if b == 'z' {
			position += 4
		}
	}
	if digits > 0 {
		for n := digits; n < 5; n++ {
			value = value*85 + 84
		}
		for n := 0; n < digits-1; n++ {
			out[position+n] = byte(value >> uint(24-8*n))
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// ascii85Size 校验ASCII85分组和结束标记，计算解码长度
// 入参: ctx 取消上下文, data 编码数据
// 返回: int 结束标记位置, int 解码字节数, error 编码、溢出或取消错误
func ascii85Size(ctx context.Context, data []byte) (int, int, error) {
	if err := ctx.Err(); err != nil {
		return 0, 0, err
	}
	var value uint64
	digits, size := 0, 0
	for n, b := range data {
		if n&4095 == 0 {
			if err := ctx.Err(); err != nil {
				return 0, 0, err
			}
		}
		if isSpace(b) {
			continue
		}
		if b == '~' && n+1 < len(data) && data[n+1] == '>' {
			if digits == 1 {
				return 0, 0, fmt.Errorf("incomplete ASCII85 group")
			}
			if digits > 1 {
				for j := digits; j < 5; j++ {
					value = value*85 + 84
				}
				if value > 1<<32-1 {
					return 0, 0, fmt.Errorf("ASCII85 group overflow")
				}
				if digits-1 > int(^uint(0)>>1)-size {
					return 0, 0, fmt.Errorf("ASCII85 decoded length overflow")
				}
				size += digits - 1
			}
			return n, size, ctx.Err()
		}
		if b == 'z' {
			if digits != 0 {
				return 0, 0, fmt.Errorf("misplaced ASCII85 zero group")
			}
		} else {
			if b < '!' || b > 'u' {
				return 0, 0, fmt.Errorf("invalid ASCII85 character")
			}
			value = value*85 + uint64(b-'!')
			digits++
			if digits < 5 {
				continue
			}
			if value > 1<<32-1 {
				return 0, 0, fmt.Errorf("ASCII85 group overflow")
			}
			digits, value = 0, 0
		}
		if size > int(^uint(0)>>1)-4 {
			return 0, 0, fmt.Errorf("ASCII85 decoded length overflow")
		}
		size += 4
	}
	return 0, 0, fmt.Errorf("missing ASCII85 terminator")
}

// decodeRunLength 按校验后的长度一次分配解码缓冲，分段检查取消
// 入参: ctx 取消上下文, data 编码数据
// 返回: []byte 解码数据, error 编码或取消错误
func decodeRunLength(ctx context.Context, data []byte) ([]byte, error) {
	size, err := runLengthSize(ctx, data)
	if err != nil || size == 0 {
		return nil, err
	}
	out := make([]byte, size)
	position, checkpoint := 0, 0
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
			return out, ctx.Err()
		}
		count := n + 1
		if n < 128 {
			copy(out[position:], data[i:i+count])
			i += count
		} else {
			count = 257 - n
			for j := range count {
				out[position+j] = data[i]
			}
			i++
		}
		position += count
	}
	return nil, fmt.Errorf("missing RunLength terminator")
}

// runLengthSize 校验游程边界、结束标记及解码长度，不分配展开样本
// 入参: ctx 取消上下文, data 编码数据
// 返回: int 解码字节数, error 编码、溢出或取消错误
func runLengthSize(ctx context.Context, data []byte) (int, error) {
	size := 0
	checkpoint := 0
	for i := 0; i < len(data); {
		if i >= checkpoint {
			if err := ctx.Err(); err != nil {
				return 0, err
			}
			checkpoint = i + min(4096, len(data)-i)
		}
		n := int(data[i])
		i++
		if n == 128 {
			return size, ctx.Err()
		}
		count := n + 1
		if n > 128 {
			count = 257 - n
		}
		if n < 128 {
			if count > len(data)-i {
				return 0, io.ErrUnexpectedEOF
			}
			i += count
		} else {
			if i == len(data) {
				return 0, io.ErrUnexpectedEOF
			}
			i++
		}
		if count > int(^uint(0)>>1)-size {
			return 0, fmt.Errorf("RunLength decoded length overflow")
		}
		size += count
	}
	return 0, fmt.Errorf("missing RunLength terminator")
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

// restorePredictorContext 在独占缓冲中还原TIFF及PNG预测样本，原位移除PNG滤波字节
// 入参: ctx 取消上下文, data 本次解压所得的可写缓冲, params 预测参数
// 返回: []byte 引用原缓冲的还原数据, error 参数或取消错误
func restorePredictorContext(ctx context.Context, data []byte, params Dictionary) ([]byte, error) {
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
		out := data
		for start := int64(0); start < int64(len(out)); start += row {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			line := out[start : start+row]
			if bits == 8 {
				for x := int(colors); x < len(line); x++ {
					if x&4095 == 0 {
						if err := ctx.Err(); err != nil {
							return nil, err
						}
					}
					line[x] += line[x-int(colors)]
				}
				continue
			}
			for x := colors; x < columns*colors; x++ {
				if x&4095 == 0 {
					if err := ctx.Err(); err != nil {
						return nil, err
					}
				}
				value := packedSample(line, x, int(bits)) + packedSample(line, x-colors, int(bits))
				setPackedSample(line, x, int(bits), value)
			}
		}
		return out, ctx.Err()
	}
	if predictor < 10 || predictor > 15 {
		return nil, fmt.Errorf("invalid predictor")
	}
	if int64(len(data))%(row+1) != 0 {
		return nil, io.ErrUnexpectedEOF
	}
	rows := int64(len(data)) / (row + 1)
	out := data[: rows*row : rows*row]
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
					if x&4095 == 0 {
						if err := ctx.Err(); err != nil {
							return nil, err
						}
					}
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
				if x&4095 == 0 {
					if err := ctx.Err(); err != nil {
						return nil, err
					}
				}
				line[x] += line[x-int(bpp)]
			}
		case 2:
			for x := range line {
				if x&4095 == 0 {
					if err := ctx.Err(); err != nil {
						return nil, err
					}
				}
				line[x] += previous[x]
			}
		case 3:
			for x := range line {
				if x&4095 == 0 {
					if err := ctx.Err(); err != nil {
						return nil, err
					}
				}
				var left byte
				if x >= int(bpp) {
					left = line[x-int(bpp)]
				}
				line[x] += byte((int(left) + int(previous[x])) / 2)
			}
		case 4:
			for x := range line {
				if x&4095 == 0 {
					if err := ctx.Err(); err != nil {
						return nil, err
					}
				}
				var left, corner byte
				if x >= int(bpp) {
					left, corner = line[x-int(bpp)], previous[x-int(bpp)]
				}
				line[x] += paeth(left, previous[x], corner)
			}
		}
		previous = line
	}
	return out, ctx.Err()
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
// 入参: data 行数据, index 分量索引, depth 分量位深
// 返回: uint16 分量值
func packedSample(data []byte, index int64, depth int) uint16 {
	if depth == 16 {
		return binary.BigEndian.Uint16(data[index*2:])
	}
	byteShift := uint(4 - bits.Len8(uint8(depth)))
	shift := uint(8-depth) - uint(index&int64(1<<byteShift-1))*uint(depth)
	return uint16(data[index>>byteShift]>>shift) & uint16((1<<depth)-1)
}

// setPackedSample 写入图像分量，保留同字节其他分量和行尾填充
// 入参: data 行数据, index 分量索引, depth 分量位深, value 分量值
func setPackedSample(data []byte, index int64, depth int, value uint16) {
	if depth == 16 {
		binary.BigEndian.PutUint16(data[index*2:], value)
		return
	}
	byteShift := uint(4 - bits.Len8(uint8(depth)))
	shift := uint(8-depth) - uint(index&int64(1<<byteShift-1))*uint(depth)
	mask := byte((1<<depth)-1) << shift
	data[index>>byteShift] = data[index>>byteShift]&^mask | byte(value<<shift)&mask
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
