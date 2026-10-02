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
	"os"
)

// ColorFunction 保存单输入单输出颜色函数，解析后可独立于阅读器并发求值
type ColorFunction struct {
	function *gradientFunction
}

// ColorConversion 保存RGB转设备四色的黑版生成及底色去除函数，空值采用恒等设备默认值
type ColorConversion struct {
	BlackGeneration   *ColorFunction
	UndercolorRemoval *ColorFunction
}

// ReadColorFunction 解析类型0、2、3及4的单分量颜色函数，不截断负输出
// 入参: object 函数字典或数据流
// 返回: *ColorFunction 已解析函数, error 引用或函数错误
func (r *Reader) ReadColorFunction(object Object) (*ColorFunction, error) {
	if r.closed {
		return nil, os.ErrClosed
	}
	value, err := r.Resolve(object)
	if err != nil {
		return nil, err
	}
	switch value.(type) {
	case Dictionary, *Stream:
	default:
		return nil, fmt.Errorf("invalid color function")
	}
	function, err := r.readGradientFunction(value, 1, 0)
	if err != nil {
		return nil, err
	}
	return &ColorFunction{function: function}, nil
}

// Evaluate 按函数定义域及输出范围求值，空函数为恒等函数
// 入参: value 有限输入值
// 返回: float64 函数结果, error 非有限值或计算错误
func (f *ColorFunction) Evaluate(value float64) (float64, error) {
	if math.IsNaN(value) || math.IsInf(value, 0) {
		return 0, fmt.Errorf("invalid color function input")
	}
	if f == nil || f.function == nil {
		return value, nil
	}
	result, err := f.function.evaluate(value)
	if err != nil {
		return 0, err
	}
	if math.IsNaN(result[0]) || math.IsInf(result[0], 0) {
		return 0, fmt.Errorf("nonfinite color function result")
	}
	return result[0], nil
}

// RGBToCMYK 按PDF设备转换规则生成黑版并去除底色，最终分量裁切至单位范围
// 入参: rgb 设备RGB单位分量
// 返回: [4]float64 设备四色分量, error 无效分量或函数错误
func (c ColorConversion) RGBToCMYK(rgb [3]float64) ([4]float64, error) {
	var result [4]float64
	for _, value := range rgb {
		if math.IsNaN(value) || math.IsInf(value, 0) || value < 0 || value > 1 {
			return result, fmt.Errorf("invalid device RGB component")
		}
	}
	k := 1 - math.Max(rgb[0], math.Max(rgb[1], rgb[2]))
	black, err := c.BlackGeneration.Evaluate(k)
	if err != nil {
		return result, err
	}
	removed, err := c.UndercolorRemoval.Evaluate(k)
	if err != nil {
		return result, err
	}
	for i, value := range rgb {
		result[i] = math.Max(0, math.Min(1, 1-value-removed))
	}
	result[3] = math.Max(0, math.Min(1, black))
	return result, nil
}
