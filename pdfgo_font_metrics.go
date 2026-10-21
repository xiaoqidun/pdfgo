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
	"context"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"math"
	"slices"
	"sort"
	"strconv"
	"sync"
)

// cidMetricRange 保存连续CID共用的度量，不展开逐字缓存
type cidMetricRange[T comparable] struct {
	first, last uint32
	value       T
}

// cidMetrics 按CID排列互不相交的度量范围，后声明的范围覆盖前值
type cidMetrics[T comparable] struct {
	ranges []cidMetricRange[T]
}

// fontProgramMetrics 延迟读取内嵌度量资源，不缓存取消或解析失败
type fontProgramMetrics struct {
	mu           sync.Mutex
	program      []byte
	type1        bool
	type1Metrics *type1FontMetrics
	cffMetrics   *cffFontMetrics
}

// type1FontMetrics 保存字体矩阵及未执行的Type1字形和子程序
type type1FontMetrics struct {
	matrix Matrix
	lenIV  int
	glyphs map[string][]byte
	subrs  map[int][]byte
}

// cffFontMetrics 保存CFF字形、子程序和私有字宽，仅解释实际使用的字形
type cffFontMetrics struct {
	chars, locals, globals     [][]byte
	matrix                     Matrix
	kind                       int
	defaultWidth, nominalWidth float64
}

// set 覆盖指定范围，保留两端旧值并合并相邻同值范围
// 入参: first 起始CID, last 结束CID, value 度量
func (m *cidMetrics[T]) set(first, last uint32, value T) {
	i := sort.Search(len(m.ranges), func(i int) bool { return m.ranges[i].last >= first })
	j := sort.Search(len(m.ranges), func(i int) bool { return m.ranges[i].first > last })
	var replacement [3]cidMetricRange[T]
	n := 0
	if i < j && m.ranges[i].first < first {
		replacement[n] = m.ranges[i]
		replacement[n].last = first - 1
		n++
	}
	replacement[n] = cidMetricRange[T]{first: first, last: last, value: value}
	n++
	if i < j && m.ranges[j-1].last > last {
		replacement[n] = m.ranges[j-1]
		replacement[n].first = last + 1
		n++
	}
	m.ranges = slices.Replace(m.ranges, i, j, replacement[:n]...)
	for k := max(i-1, 0); k+1 < len(m.ranges) && k <= i+n; {
		if m.ranges[k].last+1 == m.ranges[k+1].first && m.ranges[k].value == m.ranges[k+1].value {
			m.ranges[k].last = m.ranges[k+1].last
			m.ranges = slices.Delete(m.ranges, k+1, k+2)
		} else {
			k++
		}
	}
}

// get 查找CID的显式度量，未声明时交由字体使用默认值
// 入参: cid 字符标识
// 返回: T 度量, bool 是否存在
func (m *cidMetrics[T]) get(cid uint32) (T, bool) {
	i := sort.Search(len(m.ranges), func(i int) bool { return m.ranges[i].last >= cid })
	if i < len(m.ranges) && m.ranges[i].first <= cid {
		return m.ranges[i].value, true
	}
	var zero T
	return zero, false
}

// load 在首次实际解码时读取度量资源，取消后允许重新读取
// 入参: ctx 取消上下文
// 返回: *type1FontMetrics Type1资源, *cffFontMetrics CFF资源, error 读取或取消错误
func (m *fontProgramMetrics) load(ctx context.Context) (*type1FontMetrics, *cffFontMetrics, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	if m.type1Metrics == nil && m.cffMetrics == nil {
		var err error
		if m.type1 {
			m.type1Metrics, err = readType1FontMetrics(ctx, m.program)
		} else {
			m.cffMetrics, err = readCFFFontMetrics(ctx, m.program)
		}
		if err != nil {
			m.type1Metrics, m.cffMetrics = nil, nil
			return nil, nil, err
		}
	}
	return m.type1Metrics, m.cffMetrics, ctx.Err()
}

