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
	"image/color"
	"math"
)

// iccRGBSpace 保存RGB矩阵曲线配置文件到sRGB的变换
type iccRGBSpace struct {
	matrix calRGBSpace
	curves [3][]uint16
}

// iccGraySpace 保存灰度曲线及D50连接空间到sRGB的变换
type iccGraySpace struct {
	matrix calRGBSpace
	curve  []uint16
}

// validateRGBGroupSpace 检查混合空间与sRGB的偏差是否在8位量化精度内
// 入参: object 组颜色空间
// 返回: error 不支持的混合空间或解析错误
func (r *Reader) validateRGBGroupSpace(object Object) error {
	value, err := r.Resolve(object)
	if err != nil {
		return err
	}
	if value == Name("DeviceRGB") {
		return nil
	}
	a, ok := value.(Array)
	if !ok || len(a) != 2 || a[0] != Name("ICCBased") {
		return &UnsupportedError{Feature: "group color space"}
	}
	s, err := r.readICCRGB(a)
	if err != nil {
		return err
	}
	for channel := 0; channel < 3; channel++ {
		for i := 0; i <= 256; i++ {
			input := [3]float64{}
			input[channel] = float64(i) / 256
			output, err := s.color(input[:], "RelativeColorimetric")
			if err != nil {
				return err
			}
			for j := range output {
				if math.Abs(output[j]-input[j]) > 1.0/255 {
					return &UnsupportedError{Feature: "non-sRGB group profile"}
				}
			}
		}
	}
	for red := 0; red <= 4; red++ {
		for green := 0; green <= 4; green++ {
			for blue := 0; blue <= 4; blue++ {
				input := [3]float64{float64(red) / 4, float64(green) / 4, float64(blue) / 4}
				output, err := s.color(input[:], "RelativeColorimetric")
				if err != nil {
					return err
				}
				for i := range output {
					if math.Abs(output[i]-input[i]) > 1.0/255 {
						return &UnsupportedError{Feature: "non-sRGB group profile"}
					}
				}
			}
		}
	}
	return nil
}

// readICCRGB 解析基于矩阵和曲线的ICC颜色空间，不以备用空间替代配置文件
// 入参: object ICCBased颜色空间数组
// 返回: *iccRGBSpace 颜色变换, error 错误信息
func (r *Reader) readICCRGB(object Array) (*iccRGBSpace, error) {
	data, err := r.readICCProfile(object, 3)
	if err != nil {
		return nil, err
	}
	return parseICCRGB(data)
}

// readICCGray 解析ICC灰度配置文件
// 入参: object ICCBased颜色空间数组
// 返回: *iccGraySpace 灰度颜色变换, error 错误信息
func (r *Reader) readICCGray(object Array) (*iccGraySpace, error) {
	data, err := r.readICCProfile(object, 1)
	if err != nil {
		return nil, err
	}
	return parseICCGray(data)
}

// readICCProfile 读取并检查ICC颜色空间的配置文件流
// 入参: object ICCBased颜色空间数组, components 分量数
// 返回: []byte 配置文件数据, error 错误信息
func (r *Reader) readICCProfile(object Array, components int64) ([]byte, error) {
	if len(object) != 2 || object[0] != Name("ICCBased") {
		return nil, fmt.Errorf("invalid ICCBased color space")
	}
	value, err := r.Resolve(object[1])
	if err != nil {
		return nil, err
	}
	stream, ok := value.(*Stream)
	if !ok {
		return nil, fmt.Errorf("invalid ICC profile stream")
	}
	count, ok := stream.Dictionary["N"].(Integer)
	if !ok {
		return nil, fmt.Errorf("invalid ICC component count")
	}
	if count != Integer(components) {
		return nil, &UnsupportedError{Feature: "ICC component count"}
	}
	if v := stream.Dictionary["Range"]; v != nil {
		v, err = r.Resolve(v)
		if err != nil {
			return nil, err
		}
		a, ok := v.(Array)
		if !ok {
			return nil, fmt.Errorf("invalid ICC component range")
		}
		n, err := numbers(a, int(components)*2)
		if err != nil {
			return nil, err
		}
		for i, v := range n {
			if v != float64(i%2) {
				return nil, &UnsupportedError{Feature: "ICC component range"}
			}
		}
	}
	return stream.Decode()
}

// parseICCRGB 校验ICC标签边界并读取D50矩阵和色调曲线
// 入参: data 配置文件数据
// 返回: *iccRGBSpace 颜色变换, error 错误信息
func parseICCRGB(data []byte) (*iccRGBSpace, error) {
	tags, err := iccTags(data, "RGB ")
	if err != nil {
		return nil, err
	}
	s := &iccRGBSpace{matrix: calRGBSpace{gamma: [3]float64{1, 1, 1}}}
	source, target := bradford([3]float64{0.9642, 1, 0.8249}), bradford([3]float64{0.95047, 1, 1.08883})
	for i, channel := range []string{"r", "g", "b"} {
		s.matrix.adapt[i] = target[i] / source[i]
		xyz := tags[channel+"XYZ"]
		if len(xyz) != 20 || string(xyz[:4]) != "XYZ " {
			return nil, &UnsupportedError{Feature: "ICC colorant matrix"}
		}
		for j := 0; j < 3; j++ {
			s.matrix.matrix[i*3+j] = float64(int32(binary.BigEndian.Uint32(xyz[8+4*j:]))) / 65536
		}
		s.curves[i], err = iccCurve(tags[channel+"TRC"])
		if err != nil {
			return nil, err
		}
	}
	return s, nil
}

