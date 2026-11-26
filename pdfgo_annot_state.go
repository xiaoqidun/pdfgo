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
	"maps"
)

// annotationRewrittenGS 保存图形状态剥离字体覆盖后的资源名映射
type annotationRewrittenGS struct {
	font  Name
	state Name
}

// annotationGraphicsStateCache 保存单次外观生成的资源写时拷贝与图形状态复用缓存
type annotationGraphicsStateCache struct {
	cloned    map[Name]bool
	nextIndex map[Name]int
	rewritten map[Name]annotationRewrittenGS
}

// annotationTextGraphicsState 分离已生效的字体覆盖，避免重放状态时覆盖排版字号并复用重复图形状态
// 入参: p 默认外观解释器, op 已验证并执行的gs操作, cache 本次外观资源缓存
// 返回: Name 覆盖字体名，未覆盖时为空, Operation 重放操作, error 资源错误
func (r *Reader) annotationTextGraphicsState(p *pageInterpreter, op Operation, cache *annotationGraphicsStateCache) (Name, Operation, error) {
	orig := op.Operands[0].(Name)
	if entry, ok := cache.rewritten[orig]; ok {
		op.Operands = []Object{entry.state}
		return entry.font, op, nil
	}
	value, err := r.Resolve(p.resources["ExtGState"])
	if err != nil {
		return "", op, err
	}
	states, ok := value.(Dictionary)
	if !ok {
		return "", op, fmt.Errorf("invalid default appearance graphics resources")
	}
	value, err = r.Resolve(states[orig])
	if err != nil {
		return "", op, err
	}
	state, ok := value.(Dictionary)
	if !ok {
		return "", op, fmt.Errorf("invalid default appearance graphics state")
	}
	font, err := r.Resolve(state["Font"])
	if err != nil {
		return "", op, err
	}
	if cache.rewritten == nil {
		cache.rewritten = make(map[Name]annotationRewrittenGS)
	}
	if font == nil {
		cache.rewritten[orig] = annotationRewrittenGS{state: orig}
		return "", op, nil
	}
	name, err := r.addAnnotationResource(p.resources, "Font", p.state.font.Dictionary, cache)
	if err != nil {
		return "", op, err
	}
	state = maps.Clone(state)
	delete(state, "Font")
	key, err := r.addAnnotationResource(p.resources, "ExtGState", state, cache)
	if err != nil {
		return "", op, err
	}
	cache.rewritten[orig] = annotationRewrittenGS{font: name, state: key}
	op.Operands = []Object{key}
	return name, op, nil
}

// addAnnotationResource 向本次外观资源添加独立名称，按类别首次写入时克隆以不修改源资源子字典
// 入参: resources 本次外观资源, category 资源类别, object 资源对象, cache 本次外观资源缓存
// 返回: Name 新资源名, error 资源字典错误
func (r *Reader) addAnnotationResource(resources Dictionary, category Name, object Object, cache *annotationGraphicsStateCache) (Name, error) {
	var entries Dictionary
	if cache.cloned[category] {
		entries = resources[category].(Dictionary)
	} else {
		value, err := r.Resolve(resources[category])
		if err != nil {
			return "", err
		}
		var ok bool
		entries, ok = value.(Dictionary)
		if value != nil && !ok {
			return "", fmt.Errorf("invalid annotation %s resources", category)
		}
		entries = maps.Clone(entries)
		if entries == nil {
			entries = Dictionary{}
		}
		resources[category] = entries
		if cache.cloned == nil {
			cache.cloned = make(map[Name]bool)
		}
		cache.cloned[category] = true
	}
	if cache.nextIndex == nil {
		cache.nextIndex = make(map[Name]int)
	}
	index := cache.nextIndex[category]
	if index < len(entries) {
		index = len(entries)
	}
	for ; ; index++ {
		name := Name(fmt.Sprintf("DA%d", index))
		if _, exists := entries[name]; exists {
			continue
		}
		entries[name] = object
		cache.nextIndex[category] = index + 1
		return name, nil
	}
}
