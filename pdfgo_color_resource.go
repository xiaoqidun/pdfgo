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
	"context"
	"fmt"
)

// maxColorSpaceRemapNodes 限制单次颜色空间重映射允许访问的最大节点数，防止共享DAG指数展开
const maxColorSpaceRemapNodes = 4096

// colorRemapKey 标识单次重映射中可复用的间接颜色空间引用及设备色替换模式
type colorRemapKey struct {
	ref   Reference
	remap bool
}

// iccAlternateKey 标识单次重映射中可复用的ICC备用颜色空间流及其替换模式
type iccAlternateKey struct {
	stream *Stream
	count  int
	remap  bool
}

// colorRemapState 保存单次颜色空间重映射的节点预算与中间缓存
type colorRemapState struct {
	nodes         int
	cache         map[colorRemapKey]Object
	defaults      map[Name]Object
	iccValid      map[iccStreamKey]bool
	iccV5         map[iccStreamKey]bool
	iccAlternates map[iccAlternateKey]Object
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

// resourceColorSpace 解析当前资源的颜色空间及默认设备色，不修改共享定义
// 入参: value 颜色空间, resources 当前资源字典
// 返回: Object 有效颜色空间, error 引用或定义错误
func (r *Reader) resourceColorSpace(value Object, resources Dictionary) (Object, error) {
	return r.remapColorSpaceContext(context.Background(), value, resources, true)
}

// resourceColorSpaceContext 在取消上下文内解析当前资源的颜色空间及默认设备色
// 入参: ctx 取消上下文, value 颜色空间, resources 当前资源字典
// 返回: Object 有效颜色空间, error 引用、定义或取消错误
func (r *Reader) resourceColorSpaceContext(ctx context.Context, value Object, resources Dictionary) (Object, error) {
	return r.remapColorSpaceContext(ctx, value, resources, true)
}

// remapColorSpaceContext 在取消上下文内解析资源别名和底层设备色，默认空间本身不重复重映射
// 入参: ctx 取消上下文, value 颜色空间, resources 当前资源, remap 是否替换设备色
// 返回: Object 有效定义, error 引用、定义、预算或取消错误
func (r *Reader) remapColorSpaceContext(ctx context.Context, value Object, resources Dictionary, remap bool) (Object, error) {
	return r.remapColorSpaceState(ctx, value, resources, remap, 0, &colorRemapState{})
}

// remapColorSpaceState 携带节点预算与间接引用记忆表执行颜色空间重映射
// 入参: ctx 取消上下文, value 颜色空间, resources 当前资源, remap 是否替换设备色, depth 嵌套深度, state 重映射状态
// 返回: Object 有效定义, error 引用、定义、预算或取消错误
func (r *Reader) remapColorSpaceState(ctx context.Context, value Object, resources Dictionary, remap bool, depth int, state *colorRemapState) (Object, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if depth >= 64 {
		return nil, fmt.Errorf("color space recursion limit exceeded")
	}
	state.nodes++
	if state.nodes > maxColorSpaceRemapNodes {
		return nil, fmt.Errorf("%w: color space remap limit exceeded", ErrStreamLimitExceeded)
	}
	ref, isRef := value.(Reference)
	key := colorRemapKey{ref: ref, remap: remap}
	if isRef && state.cache != nil {
		if cached, ok := state.cache[key]; ok {
			return cached, nil
		}
	}
	result, err := r.remapColorSpaceNode(ctx, value, resources, remap, depth, state)
	if err != nil {
		return nil, err
	}
	if isRef {
		if state.cache == nil {
			state.cache = make(map[colorRemapKey]Object)
		}
		state.cache[key] = result
	}
	return result, nil
}

// resolveColorSpaceAlias 仅展开颜色空间资源名称别名，不递归子树并保留末端间接引用
// 入参: ctx 取消上下文, value 颜色空间对象, resources 当前资源, depth 当前嵌套深度, state 重映射状态
// 返回: Object 保留末端引用的目标对象, Object 已解析的顶层定义, int 展开后的嵌套深度, error 引用、别名、预算或取消错误
func (r *Reader) resolveColorSpaceAlias(ctx context.Context, value Object, resources Dictionary, depth int, state *colorRemapState) (Object, Object, int, error) {
	current := value
	for {
		if err := ctx.Err(); err != nil {
			return nil, nil, 0, err
		}
		if depth >= 64 {
			return nil, nil, 0, fmt.Errorf("color space recursion limit exceeded")
		}
		state.nodes++
		if state.nodes > maxColorSpaceRemapNodes {
			return nil, nil, 0, fmt.Errorf("%w: color space remap limit exceeded", ErrStreamLimitExceeded)
		}
		resolved, err := r.resolveColorSpace(current)
		if err != nil {
			return nil, nil, 0, err
		}
		name, named := resolved.(Name)
		if array, ok := resolved.(Array); ok && len(array) == 1 {
			name, named = array[0].(Name)
		}
		if !named || name == "DeviceGray" || name == "DeviceRGB" || name == "DeviceCMYK" || name == "Pattern" {
			return current, resolved, depth, nil
		}
		object, err := r.Resolve(resources["ColorSpace"])
		if err != nil {
			return nil, nil, 0, err
		}
		spaces, ok := object.(Dictionary)
		if object != nil && !ok {
			return nil, nil, 0, fmt.Errorf("invalid color space resources")
		}
		target := spaces[name]
		resolvedTarget, err := r.Resolve(target)
		if err != nil {
			return nil, nil, 0, err
		}
		if resolvedTarget == nil {
			if r.warning == nil {
				return nil, nil, 0, fmt.Errorf("missing color space resource %q", name)
			}
			r.warning(Diagnostic{Message: fmt.Sprintf("missing named color space %q; PDF viewer DeviceGray default used", name)})
			return Name("DeviceGray"), Name("DeviceGray"), depth, nil
		}
		current = target
		depth++
	}
}

// remapColorSpaceNode 解析单个颜色空间节点并按需替换默认设备色
// 入参: ctx 取消上下文, value 颜色空间, resources 当前资源, remap 是否替换设备色, depth 嵌套深度, state 重映射状态
// 返回: Object 有效定义, error 引用、定义、预算或取消错误
func (r *Reader) remapColorSpaceNode(ctx context.Context, value Object, resources Dictionary, remap bool, depth int, state *colorRemapState) (Object, error) {
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
			target := spaces[name]
			resolved, err := r.Resolve(target)
			if err != nil {
				return nil, err
			}
			if resolved == nil {
				if r.warning == nil {
					return nil, fmt.Errorf("missing color space resource %q", name)
				}
				r.warning(Diagnostic{Message: fmt.Sprintf("missing named color space %q; PDF viewer DeviceGray default used", name)})
				return Name("DeviceGray"), nil
			}
			return r.remapColorSpaceState(ctx, target, resources, remap, depth+1, state)
		}
		if state.defaults != nil {
			if cached, ok := state.defaults[key]; ok {
				return cached, nil
			}
		}
		defaultTarget := spaces[key]
		defaultValue, err := r.Resolve(defaultTarget)
		if err != nil {
			return nil, err
		}
		if defaultValue == nil {
			result := Object(name)
			if profile := r.deviceProfile; profile != nil && profile.Dictionary["N"] == Integer(count) {
				result = Array{Name("ICCBased"), profile}
			}
			if state.defaults == nil {
				state.defaults = make(map[Name]Object)
			}
			state.defaults[key] = result
			return result, nil
		}
		defaultValue, err = r.remapColorSpaceState(ctx, defaultTarget, resources, false, depth+1, state)
		if err != nil {
			return nil, err
		}
		family := colorSpaceFamily(defaultValue)
		if family == "Lab" || family == "Indexed" || family == "Pattern" {
			return nil, fmt.Errorf("invalid %s color space %q", key, family)
		}
		base, err := r.readPatternBaseContext(ctx, defaultValue)
		if err != nil {
			return nil, err
		}
		if base.components != count {
			return nil, fmt.Errorf("invalid %s component count", key)
		}
		if state.defaults == nil {
			state.defaults = make(map[Name]Object)
		}
		state.defaults[key] = defaultValue
		return defaultValue, nil
	}
	if !indexed || len(array) == 0 {
		return value, nil
	}
	if array[0] == Name("ICCBased") {
		return r.remapICCAlternate(ctx, array, resources, remap, depth, state)
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
	target := array[base]
	nextDepth := depth + 1
	if base == 2 {
		var original Object
		target, original, nextDepth, err = r.resolveColorSpaceAlias(ctx, target, resources, nextDepth, state)
		if err != nil {
			return nil, err
		}
		switch colorSpaceFamily(original) {
		case "DeviceGray", "DeviceRGB", "DeviceCMYK", "CalGray", "CalRGB", "Lab", "ICCBased":
		default:
			return nil, fmt.Errorf("invalid alternate color space")
		}
	}
	resolved, err := r.remapColorSpaceState(ctx, target, resources, remap, nextDepth, state)
	if err != nil {
		return nil, err
	}
	array = append(Array(nil), array...)
	array[base] = resolved
	if array[0] == Name("DeviceN") && len(array) == 5 {
		return r.remapNChannelAttributes(ctx, array, resources, remap, depth+1, state)
	}
	return array, nil
}

