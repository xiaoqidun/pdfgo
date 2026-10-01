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
	"fmt"
	"io"
	"sort"
	"strings"
)

// faxNode 保存传真变长码的二叉解码节点
type faxNode struct {
	next [2]int
	run  int
}

// faxGroup3Reader 按行读取T.4编码，保留二维参考行和精确输入边界
type faxGroup3Reader struct {
	source                         io.Reader
	bits                           uint64
	available                      uint
	byte                           [1]byte
	width, height, rows, offset    int
	mixed, eol, align, invert, eob bool
	allowDamage, damage            int
	previousDamaged                bool
	done                           bool
	row, previous                  []byte
	changes                        []int
	pending                        []int
	err                            error
}

// faxCodes 使用ITU-TT.4表2、表3的终止码、补充码和扩展补充码
// https://www.itu.int/rec/T-REC-T.4/en
var faxCodes = [2][]faxNode{
	faxCodebook(`00110101 000111 0111 1000 1011 1100 1110 1111 10011 10100 00111 01000 001000 000011 110100 110101
	101010 101011 0100111 0001100 0001000 0010111 0000011 0000100 0101000 0101011 0010011 0100100 0011000 00000010 00000011 00011010
	00011011 00010010 00010011 00010100 00010101 00010110 00010111 00101000 00101001 00101010 00101011 00101100 00101101 00000100 00000101 00001010
	00001011 01010010 01010011 01010100 01010101 00100100 00100101 01011000 01011001 01011010 01011011 01001010 01001011 00110010 00110011 00110100
	11011 10010 010111 0110111 00110110 00110111 01100100 01100101 01101000 01100111 011001100 011001101 011010010 011010011 011010100 011010101
	011010110 011010111 011011000 011011001 011011010 011011011 010011000 010011001 010011010 011000 010011011`),
	faxCodebook(`0000110111 010 11 10 011 0011 0010 00011 000101 000100 0000100 0000101 0000111 00000100 00000111 000011000
	0000010111 0000011000 0000001000 00001100111 00001101000 00001101100 00000110111 00000101000 00000010111 00000011000 000011001010 000011001011 000011001100 000011001101 000001101000 000001101001
	000001101010 000001101011 000011010010 000011010011 000011010100 000011010101 000011010110 000011010111 000001101100 000001101101 000011011010 000011011011 000001010100 000001010101 000001010110 000001010111
	000001100100 000001100101 000001010010 000001010011 000000100100 000000110111 000000111000 000000100111 000000101000 000001011000 000001011001 000000101011 000000101100 000001011010 000001100110 000001100111
	0000001111 000011001000 000011001001 000001011011 000000110011 000000110100 000000110101 0000001101100 0000001101101 0000001001010 0000001001011 0000001001100 0000001001101 0000001110010 0000001110011 0000001110100
	0000001110101 0000001110110 0000001110111 0000001010010 0000001010011 0000001010100 0000001010101 0000001011010 0000001011011 0000001100100 0000001100101`),
}

// faxCodebook 将标准码字按行程长度构造成前缀树
// 入参: codes 终止码和补充码
// 返回: []faxNode 解码节点
func faxCodebook(codes string) []faxNode {
	words := strings.Fields(codes + ` 00000001000 00000001100 00000001101 000000010010 000000010011 000000010100 000000010101 000000010110 000000010111 000000011100 000000011101 000000011110 000000011111`)
	nodes := []faxNode{{run: -1}}
	for n, word := range words {
		index := 0
		for _, bit := range word {
			b := int(bit - '0')
			if nodes[index].next[b] == 0 {
				nodes[index].next[b] = len(nodes)
				nodes = append(nodes, faxNode{run: -1})
			}
			index = nodes[index].next[b]
		}
		run := n
		if n >= 64 && n < 91 {
			run = (n - 63) * 64
		} else if n >= 91 {
			run = 1792 + (n-91)*64
		}
		nodes[index].run = run
	}
	return nodes
}

// peek 逐字节取得编码位，不越过当前码字所需的输入边界
// 入参: count 所需位数
// 返回: uint64 未消费的编码位, error 输入错误
func (r *faxGroup3Reader) peek(count uint) (uint64, error) {
	for r.available < count {
		if _, err := io.ReadFull(r.source, r.byte[:]); err != nil {
			return 0, err
		}
		r.bits = r.bits<<8 | uint64(r.byte[0])
		r.available += 8
	}
	return r.bits >> (r.available - count) & (1<<count - 1), nil
}

// take 消费指定数量的编码位
// 入参: count 位数
// 返回: uint64 编码值, error 输入错误
func (r *faxGroup3Reader) take(count uint) (uint64, error) {
	v, err := r.peek(count)
	if err == nil {
		r.available -= count
		r.bits &= 1<<r.available - 1
	}
	return v, err
}

