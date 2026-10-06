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

// ActionTarget 保存注解或字段的间接引用或完整字段名称，两者仅有一个有效
type ActionTarget struct {
	Reference Reference
	Name      string
}

// ResetFormAction 保存表单重置范围，不修改字段值
// AllFields表示Fields缺省，此时忽略Exclude；空数组与缺省范围不同
type ResetFormAction struct {
	Fields    []ActionTarget
	AllFields bool
	Exclude   bool
}

// ImportDataAction 保存FDF导入目标，不读取文件或修改表单
type ImportDataAction struct {
	File FileSpecification
}

// HideAction 保存注解或表单字段的显示动作，不改变可见性
type HideAction struct {
	Targets []ActionTarget
	Hide    bool
}

// ReadResetFormAction 按PDF标准表238和表239解析重置范围及排除标志
// 入参: ctx 取消上下文, object 动作字典或引用
// 返回: ResetFormAction 重置范围, error 字段、标志、引用或取消错误
func (r *Reader) ReadResetFormAction(ctx context.Context, object Object) (ResetFormAction, error) {
	var result ResetFormAction
	dict, err := r.actionDictionary(ctx, object, "ResetForm")
	if err != nil {
		return result, err
	}
	value, err := r.Resolve(dict["Flags"])
	if err != nil {
		return result, err
	}
	if value != nil {
		flags, ok := value.(Integer)
		if !ok || flags < 0 || flags&^1 != 0 {
			return result, fmt.Errorf("invalid reset-form flags")
		}
		result.Exclude = flags&1 != 0
	}
	value, err = r.Resolve(dict["Fields"])
	if err != nil {
		return result, err
	}
	if value == nil {
		result.AllFields = true
		return result, ctx.Err()
	}
	array, ok := value.(Array)
	if !ok {
		return result, fmt.Errorf("invalid reset-form fields")
	}
	result.Fields, err = r.actionTargets(ctx, array, false)
	return result, err
}

// ReadImportDataAction 按PDF标准表240解析FDF文件说明，不访问外部文件
// 入参: ctx 取消上下文, object 动作字典或引用
// 返回: ImportDataAction 文件信息, error 字典、文件说明或取消错误
func (r *Reader) ReadImportDataAction(ctx context.Context, object Object) (ImportDataAction, error) {
	var result ImportDataAction
	dict, err := r.actionDictionary(ctx, object, "ImportData")
	if err != nil {
		return result, err
	}
	result.File, err = r.ReadFileSpecification(dict["F"])
	if err != nil {
		return result, err
	}
	return result, ctx.Err()
}

// ReadHideAction 按PDF标准表210解析注解及字段的隐藏或显示目标
// 入参: ctx 取消上下文, object 动作字典或引用
// 返回: HideAction 目标和可见性操作, error 目标、标志或取消错误
func (r *Reader) ReadHideAction(ctx context.Context, object Object) (HideAction, error) {
	result := HideAction{Hide: true}
	dict, err := r.actionDictionary(ctx, object, "Hide")
	if err != nil {
		return result, err
	}
	value, err := r.Resolve(dict["H"])
	if err != nil {
		return result, err
	}
	if value != nil {
		flag, ok := value.(Boolean)
		if !ok {
			return result, fmt.Errorf("invalid hide action flag")
		}
		result.Hide = bool(flag)
	}
	value, err = r.Resolve(dict["T"])
	if err != nil {
		return result, err
	}
	targets, ok := value.(Array)
	if !ok {
		targets = Array{dict["T"]}
	}
	result.Targets, err = r.actionTargets(ctx, targets, true)
	return result, err
}

// actionTargets 读取动作目标，区分字段引用与注解引用，不修改源字典
// 入参: ctx 取消上下文, objects 混合目标数组, annotations 是否限定注解引用
// 返回: []ActionTarget 引用和字段名称, error 结构、文字或取消错误
func (r *Reader) actionTargets(ctx context.Context, objects Array, annotations bool) ([]ActionTarget, error) {
	result := make([]ActionTarget, 0, len(objects))
	for _, object := range objects {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		value, err := r.Resolve(object)
		if err != nil {
			return nil, err
		}
		if text, ok := value.(String); ok {
			name, err := DecodeTextString(text)
			if err != nil {
				return nil, err
			}
			result = append(result, ActionTarget{Name: name})
			continue
		}
		ref, indirect := object.(Reference)
		dict, valid := value.(Dictionary)
		if !indirect || ref.Number <= 0 || !valid || dict == nil {
			return nil, fmt.Errorf("invalid action target")
		}
		if annotations {
			subtype, err := r.Resolve(dict["Subtype"])
			if err != nil {
				return nil, err
			}
			if name, ok := subtype.(Name); !ok || name == "" {
				return nil, fmt.Errorf("invalid hide annotation target")
			}
		}
		result = append(result, ActionTarget{Reference: ref})
	}
	return result, ctx.Err()
}
