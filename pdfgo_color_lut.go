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

// iccLUT 保存ICC输入曲线、多维查找表及输出曲线
type iccLUT struct {
	input, output [][]float64
	grid          int
	values        []float64
	matrix        [9]float64
	precision     int
}

// iccLUTSpace 保存按渲染意图划分的设备与连接空间变换
type iccLUTSpace struct {
	components int
	pcs        string
	toPCS      [3]*iccLUT
	fromPCS    [3]*iccLUT
}

// parseICCLUTSpace 读取ICC双向查找表，保留不同渲染意图
// 入参: data 配置文件数据, tags 已校验的标签
// 返回: *iccLUTSpace 颜色变换, error 格式或能力错误
func parseICCLUTSpace(data []byte, tags map[string][]byte) (*iccLUTSpace, error) {
	s := &iccLUTSpace{pcs: string(data[20:24])}
	switch string(data[16:20]) {
	case "GRAY":
		s.components = 1
	case "RGB ":
		s.components = 3
	case "CMYK":
		s.components = 4
	default:
		return nil, &UnsupportedError{Feature: "ICC lookup table color model"}
	}
	for _, tag := range []string{"D2B0", "D2B1", "D2B2", "D2B3"} {
		if tags[tag] != nil {
			return nil, &UnsupportedError{Feature: "ICC multi-process transform"}
		}
	}
	for i := range s.toPCS {
		for _, direction := range []struct {
			name       string
			input, out int
			target     **iccLUT
		}{{fmt.Sprintf("A2B%d", i), s.components, 3, &s.toPCS[i]}, {fmt.Sprintf("B2A%d", i), 3, s.components, &s.fromPCS[i]}} {
			if tag := tags[direction.name]; tag != nil {
				lut, err := parseICCLUT(tag)
				if err != nil {
					return nil, fmt.Errorf("ICC %s: %w", direction.name, err)
				}
				if len(lut.input) != direction.input || len(lut.output) != direction.out {
					return nil, fmt.Errorf("invalid ICC %s channels", direction.name)
				}
				if s.pcs == "XYZ " && lut.precision == 8 {
					return nil, &UnsupportedError{Feature: "8-bit ICC XYZ encoding"}
				}
				if direction.name[0] == 'A' || s.pcs != "XYZ " {
					if lut.matrix != [9]float64{1, 0, 0, 0, 1, 0, 0, 0, 1} {
						return nil, fmt.Errorf("invalid ICC lookup matrix")
					}
				}
				*direction.target = lut
			}
		}
	}
	if s.toPCS[0] == nil {
		return nil, fmt.Errorf("missing ICC A2B0 transform")
	}
	return s, nil
}

// parseICCLUT 解析lut8Type和lut16Type，按实际数据长度校验维数
// 入参: data 标签数据
// 返回: *iccLUT 查找表, error 格式错误
func parseICCLUT(data []byte) (*iccLUT, error) {
	if len(data) < 48 {
		return nil, fmt.Errorf("invalid ICC lookup table header")
	}
	s := &iccLUT{grid: int(data[10]), precision: 8}
	width, offset, input, output := 1, 48, 256, 256
	switch string(data[:4]) {
	case "mft1":
	case "mft2":
		if len(data) < 52 {
			return nil, fmt.Errorf("invalid ICC lookup table header")
		}
		width, offset, s.precision = 2, 52, 16
		input, output = int(binary.BigEndian.Uint16(data[48:])), int(binary.BigEndian.Uint16(data[50:]))
		if input < 2 || input > 4096 || output < 2 || output > 4096 {
			return nil, fmt.Errorf("invalid ICC lookup curve size")
		}
	default:
		return nil, &UnsupportedError{Feature: "ICC lookup table type " + string(data[:4])}
	}
	in, out := int(data[8]), int(data[9])
	if in < 1 || in > 4 || out < 1 || out > 4 || s.grid < 2 {
		return nil, fmt.Errorf("invalid ICC lookup dimensions")
	}
	count := out
	for i := 0; i < in; i++ {
		if count > (len(data)-offset)/width/s.grid {
			return nil, fmt.Errorf("truncated ICC lookup grid")
		}
		count *= s.grid
	}
	if input*in+count+output*out > (len(data)-offset)/width {
		return nil, fmt.Errorf("truncated ICC lookup table")
	}
	for i := range s.matrix {
		s.matrix[i] = float64(int32(binary.BigEndian.Uint32(data[12+4*i:]))) / 65536
	}
	read := func(count int) []float64 {
		values := make([]float64, count)
		for i := range values {
			if width == 1 {
				values[i] = float64(data[offset]) / 255
			} else {
				values[i] = float64(binary.BigEndian.Uint16(data[offset:])) / 65535
			}
			offset += width
		}
		return values
	}
	s.input = make([][]float64, in)
	for i := range s.input {
		s.input[i] = read(input)
	}
	s.values = read(count)
	s.output = make([][]float64, out)
	for i := range s.output {
		s.output[i] = read(output)
	}
	return s, nil
}

