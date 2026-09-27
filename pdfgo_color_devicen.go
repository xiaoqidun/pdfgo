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
	"math"
)

// singleDeviceNSpace 保存单分量专色的备用空间及着色函数
type singleDeviceNSpace struct {
	alternate *ColorSpace
	transform *gradientFunction
}

// paint 按专色浓度生成备用空间颜色并保留原始分量
// 入参: tint 专色浓度, intent 渲染意图
// 返回: Paint 颜色, error 颜色转换错误
func (s *singleDeviceNSpace) paint(tint float64, intent Name) (Paint, error) {
	values := s.values(tint)
	rgb, err := s.alternate.RGB(values[:s.alternate.Components()], intent)
	if err != nil {
		return Paint{}, err
	}
	paint := Paint{RGB: rgb, Space: s.alternate, Values: values}
	if s.alternate.Model == "DeviceCMYK" {
		paint.CMYK = &values
	}
	return paint, nil
}

// values 将专色浓度映射到备用空间的有效分量区间
// 入参: tint 专色浓度
// 返回: [4]float64 备用空间分量
func (s *singleDeviceNSpace) values(tint float64) [4]float64 {
	values := s.transform.value(math.Max(0, math.Min(1, tint)))
	for c := 0; c < s.alternate.Components(); c++ {
		values[c] = math.Max(0, math.Min(1, values[c]))
	}
	return values
}

// readSingleDeviceN 解析单分量DeviceN颜色空间，不将专色浓度解释为设备灰度
// 入参: space 颜色空间数组
// 返回: *singleDeviceNSpace 着色定义, error 解析或未支持的分量错误
func (r *Reader) readSingleDeviceN(space Array) (*singleDeviceNSpace, error) {
	if len(space) != 4 && len(space) != 5 {
		return nil, fmt.Errorf("invalid DeviceN color space")
	}
	object, err := r.Resolve(space[1])
	if err != nil {
		return nil, err
	}
	names, ok := object.(Array)
	if !ok || len(names) == 0 {
		return nil, fmt.Errorf("invalid DeviceN colorants")
	}
	if len(names) != 1 {
		return nil, &UnsupportedError{Feature: "DeviceN component count"}
	}
	name, ok := names[0].(Name)
	if !ok || name == "All" {
		return nil, fmt.Errorf("invalid DeviceN colorant")
	}
	if name == "None" {
		return nil, &UnsupportedError{Feature: "DeviceN None colorant"}
	}
	alternate, err := r.readColorSpace(space[2])
	if err != nil {
		return nil, err
	}
	transform, err := r.readGradientFunction(space[3], alternate.Components(), 0)
	if err != nil {
		return nil, err
	}
	return &singleDeviceNSpace{alternate: alternate, transform: transform}, nil
}
