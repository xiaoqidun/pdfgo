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
	"os"
	"reflect"
)

// maxOptionalContentSteps 限制单次可选内容可见性求值的最大步数
const maxOptionalContentSteps = 65536

// optionalContentKey 标识可选内容求值缓存与环路检测中的唯一对象
type optionalContentKey struct {
	ref    Reference
	kind   uint8
	ptr    uintptr
	length int
}

// optionalContentDirectEntry 保存单次求值中直接对象的强引用与可见性结果
type optionalContentDirectEntry struct {
	keep    Object
	visible bool
}

// optionalContentDictState 保存直接OCG字典的强引用与默认开关状态
type optionalContentDictState struct {
	dict Dictionary
	on   bool
}

// optionalContentEvaluator 维护可选内容求值的缓存、环路与步数状态
type optionalContentEvaluator struct {
	reader          *Reader
	ctx             context.Context
	config          Dictionary
	steps           int
	refMemo         map[Reference]bool
	directMemo      map[optionalContentKey]optionalContentDirectEntry
	active          map[optionalContentKey]bool
	intentsReady    bool
	configIntentSet map[Name]bool
	configIntentAll bool
	stateReady      bool
	baseState       bool
	groupStates     map[Reference]bool
	dictReady       bool
	dictStates      map[uintptr]optionalContentDictState
}

// OptionalContentVisible 按文档默认配置判断可选内容的可见性，供固定版式输出使用
// 不应用交互阅读器的Usage自动状态；交互状态可通过Visitor.OptionalContent提供
// 入参: object OCG引用或OCMD字典
// 返回: bool 是否可见, error 配置、表达式或超限错误
func (r *Reader) OptionalContentVisible(object Object) (bool, error) {
	return r.optionalContentVisibleContext(context.Background(), object)
}

// newOptionalContentEvaluator 初始化文档默认配置的可选内容求值器，文档无OCProperties时返回空值
// 入参: ctx 取消上下文
// 返回: *optionalContentEvaluator 求值器, bool 文档是否声明可选内容配置, error 配置或取消错误
func (r *Reader) newOptionalContentEvaluator(ctx context.Context) (*optionalContentEvaluator, bool, error) {
	if r == nil {
		return nil, false, fmt.Errorf("invalid reader")
	}
	if ctx == nil {
		return nil, false, fmt.Errorf("invalid optional content context")
	}
	if err := ctx.Err(); err != nil {
		return nil, false, err
	}
	if r.closed {
		return nil, false, os.ErrClosed
	}
	root, err := r.optionalDictionary(r.Trailer["Root"])
	if err != nil {
		return nil, false, err
	}
	properties, err := r.optionalDictionary(root["OCProperties"])
	if err != nil {
		return nil, false, err
	}
	if properties == nil {
		return nil, false, nil
	}
	config, err := r.optionalDictionary(properties["D"])
	if err != nil {
		return nil, false, err
	}
	return &optionalContentEvaluator{
		reader:     r,
		ctx:        ctx,
		config:     config,
		refMemo:    map[Reference]bool{},
		directMemo: map[optionalContentKey]optionalContentDirectEntry{},
		active:     map[optionalContentKey]bool{},
	}, true, nil
}

