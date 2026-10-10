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
	"fmt"
	"slices"
)

// jpegHuffman 保存规范霍夫曼表及反向编码
type jpegHuffman struct {
	counts [16]byte
	values []byte
	codes  [256]uint16
	length [256]uint8
	lookup [256]uint16
	min    [17]int
	max    [17]int
	base   [17]int
}

// jpegEntropy 读取转义后的JPEG扫描位流
type jpegEntropy struct {
	data []byte
	pos  int
	bits uint32
	n    uint
}

// jpegBitWriter 写出JPEG扫描位流及字节转义，按需限制候选体积
type jpegBitWriter struct {
	bytes.Buffer
	bits     uint32
	n        uint
	limit    int
	exceeded bool
}

// jpegScan 保存单扫描顺序JPEG的组件、重启间隔及原始扫描头片段
type jpegScan struct {
	header  [][]byte
	sos     int
	start   int
	mcus    int
	restart int
	blocks  []int
	tables  [8]jpegHuffman
}

// OptimizeJPEG 在不改变量化系数、尺寸和元数据的情况下优化JPEG熵编码
// 当前优化单扫描顺序霍夫曼JPEG，其他编码及没有体积收益的图片返回原数据
// 返回数据可能与输入共享内存，调用方不得修改；取消与损坏扫描返回错误
// 入参: ctx 取消上下文, data JPEG原始数据
// 返回: []byte 优化数据, error 编码错误
func OptimizeJPEG(ctx context.Context, data []byte) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	scan, err := readJPEGScan(ctx, data)
	if err != nil || scan == nil {
		return data, err
	}
	var frequencies [8][256]uint64
	var bitCount uint64
	end, err := scan.walk(ctx, data, func(table int, symbol byte, bits uint32, n uint) {
		frequencies[table][symbol]++
		bitCount += uint64(n)
	}, nil)
	if err != nil {
		return nil, err
	}
	var tables [8]jpegHuffman
	var dht bytes.Buffer
	for i, frequency := range frequencies {
		table := optimalJPEGHuffman(frequency)
		tables[i] = table
		if len(table.values) == 0 {
			continue
		}
		dht.WriteByte(byte(i%4) | byte(i/4)<<4)
		dht.Write(table.counts[:])
		dht.Write(table.values)
		for _, symbol := range table.values {
			bitCount += frequency[symbol] * uint64(table.length[symbol])
		}
	}
	limit := end - (scan.start - scan.sos) - 4 - dht.Len() - 1
	for _, part := range scan.header {
		limit -= len(part)
	}
	if limit <= 0 {
		return data, ctx.Err()
	}
	output := jpegBitWriter{limit: limit}
	maxEntropy := 2 * ((bitCount + 7) / 8)
	if scan.restart > 0 {
		maxEntropy += 4 * uint64((scan.mcus-1)/scan.restart)
	}
	overhead := len(data) - 1 - limit
	capacity := min(uint64(limit), maxEntropy) + min(uint64(overhead), maxEntropy)
	output.Grow(int(min(uint64(len(data)-1), capacity)))
	_, err = scan.walk(ctx, data, func(table int, symbol byte, bits uint32, n uint) {
		code := &tables[table]
		output.put(uint32(code.codes[symbol]), uint(code.length[symbol]))
		output.put(bits, n)
	}, &output)
	if err != nil {
		return nil, err
	}
	output.flush()
	if output.exceeded {
		return data, ctx.Err()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	entropy := output.Len()
	output.Grow(overhead)
	output.Write(output.AvailableBuffer()[:overhead])
	result := output.Bytes()
	prefix := overhead - (len(data) - end)
	copy(result[prefix:], result[:entropy])
	pos := 0
	for _, part := range scan.header {
		pos += copy(result[pos:], part)
	}
	pos += copy(result[pos:], []byte{0xff, 0xc4, byte((dht.Len() + 2) >> 8), byte(dht.Len() + 2)})
	pos += copy(result[pos:], dht.Bytes())
	pos += copy(result[pos:], data[scan.sos:scan.start])
	copy(result[pos+entropy:], data[end:])
	return result, nil
}

