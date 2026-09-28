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
	"strings"
)

// calculatorStackSize 采用PDF要求至少支持的百项操作数栈容量
const calculatorStackSize = 100

// 计算器操作数类型
const (
	calculatorRealKind uint8 = iota
	calculatorIntegerKind
	calculatorBooleanKind
)

// calculatorValue 保存实数、整数或布尔操作数，避免逐像素装箱分配
type calculatorValue struct {
	number float64
	kind   uint8
}

// calculatorInstruction 保存计算器操作及已编译的条件分支
type calculatorInstruction struct {
	op       string
	value    calculatorValue
	branches [][]calculatorInstruction
}

// compileCalculator 按PDF语法编译计算器函数，不将条件代码块作为可操作对象
// 入参: data 函数字节
// 返回: []calculatorInstruction 指令序列, error 语法错误
func compileCalculator(data []byte) ([]calculatorInstruction, error) {
	p := objectParser{data: data}
	p.skipSpace()
	if p.pos == len(data) || data[p.pos] != '{' {
		return nil, fmt.Errorf("invalid calculator procedure")
	}
	p.pos++
	frames := [][]calculatorInstruction{nil}
	for len(frames) != 0 {
		p.skipSpace()
		if p.pos == len(data) {
			return nil, fmt.Errorf("unterminated calculator procedure")
		}
		index := len(frames) - 1
		switch data[p.pos] {
		case '{':
			p.pos++
			frames = append(frames, nil)
			continue
		case '}':
			p.pos++
			body := frames[index]
			for _, instruction := range body {
				if instruction.op == "{}" {
					return nil, fmt.Errorf("calculator procedure requires conditional operator")
				}
			}
			frames = frames[:index]
			if index == 0 {
				p.skipSpace()
				if p.pos != len(data) {
					return nil, fmt.Errorf("data after calculator procedure")
				}
				return body, nil
			}
			frames[index-1] = append(frames[index-1], calculatorInstruction{op: "{}", branches: [][]calculatorInstruction{body}})
			continue
		}
		token := p.token()
		instruction := calculatorInstruction{op: token}
		switch token {
		case "if", "ifelse":
			count := 1
			if token == "ifelse" {
				count = 2
			}
			body := frames[index]
			if len(body) < count {
				return nil, fmt.Errorf("missing calculator conditional procedure")
			}
			for _, branch := range body[len(body)-count:] {
				if branch.op != "{}" {
					return nil, fmt.Errorf("invalid calculator conditional procedure")
				}
				instruction.branches = append(instruction.branches, branch.branches[0])
			}
			frames[index] = body[:len(body)-count]
		case "true", "false":
			instruction.op, instruction.value = "", calculatorBoolean(token == "true")
		case "abs", "add", "atan", "ceiling", "cos", "cvi", "cvr", "div", "exp", "floor", "idiv", "ln", "log", "mod", "mul", "neg", "round", "sin", "sqrt", "sub", "truncate", "and", "bitshift", "eq", "ge", "gt", "le", "lt", "ne", "not", "or", "xor", "copy", "dup", "exch", "index", "pop", "roll":
		default:
			if token == "" {
				return nil, fmt.Errorf("invalid calculator token")
			}
			value, err := strconv.ParseFloat(token, 64)
			if err != nil || strings.IndexFunc(token, func(c rune) bool { return c != '+' && c != '-' && c != '.' && (c < '0' || c > '9') }) >= 0 || math.IsNaN(value) || math.IsInf(value, 0) {
				return nil, &UnsupportedError{Feature: "calculator operator " + token}
			}
			instruction.op, instruction.value = "", calculatorValue{number: value}
			if !strings.Contains(token, ".") && value >= math.MinInt32 && value <= math.MaxInt32 {
				instruction.value = calculatorInteger(int64(value))
			}
		}
		frames[index] = append(frames[index], instruction)
	}
	return nil, fmt.Errorf("invalid calculator procedure")
}