// remapNChannelAttributes 解析过程空间别名与所用专色备用色，忽略未用和过程色的同名专色定义
// 入参: ctx 取消上下文, space 多色定义, resources 当前资源, remap 是否替换设备备用色, depth 嵌套深度, state 重映射状态
// 返回: Object 独立属性副本, error 引用、定义、预算或取消错误
func (r *Reader) remapNChannelAttributes(ctx context.Context, space Array, resources Dictionary, remap bool, depth int, state *colorRemapState) (Object, error) {
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
		process["ColorSpace"], err = r.remapColorSpaceState(ctx, process["ColorSpace"], resources, false, depth, state)
		if err != nil {
			return nil, err
		}
		family := colorSpaceFamily(process["ColorSpace"])
		if family != "DeviceGray" && family != "DeviceRGB" && family != "DeviceCMYK" && family != "CalGray" && family != "CalRGB" && family != "Lab" && family != "ICCBased" {
			return nil, fmt.Errorf("invalid NChannel process color space")
		}
		output, _, err := r.readDeviceNAlternateContext(ctx, process["ColorSpace"])
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
		target, resolved, nextDepth, err := r.resolveColorSpaceAlias(ctx, colorants[name], resources, depth, state)
		if err != nil {
			return nil, err
		}
		definition, ok := resolved.(Array)
		if !ok || len(definition) != 4 || definition[0] != Name("Separation") {
			return nil, fmt.Errorf("invalid NChannel spot colorant %q", name)
		}
		colorants[name], err = r.remapColorSpaceState(ctx, target, resources, remap, nextDepth, state)
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

// resourceBlendingSpaceContext 在取消上下文内解析当前资源中的混合空间及默认设备色
// 入参: ctx 取消上下文, value 混合空间, resources 当前资源字典
// 返回: *ColorSpace 混合空间, error 定义、双向变换或取消错误
func (r *Reader) resourceBlendingSpaceContext(ctx context.Context, value Object, resources Dictionary) (*ColorSpace, error) {
	value, err := r.resourceColorSpaceContext(ctx, value, resources)
	if err != nil {
		return nil, err
	}
	return r.readBlendingSpaceContext(ctx, value)
}