// readJPEGScan 读取可优化的扫描头，其他合法JPEG编码留给原始字节直通
// 入参: ctx 取消上下文, data JPEG数据
// 返回: *jpegScan 扫描配置，nil表示无需优化, error 解析或取消错误
func readJPEGScan(ctx context.Context, data []byte) (*jpegScan, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if len(data) < 4 || data[0] != 0xff || data[1] != 0xd8 {
		return nil, fmt.Errorf("invalid JPEG header")
	}
	s := &jpegScan{}
	headerStart := 0
	type component struct{ id, h, v byte }
	var components []component
	width, height, maxH, maxV := 0, 0, 0, 0
	for pos := 2; pos+4 <= len(data); {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		begin := pos
		if data[pos] != 0xff {
			return nil, fmt.Errorf("invalid JPEG marker")
		}
		for pos < len(data) && data[pos] == 0xff {
			if pos&4095 == 0 {
				if err := ctx.Err(); err != nil {
					return nil, err
				}
			}
			pos++
		}
		if pos+3 > len(data) {
			break
		}
		marker := data[pos]
		length := int(binary.BigEndian.Uint16(data[pos+1:]))
		stop := pos + 1 + length
		if length < 2 || stop > len(data) {
			return nil, fmt.Errorf("invalid JPEG segment")
		}
		value := data[pos+3 : stop]
		switch marker {
		case 0xc0:
			if len(value) < 6 || value[0] != 8 || len(value) != 6+3*int(value[5]) || value[5] == 0 {
				return nil, nil
			}
			height, width = int(binary.BigEndian.Uint16(value[1:])), int(binary.BigEndian.Uint16(value[3:]))
			for i := 6; i < len(value); i += 3 {
				c := component{value[i], value[i+1] >> 4, value[i+1] & 15}
				if c.h == 0 || c.v == 0 || c.h > 4 || c.v > 4 {
					return nil, fmt.Errorf("invalid JPEG sampling")
				}
				components = append(components, c)
				maxH, maxV = max(maxH, int(c.h)), max(maxV, int(c.v))
			}
		case 0xc4:
			if headerStart < begin {
				s.header = append(s.header, data[headerStart:begin])
			}
			headerStart = stop
			for len(value) > 0 {
				if err := ctx.Err(); err != nil {
					return nil, err
				}
				if len(value) < 17 || value[0]&0xec != 0 {
					return nil, fmt.Errorf("invalid JPEG Huffman table")
				}
				index := int(value[0]&3) + int(value[0]>>4)*4
				table := jpegHuffman{}
				copy(table.counts[:], value[1:17])
				n := 0
				for _, count := range table.counts {
					n += int(count)
				}
				if n > 256 || len(value) < 17+n {
					return nil, fmt.Errorf("invalid JPEG Huffman symbols")
				}
				table.values = bytes.Clone(value[17 : 17+n])
				if err := table.prepare(); err != nil {
					return nil, err
				}
				s.tables[index] = table
				value = value[17+n:]
			}
		case 0xdd:
			if len(value) != 2 {
				return nil, fmt.Errorf("invalid JPEG restart interval")
			}
			s.restart = int(binary.BigEndian.Uint16(value))
		case 0xda:
			if len(value) < 4 || len(value) != 4+2*int(value[0]) || int(value[0]) != len(components) || width == 0 || height == 0 {
				return nil, nil
			}
			if !bytes.Equal(value[len(value)-3:], []byte{0, 63, 0}) {
				return nil, nil
			}
			for i := 0; i < int(value[0]); i++ {
				found := false
				for _, c := range components {
					if c.id != value[1+2*i] {
						continue
					}
					selector := value[2+2*i]
					if selector>>4 > 3 || selector&15 > 3 {
						return nil, fmt.Errorf("invalid JPEG table selector")
					}
					blocks := int(c.h) * int(c.v)
					if len(components) == 1 {
						blocks, maxH, maxV = 1, 1, 1
					}
					for range blocks {
						s.blocks = append(s.blocks, int(selector>>4), int(selector&15)+4)
					}
					found = true
					break
				}
				if !found {
					return nil, fmt.Errorf("missing JPEG component")
				}
			}
			s.mcus = ((width + maxH*8 - 1) / (maxH * 8)) * ((height + maxV*8 - 1) / (maxV * 8))
			s.start = stop
			s.sos = begin
			if headerStart < begin {
				s.header = append(s.header, data[headerStart:begin])
			}
			return s, nil
		default:
			if marker >= 0xc1 && marker <= 0xcf && marker != 0xc4 {
				return nil, nil
			}
		}
		pos = stop
	}
	return nil, fmt.Errorf("missing JPEG scan")
}

// prepare 构造霍夫曼编解码索引
// 返回: error 码表错误
func (h *jpegHuffman) prepare() error {
	code, index := 0, 0
	for length, count := range h.counts {
		n := int(count)
		h.min[length+1], h.max[length+1], h.base[length+1] = code, code+n-1, index
		if code+n > 1<<(length+1) {
			return fmt.Errorf("oversubscribed JPEG Huffman table")
		}
		for range n {
			symbol := h.values[index]
			if h.length[symbol] != 0 {
				return fmt.Errorf("duplicate JPEG Huffman symbol")
			}
			h.codes[symbol], h.length[symbol] = uint16(code), uint8(length+1)
			if length < 8 {
				start := code << (7 - length)
				for suffix := range 1 << (7 - length) {
					h.lookup[start+suffix] = uint16(symbol)<<8 | uint16(length+1)
				}
			}
			code++
			index++
		}
		code <<= 1
	}
	return nil
}

