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

// RenditionAction 描述媒体呈现操作，Annotation和Target分别引用原有或新增屏幕注解，须指定一项
// Operation取0至4，依次表示播放并替换、停止、暂停、恢复、播放或恢复；0和4须提供Rendition
// 呈现及片段按结构化字段写出，Dictionary不参与写出；参数中的引用须属于当前文件
// 同一呈现、片段或文件指针复用输出资源，不读取外部文件，不执行播放或脚本
type RenditionAction struct {
	Annotation Reference
	Target     *ScreenAnnotation
	Operation  int
	Rendition  *Rendition
}

// renditionOutput 保存呈现、片段及文件的写出状态，复用共享资源
type renditionOutput struct {
	reader     *Reader
	copy       rewriteObjectCopy
	renditions map[*Rendition]Reference
	clips      map[*MediaClip]Reference
	files      map[*FileSpecification]Reference
}

// renditionActions 校验屏幕归属并写出媒体动作及共享资源
// 入参: ctx 取消上下文, options 替换配置, added 新增屏幕引用, result 已有替换对象
// 返回: map[*RenditionAction]Dictionary 呈现动作, error 目标、媒体或取消错误
func (r *Reader) renditionActions(ctx context.Context, options RewriteOptions, added map[*ScreenAnnotation]Reference, result map[Reference]Object) (map[*RenditionAction]Dictionary, error) {
	prepared := make(map[*RenditionAction]Dictionary)
	refs := make(map[Reference]bool)
	for actions := range options.actionSequences() {
		for _, action := range actions {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			value := action.Rendition
			if value == nil {
				continue
			}
			if value.Operation < 0 || value.Operation > 4 || (value.Operation == 0 || value.Operation == 4) && value.Rendition == nil {
				return nil, fmt.Errorf("invalid rendition operation or missing media")
			}
			if value.Target != nil {
				if value.Annotation != (Reference{}) || added[value.Target] == (Reference{}) {
					return nil, fmt.Errorf("invalid new screen annotation target")
				}
			} else {
				if value.Annotation.Number <= 0 || value.Annotation.Generation < 0 || value.Annotation.Generation > 65535 {
					return nil, fmt.Errorf("invalid screen annotation reference")
				}
				refs[value.Annotation] = false
			}
			prepared[value] = nil
		}
	}
	if len(prepared) == 0 {
		return nil, ctx.Err()
	}
	if len(refs) != 0 {
		if err := r.WalkPages(ctx, func(_ int, page *Page) error {
			value, err := r.Resolve(page.Dictionary["Annots"])
			if err != nil || value == nil {
				return err
			}
			annotations, ok := value.(Array)
			if !ok {
				return fmt.Errorf("invalid PDF page annotations")
			}
			for _, object := range annotations {
				if err := ctx.Err(); err != nil {
					return err
				}
				ref, indirect := object.(Reference)
				if _, selected := refs[ref]; !indirect || !selected {
					continue
				}
				value, err := r.Resolve(ref)
				if err != nil {
					return err
				}
				dict, ok := value.(Dictionary)
				if !ok {
					return fmt.Errorf("invalid screen annotation")
				}
				kind, err := r.Resolve(dict["Subtype"])
				if err != nil {
					return err
				}
				if kind == Name("Screen") && dict["P"] == page.Reference {
					refs[ref] = true
				}
			}
			return nil
		}); err != nil {
			return nil, err
		}
		for _, found := range refs {
			if !found {
				return nil, fmt.Errorf("rendition target has no matching page ownership")
			}
		}
	}
	number := int64(0)
	for id := range r.xref {
		number = max(number, id)
	}
	for ref := range result {
		number = max(number, ref.Number)
	}
	output := renditionOutput{
		reader:     r,
		copy:       rewriteObjectCopy{ctx: ctx, source: r, objects: result, number: &number, copied: make(map[rewriteObjectIdentity]Reference)},
		renditions: make(map[*Rendition]Reference), clips: make(map[*MediaClip]Reference), files: make(map[*FileSpecification]Reference),
	}
	for actions := range options.actionSequences() {
		for _, action := range actions {
			value := action.Rendition
			if value == nil || prepared[value] != nil {
				continue
			}
			target := value.Annotation
			if value.Target != nil {
				target = added[value.Target]
			}
			dict := Dictionary{"S": Name("Rendition"), "OP": Integer(value.Operation), "AN": target}
			if value.Rendition != nil {
				ref, err := output.rendition(value.Rendition, 0)
				if err != nil {
					return nil, err
				}
				dict["R"] = ref
			}
			prepared[value] = dict
		}
	}
	view := r.rewriteReader(result)
	validation := mediaReader{Reader: view, renditions: make(map[uintptr]mediaReadEntry[Rendition]), clips: make(map[uintptr]mediaReadEntry[MediaClip])}
	for _, ref := range output.renditions {
		if _, err := validation.readRendition(ctx, ref, 0); err != nil {
			return nil, err
		}
	}
	return prepared, ctx.Err()
}

