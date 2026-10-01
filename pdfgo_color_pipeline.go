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
)

// iccPipeline 保存ICC.1的mAB、mBA曲线、矩阵和非均匀网格
type iccPipeline struct {
	a, b, m []iccToneCurve
	matrix  []float64
	grid    []int
	values  []float64
	input   int
	output  int
	reverse bool
}

// parseICCPipeline 解析ICC.1第10.12、10.13节的多阶段查找表
// 入参: data 标签数据
// 返回: *iccLUT 颜色变换, error 格式错误
func parseICCPipeline(data []byte) (*iccLUT, error) {
	if len(data) < 32 || string(data[:4]) != "mAB " && string(data[:4]) != "mBA " {
		return nil, fmt.Errorf("invalid ICC pipeline header")
	}
	s := &iccPipeline{input: int(data[8]), output: int(data[9]), reverse: string(data[:4]) == "mBA "}
	if s.input < 1 || s.input > 4 || s.output < 1 || s.output > 4 {
		return nil, fmt.Errorf("invalid ICC pipeline channels")
	}
	var offsets [5]int
	for n := range offsets {
		value := uint64(binary.BigEndian.Uint32(data[12+4*n:]))
		if value != 0 && (value < 32 || value%4 != 0 || value >= uint64(len(data))) {
			return nil, fmt.Errorf("invalid ICC pipeline offset")
		}
		offsets[n] = int(value)
	}
	if offsets[0] == 0 || (offsets[1] == 0) != (offsets[2] == 0) || (offsets[3] == 0) != (offsets[4] == 0) || offsets[3] == 0 && s.input != s.output {
		return nil, fmt.Errorf("invalid ICC pipeline stage combination")
	}
	aCount, bCount := s.input, s.output
	if s.reverse {
		aCount, bCount = s.output, s.input
	}
	var err error
	curvesAt := func(offset, count int) ([]iccToneCurve, error) {
		end := len(data)
		for _, boundary := range []int{offsets[1], offsets[3]} {
			if boundary > offset && boundary < end {
				end = boundary
			}
		}
		return iccEmbeddedCurves(data[offset:end], count)
	}
	s.b, err = curvesAt(offsets[0], bCount)
	if err != nil {
		return nil, err
	}
	if offsets[1] != 0 {
		if bCount != 3 || len(data)-offsets[1] < 48 {
			return nil, fmt.Errorf("invalid ICC pipeline matrix")
		}
		for n, offset := range offsets {
			if n != 1 && offset >= offsets[1] && offset < offsets[1]+48 {
				return nil, fmt.Errorf("overlapping ICC pipeline matrix")
			}
		}
		s.matrix = make([]float64, 12)
		for n := range s.matrix {
			s.matrix[n] = float64(int32(binary.BigEndian.Uint32(data[offsets[1]+4*n:]))) / 65536
		}
		s.m, err = curvesAt(offsets[2], 3)
		if err != nil {
			return nil, err
		}
	}
	if offsets[3] != 0 {
		s.a, err = curvesAt(offsets[4], aCount)
		if err != nil {
			return nil, err
		}
		clut := data[offsets[3]:]
		if len(clut) < 20 || clut[16] != 1 && clut[16] != 2 {
			return nil, fmt.Errorf("invalid ICC pipeline CLUT header")
		}
		width, count := int(clut[16]), s.output
		s.grid = make([]int, s.input)
		for n := range s.grid {
			s.grid[n] = int(clut[n])
			if s.grid[n] < 2 || count > (len(clut)-20)/width/s.grid[n] {
				return nil, fmt.Errorf("invalid ICC pipeline CLUT dimensions")
			}
			count *= s.grid[n]
		}
		for n, offset := range offsets {
			if n != 3 && offset >= offsets[3] && offset < offsets[3]+20+count*width {
				return nil, fmt.Errorf("overlapping ICC pipeline CLUT")
			}
		}
		s.values = make([]float64, count)
		for n := range s.values {
			if width == 1 {
				s.values[n] = float64(clut[20+n]) / 255
			} else {
				s.values[n] = float64(binary.BigEndian.Uint16(clut[20+2*n:])) / 65535
			}
		}
	}
	return &iccLUT{input: make([][]float64, s.input), output: make([][]float64, s.output), precision: 16, pipeline: s}, nil
}