// readType1FontMetrics 读取PFA或PFB的矩阵及字形索引，不执行字形指令
// 入参: ctx 取消上下文, data 内嵌Type1程序
// 返回: *type1FontMetrics 度量资源, error 结构或取消错误
func readType1FontMetrics(ctx context.Context, data []byte) (*type1FontMetrics, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	var encrypted []byte
	pfb := len(data) > 0 && data[0] == 0x80
	if pfb {
		var header []byte
		ended := false
		for len(data) >= 2 {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			if data[0] != 0x80 {
				return nil, fmt.Errorf("invalid PFB metric segment")
			}
			kind := data[1]
			if kind == 3 {
				ended = true
				break
			}
			if len(data) < 6 || kind != 1 && kind != 2 {
				return nil, fmt.Errorf("invalid PFB metric segment")
			}
			length := uint64(binary.LittleEndian.Uint32(data[2:6]))
			if length > uint64(len(data)-6) {
				return nil, fmt.Errorf("truncated PFB metric segment")
			}
			if kind == 2 {
				encrypted = append(encrypted, data[6:6+int(length)]...)
			} else if encrypted == nil {
				header = append(header, data[6:6+int(length)]...)
			}
			data = data[6+int(length):]
		}
		if !ended {
			return nil, fmt.Errorf("missing PFB metric end marker")
		}
		data = header
	}
	scanner := type1EncodingScanner{data: data}
	var matrix Matrix
	matrixFound := false
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		token := scanner.next()
		if token == "" {
			return nil, fmt.Errorf("Type1 metrics lack eexec data")
		}
		if token == "eexec" {
			break
		}
		if token != "/FontMatrix" {
			continue
		}
		close := "]"
		switch scanner.next() {
		case "[":
		case "{":
			close = "}"
		default:
			return nil, fmt.Errorf("invalid Type1 metric matrix")
		}
		for i := range matrix {
			value, err := strconv.ParseFloat(scanner.next(), 64)
			if err != nil || math.IsNaN(value) || math.IsInf(value, 0) {
				return nil, fmt.Errorf("invalid Type1 metric matrix value")
			}
			matrix[i] = value
		}
		if scanner.next() != close || matrix[0]*matrix[3]-matrix[1]*matrix[2] == 0 {
			return nil, fmt.Errorf("invalid Type1 metric matrix")
		}
		matrixFound = true
	}
	if !matrixFound {
		return nil, fmt.Errorf("Type1 metrics lack FontMatrix")
	}
	if !pfb {
		if scanner.pos >= len(data) || !type1EncodingSpace(data[scanner.pos]) {
			return nil, fmt.Errorf("missing Type1 eexec separator")
		}
		separator := data[scanner.pos]
		scanner.pos++
		if separator == '\r' && scanner.pos < len(data) && data[scanner.pos] == '\n' {
			scanner.pos++
		}
		encrypted = data[scanner.pos:]
		digits := 0
		for _, b := range encrypted {
			if type1EncodingSpace(b) {
				continue
			}
			if bytes.IndexByte([]byte("0123456789abcdefABCDEF"), b) < 0 {
				break
			}
			digits++
			if digits == 4 {
				break
			}
		}
		if digits == 4 {
			if end := bytes.Index(encrypted, []byte("cleartomark")); end >= 0 {
				encrypted = encrypted[:end]
			}
			var encoded []byte
			for i, b := range encrypted {
				if i&4095 == 0 {
					if err := ctx.Err(); err != nil {
						return nil, err
					}
				}
				if type1EncodingSpace(b) {
					continue
				}
				if bytes.IndexByte([]byte("0123456789abcdefABCDEF"), b) < 0 {
					break
				}
				encoded = append(encoded, b)
			}
			encrypted = make([]byte, hex.DecodedLen(len(encoded)))
			if _, err := hex.Decode(encrypted, encoded); err != nil {
				return nil, fmt.Errorf("invalid Type1 metric eexec encoding: %w", err)
			}
		}
	}
	plain, err := type1MetricDecrypt(ctx, encrypted, 55665, 4)
	if err != nil {
		return nil, err
	}
	scanner = type1EncodingScanner{data: plain}
	lenIV := 4
	subrs := map[int][]byte{}
	glyphs := map[string][]byte{}
	section := ""
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		token := scanner.next()
		if token == "" || token == "closefile" {
			break
		}
		switch token {
		case "/lenIV":
			lenIV, err = strconv.Atoi(scanner.next())
			if err != nil || lenIV < -1 || lenIV > len(plain) {
				return nil, fmt.Errorf("invalid Type1 metric lenIV")
			}
		case "/Subrs", "/CharStrings":
			section = token
			count, err := strconv.Atoi(scanner.next())
			if err != nil || count < 0 || count > len(plain) {
				return nil, fmt.Errorf("invalid Type1 metric dictionary size")
			}
		case "def", "ND", "|-":
			if section == "/Subrs" {
				section = ""
			}
		case "end":
			if section == "/CharStrings" {
				section = ""
			}
		case "dup":
			if section != "/Subrs" {
				continue
			}
			index, err := strconv.Atoi(scanner.next())
			if err != nil || index < 0 || index > len(plain) {
				return nil, fmt.Errorf("invalid Type1 metric subroutine index")
			}
			raw, err := scanner.type1MetricBinary()
			if err != nil {
				return nil, err
			}
			subrs[index] = raw
		default:
			if section != "/CharStrings" || len(token) < 2 || token[0] != '/' {
				continue
			}
			raw, err := scanner.type1MetricBinary()
			if err != nil {
				return nil, err
			}
			glyphs[token[1:]] = raw
		}
	}
	if len(glyphs) == 0 {
		return nil, fmt.Errorf("Type1 metrics lack CharStrings")
	}
	return &type1FontMetrics{matrix: matrix, lenIV: lenIV, glyphs: glyphs, subrs: subrs}, ctx.Err()
}

