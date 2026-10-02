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

// jpegBitWriter 写出JPEG扫描位流及字节转义
type jpegBitWriter struct {
	bytes.Buffer
	bits uint32
	n    uint
}

// jpegScan 保存单扫描顺序JPEG的组件及重启间隔
type jpegScan struct {
	header  []byte
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
	scan, err := readJPEGScan(data)
	if err != nil || scan == nil {
		return data, err
	}
	var frequencies [8][256]uint64
	end, err := scan.walk(ctx, data, func(table int, symbol byte, bits uint32, n uint) {
		frequencies[table][symbol]++
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
	}
	var output jpegBitWriter
	output.Write(scan.header)
	output.Write([]byte{0xff, 0xc4, byte((dht.Len() + 2) >> 8), byte(dht.Len() + 2)})
	output.Write(dht.Bytes())
	output.Write(data[scan.sos:scan.start])
	_, err = scan.walk(ctx, data, func(table int, symbol byte, bits uint32, n uint) {
		code := &tables[table]
		output.put(uint32(code.codes[symbol]), uint(code.length[symbol]))
		output.put(bits, n)
	}, &output)
	if err != nil {
		return nil, err
	}
	output.flush()
	output.Write(data[end:])
	if output.Len() >= len(data) {
		return data, nil
	}
	return output.Bytes(), nil
}

// readJPEGScan 读取可优化的扫描头，其他合法JPEG编码留给原始字节直通
// 入参: data JPEG数据
// 返回: *jpegScan 扫描配置，nil表示无需优化, error 解析错误
func readJPEGScan(data []byte) (*jpegScan, error) {
	if len(data) < 4 || data[0] != 0xff || data[1] != 0xd8 {
		return nil, fmt.Errorf("invalid JPEG header")
	}
	s := &jpegScan{}
	var header bytes.Buffer
	header.Write(data[:2])
	type component struct{ id, h, v byte }
	var components []component
	width, height, maxH, maxV := 0, 0, 0, 0
	for pos := 2; pos+4 <= len(data); {
		begin := pos
		if data[pos] != 0xff {
			return nil, fmt.Errorf("invalid JPEG marker")
		}
		for pos < len(data) && data[pos] == 0xff {
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
			for len(value) > 0 {
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
			s.header = header.Bytes()
			return s, nil
		default:
			if marker >= 0xc1 && marker <= 0xcf && marker != 0xc4 {
				return nil, nil
			}
		}
		if marker != 0xc4 {
			header.Write(data[begin:stop])
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

// symbol 按规范霍夫曼表读取符号
// 入参: h 霍夫曼码表
// 返回: byte 符号, error 解码错误
func (r *jpegEntropy) symbol(h *jpegHuffman) (byte, error) {
	if len(h.values) == 0 {
		return 0, fmt.Errorf("missing JPEG Huffman table")
	}
	code := 0
	for length := 1; length <= 16; length++ {
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

// walk 扫描量化系数符号，统计与写出共用同一遍历
// 入参: ctx 取消上下文, data JPEG数据, visit 符号访问回调, output 可选输出
// 返回: int 结束标记偏移, error 扫描错误
func (s *jpegScan) walk(ctx context.Context, data []byte, visit func(int, byte, uint32, uint), output *jpegBitWriter) (int, error) {
	r := jpegEntropy{data: data, pos: s.start}
	restart := byte(0)
	for mcu := 0; mcu < s.mcus; mcu++ {
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
				output.Write([]byte{0xff, 0xd0 + restart})
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
	for r.pos+1 < len(data) && data[r.pos] == 0xff && data[r.pos+1] == 0xff {
		r.pos++
	}
	if r.pos+2 > len(data) || data[r.pos] != 0xff || data[r.pos+1] != 0xd9 {
		return 0, fmt.Errorf("missing JPEG end marker")
	}
	return r.pos, nil
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
	w.bits = w.bits<<n | value&((1<<n)-1)
	w.n += n
	for w.n >= 8 {
		w.n -= 8
		b := byte(w.bits >> w.n)
		w.WriteByte(b)
		if b == 0xff {
			w.WriteByte(0)
		}
	}
}

// flush 用全1位补齐最后一个字节
func (w *jpegBitWriter) flush() {
	if w.n > 0 {
		w.put((1<<(8-w.n))-1, 8-w.n)
	}
	w.bits = 0
}
