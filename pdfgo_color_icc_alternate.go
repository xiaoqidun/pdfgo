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
	"maps"
	"math"
	"slices"
)

// validateICCSourceHeader 按PDF要求校验源配置类别、模型与分量数
// 入参: data 配置数据, count PDF声明的分量数
// 返回: error 配置格式或声明冲突
func validateICCSourceHeader(data []byte, count int) error {
	if len(data) < 132 || string(data[36:40]) != "acsp" || uint64(binary.BigEndian.Uint32(data)) != uint64(len(data)) {
		return fmt.Errorf("invalid ICC profile header")
	}
	model := string(data[16:20])
	components := map[string]int{"GRAY": 1, "RGB ": 3, "CMYK": 4, "Lab ": 3}[model]
	if components == 0 || components != count {
		return fmt.Errorf("invalid ICC source profile components")
	}
	switch string(data[12:16]) {
	case "scnr", "mntr", "prtr", "spac":
	default:
		return fmt.Errorf("invalid ICC source profile class")
	}
	if string(data[20:24]) != "XYZ " && string(data[20:24]) != "Lab " {
		return fmt.Errorf("invalid ICC profile connection space")
	}
	return nil
}

// remapICCAlternate 仅重映射实际使用的备用设备色，保留原流与源范围
// 入参: object ICCBased数组, resources 当前资源, remap 是否替换设备色, depth 嵌套深度
// 返回: Object 有效ICC定义, error 配置或资源错误
func (r *Reader) remapICCAlternate(object Array, resources Dictionary, remap bool, depth int) (Object, error) {
	stream, count, err := r.iccProfileStream(object)
	if err != nil {
		return nil, err
	}
	data, err := stream.Decode()
	if err != nil {
		return nil, err
	}
	if len(data) < 9 || data[8] <= 4 {
		return object, nil
	}
	if err := validateICCSourceHeader(data, int(count)); err != nil {
		return nil, err
	}
	alternate, err := r.iccAlternateObject(stream, int(count))
	if err != nil {
		return nil, err
	}
	alternate, err = r.remapColorSpace(alternate, resources, false, depth+1)
	if err != nil {
		return nil, err
	}
	copy := *stream
	copy.Dictionary = maps.Clone(stream.Dictionary)
	copy.Dictionary["Alternate"] = alternate
	if !remap {
		return Array{Name("ICCBased"), &copy}, nil
	}
	if _, err := r.readICCColorSpace(Array{Name("ICCBased"), &copy}); err != nil {
		return nil, err
	}
	alternate, err = r.remapColorSpace(alternate, resources, remap, depth+1)
	if err != nil {
		return nil, err
	}
	copy.Dictionary["Alternate"] = alternate
	return Array{Name("ICCBased"), &copy}, nil
}

// iccAlternateObject 读取备用颜色空间，缺省按N选择设备空间
// 入参: stream 配置流, count 源分量数
// 返回: Object 备用定义, error 引用错误
func (r *Reader) iccAlternateObject(stream *Stream, count int) (Object, error) {
	value, err := r.Resolve(stream.Dictionary["Alternate"])
	if err != nil {
		return nil, err
	}
	if value == nil {
		value = map[int]Name{1: "DeviceGray", 3: "DeviceRGB", 4: "DeviceCMYK"}[count]
	}
	return value, nil
}

// readICCAlternate 编译同分量备用空间，不预先转换原始分量或共享配置缓存
// 入参: stream 配置流, count 源分量数, ranges 源范围, effective 是否已按资源校验并替换
// 返回: *iccColorSpace 备用源变换, error 定义或分量错误
func (r *Reader) readICCAlternate(stream *Stream, count int, ranges *[8]float64, effective bool) (*iccColorSpace, error) {
	object, err := r.iccAlternateObject(stream, count)
	if err != nil {
		return nil, err
	}
	alternate, err := r.readPatternColorSpace(object, effective)
	if err != nil {
		return nil, err
	}
	if alternate.components != count {
		return nil, fmt.Errorf("invalid ICC alternate component count")
	}
	return &iccColorSpace{alternate: alternate, alternateCount: count, ranges: ranges, alternateBlending: alternate.blending}, nil
}

// alternateValues 裁切源范围后原样传入备用空间，不先归一化源分量
// 入参: values PDF源分量
// 返回: []float64 裁切后的分量，未超出范围时复用输入, error 数量或非有限值错误
func (s *iccColorSpace) alternateValues(values []float64) ([]float64, error) {
	if len(values) != s.components() {
		return nil, fmt.Errorf("invalid ICC alternate input count")
	}
	result := values
	ranges := s.sourceRanges()
	for i, value := range values {
		if math.IsNaN(value) || math.IsInf(value, 0) {
			return nil, fmt.Errorf("invalid ICC alternate input")
		}
		clipped := math.Max(ranges[2*i], math.Min(ranges[2*i+1], value))
		if clipped != value {
			if &result[0] == &values[0] {
				result = slices.Clone(values)
			}
			result[i] = clipped
		}
	}
	return result, nil
}

// alternatePaint 从固定源分量构建备用颜色，独立保存最终输出空间
// 入参: input 实际源分量, intent 渲染意图
// 返回: Paint 备用空间结果, error 裁切或转换错误
func (s *iccColorSpace) alternatePaint(input [4]float64, intent Name) (Paint, error) {
	values, err := s.alternateValues(input[:s.components()])
	if err != nil {
		return Paint{}, err
	}
	paint, err := s.alternate.convert(values, intent)
	paint.SourceSpace = "ICCBased"
	return paint, err
}

// alternateColor 将单位源分量恢复后交给备用空间，不影响原ICC快速路径的分配
// 入参: input 单位源分量, intent 渲染意图
// 返回: [3]float64 显示颜色, error 备用转换错误
func (s *iccColorSpace) alternateColor(input [4]float64, intent Name) ([3]float64, error) {
	device := s.deviceValues(input[:s.components()])
	paint, err := s.alternatePaint(device, intent)
	return paint.RGB, err
}

// alternateXYZ 保留实际备用空间的色度计算，不先截断到显示RGB
// 入参: input 单位源分量, intent 渲染意图
// 返回: [3]float64 D50色度, error 转换错误
func (s *iccColorSpace) alternateXYZ(input [4]float64, intent Name) ([3]float64, error) {
	device := s.deviceValues(input[:s.components()])
	paint, err := s.alternatePaint(device, intent)
	if err != nil {
		return [3]float64{}, err
	}
	if paint.Space != nil {
		return paint.Space.xyz(paint.Values[:paint.Space.Components()], intent)
	}
	return (&ColorSpace{Model: "DeviceRGB"}).xyz(paint.RGB[:], intent)
}

// alternateFromXYZ 仅将合法可逆的备用混合空间用于目标转换
// 入参: xyz D50色度, intent 渲染意图
// 返回: [4]float64 目标分量, error 混合约束或逆变换错误
func (s *iccColorSpace) alternateFromXYZ(xyz [3]float64, intent Name) ([4]float64, error) {
	if err := s.validateBlending(); err != nil {
		return [4]float64{}, err
	}
	if p := s.alternateBlending.profile; p != nil {
		return p.fromXYZ(xyz, intent)
	}
	rgb := iccXYZRGB(xyz)
	return s.alternateBlending.Convert(rgb[:], &ColorSpace{Model: "DeviceRGB"}, intent)
}
