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
	"encoding/binary"
	"fmt"
	"math"
	"sort"
)

// iccProcessElements 保存ICC浮点多阶段变换
type iccProcessElements struct {
	input, output, channels int
	elements                []iccProcessElement
}

// iccProcessElement 保存曲线组、矩阵、查找表或扩展占位元素
type iccProcessElement struct {
	kind          string
	input, output int
	curves        []iccSegmentedCurve
	values        []float64
	grid          []int
}

// parseICCProcessElements 按ICC.1第10.16节读取浮点多阶段变换
// 入参: data 标签数据
// 返回: *iccProcessElements 变换, error 格式或类型错误
func parseICCProcessElements(data []byte) (*iccProcessElements, error) {
	if len(data) < 16 || string(data[:4]) != "mpet" {
		return nil, fmt.Errorf("invalid ICC process header")
	}
	s := &iccProcessElements{input: int(binary.BigEndian.Uint16(data[8:])), output: int(binary.BigEndian.Uint16(data[10:]))}
	if s.input == 0 || s.output == 0 {
		return nil, fmt.Errorf("invalid ICC process channels")
	}
	blocks, err := iccProcessBlocks(data, 16, uint64(binary.BigEndian.Uint32(data[12:])))
	if err != nil {
		return nil, err
	}
	channels := s.input
	s.channels = max(s.input, s.output)
	shared := make(map[*byte]iccProcessElement)
	for _, block := range blocks {
		e, found := shared[&block[0]]
		if !found {
			e, err = parseICCProcessElement(block)
			if err != nil {
				return nil, err
			}
			shared[&block[0]] = e
		}
		if e.input != channels {
			return nil, fmt.Errorf("inconsistent ICC process channels")
		}
		channels = e.output
		s.channels = max(s.channels, channels)
		s.elements = append(s.elements, e)
	}
	if channels != s.output {
		return nil, fmt.Errorf("inconsistent ICC process output")
	}
	return s, nil
}

// iccProcessBlocks 校验位置表和共享数据边界，拒绝部分重叠
// 入参: data 容器数据, start 位置表起点, count 数据块数
// 返回: [][]byte 顺序数据块, error 边界错误
func iccProcessBlocks(data []byte, start int, count uint64) ([][]byte, error) {
	if count == 0 || count > uint64(len(data)-start)/8 {
		return nil, fmt.Errorf("invalid ICC process position table")
	}
	end := uint64(start) + 8*count
	positions := make([][2]uint64, int(count))
	blocks := make([][]byte, int(count))
	for n := range blocks {
		entry := data[start+8*n:]
		offset, size := uint64(binary.BigEndian.Uint32(entry)), uint64(binary.BigEndian.Uint32(entry[4:]))
		if offset < end || offset%4 != 0 || offset > uint64(len(data)) || size < 12 || size > uint64(len(data))-offset {
			return nil, fmt.Errorf("invalid ICC process bounds")
		}
		positions[n] = [2]uint64{offset, offset + size}
		blocks[n] = data[offset : offset+size]
	}
	sort.Slice(positions, func(i, j int) bool { return positions[i][0] < positions[j][0] })
	for n := 1; n < len(positions); n++ {
		if positions[n][0] < positions[n-1][1] && positions[n] != positions[n-1] {
			return nil, fmt.Errorf("overlapping ICC process data")
		}
	}
	return blocks, nil
}