// take 读取扫描位并消费FF转义
// 入参: n 位数
// 返回: uint32 编码值, error 读取错误
func (r *jpegEntropy) take(n uint) (uint32, error) {
	for r.n < n {
		if r.pos >= len(r.data) {
			return 0, fmt.Errorf("truncated JPEG scan")
		}
		b := r.data[r.pos]
		r.pos++
		if b == 0xff {
			if r.pos >= len(r.data) || r.data[r.pos] != 0 {
				return 0, fmt.Errorf("unexpected JPEG scan marker")
			}
			r.pos++
		}
		r.bits = r.bits<<8 | uint32(b)
		r.n += 8
	}
	r.n -= n
	return r.bits >> r.n & ((1 << n) - 1), nil
}

// symbol 查表解码短码，长码及扫描末尾逐位查找，不提前消费标记
// 入参: h 霍夫曼码表
// 返回: byte 符号, error 解码错误
func (r *jpegEntropy) symbol(h *jpegHuffman) (byte, error) {
	if len(h.values) == 0 {
		return 0, fmt.Errorf("missing JPEG Huffman table")
	}
	code, length := 0, 1
	bits, available := r.bits, r.n
	if available < 8 && r.pos < len(r.data) {
		value := r.data[r.pos]
		if value != 0xff || r.pos+1 < len(r.data) && r.data[r.pos+1] == 0 {
			bits = bits<<8 | uint32(value)
			available += 8
		}
	}
	if available >= 8 {
		prefix := bits >> (available - 8) & 255
		entry := h.lookup[prefix]
		if n := uint(entry & 255); n != 0 {
			_, err := r.take(n)
			return byte(entry >> 8), err
		}
		if _, err := r.take(8); err != nil {
			return 0, err
		}
		code, length = int(prefix), 9
	}
	for ; length <= 16; length++ {
		bit, err := r.take(1)
		if err != nil {
			return 0, err
		}
		code = code<<1 | int(bit)
		if h.min[length] <= code && code <= h.max[length] {
			return h.values[h.base[length]+code-h.min[length]], nil
		}
	}
	return 0, fmt.Errorf("invalid JPEG Huffman code")
}

// walk 遍历量化系数符号，重编码候选超限时提前结束
// 入参: ctx 取消上下文, data JPEG数据, visit 符号访问回调, output 可选输出
// 返回: int 扫描后片段偏移, error 扫描错误
func (s *jpegScan) walk(ctx context.Context, data []byte, visit func(int, byte, uint32, uint), output *jpegBitWriter) (int, error) {
	r := jpegEntropy{data: data, pos: s.start}
	restart := byte(0)
	for mcu := 0; mcu < s.mcus; mcu++ {
		if output != nil && output.exceeded {
			return 0, ctx.Err()
		}
		if mcu%256 == 0 {
			if err := ctx.Err(); err != nil {
				return 0, err
			}
		}
		if mcu > 0 && s.restart > 0 && mcu%s.restart == 0 {
			r.n = 0
			for r.pos+1 < len(data) && data[r.pos] == 0xff && data[r.pos+1] == 0xff {
				r.pos++
			}
			if r.pos+2 > len(data) || data[r.pos] != 0xff || data[r.pos+1] != 0xd0+restart {
				return 0, fmt.Errorf("invalid JPEG restart marker")
			}
			r.pos += 2
			if output != nil {
				output.flush()
				output.write([]byte{0xff, 0xd0 + restart})
			}
			restart = (restart + 1) & 7
		}
		for i := 0; i < len(s.blocks); i += 2 {
			table := s.blocks[i]
			symbol, err := r.symbol(&s.tables[table])
			if err != nil || symbol > 11 {
				return 0, fmt.Errorf("invalid JPEG DC coefficient")
			}
			bits, err := r.take(uint(symbol))
			if err != nil {
				return 0, err
			}
			visit(table, symbol, bits, uint(symbol))
			table = s.blocks[i+1]
			for k := 1; k < 64; {
				symbol, err = r.symbol(&s.tables[table])
				if err != nil {
					return 0, err
				}
				n := uint(symbol & 15)
				if n > 10 || n == 0 && symbol != 0 && symbol != 0xf0 {
					return 0, fmt.Errorf("invalid JPEG AC coefficient")
				}
				bits, err = r.take(n)
				if err != nil {
					return 0, err
				}
				visit(table, symbol, bits, n)
				if symbol == 0 {
					break
				}
				k += int(symbol>>4) + 1
				if k > 64 {
					return 0, fmt.Errorf("invalid JPEG coefficient run")
				}
			}
		}
	}
	if err := jpegScanTail(ctx, data, r.pos); err != nil {
		return 0, err
	}
	return r.pos, nil
}

