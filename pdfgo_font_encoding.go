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
	_ "embed"
	"encoding/binary"
	"fmt"
	"strconv"
	"strings"
	"sync"
)

// type1BuiltInEncoding 读取嵌入Type1字体明文段中的内建编码表
// 入参: program PFA或PFB字体数据
// 返回: map[uint32]string 字码与字形名称, error 错误信息
func type1BuiltInEncoding(program []byte) (map[uint32]string, error) {
	if len(program) >= 6 && program[0] == 0x80 && program[1] == 1 {
		length := binary.LittleEndian.Uint32(program[2:6])
		if uint64(length) > uint64(len(program)-6) {
			return nil, fmt.Errorf("truncated PFB header")
		}
		program = program[6 : 6+length]
	}
	if at := bytes.Index(program, []byte("eexec")); at >= 0 {
		program = program[:at]
	}
	scanner := type1EncodingScanner{data: program}
	for token := scanner.next(); token != ""; token = scanner.next() {
		if token != "/Encoding" {
			continue
		}
		switch scanner.next() {
		case "StandardEncoding":
			names := make(map[uint32]string, 256)
			for code, name := range pdfStandardNames {
				names[uint32(code)] = name
			}
			return names, nil
		case "256":
			if scanner.next() != "array" {
				return nil, fmt.Errorf("invalid Type1 encoding array")
			}
			names := make(map[uint32]string, 256)
			for code := 0; code < 256; code++ {
				names[uint32(code)] = ".notdef"
			}
			for token := scanner.next(); token != ""; token = scanner.next() {
				if token == "def" {
					return names, nil
				}
				if token != "dup" {
					continue
				}
				code, err := strconv.Atoi(scanner.next())
				if err != nil || code < 0 || code > 255 {
					return nil, fmt.Errorf("invalid Type1 encoding code")
				}
				name := scanner.next()
				if len(name) < 2 || name[0] != '/' || scanner.next() != "put" {
					return nil, fmt.Errorf("invalid Type1 encoding entry")
				}
				names[uint32(code)] = name[1:]
			}
			return nil, fmt.Errorf("incomplete Type1 encoding")
		default:
			return nil, &UnsupportedError{Feature: "Type1 built-in encoding"}
		}
	}
	return nil, fmt.Errorf("Type1 font has no built-in encoding")
}

type type1EncodingScanner struct {
	data []byte
	pos  int
}

// next 读取Type1明文段的下一个PostScript词项
// 返回: string 词项
func (s *type1EncodingScanner) next() string {
	for s.pos < len(s.data) {
		c := s.data[s.pos]
		if c == ' ' || c == '\t' || c == '\r' || c == '\n' || c == '\f' {
			s.pos++
			continue
		}
		if c == '%' {
			for s.pos < len(s.data) && s.data[s.pos] != '\r' && s.data[s.pos] != '\n' {
				s.pos++
			}
			continue
		}
		break
	}
	if s.pos == len(s.data) {
		return ""
	}
	start := s.pos
	if bytes.IndexByte([]byte("[]{}"), s.data[s.pos]) >= 0 {
		s.pos++
		return string(s.data[start:s.pos])
	}
	for s.pos < len(s.data) && s.data[s.pos] != ' ' && s.data[s.pos] != '\t' && s.data[s.pos] != '\r' && s.data[s.pos] != '\n' && bytes.IndexByte([]byte("[]{}%"), s.data[s.pos]) < 0 {
		s.pos++
	}
	return string(s.data[start:s.pos])
}

//go:embed assets/glyph/glyphlist.txt
var adobeGlyphList []byte

var adobeGlyphNames = sync.OnceValue(func() map[string]string {
	names := make(map[string]string)
	for _, line := range strings.Split(string(adobeGlyphList), "\n") {
		if line == "" || line[0] == '#' {
			continue
		}
		name, codes, ok := strings.Cut(line, ";")
		if !ok {
			continue
		}
		var value strings.Builder
		for _, code := range strings.Fields(codes) {
			n, err := strconv.ParseUint(code, 16, 32)
			if err != nil {
				continue
			}
			value.WriteRune(rune(n))
		}
		names[name] = value.String()
	}
	return names
})

