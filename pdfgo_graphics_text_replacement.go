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

import "fmt"

// replacementText 读取Span的替换文本，区分空字符串与未声明属性
// 入参: properties 区段属性
// 返回: *ReplacementText 替换区段，未声明时为空, error 类型、编码或取消错误
func (p *pageInterpreter) replacementText(properties Dictionary) (*ReplacementText, error) {
	if err := p.ctx.Err(); err != nil {
		return nil, err
	}
	object, err := p.reader.Resolve(properties["ActualText"])
	if err != nil || object == nil {
		return nil, err
	}
	value, ok := object.(String)
	if !ok {
		return nil, fmt.Errorf("ActualText is not a text string")
	}
	text, err := DecodeTextString(value)
	if err != nil {
		return nil, err
	}
	if err := p.ctx.Err(); err != nil {
		return nil, err
	}
	return &ReplacementText{Text: text, Parent: p.replacement}, nil
}
