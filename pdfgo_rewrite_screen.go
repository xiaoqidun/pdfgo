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
	"math"
	"slices"
)

// ScreenAnnotation 描述媒体播放区域及点击动作，Rect使用默认用户空间，Title使用UTF-8文字
// Appearance须为表单流，空值写出空白外观；嵌套引用按流所属Reader解析
// Actions为空时不添加点击动作，不自动启动媒体，也不创建页面跳转
// Hidden隐藏静态注解并关闭其鼠标交互，不禁止外部动作在该区域播放媒体
type ScreenAnnotation struct {
	Rect       Rectangle
	Title      string
	Appearance *Stream
	Actions    []NavigationAction
	Hidden     bool
}

// screenAnnotationReplacements 追加屏幕注解并记录页面归属，不改变原注解顺序
// 入参: ctx 取消上下文, additions 页面及新增注解, result 已有替换对象
// 返回: map[*ScreenAnnotation]Reference 新增注解引用, error 页面、外观或取消错误
func (r *Reader) screenAnnotationReplacements(ctx context.Context, additions map[Reference][]*ScreenAnnotation, result map[Reference]Object) (map[*ScreenAnnotation]Reference, error) {
	if len(additions) == 0 {
		return nil, nil
	}
	number := int64(0)
	for id := range r.xref {
		number = max(number, id)
	}
	for ref := range result {
		number = max(number, ref.Number)
	}
	refs := make(map[*ScreenAnnotation]Reference)
	visited := make(map[Reference]bool)
	copier := rewriteObjectCopy{ctx: ctx, source: r, objects: result, number: &number, copied: make(map[rewriteObjectIdentity]Reference)}
	empty := &Stream{Dictionary: Dictionary{"Type": Name("XObject"), "Subtype": Name("Form"), "BBox": Array{Integer(0), Integer(0), Integer(1), Integer(1)}}}
	err := r.WalkPages(ctx, func(_ int, source *Page) error {
		annotations, exists := additions[source.Reference]
		if !exists {
			return nil
		}
		visited[source.Reference] = true
		if len(annotations) == 0 {
			return nil
		}
		page, err := r.rewriteDictionary(source.Reference, result)
		if err != nil {
			return err
		}
		value, err := r.Resolve(page["Annots"])
		if err != nil {
			return err
		}
		var array Array
		if value != nil {
			var ok bool
			array, ok = value.(Array)
			if !ok {
				return fmt.Errorf("invalid PDF page annotations")
			}
			array = slices.Clone(array)
		}
		for _, annotation := range annotations {
			if err := ctx.Err(); err != nil {
				return err
			}
			if annotation == nil || refs[annotation] != (Reference{}) {
				return fmt.Errorf("missing or repeated screen annotation")
			}
			box := annotation.Rect
			for _, value := range []float64{box.XMin, box.YMin, box.XMax, box.YMax} {
				if math.IsNaN(value) || math.IsInf(value, 0) {
					return fmt.Errorf("invalid screen annotation rectangle")
				}
			}
			if box.XMin > box.XMax || box.YMin > box.YMax {
				return fmt.Errorf("inverted screen annotation rectangle")
			}
			title, err := EncodeTextString(annotation.Title)
			if err != nil {
				return err
			}
			appearance := annotation.Appearance
			if appearance == nil {
				appearance = empty
			}
			if err := r.validateMediaForm(appearance); err != nil {
				return err
			}
			form, err := copier.copy(appearance, 0)
			if err != nil {
				return err
			}
			dict := Dictionary{"Type": Name("Annot"), "Subtype": Name("Screen"), "P": source.Reference, "Rect": Array{Real(box.XMin), Real(box.YMin), Real(box.XMax), Real(box.YMax)}, "T": title, "AP": Dictionary{"N": form}}
			if annotation.Hidden {
				dict["F"] = Integer(2)
			}
			ref, err := appendRewriteObject(result, &number, dict)
			if err != nil {
				return err
			}
			refs[annotation] = ref
			array = append(array, ref)
		}
		page["Annots"] = array
		result[source.Reference] = page
		return nil
	})
	if err != nil {
		return nil, err
	}
	if len(visited) != len(additions) {
		return nil, fmt.Errorf("screen annotation page is not in the page tree")
	}
	return refs, ctx.Err()
}

// validateMediaForm 校验媒体表单的类型和边界，不解码内容流
// 入参: form 表单流
// 返回: error 类型、边界或引用错误
func (r *Reader) validateMediaForm(form *Stream) error {
	if form == nil {
		return fmt.Errorf("missing media form")
	}
	source := form.reader
	if source == nil {
		source = r
	}
	kind, err := source.Resolve(form.Dictionary["Subtype"])
	if err != nil {
		return err
	}
	if kind != Name("Form") {
		return fmt.Errorf("invalid media form subtype")
	}
	value, err := source.Resolve(form.Dictionary["Type"])
	if err != nil {
		return err
	}
	if value != nil && value != Name("XObject") {
		return fmt.Errorf("invalid media form type")
	}
	_, err = source.rectangle(form.Dictionary["BBox"])
	return err
}

// screenAnnotationActions 在目标引用确定后绑定屏幕注解的点击动作
// 入参: ctx 取消上下文, options 替换配置, added 新增注解, result 已有替换对象, resources 动作资源
// 返回: error 动作、目标或取消错误
func (r *Reader) screenAnnotationActions(ctx context.Context, options RewriteOptions, added map[*ScreenAnnotation]Reference, result map[Reference]Object, resources *rewriteResources) error {
	if len(added) == 0 {
		return nil
	}
	pages := make(map[Reference]bool)
	if err := r.WalkPages(ctx, func(_ int, page *Page) error {
		pages[page.Reference] = true
		return nil
	}); err != nil {
		return err
	}
	for _, annotations := range options.ScreenAnnotations {
		for _, annotation := range annotations {
			action, err := r.navigationAction(ctx, annotation.Actions, pages, resources)
			if err != nil {
				return err
			}
			if action != nil {
				result[added[annotation]].(Dictionary)["A"] = action
			}
		}
	}
	return ctx.Err()
}