// glyphWidth 仅解密所选字形及必要子程序，转换为千分之一横向字宽
// 入参: ctx 取消上下文, name 实际字形名称
// 返回: float64 横向字宽, error 字形度量或取消错误
func (m *type1FontMetrics) glyphWidth(ctx context.Context, name string) (float64, error) {
	raw, ok := m.glyphs[name]
	if !ok {
		return 0, fmt.Errorf("Type1 glyph %s lacks metrics", name)
	}
	data, err := type1MetricDecrypt(ctx, raw, 4330, m.lenIV)
	if err != nil {
		return 0, err
	}
	steps := 0
	args := make([]float64, 0, 24)
	width, found, err := type1MetricWidth(ctx, data, m.subrs, m.lenIV, &args, &steps, 0)
	if err != nil {
		return 0, fmt.Errorf("Type1 glyph %s metrics: %w", name, err)
	}
	if !found {
		return 0, fmt.Errorf("Type1 glyph %s lacks width", name)
	}
	x := (m.matrix[0]*width.X + m.matrix[2]*width.Y) * 1000
	y := (m.matrix[1]*width.X + m.matrix[3]*width.Y) * 1000
	if math.IsNaN(x) || math.IsInf(x, 0) || math.IsNaN(y) || math.IsInf(y, 0) {
		return 0, fmt.Errorf("invalid Type1 glyph %s width", name)
	}
	if y != 0 {
		return 0, &UnsupportedError{Feature: "nonhorizontal Type1 glyph metrics"}
	}
	return x, ctx.Err()
}

// type1MetricBinary 按声明长度读取二进制字形，不解释数据中的词项
// 返回: []byte 原始字形数据, error 长度或分隔符错误
func (s *type1EncodingScanner) type1MetricBinary() ([]byte, error) {
	length, err := strconv.Atoi(s.next())
	if err != nil || length < 0 {
		return nil, fmt.Errorf("invalid Type1 metric charstring length")
	}
	if token := s.next(); token != "RD" && token != "-|" {
		return nil, fmt.Errorf("missing Type1 metric charstring data")
	}
	if s.pos >= len(s.data) || !type1EncodingSpace(s.data[s.pos]) {
		return nil, fmt.Errorf("missing Type1 metric charstring separator")
	}
	s.pos++
	if length > len(s.data)-s.pos {
		return nil, fmt.Errorf("truncated Type1 metric charstring")
	}
	raw := s.data[s.pos : s.pos+length]
	s.pos += length
	return raw, nil
}

