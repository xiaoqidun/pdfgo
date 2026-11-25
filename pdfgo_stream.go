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
	"compress/flate"
	"compress/zlib"
	"context"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"hash/adler32"
	"io"
	"math"
	"math/bits"
	"os"
	"strings"
	"sync"

	"github.com/andybalholm/brotli"
)

// defaultMaxStreamBytes 限制单个流默认最大解压字节数，防止压缩炸弹耗尽内存
const defaultMaxStreamBytes int64 = 256 << 20

// defaultMaxFilterChain 限制单个流默认最大过滤器链深度
const defaultMaxFilterChain = 16

// normalizeMaxStreamBytes 规范化流解压字节上限，零值取默认值，负值表示不限制
// 入参: limit 配置的字节上限
// 返回: int64 规范化上限
func normalizeMaxStreamBytes(limit int64) int64 {
	if limit < 0 {
		return -1
	}
	if limit == 0 {
		return defaultMaxStreamBytes
	}
	return limit
}

// normalizeMaxFilterChain 规范化过滤器链深度上限，零值取默认值，负值表示不限制
// 入参: limit 配置的链深度上限
// 返回: int 规范化上限
func normalizeMaxFilterChain(limit int) int {
	if limit < 0 {
		return -1
	}
	if limit == 0 {
		return defaultMaxFilterChain
	}
	return limit
}

// effectiveStreamLimits 计算生效的流解压字节上限与过滤器链深度上限，零值表示不限制
// 入参: reader 关联阅读器, stream 目标数据流
// 返回: int64 生效字节上限, int 生效过滤器链深度上限
func effectiveStreamLimits(reader *Reader, stream *Stream) (int64, int) {
	var maxBytes int64
	var maxChain int
	if reader != nil {
		maxBytes = reader.maxStreamBytes
		maxChain = reader.maxFilterChain
	} else if stream != nil {
		maxBytes = stream.maxStreamBytes
		maxChain = stream.maxFilterChain
	}
	if maxBytes == 0 {
		maxBytes = defaultMaxStreamBytes
	} else if maxBytes < 0 {
		maxBytes = 0
	}
	if maxChain == 0 {
		maxChain = defaultMaxFilterChain
	} else if maxChain < 0 {
		maxChain = 0
	}
	return maxBytes, maxChain
}

// streamFilterAliases 保存内联图像的标准过滤器缩写
var streamFilterAliases = map[Name]Name{
	"AHx": "ASCIIHexDecode",
	"A85": "ASCII85Decode",
	"LZW": "LZWDecode",
	"Fl":  "FlateDecode",
	"RL":  "RunLengthDecode",
	"CCF": "CCITTFaxDecode",
	"DCT": "DCTDecode",
}

// flateReaderPool 复用zlib解码器，降低高频Flate流解压的堆分配
var flateReaderPool sync.Pool

// brotliReaderPool 复用Brotli解码器，降低高频Brotli流解压的堆分配
var brotliReaderPool sync.Pool

// emptyZlibStream 保存空数据的合法zlib流，用于在归还解码器时解除原输入引用
var emptyZlibStream = []byte{0x78, 0x9c, 0x03, 0x00, 0x00, 0x00, 0x00, 0x01}

// filterRefEntry 保存过滤器间接引用的只读解析结果或原始解析错误
type filterRefEntry struct {
	value Object
	err   error
}

// Stream 保存流字典、未经解码的数据及间接对象引用
// 多工作视图并发场景应通过Reader.DecodeStreamContext解码共享流
type Stream struct {
	Dictionary     Dictionary
	Data           []byte
	Reference      Reference
	reader         *Reader
	cache          *objectCache
	filterRefs     map[Reference]filterRefEntry
	maxStreamBytes int64
	maxFilterChain int
	hasWarning     bool
	decrypted      bool
	compressed     bool
}

// contextInput 在分块读取之间检查取消
type contextInput struct {
	ctx    context.Context
	reader io.Reader
}