// marker 读取可选或必需的EOL及其前导填充位
// 入参: required 是否必须存在EOL
// 返回: bool 是否读取标记, error 编码错误
func (r *faxGroup3Reader) marker(required bool) (bool, error) {
	if !required {
		for count := uint(1); count < 12; count++ {
			v, err := r.peek(count)
			if err == io.EOF || err == io.ErrUnexpectedEOF {
				return false, nil
			}
			if err != nil {
				return false, err
			}
			if v != 0 {
				return false, nil
			}
		}
	}
	v, err := r.peek(12)
	if err != nil {
		if !required && (err == io.EOF || err == io.ErrUnexpectedEOF) {
			return false, nil
		}
		return false, err
	}
	if v > 1 {
		if required {
			return false, fmt.Errorf("missing CCITT end-of-line marker")
		}
		return false, nil
	}
	for {
		bit, err := r.take(1)
		if err != nil {
			return false, err
		}
		if bit != 0 {
			return true, nil
		}
	}
}

// run 解码一个颜色行程，累加补充码直到终止码
// 入参: black 是否读取黑色行程, remaining 剩余像素数
// 返回: int 行程长度或未压缩模式标记-1, error 编码错误
func (r *faxGroup3Reader) run(black bool, remaining int) (int, error) {
	book := faxCodes[0]
	if black {
		book = faxCodes[1]
	}
	total := 0
	for {
		node, code, length := 0, uint64(0), 0
		for book[node].run < 0 {
			bit, err := r.take(1)
			if err != nil {
				return 0, err
			}
			code, length = code<<1|bit, length+1
			if node != 0 || length == 1 {
				node = book[node].next[bit]
			}
			if node == 0 {
				if code == 0 && length < 9 {
					continue
				}
				if code == 1 && length == 9 && total == 0 {
					extension, err := r.peek(3)
					if err != nil {
						return 0, err
					}
					if extension == 7 {
						_, _ = r.take(3)
						return -1, nil
					}
					return 0, &UnsupportedError{Feature: "CCITT one-dimensional extension"}
				}
				return 0, fmt.Errorf("invalid CCITT run code")
			}
		}
		n := book[node].run
		if n > remaining-total {
			return 0, fmt.Errorf("CCITT run exceeds row width")
		}
		total += n
		if n < 64 {
			return total, nil
		}
	}
}

// mode 解码T.4二维模式，水平为4、跳过为5、未压缩为6，其余为垂直偏移
// 返回: int 模式, error 编码错误
func (r *faxGroup3Reader) mode() (int, error) {
	code := uint64(0)
	for n := 1; n <= 7; n++ {
		bit, err := r.take(1)
		if err != nil {
			return 0, err
		}
		code = code<<1 | bit
		switch {
		case n == 1 && code == 1:
			return 0, nil
		case n == 3 && code == 1:
			return 4, nil
		case n == 3 && code >= 2:
			return int(code)*2 - 5, nil
		case n == 4 && code == 1:
			return 5, nil
		case n == 6 && code >= 2:
			return int(code)*4 - 10, nil
		case n == 7 && code >= 2:
			return int(code)*6 - 15, nil
		case n == 7 && code == 1:
			extension, err := r.peek(3)
			if err != nil {
				return 0, err
			}
			if extension == 7 {
				_, _ = r.take(3)
				return 6, nil
			}
			return 0, &UnsupportedError{Feature: "CCITT two-dimensional extension"}
		}
	}
	return 0, fmt.Errorf("invalid CCITT two-dimensional code")
}

// uncompressed 读取T.4表5的图像片段和退出颜色标记
// 入参: x 当前像素, black 当前行程颜色, changes 已解码的变化位置
// 返回: int 下一像素, bool 下一行程颜色, []int 变化位置, error 编码错误
func (r *faxGroup3Reader) uncompressed(x int, black bool, changes []int) (int, bool, []int, error) {
	for {
		zeros := 0
		for {
			bit, err := r.take(1)
			if err != nil {
				return x, black, changes, err
			}
			if bit != 0 {
				break
			}
			zeros++
			if zeros > 10 {
				return x, black, changes, fmt.Errorf("invalid CCITT uncompressed code")
			}
		}
		white := zeros
		if zeros >= 6 {
			white -= 6
		}
		if white > r.width-x || zeros < 5 && white == r.width-x {
			return x, black, changes, fmt.Errorf("CCITT uncompressed data exceeds row width")
		}
		if white > 0 {
			if black {
				changes = append(changes, x)
				black = false
			}
			x += white
		}
		if zeros < 5 {
			if !black {
				changes = append(changes, x)
				black = true
			}
			r.black(x, x+1)
			x++
		}
		if zeros >= 6 {
			tag, err := r.take(1)
			if err != nil {
				return x, black, changes, err
			}
			if black != (tag != 0) {
				changes = append(changes, x)
			}
			return x, tag != 0, changes, nil
		}
	}
}

// black 将黑色行程写入逐行字节对齐的图像样本
// 入参: start 起始像素, end 结束像素
func (r *faxGroup3Reader) black(start, end int) {
	for start < end && start%8 != 0 {
		r.row[start/8] &^= 1 << (7 - start%8)
		start++
	}
	for start+8 <= end {
		r.row[start/8] = 0
		start += 8
	}
	for start < end {
		r.row[start/8] &^= 1 << (7 - start%8)
		start++
	}
}

