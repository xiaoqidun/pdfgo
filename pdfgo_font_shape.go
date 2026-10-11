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

import "context"

// TextShaper 使用源字体整形单行文字，包括脚本分段和双向排列，不替换字体或修改源数据
// 字形编号须对应Font.Program；rightToLeft指定段落方向，断行后保持该方向
// 入参: ctx 取消上下文, font 源字体, text 单行原文, rightToLeft 段落是否从右向左
// 返回: []ShapedGlyph 逻辑簇顺序的定位字形, error 整形或取消错误
type TextShaper func(ctx context.Context, font *Font, text string, rightToLeft bool) ([]ShapedGlyph, error)

// ShapedGlyph 保存整形后的字形编号、原文簇及千分之一字号的定位与推进量
// Cluster为原文的符文索引，VisualOrder为簇从左到右的零起始连续序号，同簇字形共用序号
// X和Y相对行起点及基线，横轴向右、纵轴向上，Advance为横向推进量
// Width为字体程序的横向字宽，不含整形调整，同一字形须保持一致
type ShapedGlyph struct {
	ID            uint16
	Cluster       int
	VisualOrder   int
	X, Y, Advance float64
	Width         float64
}