// limitStreamReader 在有效字节上限小于最大有符号整数时包装超限探测读取器，避免加一溢出
// 入参: input 原始读取器, maxBytes 解码字节上限，非正数表示不限制
// 返回: io.Reader 受限或原始读取器
func limitStreamReader(input io.Reader, maxBytes int64) io.Reader {
	if maxBytes > 0 && maxBytes < math.MaxInt64 {
		return io.LimitReader(input, maxBytes+1)
	}
	return input
}

// completeFlatePayload 验证完整压缩块及截断校验尾部，不恢复截断压缩数据
// 入参: ctx 取消上下文, encoded 压缩数据, decodedLen 已解码字节长度
// 返回: bool 是否仅存在校验尾部问题
func completeFlatePayload(ctx context.Context, encoded []byte, decodedLen int64) bool {
	if len(encoded) < 2 || encoded[1]&0x20 != 0 || decodedLen < 0 || decodedLen == math.MaxInt64 {
		return false
	}
	source := bytes.NewReader(encoded[2:])
	reader := flate.NewReader(source)
	checksum := adler32.New()
	count, err := io.Copy(checksum, io.LimitReader(&contextInput{ctx: ctx, reader: reader}, decodedLen+1))
	closeErr := reader.Close()
	if err != nil || closeErr != nil || ctx.Err() != nil || count != decodedLen || source.Len() > 4 {
		return false
	}
	var expected [4]byte
	binary.BigEndian.PutUint32(expected[:], checksum.Sum32())
	if source.Len() == 4 {
		return true
	}
	return bytes.Equal(encoded[len(encoded)-source.Len():], expected[:source.Len()])
}

// decodeASCIIHexContext 按校验后的长度解码十六进制流，补齐末尾半字节
// 入参: ctx 取消上下文, data 编码数据, warning 可恢复诊断
// 返回: []byte 解码数据, error 编码或取消错误
func decodeASCIIHexContext(ctx context.Context, data []byte, warning func(Diagnostic)) ([]byte, error) {
	return decodeASCIIHexLimitContext(ctx, data, warning, 0)
}

// decodeASCIIHexLimitContext 按上限校验长度后解码十六进制流，补齐末尾半字节
// 入参: ctx 取消上下文, data 编码数据, warning 可恢复诊断, limit 解码字节上限，非正数表示不限
// 返回: []byte 解码数据, error 编码、超限或取消错误
func decodeASCIIHexLimitContext(ctx context.Context, data []byte, warning func(Diagnostic), limit int64) ([]byte, error) {
	end, size, err := asciiHexSize(ctx, data, warning)
	if err != nil {
		return nil, err
	}
	if limit > 0 && int64(size) > limit {
		return nil, fmt.Errorf("ASCIIHex %w", ErrStreamLimitExceeded)
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
// 入参: ctx 取消上下文, data 编码数据, warning 可恢复诊断
// 返回: int 结束标记位置, int 解码字节数, error 编码或取消错误
func asciiHexSize(ctx context.Context, data []byte, warning func(Diagnostic)) (int, int, error) {
	if err := ctx.Err(); err != nil {
		return 0, 0, err
	}
	digits, invalid := 0, false
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
			if warning == nil {
				return 0, 0, hex.InvalidByteError(b)
			}
			if !invalid {
				warning(Diagnostic{Offset: int64(n), Message: "non-hexadecimal ASCIIHex bytes ignored"})
				invalid = true
			}
		}
	}
	if warning != nil {
		warning(Diagnostic{Offset: int64(len(data)), Message: "ASCIIHex terminator missing; encoded stream boundary retained"})
		return len(data), digits/2 + digits%2, ctx.Err()
	}
	return 0, 0, fmt.Errorf("missing ASCIIHex terminator")
}

