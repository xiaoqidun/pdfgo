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

// iccSegmentedCurve 保存覆盖整个浮点定义域的分段曲线
type iccSegmentedCurve struct {
	breaks   []float64
	segments []iccCurveSegment
}

// iccCurveSegment 保存公式或采样段，采样起值取自前段
type iccCurveSegment struct {
	function              uint16
	parameters, samples   []float64
	lower, upper, initial float64
}

// parseICCSegmentedCurve 读取ICC浮点曲线和不递减断点
// 入参: data 曲线数据
// 返回: iccSegmentedCurve 曲线, error 格式错误
func parseICCSegmentedCurve(data []byte) (iccSegmentedCurve, error) {
	var c iccSegmentedCurve
	if len(data) < 12 || string(data[:4]) != "curf" {
		return c, fmt.Errorf("invalid ICC segmented curve header")
	}
	count := int(binary.BigEndian.Uint16(data[8:]))
	if count == 0 || count-1 > (len(data)-12)/4 {
		return c, fmt.Errorf("invalid ICC segmented curve count")
	}
	var err error
	c.breaks, err = iccFloatNumbers(data[12:], uint64(count-1))
	if err != nil {
		return c, err
	}
	for n := 1; n < len(c.breaks); n++ {
		if c.breaks[n] < c.breaks[n-1] {
			return c, fmt.Errorf("invalid ICC curve break points")
		}
	}
	data = data[12+4*(count-1):]
	for n := 0; n < count; n++ {
		if len(data) < 12 {
			return c, fmt.Errorf("truncated ICC curve segment")
		}
		s := iccCurveSegment{lower: math.Inf(-1), upper: math.Inf(1)}
		if n > 0 {
			s.lower = c.breaks[n-1]
		}
		if n < count-1 {
			s.upper = c.breaks[n]
		}
		length := 12
		switch string(data[:4]) {
		case "parf":
			s.function = binary.BigEndian.Uint16(data[8:])
			counts := [...]uint64{4, 5, 5}
			if int(s.function) >= len(counts) {
				return c, &UnsupportedError{Feature: "ICC curve segment function"}
			}
			s.parameters, err = iccFloatNumbers(data[12:], counts[s.function])
			length += 4 * int(counts[s.function])
		case "samf":
			if n == 0 || n == count-1 {
				return c, fmt.Errorf("sampled ICC curve endpoint segment")
			}
			size := uint64(binary.BigEndian.Uint32(data[8:]))
			if size == 0 {
				return c, fmt.Errorf("empty ICC curve sample segment")
			}
			s.samples, err = iccFloatNumbers(data[12:], size)
			if err == nil {
				length += 4 * int(size)
				s.initial = c.segments[n-1].evaluate(s.lower)
				if math.IsNaN(s.initial) || math.IsInf(s.initial, 0) || math.Abs(s.initial) > math.MaxFloat32 {
					return c, fmt.Errorf("undefined ICC curve sample start")
				}
			}
		default:
			return c, &UnsupportedError{Feature: "ICC curve segment " + string(data[:4])}
		}
		if err != nil {
			return c, err
		}
		c.segments = append(c.segments, s)
		data = data[length:]
	}
	return c, nil
}

// evaluate 选择首个包含输入的曲线段，不截断输入输出
// 入参: value 浮点输入
// 返回: float64 曲线值
func (c *iccSegmentedCurve) evaluate(value float64) float64 {
	n := sort.Search(len(c.breaks), func(n int) bool { return value <= c.breaks[n] })
	return c.segments[n].evaluate(value)
}

// evaluate 求值公式或采样段，采样段右端包含最后一个样本
// 入参: x 浮点输入
// 返回: float64 曲线值
func (s *iccCurveSegment) evaluate(x float64) float64 {
	if len(s.samples) != 0 {
		if x <= s.lower {
			return s.initial
		}
		if x >= s.upper {
			return s.samples[len(s.samples)-1]
		}
		position := (x - s.lower) / (s.upper - s.lower) * float64(len(s.samples))
		n := min(int(position), len(s.samples)-1)
		from := s.initial
		if n > 0 {
			from = s.samples[n-1]
		}
		return from + (position-float64(n))*(s.samples[n]-from)
	}
	p := s.parameters
	switch s.function {
	case 0:
		return math.Pow(p[1]*x+p[2], p[0]) + p[3]
	case 1:
		return p[1]*math.Log10(p[2]*math.Pow(x, p[0])+p[3]) + p[4]
	default:
		return p[0]*math.Pow(p[1], p[2]*x+p[3]) + p[4]
	}
}