// evaluateCalculator 执行已编译函数，保持整数、实数及布尔类型
// 入参: program 指令序列, input 输入值, output 输出缓冲区
// 返回: error 类型、算术或栈错误
func evaluateCalculator(program []calculatorInstruction, input, output []float64) error {
	if len(input) > calculatorStackSize {
		return fmt.Errorf("calculator stack overflow")
	}
	var initial [calculatorStackSize]calculatorValue
	stack := initial[:0]
	for _, value := range input {
		if math.IsNaN(value) || math.IsInf(value, 0) {
			return fmt.Errorf("invalid calculator input")
		}
		stack = append(stack, calculatorValue{number: value})
	}
	var execution [16][]calculatorInstruction
	pending := execution[:1]
	pending[0] = program
	for len(pending) != 0 {
		last := len(pending) - 1
		if len(pending[last]) == 0 {
			pending = pending[:last]
			continue
		}
		instruction := pending[last][0]
		pending[last] = pending[last][1:]
		if instruction.op == "" {
			if len(stack) == calculatorStackSize {
				return fmt.Errorf("calculator stack overflow")
			}
			stack = append(stack, instruction.value)
			continue
		}
		op := instruction.op
		n, required := len(stack), 2
		switch op {
		case "abs", "ceiling", "cos", "cvi", "cvr", "floor", "ln", "log", "neg", "round", "sin", "sqrt", "truncate", "not", "copy", "dup", "index", "pop", "if", "ifelse":
			required = 1
		}
		if n < required {
			return fmt.Errorf("calculator stack underflow: %s", op)
		}
		switch op {
		case "if", "ifelse":
			flag, ok := stack[n-1].number != 0, stack[n-1].kind == calculatorBooleanKind
			if !ok {
				return fmt.Errorf("invalid calculator condition")
			}
			stack = stack[:n-1]
			if flag {
				pending = append(pending, instruction.branches[0])
			} else if op == "ifelse" {
				pending = append(pending, instruction.branches[1])
			}
		case "dup":
			if n == calculatorStackSize {
				return fmt.Errorf("calculator stack overflow")
			}
			stack = append(stack, stack[n-1])
		case "pop":
			stack = stack[:n-1]
		case "exch":
			stack[n-1], stack[n-2] = stack[n-2], stack[n-1]
		case "copy", "index":
			count, ok := int64(stack[n-1].number), stack[n-1].kind == calculatorIntegerKind
			if !ok || count < 0 || count > int64(n-1) || op == "index" && count == int64(n-1) {
				return fmt.Errorf("invalid calculator %s operand", op)
			}
			stack = stack[:n-1]
			if op == "copy" && int(count) > calculatorStackSize-len(stack) {
				return fmt.Errorf("calculator stack overflow")
			}
			if op == "copy" {
				stack = append(stack, stack[len(stack)-int(count):]...)
			} else {
				stack = append(stack, stack[len(stack)-1-int(count)])
			}
		case "roll":
			count, ok := int64(stack[n-2].number), stack[n-2].kind == calculatorIntegerKind
			shift, shiftOK := int64(stack[n-1].number), stack[n-1].kind == calculatorIntegerKind
			if !ok || !shiftOK || count < 0 || count > int64(n-2) {
				return fmt.Errorf("invalid calculator roll operands")
			}
			stack = stack[:n-2]
			if count != 0 {
				values := stack[len(stack)-int(count):]
				offset := int((shift%count + count) % count)
				slices.Reverse(values)
				slices.Reverse(values[:offset])
				slices.Reverse(values[offset:])
			}
		default:
			value, err := calculatorOperation(op, stack[n-required:])
			if err != nil {
				return err
			}
			stack = append(stack[:n-required], value)
		}
	}
	if len(stack) != len(output) {
		return fmt.Errorf("invalid calculator result count")
	}
	for i, value := range stack {
		n, ok := calculatorNumber(value)
		if !ok {
			return fmt.Errorf("invalid calculator result type")
		}
		output[i] = n
	}
	return nil
}

// calculatorNumber 取得整数或实数，不将布尔量隐式转换为数字
// 入参: value 栈值
// 返回: float64 数值, bool 是否数字
func calculatorNumber(value calculatorValue) (float64, bool) {
	return value.number, value.kind != calculatorBooleanKind
}

// calculatorInteger 构造整数操作数
// 入参: value 整数值
// 返回: calculatorValue 整数操作数
func calculatorInteger(value int64) calculatorValue {
	return calculatorValue{number: float64(value), kind: calculatorIntegerKind}
}

// calculatorBoolean 构造布尔操作数
// 入参: value 布尔值
// 返回: calculatorValue 布尔操作数
func calculatorBoolean(value bool) calculatorValue {
	if value {
		return calculatorValue{number: 1, kind: calculatorBooleanKind}
	}
	return calculatorValue{kind: calculatorBooleanKind}
}

