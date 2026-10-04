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

// formCacheLimit 限制单次内容访问保留的表单数据及操作估算字节数
const formCacheLimit = 8 << 20

// formCache 共享同次访问的解码数据，不保存调用图形状态
type formCache struct {
	entries map[*Stream]*formContent
	bytes   int
}

// formContent 保存表单内容及可独立于资源作用域重放的操作
type formContent struct {
	data       []byte
	operations []Operation
	parsed     bool
}

// formData 解码表单并有界保留内容，大型或超额表单仍正常访问
// 入参: stream 表单数据流
// 返回: []byte 解码内容, *formContent 可复用内容, error 解码或取消错误
func (p *pageInterpreter) formData(stream *Stream) ([]byte, *formContent, error) {
	if err := p.ctx.Err(); err != nil {
		return nil, nil, err
	}
	if p.forms == nil {
		p.forms = &formCache{}
	}
	if content := p.forms.entries[stream]; content != nil {
		return content.data, content, nil
	}
	data, err := stream.DecodeContext(p.ctx)
	if err != nil {
		return nil, nil, err
	}
	if len(data) > 1<<20 || len(p.forms.entries) >= 1024 || len(data)+256 > formCacheLimit-p.forms.bytes {
		return data, nil, nil
	}
	if p.forms.entries == nil {
		p.forms.entries = make(map[*Stream]*formContent)
	}
	content := &formContent{data: data}
	p.forms.entries[stream] = content
	p.forms.bytes += len(data) + 256
	return data, content, nil
}

// walkContent 顺序访问内容，仅收集不暴露可变字典或内联图像的表单操作
// 入参: data 解码内容
// 返回: []Operation 可保留操作, int 估算字节数, error 解析、访问或取消错误
func (p *pageInterpreter) walkContent(data []byte) ([]Operation, int, error) {
	if p.content != nil && p.content.operations != nil {
		for _, operation := range p.content.operations {
			if err := p.ctx.Err(); err != nil {
				return nil, 0, err
			}
			if err := p.operation(operation); err != nil {
				return nil, 0, fmt.Errorf("content offset %d: %w", operation.Offset, err)
			}
		}
		return nil, 0, p.ctx.Err()
	}
	var operations []Operation
	if p.content != nil && !p.content.parsed {
		operations = []Operation{}
	}
	size := 0
	err := walkOperations(p.ctx, data, func(operation Operation) error {
		if operations != nil {
			cost, ok := formOperationSize(operation)
			if !ok || cost > formCacheLimit-p.forms.bytes-size {
				operations = nil
				size = 0
			} else {
				operations = append(operations, operation)
				size += cost
			}
		}
		return p.operation(operation)
	}, p.contentColorSpace)
	return operations, size, err
}

// contentColorSpace 在当前资源作用域解析内联图像的颜色空间
// 入参: value 颜色空间对象
// 返回: Object 解析结果, error 资源或引用错误
func (p *pageInterpreter) contentColorSpace(value Object) (Object, error) {
	if name, ok := value.(Name); ok && name != "DeviceGray" && name != "DeviceRGB" && name != "DeviceCMYK" {
		var err error
		value, err = p.resource("ColorSpace", name)
		if err != nil {
			return nil, err
		}
	}
	return p.reader.Resolve(value)
}

// formOperationSize 估算操作及操作数占用，可变语义对象不参与重放
// 入参: operation 内容操作
// 返回: int 估算字节数, bool 是否可复用
func formOperationSize(operation Operation) (int, bool) {
	size := 128 + len(operation.Operator)
	for _, operand := range operation.Operands {
		cost, ok := formOperandSize(operand)
		if !ok || cost > formCacheLimit-size {
			return 0, false
		}
		size += cost
	}
	return size, true
}

// formOperandSize 估算标量及数组占用，不复用字典、引用或数据流
// 入参: operand 操作数
// 返回: int 估算字节数, bool 是否可复用
func formOperandSize(operand Object) (int, bool) {
	size := 64
	switch value := operand.(type) {
	case nil, Boolean, Integer, Real:
	case Name:
		size += len(value)
	case String:
		size += 2 * len(value)
	case Array:
		for _, item := range value {
			cost, ok := formOperandSize(item)
			if !ok || cost > formCacheLimit-size {
				return 0, false
			}
			size += cost
		}
	default:
		return 0, false
	}
	return size, size <= formCacheLimit
}
