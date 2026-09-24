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

// Operation 保存内容流操作符及其操作数，不解释或执行操作
type Operation struct {
	Operator string
	Operands []Object
	Offset   int64
}

// WalkOperations 顺序解析内容操作，未知操作符交由调用方处理
// 入参: ctx 取消上下文, data 内容流, visit 操作访问函数
// 返回: error 错误信息
func WalkOperations(ctx context.Context, data []byte, visit func(Operation) error) error {
	p := objectParser{data: data}
	var operands []Object
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		p.skipSpace()
		if p.pos == len(data) {
			if len(operands) != 0 {
				return p.fail("content ends with unused operands")
			}
			return nil
		}
		start := p.pos
		b := data[start]
		isObject := b == '/' || b == '(' || b == '<' || b == '[' || b == '+' || b == '-' || b == '.' || (b >= '0' && b <= '9')
		if !isObject {
			token := p.token()
			if token == "true" || token == "false" || token == "null" {
				isObject = true
			}
			p.pos = start
		}
		if isObject {
			object, err := p.object()
			if err != nil {
				return err
			}
			if _, ok := object.(Reference); ok {
				return p.fail("indirect reference in content stream")
			}
			operands = append(operands, object)
			continue
		}
		op := p.token()
		if op == "" {
			return p.fail("invalid content operator")
		}
		if op == "BI" {
			return &UnsupportedError{Feature: "inline image content parsing"}
		}
		if err := visit(Operation{Operator: op, Operands: operands, Offset: int64(start)}); err != nil {
			return fmt.Errorf("content offset %d: %w", start, err)
		}
		operands = nil
	}
}