// iccLookupCurve 线性插值输入或输出表，允许非单调曲线
// 入参: curve 查找表, x 单位输入
// 返回: float64 插值结果
func iccLookupCurve(curve []float64, x float64) float64 {
	x = math.Max(0, math.Min(1, x)) * float64(len(curve)-1)
	i := min(int(x), len(curve)-2)
	return curve[i] + (x-float64(i))*(curve[i+1]-curve[i])
}

// evaluate 按矩阵、输入表、多维线性插值和输出表求值
// 入参: values 单位输入分量
// 返回: [4]float64 单位输出分量
func (s *iccLUT) evaluate(values []float64) [4]float64 {
	var input [4]float64
	copy(input[:], values)
	if len(s.input) == 3 {
		v := input
		for i := 0; i < 3; i++ {
			input[i] = s.matrix[3*i]*v[0] + s.matrix[3*i+1]*v[1] + s.matrix[3*i+2]*v[2]
		}
	}
	var index [4]int
	var fraction [4]float64
	for i, curve := range s.input {
		x := iccLookupCurve(curve, input[i]) * float64(s.grid-1)
		index[i] = min(int(x), s.grid-2)
		fraction[i] = x - float64(index[i])
	}
	var result [4]float64
	for corner := 0; corner < 1<<len(s.input); corner++ {
		weight, offset := 1.0, 0
		for i := range s.input {
			bit := corner >> i & 1
			offset = offset*s.grid + index[i] + bit
			if bit == 0 {
				weight *= 1 - fraction[i]
			} else {
				weight *= fraction[i]
			}
		}
		for i := range s.output {
			result[i] += weight * s.values[offset*len(s.output)+i]
		}
	}
	for i, curve := range s.output {
		result[i] = iccLookupCurve(curve, result[i])
	}
	return result
}

// iccIntentIndex 选择ICC查找表的渲染意图
// 入参: intent PDF渲染意图
// 返回: int 标签序号, error 未支持的意图
func iccIntentIndex(intent Name) (int, error) {
	switch intent {
	case "", "RelativeColorimetric":
		return 1, nil
	case "Perceptual":
		return 0, nil
	case "Saturation":
		return 2, nil
	default:
		return 0, &UnsupportedError{Feature: "ICC rendering intent " + string(intent)}
	}
}

// xyz 将设备分量变换到D50连接空间，保持色度值而非显示亮度
// 入参: values 设备分量, intent 渲染意图
// 返回: [3]float64 D50色度, error 意图错误
func (s *iccLUTSpace) xyz(values []float64, intent Name) ([3]float64, error) {
	i, err := iccIntentIndex(intent)
	if err != nil {
		return [3]float64{}, err
	}
	lut := s.toPCS[i]
	if lut == nil {
		lut = s.toPCS[0]
	}
	v := lut.evaluate(values)
	if s.pcs == "XYZ " {
		return [3]float64{v[0] * 65535 / 32768, v[1] * 65535 / 32768, v[2] * 65535 / 32768}, nil
	}
	scale := 1.0
	if lut.precision == 16 {
		scale = 65535.0 / 65280
	}
	l, a, b := math.Min(100, v[0]*100*scale), v[1]*255*scale-128, v[2]*255*scale-128
	y := (l + 16) / 116
	inverse := func(v float64) float64 {
		if v > 6.0/29 {
			return v * v * v
		}
		return 3 * (6.0 / 29) * (6.0 / 29) * (v - 4.0/29)
	}
	return [3]float64{0.9642 * inverse(y+a/500), inverse(y), 0.8249 * inverse(y-b/200)}, nil
}

// iccXYZRGB 将D50色度适应到D65并编码为sRGB
// 入参: xyz D50色度
// 返回: [3]float64 单位sRGB分量
func iccXYZRGB(xyz [3]float64) [3]float64 {
	source, target := bradford([3]float64{0.9642, 1, 0.8249}), bradford([3]float64{0.95047, 1, 1.08883})
	s := calRGBSpace{gamma: [3]float64{1, 1, 1}, matrix: [9]float64{1, 0, 0, 0, 1, 0, 0, 0, 1}}
	for i := range s.adapt {
		s.adapt[i] = target[i] / source[i]
	}
	c := s.color(xyz[0], xyz[1], xyz[2])
	return [3]float64{float64(c.R) / 65535, float64(c.G) / 65535, float64(c.B) / 65535}
}