// rendition 写出媒体呈现或有序选择器，保持候选顺序和约束
// 入参: media 呈现数据, depth 嵌套深度
// 返回: Reference 呈现引用, error 类型、循环或资源错误
func (w *renditionOutput) rendition(media *Rendition, depth int) (Reference, error) {
	if err := w.copy.ctx.Err(); err != nil {
		return Reference{}, err
	}
	if media == nil || depth >= 32 {
		return Reference{}, fmt.Errorf("missing or recursively nested rendition")
	}
	if ref, ok := w.renditions[media]; ok {
		return ref, nil
	}
	dict, err := w.header("Rendition", media.Subtype, media.Name, map[Name]Dictionary{"MH": media.MustHonor, "BE": media.BestEffort})
	if err != nil {
		return Reference{}, err
	}
	switch media.Subtype {
	case "MR":
		if len(media.Alternatives) != 0 {
			return Reference{}, fmt.Errorf("media rendition has selector alternatives")
		}
		if media.Clip != nil {
			dict["C"], err = w.clip(media.Clip, 0)
			if err != nil {
				return Reference{}, err
			}
		}
		for key, value := range map[Name]Dictionary{"P": media.Play, "SP": media.Screen} {
			if value != nil {
				dict[key], err = w.copy.copy(value, 0)
				if err != nil {
					return Reference{}, err
				}
			}
		}
	case "SR":
		if media.Clip != nil || media.Play != nil || media.Screen != nil {
			return Reference{}, fmt.Errorf("selector rendition has media parameters")
		}
		array := make(Array, 0, len(media.Alternatives))
		for _, child := range media.Alternatives {
			ref, err := w.rendition(child, depth+1)
			if err != nil {
				return Reference{}, err
			}
			array = append(array, ref)
		}
		dict["R"] = array
	default:
		return Reference{}, &UnsupportedError{Feature: "rendition subtype " + string(media.Subtype)}
	}
	ref, err := appendRewriteObject(w.copy.objects, w.copy.number, dict)
	if err == nil {
		w.renditions[media] = ref
	}
	return ref, err
}

// clip 写出文件、表单或嵌套片段，不展开共享媒体数据
// 入参: clip 片段数据, depth 嵌套深度
// 返回: Reference 片段引用, error 类型、循环或资源错误
func (w *renditionOutput) clip(clip *MediaClip, depth int) (Reference, error) {
	if err := w.copy.ctx.Err(); err != nil {
		return Reference{}, err
	}
	if clip == nil || depth >= 32 {
		return Reference{}, fmt.Errorf("missing or recursively nested media clip")
	}
	if ref, ok := w.clips[clip]; ok {
		return ref, nil
	}
	dict, err := w.header("MediaClip", clip.Subtype, clip.Name, map[Name]Dictionary{"MH": clip.MustHonor, "BE": clip.BestEffort})
	if err != nil {
		return Reference{}, err
	}
	switch clip.Subtype {
	case "MCS":
		if clip.File != nil || clip.Form != nil || clip.ContentType != "" || clip.Permissions != nil || clip.Players != nil {
			return Reference{}, fmt.Errorf("media clip section has data parameters")
		}
		dict["D"], err = w.clip(clip.Section, depth+1)
		if err != nil {
			return Reference{}, err
		}
	case "MCD":
		if clip.Section != nil || (clip.File == nil) == (clip.Form == nil) {
			return Reference{}, fmt.Errorf("invalid media clip data source")
		}
		if clip.File != nil {
			ref, exists := w.files[clip.File]
			if !exists {
				ref, err = rewriteFile(w.copy.ctx, *clip.File, w.copy.objects, w.copy.number)
				if err != nil {
					return Reference{}, err
				}
				w.files[clip.File] = ref
			}
			dict["D"] = ref
			if clip.ContentType != "" {
				dict["CT"] = String(clip.ContentType)
			}
		} else {
			if clip.ContentType != "" {
				return Reference{}, fmt.Errorf("form media clip has a content type")
			}
			if err := w.reader.validateMediaForm(clip.Form); err != nil {
				return Reference{}, err
			}
			dict["D"], err = w.copy.copy(clip.Form, 0)
			if err != nil {
				return Reference{}, err
			}
		}
		for key, value := range map[Name]Dictionary{"P": clip.Permissions, "PL": clip.Players} {
			if value != nil {
				dict[key], err = w.copy.copy(value, 0)
				if err != nil {
					return Reference{}, err
				}
			}
		}
	default:
		return Reference{}, &UnsupportedError{Feature: "media clip subtype " + string(clip.Subtype)}
	}
	ref, err := appendRewriteObject(w.copy.objects, w.copy.number, dict)
	if err == nil {
		w.clips[clip] = ref
	}
	return ref, err
}

// header 生成媒体类型、名称和独立参数字典，不复制来源描述中的未知结构字段
// 入参: kind 对象类型, subtype 子类型, name UTF-8名称, parameters 约束字典
// 返回: Dictionary 输出字典, error 编码、引用或取消错误
func (w *renditionOutput) header(kind, subtype Name, name string, parameters map[Name]Dictionary) (Dictionary, error) {
	dict := Dictionary{"Type": kind, "S": subtype}
	if name != "" {
		text, err := EncodeTextString(name)
		if err != nil {
			return nil, err
		}
		dict["N"] = text
	}
	for key, value := range parameters {
		if value != nil {
			copy, err := w.copy.copy(value, 0)
			if err != nil {
				return nil, err
			}
			dict[key] = copy
		}
	}
	return dict, w.copy.ctx.Err()
}