// decodeRow 按一维行程或二维参考行生成当前行
// 入参: oneD 是否为一维编码
// 返回: []int 当前行变化位置, error 编码错误
func (r *faxGroup3Reader) decodeRow(oneD bool) ([]int, error) {
	changes := r.pending[:0]
	x, black := 0, false
	for steps := 0; x < r.width; steps++ {
		if steps > 2*r.width+2 {
			return nil, fmt.Errorf("CCITT row does not advance")
		}
		if oneD {
			length, err := r.run(black, r.width-x)
			if err != nil {
				return nil, err
			}
			if length < 0 {
				x, black, changes, err = r.uncompressed(x, black, changes)
				if err != nil {
					return nil, err
				}
				continue
			}
			if black {
				r.black(x, x+length)
			}
			x += length
			changes = append(changes, x)
			black = !black
			continue
		}
		index := sort.Search(len(r.changes), func(n int) bool { return r.changes[n] > x || steps == 0 && r.changes[n] == x })
		if (index%2 == 1) != black {
			index++
		}
		b1, b2 := r.width, r.width
		if index < len(r.changes) {
			b1 = r.changes[index]
		}
		if index+1 < len(r.changes) {
			b2 = r.changes[index+1]
		}
		mode, err := r.mode()
		if err != nil {
			return nil, err
		}
		end := b1 + mode
		switch mode {
		case 6:
			x, black, changes, err = r.uncompressed(x, black, changes)
			if err != nil {
				return nil, err
			}
			continue
		case 4:
			first, err := r.run(black, r.width-x)
			if err != nil {
				return nil, err
			}
			if first < 0 {
				return nil, fmt.Errorf("CCITT extension inside horizontal run")
			}
			second, err := r.run(!black, r.width-x-first)
			if err != nil {
				return nil, err
			}
			if second < 0 {
				return nil, fmt.Errorf("CCITT extension inside horizontal run")
			}
			if black {
				r.black(x, x+first)
			} else {
				r.black(x+first, x+first+second)
			}
			x += first + second
			changes = append(changes, x-second, x)
			continue
		case 5:
			end = b2
		}
		if end < x || end > r.width {
			return nil, fmt.Errorf("invalid CCITT vertical or pass position")
		}
		if black {
			r.black(x, end)
		}
		x = end
		if mode != 5 {
			changes = append(changes, x)
			black = !black
		}
	}
	if len(changes) == 0 || changes[len(changes)-1] != r.width {
		changes = append(changes, r.width)
	}
	return changes, nil
}

// nextRow 处理行边界、RTC和标准允许的坏行替换
// 返回: error 编码结束或错误
func (r *faxGroup3Reader) nextRow() (result error) {
	defer func() {
		if result == io.EOF && !r.done {
			result = io.ErrUnexpectedEOF
		}
	}()
	if !r.eob && r.rows >= r.height {
		r.done = true
		return io.EOF
	}
	if r.align {
		if _, err := r.take(r.available % 8); err != nil {
			return err
		}
	}
	marked, err := r.marker(r.eol || r.mixed)
	if err != nil {
		return err
	}
	oneD := true
	if r.mixed {
		bit, err := r.take(1)
		if err != nil {
			return err
		}
		oneD = bit != 0
	}
	if marked {
		for markers := 1; markers < 6; markers++ {
			more, err := r.marker(false)
			if err != nil {
				return err
			}
			if !more {
				if markers != 1 {
					return fmt.Errorf("incomplete CCITT return-to-control marker")
				}
				break
			}
			if r.mixed {
				bit, err := r.take(1)
				if err != nil || bit != 1 || !oneD {
					return fmt.Errorf("invalid CCITT return-to-control tag")
				}
			}
			if markers == 5 {
				r.done = true
				return io.EOF
			}
		}
	}
	for n := range r.row {
		r.row[n] = 255
	}
	changes, err := r.decodeRow(oneD)
	if err != nil {
		if !r.eol || r.damage >= r.allowDamage {
			return err
		}
		for {
			v, boundaryErr := r.peek(12)
			if boundaryErr != nil {
				return boundaryErr
			}
			if v == 1 {
				break
			}
			if _, boundaryErr := r.take(1); boundaryErr != nil {
				return boundaryErr
			}
		}
		r.damage++
		if r.previousDamaged || r.rows == 0 {
			for n := range r.row {
				r.row[n] = 255
			}
			r.changes = []int{r.width}
		} else {
			copy(r.row, r.previous)
		}
		r.previousDamaged = true
	} else {
		r.previousDamaged = false
		r.pending, r.changes = r.changes, changes
	}
	copy(r.previous, r.row)
	if r.invert {
		for n := range r.row {
			r.row[n] = ^r.row[n]
		}
	}
	r.rows++
	r.offset = 0
	return nil
}

// Read 输出逐行字节对齐的传真图像样本
// 入参: p 输出缓冲区
// 返回: int 输出字节数, error 编码结束或错误
func (r *faxGroup3Reader) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	if r.offset == len(r.row) && r.err == nil {
		r.err = r.nextRow()
	}
	if r.err != nil {
		return 0, r.err
	}
	n := copy(p, r.row[r.offset:])
	r.offset += n
	return n, nil
}
