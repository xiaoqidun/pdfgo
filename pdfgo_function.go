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
	"fmt"
	"math"
	"slices"
	"strconv"
)

// affineValue 保存常数项及各输入分量的系数
type affineValue []float64

// constant 判断表达式是否与输入无关
// 返回: bool 是否常量
func (v affineValue) constant() bool {
	for _, c := range v[1:] {
		if c != 0 {
			return false
		}
	}
	return true
}

// affineCalculator 符号执行计算器函数，保留多输入仿射关系
// 入参: data 函数内容, inputs 输入分量数, outputs 输出分量数
// 返回: []affineValue 输出表达式, error 格式或非仿射运算错误
func affineCalculator(data []byte, inputs, outputs int) ([]affineValue, error) {
	p := objectParser{data: data}
	p.skipSpace()
	if p.pos == len(data) || data[p.pos] != '{' {
		return nil, fmt.Errorf("invalid calculator procedure")
	}
	p.pos++
	stack := make([]affineValue, inputs)
	for i := range stack {
		stack[i] = make(affineValue, inputs+1)
		stack[i][i+1] = 1
	}
	for {
		p.skipSpace()
		if p.pos == len(data) {
			return nil, fmt.Errorf("unterminated calculator procedure")
		}
		if data[p.pos] == '}' {
			p.pos++
			break
		}
		token := p.token()
		if value, err := strconv.ParseFloat(token, 64); err == nil && !math.IsNaN(value) && !math.IsInf(value, 0) {
			v := make(affineValue, inputs+1)
			v[0] = value
			stack = append(stack, v)
			continue
		}
		n, required := len(stack), 2
		switch token {
		case "dup", "pop", "neg", "cvr", "index", "copy":
			required = 1
		}
		if n < required {
			return nil, fmt.Errorf("calculator stack underflow")
		}
		switch token {
		case "dup":
			stack = append(stack, stack[n-1])
		case "pop":
			stack = stack[:n-1]
		case "exch":
			stack[n-1], stack[n-2] = stack[n-2], stack[n-1]
		case "cvr":
		case "neg":
			v := slices.Clone(stack[n-1])
			for i := range v {
				v[i] = -v[i]
			}
			stack[n-1] = v
		case "index", "copy":
			v := stack[n-1]
			if !v.constant() || math.Trunc(v[0]) != v[0] || v[0] < 0 || v[0] > float64(n-1) || token == "index" && v[0] == float64(n-1) {
				return nil, fmt.Errorf("invalid calculator %s operand", token)
			}
			stack = stack[:n-1]
			if token == "index" {
				stack = append(stack, stack[len(stack)-1-int(v[0])])
			} else {
				stack = append(stack, stack[len(stack)-int(v[0]):]...)
			}
		case "roll":
			count, shift := stack[n-2], stack[n-1]
			if !count.constant() || !shift.constant() || math.Trunc(count[0]) != count[0] || math.Trunc(shift[0]) != shift[0] || count[0] < 0 || count[0] > float64(n-2) {
				return nil, fmt.Errorf("invalid calculator roll operands")
			}
			stack = stack[:n-2]
			if count[0] != 0 {
				width := int(count[0])
				offset := int(math.Mod(shift[0], count[0]))
				values := slices.Clone(stack[len(stack)-width:])
				for i, v := range values {
					stack[len(stack)-width+(i+offset+width)%width] = v
				}
			}
		case "add", "sub", "mul", "div":
			a, b := stack[n-2], stack[n-1]
			if token == "mul" && !a.constant() && !b.constant() || token == "div" && !b.constant() {
				return nil, &UnsupportedError{Feature: "nonlinear calculator function"}
			}
			if token == "div" && b[0] == 0 {
				return nil, fmt.Errorf("calculator division by zero")
			}
			v := make(affineValue, inputs+1)
			for i := range v {
				switch token {
				case "add":
					v[i] = a[i] + b[i]
				case "sub":
					v[i] = a[i] - b[i]
				case "mul":
					if b.constant() {
						v[i] = a[i] * b[0]
					} else {
						v[i] = b[i] * a[0]
					}
				case "div":
					v[i] = a[i] / b[0]
				}
				if math.IsNaN(v[i]) || math.IsInf(v[i], 0) {
					return nil, fmt.Errorf("nonfinite calculator result")
				}
			}
			stack[n-2] = v
			stack = stack[:n-1]
		default:
			return nil, &UnsupportedError{Feature: "calculator operator " + token}
		}
	}
	p.skipSpace()
	if p.pos != len(data) || len(stack) != outputs {
		return nil, fmt.Errorf("invalid calculator result")
	}
	return stack, nil
}
