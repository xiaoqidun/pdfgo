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
	"encoding/hex"
	"math"
	"strconv"
	"strings"
)

// objectParser 维护解析位置、嵌套深度及试探读取的窗口耗尽状态
type objectParser struct {
	data      []byte
	pos       int
	base      int64
	depth     int
	exhausted bool
	final     bool
	content   bool
	warning   func(Diagnostic)
}

// isSpace 判断PDF空白字节
// 入参: b 待检查的字节
// 返回: bool 是否为空白字节
func isSpace(b byte) bool {
	return b == 0 || b == 9 || b == 10 || b == 12 || b == 13 || b == 32
}

// isDelimiter 判断PDF空白字节或对象分隔符
// 入参: b 待检查的字节
// 返回: bool 是否为词法边界
func isDelimiter(b byte) bool {
	switch b {
	case '(', ')', '<', '>', '[', ']', '{', '}', '/', '%':
		return true
	}
	return isSpace(b)
}

// parseFastInt 无分配解析常见十进制整数，超长或非纯整数交由通用路径处理
// 入参: raw 原始词法字节
// 返回: int64 整数值, bool 是否快速解析成功
func parseFastInt(raw []byte) (int64, bool) {
	if len(raw) == 0 || len(raw) > 18 {
		return 0, false
	}
	i, neg := 0, false
	if raw[0] == '-' {
		neg = true
		i = 1
	} else if raw[0] == '+' {
		i = 1
	}
	if i == len(raw) {
		return 0, false
	}
	var value int64
	for ; i < len(raw); i++ {
		b := raw[i]
		if b < '0' || b > '9' {
			return 0, false
		}
		value = value*10 + int64(b-'0')
	}
	if neg {
		value = -value
	}
	return value, true
}

// parseGeneration 解析间接引用中的非负代数，支持任意前导零并避免常见场景分配
// 入参: raw 原始词法字节
// 返回: int64 代数值, bool 是否为合法代数
func parseGeneration(raw []byte) (int64, bool) {
	if generation, ok := parseFastInt(raw); ok {
		return generation, generation >= 0 && generation <= 65535
	}
	if len(raw) == 0 {
		return 0, false
	}
	s := string(raw)
	if !validNumber(s) || strings.ContainsRune(s, '.') {
		return 0, false
	}
	generation, err := strconv.ParseInt(s, 10, 64)
	return generation, err == nil && generation >= 0 && generation <= 65535
}

// validNumber 检查不含指数形式的PDF数字语法
// 入参: s 数字文本
// 返回: bool 是否符合数字语法
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

// validScientificNumber 识别有限十进制指数语法，不允许NaN、Inf和非十进制形式
// 入参: s 数字文本
// 返回: bool 是否为十进制指数语法
func validScientificNumber(s string) bool {
	for i, b := range []byte(s) {
		if b != 'e' && b != 'E' {
			continue
		}
		if !validNumber(s[:i]) {
			return false
		}
		exponent := s[i+1:]
		if len(exponent) > 0 && (exponent[0] == '+' || exponent[0] == '-') {
			exponent = exponent[1:]
		}
		if exponent == "" {
			return false
		}
		for _, digit := range []byte(exponent) {
			if digit < '0' || digit > '9' {
				return false
			}
		}
		return true
	}
	return false
}

// fontNamePart 限定可恢复的未转义字体名称片段，不接收PDF结构关键字
// 入参: part 空白分隔的名称片段
// 返回: bool 是否为普通字体标识符
func fontNamePart(part string) bool {
	switch part {
	case "obj", "endobj", "stream", "endstream", "xref", "trailer", "startxref", "true", "false", "null", "R":
		return false
	}
	for _, b := range []byte(part) {
		if b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z' || b >= '0' && b <= '9' || b == '-' || b == '_' || b == '+' || b == ',' || b == '.' {
			continue
		}
		return false
	}
	return part != ""
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
	p.exhausted = true
}

// fail 创建当前偏移位置的语法错误
// 入参: message 错误描述
// 返回: error 包含字节偏移的语法错误
func (p *objectParser) fail(message string) error {
	return &SyntaxError{Offset: p.base + int64(p.pos), Message: message}
}

