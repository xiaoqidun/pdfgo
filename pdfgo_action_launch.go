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
)

// WindowsLaunchParameters 保存Windows启动参数，路径和参数保留原始字节
// Operation缺省为open，Parameters为空不代表允许执行应用程序
type WindowsLaunchParameters struct {
	File       String
	Directory  String
	Operation  string
	Parameters String
}

// LaunchAction 保存文件启动动作，不执行应用程序、打印或访问外部文件
// File为跨平台后备目标，平台参数优先；Mac和Unix保留标准未定义的只读原始值
// NewWindow为空时使用阅读器偏好，仅对PDF目标文档有效
type LaunchAction struct {
	File      *FileSpecification
	Windows   *WindowsLaunchParameters
	Mac, Unix Object
	NewWindow *bool
}

// ReadLaunchAction 按PDF标准表203和表204解析启动动作及平台参数
// 入参: ctx 取消上下文, object 动作字典或引用
// 返回: LaunchAction 启动信息, error 结构、参数或取消错误
func (r *Reader) ReadLaunchAction(ctx context.Context, object Object) (LaunchAction, error) {
	var result LaunchAction
	if ctx == nil {
		return result, fmt.Errorf("invalid launch action context")
	}
	if err := ctx.Err(); err != nil {
		return result, err
	}
	value, err := r.Resolve(object)
	if err != nil {
		return result, err
	}
	dict, ok := value.(Dictionary)
	if !ok || dict == nil {
		return result, fmt.Errorf("invalid launch action")
	}
	value, err = r.Resolve(dict["S"])
	if err != nil {
		return result, err
	}
	if value != Name("Launch") {
		return result, fmt.Errorf("invalid launch action type")
	}
	value, err = r.Resolve(dict["F"])
	if err != nil {
		return result, err
	}
	if value != nil {
		file, err := r.ReadFileSpecification(value)
		if err != nil {
			return result, err
		}
		result.File = &file
	}
	value, err = r.Resolve(dict["Win"])
	if err != nil {
		return result, err
	}
	if value != nil {
		parameters, ok := value.(Dictionary)
		if !ok || parameters == nil {
			return result, fmt.Errorf("invalid Windows launch parameters")
		}
		windows := &WindowsLaunchParameters{Operation: "open"}
		for _, field := range []struct {
			key    Name
			target *String
		}{{"F", &windows.File}, {"D", &windows.Directory}, {"P", &windows.Parameters}} {
			value, err := r.Resolve(parameters[field.key])
			if err != nil {
				return result, err
			}
			if value == nil && field.key != "F" {
				continue
			}
			encoded, ok := value.(String)
			if !ok {
				return result, fmt.Errorf("invalid Windows launch %s", field.key)
			}
			*field.target = String(bytes.Clone(encoded))
		}
		value, err := r.Resolve(parameters["O"])
		if err != nil {
			return result, err
		}
		if value != nil {
			operation, ok := value.(String)
			if !ok || string(operation) != "open" && string(operation) != "print" {
				return result, fmt.Errorf("invalid Windows launch operation")
			}
			windows.Operation = string(operation)
		}
		result.Windows = windows
	}
	result.Mac, err = r.Resolve(dict["Mac"])
	if err != nil {
		return result, err
	}
	result.Unix, err = r.Resolve(dict["Unix"])
	if err != nil {
		return result, err
	}
	if result.File == nil && result.Windows == nil && result.Mac == nil && result.Unix == nil {
		return result, fmt.Errorf("missing launch target")
	}
	value, err = r.Resolve(dict["NewWindow"])
	if err != nil {
		return result, err
	}
	if value != nil {
		window, ok := value.(Boolean)
		if !ok {
			return result, fmt.Errorf("invalid launch window flag")
		}
		flag := bool(window)
		result.NewWindow = &flag
	}
	return result, ctx.Err()
}
