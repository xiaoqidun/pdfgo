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

// annotationTextGraphicsState 分离已生效的字体覆盖，避免重放状态时覆盖排版字号
// 入参: p 默认外观解释器, op 已验证并执行的gs操作
// 返回: Name 覆盖字体名，未覆盖时为空, Operation 重放操作, error 资源错误
func (r *Reader) annotationTextGraphicsState(p *pageInterpreter, op Operation) (Name, Operation, error) {
	value, err := r.Resolve(p.resources["ExtGState"])
	if err != nil {
		return "", op, err
	}
	states, ok := value.(Dictionary)
	if !ok {
		return "", op, fmt.Errorf("invalid default appearance graphics resources")
	}
	value, err = r.Resolve(states[op.Operands[0].(Name)])
	if err != nil {
		return "", op, err
	}
	state, ok := value.(Dictionary)
	if !ok {
		return "", op, fmt.Errorf("invalid default appearance graphics state")
	}
	font, err := r.Resolve(state["Font"])
	if err != nil || font == nil {
		return "", op, err
	}
	name, err := r.addAnnotationResource(p.resources, "Font", p.state.font.Dictionary)
	if err != nil {
		return "", op, err
	}
	state = maps.Clone(state)
	delete(state, "Font")
	key, err := r.addAnnotationResource(p.resources, "ExtGState", state)
	if err != nil {
		return "", op, err
	}
	op.Operands = []Object{key}
	return name, op, nil
}

// addAnnotationResource 向本次外观资源添加独立名称，不修改源资源子字典
// 入参: resources 本次外观资源, category 资源类别, object 资源对象
// 返回: Name 新资源名, error 资源字典错误
func (r *Reader) addAnnotationResource(resources Dictionary, category Name, object Object) (Name, error) {
	value, err := r.Resolve(resources[category])
	if err != nil {
		return "", err
	}
	entries, ok := value.(Dictionary)
	if resources == nil || value != nil && !ok {
		return "", fmt.Errorf("invalid annotation %s resources", category)
	}
	entries = maps.Clone(entries)
	if entries == nil {
		entries = Dictionary{}
	}
	for i := len(entries); ; i++ {
		name := Name(fmt.Sprintf("DA%d", i))
		if _, exists := entries[name]; exists {
			continue
		}
		entries[name] = object
		resources[category] = entries
		return name, nil
	}
}
