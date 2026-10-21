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
	"slices"
)

// Operation 保存内容流操作符及其操作数，不解释或执行操作
// BI操作将完整内联图像作为唯一的Stream操作数交付，字典缩写已展开
type Operation struct {
	Operator string
	Operands []Object
	Offset   int64
}

// WalkOperations 顺序解析内容操作，未知操作符交由调用方处理
// 入参: ctx 取消上下文, data 内容流, visit 操作访问函数
// 返回: error 错误信息
func WalkOperations(ctx context.Context, data []byte, visit func(Operation) error) error {
	return walkOperations(ctx, data, visit, nil, nil)
}

// walkOperations 解析内容流，并在内联图像引用命名颜色空间时使用当前资源作用域
// 入参: ctx 取消上下文, data 内容流, visit 操作访问函数, resolve 颜色空间解析器, warning 可恢复诊断
// 返回: error 解析或访问错误
func walkOperations(ctx context.Context, data []byte, visit func(Operation) error, resolve func(Object) (Object, error), warning func(Diagnostic)) error {
	p := objectParser{data: data, warning: warning, content: true}
	var operands []Object
	compatibility := 0
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		p.skipSpace()
		if p.pos == len(data) {
			if len(operands) != 0 {
				if warning == nil {
					return p.fail("content ends with unused operands")
				}
				warning(Diagnostic{Offset: int64(p.pos), Message: "content ends with unused operands"})
			}
			return ctx.Err()
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
			var object Object
			var err error
			if b == '[' && warning != nil {
				object, err = p.contentArray(ctx)
			} else {
				object, err = p.object()
			}
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
		if warning != nil && compatibility == 0 && !contentOperator(op) {
			for i := 1; i < len(op); i++ {
				b := op[i]
				if b == '+' || b == '-' || b == '.' || b >= '0' && b <= '9' {
					if count := graphicsOperandCount(op[:i]); count >= 0 && len(operands) == count {
						p.pos -= len(op) - i
						op = op[:i]
						warning(Diagnostic{Offset: int64(p.pos), Message: "missing separator after content operator " + op})
					}
					break
				}
			}
		}
		if op == "" {
			return p.fail("invalid content operator")
		}
		if op == "BX" {
			compatibility++
		} else if op == "EX" && compatibility > 0 {
			compatibility--
		}
		if op == "BI" {
			if len(operands) != 0 {
				return p.fail("inline image has operands")
			}
			stream, err := p.inlineImage(resolve)
			if err != nil {
				return err
			}
			operands = append(operands, stream)
		}
		var values []Object
		if len(operands) != 0 {
			values = slices.Clone(operands)
		}
		if err := visit(Operation{Operator: op, Operands: values, Offset: int64(start)}); err != nil {
			return fmt.Errorf("content offset %d: %w", start, err)
		}
		clear(operands)
		operands = operands[:0]
	}
}

// contentNumber 在内容流中恢复粘连的数字边界及无有效位的零，不改变结构对象语法
// 入参: token 已读取的词法单元
// 返回: string 当前数字或未经恢复的原词法单元
func (p *objectParser) contentNumber(token string) string {
	if token == "" || validNumber(token) || validScientificNumber(token) {
		return token
	}
	switch token {
	case "-", "+", ".", "-.", "+.":
		p.warning(Diagnostic{Offset: p.base + int64(p.pos-len(token)), Message: "content number without digits interpreted as zero: " + token})
		return "0"
	}
	i := 0
	if token[0] == '+' || token[0] == '-' {
		i++
	}
	dot := false
	for i < len(token) {
		b := token[i]
		if b >= '0' && b <= '9' {
			i++
		} else if b == '.' && !dot {
			dot = true
			i++
		} else {
			break
		}
	}
	if i == len(token) || !validNumber(token[:i]) {
		return token
	}
	b := token[i]
	if b != '+' && b != '-' && b != '.' && !contentOperator(token[i:]) {
		return token
	}
	p.pos -= len(token) - i
	p.warning(Diagnostic{Offset: p.base + int64(p.pos), Message: "missing separator after content number " + token[:i]})
	return token[:i]
}

// contentOperator 限定可从相邻数字分离的标准内容操作符
// 入参: token 操作符候选
// 返回: bool 是否为已知操作符
func contentOperator(token string) bool {
	if graphicsOperandCount(token) >= 0 {
		return true
	}
	switch token {
	case "d", "ri", "i", "gs", "CS", "cs", "SC", "SCN", "sc", "scn", "sh", "Do", "BI", "Tf", "Tj", "TJ", "'", "\"", "MP", "DP", "BMC", "BDC", "EMC":
		return true
	}
	return false
}

// contentArray 读取内容流数组，诊断并略过其中不能作为数组元素的操作符
// 入参: ctx 取消上下文
// 返回: Array 原有有效元素, error 嵌套、语法或取消错误
func (p *objectParser) contentArray(ctx context.Context) (Array, error) {
	if p.depth >= 256 {
		return nil, p.fail("object nesting limit exceeded")
	}
	p.depth++
	defer func() { p.depth-- }()
	p.pos++
	values := Array{}
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		p.skipSpace()
		if p.pos == len(p.data) {
			return nil, p.fail("unterminated content array")
		}
		b := p.data[p.pos]
		if b == ']' {
			p.pos++
			return values, nil
		}
		if b != '/' && b != '(' && b != '<' && b != '[' && b != '+' && b != '-' && b != '.' && (b < '0' || b > '9') {
			start := p.pos
			token := p.token()
			if token != "true" && token != "false" && token != "null" {
				if token == "" {
					return nil, p.fail("invalid content array element")
				}
				p.warning(Diagnostic{Offset: int64(start), Message: "operator in content array ignored: " + token})
				continue
			}
			p.pos = start
		}
		var object Object
		var err error
		if b == '[' {
			object, err = p.contentArray(ctx)
		} else {
			object, err = p.object()
		}
		if err != nil {
			return nil, err
		}
		values = append(values, object)
	}
}