// calculatorOperation 执行单个算术、比较或位运算，拒绝未定义结果
// 入参: op 操作名, values 操作数
// 返回: calculatorValue 结果, error 类型或算术错误
func calculatorOperation(op string, values []calculatorValue) (calculatorValue, error) {
	a, aNumber := calculatorNumber(values[0])
	b, bNumber := 0.0, false
	if len(values) == 2 {
		b, bNumber = calculatorNumber(values[1])
	}
	ai, aInteger := int64(values[0].number), values[0].kind == calculatorIntegerKind
	var bi int64
	var bInteger bool
	if len(values) == 2 {
		bi, bInteger = int64(values[1].number), values[1].kind == calculatorIntegerKind
	}
	if op == "eq" || op == "ne" {
		equal := values[0] == values[1]
		if aNumber && bNumber {
			equal = a == b
		}
		return calculatorBoolean(equal != (op == "ne")), nil
	}
	switch op {
	case "and", "or", "xor", "not":
		if ab := values[0].number != 0; values[0].kind == calculatorBooleanKind {
			if op == "not" {
				return calculatorBoolean(!ab), nil
			}
			bb, ok := values[1].number != 0, values[1].kind == calculatorBooleanKind
			if !ok {
				return calculatorValue{}, fmt.Errorf("invalid calculator boolean operand")
			}
			switch op {
			case "and":
				return calculatorBoolean(ab && bb), nil
			case "or":
				return calculatorBoolean(ab || bb), nil
			default:
				return calculatorBoolean(ab != bb), nil
			}
		}
		if !aInteger || op != "not" && !bInteger {
			return calculatorValue{}, fmt.Errorf("invalid calculator bitwise operand")
		}
		switch op {
		case "and":
			return calculatorInteger(int64(int32(ai) & int32(bi))), nil
		case "or":
			return calculatorInteger(int64(int32(ai) | int32(bi))), nil
		case "xor":
			return calculatorInteger(int64(int32(ai) ^ int32(bi))), nil
		default:
			return calculatorInteger(int64(^int32(ai))), nil
		}
	case "bitshift":
		if !aInteger || !bInteger {
			return calculatorValue{}, fmt.Errorf("invalid calculator shift operand")
		}
		if bi >= 32 || bi <= -32 {
			return calculatorInteger(0), nil
		}
		if bi < 0 {
			return calculatorInteger(int64(int32(uint32(ai) >> uint(-bi)))), nil
		}
		return calculatorInteger(int64(int32(uint32(ai) << uint(bi)))), nil
	}
	if !aNumber || len(values) == 2 && !bNumber {
		return calculatorValue{}, fmt.Errorf("invalid calculator numeric operand: %s", op)
	}
	result, integer := a, aInteger
	switch op {
	case "add":
		result, integer = a+b, aInteger && bInteger
	case "sub":
		result, integer = a-b, aInteger && bInteger
	case "mul":
		result, integer = a*b, aInteger && bInteger
	case "div", "idiv", "mod":
		if b == 0 {
			return calculatorValue{}, fmt.Errorf("calculator division by zero")
		}
		switch op {
		case "div":
			result, integer = a/b, false
		case "idiv":
			if !aInteger || !bInteger {
				return calculatorValue{}, fmt.Errorf("invalid calculator integer division operand")
			}
			result, integer = math.Trunc(a/b), true
			if result < math.MinInt32 || result > math.MaxInt32 {
				return calculatorValue{}, fmt.Errorf("calculator integer overflow")
			}
		case "mod":
			if !aInteger || !bInteger {
				return calculatorValue{}, fmt.Errorf("invalid calculator modulo operand")
			}
			result, integer = float64(ai%bi), true
		}
	case "abs":
		result = math.Abs(a)
	case "neg":
		result = -a
	case "ceiling":
		result = math.Ceil(a)
	case "floor":
		result = math.Floor(a)
	case "round":
		result = math.Floor(a + 0.5)
	case "truncate":
		result = math.Trunc(a)
	case "cvi":
		result, integer = math.Trunc(a), true
		if result < math.MinInt32 || result > math.MaxInt32 {
			return calculatorValue{}, fmt.Errorf("calculator integer overflow")
		}
	case "cvr":
		integer = false
	case "sin":
		result, integer = math.Sin(math.Mod(a, 360)*math.Pi/180), false
	case "cos":
		result, integer = math.Cos(math.Mod(a, 360)*math.Pi/180), false
	case "atan":
		if a == 0 && b == 0 {
			return calculatorValue{}, fmt.Errorf("undefined calculator angle")
		}
		result, integer = math.Atan2(a, b)*180/math.Pi, false
		if result < 0 {
			result += 360
		}
	case "exp":
		result, integer = math.Pow(a, b), false
	case "sqrt":
		result, integer = math.Sqrt(a), false
	case "ln":
		result, integer = math.Log(a), false
	case "log":
		result, integer = math.Log10(a), false
	case "ge":
		return calculatorBoolean(a >= b), nil
	case "gt":
		return calculatorBoolean(a > b), nil
	case "le":
		return calculatorBoolean(a <= b), nil
	case "lt":
		return calculatorBoolean(a < b), nil
	default:
		return calculatorValue{}, &UnsupportedError{Feature: "calculator operator " + op}
	}
	if math.IsNaN(result) || math.IsInf(result, 0) {
		return calculatorValue{}, fmt.Errorf("undefined calculator result: %s", op)
	}
	if integer && result >= math.MinInt32 && result <= math.MaxInt32 {
		return calculatorInteger(int64(result)), nil
	}
	return calculatorValue{number: result}, nil
}
