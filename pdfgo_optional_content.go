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

// OptionalContentVisible 按文档默认配置判断可选内容的可见性，供固定版式输出使用
// 不应用交互阅读器的Usage自动状态；交互状态可通过Visitor.OptionalContent提供
// 入参: object OCG引用或OCMD字典
// 返回: bool 是否可见, error 配置或表达式错误
func (r *Reader) OptionalContentVisible(object Object) (bool, error) {
	root, err := r.optionalDictionary(r.Trailer["Root"])
	if err != nil {
		return false, err
	}
	properties, err := r.optionalDictionary(root["OCProperties"])
	if err != nil {
		return false, err
	}
	if properties == nil {
		return true, nil
	}
	config, err := r.optionalDictionary(properties["D"])
	if err != nil {
		return false, err
	}
	return r.optionalContentVisible(object, config, 0)
}

// optionalDictionary 解析可缺省的可选内容字典
// 入参: object 字典或引用
// 返回: Dictionary 字典, error 类型错误
func (r *Reader) optionalDictionary(object Object) (Dictionary, error) {
	value, err := r.Resolve(object)
	if err != nil || value == nil {
		return nil, err
	}
	dict, ok := value.(Dictionary)
	if !ok {
		return nil, fmt.Errorf("invalid optional content dictionary")
	}
	return dict, nil
}

// optionalContentVisible 递归求解成员策略及可见性表达式
// 入参: object 组或表达式, config 默认配置, depth 嵌套深度
// 返回: bool 是否可见, error 解析错误
func (r *Reader) optionalContentVisible(object Object, config Dictionary, depth int) (bool, error) {
	if depth >= 64 {
		return false, fmt.Errorf("optional content recursion limit exceeded")
	}
	value, err := r.Resolve(object)
	if err != nil {
		return false, err
	}
	if expression, ok := value.(Array); ok {
		if len(expression) < 2 {
			return false, fmt.Errorf("invalid optional content expression")
		}
		op, ok := expression[0].(Name)
		if !ok || op != "And" && op != "Or" && op != "Not" || op == "Not" && len(expression) != 2 {
			return false, fmt.Errorf("invalid optional content expression operator")
		}
		result := op == "And"
		for _, operand := range expression[1:] {
			state, err := r.optionalContentVisible(operand, config, depth+1)
			if err != nil {
				return false, err
			}
			switch op {
			case "And":
				result = result && state
			case "Or":
				result = result || state
			case "Not":
				result = !state
			}
		}
		return result, nil
	}
	dict, ok := value.(Dictionary)
	if !ok {
		return false, fmt.Errorf("invalid optional content object")
	}
	kind, err := r.Resolve(dict["Type"])
	if err != nil {
		return false, err
	}
	switch kind {
	case Name("OCG"):
		return r.optionalGroupState(object, dict, config)
	case Name("OCMD"):
		if dict["VE"] != nil {
			value, err := r.Resolve(dict["VE"])
			if err != nil {
				return false, err
			}
			if _, ok := value.(Array); !ok {
				return false, fmt.Errorf("invalid optional content expression")
			}
			return r.optionalContentVisible(value, config, depth+1)
		}
		groups, err := r.Resolve(dict["OCGs"])
		if err != nil {
			return false, err
		}
		if groups == nil {
			return true, nil
		}
		list, ok := groups.(Array)
		if !ok {
			list = Array{dict["OCGs"]}
		}
		if len(list) == 0 {
			return true, nil
		}
		policy := dict["P"]
		if policy == nil {
			policy = Name("AnyOn")
		}
		if policy != Name("AnyOn") && policy != Name("AllOn") && policy != Name("AnyOff") && policy != Name("AllOff") {
			return false, fmt.Errorf("invalid optional content policy")
		}
		all := policy == Name("AllOn") || policy == Name("AllOff")
		result := all
		for _, group := range list {
			state, err := r.optionalContentVisible(group, config, depth+1)
			if err != nil {
				return false, err
			}
			if policy == Name("AnyOff") || policy == Name("AllOff") {
				state = !state
			}
			if all {
				result = result && state
			} else {
				result = result || state
			}
		}
		return result, nil
	default:
		return false, fmt.Errorf("invalid optional content type")
	}
}

// optionalGroupState 按配置意图、基础状态和显式开关确定组状态
// 入参: object 组引用, group 组字典, config 默认配置
// 返回: bool 是否可见, error 配置错误
func (r *Reader) optionalGroupState(object Object, group, config Dictionary) (bool, error) {
	intents := func(object Object) (Array, error) {
		value, err := r.Resolve(object)
		if err != nil {
			return nil, err
		}
		if value == nil {
			return Array{Name("View")}, nil
		}
		if name, ok := value.(Name); ok {
			return Array{name}, nil
		}
		array, ok := value.(Array)
		if !ok {
			return nil, fmt.Errorf("invalid optional content intent")
		}
		for _, item := range array {
			if _, ok := item.(Name); !ok {
				return nil, fmt.Errorf("invalid optional content intent")
			}
		}
		return array, nil
	}
	a, err := intents(group["Intent"])
	if err != nil {
		return false, err
	}
	b, err := intents(config["Intent"])
	if err != nil {
		return false, err
	}
	match := false
	for _, x := range a {
		for _, y := range b {
			match = match || x == y || y == Name("All")
		}
	}
	if !match {
		return true, nil
	}
	state := true
	switch config["BaseState"] {
	case nil, Name("ON"), Name("Unchanged"):
	case Name("OFF"):
		state = false
	default:
		return false, fmt.Errorf("invalid optional content base state")
	}
	for _, key := range []Name{"ON", "OFF"} {
		value, err := r.Resolve(config[key])
		if err != nil {
			return false, err
		}
		if value == nil {
			continue
		}
		list, ok := value.(Array)
		if !ok {
			return false, fmt.Errorf("invalid optional content state list")
		}
		for _, item := range list {
			ref, ok := item.(Reference)
			if !ok {
				return false, fmt.Errorf("invalid optional content group reference")
			}
			if current, ok := object.(Reference); ok && current == ref {
				state = key == "ON"
			}
		}
	}
	return state, nil
}

// optionalVisible 使用调用方状态或默认配置判断可选内容是否绘制
// 入参: object 可选内容属性
// 返回: bool 是否可见, error 配置错误
func (p *pageInterpreter) optionalVisible(object Object) (bool, error) {
	value, err := p.reader.Resolve(object)
	if err != nil {
		return false, err
	}
	if value == nil {
		return true, nil
	}
	if p.visitor.OptionalContent != nil {
		return p.visitor.OptionalContent(object)
	}
	return p.reader.OptionalContentVisible(object)
}
