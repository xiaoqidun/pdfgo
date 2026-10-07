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

// ThreeDCrossSection 保存世界坐标中的剖切面，Axis为初始法线轴索引
// Rotation按XYZ顺序旋转，Axis对应分量不参与旋转，正侧几何被裁去
type ThreeDCrossSection struct {
	Center              [3]float64
	Axis                int
	Rotation            [3]float64
	Opacity             float64
	Color               [3]float64
	IntersectionVisible bool
	IntersectionColor   [3]float64
	Dictionary          Dictionary
}

// ReadThreeDCrossSection 按PDF标准表311读取剖切参数，不执行三维绘制
// 入参: object 剖切字典或引用
// 返回: ThreeDCrossSection 剖切参数, error 类型、方向、数值或引用错误
func (r *Reader) ReadThreeDCrossSection(object Object) (ThreeDCrossSection, error) {
	result := ThreeDCrossSection{Axis: -1, Opacity: .5, Color: [3]float64{1, 1, 1}, IntersectionColor: [3]float64{0, 1, 0}}
	dict, err := r.mediaDictionary(object, false)
	if err != nil {
		return result, err
	}
	result.Dictionary = dict
	typeName := Name("3DCrossSection")
	if err := r.threeDName(dict, "Type", &typeName, "3DCrossSection"); err != nil {
		return result, err
	}
	center, err := r.Resolve(dict["C"])
	if err != nil {
		return result, err
	}
	if center != nil {
		values, err := r.numberArray(center, 3)
		if err != nil {
			return result, err
		}
		for i, v := range values {
			if math.IsNaN(v) || math.IsInf(v, 0) {
				return result, fmt.Errorf("invalid 3D cross section center")
			}
			result.Center[i] = v
		}
	}
	value, err := r.Resolve(dict["O"])
	if err != nil {
		return result, err
	}
	orientation, ok := value.(Array)
	if !ok || len(orientation) != 3 {
		return result, fmt.Errorf("invalid 3D cross section orientation")
	}
	for i, component := range orientation {
		v, err := r.Resolve(component)
		if err != nil {
			return result, err
		}
		if v == nil {
			if result.Axis >= 0 {
				return result, fmt.Errorf("multiple 3D cross section axes")
			}
			result.Axis = i
		} else {
			number, err := r.number(v)
			if err != nil || math.IsNaN(number) || math.IsInf(number, 0) {
				return result, fmt.Errorf("invalid 3D cross section rotation")
			}
			result.Rotation[i] = number
		}
	}
	if result.Axis < 0 {
		return result, fmt.Errorf("missing 3D cross section axis")
	}
	if err := r.threeDNumber(dict, "PO", &result.Opacity); err != nil {
		return result, err
	}
	if result.Opacity < 0 || result.Opacity > 1 {
		return result, fmt.Errorf("invalid 3D cross section opacity")
	}
	if err := r.threeDColor(dict["PC"], &result.Color); err != nil {
		return result, err
	}
	value, err = r.Resolve(dict["IV"])
	if err != nil {
		return result, err
	}
	if value != nil {
		flag, ok := value.(Boolean)
		if !ok {
			return result, fmt.Errorf("invalid 3D intersection visibility")
		}
		result.IntersectionVisible = bool(flag)
	}
	if result.IntersectionVisible {
		if err := r.threeDColor(dict["IC"], &result.IntersectionColor); err != nil {
			return result, err
		}
	}
	return result, nil
}

// threeDColor 读取带颜色空间的三维颜色，未知空间保留调用方默认颜色
// 入参: object 颜色数组或引用, target 默认及输出颜色
// 返回: error 类型、分量或引用错误
func (r *Reader) threeDColor(object Object, target *[3]float64) error {
	value, err := r.Resolve(object)
	if err != nil || value == nil {
		return err
	}
	array, ok := value.(Array)
	if !ok || len(array) == 0 {
		return fmt.Errorf("invalid 3D color array")
	}
	space, err := r.Resolve(array[0])
	if err != nil {
		return err
	}
	if space != Name("DeviceRGB") {
		return nil
	}
	values, err := r.numberArray(Array(array[1:]), 3)
	if err != nil {
		return err
	}
	for i, v := range values {
		if math.IsNaN(v) || math.IsInf(v, 0) {
			return fmt.Errorf("invalid 3D color component")
		}
		target[i] = min(1, max(0, v))
	}
	return nil
}
