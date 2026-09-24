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
	"strconv"
)

// SoftMask 保存灰度亮度蒙版及线性传递函数，Transfer为零值和单位值的输出
type SoftMask struct {
	Background  float64
	Transfer    [2]float64
	interpreter pageInterpreter
	stream      *Stream
}

// Walk 访问蒙版图元，坐标与引用蒙版的页面一致
// 入参: visitor 蒙版图元访问器
// 返回: error 解析或访问错误
func (m *SoftMask) Walk(visitor Visitor) error {
	p := m.interpreter
	p.visitor = visitor
	return p.form(m.stream)
}

// readSoftMask 读取灰度亮度蒙版，保留图形与线性传递关系
// 入参: value 蒙版字典
// 返回: *SoftMask 蒙版信息, error 错误信息
func (p *pageInterpreter) readSoftMask(value Object) (*SoftMask, error) {
	dict, ok := value.(Dictionary)
	if !ok || dict["S"] != Name("Luminosity") {
		return nil, &UnsupportedError{Feature: "soft mask subtype"}
	}
	v, err := p.reader.Resolve(dict["G"])
	if err != nil {
		return nil, err
	}
	stream, ok := v.(*Stream)
	if !ok || stream.Dictionary["Subtype"] != Name("Form") {
		return nil, fmt.Errorf("invalid soft mask group")
	}
	v, err = p.reader.Resolve(stream.Dictionary["Group"])
	if err != nil {
		return nil, err
	}
	group, ok := v.(Dictionary)
	if !ok || group["CS"] != Name("DeviceGray") {
		return nil, &UnsupportedError{Feature: "soft mask blending color space"}
	}
	m := &SoftMask{Transfer: [2]float64{0, 1}, interpreter: *p, stream: stream}
	m.interpreter.maskGroup = true
	m.interpreter.state.style.SoftMask = nil
	m.interpreter.state.style.Clips = nil
	m.interpreter.state.style.Fill.Alpha, m.interpreter.state.style.Stroke.Alpha = 1, 1
	if dict["BC"] != nil {
		v, err := p.reader.Resolve(dict["BC"])
		if err != nil {
			return nil, err
		}
		a, ok := v.(Array)
		if !ok {
			return nil, fmt.Errorf("invalid soft mask backdrop")
		}
		n, err := numbers(a, 1)
		if err != nil || n[0] < 0 || n[0] > 1 {
			return nil, fmt.Errorf("invalid soft mask backdrop")
		}
		m.Background = n[0]
	}
	if dict["TR"] != nil && dict["TR"] != Name("Identity") {
		v, err := p.reader.Resolve(dict["TR"])
		if err != nil {
			return nil, err
		}
		function, ok := v.(*Stream)
		if !ok || function.Dictionary["FunctionType"] != Integer(4) {
			return nil, &UnsupportedError{Feature: "soft mask transfer function"}
		}
		for _, key := range []Name{"Domain", "Range"} {
			v, err := p.reader.Resolve(function.Dictionary[key])
			if err != nil {
				return nil, err
			}
			a, ok := v.(Array)
			if !ok {
				return nil, fmt.Errorf("invalid transfer function %s", key)
			}
			n, err := numbers(a, 2)
			if err != nil || n[0] != 0 || n[1] != 1 {
				return nil, &UnsupportedError{Feature: "transfer function domain or range"}
			}
		}
		data, err := function.Decode()
		if err != nil {
			return nil, err
		}
		m.Transfer, err = linearCalculator(data)
		if err != nil {
			return nil, err
		}
	}
	return m, nil
}

// linearCalculator 解析计算器函数的仿射运算，不将非线性函数近似为直线
// 入参: data 计算器函数内容
// 返回: [2]float64 函数端点值, error 非线性或语法错误
func linearCalculator(data []byte) ([2]float64, error) {
	p := objectParser{data: data}
	p.skipSpace()
	if p.pos == len(data) || data[p.pos] != '{' {
		return [2]float64{}, fmt.Errorf("invalid calculator procedure")
	}
	p.pos++
	stack := [][2]float64{{0, 1}}
	for {
		p.skipSpace()
		if p.pos == len(data) {
			return [2]float64{}, fmt.Errorf("unterminated calculator procedure")
		}
		if data[p.pos] == '}' {
			p.pos++
			break
		}
		token := p.token()
		if v, err := strconv.ParseFloat(token, 64); err == nil && !math.IsNaN(v) && !math.IsInf(v, 0) {
			stack = append(stack, [2]float64{v, v})
			continue
		}
		n := len(stack)
		required := 2
		if token == "dup" || token == "pop" || token == "neg" {
			required = 1
		}
		if n < required {
			return [2]float64{}, fmt.Errorf("calculator stack underflow")
		}
		switch token {
		case "dup":
			stack = append(stack, stack[n-1])
		case "pop":
			stack = stack[:n-1]
		case "exch":
			stack[n-1], stack[n-2] = stack[n-2], stack[n-1]
		case "neg":
			stack[n-1] = [2]float64{-stack[n-1][0], -stack[n-1][1]}
		case "add", "sub", "mul", "div":
			a, b := stack[n-2], stack[n-1]
			if token == "mul" && a[0] != a[1] && b[0] != b[1] || token == "div" && (b[0] != b[1] || b[0] == 0) {
				return [2]float64{}, &UnsupportedError{Feature: "nonlinear calculator function"}
			}
			for i := range a {
				switch token {
				case "add":
					a[i] += b[i]
				case "sub":
					a[i] -= b[i]
				case "mul":
					a[i] *= b[i]
				case "div":
					a[i] /= b[i]
				}
			}
			stack[n-2] = a
			stack = stack[:n-1]
		default:
			return [2]float64{}, &UnsupportedError{Feature: "calculator operator " + token}
		}
	}
	p.skipSpace()
	if p.pos != len(data) || len(stack) != 1 {
		return [2]float64{}, fmt.Errorf("invalid calculator result")
	}
	for _, v := range stack[0] {
		if math.IsNaN(v) || math.IsInf(v, 0) {
			return [2]float64{}, fmt.Errorf("nonfinite calculator result")
		}
	}
	return stack[0], nil
}
