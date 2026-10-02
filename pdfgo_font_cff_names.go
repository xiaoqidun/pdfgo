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

import "strings"

// cffExpertCharset 按CFF规范附录C保存Expert字符集的SID顺序
var cffExpertCharset = [...]uint16{
	0, 1, 229, 230, 231, 232, 233, 234, 235, 236, 237, 238, 13, 14, 15, 99,
	239, 240, 241, 242, 243, 244, 245, 246, 247, 248, 27, 28, 249, 250, 251, 252,
	253, 254, 255, 256, 257, 258, 259, 260, 261, 262, 263, 264, 265, 266, 109, 110,
	267, 268, 269, 270, 271, 272, 273, 274, 275, 276, 277, 278, 279, 280, 281, 282,
	283, 284, 285, 286, 287, 288, 289, 290, 291, 292, 293, 294, 295, 296, 297, 298,
	299, 300, 301, 302, 303, 304, 305, 306, 307, 308, 309, 310, 311, 312, 313, 314,
	315, 316, 317, 318, 158, 155, 163, 319, 320, 321, 322, 323, 324, 325, 326, 150,
	164, 169, 327, 328, 329, 330, 331, 332, 333, 334, 335, 336, 337, 338, 339, 340,
	341, 342, 343, 344, 345, 346, 347, 348, 349, 350, 351, 352, 353, 354, 355, 356,
	357, 358, 359, 360, 361, 362, 363, 364, 365, 366, 367, 368, 369, 370, 371, 372,
	373, 374, 375, 376, 377, 378,
}

// cffExpertSubsetCharset 按CFF规范附录C保存ExpertSubset字符集的SID顺序
var cffExpertSubsetCharset = [...]uint16{
	0, 1, 231, 232, 235, 236, 237, 238, 13, 14, 15, 99, 239, 240, 241, 242,
	243, 244, 245, 246, 247, 248, 27, 28, 249, 250, 251, 253, 254, 255, 256, 257,
	258, 259, 260, 261, 262, 263, 264, 265, 266, 109, 110, 267, 268, 269, 270, 272,
	300, 301, 302, 305, 314, 315, 158, 155, 163, 320, 321, 322, 323, 324, 325, 326,
	150, 164, 169, 327, 328, 329, 330, 331, 332, 333, 334, 335, 336, 337, 338, 339,
	340, 341, 342, 343, 344, 345, 346,
}

// cffExpertEncoding 按CFF规范附录B保存ExpertEncoding的字符码到SID映射
var cffExpertEncoding = [256]uint16{
	32: 1, 33: 229, 34: 230, 36: 231, 37: 232, 38: 233, 39: 234,
	40: 235, 41: 236, 42: 237, 43: 238, 44: 13, 45: 14, 46: 15, 47: 99,
	48: 239, 49: 240, 50: 241, 51: 242, 52: 243, 53: 244, 54: 245, 55: 246,
	56: 247, 57: 248, 58: 27, 59: 28, 60: 249, 61: 250, 62: 251, 63: 252,
	65: 253, 66: 254, 67: 255, 68: 256, 69: 257, 73: 258, 76: 259, 77: 260,
	78: 261, 79: 262, 82: 263, 83: 264, 84: 265, 86: 266, 87: 109, 88: 110,
	89: 267, 90: 268, 91: 269, 93: 270, 94: 271, 95: 272, 96: 273,
	97: 274, 98: 275, 99: 276, 100: 277, 101: 278, 102: 279, 103: 280,
	104: 281, 105: 282, 106: 283, 107: 284, 108: 285, 109: 286, 110: 287,
	111: 288, 112: 289, 113: 290, 114: 291, 115: 292, 116: 293, 117: 294,
	118: 295, 119: 296, 120: 297, 121: 298, 122: 299, 123: 300, 124: 301,
	125: 302, 126: 303, 161: 304, 162: 305, 163: 306, 166: 307, 167: 308,
	168: 309, 169: 310, 170: 311, 172: 312, 175: 313, 178: 314, 179: 315,
	182: 316, 183: 317, 184: 318, 188: 158, 189: 155, 190: 163, 191: 319,
	192: 320, 193: 321, 194: 322, 195: 323, 196: 324, 197: 325, 200: 326,
	201: 150, 202: 164, 203: 169, 204: 327, 205: 328, 206: 329, 207: 330,
	208: 331, 209: 332, 210: 333, 211: 334, 212: 335, 213: 336, 214: 337,
	215: 338, 216: 339, 217: 340, 218: 341, 219: 342, 220: 343, 221: 344,
	222: 345, 223: 346, 224: 347, 225: 348, 226: 349, 227: 350, 228: 351,
	229: 352, 230: 353, 231: 354, 232: 355, 233: 356, 234: 357, 235: 358,
	236: 359, 237: 360, 238: 361, 239: 362, 240: 363, 241: 364, 242: 365,
	243: 366, 244: 367, 245: 368, 246: 369, 247: 370, 248: 371, 249: 372,
	250: 373, 251: 374, 252: 375, 253: 376, 254: 377, 255: 378,
}

