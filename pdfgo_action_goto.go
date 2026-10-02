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
	"bytes"
	"context"
	"fmt"
	"reflect"
)

// RemoteDestination 保存目标文档中的零基页码或原始命名目标，不引用源文档页面
// Named仅为Name或String，非空时不使用Page、Mode和Parameters
type RemoteDestination struct {
	Named      Object
	Page       int64
	Mode       Name
	Parameters Array
}

// EmbeddedTarget 保存嵌入目标路径的一步，Relation为P或C
// Name为名称树原始键；Page为零基页码或命名目标字节串；Annotation为零基索引或文本名称
type EmbeddedTarget struct {
	Relation   Name
	Name       *String
	Page       Object
	Annotation Object
}

// ExternalGoToAction 保存GoToR或GoToE目标，不执行跳转或读取目标文件
// GoToE的File相对于源文档根文件，Target按顺序遍历；NewWindow为空时沿用阅读器偏好
type ExternalGoToAction struct {
	Type        Name
	File        *FileSpecification
	Destination RemoteDestination
	Target      []EmbeddedTarget
	NewWindow   *bool
}

// ReadExternalGoToAction 解析单个跨文件或嵌入文件跳转，检测目标路径循环
// 入参: ctx 取消上下文, object 动作字典或间接引用
// 返回: ExternalGoToAction 目标信息, error 类型、必填项目或路径错误
func (r *Reader) ReadExternalGoToAction(ctx context.Context, object Object) (ExternalGoToAction, error) {
	if err := ctx.Err(); err != nil {
		return ExternalGoToAction{}, err
	}
	value, err := r.Resolve(object)
	if err != nil {
		return ExternalGoToAction{}, err
	}
	dict, ok := value.(Dictionary)
	if !ok {
		return ExternalGoToAction{}, fmt.Errorf("invalid external go-to action")
	}
	value, err = r.Resolve(dict["S"])
	if err != nil {
		return ExternalGoToAction{}, err
	}
	kind, ok := value.(Name)
	if !ok || kind != "GoToR" && kind != "GoToE" {
		return ExternalGoToAction{}, fmt.Errorf("invalid external go-to action type")
	}
	result := ExternalGoToAction{Type: kind}
	value, err = r.Resolve(dict["F"])
	if err != nil {
		return result, err
	}
	if value != nil {
		file, err := r.ReadFileSpecification(value)
		if err != nil {
			return result, err
		}
		if file.Embedded != nil {
			return result, fmt.Errorf("external go-to root file is embedded")
		}
		result.File = &file
	} else if kind == "GoToR" {
		return result, fmt.Errorf("missing remote go-to file")
	}
	result.Destination, err = r.ReadRemoteDestination(dict["D"])
	if err != nil {
		return result, err
	}
	value, err = r.Resolve(dict["NewWindow"])
	if err != nil {
		return result, err
	}
	if value != nil {
		window, ok := value.(Boolean)
		if !ok {
			return result, fmt.Errorf("invalid external go-to window flag")
		}
		flag := bool(window)
		result.NewWindow = &flag
	}
	if kind == "GoToR" {
		return result, ctx.Err()
	}
	value, err = r.Resolve(dict["T"])
	if err != nil {
		return result, err
	}
	if value == nil && result.File == nil {
		return result, fmt.Errorf("missing embedded go-to target")
	}
	seen := map[uintptr]bool{}
	for value != nil {
		if err := ctx.Err(); err != nil {
			return result, err
		}
		dict, ok := value.(Dictionary)
		if !ok || dict == nil {
			return result, fmt.Errorf("invalid embedded target dictionary")
		}
		id := reflect.ValueOf(dict).Pointer()
		if seen[id] {
			return result, fmt.Errorf("cyclic embedded target path")
		}
		seen[id] = true
		target, err := r.readEmbeddedTarget(dict)
		if err != nil {
			return result, err
		}
		result.Target = append(result.Target, target)
		value, err = r.Resolve(dict["T"])
		if err != nil {
			return result, err
		}
	}
	return result, ctx.Err()
}

