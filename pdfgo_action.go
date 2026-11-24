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
	"errors"
	"fmt"
	"maps"
	"os"
	"reflect"
)

// maxActionWalkSteps 限制WalkActions单次遍历最大访问步数，防止有向无环图引起指数级展开
const maxActionWalkSteps = 65536

// ErrInvalidAction 表示已识别的动作结构或参数错误，不包含资源读取和取消错误
var ErrInvalidAction = errors.New("invalid action")

// wrapActionError 将动作解析期产生的结构或参数错误统一包装为ErrInvalidAction，保留读取、取消、关闭及未支持错误
// 入参: err 原始错误
// 返回: error 包装后的错误
func wrapActionError(err error) error {
	if err == nil || errors.Is(err, ErrInvalidAction) || errors.Is(err, os.ErrClosed) || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	var readErr *ReadError
	var unsupported *UnsupportedError
	var security *objectSecurityError
	if errors.As(err, &readErr) || errors.As(err, &unsupported) || errors.As(err, &security) {
		return err
	}
	return fmt.Errorf("%w: %w", ErrInvalidAction, err)
}

// ActionInfo 保存动作类型和原始属性，不执行脚本、启动程序或访问网络
type ActionInfo struct {
	Type       Name
	Dictionary Dictionary
}

// actionFrame 保存动作链遍历位置，退出标记用于识别循环而非重复引用
type actionFrame struct {
	object   Object
	leave    uintptr
	leaveRef Reference
}

// WalkActions 按动作及Next顺序遍历动作链，允许不同分支共享动作并限制访问步数
// 入参: ctx 取消上下文, object 起始动作, visit 动作访问器
// 返回: error 类型、循环引用、步数超限或访问器错误
func (r *Reader) WalkActions(ctx context.Context, object Object, visit func(ActionInfo) error) error {
	if r == nil || ctx == nil {
		return fmt.Errorf("invalid action context")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if r.closed {
		return os.ErrClosed
	}
	if visit == nil {
		return fmt.Errorf("missing action visitor")
	}
	resolved, err := r.Resolve(object)
	if err != nil {
		return err
	}
	if resolved == nil {
		return ctx.Err()
	}
	stack := []actionFrame{{object: object}}
	active := map[uintptr]bool{}
	activeRefs := map[Reference]bool{}
	steps := 0
	for len(stack) > 0 {
		if err := ctx.Err(); err != nil {
			return err
		}
		frame := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if frame.leave != 0 || frame.leaveRef.Number != 0 {
			if frame.leave != 0 {
				delete(active, frame.leave)
			}
			if frame.leaveRef.Number != 0 {
				delete(activeRefs, frame.leaveRef)
			}
			continue
		}
		steps++
		if steps > maxActionWalkSteps {
			return fmt.Errorf("%w: action chain limit exceeded", ErrInvalidAction)
		}
		ref, isRef := frame.object.(Reference)
		if isRef && activeRefs[ref] {
			return fmt.Errorf("%w: cyclic action chain", ErrInvalidAction)
		}
		value, err := r.Resolve(frame.object)
		if err != nil {
			return err
		}
		dict, ok := value.(Dictionary)
		if !ok || dict == nil {
			return fmt.Errorf("%w: invalid action dictionary", ErrInvalidAction)
		}
		id := reflect.ValueOf(dict).Pointer()
		if active[id] {
			return fmt.Errorf("%w: cyclic action chain", ErrInvalidAction)
		}
		dictType, err := r.Resolve(dict["Type"])
		if err != nil {
			return err
		}
		if dictType != nil && dictType != Name("Action") {
			return fmt.Errorf("%w: dictionary type", ErrInvalidAction)
		}
		kind, err := r.Resolve(dict["S"])
		if err != nil {
			return err
		}
		name, ok := kind.(Name)
		if !ok || name == "" {
			return fmt.Errorf("%w: invalid action type", ErrInvalidAction)
		}
		cloned := maps.Clone(dict)
		cloned["S"] = name
		if err := visit(ActionInfo{Type: name, Dictionary: cloned}); err != nil {
			return err
		}
		next, err := r.Resolve(dict["Next"])
		if err != nil {
			return err
		}
		if next == nil {
			continue
		}
		active[id] = true
		var leaveRef Reference
		if isRef {
			activeRefs[ref] = true
			leaveRef = ref
		}
		stack = append(stack, actionFrame{object: dict, leave: id, leaveRef: leaveRef})
		if list, ok := next.(Array); ok {
			if len(stack)+len(list) > maxActionWalkSteps*2 {
				return fmt.Errorf("%w: action chain limit exceeded", ErrInvalidAction)
			}
			for i := len(list) - 1; i >= 0; i-- {
				stack = append(stack, actionFrame{object: list[i]})
			}
		} else {
			if nextRef, ok := dict["Next"].(Reference); ok {
				stack = append(stack, actionFrame{object: nextRef})
			} else {
				stack = append(stack, actionFrame{object: next})
			}
		}
	}
	return nil
}

// actionDictionary 读取指定标准动作的字典，不执行动作或访问外部资源
// 入参: ctx 取消上下文, object 动作字典或引用, kind 动作类型
// 返回: Dictionary 只读原始字典, error 类型、引用或取消错误
func (r *Reader) actionDictionary(ctx context.Context, object Object, kind Name) (Dictionary, error) {
	if r == nil || ctx == nil {
		return nil, fmt.Errorf("invalid action context")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if r.closed {
		return nil, os.ErrClosed
	}
	value, err := r.Resolve(object)
	if err != nil {
		return nil, err
	}
	dict, ok := value.(Dictionary)
	if !ok || dict == nil {
		return nil, fmt.Errorf("%w: %s dictionary", ErrInvalidAction, kind)
	}
	value, err = r.Resolve(dict["S"])
	if err != nil {
		return nil, err
	}
	if value != kind {
		return nil, fmt.Errorf("%w: %s type", ErrInvalidAction, kind)
	}
	value, err = r.Resolve(dict["Type"])
	if err != nil {
		return nil, err
	}
	if value != nil && value != Name("Action") {
		return nil, fmt.Errorf("%w: dictionary type", ErrInvalidAction)
	}
	return dict, ctx.Err()
}
