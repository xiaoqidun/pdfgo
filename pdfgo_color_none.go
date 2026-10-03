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

// deviceNColorants 读取色料名称，允许DeviceN重复None，禁止NChannel包含None
// 入参: space 多色颜色空间
// 返回: []Name 色料名称, bool 是否全部为None, error 定义错误
func (r *Reader) deviceNColorants(space Array) ([]Name, bool, error) {
	if len(space) != 4 && len(space) != 5 {
		return nil, false, fmt.Errorf("invalid DeviceN color space")
	}
	value, err := r.Resolve(space[1])
	if err != nil {
		return nil, false, err
	}
	array, ok := value.(Array)
	if !ok || len(array) == 0 {
		return nil, false, fmt.Errorf("invalid DeviceN colorants")
	}
	names, seen, none := make([]Name, len(array)), map[Name]bool{}, true
	for i, item := range array {
		value, err := r.Resolve(item)
		if err != nil {
			return nil, false, err
		}
		name, ok := value.(Name)
		if !ok || name == "All" || seen[name] && name != "None" {
			return nil, false, fmt.Errorf("invalid DeviceN colorant")
		}
		names[i], seen[name] = name, true
		none = none && name == "None"
	}
	if len(space) == 5 {
		value, err := r.Resolve(space[4])
		if err != nil {
			return nil, false, err
		}
		attrs, ok := value.(Dictionary)
		if !ok {
			return nil, false, fmt.Errorf("invalid DeviceN attributes")
		}
		subtype, err := r.Resolve(attrs["Subtype"])
		if err != nil {
			return nil, false, err
		}
		if subtype != nil && subtype != Name("DeviceN") && subtype != Name("NChannel") {
			return nil, false, fmt.Errorf("invalid DeviceN subtype")
		}
		if subtype == Name("NChannel") && seen["None"] {
			return nil, false, fmt.Errorf("invalid NChannel None colorant")
		}
	}
	return names, none, nil
}

// ignoredTintSpace 验证被忽略的备用空间和函数对象类型，不解码资源或求值
// 入参: space 分色或多色颜色空间
// 返回: error 对象类型或引用错误
func (r *Reader) ignoredTintSpace(space Array) error {
	alternate, err := r.resolveColorSpace(space[2])
	if err != nil {
		return err
	}
	switch value := alternate.(type) {
	case Name:
		if value != "DeviceGray" && value != "DeviceRGB" && value != "DeviceCMYK" {
			return fmt.Errorf("invalid alternate color space")
		}
	case Array:
		if len(value) == 0 || value[0] != Name("CalGray") && value[0] != Name("CalRGB") && value[0] != Name("Lab") && value[0] != Name("ICCBased") {
			return fmt.Errorf("invalid alternate color space")
		}
	default:
		return fmt.Errorf("invalid alternate color space")
	}
	function, err := r.Resolve(space[3])
	if err != nil {
		return err
	}
	switch function.(type) {
	case Dictionary, *Stream:
		return nil
	default:
		return fmt.Errorf("invalid tint function")
	}
}

// colorantNone 判断颜色空间是否丢弃全部输出，索引色沿用底层色料
// 入参: object 颜色空间
// 返回: bool 是否不产生输出, error 色料定义错误
func (r *Reader) colorantNone(object Object) (bool, error) {
	return r.colorSpaceNone(object, false)
}

// colorSpaceNone 判断原始或已按资源校验的空间是否完全不绘制
// 入参: object 颜色空间, effective 是否已按资源校验并替换
// 返回: bool 是否不产生输出, error 色料或变换错误
func (r *Reader) colorSpaceNone(object Object, effective bool) (bool, error) {
	object, err := r.resolveColorSpace(object)
	if err != nil {
		return false, err
	}
	array, ok := object.(Array)
	if !ok || len(array) == 0 {
		return false, nil
	}
	if array[0] == Name("Indexed") && len(array) == 4 {
		object, err = r.resolveColorSpace(array[1])
		if err != nil {
			return false, err
		}
		array, ok = object.(Array)
		if !ok || len(array) == 0 {
			return false, nil
		}
	}
	switch array[0] {
	case Name("Separation"):
		if len(array) != 4 {
			return false, fmt.Errorf("invalid Separation color space")
		}
		value, err := r.Resolve(array[1])
		if err != nil {
			return false, err
		}
		if value == Name("None") {
			return true, r.ignoredTintSpace(array)
		}
		if family := colorSpaceFamily(array[2]); effective && (family == "Separation" || family == "DeviceN") {
			space, err := r.readSeparationSpace(array, true, 0)
			if err != nil {
				return false, err
			}
			return space.none, nil
		}
	case Name("DeviceN"):
		_, none, err := r.deviceNColorants(array)
		if err != nil {
			return false, err
		}
		if none {
			return true, r.ignoredTintSpace(array)
		}
		if family := colorSpaceFamily(array[2]); effective && (family == "Separation" || family == "DeviceN") {
			space, err := r.readDeviceNSpace(array, true, 0)
			if err != nil {
				return false, err
			}
			return space.none, nil
		}
	}
	return false, nil
}
