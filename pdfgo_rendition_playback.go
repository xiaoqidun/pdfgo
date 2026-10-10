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

// RenditionPlayback 保存媒体动作、屏幕归属及脚本后备操作，不执行播放或脚本
// Operation为空表示仅有脚本；同时存在时Script优先，Operation用于无法执行脚本的环境
// Page为屏幕声明的所属页，调用方可进一步检查其是否在所需页面集合中
type RenditionPlayback struct {
	Annotation Annotation
	Page       Reference
	Operation  *int64
	Rendition  *Rendition
	Script     *JavaScriptAction
}

// ReadRenditionPlayback 按PDF表214读取媒体动作，校验屏幕在所属页注解数组中的引用
// 入参: ctx 取消上下文, object 动作字典或引用
// 返回: RenditionPlayback 媒体动作, error 参数、目标、资源或取消错误
func (r *Reader) ReadRenditionPlayback(ctx context.Context, object Object) (RenditionPlayback, error) {
	var result RenditionPlayback
	if r == nil || ctx == nil {
		return result, fmt.Errorf("invalid rendition playback context")
	}
	dict, err := r.actionDictionary(ctx, object, "Rendition")
	if err != nil {
		return result, err
	}
	js, err := r.Resolve(dict["JS"])
	if err != nil {
		return result, err
	}
	if js != nil {
		script, err := r.ReadJavaScriptAction(ctx, Dictionary{"S": Name("JavaScript"), "JS": js})
		if err != nil {
			return result, err
		}
		result.Script = &script
	}
	value, err := r.Resolve(dict["OP"])
	if err != nil {
		return result, err
	}
	if value != nil {
		op, ok := value.(Integer)
		if !ok {
			return result, fmt.Errorf("%w: invalid rendition operation", ErrInvalidAction)
		}
		operation := int64(op)
		if (operation < 0 || operation > 4) && result.Script == nil {
			return result, fmt.Errorf("%w: unrecognized rendition operation", ErrInvalidAction)
		}
		result.Operation = &operation
	} else if result.Script == nil {
		return result, fmt.Errorf("%w: missing rendition operation or script", ErrInvalidAction)
	}
	value, err = r.Resolve(dict["R"])
	if err != nil {
		return result, err
	}
	if value != nil {
		result.Rendition, err = r.ReadRendition(ctx, value)
		if err != nil {
			return result, err
		}
	} else if result.Operation != nil && (*result.Operation == 0 || *result.Operation == 4) {
		return result, fmt.Errorf("%w: missing media rendition", ErrInvalidAction)
	}
	value, err = r.Resolve(dict["AN"])
	if err != nil {
		return result, err
	}
	if value == nil && (result.Operation == nil || *result.Operation < 0 || *result.Operation > 4) {
		return result, ctx.Err()
	}
	ref, ok := dict["AN"].(Reference)
	if !ok {
		return result, fmt.Errorf("%w: missing screen annotation reference", ErrInvalidAction)
	}
	screen, ok := value.(Dictionary)
	if !ok {
		return result, fmt.Errorf("%w: invalid screen annotation", ErrInvalidAction)
	}
	kind, err := r.Resolve(screen["Subtype"])
	if err != nil {
		return result, err
	}
	if kind != Name("Screen") {
		return result, fmt.Errorf("%w: rendition target is not a screen annotation", ErrInvalidAction)
	}
	page, ok := screen["P"].(Reference)
	if !ok {
		return result, fmt.Errorf("%w: missing screen page reference", ErrInvalidAction)
	}
	value, err = r.Resolve(page)
	if err != nil {
		return result, err
	}
	owner, ok := value.(Dictionary)
	if !ok {
		return result, fmt.Errorf("%w: invalid screen page", ErrInvalidAction)
	}
	kind, err = r.Resolve(owner["Type"])
	if err != nil {
		return result, err
	}
	if kind != Name("Page") {
		return result, fmt.Errorf("%w: invalid screen page type", ErrInvalidAction)
	}
	value, err = r.Resolve(owner["Annots"])
	if err != nil {
		return result, err
	}
	annots, ok := value.(Array)
	if !ok {
		return result, fmt.Errorf("%w: invalid screen page annotations", ErrInvalidAction)
	}
	found := false
	for _, object := range annots {
		if err := ctx.Err(); err != nil {
			return result, err
		}
		if current, ok := object.(Reference); ok && current == ref {
			found = true
			break
		}
	}
	if !found {
		return result, fmt.Errorf("%w: screen annotation is not on its declared page", ErrInvalidAction)
	}
	box, err := r.rectangle(screen["Rect"])
	if err != nil {
		return result, err
	}
	result.Annotation = Annotation{Reference: ref, Subtype: "Screen", Rect: box, Dictionary: screen}
	result.Page = page
	return result, ctx.Err()
}
