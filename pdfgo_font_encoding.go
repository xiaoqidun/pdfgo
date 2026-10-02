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
	"encoding/binary"
	"fmt"
	"strconv"
	"strings"
)

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

// pdfMacExpertNames 按PDF32000-1附录D.4保存MacExpertEncoding，与CFFExpertEncoding区分
var pdfMacExpertNames = [256]string{
	0o040: "space",
	0o041: "exclamsmall",
	0o042: "Hungarumlautsmall",
	0o043: "centoldstyle",
	0o044: "dollaroldstyle",
	0o045: "dollarsuperior",
	0o046: "ampersandsmall",
	0o047: "Acutesmall",
	0o050: "parenleftsuperior",
	0o051: "parenrightsuperior",
	0o052: "twodotenleader",
	0o053: "onedotenleader",
	0o054: "comma",
	0o055: "hyphen",
	0o056: "period",
	0o057: "fraction",
	0o060: "zerooldstyle",
	0o061: "oneoldstyle",
	0o062: "twooldstyle",
	0o063: "threeoldstyle",
	0o064: "fouroldstyle",
	0o065: "fiveoldstyle",
	0o066: "sixoldstyle",
	0o067: "sevenoldstyle",
	0o070: "eightoldstyle",
	0o071: "nineoldstyle",
	0o072: "colon",
	0o073: "semicolon",
	0o075: "threequartersemdash",
	0o077: "questionsmall",
	0o104: "Ethsmall",
	0o107: "onequarter",
	0o110: "onehalf",
	0o111: "threequarters",
	0o112: "oneeighth",
	0o113: "threeeighths",
	0o114: "fiveeighths",
	0o115: "seveneighths",
	0o116: "onethird",
	0o117: "twothirds",
	0o126: "ff",
	0o127: "fi",
	0o130: "fl",
	0o131: "ffi",
	0o132: "ffl",
	0o133: "parenleftinferior",
	0o135: "parenrightinferior",
	0o136: "Circumflexsmall",
	0o137: "hypheninferior",
	0o140: "Gravesmall",
	0o141: "Asmall",
	0o142: "Bsmall",
	0o143: "Csmall",
	0o144: "Dsmall",
	0o145: "Esmall",
	0o146: "Fsmall",
	0o147: "Gsmall",
	0o150: "Hsmall",
	0o151: "Ismall",
	0o152: "Jsmall",
	0o153: "Ksmall",
	0o154: "Lsmall",
	0o155: "Msmall",
	0o156: "Nsmall",
	0o157: "Osmall",
	0o160: "Psmall",
	0o161: "Qsmall",
	0o162: "Rsmall",
	0o163: "Ssmall",
	0o164: "Tsmall",
	0o165: "Usmall",
	0o166: "Vsmall",
	0o167: "Wsmall",
	0o170: "Xsmall",
	0o171: "Ysmall",
	0o172: "Zsmall",
	0o173: "colonmonetary",
	0o174: "onefitted",
	0o175: "rupiah",
	0o176: "Tildesmall",
	0o201: "asuperior",
	0o202: "centsuperior",
	0o207: "Aacutesmall",
	0o210: "Agravesmall",
	0o211: "Acircumflexsmall",
	0o212: "Adieresissmall",
	0o213: "Atildesmall",
	0o214: "Aringsmall",
	0o215: "Ccedillasmall",
	0o216: "Eacutesmall",
	0o217: "Egravesmall",
	0o220: "Ecircumflexsmall",
	0o221: "Edieresissmall",
	0o222: "Iacutesmall",
	0o223: "Igravesmall",
	0o224: "Icircumflexsmall",
	0o225: "Idieresissmall",
	0o226: "Ntildesmall",
	0o227: "Oacutesmall",
	0o230: "Ogravesmall",
	0o231: "Ocircumflexsmall",
	0o232: "Odieresissmall",
	0o233: "Otildesmall",
	0o234: "Uacutesmall",
	0o235: "Ugravesmall",
	0o236: "Ucircumflexsmall",
	0o237: "Udieresissmall",
	0o241: "eightsuperior",
	0o242: "fourinferior",
	0o243: "threeinferior",
	0o244: "sixinferior",
	0o245: "eightinferior",
	0o246: "seveninferior",
	0o247: "Scaronsmall",
	0o251: "centinferior",
	0o252: "twoinferior",
	0o254: "Dieresissmall",
	0o256: "Caronsmall",
	0o257: "osuperior",
	0o260: "fiveinferior",
	0o262: "commainferior",
	0o263: "periodinferior",
	0o264: "Yacutesmall",
	0o266: "dollarinferior",
	0o271: "Thornsmall",
	0o273: "nineinferior",
	0o274: "zeroinferior",
	0o275: "Zcaronsmall",
	0o276: "AEsmall",
	0o277: "Oslashsmall",
	0o300: "questiondownsmall",
	0o301: "oneinferior",
	0o302: "Lslashsmall",
	0o311: "Cedillasmall",
	0o317: "OEsmall",
	0o320: "figuredash",
	0o321: "hyphensuperior",
	0o326: "exclamdownsmall",
	0o330: "Ydieresissmall",
	0o332: "onesuperior",
	0o333: "twosuperior",
	0o334: "threesuperior",
	0o335: "foursuperior",
	0o336: "fivesuperior",
	0o337: "sixsuperior",
	0o340: "sevensuperior",
	0o341: "ninesuperior",
	0o342: "zerosuperior",
	0o344: "esuperior",
	0o345: "rsuperior",
	0o346: "tsuperior",
	0o351: "isuperior",
	0o352: "ssuperior",
	0o353: "dsuperior",
	0o361: "lsuperior",
	0o362: "Ogoneksmall",
	0o363: "Brevesmall",
	0o364: "Macronsmall",
	0o365: "bsuperior",
	0o366: "nsuperior",
	0o367: "msuperior",
	0o370: "commasuperior",
	0o371: "periodsuperior",
	0o372: "Dotaccentsmall",
	0o373: "Ringsmall",
}

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

// type1EncodingScanner 按词项读取Type1字体明文段
type type1EncodingScanner struct {
	data []byte
	pos  int
}

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