// type1MetricDecrypt 解密Type1数据并移除随机前缀，负前缀长度表示明文
// 入参: ctx 取消上下文, data 字体密文, seed 密钥, skip 随机前缀长度
// 返回: []byte 明文数据, error 截断或取消错误
func type1MetricDecrypt(ctx context.Context, data []byte, seed uint16, skip int) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if skip < 0 {
		return data, nil
	}
	if len(data) < skip {
		return nil, fmt.Errorf("truncated Type1 metric ciphertext")
	}
	plain := make([]byte, len(data))
	for i, b := range data {
		if i&4095 == 0 {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
		}
		plain[i] = b ^ byte(seed>>8)
		seed = uint16((uint32(b)+uint32(seed))*52845 + 22719)
	}
	return plain[skip:], nil
}

// type1MetricWidth 执行Type1前缀直到hsbw或sbw，不执行后续轮廓指令
// 入参: ctx 取消上下文, data 字形指令, subrs 子程序, lenIV 加密前缀长度, args 操作数栈, steps 累计指令数, depth 调用深度
// 返回: Point 字体空间字宽向量, bool 是否获得字宽, error 指令或资源限制错误
func type1MetricWidth(ctx context.Context, data []byte, subrs map[int][]byte, lenIV int, args *[]float64, steps *int, depth int) (Point, bool, error) {
	if depth > 10 {
		return Point{}, false, fmt.Errorf("Type1 metric subroutine nesting limit")
	}
	for pos := 0; pos < len(data); {
		if *steps&255 == 0 {
			if err := ctx.Err(); err != nil {
				return Point{}, false, err
			}
		}
		*steps++
		if *steps > 100000 {
			return Point{}, false, fmt.Errorf("Type1 metric instruction limit")
		}
		op := int(data[pos])
		pos++
		if op >= 32 {
			var value float64
			switch {
			case op <= 246:
				value = float64(op - 139)
			case op <= 254:
				if pos >= len(data) {
					return Point{}, false, fmt.Errorf("truncated Type1 metric operand")
				}
				if op <= 250 {
					value = float64((op-247)*256 + int(data[pos]) + 108)
				} else {
					value = float64(-(op-251)*256 - int(data[pos]) - 108)
				}
				pos++
			default:
				if len(data)-pos < 4 {
					return Point{}, false, fmt.Errorf("truncated Type1 metric operand")
				}
				value = float64(int32(binary.BigEndian.Uint32(data[pos:])))
				pos += 4
			}
			if len(*args) >= 24 {
				return Point{}, false, fmt.Errorf("Type1 metric operand limit")
			}
			*args = append(*args, value)
			continue
		}
		if op == 12 {
			if pos >= len(data) {
				return Point{}, false, fmt.Errorf("truncated Type1 metric operator")
			}
			op = 1200 + int(data[pos])
			pos++
		}
		a := *args
		switch op {
		case 13:
			if len(a) != 2 {
				return Point{}, false, fmt.Errorf("invalid Type1 hsbw metrics")
			}
			return Point{X: a[1]}, true, nil
		case 1207:
			if len(a) != 4 {
				return Point{}, false, fmt.Errorf("invalid Type1 sbw metrics")
			}
			return Point{X: a[2], Y: a[3]}, true, nil
		case 1212:
			if len(a) < 2 || a[len(a)-1] == 0 {
				return Point{}, false, fmt.Errorf("invalid Type1 metric division")
			}
			value := a[len(a)-2] / a[len(a)-1]
			if math.IsNaN(value) || math.IsInf(value, 0) {
				return Point{}, false, fmt.Errorf("invalid Type1 metric quotient")
			}
			*args = append(a[:len(a)-2], value)
		case 10:
			if len(a) == 0 {
				return Point{}, false, fmt.Errorf("missing Type1 metric subroutine index")
			}
			value := a[len(a)-1]
			if value < 0 || value > math.MaxInt32 || math.Trunc(value) != value {
				return Point{}, false, fmt.Errorf("invalid Type1 metric subroutine index")
			}
			subr, ok := subrs[int(value)]
			if !ok {
				return Point{}, false, fmt.Errorf("missing Type1 metric subroutine")
			}
			*args = a[:len(a)-1]
			subr, err := type1MetricDecrypt(ctx, subr, 4330, lenIV)
			if err != nil {
				return Point{}, false, err
			}
			width, found, err := type1MetricWidth(ctx, subr, subrs, lenIV, args, steps, depth+1)
			if err != nil || found {
				return width, found, err
			}
		case 11:
			if depth == 0 {
				return Point{}, false, fmt.Errorf("Type1 metric return outside subroutine")
			}
			return Point{}, false, nil
		default:
			return Point{}, false, &UnsupportedError{Feature: fmt.Sprintf("Type1 metric operator %d before width", op)}
		}
	}
	return Point{}, false, fmt.Errorf("incomplete Type1 glyph metrics")
}