// ReadRemoteDestination 读取远程目标，不查找源文档名称树，不解释页对象引用
// 入参: object 名称、字节串或显式目标数组
// 返回: RemoteDestination 目标信息, error 页码、模式或参数错误
func (r *Reader) ReadRemoteDestination(object Object) (RemoteDestination, error) {
	value, err := r.Resolve(object)
	if err != nil {
		return RemoteDestination{}, err
	}
	switch encoded := value.(type) {
	case String:
		return RemoteDestination{Named: String(bytes.Clone(encoded))}, nil
	case Name:
		return RemoteDestination{Named: value}, nil
	}
	array, ok := value.(Array)
	if !ok || len(array) < 2 {
		return RemoteDestination{}, fmt.Errorf("invalid remote destination")
	}
	value, err = r.Resolve(array[0])
	if err != nil {
		return RemoteDestination{}, err
	}
	page, ok := value.(Integer)
	if !ok || page < 0 {
		return RemoteDestination{}, fmt.Errorf("invalid remote destination page")
	}
	mode, parameters, err := r.destinationParameters(array)
	if err != nil {
		return RemoteDestination{}, err
	}
	return RemoteDestination{Page: int64(page), Mode: mode, Parameters: parameters}, nil
}

// ResolveRemoteDestination 在目标阅读器中解析远程目标并检查页面归属
// 入参: ctx 取消上下文, remote 已解析的远程目标
// 返回: Destination 当前文档目标, error 命名目标、越界或页面树错误
func (r *Reader) ResolveRemoteDestination(ctx context.Context, remote RemoteDestination) (Destination, error) {
	if err := ctx.Err(); err != nil {
		return Destination{}, err
	}
	var destination Destination
	if remote.Named != nil {
		switch remote.Named.(type) {
		case Name, String:
		default:
			return destination, fmt.Errorf("invalid named remote destination")
		}
		var err error
		destination, err = r.readDestination(ctx, remote.Named)
		if err != nil {
			return destination, err
		}
	} else {
		array := append(Array{Integer(remote.Page), remote.Mode}, remote.Parameters...)
		parsed, err := r.ReadRemoteDestination(array)
		if err != nil {
			return destination, err
		}
		destination.Mode, destination.Parameters = parsed.Mode, parsed.Parameters
	}
	found := false
	err := r.WalkPages(ctx, func(index int, page *Page) error {
		if remote.Named != nil {
			found = found || page.Reference == destination.Page
		} else if int64(index) == remote.Page {
			destination.Page, found = page.Reference, true
		}
		return nil
	})
	if err != nil {
		return Destination{}, err
	}
	if !found {
		return Destination{}, fmt.Errorf("%w: remote page", ErrDestinationNotFound)
	}
	return destination, nil
}

// ReadEmbeddedTargetFile 定位当前文档中的子文件，不解码内容，不推测父文档
// 入参: ctx 取消上下文, target 子文件路径的一步
// 返回: FileSpecification 内嵌文件说明, error 路径、目标缺失或结构错误
func (r *Reader) ReadEmbeddedTargetFile(ctx context.Context, target EmbeddedTarget) (FileSpecification, error) {
	if err := ctx.Err(); err != nil {
		return FileSpecification{}, err
	}
	dict := Dictionary{"R": target.Relation, "P": target.Page, "A": target.Annotation}
	if target.Name != nil {
		dict["N"] = *target.Name
	}
	target, err := r.readEmbeddedTarget(dict)
	if err != nil {
		return FileSpecification{}, err
	}
	if target.Relation != "C" {
		return FileSpecification{}, &UnsupportedError{Feature: "parent embedded document lookup without container context"}
	}
	var object Object
	found := false
	if target.Name != nil {
		catalog, err := r.catalogDictionary()
		if err != nil {
			return FileSpecification{}, err
		}
		value, err := r.Resolve(catalog["Names"])
		if err != nil {
			return FileSpecification{}, err
		}
		if value != nil {
			names, ok := value.(Dictionary)
			if !ok {
				return FileSpecification{}, fmt.Errorf("invalid names dictionary")
			}
			err = r.WalkNameTree(ctx, names["EmbeddedFiles"], func(name string, value Object) error {
				if name == string(*target.Name) {
					object = value
					found = true
				}
				return nil
			})
			if err != nil {
				return FileSpecification{}, err
			}
		}
	} else {
		object, err = r.embeddedTargetAnnotation(ctx, target)
		if err != nil {
			return FileSpecification{}, err
		}
		found = true
	}
	if !found {
		return FileSpecification{}, fmt.Errorf("%w: embedded file", ErrDestinationNotFound)
	}
	file, err := r.ReadFileSpecification(object)
	if err != nil {
		return file, err
	}
	if file.Embedded == nil {
		return file, fmt.Errorf("embedded target has no embedded file stream")
	}
	return file, ctx.Err()
}

