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

// ActionTrigger 保存附加动作的事件和只读字典，不执行动作或展开Next链
type ActionTrigger struct {
	Event  Name
	Action Dictionary
}

// OpenAction 区分文档初始目标与打开动作，均为空时使用阅读器默认视图
type OpenAction struct {
	Destination *Destination
	Action      Dictionary
}

// ReadOpenAction 读取文档初始目标或打开动作，不跳转页面或执行动作
// 入参: ctx 取消上下文
// 返回: OpenAction 打开行为, error 类型、目标或取消错误
func (r *Reader) ReadOpenAction(ctx context.Context) (OpenAction, error) {
	if ctx == nil {
		return OpenAction{}, fmt.Errorf("invalid action context")
	}
	if err := ctx.Err(); err != nil {
		return OpenAction{}, err
	}
	catalog, err := r.catalogDictionary()
	if err != nil {
		return OpenAction{}, err
	}
	value, err := r.Resolve(catalog["OpenAction"])
	if err != nil || value == nil {
		return OpenAction{}, err
	}
	switch value.(type) {
	case Array:
		destination, err := r.readDestination(ctx, value)
		if err != nil {
			return OpenAction{}, err
		}
		return OpenAction{Destination: &destination}, ctx.Err()
	case Dictionary:
		action, err := r.readActionTrigger(ctx, "OpenAction", value)
		if err != nil {
			return OpenAction{}, err
		}
		return OpenAction{Action: action.Action}, ctx.Err()
	default:
		return OpenAction{}, fmt.Errorf("invalid document open action")
	}
}

// ReadDocumentTriggers 读取文档关闭、保存和打印事件，标准要求起始动作为JavaScript
// 入参: ctx 取消上下文
// 返回: []ActionTrigger 附加动作, error 类型、引用或取消错误
func (r *Reader) ReadDocumentTriggers(ctx context.Context) ([]ActionTrigger, error) {
	if ctx == nil {
		return nil, fmt.Errorf("invalid action context")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	catalog, err := r.catalogDictionary()
	if err != nil {
		return nil, err
	}
	triggers, err := r.readAdditionalActions(ctx, catalog["AA"], []Name{"WC", "WS", "DS", "WP", "DP"})
	if err != nil {
		return nil, err
	}
	for _, trigger := range triggers {
		if _, err := r.actionDictionary(ctx, trigger.Action, "JavaScript"); err != nil {
			return nil, err
		}
	}
	return triggers, ctx.Err()
}

// ReadPageTriggers 读取页面打开和关闭动作，不从父级页树继承AA
// 入参: ctx 取消上下文, page 页面
// 返回: []ActionTrigger 附加动作, error 类型、引用或取消错误
func (r *Reader) ReadPageTriggers(ctx context.Context, page *Page) ([]ActionTrigger, error) {
	if page == nil {
		return nil, fmt.Errorf("missing action page")
	}
	return r.readAdditionalActions(ctx, page.Dictionary["AA"], []Name{"O", "C"})
}

// readAdditionalActions 按给定顺序读取附加动作，忽略未知事件和空值
// 入参: ctx 取消上下文, object 附加动作字典, events 有效事件
// 返回: []ActionTrigger 附加动作, error 类型、引用或取消错误
func (r *Reader) readAdditionalActions(ctx context.Context, object Object, events []Name) ([]ActionTrigger, error) {
	if ctx == nil {
		return nil, fmt.Errorf("invalid action context")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	value, err := r.Resolve(object)
	if err != nil || value == nil {
		return nil, err
	}
	additional, ok := value.(Dictionary)
	if !ok {
		return nil, fmt.Errorf("invalid additional-actions dictionary")
	}
	var result []ActionTrigger
	for _, event := range events {
		trigger, err := r.readActionTrigger(ctx, event, additional[event])
		if err != nil {
			return nil, err
		}
		if trigger != nil {
			result = append(result, *trigger)
		}
	}
	return result, ctx.Err()
}

// readActionTrigger 校验单个事件的动作字典，空值表示未定义事件
// 入参: ctx 取消上下文, event 事件名, object 动作对象
// 返回: *ActionTrigger 动作, error 类型、引用或取消错误
func (r *Reader) readActionTrigger(ctx context.Context, event Name, object Object) (*ActionTrigger, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	value, err := r.Resolve(object)
	if err != nil || value == nil {
		return nil, err
	}
	action, ok := value.(Dictionary)
	if !ok {
		return nil, fmt.Errorf("invalid %s action", event)
	}
	kind, err := r.Resolve(action["S"])
	if err != nil {
		return nil, err
	}
	name, ok := kind.(Name)
	if !ok || name == "" {
		return nil, fmt.Errorf("invalid %s action type", event)
	}
	action, err = r.actionDictionary(ctx, action, name)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", event, err)
	}
	return &ActionTrigger{Event: event, Action: action}, nil
}