// readCFFFontMetrics 读取CFF索引、矩阵及私有字宽，不执行字形指令
// 入参: ctx 取消上下文, data CFF字体程序
// 返回: *cffFontMetrics 度量资源, error 字典或取消错误
func readCFFFontMetrics(ctx context.Context, data []byte) (*cffFontMetrics, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	dict, _, chars, err := cffFontData(data)
	if err != nil {
		return nil, err
	}
	metrics := &cffFontMetrics{chars: chars, kind: 2, matrix: Matrix{.001, 0, 0, .001, 0, 0}}
	if values := dict[1207]; values != nil {
		copy(metrics.matrix[:], values)
	}
	if values := dict[1206]; values != nil {
		metrics.kind = int(values[0])
	}
	pos := int(data[2])
	for range 3 {
		_, pos, err = cffIndex(data, pos)
		if err != nil {
			return nil, err
		}
	}
	metrics.globals, _, err = cffIndex(data, pos)
	if err != nil {
		return nil, err
	}
	if values, present := dict[18]; present {
		if len(values) != 2 || values[0] < 0 || values[1] < 0 || values[0] > float64(len(data)) || values[1] > float64(len(data)) || math.Trunc(values[0]) != values[0] || math.Trunc(values[1]) != values[1] {
			return nil, fmt.Errorf("invalid CFF metric private dictionary")
		}
		size, offset := int(values[0]), int(values[1])
		if size > len(data)-offset {
			return nil, fmt.Errorf("truncated CFF metric private dictionary")
		}
		private, err := cffDictionary(data[offset : offset+size])
		if err != nil {
			return nil, err
		}
		for _, entry := range []struct {
			op    int
			value *float64
		}{{20, &metrics.defaultWidth}, {21, &metrics.nominalWidth}} {
			if values, present := private[entry.op]; present {
				if len(values) != 1 || math.IsNaN(values[0]) || math.IsInf(values[0], 0) {
					return nil, fmt.Errorf("invalid CFF private width")
				}
				*entry.value = values[0]
			}
		}
		if _, present := private[19]; present {
			relative, err := cffOffset(data[offset:], private, 19, 0)
			if err != nil {
				return nil, err
			}
			metrics.locals, _, err = cffIndex(data, offset+relative)
			if err != nil {
				return nil, err
			}
		}
	}
	return metrics, ctx.Err()
}

// glyphWidth 从所选CFF字形读取实际字宽，不按字体名称替代度量
// 入参: ctx 取消上下文, gid 实际字形编号
// 返回: float64 千分之一横向字宽, error 度量或取消错误
func (m *cffFontMetrics) glyphWidth(ctx context.Context, gid uint16) (float64, error) {
	if int(gid) >= len(m.chars) {
		return 0, fmt.Errorf("invalid CFF metric glyph index")
	}
	steps := 0
	args := make([]float64, 0, 48)
	var width Point
	if m.kind == 1 {
		subrs := make(map[int][]byte, len(m.locals))
		for i, data := range m.locals {
			subrs[i] = data
		}
		value, found, err := type1MetricWidth(ctx, m.chars[gid], subrs, -1, &args, &steps, 0)
		if err != nil {
			return 0, err
		}
		if !found {
			return 0, fmt.Errorf("CFF Type1 glyph lacks width")
		}
		width = value
	} else {
		value, found, err := m.type2Width(ctx, m.chars[gid], &args, &steps, 0)
		if err != nil {
			return 0, err
		}
		if !found {
			return 0, fmt.Errorf("CFF Type2 glyph lacks width operator")
		}
		width.X = value
	}
	x := (m.matrix[0]*width.X + m.matrix[2]*width.Y) * 1000
	y := (m.matrix[1]*width.X + m.matrix[3]*width.Y) * 1000
	if math.IsNaN(x) || math.IsInf(x, 0) || math.IsNaN(y) || math.IsInf(y, 0) {
		return 0, fmt.Errorf("invalid CFF glyph width")
	}
	if y != 0 {
		return 0, &UnsupportedError{Feature: "nonhorizontal CFF glyph metrics"}
	}
	return x, nil
}