// readEmbeddedTarget 检查单步路径字段，区分名称树和注解两种子文件选择方式
// 入参: dict 目标字典
// 返回: EmbeddedTarget 路径一步, error 字段类型或组合错误
func (r *Reader) readEmbeddedTarget(dict Dictionary) (EmbeddedTarget, error) {
	values := make([]Object, 4)
	for index, key := range []Name{"R", "N", "P", "A"} {
		var err error
		values[index], err = r.Resolve(dict[key])
		if err != nil {
			return EmbeddedTarget{}, err
		}
	}
	relation, ok := values[0].(Name)
	if !ok || relation != "P" && relation != "C" {
		return EmbeddedTarget{}, fmt.Errorf("invalid embedded target relationship")
	}
	result := EmbeddedTarget{Relation: relation}
	if relation == "P" {
		if values[1] != nil || values[2] != nil || values[3] != nil {
			return result, fmt.Errorf("parent embedded target contains child selectors")
		}
		return result, nil
	}
	if values[1] != nil {
		name, ok := values[1].(String)
		if !ok || values[2] != nil || values[3] != nil {
			return result, fmt.Errorf("invalid embedded file name selector")
		}
		name = bytes.Clone(name)
		result.Name = &name
		return result, nil
	}
	for index, label := range []string{"page", "annotation"} {
		switch value := values[index+2].(type) {
		case Integer:
			if value < 0 {
				return result, fmt.Errorf("negative embedded target %s", label)
			}
		case String:
			if index == 1 {
				if _, err := DecodeTextString(value); err != nil {
					return result, err
				}
			}
			values[index+2] = String(bytes.Clone(value))
		default:
			return result, fmt.Errorf("invalid embedded target %s selector", label)
		}
	}
	result.Page, result.Annotation = values[2], values[3]
	return result, nil
}

// embeddedTargetAnnotation 按原始Annots索引或NM文本名称查找附件，页名称保留原始字节
// 入参: ctx 取消上下文, target 注解选择路径
// 返回: Object 附件文件说明, error 目标缺失或结构错误
func (r *Reader) embeddedTargetAnnotation(ctx context.Context, target EmbeddedTarget) (Object, error) {
	var reference Reference
	if name, ok := target.Page.(String); ok {
		destination, err := r.readDestination(ctx, name)
		if err != nil {
			return nil, err
		}
		reference = destination.Page
	}
	var selected *Page
	err := r.WalkPages(ctx, func(index int, page *Page) error {
		if number, ok := target.Page.(Integer); ok && int64(index) == int64(number) || reference != (Reference{}) && reference == page.Reference {
			selected = page
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if selected == nil {
		return nil, fmt.Errorf("%w: embedded target page", ErrDestinationNotFound)
	}
	value, err := r.Resolve(selected.Dictionary["Annots"])
	if err != nil {
		return nil, err
	}
	annotations, ok := value.(Array)
	if !ok {
		return nil, fmt.Errorf("invalid embedded target annotations")
	}
	var chosen Object
	if index, ok := target.Annotation.(Integer); ok {
		if int64(index) >= int64(len(annotations)) {
			return nil, fmt.Errorf("%w: embedded target annotation", ErrDestinationNotFound)
		}
		chosen = annotations[index]
	} else {
		name, err := DecodeTextString(target.Annotation.(String))
		if err != nil {
			return nil, err
		}
		for _, object := range annotations {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			value, err := r.Resolve(object)
			if err != nil {
				return nil, err
			}
			dict, ok := value.(Dictionary)
			if !ok {
				return nil, fmt.Errorf("invalid embedded target annotation")
			}
			value, err = r.Resolve(dict["NM"])
			if err != nil {
				return nil, err
			}
			if value == nil {
				continue
			}
			text, ok := value.(String)
			if !ok {
				return nil, fmt.Errorf("invalid annotation name")
			}
			decoded, err := DecodeTextString(text)
			if err != nil {
				return nil, err
			}
			if decoded == name {
				if chosen != nil {
					return nil, fmt.Errorf("duplicate embedded target annotation name")
				}
				chosen = object
			}
		}
	}
	if chosen == nil {
		return nil, fmt.Errorf("%w: embedded target annotation", ErrDestinationNotFound)
	}
	value, err = r.Resolve(chosen)
	if err != nil {
		return nil, err
	}
	dict, ok := value.(Dictionary)
	if !ok {
		return nil, fmt.Errorf("invalid embedded target annotation")
	}
	value, err = r.Resolve(dict["Subtype"])
	if err != nil {
		return nil, err
	}
	if value != Name("FileAttachment") {
		return nil, fmt.Errorf("embedded target annotation is not a file attachment")
	}
	value, err = r.Resolve(dict["FS"])
	if err != nil {
		return nil, err
	}
	if value == nil {
		return nil, fmt.Errorf("missing embedded target file specification")
	}
	return value, nil
}
