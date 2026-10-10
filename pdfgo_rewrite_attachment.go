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
	"math"
	"mime"
	"slices"
	"strings"
)

// EmbeddedFile 保存待写入附件，名称及说明使用UTF-8，MediaType为空时不写媒体类型
// Data为原始文件字节，调用期间须保持不变；压缩不改变内容，不自动添加时间等元数据
type EmbeddedFile struct {
	Name        string
	Description string
	MediaType   string
	Data        []byte
}

// AttachmentAction 按名称树原始键打开输出文档中的附件，不执行文件
// NewWindow为空时沿用阅读器偏好，PDF标准仅对PDF附件定义窗口行为
type AttachmentAction struct {
	Key       string
	NewWindow *bool
}

// attachmentReplacements 更新附件名称树，保留其他名称树及未修改的文件引用
// 入参: ctx 取消上下文, options 替换配置, result 已有替换对象
// 返回: map[string]Object 动作使用的附件引用, error 名称树、附件或取消错误
func (r *Reader) attachmentReplacements(ctx context.Context, options RewriteOptions, result map[Reference]Object) (map[string]Object, error) {
	targets := make(map[string]bool)
	for actions := range options.actionSequences() {
		for _, action := range actions {
			if action.Attachment != nil {
				targets[action.Attachment.Key] = true
			}
		}
	}
	if len(options.Attachments) == 0 && len(targets) == 0 {
		return nil, nil
	}
	catalog, err := r.catalogDictionary()
	if err != nil {
		return nil, err
	}
	value, err := r.Resolve(catalog["Names"])
	if err != nil {
		return nil, err
	}
	names := Dictionary{}
	if value != nil {
		original, ok := value.(Dictionary)
		if !ok {
			return nil, fmt.Errorf("invalid PDF names dictionary")
		}
		names = maps.Clone(original)
	}
	files := make(map[string]Object)
	if err := r.WalkNameTree(ctx, names["EmbeddedFiles"], func(key string, value Object) error {
		files[key] = value
		return nil
	}); err != nil {
		return nil, err
	}
	for key := range targets {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if _, changed := options.Attachments[key]; changed {
			continue
		}
		object, exists := files[key]
		if !exists {
			return nil, fmt.Errorf("PDF attachment target not found")
		}
		file, err := r.ReadFileSpecification(object)
		if err != nil {
			return nil, err
		}
		if file.Embedded == nil {
			return nil, fmt.Errorf("PDF attachment target has no embedded data")
		}
	}
	if len(options.Attachments) == 0 {
		return files, nil
	}
	root, ok := r.Trailer["Root"].(Reference)
	if !ok || result[root] != nil {
		return nil, fmt.Errorf("invalid or conflicting PDF catalog replacement")
	}
	number := int64(0)
	for id := range r.xref {
		number = max(number, id)
	}
	for ref := range result {
		number = max(number, ref.Number)
	}
	changed := false
	for _, key := range slices.Sorted(maps.Keys(options.Attachments)) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		file := options.Attachments[key]
		if file == nil {
			_, exists := files[key]
			changed = changed || exists
			delete(files, key)
			continue
		}
		spec, err := externalFileSpecification(FileSpecification{Name: file.Name, Description: file.Description})
		if err != nil {
			return nil, err
		}
		dict := Dictionary{"Type": Name("EmbeddedFile"), "Params": Dictionary{"Size": Integer(len(file.Data))}}
		if file.MediaType != "" {
			mediaType, parameters, err := mime.ParseMediaType(file.MediaType)
			if err != nil || len(parameters) != 0 || !strings.Contains(mediaType, "/") {
				return nil, fmt.Errorf("invalid embedded file media type")
			}
			dict["Subtype"] = Name(mediaType)
		}
		stream, err := appendRewriteObject(result, &number, &Stream{Dictionary: dict, Data: file.Data})
		if err != nil {
			return nil, err
		}
		spec["EF"] = Dictionary{"F": stream, "UF": stream}
		ref, err := appendRewriteObject(result, &number, spec)
		if err != nil {
			return nil, err
		}
		files[key] = ref
		changed = true
	}
	if !changed {
		return files, nil
	}
	if len(files) == 0 {
		delete(names, "EmbeddedFiles")
	} else {
		tree, err := rewriteNameTree(ctx, slices.Sorted(maps.Keys(files)), files, result, &number)
		if err != nil {
			return nil, err
		}
		original, err := r.Resolve(names["EmbeddedFiles"])
		if err != nil {
			return nil, err
		}
		if dict, ok := original.(Dictionary); ok {
			updated := maps.Clone(dict)
			delete(updated, "Names")
			delete(updated, "Kids")
			delete(updated, "Limits")
			maps.Copy(updated, tree)
			tree = updated
		}
		names["EmbeddedFiles"] = tree
	}
	updated := maps.Clone(catalog)
	if len(names) == 0 {
		delete(updated, "Names")
	} else {
		updated["Names"] = names
	}
	result[root] = updated
	return files, nil
}

// appendRewriteObject 分配未被源文件及其他替换占用的对象编号
// 入参: objects 输出对象, number 已分配最大编号, value 新对象
// 返回: Reference 对象引用, error 编号越界错误
func appendRewriteObject(objects map[Reference]Object, number *int64, value Object) (Reference, error) {
	if *number >= math.MaxInt32-1024 {
		return Reference{}, fmt.Errorf("PDF object number exceeds output limit")
	}
	*number++
	ref := Reference{Number: *number}
	objects[ref] = value
	return ref, nil
}

// rewriteNameTree 按原始字节顺序构建名称树，每个节点最多保存64项
// 入参: ctx 取消上下文, keys 有序键, values 名称映射, objects 输出对象, number 最大对象编号
// 返回: Dictionary 名称树根节点, error 分配或取消错误
func rewriteNameTree(ctx context.Context, keys []string, values map[string]Object, objects map[Reference]Object, number *int64) (Dictionary, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if len(keys) <= 64 {
		pairs := make(Array, 0, len(keys)*2)
		for _, key := range keys {
			pairs = append(pairs, String(key), values[key])
		}
		return Dictionary{"Names": pairs}, nil
	}
	nodes := make([]Dictionary, 0, (len(keys)-1)/64+1)
	for start := 0; start < len(keys); start += 64 {
		end := min(start+64, len(keys))
		child, err := rewriteNameTree(ctx, keys[start:end], values, objects, number)
		if err != nil {
			return nil, err
		}
		child["Limits"] = Array{String(keys[start]), String(keys[end-1])}
		nodes = append(nodes, child)
	}
	for len(nodes) > 1 {
		parents := make([]Dictionary, 0, (len(nodes)-1)/64+1)
		for start := 0; start < len(nodes); start += 64 {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			end := min(start+64, len(nodes))
			kids := make(Array, 0, end-start)
			for _, child := range nodes[start:end] {
				ref, err := appendRewriteObject(objects, number, child)
				if err != nil {
					return nil, err
				}
				kids = append(kids, ref)
			}
			limits := Array{nodes[start]["Limits"].(Array)[0], nodes[end-1]["Limits"].(Array)[1]}
			parents = append(parents, Dictionary{"Kids": kids, "Limits": limits})
		}
		nodes = parents
	}
	delete(nodes[0], "Limits")
	return nodes[0], nil
}
