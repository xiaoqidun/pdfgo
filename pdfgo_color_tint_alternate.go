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

import "fmt"

// tintAlternate 保存默认设备色替换后的输入分量和最终备用空间
type tintAlternate struct {
	space      *ColorSpace
	lab        *labSpace
	separation *separationSpace
	deviceN    *deviceNSpace
	components int
	none       bool
}

// readTintAlternate 读取备用空间，特殊嵌套只允许已校验的默认设备色替换
// 入参: object 备用空间, effective 是否已按资源校验并替换, depth 嵌套深度
// 返回: *tintAlternate 输入与输出定义, error 格式或变换错误
func (r *Reader) readTintAlternate(object Object, effective bool, depth int) (*tintAlternate, error) {
	if depth >= 64 {
		return nil, fmt.Errorf("color space recursion limit exceeded")
	}
	object, err := r.resolveColorSpace(object)
	if err != nil {
		return nil, err
	}
	result := &tintAlternate{}
	if array, ok := object.(Array); ok && len(array) != 0 && (array[0] == Name("Separation") || array[0] == Name("DeviceN")) {
		if !effective {
			return nil, fmt.Errorf("invalid alternate color space")
		}
		if array[0] == Name("Separation") {
			result.separation, err = r.readSeparationSpace(array, true, depth+1)
			if err == nil {
				result.space, result.components = result.separation.space, 1
				result.none = result.separation.none || result.separation.name == "None"
			}
		} else {
			result.deviceN, err = r.readDeviceNSpace(array, true, depth+1)
			if err == nil {
				result.space, result.components, result.none = result.deviceN.alternate, result.deviceN.components, result.deviceN.none
			}
		}
		if err == nil && (result.components < 1 || result.components > 4) {
			return nil, fmt.Errorf("invalid default alternate component count")
		}
		return result, err
	}
	result.space, result.lab, err = r.readDeviceNAlternate(object)
	if err != nil {
		return nil, err
	}
	result.components = result.space.Components()
	return result, nil
}

// values 将备用输入转换为最终颜色分量，不混用函数输出数和显示空间分量数
// 入参: input 备用空间输入分量
// 返回: [4]float64 最终分量, error 着色错误
func (s *tintAlternate) values(input []float64) ([4]float64, error) {
	if len(input) != s.components {
		return [4]float64{}, fmt.Errorf("invalid alternate component count")
	}
	if s.separation != nil {
		return s.separation.values(input[0])
	}
	if s.deviceN != nil {
		return s.deviceN.values(input...)
	}
	return [4]float64{}, fmt.Errorf("missing nested tint transform")
}