// cffStandardNames 按CFF标准SID顺序保存字形名称
var cffStandardNames = strings.Fields(`
.notdef space exclam quotedbl numbersign dollar percent ampersand quoteright parenleft parenright asterisk
plus comma hyphen period slash zero one two three four five six
seven eight nine colon semicolon less equal greater question at A B
C D E F G H I J K L M N
O P Q R S T U V W X Y Z
bracketleft backslash bracketright asciicircum underscore quoteleft a b c d e f
g h i j k l m n o p q r
s t u v w x y z braceleft bar braceright asciitilde
exclamdown cent sterling fraction yen florin section currency quotesingle quotedblleft guillemotleft guilsinglleft
guilsinglright fi fl endash dagger daggerdbl periodcentered paragraph bullet quotesinglbase quotedblbase quotedblright
guillemotright ellipsis perthousand questiondown grave acute circumflex tilde macron breve dotaccent dieresis
ring cedilla hungarumlaut ogonek caron emdash AE ordfeminine Lslash Oslash OE ordmasculine
ae dotlessi lslash oslash oe germandbls onesuperior logicalnot mu trademark Eth onehalf
plusminus Thorn onequarter divide brokenbar degree thorn threequarters twosuperior registered minus eth
multiply threesuperior copyright Aacute Acircumflex Adieresis Agrave Aring Atilde Ccedilla Eacute Ecircumflex
Edieresis Egrave Iacute Icircumflex Idieresis Igrave Ntilde Oacute Ocircumflex Odieresis Ograve Otilde
Scaron Uacute Ucircumflex Udieresis Ugrave Yacute Ydieresis Zcaron aacute acircumflex adieresis agrave
aring atilde ccedilla eacute ecircumflex edieresis egrave iacute icircumflex idieresis igrave ntilde
oacute ocircumflex odieresis ograve otilde scaron uacute ucircumflex udieresis ugrave yacute ydieresis
zcaron exclamsmall Hungarumlautsmall dollaroldstyle dollarsuperior ampersandsmall Acutesmall parenleftsuperior parenrightsuperior twodotenleader onedotenleader zerooldstyle
oneoldstyle twooldstyle threeoldstyle fouroldstyle fiveoldstyle sixoldstyle sevenoldstyle eightoldstyle nineoldstyle commasuperior threequartersemdash periodsuperior
questionsmall asuperior bsuperior centsuperior dsuperior esuperior isuperior lsuperior msuperior nsuperior osuperior rsuperior
ssuperior tsuperior ff ffi ffl parenleftinferior parenrightinferior Circumflexsmall hyphensuperior Gravesmall Asmall Bsmall
Csmall Dsmall Esmall Fsmall Gsmall Hsmall Ismall Jsmall Ksmall Lsmall Msmall Nsmall
Osmall Psmall Qsmall Rsmall Ssmall Tsmall Usmall Vsmall Wsmall Xsmall Ysmall Zsmall
colonmonetary onefitted rupiah Tildesmall exclamdownsmall centoldstyle Lslashsmall Scaronsmall Zcaronsmall Dieresissmall Brevesmall Caronsmall
Dotaccentsmall Macronsmall figuredash hypheninferior Ogoneksmall Ringsmall Cedillasmall questiondownsmall oneeighth threeeighths fiveeighths seveneighths
onethird twothirds zerosuperior foursuperior fivesuperior sixsuperior sevensuperior eightsuperior ninesuperior zeroinferior oneinferior twoinferior
threeinferior fourinferior fiveinferior sixinferior seveninferior eightinferior nineinferior centinferior dollarinferior periodinferior commainferior Agravesmall
Aacutesmall Acircumflexsmall Atildesmall Adieresissmall Aringsmall AEsmall Ccedillasmall Egravesmall Eacutesmall Ecircumflexsmall Edieresissmall Igravesmall
Iacutesmall Icircumflexsmall Idieresissmall Ethsmall Ntildesmall Ogravesmall Oacutesmall Ocircumflexsmall Otildesmall Odieresissmall OEsmall Oslashsmall
Ugravesmall Uacutesmall Ucircumflexsmall Udieresissmall Yacutesmall Thornsmall Ydieresissmall 001.000 001.001 001.002 001.003 Black
Bold Book Light Medium Regular Roman Semibold
`)
