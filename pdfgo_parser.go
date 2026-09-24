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
	"encoding/hex"
	"math"
	"strconv"
)

// objectParser 维护对象语法解析位置与嵌套深度
type objectParser struct {
	data  []byte
	pos   int
	base  int64
	depth int
}

// isSpace 判断PDF空白字节
func isSpace(b byte) bool {
	return b == 0 || b == 9 || b == 10 || b == 12 || b == 13 || b == 32
}

// isDelimiter 判断PDF对象分隔符
func isDelimiter(b byte) bool {
	switch b {
	case '(', ')', '<', '>', '[', ']', '{', '}', '/', '%':
		return true
	}
	return isSpace(b)
}

// skipSpace 跳过空白与注释
func (p *objectParser) skipSpace() {
	for p.pos < len(p.data) {
		if isSpace(p.data[p.pos]) {
			p.pos++
		} else if p.data[p.pos] == '%' {
			for p.pos < len(p.data) && p.data[p.pos] != '\r' && p.data[p.pos] != '\n' {
				p.pos++
			}
		} else {
			return
		}
	}
}

// fail 创建当前偏移位置的语法错误
func (p *objectParser) fail(message string) error {
	return &SyntaxError{Offset: p.base + int64(p.pos), Message: message}
}

// token 读取非分隔符组成的词法单元
func (p *objectParser) token() string {
	p.skipSpace()
	start := p.pos
	for p.pos < len(p.data) && !isDelimiter(p.data[p.pos]) {
		p.pos++
	}
	return string(p.data[start:p.pos])
}

// object 读取单个PDF对象
func (p *objectParser) object() (Object, error) {
	p.skipSpace()
	if p.pos == len(p.data) {
		return nil, p.fail("unexpected end of object")
	}
	if p.depth >= 256 {
		return nil, p.fail("object nesting limit exceeded")
	}
	p.depth++
	defer func() { p.depth-- }()
	switch p.data[p.pos] {
	case '/':
		return p.name()
	case '(':
		return p.literalString()
	case '<':
		if p.pos+1 < len(p.data) && p.data[p.pos+1] == '<' {
			return p.dictionary()
		}
		return p.hexString()
	case '[':
		p.pos++
		values := Array{}
		for {
			p.skipSpace()
			if p.pos < len(p.data) && p.data[p.pos] == ']' {
				p.pos++
				return values, nil
			}
			value, err := p.object()
			if err != nil {
				return nil, err
			}
			values = append(values, value)
		}
	}
	token := p.token()
	switch token {
	case "true":
		return Boolean(true), nil
	case "false":
		return Boolean(false), nil
	case "null":
		return nil, nil
	}
	if !validNumber(token) {
		return nil, p.fail("invalid object token")
	}
	if value, err := strconv.ParseInt(token, 10, 64); err == nil {
		saved := p.pos
		generation, err := strconv.ParseInt(p.token(), 10, 64)
		if err == nil && generation >= 0 && generation <= 65535 && value > 0 && p.token() == "R" {
			return Reference{Number: value, Generation: generation}, nil
		}
		p.pos = saved
		return Integer(value), nil
	}
	value, err := strconv.ParseFloat(token, 64)
	if err != nil || math.IsInf(value, 0) || math.IsNaN(value) {
		return nil, p.fail("number out of range")
	}
	return Real(value), nil
}

// validNumber 检查不含指数形式的PDF数字语法
func validNumber(s string) bool {
	if len(s) > 0 && (s[0] == '+' || s[0] == '-') {
		s = s[1:]
	}
	digits, dots := 0, 0
	for _, b := range []byte(s) {
		if b >= '0' && b <= '9' {
			digits++
		} else if b == '.' {
			dots++
		} else {
			return false
		}
	}
	return digits > 0 && dots <= 1
}

// name 读取名称并解除十六进制转义
func (p *objectParser) name() (Object, error) {
	p.pos++
	var value []byte
	for p.pos < len(p.data) && !isDelimiter(p.data[p.pos]) {
		b := p.data[p.pos]
		p.pos++
		if b == '#' {
			if p.pos+2 > len(p.data) {
				return nil, p.fail("incomplete name escape")
			}
			var decoded [1]byte
			if _, err := hex.Decode(decoded[:], p.data[p.pos:p.pos+2]); err != nil {
				return nil, p.fail("invalid name escape")
			}
			b = decoded[0]
			p.pos += 2
		}
		if b == 0 {
			return nil, p.fail("null byte in name")
		}
		value = append(value, b)
	}
	return Name(value), nil
}

// literalString 读取括号字符串并处理嵌套与转义
func (p *objectParser) literalString() (Object, error) {
	p.pos++
	level := 1
	value := String{}
	for p.pos < len(p.data) {
		b := p.data[p.pos]
		p.pos++
		switch b {
		case '(':
			level++
		case ')':
			level--
			if level == 0 {
				return value, nil
			}
		case '\r':
			if p.pos < len(p.data) && p.data[p.pos] == '\n' {
				p.pos++
			}
			b = '\n'
		case '\\':
			if p.pos == len(p.data) {
				return nil, p.fail("incomplete string escape")
			}
			b = p.data[p.pos]
			p.pos++
			switch b {
			case 'n':
				b = '\n'
			case 'r':
				b = '\r'
			case 't':
				b = '\t'
			case 'b':
				b = '\b'
			case 'f':
				b = '\f'
			case '\n':
				continue
			case '\r':
				if p.pos < len(p.data) && p.data[p.pos] == '\n' {
					p.pos++
				}
				continue
			default:
				if b >= '0' && b <= '7' {
					v := int(b - '0')
					for n := 1; n < 3 && p.pos < len(p.data) && p.data[p.pos] >= '0' && p.data[p.pos] <= '7'; n++ {
						v = v*8 + int(p.data[p.pos]-'0')
						p.pos++
					}
					b = byte(v)
				}
			}
		}
		value = append(value, b)
	}
	return nil, p.fail("unterminated literal string")
}

// hexString 读取十六进制字符串
func (p *objectParser) hexString() (Object, error) {
	p.pos++
	var digits []byte
	for p.pos < len(p.data) {
		b := p.data[p.pos]
		p.pos++
		if b == '>' {
			if len(digits)%2 != 0 {
				digits = append(digits, '0')
			}
			value := make(String, len(digits)/2)
			if _, err := hex.Decode(value, digits); err != nil {
				return nil, p.fail("invalid hex string")
			}
			return value, nil
		}
		if !isSpace(b) {
			digits = append(digits, b)
		}
	}
	return nil, p.fail("unterminated hex string")
}

// dictionary 读取字典并拒绝有歧义的重复键
func (p *objectParser) dictionary() (Object, error) {
	p.pos += 2
	value := Dictionary{}
	for {
		p.skipSpace()
		if p.pos+1 < len(p.data) && p.data[p.pos] == '>' && p.data[p.pos+1] == '>' {
			p.pos += 2
			return value, nil
		}
		if p.pos == len(p.data) || p.data[p.pos] != '/' {
			return nil, p.fail("expected dictionary key")
		}
		key, err := p.name()
		if err != nil {
			return nil, err
		}
		name := key.(Name)
		if _, exists := value[name]; exists {
			return nil, p.fail("duplicate dictionary key")
		}
		entry, err := p.object()
		if err != nil {
			return nil, err
		}
		value[name] = entry
	}
}
