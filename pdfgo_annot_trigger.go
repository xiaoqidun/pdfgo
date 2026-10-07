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

// ReadAnnotationTriggers 读取注解生效事件，主动作优先于U，焦点事件仅适用于Widget
// 未知事件不激活；返回字典为只读视图，动作链可交给WalkActions遍历
// 入参: ctx 取消上下文, annotation 注解
// 返回: []ActionTrigger 按标准表顺序排列的事件, error 字典、引用或取消错误
func (r *Reader) ReadAnnotationTriggers(ctx context.Context, annotation Annotation) ([]ActionTrigger, error) {
	if ctx == nil {
		return nil, fmt.Errorf("invalid annotation trigger context")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	value, err := r.Resolve(annotation.Dictionary["AA"])
	if err != nil || value == nil {
		return nil, err
	}
	additional, ok := value.(Dictionary)
	if !ok {
		return nil, fmt.Errorf("invalid annotation additional-actions dictionary")
	}
	var result []ActionTrigger
	for _, event := range []Name{"E", "X", "D", "U", "Fo", "Bl", "PO", "PC", "PV", "PI"} {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if (event == "Fo" || event == "Bl") && annotation.Subtype != "Widget" {
			continue
		}
		if additional[event] == nil {
			continue
		}
		if event == "U" {
			primary, err := r.Resolve(annotation.Dictionary["A"])
			if err != nil {
				return nil, err
			}
			if primary != nil {
				continue
			}
		}
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
