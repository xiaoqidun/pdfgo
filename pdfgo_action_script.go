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

// JavaScriptAction 保存脚本文字，不解释或执行JavaScript
type JavaScriptAction struct {
	Script string
}

// ReadJavaScriptAction 按PDF标准表217读取文本串或文本流中的脚本
// 入参: ctx 取消上下文, object 动作字典或引用
// 返回: JavaScriptAction 脚本文字, error 字典、编码、流解码或取消错误
func (r *Reader) ReadJavaScriptAction(ctx context.Context, object Object) (JavaScriptAction, error) {
	var result JavaScriptAction
	dict, err := r.actionDictionary(ctx, object, "JavaScript")
	if err != nil {
		return result, err
	}
	value, err := r.Resolve(dict["JS"])
	if err != nil {
		return result, err
	}
	var encoded String
	switch value := value.(type) {
	case String:
		encoded = value
	case *Stream:
		if value == nil {
			return result, fmt.Errorf("invalid JavaScript text stream")
		}
		data, err := value.DecodeContext(ctx)
		if err != nil {
			return result, err
		}
		encoded = String(data)
	default:
		return result, fmt.Errorf("invalid JavaScript action source")
	}
	result.Script, err = DecodeTextString(encoded)
	if err != nil {
		return JavaScriptAction{}, err
	}
	if err := ctx.Err(); err != nil {
		return JavaScriptAction{}, err
	}
	return result, nil
}