// iccEmbeddedCurves 按曲线自身长度读取四字节对齐的连续曲线，允许共享数据
// 入参: data 曲线数据, count 曲线数
// 返回: []iccToneCurve 色调曲线, error 格式错误
func iccEmbeddedCurves(data []byte, count int) ([]iccToneCurve, error) {
	curves := make([]iccToneCurve, count)
	for n := range curves {
		if len(data) < 12 {
			return nil, fmt.Errorf("truncated ICC embedded curve")
		}
		length := uint64(12)
		switch string(data[:4]) {
		case "curv":
			length += 2 * uint64(binary.BigEndian.Uint32(data[8:]))
		case "para":
			counts := [...]int{1, 3, 4, 5, 7}
			kind := int(binary.BigEndian.Uint16(data[8:]))
			if kind >= len(counts) {
				return nil, fmt.Errorf("invalid ICC embedded parametric curve")
			}
			length += uint64(4 * counts[kind])
		default:
			return nil, &UnsupportedError{Feature: "ICC embedded curve type " + string(data[:4])}
		}
		if length > uint64(len(data)) {
			return nil, fmt.Errorf("truncated ICC embedded curve")
		}
		curve, err := iccCurve(data[:length])
		if err != nil {
			return nil, err
		}
		curves[n] = curve
		if n+1 < count {
			length = (length + 3) &^ 3
			if length > uint64(len(data)) {
				return nil, fmt.Errorf("truncated ICC curve padding")
			}
			data = data[length:]
		}
	}
	return curves, nil
}

// evaluate 按mAB或mBA规定的顺序执行曲线、网格和带偏移矩阵
// 入参: values 单位输入分量
// 返回: [4]float64 单位输出分量
func (s *iccPipeline) evaluate(values []float64) [4]float64 {
	var v [4]float64
	copy(v[:], values)
	curves := func(set []iccToneCurve) {
		for n, c := range set {
			v[n] = c.evaluate(v[n])
		}
	}
	matrix := func() {
		if len(s.matrix) != 0 {
			in := v
			for n := 0; n < 3; n++ {
				v[n] = s.matrix[3*n]*in[0] + s.matrix[3*n+1]*in[1] + s.matrix[3*n+2]*in[2] + s.matrix[9+n]
			}
		}
	}
	if !s.reverse {
		curves(s.a)
		v = s.lookup(v)
		curves(s.m)
		matrix()
		curves(s.b)
	} else {
		curves(s.b)
		matrix()
		curves(s.m)
		v = s.lookup(v)
		curves(s.a)
	}
	return v
}

// lookup 对各维独立网格执行多线性插值，缺少网格时保留输入
// 入参: input 单位输入分量
// 返回: [4]float64 插值结果
func (s *iccPipeline) lookup(input [4]float64) [4]float64 {
	if len(s.grid) == 0 {
		return input
	}
	var index [4]int
	var fraction [4]float64
	for n, size := range s.grid {
		x := math.Max(0, math.Min(1, input[n])) * float64(size-1)
		index[n] = min(int(x), size-2)
		fraction[n] = x - float64(index[n])
	}
	var result [4]float64
	for corner := 0; corner < 1<<len(s.grid); corner++ {
		weight, offset := 1.0, 0
		for n, size := range s.grid {
			bit := corner >> n & 1
			offset = offset*size + index[n] + bit
			if bit == 0 {
				weight *= 1 - fraction[n]
			} else {
				weight *= fraction[n]
			}
		}
		for n := 0; n < s.output; n++ {
			result[n] += weight * s.values[offset*s.output+n]
		}
	}
	return result
}
