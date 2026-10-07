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

// ThreeDNode 保存PDF三维节点覆盖，空透明度和可见性沿父节点继承
// Matrix为空时保留原变换，非空时替换相对父节点的变换
type ThreeDNode struct {
	Name       string
	Opacity    *float64
	Visible    *bool
	Matrix     *[12]float64
	Dictionary Dictionary
}

// ReadThreeDNode 按PDF标准表312读取三维节点，不修改原始字典
// 入参: object 节点字典或引用
// 返回: ThreeDNode 节点参数, error 类型、数值或引用错误
func (r *Reader) ReadThreeDNode(object Object) (ThreeDNode, error) {
	result := ThreeDNode{}
	dict, err := r.mediaDictionary(object, false)
	if err != nil {
		return result, err
	}
	result.Dictionary = dict
	typeName := Name("3DNode")
	if err := r.threeDName(dict, "Type", &typeName, "3DNode"); err != nil {
		return result, err
	}
	value, err := r.Resolve(dict["N"])
	if err != nil {
		return result, err
	}
	name, ok := value.(String)
	if !ok {
		return result, fmt.Errorf("invalid 3D node name")
	}
	result.Name, err = DecodeTextString(name)
	if err != nil {
		return result, err
	}
	value, err = r.Resolve(dict["O"])
	if err != nil {
		return result, err
	}
	if value != nil {
		opacity := 0.0
		if err := r.threeDNumber(dict, "O", &opacity); err != nil {
			return result, err
		}
		if opacity < 0 || opacity > 1 {
			return result, fmt.Errorf("invalid 3D node opacity")
		}
		result.Opacity = &opacity
	}
	value, err = r.Resolve(dict["V"])
	if err != nil {
		return result, err
	}
	if value != nil {
		flag, ok := value.(Boolean)
		if !ok {
			return result, fmt.Errorf("invalid 3D node visibility")
		}
		visible := bool(flag)
		result.Visible = &visible
	}
	value, err = r.Resolve(dict["M"])
	if err != nil {
		return result, err
	}
	if value != nil {
		values, err := r.numberArray(value, 12)
		if err != nil {
			return result, err
		}
		var matrix [12]float64
		for i, v := range values {
			if math.IsNaN(v) || math.IsInf(v, 0) {
				return result, fmt.Errorf("invalid 3D node matrix")
			}
			matrix[i] = v
		}
		result.Matrix = &matrix
	}
	return result, nil
}