// tokenBytes 读取非分隔符组成的词法单元原始切片，不分配字符串
// 返回: []byte 词法单元切片，遇到分隔符或输入末尾时为空
func (p *objectParser) tokenBytes() []byte {
	p.skipSpace()
	start := p.pos
	for p.pos < len(p.data) && !isDelimiter(p.data[p.pos]) {
		p.pos++
	}
	if p.pos == len(p.data) {
		p.exhausted = true
	}
	return p.data[start:p.pos]
}

// token 读取非分隔符组成的词法单元
// 返回: string 词法单元，遇到分隔符或输入末尾时为空
func (p *objectParser) token() string {
	return string(p.tokenBytes())
}

// streamDataStart 校验流头换行，诊断模式允许单独CR或换行前的水平空白
// 返回: bool 是否找到明确的数据起点
func (p *objectParser) streamDataStart() bool {
	start := p.pos
	if p.warning != nil {
		for p.pos < len(p.data) && (p.data[p.pos] == ' ' || p.data[p.pos] == '\t' || p.data[p.pos] == 0 || p.data[p.pos] == '\f') {
			p.pos++
		}
	}
	if p.pos == len(p.data) {
		p.exhausted = true
		return false
	}
	if p.data[p.pos] == '\r' {
		p.pos++
		if p.pos == len(p.data) {
			p.exhausted = true
			return false
		}
		if p.data[p.pos] != '\n' {
			if p.warning == nil {
				return false
			}
			p.warning(Diagnostic{Offset: p.base + int64(start), Message: "stream line ending recovered from CR"})
			return true
		}
	}
	if p.data[p.pos] != '\n' {
		return false
	}
	p.pos++
	if p.warning != nil && p.pos-start > 1 && p.data[start] != '\r' {
		p.warning(Diagnostic{Offset: p.base + int64(start), Message: "horizontal whitespace before stream line ending"})
	}
	return true
}

// object 读取单个PDF对象
// 返回: Object 对象，null对应nil, error 语法错误
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
			if p.pos == len(p.data) && p.final && p.warning != nil {
				p.warning(Diagnostic{Offset: p.base + int64(p.pos), Message: "array closed at source EOF; parsed elements retained"})
				return values, nil
			}
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
	raw := p.tokenBytes()
	if value, ok := parseFastInt(raw); ok {
		return p.integerOrReference(value), nil
	}
	if len(raw) == 4 && bytes.Equal(raw, []byte("true")) {
		return Boolean(true), nil
	}
	if len(raw) == 5 && bytes.Equal(raw, []byte("false")) {
		return Boolean(false), nil
	}
	if len(raw) == 4 && bytes.Equal(raw, []byte("null")) {
		return nil, nil
	}
	token := string(raw)
	if p.content && p.warning != nil {
		token = p.contentNumber(token)
	}
	if !p.content && p.warning != nil {
		if prefix, ok := strings.CutSuffix(token, "endobj"); ok && validNumber(prefix) {
			token = prefix
			p.pos -= len("endobj")
			p.warning(Diagnostic{Offset: p.base + int64(p.pos), Message: "numeric object adjacent to endobj; numeric value retained"})
		}
	}
	valid := validNumber(token)
	scientific := !valid && p.warning != nil && validScientificNumber(token)
	if !valid && !scientific {
		return nil, p.fail("invalid object token")
	}
	if scientific {
		p.warning(Diagnostic{Offset: p.base + int64(p.pos-len(token)), Message: "non-standard scientific numeric notation"})
	}
	if !strings.ContainsAny(token, ".eE") {
		if value, err := strconv.ParseInt(token, 10, 64); err == nil {
			return p.integerOrReference(value), nil
		}
	}
	value, err := strconv.ParseFloat(token, 64)
	if err != nil || math.IsInf(value, 0) || math.IsNaN(value) {
		return nil, p.fail("number out of range")
	}
	return Real(value), nil
}