// parseICCGray 读取灰度曲线，使用ICC定义的D50连接空间
// 入参: data 配置文件数据
// 返回: *iccGraySpace 灰度颜色变换, error 错误信息
func parseICCGray(data []byte) (*iccGraySpace, error) {
	tags, err := iccTags(data, "GRAY")
	if err != nil {
		return nil, err
	}
	white := tags["wtpt"]
	if len(white) != 20 || string(white[:4]) != "XYZ " {
		return nil, &UnsupportedError{Feature: "ICC gray white point"}
	}
	curve, err := iccCurve(tags["kTRC"])
	if err != nil {
		return nil, err
	}
	s := &iccGraySpace{curve: curve, matrix: calRGBSpace{gamma: [3]float64{1, 1, 1}, matrix: [9]float64{0.9642, 0, 0, 0, 1, 0, 0, 0, 0.8249}}}
	source, target := bradford([3]float64{0.9642, 1, 0.8249}), bradford([3]float64{0.95047, 1, 1.08883})
	for i := range s.matrix.adapt {
		s.matrix.adapt[i] = target[i] / source[i]
	}
	return s, nil
}

// iccTags 校验ICC标签边界并读取指定模型的配置文件标签
// 入参: data 配置文件数据, model 颜色模型
// 返回: map[string][]byte 配置文件标签, error 错误信息
func iccTags(data []byte, model string) (map[string][]byte, error) {
	if len(data) < 132 || string(data[36:40]) != "acsp" || uint64(binary.BigEndian.Uint32(data)) != uint64(len(data)) {
		return nil, fmt.Errorf("invalid ICC profile header")
	}
	if string(data[16:20]) != model || string(data[20:24]) != "XYZ " || data[8] != 2 && data[8] != 4 {
		return nil, &UnsupportedError{Feature: "ICC profile model"}
	}
	count := uint64(binary.BigEndian.Uint32(data[128:]))
	if count > uint64(len(data)-132)/12 {
		return nil, fmt.Errorf("invalid ICC tag table")
	}
	tags := make(map[string][]byte, int(count))
	for i := 0; i < int(count); i++ {
		entry := data[132+12*i:]
		offset, size := uint64(binary.BigEndian.Uint32(entry[4:])), uint64(binary.BigEndian.Uint32(entry[8:]))
		if offset < 132+12*count || offset > uint64(len(data)) || size > uint64(len(data))-offset {
			return nil, fmt.Errorf("invalid ICC tag bounds")
		}
		name := string(entry[:4])
		if tags[name] != nil {
			return nil, fmt.Errorf("duplicate ICC tag %q", name)
		}
		tags[name] = data[offset : offset+size]
	}
	for _, name := range []string{"A2B0", "A2B1", "A2B2", "D2B0", "D2B1", "D2B2", "D2B3"} {
		if tags[name] != nil {
			return nil, &UnsupportedError{Feature: "ICC lookup table transform"}
		}
	}
	return tags, nil
}

// iccCurve 读取ICC曲线标签并检查单调性
// 入参: data 色调曲线标签数据
// 返回: []uint16 曲线采样或指数, error 错误信息
func iccCurve(data []byte) ([]uint16, error) {
	if len(data) < 12 || string(data[:4]) != "curv" {
		return nil, &UnsupportedError{Feature: "ICC tone curve type"}
	}
	n := uint64(binary.BigEndian.Uint32(data[8:]))
	if n > uint64(len(data)-12)/2 {
		return nil, fmt.Errorf("invalid ICC tone curve length")
	}
	curve := make([]uint16, int(n))
	for j := range curve {
		curve[j] = binary.BigEndian.Uint16(data[12+2*j:])
		if j > 0 && curve[j] < curve[j-1] {
			return nil, fmt.Errorf("nonmonotonic ICC tone curve")
		}
	}
	if n == 1 && curve[0] == 0 {
		return nil, fmt.Errorf("invalid ICC curve gamma")
	}
	return curve, nil
}

// color 将ICC灰度样本转换为sRGB颜色
// 入参: value 灰度编码分量
// 返回: color.NRGBA64 非预乘sRGB颜色
func (s *iccGraySpace) color(value float64) color.NRGBA64 {
	v := math.Max(0, math.Min(1, value))
	if len(s.curve) == 1 {
		v = math.Pow(v, float64(s.curve[0])/256)
	} else if len(s.curve) > 1 {
		position := v * float64(len(s.curve)-1)
		index := min(int(position), len(s.curve)-2)
		v = (float64(s.curve[index]) + (position-float64(index))*float64(s.curve[index+1]-s.curve[index])) / 65535
	}
	return s.matrix.color(v, v, v)
}

// color 将ICC编码分量变换到sRGB，矩阵配置文件使用同一色度变换
// 入参: values RGB编码分量, intent 渲染意图
// 返回: [3]float64 sRGB分量, error 错误信息
func (s *iccRGBSpace) color(values []float64, intent Name) ([3]float64, error) {
	if intent == "AbsoluteColorimetric" {
		return [3]float64{}, &UnsupportedError{Feature: "ICC absolute colorimetric conversion"}
	}
	linear := [3]float64{}
	for i, curve := range s.curves {
		v := math.Max(0, math.Min(1, values[i]))
		switch len(curve) {
		case 0:
			linear[i] = v
		case 1:
			linear[i] = math.Pow(v, float64(curve[0])/256)
		default:
			position := v * float64(len(curve)-1)
			index := min(int(position), len(curve)-2)
			linear[i] = (float64(curve[index]) + (position-float64(index))*float64(curve[index+1]-curve[index])) / 65535
		}
	}
	c := s.matrix.color(linear[0], linear[1], linear[2])
	return [3]float64{float64(c.R) / 65535, float64(c.G) / 65535, float64(c.B) / 65535}, nil
}