// glyphNameUnicode 按Adobe字形名称规则取得Unicode文本
// 入参: name 字形名称
// 返回: string Unicode文本, bool 是否有明确映射
func glyphNameUnicode(name string) (string, bool) {
	name, _, _ = strings.Cut(name, ".")
	if value, ok := adobeGlyphNames()[name]; ok {
		return value, true
	}
	if strings.Contains(name, "_") {
		var value strings.Builder
		for _, part := range strings.Split(name, "_") {
			text, ok := glyphNameUnicode(part)
			if !ok {
				return "", false
			}
			value.WriteString(text)
		}
		return value.String(), true
	}
	if strings.HasPrefix(name, "uni") && len(name) > 3 && (len(name)-3)%4 == 0 {
		var value strings.Builder
		for i := 3; i < len(name); i += 4 {
			n, err := strconv.ParseUint(name[i:i+4], 16, 16)
			if err != nil || n >= 0xD800 && n <= 0xDFFF {
				return "", false
			}
			value.WriteRune(rune(n))
		}
		return value.String(), true
	}
	if strings.HasPrefix(name, "u") && len(name) >= 5 && len(name) <= 7 {
		n, err := strconv.ParseUint(name[1:], 16, 32)
		if err == nil && n <= 0x10FFFF && (n < 0xD800 || n > 0xDFFF) {
			return string(rune(n)), true
		}
	}
	return "", false
}

// pdfWinAnsiNames 保存PDF标准简单字体编码的字形名称
var pdfWinAnsiNames = strings.Fields(`
.notdef .notdef .notdef .notdef .notdef .notdef .notdef .notdef .notdef .notdef .notdef .notdef .notdef .notdef .notdef .notdef
.notdef .notdef .notdef .notdef .notdef .notdef .notdef .notdef .notdef .notdef .notdef .notdef .notdef .notdef .notdef .notdef
space exclam quotedbl numbersign dollar percent ampersand quotesingle parenleft parenright asterisk plus comma hyphen period slash
zero one two three four five six seven eight nine colon semicolon less equal greater question
at A B C D E F G H I J K L M N O
P Q R S T U V W X Y Z bracketleft backslash bracketright asciicircum underscore
grave a b c d e f g h i j k l m n o
p q r s t u v w x y z braceleft bar braceright asciitilde bullet
Euro bullet quotesinglbase florin quotedblbase ellipsis dagger daggerdbl circumflex perthousand Scaron guilsinglleft OE bullet Zcaron bullet
bullet quoteleft quoteright quotedblleft quotedblright bullet endash emdash tilde trademark scaron guilsinglright oe bullet zcaron Ydieresis
space exclamdown cent sterling currency yen brokenbar section dieresis copyright ordfeminine guillemotleft logicalnot hyphen registered macron
degree plusminus twosuperior threesuperior acute mu paragraph periodcentered cedilla onesuperior ordmasculine guillemotright onequarter onehalf threequarters questiondown
Agrave Aacute Acircumflex Atilde Adieresis Aring AE Ccedilla Egrave Eacute Ecircumflex Edieresis Igrave Iacute Icircumflex Idieresis
Eth Ntilde Ograve Oacute Ocircumflex Otilde Odieresis multiply Oslash Ugrave Uacute Ucircumflex Udieresis Yacute Thorn germandbls
agrave aacute acircumflex atilde adieresis aring ae ccedilla egrave eacute ecircumflex edieresis igrave iacute icircumflex idieresis
eth ntilde ograve oacute ocircumflex otilde odieresis divide oslash ugrave uacute ucircumflex udieresis yacute thorn ydieresis
`)