// jpegScanTail 校验最后扫描至结束标记之间的表、应用及注释片段，不丢弃原字节
// 入参: ctx 取消上下文, data JPEG编码, pos 扫描后偏移
// 返回: error 片段或取消错误
func jpegScanTail(ctx context.Context, data []byte, pos int) error {
	for pos < len(data) {
		if err := ctx.Err(); err != nil {
			return err
		}
		if data[pos] != 255 {
			return fmt.Errorf("invalid JPEG scan tail")
		}
		for pos < len(data) && data[pos] == 255 {
			if pos&4095 == 0 {
				if err := ctx.Err(); err != nil {
					return err
				}
			}
			pos++
		}
		if pos == len(data) {
			break
		}
		marker := data[pos]
		pos++
		if marker == 0xd9 {
			return ctx.Err()
		}
		if marker == 1 {
			continue
		}
		if !(marker >= 0xe0 && marker <= 0xef || marker == 0xfe || marker == 0xc4 || marker == 0xcc || marker == 0xdb || marker == 0xdd) {
			return fmt.Errorf("invalid JPEG scan tail marker")
		}
		if pos+2 > len(data) {
			break
		}
		size := int(binary.BigEndian.Uint16(data[pos:]))
		if size < 2 || size > len(data)-pos {
			return fmt.Errorf("invalid JPEG scan tail length")
		}
		pos += size
	}
	return fmt.Errorf("missing JPEG end marker")
}

// optimalJPEGHuffman 限制码长至16位并预留全1填充码
// 入参: frequency 符号频数
// 返回: jpegHuffman 优化码表
func optimalJPEGHuffman(frequency [256]uint64) jpegHuffman {
	var weights [513]uint64
	var parents [513]int
	var active []int
	var symbols []int
	for i, count := range frequency {
		if count > 0 {
			weights[i] = count
			active = append(active, i)
			symbols = append(symbols, i)
		}
	}
	if len(symbols) == 0 {
		return jpegHuffman{}
	}
	weights[256] = 1
	active = append(active, 256)
	next := 257
	for len(active) > 1 {
		slices.SortFunc(active, func(a, b int) int {
			if weights[a] < weights[b] {
				return -1
			}
			if weights[a] > weights[b] {
				return 1
			}
			return b - a
		})
		a, b := active[0], active[1]
		weights[next], parents[a], parents[b] = weights[a]+weights[b], next, next
		active = append(active[2:], next)
		next++
	}
	var counts [257]int
	for _, symbol := range append(slices.Clone(symbols), 256) {
		depth := 0
		for parents[symbol] != 0 {
			depth++
			symbol = parents[symbol]
		}
		counts[depth]++
	}
	for i := 256; i > 16; i-- {
		for counts[i] > 0 {
			j := i - 2
			for counts[j] == 0 {
				j--
			}
			counts[i] -= 2
			counts[i-1]++
			counts[j+1] += 2
			counts[j]--
		}
	}
	for i := 16; i > 0; i-- {
		if counts[i] > 0 {
			counts[i]--
			break
		}
	}
	slices.SortFunc(symbols, func(a, b int) int {
		if frequency[a] > frequency[b] {
			return -1
		}
		if frequency[a] < frequency[b] {
			return 1
		}
		return a - b
	})
	h := jpegHuffman{}
	for i := 1; i <= 16; i++ {
		h.counts[i-1] = byte(counts[i])
	}
	for _, symbol := range symbols {
		h.values = append(h.values, byte(symbol))
	}
	_ = h.prepare()
	return h
}

// put 写入高位优先的编码位
// 入参: value 编码值, n 位数
func (w *jpegBitWriter) put(value uint32, n uint) {
	if w.exceeded {
		return
	}
	w.bits = w.bits<<n | value&((1<<n)-1)
	w.n += n
	for w.n >= 8 {
		w.n -= 8
		b := byte(w.bits >> w.n)
		size := 1
		if b == 0xff {
			size++
		}
		if w.limit > 0 && size > w.limit-w.Len() {
			w.exceeded = true
			return
		}
		w.WriteByte(b)
		if b == 0xff {
			w.WriteByte(0)
		}
	}
}

// write 写入未转义的标记，超出候选上限时停止输出
// 入参: data 标记数据
func (w *jpegBitWriter) write(data []byte) {
	if w.exceeded {
		return
	}
	if w.limit > 0 && len(data) > w.limit-w.Len() {
		w.exceeded = true
		return
	}
	w.Write(data)
}

// flush 用全1位补齐最后一个字节
func (w *jpegBitWriter) flush() {
	if w.n > 0 && !w.exceeded {
		w.put((1<<(8-w.n))-1, 8-w.n)
	}
	w.bits = 0
}
