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

// rewriteExternalGoTo 生成跨文件跳转动作，不读取或执行目标文件
// 入参: ctx 取消上下文, action 跨文件跳转
// 返回: Dictionary 动作字典, error 文件、目标或路径错误
func (r *Reader) rewriteExternalGoTo(ctx context.Context, action ExternalGoToAction) (Dictionary, error) {
	if action.Type != "GoToR" && action.Type != "GoToE" {
		return nil, fmt.Errorf("invalid external go-to action type")
	}
	if action.Type == "GoToR" && (action.File == nil || len(action.Target) != 0) {
		return nil, fmt.Errorf("remote go-to requires a file without an embedded target")
	}
	if action.Type == "GoToE" && action.File == nil && len(action.Target) == 0 {
		return nil, fmt.Errorf("missing embedded go-to target")
	}
	destination, err := r.rewriteRemoteDestination(action.Destination)
	if err != nil {
		return nil, err
	}
	dict := Dictionary{"S": action.Type, "D": destination}
	if action.File != nil {
		file, err := externalFileSpecification(*action.File)
		if err != nil {
			return nil, err
		}
		dict["F"] = file
	}
	if action.NewWindow != nil {
		dict["NewWindow"] = Boolean(*action.NewWindow)
	}
	parent := dict
	for _, target := range action.Target {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		step := Dictionary{"R": target.Relation}
		if target.Name != nil {
			step["N"] = *target.Name
		}
		if target.Page != nil {
			step["P"] = target.Page
		}
		if target.Annotation != nil {
			step["A"] = target.Annotation
		}
		for _, value := range []Object{target.Page, target.Annotation} {
			switch value.(type) {
			case nil, Integer, String:
			default:
				return nil, fmt.Errorf("invalid embedded target selector")
			}
		}
		parsed, err := r.readEmbeddedTarget(step)
		if err != nil {
			return nil, err
		}
		if parsed.Name != nil {
			step["N"] = *parsed.Name
		}
		if parsed.Page != nil {
			step["P"] = parsed.Page
			step["A"] = parsed.Annotation
		}
		parent["T"] = step
		parent = step
	}
	return dict, ctx.Err()
}

// rewriteRemoteDestination 生成远程目标，保留命名目标字节，不使用源文档页引用
// 入参: destination 远程目标
// 返回: Object 名称或显式目标, error 页码或显示参数错误
func (r *Reader) rewriteRemoteDestination(destination RemoteDestination) (Object, error) {
	if destination.Named != nil {
		switch name := destination.Named.(type) {
		case Name:
			return name, nil
		case String:
			return String(bytes.Clone(name)), nil
		default:
			return nil, fmt.Errorf("invalid named remote destination")
		}
	}
	if destination.Page < 0 {
		return nil, fmt.Errorf("invalid remote destination page")
	}
	return r.rewriteDestinationArray(Integer(destination.Page), destination.Mode, destination.Parameters)
}

// externalFileSpecification 生成外部文件说明，路径使用UTF-8字节及Unicode文件名
// 入参: file 文件名称、文件系统及说明
// 返回: Dictionary 文件说明, error 名称编码或内嵌文件错误
func externalFileSpecification(file FileSpecification) (Dictionary, error) {
	if file.Name == "" || file.Embedded != nil {
		return nil, fmt.Errorf("invalid external file specification")
	}
	name, err := EncodeTextString(file.Name)
	if err != nil {
		return nil, err
	}
	dict := Dictionary{"Type": Name("Filespec"), "F": String(file.Name), "UF": name}
	if file.FileSystem != "" {
		dict["FS"] = file.FileSystem
	}
	if file.FileSystem == "URL" {
		uri, err := navigationURI(file.Name)
		if err != nil {
			return nil, err
		}
		dict["F"], dict["UF"] = uri, String(bytes.Clone(uri))
	}
	if file.Description != "" {
		description, err := EncodeTextString(file.Description)
		if err != nil {
			return nil, err
		}
		dict["Desc"] = description
	}
	return dict, nil
}

// navigationVersion 按对象去重计算动作链及文件说明所需的PDF版本
// 入参: ctx 取消上下文, object 首个动作
// 返回: string 最低版本, error 动作结构或取消错误
func (r *Reader) navigationVersion(ctx context.Context, object Object) (string, error) {
	version := "1.1"
	stack := []Object{object}
	seen := make(map[uintptr]bool)
	for len(stack) != 0 {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		object = stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		value, err := r.Resolve(object)
		if err != nil {
			return "", err
		}
		action, ok := value.(Dictionary)
		if !ok || action == nil {
			return "", fmt.Errorf("invalid action dictionary")
		}
		id := reflect.ValueOf(action).Pointer()
		if seen[id] {
			continue
		}
		seen[id] = true
		next, err := r.Resolve(action["Next"])
		if err != nil {
			return "", err
		}
		if next != nil {
			if list, ok := next.(Array); ok {
				stack = append(stack, list...)
			} else {
				stack = append(stack, next)
			}
		}
		if next != nil || action["NewWindow"] != nil {
			version = max(version, "1.2")
		}
		kind, err := r.Resolve(action["S"])
		if err != nil {
			return "", err
		}
		if kind == Name("GoToE") {
			version = max(version, "1.6")
		}
		if kind == Name("Rendition") {
			version = max(version, "1.5")
		}
		if kind == Name("Sound") || kind == Name("Movie") {
			version = max(version, "1.2")
		}
		if kind != Name("GoToR") && kind != Name("GoToE") {
			continue
		}
		value, err = r.Resolve(action["F"])
		if err != nil {
			return "", err
		}
		if file, ok := value.(Dictionary); ok {
			if file["UF"] != nil {
				version = max(version, "1.7")
			}
			if file["Desc"] != nil {
				version = max(version, "1.6")
			}
		}
	}
	return version, ctx.Err()
}