// pdfMacRomanNames 保存PDF标准简单字体编码的字形名称
var pdfMacRomanNames = strings.Fields(`
.notdef .notdef .notdef .notdef .notdef .notdef .notdef .notdef .notdef .notdef .notdef .notdef .notdef .notdef .notdef .notdef
.notdef .notdef .notdef .notdef .notdef .notdef .notdef .notdef .notdef .notdef .notdef .notdef .notdef .notdef .notdef .notdef
space exclam quotedbl numbersign dollar percent ampersand quotesingle parenleft parenright asterisk plus comma hyphen period slash
zero one two three four five six seven eight nine colon semicolon less equal greater question
at A B C D E F G H I J K L M N O
P Q R S T U V W X Y Z bracketleft backslash bracketright asciicircum underscore
grave a b c d e f g h i j k l m n o
p q r s t u v w x y z braceleft bar braceright asciitilde .notdef
Adieresis Aring Ccedilla Eacute Ntilde Odieresis Udieresis aacute agrave acircumflex adieresis atilde aring ccedilla eacute egrave
ecircumflex edieresis iacute igrave icircumflex idieresis ntilde oacute ograve ocircumflex odieresis otilde uacute ugrave ucircumflex udieresis
dagger degree cent sterling section bullet paragraph germandbls registered copyright trademark acute dieresis notequal AE Oslash
infinity plusminus lessequal greaterequal yen mu partialdiff summation product pi integral ordfeminine ordmasculine Omega ae oslash
questiondown exclamdown logicalnot radical florin approxequal Delta guillemotleft guillemotright ellipsis space Agrave Atilde Otilde OE oe
endash emdash quotedblleft quotedblright quoteleft quoteright divide lozenge ydieresis Ydieresis fraction currency guilsinglleft guilsinglright fi fl
daggerdbl periodcentered quotesinglbase quotedblbase perthousand Acircumflex Ecircumflex Aacute Edieresis Egrave Iacute Icircumflex Idieresis Igrave Oacute Ocircumflex
apple Ograve Uacute Ucircumflex Ugrave dotlessi circumflex tilde macron breve dotaccent ring cedilla hungarumlaut ogonek caron
`)

// pdfStandardNames 保存PDF标准简单字体编码的字形名称
var pdfStandardNames = strings.Fields(`
.notdef .notdef .notdef .notdef .notdef .notdef .notdef .notdef .notdef .notdef .notdef .notdef .notdef .notdef .notdef .notdef
.notdef .notdef .notdef .notdef .notdef .notdef .notdef .notdef .notdef .notdef .notdef .notdef .notdef .notdef .notdef .notdef
space exclam quotedbl numbersign dollar percent ampersand quoteright parenleft parenright asterisk plus comma hyphen period slash
zero one two three four five six seven eight nine colon semicolon less equal greater question
at A B C D E F G H I J K L M N O
P Q R S T U V W X Y Z bracketleft backslash bracketright asciicircum underscore
quoteleft a b c d e f g h i j k l m n o
p q r s t u v w x y z braceleft bar braceright asciitilde .notdef
.notdef .notdef .notdef .notdef .notdef .notdef .notdef .notdef .notdef .notdef .notdef .notdef .notdef .notdef .notdef .notdef
.notdef .notdef .notdef .notdef .notdef .notdef .notdef .notdef .notdef .notdef .notdef .notdef .notdef .notdef .notdef .notdef
.notdef exclamdown cent sterling fraction yen florin section currency quotesingle quotedblleft guillemotleft guilsinglleft guilsinglright fi fl
.notdef endash dagger daggerdbl periodcentered .notdef paragraph bullet quotesinglbase quotedblbase quotedblright guillemotright ellipsis perthousand .notdef questiondown
.notdef grave acute circumflex tilde macron breve dotaccent dieresis .notdef ring cedilla .notdef hungarumlaut ogonek caron
emdash .notdef .notdef .notdef .notdef .notdef .notdef .notdef .notdef .notdef .notdef .notdef .notdef .notdef .notdef .notdef
.notdef AE .notdef ordfeminine .notdef .notdef .notdef .notdef Lslash Oslash OE ordmasculine .notdef .notdef .notdef .notdef
.notdef ae .notdef .notdef .notdef dotlessi .notdef .notdef lslash oslash oe germandbls .notdef .notdef .notdef .notdef
`)