// decodeLZWBytesLimitContext 有界解码LZW并返回消费长度，超出上限立即中止
// 入参: ctx 取消上下文, data 编码数据, early 提前增长标志, limit 解码字节上限，非正数表示不限
// 返回: []byte 解码数据, int 消费字节数, error 编码、超限或取消错误
func decodeLZWBytesLimitContext(ctx context.Context, data []byte, early int64, limit int64) ([]byte, int, error) {
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
		extra := len(stack) - start
		if code == next {
			extra++
		}
		if limit > 0 && int64(len(out))+int64(extra) > limit {
			return nil, 0, fmt.Errorf("%w: %w", ErrStreamLimitExceeded, io.ErrShortBuffer)
		}
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
// 入参: ctx 取消上下文, data 编码数据, warning 可恢复诊断
// 返回: []byte 独立解码数据, error 编码、溢出或取消错误
func decodeASCII85Context(ctx context.Context, data []byte, warning func(Diagnostic)) ([]byte, error) {
	return decodeASCII85LimitContext(ctx, data, warning, 0)
}

// decodeASCII85LimitContext 按上限校验长度后解码ASCII85，不复制编码数据
// 入参: ctx 取消上下文, data 编码数据, warning 可恢复诊断, limit 解码字节上限，非正数表示不限
// 返回: []byte 独立解码数据, error 编码、溢出、超限或取消错误
func decodeASCII85LimitContext(ctx context.Context, data []byte, warning func(Diagnostic), limit int64) ([]byte, error) {
	end, size, err := ascii85Size(ctx, data, warning)
	if err != nil {
		return nil, err
	}
	if limit > 0 && int64(size) > limit {
		return nil, fmt.Errorf("ASCII85 %w", ErrStreamLimitExceeded)
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
// 入参: ctx 取消上下文, data 编码数据, warning 可恢复诊断
// 返回: int 结束标记位置, int 解码字节数, error 编码、溢出或取消错误
func ascii85Size(ctx context.Context, data []byte, warning func(Diagnostic)) (int, int, error) {
	if err := ctx.Err(); err != nil {
		return 0, 0, err
	}
	var value uint64
	digits, size, end := 0, 0, -1
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
			end = n
			break
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
	if end < 0 && warning == nil {
		return 0, 0, fmt.Errorf("missing ASCII85 terminator")
	}
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
	if end < 0 {
		end = len(data)
		warning(Diagnostic{Offset: int64(end), Message: "ASCII85 terminator missing; encoded stream boundary retained"})
	}
	return end, size, ctx.Err()
}

// decodeRunLength 按校验后的长度一次分配解码缓冲，分段检查取消
// 入参: ctx 取消上下文, data 编码数据, warning 可恢复诊断
// 返回: []byte 解码数据, error 编码或取消错误
func decodeRunLength(ctx context.Context, data []byte, warning func(Diagnostic)) ([]byte, error) {
	return decodeRunLengthLimit(ctx, data, warning, 0)
}

// decodeRunLengthLimit 按上限校验游程长度后解码，超限立即拒绝且不分配展开缓冲
// 入参: ctx 取消上下文, data 编码数据, warning 可恢复诊断, limit 解码字节上限，非正数表示不限
// 返回: []byte 解码数据, error 编码、超限或取消错误
func decodeRunLengthLimit(ctx context.Context, data []byte, warning func(Diagnostic), limit int64) ([]byte, error) {
	size, err := runLengthSizeLimit(ctx, data, warning, limit)
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
	return out, ctx.Err()
}

// runLengthSize 校验游程边界、结束标记及解码长度，不分配展开样本
// 入参: ctx 取消上下文, data 编码数据, warning 可恢复诊断
// 返回: int 解码字节数, error 编码、溢出或取消错误
func runLengthSize(ctx context.Context, data []byte, warning func(Diagnostic)) (int, error) {
	return runLengthSizeLimit(ctx, data, warning, 0)
}

// runLengthSizeLimit 按字节上限校验游程边界、结束标记及解码长度，不分配展开样本
// 入参: ctx 取消上下文, data 编码数据, warning 可恢复诊断, limit 解码字节上限，非正数表示不限
// 返回: int 解码字节数, error 编码、溢出、超限或取消错误
func runLengthSizeLimit(ctx context.Context, data []byte, warning func(Diagnostic), limit int64) (int, error) {
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
		if limit > 0 && int64(size)+int64(count) > limit {
			return 0, fmt.Errorf("RunLength %w", ErrStreamLimitExceeded)
		}
		size += count
	}
	if warning != nil {
		warning(Diagnostic{Offset: int64(len(data)), Message: "RunLength terminator missing; encoded stream boundary retained"})
		return size, ctx.Err()
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

// acquireZlibReader 从池中获取并重置zlib解码器，池为空时新建实例
// 入参: input 压缩数据输入流
// 返回: io.ReadCloser 可用解码器, error 头部解析错误
func acquireZlibReader(input io.Reader) (io.ReadCloser, error) {
	if cached, ok := flateReaderPool.Get().(io.ReadCloser); ok {
		if resetter, ok := cached.(zlib.Resetter); ok {
			if err := resetter.Reset(input, nil); err != nil {
				return nil, err
			}
			return cached, nil
		}
	}
	return zlib.NewReader(input)
}

// releaseZlibReader 清除输入引用后归还zlib解码器到缓冲池
// 入参: reader 待归还的解码器
func releaseZlibReader(reader io.ReadCloser) {
	if resetter, ok := reader.(zlib.Resetter); ok {
		if resetter.Reset(bytes.NewReader(emptyZlibStream), nil) == nil {
			flateReaderPool.Put(reader)
		}
	}
}

// acquireBrotliReader 从池中获取并重置Brotli解码器，启用RFC 9841大窗口以支持PDF 2.0扩展流
// 入参: input 压缩数据输入流
// 返回: *brotli.Reader 可用解码器
func acquireBrotliReader(input io.Reader) *brotli.Reader {
	if cached, ok := brotliReaderPool.Get().(*brotli.Reader); ok {
		_ = cached.Reset(input)
		return cached
	}
	return brotli.NewReaderOptions(input, brotli.ReaderOptions{LargeWindow: true})
}

// releaseBrotliReader 清除输入引用后按规模决定是否归还Brotli解码器，避免大窗口长期驻留内存
// 入参: reader 待归还的解码器, decodedBytes 已解码字节数, failed 是否发生异常或超限
func releaseBrotliReader(reader *brotli.Reader, decodedBytes int64, failed bool) {
	if failed || decodedBytes > 1<<20 {
		return
	}
	if reader.Reset(bytes.NewReader(nil)) == nil {
		brotliReaderPool.Put(reader)
	}
}

// estimateFlateCapacity 根据压缩数据长度预估解压输出缓冲容量，忽略不可信的字典Length
// 入参: encodedLen 压缩数据长度, limit 解码字节上限
// 返回: int 建议初始容量
func estimateFlateCapacity(encodedLen int, limit int64) int {
	hint := 512
	if encodedLen > (1<<20)/3 {
		hint = 1 << 20
	} else if encodedLen*3 > hint {
		hint = encodedLen * 3
	}
	if limit > 0 && int64(hint) > limit {
		hint = int(limit)
	}
	return hint
}

// readAllEstimated 按预估容量一次性读取全部数据，减少切片倍增扩容
// 入参: input 数据源, hint 预估字节数
// 返回: []byte 完整数据, error 读取错误
func readAllEstimated(input io.Reader, hint int) ([]byte, error) {
	var buf bytes.Buffer
	if hint > 0 {
		buf.Grow(hint)
	}
	_, err := buf.ReadFrom(input)
	return buf.Bytes(), err
}

// DecodeStream 使用当前阅读器及背景上下文解码通用流过滤器，支持多工作视图并发解码共享流
// 入参: stream 数据流
// 返回: []byte 独立解码数据, error 解码或关闭错误
func (r *Reader) DecodeStream(stream *Stream) ([]byte, error) {
	return r.DecodeStreamContext(context.Background(), stream)
}

// DecodeStreamContext 使用当前阅读器及诊断配置解码通用流过滤器，支持多工作视图并发解码共享流
// 入参: ctx 取消上下文, stream 数据流
// 返回: []byte 独立解码数据, error 解码、关闭或取消错误
func (r *Reader) DecodeStreamContext(ctx context.Context, stream *Stream) ([]byte, error) {
	if ctx == nil {
		return nil, fmt.Errorf("invalid stream context")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if r == nil {
		return nil, fmt.Errorf("invalid stream reader")
	}
	if r.closed {
		return nil, os.ErrClosed
	}
	if stream == nil {
		return nil, fmt.Errorf("missing stream")
	}
	if !r.ownsObject(stream.reader, stream.cache) {
		return nil, fmt.Errorf("stream belongs to another reader")
	}
	return stream.decodeWith(ctx, r, r.warning, 0)
}

// decodeStream 使用当前阅读器及默认背景上下文解码通用流过滤器
// 入参: stream 数据流
// 返回: []byte 独立解码数据, error 解码错误
func (r *Reader) decodeStream(stream *Stream) ([]byte, error) {
	return r.DecodeStreamContext(context.Background(), stream)
}

// Decode 解码通用流过滤器（图像专用过滤器由图像接口处理），手工构造流须提供直接参数；经Reader.Fork共享后不再调用原Warning回调，源阅读器关闭后返回os.ErrClosed
// 返回: []byte 解码数据, error 解码或关闭错误
func (s *Stream) Decode() ([]byte, error) {
	return s.DecodeContext(context.Background())
}

// DecodeContext 解码通用流过滤器并在处理间检查取消，经Reader.Fork共享后不再调用原Warning回调且源阅读器关闭后返回os.ErrClosed，并发场景请用Reader.DecodeStreamContext
// 入参: ctx 取消上下文
// 返回: []byte 独立解码数据, error 解码、关闭或取消错误
func (s *Stream) DecodeContext(ctx context.Context) ([]byte, error) {
	if s == nil {
		return nil, fmt.Errorf("missing stream")
	}
	if ctx == nil {
		return nil, fmt.Errorf("invalid stream context")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if s.cache != nil && (s.cache.closed.Load() || (s.cache.origin != nil && s.cache.origin.closed.Load())) {
		return nil, os.ErrClosed
	}
	shared := s.cache != nil && s.cache.shared.Load()
	if s.reader != nil && !shared {
		return s.decodeWith(ctx, s.reader, s.reader.warning, 0)
	}
	var warning func(Diagnostic)
	if shared && s.hasWarning {
		warning = func(Diagnostic) {}
	}
	return s.decodeWith(ctx, nil, warning, 0)
}

// decodeWith 按调用方阅读器、诊断策略及可选字节上限解码通用流
// 入参: ctx 取消上下文, reader 解析间接过滤器用的阅读器, warning 恢复诊断, limit 数据上限，零值沿用通用解码
// 返回: []byte 独立解码数据, error 解码、超限或取消错误
func (s *Stream) decodeWith(ctx context.Context, reader *Reader, warning func(Diagnostic), limit int64) ([]byte, error) {
	if ctx == nil {
		return nil, fmt.Errorf("invalid stream context")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	filters, params, err := s.filterChainWith(reader, warning)
	if err != nil {
		return nil, err
	}
	maxBytes, _ := effectiveStreamLimits(reader, s)
	if limit > 0 && (maxBytes == 0 || limit < maxBytes) {
		maxBytes = limit
	}
	data := s.Data
	if limit > 0 && int64(len(data)) > limit {
		return nil, fmt.Errorf("stream recovery %w", ErrStreamLimitExceeded)
	}
	if maxBytes > 0 && int64(len(data)) > maxBytes && (len(filters) == 0 || (len(filters) == 1 && filters[0] == Name("Crypt"))) {
		return nil, ErrStreamLimitExceeded
	}
	for i, filter := range filters {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		name := filter.(Name)
		dict, _ := params[i].(Dictionary)
		if limit > 0 {
			predictor, err := integerDefault(dict, "Predictor", 1)
			if name != "FlateDecode" && name != "BrotliDecode" || err != nil || predictor != 1 {
				return nil, fmt.Errorf("unsupported bounded object stream filter")
			}
		}
		var err error
		switch name {
		case "Crypt":
			if i != 0 || dict["Name"] != nil && dict["Name"] != Name("Identity") {
				return nil, &UnsupportedError{Feature: "stream crypt filter"}
			}
		case "FlateDecode":
			encoded := data
			var reader io.ReadCloser
			reader, err = acquireZlibReader(bytes.NewReader(data))
			if err == nil {
				input := limitStreamReader(&contextInput{ctx: ctx, reader: reader}, maxBytes)
				data, err = readAllEstimated(input, estimateFlateCapacity(len(encoded), maxBytes))
				closeErr := reader.Close()
				releaseZlibReader(reader)
				if maxBytes > 0 && int64(len(data)) > maxBytes {
					if limit > 0 {
						return nil, fmt.Errorf("stream recovery %w", ErrStreamLimitExceeded)
					}
					return nil, fmt.Errorf("Flate %w", ErrStreamLimitExceeded)
				}
				if err == nil {
					err = closeErr
				}
				if warning != nil && ctx.Err() == nil && (errors.Is(err, zlib.ErrChecksum) || err == io.ErrUnexpectedEOF && completeFlatePayload(ctx, encoded, int64(len(data)))) {
					message := "Flate checksum truncated; complete compressed data retained"
					if errors.Is(err, zlib.ErrChecksum) {
						message = "Flate checksum mismatch; complete compressed data retained"
					}
					warning(Diagnostic{Message: message})
					err = nil
				}
			}
		case "BrotliDecode":
			encoded := data
			reader := acquireBrotliReader(&contextInput{ctx: ctx, reader: bytes.NewReader(encoded)})
			input := limitStreamReader(&contextInput{ctx: ctx, reader: reader}, maxBytes)
			data, err = readAllEstimated(input, estimateFlateCapacity(len(encoded), maxBytes))
			exceeded := maxBytes > 0 && int64(len(data)) > maxBytes
			releaseBrotliReader(reader, int64(len(data)), exceeded || err != nil)
			if exceeded {
				if limit > 0 {
					return nil, fmt.Errorf("stream recovery %w", ErrStreamLimitExceeded)
				}
				return nil, fmt.Errorf("Brotli %w", ErrStreamLimitExceeded)
			}
			if warning != nil && ctx.Err() == nil && err != nil && err.Error() == "brotli: excessive input" {
				warning(Diagnostic{Message: "Brotli stream has trailing bytes; complete compressed data retained"})
				err = nil
			}
		case "LZWDecode":
			var early int64
			early, err = integerDefault(dict, "EarlyChange", 1)
			if err == nil {
				data, _, err = decodeLZWBytesLimitContext(ctx, data, early, maxBytes)
				if errors.Is(err, io.ErrShortBuffer) {
					err = fmt.Errorf("LZW %w: %w", ErrStreamLimitExceeded, io.ErrShortBuffer)
				}
			}
		case "ASCIIHexDecode":
			data, err = decodeASCIIHexLimitContext(ctx, data, warning, maxBytes)
		case "ASCII85Decode":
			data, err = decodeASCII85LimitContext(ctx, data, warning, maxBytes)
		case "RunLengthDecode":
			data, err = decodeRunLengthLimit(ctx, data, warning, maxBytes)
		default:
			return nil, &UnsupportedError{Feature: "stream filter " + string(name)}
		}
		if canceled := ctx.Err(); canceled != nil {
			return nil, canceled
		}
		if err != nil {
			return nil, err
		}
		if name == "FlateDecode" || name == "LZWDecode" || name == "BrotliDecode" {
			data, err = restorePredictorContext(ctx, data, dict)
			if err != nil {
				return nil, err
			}
		}
	}
	if len(filters) == 0 || len(filters) == 1 && filters[0] == Name("Crypt") {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		data = bytes.Clone(data)
	}
	return data, nil
}

// pdfObject 实现Object接口
func (*Stream) pdfObject() {}

// cacheFilterRefs 在流进入共享缓存前记录过滤器与解码参数中的间接引用快照，不修改源字典
// 入参: reader 所属阅读器
func (s *Stream) cacheFilterRefs(reader *Reader) {
	if s.Dictionary == nil {
		return
	}
	filter, parms := s.Dictionary["Filter"], s.Dictionary["DecodeParms"]
	if !objectContainsReference(filter) && !objectContainsReference(parms) {
		return
	}
	refs := make(map[Reference]filterRefEntry)
	var capture func(Object, int)
	capture = func(value Object, depth int) {
		if depth > 4 || value == nil {
			return
		}
		if ref, ok := value.(Reference); ok {
			if _, exists := refs[ref]; exists {
				return
			}
			resolved, err := reader.Resolve(ref)
			refs[ref] = filterRefEntry{value: resolved, err: err}
			if err != nil {
				return
			}
			value = resolved
		}
		switch v := value.(type) {
		case Array:
			for _, item := range v {
				capture(item, depth+1)
			}
		case Dictionary:
			for key, item := range v {
				if key == "JBIG2Globals" {
					continue
				}
				capture(item, depth+1)
			}
		}
	}
	capture(filter, 0)
	capture(parms, 0)
	if len(refs) != 0 {
		s.filterRefs = refs
	}
}

// objectContainsReference 检查过滤器和解码参数值树是否包含间接引用
// 入参: value 待检查对象
// 返回: bool 是否含间接引用
func objectContainsReference(value Object) bool {
	switch v := value.(type) {
	case Reference:
		return true
	case Array:
		for _, item := range v {
			if objectContainsReference(item) {
				return true
			}
		}
	case Dictionary:
		for key, item := range v {
			if key == "JBIG2Globals" {
				continue
			}
			if objectContainsReference(item) {
				return true
			}
		}
	}
	return false
}

// filterChain 解析过滤器及解码参数的间接引用，不修改源字典
// 入参: reader 关联阅读器，nil时优先使用只读快照且不访问阅读器
// 返回: Array 过滤器, Array 对应参数, error 错误信息
func (s *Stream) filterChain(reader *Reader) (Array, Array, error) {
	var warning func(Diagnostic)
	if reader != nil {
		warning = reader.warning
	}
	return s.filterChainWith(reader, warning)
}

// filterChainWith 按指定阅读器与诊断回调解析过滤器及解码参数，不修改源字典
// 入参: reader 关联阅读器，nil时优先使用只读快照, warning 恢复诊断回调
// 返回: Array 过滤器, Array 对应参数, error 错误信息
func (s *Stream) filterChainWith(reader *Reader, warning func(Diagnostic)) (Array, Array, error) {
	resolve := func(value Object) (Object, error) {
		if reader != nil {
			return reader.Resolve(value)
		}
		if s.filterRefs != nil {
			if ref, ok := value.(Reference); ok {
				if entry, exists := s.filterRefs[ref]; exists {
					return entry.value, entry.err
				}
			}
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
	case Name:
		filters = Array{v}
	case Array:
		filters = append(Array(nil), v...)
	default:
		return nil, nil, fmt.Errorf("invalid stream filter")
	}
	if _, maxChain := effectiveStreamLimits(reader, s); maxChain > 0 && len(filters) > maxChain {
		return nil, nil, fmt.Errorf("%w: filter chain depth exceeded", ErrStreamLimitExceeded)
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
			if warning == nil {
				return nil, nil, fmt.Errorf("filter parameter count mismatch")
			}
			warning(Diagnostic{Message: "filter parameter count mismatch; positional parameters and missing defaults retained"})
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
		if warning != nil {
			if name, ok := streamFilterAliases[filters[i].(Name)]; ok {
				warning(Diagnostic{Message: "abbreviated stream filter expanded: " + string(filters[i].(Name))})
				filters[i] = name
			}
			for _, name := range []Name{"FlateDecode", "BrotliDecode", "LZWDecode", "ASCIIHexDecode", "ASCII85Decode", "RunLengthDecode", "CCITTFaxDecode", "DCTDecode", "JPXDecode", "JBIG2Decode", "Crypt"} {
				if filters[i] != name && strings.EqualFold(string(filters[i].(Name)), string(name)) {
					warning(Diagnostic{Message: "stream filter name case normalized: " + string(filters[i].(Name))})
					filters[i] = name
					break
				}
			}
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
			if reader != nil || s.filterRefs != nil {
				resolved := make(Dictionary, len(dict))
				for key, value := range dict {
					if key == "JBIG2Globals" {
						resolved[key] = value
						continue
					}
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

// Read 检查取消状态后读取数据
// 入参: p 数据缓冲区
// 返回: int 已读字节数, error 读取错误
func (r *contextInput) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.reader.Read(p)
}