// integerOrReference 将已解析整数按上下文尝试组合为间接引用，否则回退返回整数
// 入参: value 整数值
// 返回: Object 间接引用或整数对象
func (p *objectParser) integerOrReference(value int64) Object {
	if p.content {
		return Integer(value)
	}
	saved := p.pos
	if generation, ok := parseGeneration(p.tokenBytes()); ok && (value > 0 || (value == 0 && p.warning != nil)) {
		if refRaw := p.tokenBytes(); len(refRaw) == 1 && refRaw[0] == 'R' {
			if value == 0 {
				p.warning(Diagnostic{Offset: p.base + int64(saved), Message: "zero object number in indirect reference treated as null"})
			}
			return Reference{Number: value, Generation: generation}
		}
	}
	p.pos = saved
	return Integer(value)
}

// name 读取名称并解除十六进制转义
// 返回: Object 名称对象, error 语法错误
func (p *objectParser) name() (Object, error) {
	p.pos++
	start := p.pos
	for p.pos < len(p.data) && !isDelimiter(p.data[p.pos]) && p.data[p.pos] != '#' {
		p.pos++
	}
	if p.pos == len(p.data) || isDelimiter(p.data[p.pos]) {
		return Name(p.data[start:p.pos]), nil
	}
	value := append([]byte(nil), p.data[start:p.pos]...)
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
// 返回: Object 字符串对象, error 语法错误
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
	if p.final && p.warning != nil {
		p.warning(Diagnostic{Offset: p.base + int64(p.pos), Message: "literal string terminated at source EOF; decoded bytes retained"})
		return value, nil
	}
	return nil, p.fail("unterminated literal string")
}

// hexString 读取十六进制字符串
// 返回: Object 字符串对象, error 语法错误
func (p *objectParser) hexString() (Object, error) {
	p.pos++
	var digits []byte
	invalid := false
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
			if p.warning != nil && !(b >= '0' && b <= '9' || b >= 'a' && b <= 'f' || b >= 'A' && b <= 'F') {
				if !invalid {
					p.warning(Diagnostic{Offset: p.base + int64(p.pos-1), Message: "invalid hex string digits ignored; valid nibbles retained"})
					invalid = true
				}
				continue
			}
			digits = append(digits, b)
		}
	}
	if p.final && p.warning != nil {
		if len(digits)%2 != 0 {
			digits = append(digits, '0')
		}
		value := make(String, len(digits)/2)
		if _, err := hex.Decode(value, digits); err != nil {
			return nil, p.fail("invalid hex string")
		}
		p.warning(Diagnostic{Offset: p.base + int64(p.pos), Message: "hex string terminated at source EOF; decoded bytes retained"})
		return value, nil
	}
	return nil, p.fail("unterminated hex string")
}

// dictionary 读取字典并拒绝有歧义的重复键
// 返回: Object 字典对象, error 语法错误
func (p *objectParser) dictionary() (Object, error) {
	p.pos += 2
	value := Dictionary{}
	for {
		p.skipSpace()
		if p.pos == len(p.data) && p.final && p.warning != nil {
			p.warning(Diagnostic{Offset: p.base + int64(p.pos), Message: "dictionary closed at source EOF; parsed entries retained"})
			return value, nil
		}
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
			if p.warning == nil {
				return nil, p.fail("duplicate dictionary key")
			}
			p.warning(Diagnostic{Offset: p.base + int64(p.pos), Message: "duplicate dictionary key " + string(name) + "; last value retained"})
		}
		entry, err := p.object()
		if err != nil {
			return nil, err
		}
		if font, ok := entry.(Name); ok && p.warning != nil && (name == "BaseFont" || name == "FontName") {
			for count := 0; count < 8; count++ {
				saved := p.pos
				part := p.token()
				if part == "" {
					p.pos = saved
					break
				}
				if !fontNamePart(part) || len(font)+len(part) > 2048 {
					return nil, p.fail("invalid unescaped font name")
				}
				font += Name(" " + part)
				p.warning(Diagnostic{Offset: p.base + int64(saved), Message: "unescaped space in font name; complete name retained"})
			}
			entry = font
		}
		value[name] = entry
	}
}
