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

// resourceColorSpace 解析当前资源的颜色空间及默认设备色，不修改共享定义
// 入参: value 颜色空间, resources 当前资源字典
// 返回: Object 有效颜色空间, error 引用或定义错误
func (r *Reader) resourceColorSpace(value Object, resources Dictionary) (Object, error) {
	return r.remapColorSpace(value, resources, true, 0)
}

// remapColorSpace 解析资源别名和底层设备色，默认空间本身不重复重映射
// 入参: value 颜色空间, resources 当前资源, remap 是否替换设备色, depth 嵌套深度
// 返回: Object 有效定义, error 引用或定义错误
func (r *Reader) remapColorSpace(value Object, resources Dictionary, remap bool, depth int) (Object, error) {
	if depth >= 64 {
		return nil, fmt.Errorf("color space recursion limit exceeded")
	}
	value, err := r.resolveColorSpace(value)
	if err != nil {
		return nil, err
	}
	name, named := value.(Name)
	array, indexed := value.(Array)
	if indexed && len(array) == 1 {
		name, named = array[0].(Name)
	}
	if named {
		key := Name("")
		count := 0
		switch name {
		case "DeviceGray":
			key, count = "DefaultGray", 1
		case "DeviceRGB":
			key, count = "DefaultRGB", 3
		case "DeviceCMYK":
			key, count = "DefaultCMYK", 4
		case "Pattern":
			return value, nil
		}
		if count != 0 && !remap {
			return name, nil
		}
		object, err := r.Resolve(resources["ColorSpace"])
		if err != nil {
			return nil, err
		}
		spaces, ok := object.(Dictionary)
		if object != nil && !ok {
			return nil, fmt.Errorf("invalid color space resources")
		}
		if count == 0 {
			if spaces[name] == nil {
				return nil, fmt.Errorf("missing color space resource %q", name)
			}
			return r.remapColorSpace(spaces[name], resources, remap, depth+1)
		}
		defaultValue, err := r.Resolve(spaces[key])
		if err != nil {
			return nil, err
		}
		if defaultValue == nil {
			return name, nil
		}
		defaultValue, err = r.remapColorSpace(defaultValue, resources, false, depth+1)
		if err != nil {
			return nil, err
		}
		family := colorSpaceFamily(defaultValue)
		if family == "Lab" || family == "Indexed" || family == "Pattern" {
			return nil, fmt.Errorf("invalid %s color space %q", key, family)
		}
		base, err := r.readPatternBase(defaultValue)
		if err != nil {
			return nil, err
		}
		if base.components != count {
			return nil, fmt.Errorf("invalid %s component count", key)
		}
		return defaultValue, nil
	}
	if !indexed || len(array) == 0 {
		return value, nil
	}
	if array[0] == Name("ICCBased") {
		return r.remapICCAlternate(array, resources, remap, depth)
	}
	base := -1
	switch array[0] {
	case Name("Pattern"):
		if len(array) == 2 {
			base = 1
		}
	case Name("Indexed"):
		if len(array) == 4 {
			base = 1
		}
	case Name("Separation"):
		if len(array) == 4 {
			colorant, err := r.Resolve(array[1])
			if err != nil {
				return nil, err
			}
			if colorant != Name("All") && colorant != Name("None") {
				base = 2
			}
		}
	case Name("DeviceN"):
		if len(array) == 4 || len(array) == 5 {
			_, none, err := r.deviceNColorants(array)
			if err != nil {
				return nil, err
			}
			if !none {
				base = 2
			}
		}
	}
	if base < 0 {
		return value, nil
	}
	if base == 2 {
		original, err := r.remapColorSpace(array[base], resources, false, depth+1)
		if err != nil {
			return nil, err
		}
		switch colorSpaceFamily(original) {
		case "DeviceGray", "DeviceRGB", "DeviceCMYK", "CalGray", "CalRGB", "Lab", "ICCBased":
		default:
			return nil, fmt.Errorf("invalid alternate color space")
		}
	}
	resolved, err := r.remapColorSpace(array[base], resources, remap, depth+1)
	if err != nil {
		return nil, err
	}
	array = append(Array(nil), array...)
	array[base] = resolved
	if array[0] == Name("DeviceN") && len(array) == 5 {
		return r.remapNChannelAttributes(array, resources, remap, depth+1)
	}
	return array, nil
}

