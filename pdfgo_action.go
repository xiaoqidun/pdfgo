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
	"maps"
	"reflect"
)

// ActionInfo 保存动作类型和原始属性，不执行脚本、启动程序或访问网络
type ActionInfo struct {
	Type       Name
	Dictionary Dictionary
}

// actionFrame 保存动作链遍历位置，退出标记用于识别循环而非重复引用
type actionFrame struct {
	object Object
	leave  uintptr
}

// WalkActions 按动作及Next顺序遍历动作链，允许不同分支共享动作
// 入参: ctx 取消上下文, object 起始动作, visit 动作访问器
// 返回: error 类型、循环引用或访问器错误
func (r *Reader) WalkActions(ctx context.Context, object Object, visit func(ActionInfo) error) error {
	if object == nil {
		return nil
	}
	if visit == nil {
		return fmt.Errorf("missing action visitor")
	}
	stack := []actionFrame{{object: object}}
	active := map[uintptr]bool{}
	for len(stack) > 0 {
		if err := ctx.Err(); err != nil {
			return err
		}
		frame := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if frame.leave != 0 {
			delete(active, frame.leave)
			continue
		}
		value, err := r.Resolve(frame.object)
		if err != nil {
			return err
		}
		dict, ok := value.(Dictionary)
		if !ok || dict == nil {
			return fmt.Errorf("invalid action dictionary")
		}
		id := reflect.ValueOf(dict).Pointer()
		if active[id] {
			return fmt.Errorf("cyclic action chain")
		}
		kind, err := r.Resolve(dict["S"])
		if err != nil {
			return err
		}
		name, ok := kind.(Name)
		if !ok || name == "" {
			return fmt.Errorf("invalid action type")
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
		stack = append(stack, actionFrame{object: dict, leave: id})
		if list, ok := next.(Array); ok {
			for i := len(list) - 1; i >= 0; i-- {
				stack = append(stack, actionFrame{object: list[i]})
			}
		} else {
			stack = append(stack, actionFrame{object: next})
		}
	}
	return nil
}