// parseICCProcessElement 读取单个浮点处理元素
// 入参: data 元素数据
// 返回: iccProcessElement 元素, error 格式或类型错误
func parseICCProcessElement(data []byte) (iccProcessElement, error) {
	e := iccProcessElement{kind: string(data[:4]), input: int(binary.BigEndian.Uint16(data[8:])), output: int(binary.BigEndian.Uint16(data[10:]))}
	if e.input == 0 || e.output == 0 {
		return e, fmt.Errorf("invalid ICC process element channels")
	}
	var err error
	switch e.kind {
	case "cvst":
		if e.input != e.output {
			return e, fmt.Errorf("invalid ICC curve set channels")
		}
		blocks, err := iccProcessBlocks(data, 12, uint64(e.input))
		if err != nil {
			return e, err
		}
		e.curves = make([]iccSegmentedCurve, e.input)
		shared := make(map[*byte]iccSegmentedCurve)
		for n, block := range blocks {
			curve, found := shared[&block[0]]
			if !found {
				curve, err = parseICCSegmentedCurve(block)
				if err != nil {
					return e, err
				}
				shared[&block[0]] = curve
			}
			e.curves[n] = curve
		}
	case "matf":
		e.values, err = iccFloatNumbers(data[12:], uint64(e.output)*(uint64(e.input)+1))
	case "clut":
		if e.input > 16 || len(data) < 28 {
			return e, fmt.Errorf("invalid ICC process grid header")
		}
		count := uint64(e.output)
		e.grid = make([]int, e.input)
		for n := range e.grid {
			e.grid[n] = int(data[12+n])
			if e.grid[n] < 2 || count > uint64(len(data)-28)/4/uint64(e.grid[n]) {
				return e, fmt.Errorf("invalid ICC process grid dimensions")
			}
			count *= uint64(e.grid[n])
		}
		e.values, err = iccFloatNumbers(data[28:], count)
	case "bACS", "eACS":
		if e.input != e.output || len(data) < 16 {
			return e, fmt.Errorf("invalid ICC process expansion element")
		}
	default:
		return e, &UnsupportedError{Feature: "ICC process element " + e.kind}
	}
	return e, err
}

// iccFloatNumbers 读取有限的IEEE754单精度数据
// 入参: data 数据, count 分量数
// 返回: []float64 分量, error 长度或数值错误
func iccFloatNumbers(data []byte, count uint64) ([]float64, error) {
	if count > uint64(len(data))/4 {
		return nil, fmt.Errorf("truncated ICC floating point data")
	}
	values := make([]float64, int(count))
	for n := range values {
		values[n] = float64(math.Float32frombits(binary.BigEndian.Uint32(data[4*n:])))
		if math.IsNaN(values[n]) || math.IsInf(values[n], 0) {
			return nil, fmt.Errorf("invalid ICC floating point value")
		}
	}
	return values, nil
}

// evaluate 顺序执行浮点元素，不截断中间结果
// 入参: values 输入分量
// 返回: [4]float64 输出分量, error 求值错误
func (s *iccProcessElements) evaluate(values []float64) ([4]float64, error) {
	var result [4]float64
	if len(values) != s.input || s.output > len(result) {
		return result, fmt.Errorf("invalid ICC process component count")
	}
	var local [32]float64
	buffer := local[:]
	if s.channels > len(local)/2 {
		buffer = make([]float64, 2*s.channels)
	}
	input, output := buffer[:s.channels], buffer[s.channels:2*s.channels]
	copy(input, values)
	for _, e := range s.elements {
		switch e.kind {
		case "cvst":
			for n, curve := range e.curves {
				output[n] = curve.evaluate(input[n])
			}
		case "matf":
			for n := 0; n < e.output; n++ {
				v := e.values[e.input*e.output+n]
				for j := 0; j < e.input; j++ {
					v += e.values[n*e.input+j] * input[j]
				}
				output[n] = v
			}
		case "clut":
			e.lookup(input, output)
		default:
			copy(output, input[:e.input])
		}
		for _, v := range output[:e.output] {
			if math.IsNaN(v) || math.IsInf(v, 0) || math.Abs(v) > math.MaxFloat32 {
				return result, fmt.Errorf("undefined ICC process result")
			}
		}
		input, output = output, input
	}
	copy(result[:], input[:s.output])
	return result, nil
}

// lookup 按独立网格插值，仅将输入限制在单位区间
// 入参: input 输入分量, output 输出缓冲
func (e *iccProcessElement) lookup(input, output []float64) {
	var index [16]int
	var fraction [16]float64
	for n, size := range e.grid {
		x := math.Max(0, math.Min(1, input[n])) * float64(size-1)
		index[n] = min(int(x), size-2)
		fraction[n] = x - float64(index[n])
	}
	clear(output[:e.output])
	for corner := 0; corner < 1<<len(e.grid); corner++ {
		weight, offset := 1.0, 0
		for n, size := range e.grid {
			bit := corner >> n & 1
			offset = offset*size + index[n] + bit
			if bit == 0 {
				weight *= 1 - fraction[n]
			} else {
				weight *= fraction[n]
			}
		}
		if weight == 0 {
			continue
		}
		for n := 0; n < e.output; n++ {
			output[n] += weight * e.values[offset*e.output+n]
		}
	}
}