// type2Width 解释Type2首个宽度操作符及必要子程序，区分缺省和明确零宽度
// 入参: ctx 取消上下文, data 字形指令, args 操作数栈, steps 累计指令数, depth 调用深度
// 返回: float64 字体空间横向字宽, bool 是否获得字宽, error 指令或资源限制错误
func (m *cffFontMetrics) type2Width(ctx context.Context, data []byte, args *[]float64, steps *int, depth int) (float64, bool, error) {
	if depth > 10 {
		return 0, false, fmt.Errorf("CFF metric subroutine nesting limit")
	}
	for pos := 0; pos < len(data); {
		if *steps&255 == 0 {
			if err := ctx.Err(); err != nil {
				return 0, false, err
			}
		}
		*steps++
		if *steps > 100000 {
			return 0, false, fmt.Errorf("CFF metric instruction limit")
		}
		op := int(data[pos])
		pos++
		if op >= 32 || op == 28 {
			var value float64
			switch {
			case op == 28:
				if len(data)-pos < 2 {
					return 0, false, fmt.Errorf("truncated CFF metric operand")
				}
				value = float64(int16(binary.BigEndian.Uint16(data[pos:])))
				pos += 2
			case op <= 246:
				value = float64(op - 139)
			case op <= 254:
				if pos >= len(data) {
					return 0, false, fmt.Errorf("truncated CFF metric operand")
				}
				if op <= 250 {
					value = float64((op-247)*256 + int(data[pos]) + 108)
				} else {
					value = float64(-(op-251)*256 - int(data[pos]) - 108)
				}
				pos++
			default:
				if len(data)-pos < 4 {
					return 0, false, fmt.Errorf("truncated CFF metric operand")
				}
				value = float64(int32(binary.BigEndian.Uint32(data[pos:]))) / 65536
				pos += 4
			}
			if len(*args) >= 48 {
				return 0, false, fmt.Errorf("CFF metric operand limit")
			}
			*args = append(*args, value)
			continue
		}
		a := *args
		switch op {
		case 1, 3, 18, 23, 19, 20:
			if len(a)%2 == 1 {
				return m.nominalWidth + a[0], true, nil
			}
			return m.defaultWidth, true, nil
		case 4, 22, 21, 14:
			count := 1
			if op == 21 {
				count = 2
			} else if op == 14 {
				count = len(a) / 4 * 4
				if count > 4 {
					return 0, false, fmt.Errorf("invalid CFF metric endchar operands")
				}
			}
			if len(a) == count+1 {
				return m.nominalWidth + a[0], true, nil
			}
			if len(a) != count {
				return 0, false, fmt.Errorf("invalid CFF metric move operands")
			}
			return m.defaultWidth, true, nil
		case 10, 29:
			if len(a) == 0 {
				return 0, false, fmt.Errorf("missing CFF metric subroutine index")
			}
			subrs := m.locals
			if op == 29 {
				subrs = m.globals
			}
			bias := 107
			if len(subrs) >= 33900 {
				bias = 32768
			} else if len(subrs) >= 1240 {
				bias = 1131
			}
			index := a[len(a)-1] + float64(bias)
			if index < 0 || index >= float64(len(subrs)) || math.Trunc(index) != index {
				return 0, false, fmt.Errorf("invalid CFF metric subroutine index")
			}
			*args = a[:len(a)-1]
			width, found, err := m.type2Width(ctx, subrs[int(index)], args, steps, depth+1)
			if err != nil || found {
				return width, found, err
			}
		case 11:
			if depth == 0 {
				return 0, false, fmt.Errorf("CFF metric return outside subroutine")
			}
			return 0, false, nil
		default:
			return 0, false, &UnsupportedError{Feature: fmt.Sprintf("CFF metric operator %d before width", op)}
		}
	}
	return 0, false, fmt.Errorf("incomplete CFF glyph metrics")
}