// remapNChannelAttributes 解析过程空间别名与所用专色备用色，忽略未用和过程色的同名专色定义
// 入参: space 多色定义, resources 当前资源, remap 是否替换设备备用色, depth 嵌套深度
// 返回: Object 独立属性副本, error 引用或定义错误
func (r *Reader) remapNChannelAttributes(space Array, resources Dictionary, remap bool, depth int) (Object, error) {
	value, err := r.Resolve(space[4])
	if err != nil {
		return nil, err
	}
	attrs, ok := value.(Dictionary)
	if !ok {
		return nil, fmt.Errorf("invalid DeviceN attributes")
	}
	subtype, err := r.Resolve(attrs["Subtype"])
	if err != nil || subtype != Name("NChannel") {
		return space, err
	}
	attrs = cloneColorantDictionary(attrs)
	processNames := map[Name]bool{}
	value, err = r.Resolve(attrs["Process"])
	if err != nil {
		return nil, err
	}
	if value != nil {
		process, ok := value.(Dictionary)
		if !ok {
			return nil, fmt.Errorf("invalid NChannel process dictionary")
		}
		process = cloneColorantDictionary(process)
		process["ColorSpace"], err = r.remapColorSpace(process["ColorSpace"], resources, false, depth)
		if err != nil {
			return nil, err
		}
		family := colorSpaceFamily(process["ColorSpace"])
		if family != "DeviceGray" && family != "DeviceRGB" && family != "DeviceCMYK" && family != "CalGray" && family != "CalRGB" && family != "Lab" && family != "ICCBased" {
			return nil, fmt.Errorf("invalid NChannel process color space")
		}
		output, _, err := r.readDeviceNAlternate(process["ColorSpace"])
		if err != nil {
			return nil, err
		}
		components, err := r.Resolve(process["Components"])
		if err != nil {
			return nil, err
		}
		array, ok := components.(Array)
		if !ok {
			return nil, fmt.Errorf("invalid NChannel process components")
		}
		for _, component := range array {
			name, err := r.Resolve(component)
			if err != nil {
				return nil, err
			}
			if name, ok := name.(Name); ok {
				processNames[name] = true
			}
		}
		if output.Model == "DeviceCMYK" {
			for _, name := range [...]Name{"Cyan", "Magenta", "Yellow", "Black"} {
				processNames[name] = true
			}
		}
		attrs["Process"] = process
	}
	names, _, err := r.deviceNColorants(space)
	if err != nil {
		return nil, err
	}
	var colorants Dictionary
	for _, name := range names {
		if processNames[name] {
			continue
		}
		if colorants == nil {
			value, err := r.Resolve(attrs["Colorants"])
			if err != nil {
				return nil, err
			}
			dictionary, ok := value.(Dictionary)
			if !ok {
				return nil, fmt.Errorf("missing NChannel spot colorants")
			}
			colorants = cloneColorantDictionary(dictionary)
		}
		colorants[name], err = r.remapColorSpace(colorants[name], resources, remap, depth)
		if err != nil {
			return nil, err
		}
	}
	if colorants != nil {
		attrs["Colorants"] = colorants
	}
	space[4] = attrs
	return space, nil
}

// cloneColorantDictionary 复制需重映射的属性字典，不改变共享资源
// 入参: source 源字典
// 返回: Dictionary 独立浅副本
func cloneColorantDictionary(source Dictionary) Dictionary {
	result := make(Dictionary, len(source))
	for key, value := range source {
		result[key] = value
	}
	return result
}

// colorSpaceFamily 取得已解析颜色空间的族名
// 入参: value 颜色空间定义
// 返回: Name 族名，定义无效时为空
func colorSpaceFamily(value Object) Name {
	if name, ok := value.(Name); ok {
		return name
	}
	if array, ok := value.(Array); ok && len(array) != 0 {
		name, _ := array[0].(Name)
		return name
	}
	return ""
}

// resourceBlendingSpace 解析当前资源中的混合空间及默认设备色
// 入参: value 混合空间, resources 当前资源字典
// 返回: *ColorSpace 混合空间, error 定义或双向变换错误
func (r *Reader) resourceBlendingSpace(value Object, resources Dictionary) (*ColorSpace, error) {
	value, err := r.resourceColorSpace(value, resources)
	if err != nil {
		return nil, err
	}
	return r.readBlendingSpace(value)
}
