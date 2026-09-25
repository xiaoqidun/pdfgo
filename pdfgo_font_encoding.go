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
	_ "embed"
	"strconv"
	"strings"
	"sync"
)

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