// optionalContentVisibleContext 按文档默认配置及取消上下文判断可选内容可见性
// 入参: ctx 取消上下文, object OCG引用或OCMD字典
// 返回: bool 是否可见, error 配置、表达式、超限或取消错误
func (r *Reader) optionalContentVisibleContext(ctx context.Context, object Object) (bool, error) {
	evaluator, hasOC, err := r.newOptionalContentEvaluator(ctx)
	if err != nil || !hasOC {
		return !hasOC && err == nil, err
	}
	return evaluator.eval(object, 0)
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

// eval 递归求解成员策略、组状态及可见性表达式，带记忆化、步数上限与环路检测
// 入参: object 组或表达式, depth 嵌套深度
// 返回: bool 是否可见, error 解析、环路、超限或取消错误
func (e *optionalContentEvaluator) eval(object Object, depth int) (bool, error) {
	if depth == 0 {
		defer clear(e.directMemo)
	}
	if err := e.ctx.Err(); err != nil {
		return false, err
	}
	e.steps++
	if e.steps > maxOptionalContentSteps {
		return false, fmt.Errorf("%w: optional content evaluation step limit exceeded", ErrStreamLimitExceeded)
	}
	if depth >= 64 {
		return false, fmt.Errorf("optional content recursion limit exceeded")
	}
	ref, value, err := e.reader.resolveIndirect(object)
	if err != nil {
		return false, err
	}
	isRef := ref != (Reference{})
	var key optionalContentKey
	hasKey := false
	if isRef {
		key, hasKey = optionalContentKey{ref: ref}, true
	} else {
		switch current := value.(type) {
		case Dictionary:
			if ptr := reflect.ValueOf(current).Pointer(); ptr != 0 {
				key, hasKey = optionalContentKey{kind: 1, ptr: ptr}, true
			}
		case Array:
			if len(current) > 0 {
				if ptr := reflect.ValueOf(current).Pointer(); ptr != 0 {
					key, hasKey = optionalContentKey{kind: 2, ptr: ptr, length: len(current)}, true
				}
			}
		}
	}
	if hasKey {
		if e.active[key] {
			return false, fmt.Errorf("cyclic optional content reference")
		}
		if isRef {
			if cached, ok := e.refMemo[ref]; ok {
				return cached, nil
			}
		} else if cached, ok := e.directMemo[key]; ok {
			return cached.visible, nil
		}
		e.active[key] = true
		defer delete(e.active, key)
	}
	result, err := e.evalValue(ref, value, depth)
	if err != nil {
		return false, err
	}
	if hasKey {
		if isRef {
			if len(e.refMemo) >= maxOptionalContentSteps {
				clear(e.refMemo)
			}
			e.refMemo[ref] = result
		} else {
			e.directMemo[key] = optionalContentDirectEntry{keep: value, visible: result}
		}
	}
	return result, nil
}

// evalValue 对已解析的可选内容数组或字典节点执行求值
// 入参: ref 规范化间接引用, value 已解析对象, depth 嵌套深度
// 返回: bool 是否可见, error 格式、策略或取消错误
func (e *optionalContentEvaluator) evalValue(ref Reference, value Object, depth int) (bool, error) {
	if expression, ok := value.(Array); ok {
		if len(expression) < 2 {
			return false, fmt.Errorf("invalid optional content expression")
		}
		opVal, err := e.reader.Resolve(expression[0])
		if err != nil {
			return false, err
		}
		op, ok := opVal.(Name)
		if !ok || op != "And" && op != "Or" && op != "Not" || op == "Not" && len(expression) != 2 {
			return false, fmt.Errorf("invalid optional content expression operator")
		}
		result := op == "And"
	expr:
		for _, operand := range expression[1:] {
			state, err := e.eval(operand, depth+1)
			if err != nil {
				return false, err
			}
			switch op {
			case "And":
				if !state {
					result = false
					break expr
				}
			case "Or":
				if state {
					result = true
					break expr
				}
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
	kind, err := e.reader.Resolve(dict["Type"])
	if err != nil {
		return false, err
	}
	switch kind {
	case Name("OCG"):
		return e.groupState(ref, dict)
	case Name("OCMD"):
		if dict["VE"] != nil {
			ve, err := e.reader.Resolve(dict["VE"])
			if err != nil {
				return false, err
			}
			if ve != nil {
				if _, ok := ve.(Array); !ok {
					return false, fmt.Errorf("invalid optional content expression")
				}
				return e.eval(dict["VE"], depth+1)
			}
		}
		groups, err := e.reader.Resolve(dict["OCGs"])
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
		policy, err := e.reader.Resolve(dict["P"])
		if err != nil {
			return false, err
		}
		if policy == nil {
			policy = Name("AnyOn")
		}
		if policy != Name("AnyOn") && policy != Name("AllOn") && policy != Name("AnyOff") && policy != Name("AllOff") {
			return false, fmt.Errorf("invalid optional content policy")
		}
		all := policy == Name("AllOn") || policy == Name("AllOff")
		result := all
		count := 0
		for _, group := range list {
			if err := e.ctx.Err(); err != nil {
				return false, err
			}
			resolved, err := e.reader.Resolve(group)
			if err != nil {
				return false, err
			}
			if resolved == nil {
				e.steps++
				if e.steps > maxOptionalContentSteps {
					return false, fmt.Errorf("%w: optional content evaluation step limit exceeded", ErrStreamLimitExceeded)
				}
				continue
			}
			count++
			state, err := e.eval(group, depth+1)
			if err != nil {
				return false, err
			}
			if policy == Name("AnyOff") || policy == Name("AllOff") {
				state = !state
			}
			if all {
				if !state {
					result = false
					break
				}
			} else if state {
				result = true
				break
			}
		}
		if count == 0 {
			return true, nil
		}
		return result, nil
	default:
		return false, fmt.Errorf("invalid optional content type")
	}
}

// intents 解析可选内容意图名称列表，缺省视为View
// 入参: object 意图名称或数组
// 返回: []Name 意图名称列表, error 解析、超限或取消错误
func (e *optionalContentEvaluator) intents(object Object) ([]Name, error) {
	if err := e.ctx.Err(); err != nil {
		return nil, err
	}
	value, err := e.reader.Resolve(object)
	if err != nil {
		return nil, err
	}
	if value == nil {
		return []Name{"View"}, nil
	}
	if name, ok := value.(Name); ok {
		return []Name{name}, nil
	}
	array, ok := value.(Array)
	if !ok {
		return nil, fmt.Errorf("invalid optional content intent")
	}
	if len(array) > maxOptionalContentSteps-e.steps {
		return nil, fmt.Errorf("%w: optional content evaluation step limit exceeded", ErrStreamLimitExceeded)
	}
	resolved := make([]Name, len(array))
	for i, item := range array {
		if i&255 == 0 {
			if err := e.ctx.Err(); err != nil {
				return nil, err
			}
		}
		e.steps++
		item, err := e.reader.Resolve(item)
		if err != nil {
			return nil, err
		}
		name, ok := item.(Name)
		if !ok {
			return nil, fmt.Errorf("invalid optional content intent")
		}
		resolved[i] = name
	}
	return resolved, nil
}

// loadGroupStates 懒加载默认配置的基础状态与显式开关列表，规范化间接引用链
// 返回: error 配置、超限或取消错误
func (e *optionalContentEvaluator) loadGroupStates() error {
	if e.stateReady {
		return nil
	}
	baseState, err := e.reader.Resolve(e.config["BaseState"])
	if err != nil {
		return err
	}
	state := true
	switch baseState {
	case nil, Name("ON"), Name("Unchanged"):
	case Name("OFF"):
		state = false
	default:
		return fmt.Errorf("invalid optional content base state")
	}
	groupStates := map[Reference]bool{}
	for _, key := range []Name{"ON", "OFF"} {
		value, err := e.reader.Resolve(e.config[key])
		if err != nil {
			return err
		}
		if value == nil {
			continue
		}
		list, ok := value.(Array)
		if !ok {
			return fmt.Errorf("invalid optional content state list")
		}
		if err := e.ctx.Err(); err != nil {
			return err
		}
		if len(list) > maxOptionalContentSteps-e.steps {
			return fmt.Errorf("%w: optional content evaluation step limit exceeded", ErrStreamLimitExceeded)
		}
		on := key == "ON"
		for i, item := range list {
			if i&255 == 0 {
				if err := e.ctx.Err(); err != nil {
					return err
				}
			}
			e.steps++
			if _, ok := item.(Reference); !ok {
				return fmt.Errorf("invalid optional content group reference")
			}
			ref, err := e.reader.resolveIndirectReference(item)
			if err != nil {
				return err
			}
			groupStates[ref] = on
		}
	}
	e.baseState, e.groupStates, e.stateReady = state, groupStates, true
	return nil
}

// loadDictStates 为直接传入的OCG字典按指针建立开关索引
// 返回: error 引用解析、超限或取消错误
func (e *optionalContentEvaluator) loadDictStates() error {
	if e.dictReady {
		return nil
	}
	if err := e.ctx.Err(); err != nil {
		return err
	}
	if len(e.groupStates) > maxOptionalContentSteps-e.steps {
		return fmt.Errorf("%w: optional content evaluation step limit exceeded", ErrStreamLimitExceeded)
	}
	dictStates := map[uintptr]optionalContentDictState{}
	index := 0
	for ref, on := range e.groupStates {
		if index&255 == 0 {
			if err := e.ctx.Err(); err != nil {
				return err
			}
		}
		index++
		e.steps++
		resolved, err := e.reader.Resolve(ref)
		if err != nil {
			return err
		}
		if dict, ok := resolved.(Dictionary); ok {
			if ptr := reflect.ValueOf(dict).Pointer(); ptr != 0 {
				dictStates[ptr] = optionalContentDictState{dict: dict, on: on}
			}
		}
	}
	e.dictStates, e.dictReady = dictStates, true
	return nil
}

// groupState 按配置意图、基础状态和显式开关确定组状态
// 入参: currentRef 规范化后的组引用，直接字典时为零值, group 组字典
// 返回: bool 是否可见, error 配置、超限或取消错误
func (e *optionalContentEvaluator) groupState(currentRef Reference, group Dictionary) (bool, error) {
	a, err := e.intents(group["Intent"])
	if err != nil {
		return false, err
	}
	if !e.intentsReady {
		b, err := e.intents(e.config["Intent"])
		if err != nil {
			return false, err
		}
		set := make(map[Name]bool, len(b))
		for i, y := range b {
			if i&255 == 0 {
				if err := e.ctx.Err(); err != nil {
					return false, err
				}
			}
			if y == Name("All") {
				e.configIntentAll = true
				break
			}
			set[y] = true
		}
		e.configIntentSet, e.intentsReady = set, true
	}
	match := e.configIntentAll && len(a) > 0
	if !match {
		for i, x := range a {
			if i&255 == 0 {
				if err := e.ctx.Err(); err != nil {
					return false, err
				}
			}
			e.steps++
			if e.steps > maxOptionalContentSteps {
				return false, fmt.Errorf("%w: optional content evaluation step limit exceeded", ErrStreamLimitExceeded)
			}
			if e.configIntentSet[x] {
				match = true
				break
			}
		}
	}
	if !match {
		return true, nil
	}
	if err := e.loadGroupStates(); err != nil {
		return false, err
	}
	if currentRef != (Reference{}) {
		if state, ok := e.groupStates[currentRef]; ok {
			return state, nil
		}
		return e.baseState, nil
	}
	if err := e.loadDictStates(); err != nil {
		return false, err
	}
	if ptr := reflect.ValueOf(group).Pointer(); ptr != 0 {
		if state, ok := e.dictStates[ptr]; ok {
			return state.on, nil
		}
	}
	return e.baseState, nil
}

// optionalVisible 使用调用方状态或页面级缓存的默认配置求值器判断可选内容是否绘制
// 入参: object 可选内容属性
// 返回: bool 是否可见, error 配置、超限或取消错误
func (p *pageInterpreter) optionalVisible(object Object) (bool, error) {
	if err := p.ctx.Err(); err != nil {
		return false, err
	}
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
	walk := p.ensureWalkState()
	if walk.ocReader != p.reader {
		evaluator, hasOC, err := p.reader.newOptionalContentEvaluator(p.ctx)
		if err != nil {
			return false, err
		}
		walk.ocReader = p.reader
		walk.ocEvaluator = evaluator
		walk.ocNone = !hasOC
	}
	if walk.ocNone {
		return true, p.ctx.Err()
	}
	evaluator := walk.ocEvaluator
	evaluator.ctx = p.ctx
	evaluator.steps = 0
	clear(evaluator.active)
	return evaluator.eval(object, 0)
}
