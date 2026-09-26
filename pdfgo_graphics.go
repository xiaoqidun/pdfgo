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
	"context"
	"fmt"
	"math"
)

// Matrix 表示PDF二维仿射矩阵，使用列向量坐标约定
type Matrix [6]float64

// Point 表示用户空间坐标
type Point struct{ X, Y float64 }

// Identity 返回单位矩阵
// 返回: Matrix 单位矩阵
func Identity() Matrix { return Matrix{1, 0, 0, 1, 0, 0} }

// Mul 按当前矩阵乘以右侧矩阵进行坐标组合
// 入参: n 右侧矩阵
// 返回: Matrix 组合矩阵
func (m Matrix) Mul(n Matrix) Matrix {
	return Matrix{
		m[0]*n[0] + m[2]*n[1], m[1]*n[0] + m[3]*n[1],
		m[0]*n[2] + m[2]*n[3], m[1]*n[2] + m[3]*n[3],
		m[0]*n[4] + m[2]*n[5] + m[4], m[1]*n[4] + m[3]*n[5] + m[5],
	}
}

// Apply 将点变换到目标坐标空间
// 入参: p 原始坐标点
// 返回: Point 变换后的坐标点
func (m Matrix) Apply(p Point) Point {
	return Point{m[0]*p.X + m[2]*p.Y + m[4], m[1]*p.X + m[3]*p.Y + m[5]}
}

// Inverse 返回可逆仿射矩阵的逆变换
// 返回: Matrix 逆矩阵, bool 是否存在有限逆矩阵
func (m Matrix) Inverse() (Matrix, bool) {
	d := m[0]*m[3] - m[1]*m[2]
	if d == 0 {
		return Matrix{}, false
	}
	n := Matrix{m[3] / d, -m[1] / d, -m[2] / d, m[0] / d, (m[2]*m[5] - m[3]*m[4]) / d, (m[1]*m[4] - m[0]*m[5]) / d}
	for _, v := range n {
		if math.IsNaN(v) || math.IsInf(v, 0) {
			return Matrix{}, false
		}
	}
	return n, true
}

// Segment 保存直线、三次曲线或闭合路径，坐标已变换到页面用户空间
type Segment struct {
	Operator string
	Points   []Point
}

// Path 保存路径及填充规则
type Path struct {
	Segments []Segment
	EvenOdd  bool
	Text     []*TextClip
}

// TextClip 保存参与裁剪的字形及页面定位
type TextClip struct {
	Font                  *Font
	Glyphs                []Glyph
	Positions             []Point
	Matrix                Matrix
	Size, HorizontalScale float64
}

// Paint 保存颜色及不透明度，CMYK保留设备四色，Space与Values保留ICC源分量
type Paint struct {
	RGB    [3]float64
	CMYK   *[4]float64
	Space  *ColorSpace
	Values [4]float64
	Alpha  float64
	Axial  *AxialGradient
	Radial *RadialGradient
	Tiling *TilingPattern
}

// Style 保存绘制状态及按顺序相交的裁剪路径
type Style struct {
	Fill, Stroke    Paint
	LineWidth       float64
	Cap, Join       int
	MiterLimit      float64
	Dash            []float64
	DashPhase       float64
	Clips           []Path
	StrokeAdjust    bool
	FillOverprint   bool
	StrokeOverprint bool
	OverprintMode   int
	RenderingIntent Name
	BlendMode       Name
	AlphaIsShape    bool
	SoftMask        *SoftMask
	Smoothness      *float64
	Antialias       *bool
}

// PathMark 表示一次路径绘制，路径及裁剪坐标始终位于页面用户空间
// StrokeMatrix非零时保留非等比描边变换，线宽及虚线参数属于其逆变换后的坐标空间
type PathMark struct {
	Path         Path
	Style        Style
	Fill, Stroke bool
	StrokeMatrix Matrix
}

// TextMark 保存文字字形及其相对于文字矩阵的基线位置
// Positions始终使用字形横排原点，竖排位置已扣除竖排原点向量
// StrokeMatrix保留图形状态坐标变换，描边参数不受文字矩阵和字号影响
type TextMark struct {
	Font                  *Font
	Glyphs                []Glyph
	Positions             []Point
	Matrix                Matrix
	StrokeMatrix          Matrix
	Size, HorizontalScale float64
	Style                 Style
	Mode                  int
	Clip                  *TextClip
}

// ImageMark 保存图像资源及单位方形到页面坐标的变换
type ImageMark struct {
	Image  *Image
	Matrix Matrix
	Style  Style
}

// GroupMark 保存透明度组的边界不透明度和隔离方式，组内绘制使用独立状态
type GroupMark struct {
	Alpha        float64
	AlphaIsShape bool
	Isolated     bool
	BlendMode    Name
	SoftMask     *SoftMask
	ColorSpace   *ColorSpace
}

// MarkedContentMark 保存内容标记及其属性，结束标记沿用开始标记的标签
type MarkedContentMark struct {
	Tag        Name
	Properties Dictionary
	Operator   string
}

// Visitor 按内容顺序接收页面绘制对象，缺少对应绘制回调时返回错误
// Warning非空时报告空Type3字形、缺失的ExtGState资源及未保留的内容语义，其他解析错误仍返回错误
type Visitor struct {
	Path          func(PathMark) error
	Text          func(TextMark) error
	Image         func(ImageMark) error
	Group         func(GroupMark, func(Visitor) error) error
	MarkedContent func(MarkedContentMark) error
	Warning       func(Diagnostic)
}

// graphicsState 保存图形和文字操作的当前状态
type graphicsState struct {
	matrix                                                Matrix
	style                                                 Style
	font                                                  *Font
	fontSize, spacing, wordSpacing, hscale, leading, rise float64
	mode                                                  int
	fillSpace, strokeSpace                                Name
	fillPatternBase, strokePatternBase                    Name
	fillICC, strokeICC                                    *iccColorSpace
	fillSeparation, strokeSeparation                      *separationSpace
}

// pageInterpreter 按内容顺序解释页面或表单
type pageInterpreter struct {
	reader                 *Reader
	resources              Dictionary
	visitor                Visitor
	ctx                    context.Context
	state                  graphicsState
	stack                  []graphicsState
	marked                 []Name
	textClips              []*TextClip
	textMatrix, lineMatrix Matrix
	inText                 bool
	path                   Path
	current, start         Point
	hasPoint               bool
	pendingClip            bool
	clipEvenOdd            bool
	depth                  int
	compatibility          int
	opaqueGroup            bool
	patternMatrix          Matrix
	bounds                 Rectangle
	uncoloredPattern       bool
	type3                  bool
}

// WalkPage 解释页面内容并按绘制顺序访问可准确表达的图元，注解由Page.Annotations读取
// 入参: ctx 取消上下文, page 页面, visitor 图元访问器
// 返回: error 错误信息
func (r *Reader) WalkPage(ctx context.Context, page *Page, visitor Visitor) error {
	if page.reader != r {
		return fmt.Errorf("page belongs to another reader")
	}
	var groupSpace *ColorSpace
	if page.Dictionary["Group"] != nil {
		value, err := r.Resolve(page.Dictionary["Group"])
		if err != nil {
			return err
		}
		group, ok := value.(Dictionary)
		if !ok {
			return fmt.Errorf("invalid page group")
		}
		for key, value := range group {
			switch key {
			case "Type":
				if value != Name("Group") {
					return &UnsupportedError{Feature: "page group type"}
				}
			case "S":
				if value != Name("Transparency") {
					return &UnsupportedError{Feature: "page group subtype"}
				}
			case "CS":
				groupSpace, err = r.readBlendingSpace(value)
				if err != nil {
					return err
				}
				if visitor.Group == nil && !groupSpace.SRGBEquivalent() {
					return &UnsupportedError{Feature: "page blending space visitor missing"}
				}
			case "I":
				if _, ok := value.(Boolean); !ok {
					return fmt.Errorf("invalid page isolation flag")
				}
			case "K":
				if value != Boolean(false) {
					return &UnsupportedError{Feature: "knockout page group"}
				}
			default:
				return &UnsupportedError{Feature: fmt.Sprintf("page group field %q", key)}
			}
		}
	}
	if page.Dictionary["Trans"] != nil {
		return &UnsupportedError{Feature: `page field "Trans"`}
	}
	data, err := page.Content()
	if err != nil {
		return err
	}
	interpreter := pageInterpreter{reader: r, resources: page.Resources, visitor: visitor, ctx: ctx, bounds: page.CropBox}
	interpreter.patternMatrix = Identity()
	interpreter.state = graphicsState{matrix: Identity(), hscale: 1, fillSpace: "DeviceGray", strokeSpace: "DeviceGray", style: Style{Fill: Paint{Alpha: 1}, Stroke: Paint{Alpha: 1}, LineWidth: 1, MiterLimit: 10}}
	if groupSpace != nil && visitor.Group != nil {
		return visitor.Group(GroupMark{Alpha: 1, Isolated: true, ColorSpace: groupSpace}, func(v Visitor) error {
			child := interpreter
			child.visitor = v
			return child.run(data)
		})
	}
	return interpreter.run(data)
}

// WalkType3Glyph 按文字位置解释Type3字形内容流并访问其中的图元
// 入参: ctx 取消上下文, mark 文字绘制信息, index 字形下标, visitor 图元访问器
// 返回: error 解析或访问错误
func (r *Reader) WalkType3Glyph(ctx context.Context, mark TextMark, index int, visitor Visitor) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	font := mark.Font
	if font == nil || font.Subtype != Name("Type3") || index < 0 || index >= len(mark.Glyphs) || index >= len(mark.Positions) {
		return fmt.Errorf("invalid Type3 glyph")
	}
	object, err := r.Resolve(font.type3Procs[Name(mark.Glyphs[index].Name)])
	if err != nil {
		return err
	}
	stream, ok := object.(*Stream)
	if !ok {
		return fmt.Errorf("missing Type3 character procedure")
	}
	if len(stream.Data) == 0 {
		message := fmt.Sprintf("empty Type3 character procedure %q", mark.Glyphs[index].Name)
		if visitor.Warning == nil {
			return fmt.Errorf("%s", message)
		}
		visitor.Warning(Diagnostic{Message: message})
		return nil
	}
	data, err := stream.Decode()
	if err != nil {
		return err
	}
	position := mark.Positions[index]
	text := Matrix{mark.Size * mark.HorizontalScale, 0, 0, mark.Size, position.X, position.Y}
	interpreter := pageInterpreter{reader: r, resources: font.type3Resources, visitor: visitor, ctx: ctx, type3: true}
	interpreter.patternMatrix = Identity()
	interpreter.state = graphicsState{matrix: mark.Matrix.Mul(text).Mul(font.type3Matrix), hscale: 1, fillSpace: "DeviceGray", strokeSpace: "DeviceGray", style: mark.Style}
	return interpreter.run(data)
}

// run 解释单个页面或表单的完整内容
// 入参: data 解码后的内容流
// 返回: error 错误信息
func (p *pageInterpreter) run(data []byte) error {
	err := WalkOperations(p.ctx, data, p.operation)
	if err != nil {
		return err
	}
	if len(p.stack) != 0 || p.inText || len(p.marked) != 0 {
		return fmt.Errorf("unbalanced graphics, text or marked content state")
	}
	if p.compatibility != 0 {
		return fmt.Errorf("unbalanced compatibility section")
	}
	return nil
}

// markedContent 解析内容标记并向调用方交付其语义属性
// 入参: op 内容标记操作
// 返回: error 错误信息
func (p *pageInterpreter) markedContent(op Operation) error {
	mark := MarkedContentMark{Operator: op.Operator}
	if op.Operator == "EMC" {
		if len(op.Operands) != 0 || len(p.marked) == 0 {
			return fmt.Errorf("unmatched marked content end")
		}
		mark.Tag = p.marked[len(p.marked)-1]
		p.marked = p.marked[:len(p.marked)-1]
	} else {
		count := 1
		if op.Operator == "BDC" || op.Operator == "DP" {
			count = 2
		}
		if len(op.Operands) != count {
			return fmt.Errorf("invalid marked content operands")
		}
		var ok bool
		mark.Tag, ok = op.Operands[0].(Name)
		if !ok {
			return fmt.Errorf("invalid marked content tag")
		}
		if count == 2 {
			object := op.Operands[1]
			if name, ok := object.(Name); ok {
				var err error
				object, err = p.resource("Properties", name)
				if err != nil {
					return err
				}
			}
			value, err := p.reader.Resolve(object)
			if err != nil {
				return err
			}
			mark.Properties, ok = value.(Dictionary)
			if !ok {
				return fmt.Errorf("invalid marked content properties")
			}
		}
		if op.Operator == "BMC" || op.Operator == "BDC" {
			p.marked = append(p.marked, mark.Tag)
		}
	}
	if p.visitor.MarkedContent != nil {
		return p.visitor.MarkedContent(mark)
	}
	if op.Operator != "EMC" {
		if p.visitor.Warning == nil {
			return &UnsupportedError{Feature: "marked content requiring semantic preservation"}
		}
		p.visitor.Warning(Diagnostic{Offset: op.Offset, Message: fmt.Sprintf("marked content %s not preserved", mark.Tag)})
	}
	return nil
}

// numbers 检查操作数数量并读取有限数值
// 入参: operands 操作数, count 预期数量
// 返回: []float64 数值列表, error 错误信息
func numbers(operands []Object, count int) ([]float64, error) {
	if len(operands) != count {
		return nil, fmt.Errorf("expected %d operands, got %d", count, len(operands))
	}
	values := make([]float64, count)
	for n, v := range operands {
		switch v := v.(type) {
		case Integer:
			values[n] = float64(v)
		case Real:
			values[n] = float64(v)
		default:
			return nil, fmt.Errorf("nonnumeric operand")
		}
	}
	return values, nil
}

// operation 执行内容操作，未知可见操作返回明确错误
// 入参: op 内容操作
// 返回: error 错误信息
func (p *pageInterpreter) operation(op Operation) error {
	a := op.Operands
	if p.uncoloredPattern {
		switch op.Operator {
		case "g", "G", "rg", "RG", "k", "K", "cs", "CS", "sc", "SC", "scn", "SCN", "sh":
			return fmt.Errorf("color operator in uncolored pattern")
		}
	}
	var v []float64
	if n := graphicsOperandCount(op.Operator); n >= 0 {
		var err error
		v, err = numbers(a, n)
		if err != nil {
			return err
		}
	}
	point := func(x, y float64) Point { return p.state.matrix.Apply(Point{x, y}) }
	add := func(name string, points ...Point) { p.path.Segments = append(p.path.Segments, Segment{name, points}) }
	switch op.Operator {
	case "BX":
		p.compatibility++
	case "EX":
		if p.compatibility == 0 {
			return fmt.Errorf("unmatched operator %q", "EX")
		}
		p.compatibility--
	case "d0", "d1":
		if !p.type3 {
			return &UnsupportedError{Feature: "Type3 glyph metrics outside character procedure"}
		}
	case "q":
		if len(p.stack) >= 256 {
			return fmt.Errorf("graphics stack limit exceeded")
		}
		p.stack = append(p.stack, p.state)
	case "Q":
		if len(p.stack) == 0 {
			return fmt.Errorf("unmatched operator %q", "Q")
		}
		p.state = p.stack[len(p.stack)-1]
		p.stack = p.stack[:len(p.stack)-1]
	case "cm":
		p.state.matrix = p.state.matrix.Mul(Matrix(v))
	case "w":
		if v[0] < 0 {
			return fmt.Errorf("negative line width")
		}
		p.state.style.LineWidth = v[0]
	case "J", "j":
		if v[0] < 0 || v[0] > 2 || v[0] != math.Trunc(v[0]) {
			return fmt.Errorf("invalid line style")
		}
		if op.Operator == "J" {
			p.state.style.Cap = int(v[0])
		} else {
			p.state.style.Join = int(v[0])
		}
	case "M":
		if v[0] < 1 {
			return fmt.Errorf("invalid miter limit")
		}
		p.state.style.MiterLimit = v[0]
	case "d":
		if len(a) != 2 {
			return fmt.Errorf("invalid dash operands")
		}
		values, ok := a[0].(Array)
		if !ok {
			return fmt.Errorf("invalid dash pattern")
		}
		dash, err := numbers(values, len(values))
		if err != nil {
			return err
		}
		sum := 0.0
		for _, n := range dash {
			if n < 0 {
				return fmt.Errorf("negative dash")
			}
			sum += n
		}
		if len(dash) > 0 && sum == 0 {
			return fmt.Errorf("empty dash cycle")
		}
		phase, err := numbers(a[1:], 1)
		if err != nil {
			return err
		}
		p.state.style.Dash = dash
		p.state.style.DashPhase = phase[0]
	case "m":
		p.current = point(v[0], v[1])
		p.start = p.current
		p.hasPoint = true
		add("M", p.current)
	case "l":
		if !p.hasPoint {
			return fmt.Errorf("line without current point")
		}
		p.current = point(v[0], v[1])
		add("L", p.current)
	case "c", "v", "y":
		if !p.hasPoint {
			return fmt.Errorf("curve without current point")
		}
		var c1, c2, end Point
		if op.Operator == "c" {
			c1 = point(v[0], v[1])
			c2 = point(v[2], v[3])
			end = point(v[4], v[5])
		}
		if op.Operator == "v" {
			c1 = p.current
			c2 = point(v[0], v[1])
			end = point(v[2], v[3])
		}
		if op.Operator == "y" {
			c1 = point(v[0], v[1])
			end = point(v[2], v[3])
			c2 = end
		}
		add("B", c1, c2, end)
		p.current = end
	case "h":
		if !p.hasPoint {
			return fmt.Errorf("close without current point")
		}
		add("C")
		p.current = p.start
	case "re":
		p.start = point(v[0], v[1])
		p.current = p.start
		p.hasPoint = true
		add("M", p.start)
		add("L", point(v[0]+v[2], v[1]))
		add("L", point(v[0]+v[2], v[1]+v[3]))
		add("L", point(v[0], v[1]+v[3]))
		add("C")
	case "W", "W*":
		p.pendingClip = true
		p.clipEvenOdd = op.Operator == "W*"
	case "S", "s", "f", "F", "f*", "B", "B*", "b", "b*", "n":
		if op.Operator == "s" || op.Operator == "b" || op.Operator == "b*" {
			add("C")
		}
		fill := op.Operator == "f" || op.Operator == "F" || op.Operator == "f*" || op.Operator == "B" || op.Operator == "B*" || op.Operator == "b" || op.Operator == "b*"
		stroke := op.Operator == "S" || op.Operator == "s" || op.Operator == "B" || op.Operator == "B*" || op.Operator == "b" || op.Operator == "b*"
		p.path.EvenOdd = op.Operator == "f*" || op.Operator == "B*" || op.Operator == "b*"
		if (fill || stroke) && len(p.path.Segments) > 0 {
			if err := p.validatePaint(fill, stroke); err != nil {
				return err
			}
			style := p.state.style
			var strokeMatrix Matrix
			if stroke {
				m := p.state.matrix
				sx, sy := math.Hypot(m[0], m[1]), math.Hypot(m[2], m[3])
				if math.Abs(sx-sy) > 1e-8*math.Max(1, sx) || math.Abs(m[0]*m[2]+m[1]*m[3]) > 1e-8*math.Max(1, sx*sy) {
					strokeMatrix = Matrix{m[0], m[1], m[2], m[3], 0, 0}
					if _, ok := strokeMatrix.Inverse(); !ok {
						return &UnsupportedError{Feature: "singular path stroke"}
					}
				} else {
					style.LineWidth *= sx
					style.Dash = append([]float64(nil), style.Dash...)
					for n := range style.Dash {
						style.Dash[n] *= sx
					}
					style.DashPhase *= sx
				}
			}
			if p.visitor.Path == nil {
				return fmt.Errorf("path visitor missing")
			}
			if err := p.visitor.Path(PathMark{Path: p.path, Style: style, Fill: fill, Stroke: stroke, StrokeMatrix: strokeMatrix}); err != nil {
				return err
			}
		}
		if p.pendingClip {
			clip := p.path
			clip.EvenOdd = p.clipEvenOdd
			p.state.style.Clips = append(append([]Path(nil), p.state.style.Clips...), clip)
		}
		p.path = Path{}
		p.hasPoint = false
		p.pendingClip = false
	case "g", "G", "rg", "RG", "k", "K":
		rgb := [3]float64{}
		var cmyk *[4]float64
		for n := range v {
			v[n] = math.Max(0, math.Min(1, v[n]))
		}
		if len(v) == 1 {
			rgb = [3]float64{v[0], v[0], v[0]}
		} else if len(v) == 3 {
			copy(rgb[:], v)
		} else {
			cmyk = &[4]float64{v[0], v[1], v[2], v[3]}
		}
		if op.Operator == "G" || op.Operator == "RG" || op.Operator == "K" {
			p.state.strokeICC = nil
			p.state.strokeSeparation = nil
			p.state.style.Stroke.RGB = rgb
			p.state.style.Stroke.CMYK = cmyk
			p.state.style.Stroke.Space, p.state.style.Stroke.Values = nil, [4]float64{}
			p.state.style.Stroke.Axial = nil
			p.state.style.Stroke.Radial = nil
			p.state.style.Stroke.Tiling = nil
			p.state.strokePatternBase = ""
			if len(v) == 1 {
				p.state.strokeSpace = "DeviceGray"
			} else if cmyk != nil {
				p.state.strokeSpace = "DeviceCMYK"
			} else {
				p.state.strokeSpace = "DeviceRGB"
			}
		} else {
			p.state.style.Fill.RGB = rgb
			p.state.fillICC = nil
			p.state.fillSeparation = nil
			p.state.style.Fill.CMYK = cmyk
			p.state.style.Fill.Space, p.state.style.Fill.Values = nil, [4]float64{}
			p.state.style.Fill.Axial = nil
			p.state.style.Fill.Radial = nil
			p.state.style.Fill.Tiling = nil
			p.state.fillPatternBase = ""
			if len(v) == 1 {
				p.state.fillSpace = "DeviceGray"
			} else if cmyk != nil {
				p.state.fillSpace = "DeviceCMYK"
			} else {
				p.state.fillSpace = "DeviceRGB"
			}
		}
	case "cs", "CS":
		if len(a) != 1 {
			return fmt.Errorf("invalid color space")
		}
		name, ok := a[0].(Name)
		if !ok {
			return fmt.Errorf("invalid color space name")
		}
		var profile *iccColorSpace
		var separation *separationSpace
		var patternBase Name
		if name != "DeviceRGB" && name != "DeviceGray" && name != "DeviceCMYK" && name != "Pattern" {
			object, err := p.resource("ColorSpace", name)
			if err != nil {
				return err
			}
			object, err = p.reader.Resolve(object)
			if err != nil {
				return err
			}
			space, ok := object.(Array)
			if !ok || len(space) == 0 {
				return &UnsupportedError{Feature: "non-device color space"}
			}
			if len(space) == 1 && space[0] == Name("Pattern") {
				name = "Pattern"
			} else if len(space) == 2 && space[0] == Name("Pattern") {
				patternBase, ok = space[1].(Name)
				if !ok || patternBase != "DeviceGray" && patternBase != "DeviceRGB" && patternBase != "DeviceCMYK" {
					return &UnsupportedError{Feature: "uncolored pattern base color space"}
				}
				name = "Pattern"
			} else if space[0] == Name("Separation") {
				separation, err = p.reader.readSeparation(space)
				if err != nil {
					return err
				}
				name = "Separation"
			} else {
				profile, err = p.reader.readICCColorSpace(space)
				if err != nil {
					return err
				}
				name = "ICCBased"
			}
		}
		if op.Operator == "cs" {
			p.state.fillSpace = name
			p.state.fillPatternBase = patternBase
			p.state.fillICC = profile
			p.state.fillSeparation = separation
			p.state.style.Fill.RGB = [3]float64{}
			p.state.style.Fill.CMYK = nil
			p.state.style.Fill.Space, p.state.style.Fill.Values = nil, [4]float64{}
			p.state.style.Fill.Axial = nil
			p.state.style.Fill.Radial = nil
			p.state.style.Fill.Tiling = nil
			if name == "DeviceCMYK" {
				p.state.style.Fill.CMYK = &[4]float64{0, 0, 0, 1}
			}
		} else {
			p.state.strokeSpace = name
			p.state.strokePatternBase = patternBase
			p.state.strokeICC = profile
			p.state.strokeSeparation = separation
			p.state.style.Stroke.RGB = [3]float64{}
			p.state.style.Stroke.CMYK = nil
			p.state.style.Stroke.Space, p.state.style.Stroke.Values = nil, [4]float64{}
			p.state.style.Stroke.Axial = nil
			p.state.style.Stroke.Radial = nil
			p.state.style.Stroke.Tiling = nil
			if name == "DeviceCMYK" {
				p.state.style.Stroke.CMYK = &[4]float64{0, 0, 0, 1}
			}
		}
		if profile != nil {
			paint, err := profile.paint(make([]float64, profile.components()), p.state.style.RenderingIntent)
			if err != nil {
				return err
			}
			if op.Operator == "cs" {
				paint.Alpha = p.state.style.Fill.Alpha
				p.state.style.Fill = paint
			} else {
				paint.Alpha = p.state.style.Stroke.Alpha
				p.state.style.Stroke = paint
			}
		}
	case "sc", "scn", "SC", "SCN":
		space := p.state.fillSpace
		patternBase := p.state.fillPatternBase
		profile := p.state.fillICC
		separation := p.state.fillSeparation
		operator := "g"
		if op.Operator == "SC" || op.Operator == "SCN" {
			space = p.state.strokeSpace
			patternBase = p.state.strokePatternBase
			profile = p.state.strokeICC
			separation = p.state.strokeSeparation
			operator = "G"
		}
		if space == "Separation" {
			values, err := numbers(a, 1)
			if err != nil {
				return err
			}
			paint, err := separation.paint(values[0], p.state.style.RenderingIntent)
			if err != nil {
				return err
			}
			if operator == "g" {
				if separation.name != "None" {
					paint.Alpha = p.state.style.Fill.Alpha
				}
				p.state.style.Fill = paint
			} else {
				if separation.name != "None" {
					paint.Alpha = p.state.style.Stroke.Alpha
				}
				p.state.style.Stroke = paint
			}
			return nil
		}
		if space == "ICCBased" {
			values, err := numbers(a, profile.components())
			if err != nil {
				return err
			}
			paint, err := profile.paint(values, p.state.style.RenderingIntent)
			if err != nil {
				return err
			}
			if operator == "g" {
				paint.Alpha = p.state.style.Fill.Alpha
				p.state.style.Fill = paint
			} else {
				paint.Alpha = p.state.style.Stroke.Alpha
				p.state.style.Stroke = paint
			}
			return nil
		}
		if space == "Pattern" {
			if len(a) == 0 {
				return fmt.Errorf("missing pattern name")
			}
			name, ok := a[len(a)-1].(Name)
			if !ok {
				return fmt.Errorf("invalid pattern name")
			}
			paint := p.state.style.Fill
			if operator == "G" {
				paint = p.state.style.Stroke
			}
			if patternBase != "" {
				if err := patternBaseColor(&paint, patternBase, a[:len(a)-1]); err != nil {
					return err
				}
			} else if len(a) != 1 {
				return fmt.Errorf("invalid colored pattern operands")
			}
			pattern, err := p.tilingPattern(name)
			if err != nil {
				return err
			}
			if pattern != nil {
				if pattern.PaintType == 2 && patternBase == "" || pattern.PaintType == 1 && patternBase != "" {
					return fmt.Errorf("pattern paint type does not match color space")
				}
				paint.Tiling, paint.Axial, paint.Radial = pattern, nil, nil
				if operator == "g" {
					p.state.style.Fill = paint
				} else {
					p.state.style.Stroke = paint
				}
				return nil
			}
			if patternBase != "" {
				return fmt.Errorf("shading pattern cannot use a base color space")
			}
			gradient, err := p.shadingPattern(name)
			if err != nil {
				return err
			}
			if operator == "g" {
				p.state.style.Fill.Axial, p.state.style.Fill.Radial = gradient.Axial, gradient.Radial
				p.state.style.Fill.Tiling = nil
			} else {
				p.state.style.Stroke.Axial, p.state.style.Stroke.Radial = gradient.Axial, gradient.Radial
				p.state.style.Stroke.Tiling = nil
			}
			return nil
		}
		if space == "DeviceRGB" {
			if operator == "g" {
				operator = "rg"
			} else {
				operator = "RG"
			}
		}
		if space == "DeviceCMYK" {
			if operator == "g" {
				operator = "k"
			} else {
				operator = "K"
			}
		}
		return p.operation(Operation{Operator: operator, Operands: a})
	case "BT":
		if p.inText {
			return fmt.Errorf("nested text object")
		}
		p.inText = true
		p.textClips = nil
		p.textMatrix = Identity()
		p.lineMatrix = Identity()
	case "ET":
		if !p.inText {
			return fmt.Errorf("unmatched operator %q", "ET")
		}
		p.inText = false
		if len(p.textClips) != 0 {
			p.state.style.Clips = append(append([]Path(nil), p.state.style.Clips...), Path{Text: p.textClips})
			p.textClips = nil
		}
	case "Tf":
		if len(a) != 2 {
			return fmt.Errorf("invalid font operands")
		}
		name, ok := a[0].(Name)
		if !ok {
			return fmt.Errorf("invalid font name")
		}
		size, err := numbers(a[1:], 1)
		if err != nil {
			return err
		}
		if size[0] <= 0 {
			return &UnsupportedError{Feature: "nonpositive text font size"}
		}
		object, err := p.resource("Font", name)
		if err != nil {
			return err
		}
		font, err := p.reader.ReadFont(object)
		if err != nil {
			return err
		}
		p.state.font = font
		p.state.fontSize = size[0]
	case "Tc":
		p.state.spacing = v[0]
	case "Tw":
		p.state.wordSpacing = v[0]
	case "Tz":
		if v[0] <= 0 {
			return &UnsupportedError{Feature: "nonpositive horizontal text scale"}
		}
		p.state.hscale = v[0] / 100
	case "TL":
		p.state.leading = v[0]
	case "Tr":
		if v[0] < 0 || v[0] > 7 || v[0] != math.Trunc(v[0]) {
			return fmt.Errorf("invalid text rendering mode")
		}
		p.state.mode = int(v[0])
	case "Ts":
		p.state.rise = v[0]
	case "Tm":
		p.textMatrix = Matrix(v)
		p.lineMatrix = p.textMatrix
	case "Td", "TD":
		if op.Operator == "TD" {
			p.state.leading = -v[1]
		}
		p.lineMatrix = p.lineMatrix.Mul(Matrix{1, 0, 0, 1, v[0], v[1]})
		p.textMatrix = p.lineMatrix
	case "T*":
		p.lineMatrix = p.lineMatrix.Mul(Matrix{1, 0, 0, 1, 0, -p.state.leading})
		p.textMatrix = p.lineMatrix
	case "'", "\"":
		if op.Operator == "\"" {
			if len(a) != 3 {
				return fmt.Errorf("invalid quoted text")
			}
			n, err := numbers(a[:2], 2)
			if err != nil {
				return err
			}
			p.state.wordSpacing = n[0]
			p.state.spacing = n[1]
			a = a[2:]
		}
		if err := p.operation(Operation{Operator: "T*"}); err != nil {
			return err
		}
		return p.operation(Operation{Operator: "Tj", Operands: a})
	case "Tj":
		if len(a) != 1 {
			return fmt.Errorf("invalid text operands")
		}
		text, ok := a[0].(String)
		if !ok {
			return fmt.Errorf("text operand is not a string")
		}
		return p.showText(text)
	case "TJ":
		if len(a) != 1 {
			return fmt.Errorf("invalid text array")
		}
		array, ok := a[0].(Array)
		if !ok {
			return fmt.Errorf("text operand is not an array")
		}
		for _, item := range array {
			if str, ok := item.(String); ok {
				if err := p.showText(str); err != nil {
					return err
				}
			} else {
				adjustment, err := numbers([]Object{item}, 1)
				if err != nil {
					return err
				}
				if !p.inText || p.state.font == nil {
					return fmt.Errorf("text without active font")
				}
				shift := Matrix{1, 0, 0, 1, 0, 0}
				if p.state.font.Vertical {
					shift[5] = -adjustment[0] / 1000 * p.state.fontSize
				} else {
					shift[4] = -adjustment[0] / 1000 * p.state.fontSize * p.state.hscale
				}
				p.textMatrix = p.textMatrix.Mul(shift)
			}
		}
	case "sh":
		return p.shadingFill(a)
	case "Do":
		return p.xobject(a)
	case "gs":
		return p.extState(a, op.Offset)
	case "ri":
		if len(a) != 1 {
			return fmt.Errorf("invalid rendering intent operands")
		}
		intent, ok := a[0].(Name)
		if !ok || intent != "RelativeColorimetric" && intent != "AbsoluteColorimetric" && intent != "Perceptual" && intent != "Saturation" {
			return fmt.Errorf("invalid rendering intent")
		}
		p.state.style.RenderingIntent = intent
	case "i":
		n, err := numbers(a, 1)
		if err != nil {
			return err
		}
		if n[0] < 0 || n[0] > 100 {
			return fmt.Errorf("invalid flatness")
		}
	case "BMC", "BDC", "EMC", "MP", "DP":
		return p.markedContent(op)
	default:
		if p.compatibility != 0 {
			return nil
		}
		return &UnsupportedError{Feature: "content operator " + op.Operator}
	}
	return nil
}

// graphicsOperandCount 获取固定数值操作符的参数数量，其他操作返回负数
// 入参: operator 操作符
// 返回: int 参数数量
func graphicsOperandCount(operator string) int {
	switch operator {
	case "q", "Q", "h", "S", "s", "f", "F", "f*", "B", "B*", "b", "b*", "n", "W", "W*", "BT", "ET", "T*", "BX", "EX":
		return 0
	case "w", "J", "j", "M", "g", "G", "Tc", "Tw", "Tz", "TL", "Tr", "Ts":
		return 1
	case "m", "l", "Td", "TD", "d0":
		return 2
	case "rg", "RG":
		return 3
	case "v", "y", "re", "k", "K":
		return 4
	case "cm", "c", "Tm", "d1":
		return 6
	default:
		return -1
	}
}

// showText 保留逐字定位并更新文字矩阵
// 入参: data 编码后的文字字节
// 返回: error 错误信息
func (p *pageInterpreter) showText(data []byte) error {
	if !p.inText || p.state.font == nil {
		return fmt.Errorf("text without active font")
	}
	paintMode := p.state.mode % 4
	if err := p.validatePaint(paintMode == 0 || paintMode == 2, paintMode == 1 || paintMode == 2); err != nil {
		return err
	}
	glyphs, err := p.state.font.Decode(data)
	if err != nil {
		return err
	}
	positions := make([]Point, len(glyphs))
	advance := Point{}
	for n, glyph := range glyphs {
		spacing := p.state.spacing
		if glyph.WordSpace {
			spacing += p.state.wordSpacing
		}
		if p.state.font.Vertical {
			positions[n] = Point{-glyph.Vertical.Origin.X / 1000 * p.state.fontSize * p.state.hscale, advance.Y - glyph.Vertical.Origin.Y/1000*p.state.fontSize + p.state.rise}
			advance.Y += glyph.Vertical.Advance/1000*p.state.fontSize + spacing
		} else {
			positions[n] = Point{advance.X, p.state.rise}
			advance.X += (glyph.Width/1000*p.state.fontSize + spacing) * p.state.hscale
		}
	}
	if len(glyphs) > 0 {
		if p.visitor.Text == nil {
			return fmt.Errorf("text visitor missing")
		}
		mark := TextMark{Font: p.state.font, Glyphs: glyphs, Positions: positions, Matrix: p.state.matrix.Mul(p.textMatrix), StrokeMatrix: p.state.matrix, Size: p.state.fontSize, HorizontalScale: p.state.hscale, Style: p.state.style, Mode: p.state.mode}
		if mark.Mode >= 4 {
			mark.Clip = &TextClip{Font: mark.Font, Glyphs: glyphs, Positions: positions, Matrix: mark.Matrix, Size: mark.Size, HorizontalScale: mark.HorizontalScale}
			p.textClips = append(p.textClips, mark.Clip)
		}
		if err := p.visitor.Text(mark); err != nil {
			return err
		}
	}
	p.textMatrix = p.textMatrix.Mul(Matrix{1, 0, 0, 1, advance.X, advance.Y})
	return nil
}

// resource 从当前作用域读取资源引用
// 入参: kind 资源类别, name 资源名称
// 返回: Object 资源对象或引用, error 错误信息
func (p *pageInterpreter) resource(kind, name Name) (Object, error) {
	value, err := p.reader.Resolve(p.resources[kind])
	if err != nil {
		return nil, err
	}
	dict, ok := value.(Dictionary)
	if !ok || dict[name] == nil {
		return nil, fmt.Errorf("missing %s resource %s", kind, name)
	}
	return dict[name], nil
}

// xobject 解释图像或表单资源并限制递归
// 入参: a XObject操作数
// 返回: error 错误信息
func (p *pageInterpreter) xobject(a []Object) error {
	if len(a) != 1 {
		return fmt.Errorf("invalid XObject operands")
	}
	name, ok := a[0].(Name)
	if !ok {
		return fmt.Errorf("invalid XObject name")
	}
	object, err := p.resource("XObject", name)
	if err != nil {
		return err
	}
	value, err := p.reader.Resolve(object)
	if err != nil {
		return err
	}
	stream, ok := value.(*Stream)
	if !ok {
		return fmt.Errorf("invalid XObject stream")
	}
	if stream.Dictionary["Subtype"] == Name("Image") {
		image, err := p.reader.ReadImage(stream)
		if err != nil {
			return err
		}
		if p.opaqueGroup && (image.Mask != nil || image.SoftMask != nil || image.ImageMask) {
			return &UnsupportedError{Feature: "masked image in isolated group"}
		}
		if image.ImageMask {
			if err := p.validatePaint(true, false); err != nil {
				return err
			}
		}
		if p.visitor.Image == nil {
			return fmt.Errorf("image visitor missing")
		}
		return p.visitor.Image(ImageMark{image, p.state.matrix, p.state.style})
	}
	if stream.Dictionary["Subtype"] != Name("Form") {
		return &UnsupportedError{Feature: "XObject subtype"}
	}
	return p.form(stream)
}

// validatePaint 检查实际使用的颜色状态，避免未指定图案或未支持的色彩意图被静默替换
// 入参: fill 是否填充, stroke 是否描边
// 返回: error 颜色状态错误
func (p *pageInterpreter) validatePaint(fill, stroke bool) error {
	if fill && p.state.fillSpace == "Pattern" && p.state.style.Fill.Axial == nil && p.state.style.Fill.Radial == nil && p.state.style.Fill.Tiling == nil || stroke && p.state.strokeSpace == "Pattern" && p.state.style.Stroke.Axial == nil && p.state.style.Stroke.Radial == nil && p.state.style.Stroke.Tiling == nil {
		return fmt.Errorf("missing pattern color")
	}
	if p.state.style.RenderingIntent == "AbsoluteColorimetric" && (fill && p.state.fillICC != nil || stroke && p.state.strokeICC != nil) {
		return &UnsupportedError{Feature: "absolute colorimetric ICC transform"}
	}
	return nil
}

// form 在独立图形状态中解释表单内容及透明度组
// 入参: stream 表单内容流
// 返回: error 解析或访问错误
func (p *pageInterpreter) form(stream *Stream) error {
	if p.depth >= 32 {
		return fmt.Errorf("form recursion limit exceeded")
	}
	for _, key := range []Name{"Ref", "OC"} {
		if stream.Dictionary[key] != nil {
			return &UnsupportedError{Feature: fmt.Sprintf("form field %q", key)}
		}
	}
	child := *p
	var groupMark *GroupMark
	if stream.Dictionary["Group"] != nil {
		value, err := p.reader.Resolve(stream.Dictionary["Group"])
		if err != nil {
			return err
		}
		group, ok := value.(Dictionary)
		if !ok || group["S"] != Name("Transparency") || group["I"] != nil && group["I"] != Boolean(true) && group["I"] != Boolean(false) || group["K"] != nil && group["K"] != Boolean(false) {
			return &UnsupportedError{Feature: "form transparency group"}
		}
		groupMark = &GroupMark{Alpha: p.state.style.Fill.Alpha, AlphaIsShape: p.state.style.AlphaIsShape, Isolated: group["I"] == Boolean(true), BlendMode: p.state.style.BlendMode, SoftMask: p.state.style.SoftMask}
		if group["CS"] != nil {
			groupMark.ColorSpace, err = p.reader.readBlendingSpace(group["CS"])
			if err != nil {
				return err
			}
		}
		if p.visitor.Group == nil && groupMark.ColorSpace != nil {
			if err := p.reader.validateRGBGroupSpace(group["CS"]); err != nil {
				return err
			}
		}
		if p.visitor.Group == nil && (groupMark.Alpha != 1 || !groupMark.Isolated || groupMark.SoftMask != nil || groupMark.BlendMode != "" && groupMark.BlendMode != "Normal" && groupMark.BlendMode != "Compatible") {
			return &UnsupportedError{Feature: "transparency group visitor missing"}
		}
		child.state.style.Fill.Alpha, child.state.style.Stroke.Alpha = 1, 1
		child.state.style.SoftMask = nil
		child.state.style.AlphaIsShape = false
		child.state.style.BlendMode = "Normal"
		child.opaqueGroup = p.visitor.Group == nil
	}
	child.depth++
	child.stack = nil
	child.compatibility = 0
	child.marked = nil
	child.path = Path{}
	child.hasPoint = false
	child.pendingClip = false
	child.inText = false
	if stream.Dictionary["Matrix"] != nil {
		value, err := p.reader.Resolve(stream.Dictionary["Matrix"])
		if err != nil {
			return err
		}
		array, ok := value.(Array)
		if !ok {
			return fmt.Errorf("invalid form matrix")
		}
		m, err := numbers(array, 6)
		if err != nil {
			return err
		}
		child.state.matrix = child.state.matrix.Mul(Matrix(m))
	}
	if stream.Dictionary["Resources"] != nil {
		resources, err := p.reader.Resolve(stream.Dictionary["Resources"])
		if err != nil {
			return err
		}
		var ok bool
		child.resources, ok = resources.(Dictionary)
		if !ok {
			return fmt.Errorf("invalid form resources")
		}
	}
	box, err := p.reader.rectangle(stream.Dictionary["BBox"])
	if err != nil {
		return err
	}
	m := child.state.matrix
	clip := Path{Segments: []Segment{{"M", []Point{m.Apply(Point{box.XMin, box.YMin})}}, {"L", []Point{m.Apply(Point{box.XMax, box.YMin})}}, {"L", []Point{m.Apply(Point{box.XMax, box.YMax})}}, {"L", []Point{m.Apply(Point{box.XMin, box.YMax})}}, {"C", nil}}}
	child.state.style.Clips = append(append([]Path(nil), child.state.style.Clips...), clip)
	child.patternMatrix = child.state.matrix
	data, err := stream.Decode()
	if err != nil {
		return err
	}
	if groupMark != nil && p.visitor.Group != nil {
		return p.visitor.Group(*groupMark, func(visitor Visitor) error {
			group := child
			group.visitor = visitor
			return group.run(data)
		})
	}
	return child.run(data)
}

// extState 读取可表达的外部图形状态，不忽略未知绘制效果
// 入参: a 图形状态操作数, offset 当前内容流中的字节位置
// 返回: error 错误信息
func (p *pageInterpreter) extState(a []Object, offset int64) error {
	if len(a) != 1 {
		return fmt.Errorf("invalid graphics state operands")
	}
	name, ok := a[0].(Name)
	if !ok {
		return fmt.Errorf("invalid graphics state name")
	}
	resources, err := p.reader.Resolve(p.resources["ExtGState"])
	if err != nil {
		return err
	}
	dictionary, ok := resources.(Dictionary)
	if resources != nil && !ok {
		return fmt.Errorf("invalid ExtGState dictionary")
	}
	object := dictionary[name]
	if object == nil {
		message := fmt.Sprintf("undefined ExtGState resource %s; current graphics state retained", name)
		if p.visitor.Warning == nil {
			return fmt.Errorf("%s", message)
		}
		p.visitor.Warning(Diagnostic{Offset: offset, Message: message})
		return nil
	}
	value, err := p.reader.Resolve(object)
	if err != nil {
		return err
	}
	dict, ok := value.(Dictionary)
	if !ok {
		return fmt.Errorf("invalid external graphics state")
	}
	for key, value := range dict {
		value, err = p.reader.Resolve(value)
		if err != nil {
			return err
		}
		switch key {
		case "Type":
		case "RI":
			if err := p.operation(Operation{Operator: "ri", Operands: []Object{value}}); err != nil {
				return err
			}
		case "ca", "CA":
			n, err := numbers([]Object{value}, 1)
			if err != nil {
				return err
			}
			if n[0] < 0 || n[0] > 1 {
				return fmt.Errorf("invalid alpha")
			}
			if p.opaqueGroup && n[0] != 1 {
				return &UnsupportedError{Feature: "transparent group content"}
			}
			if key == "ca" {
				p.state.style.Fill.Alpha = n[0]
			} else {
				p.state.style.Stroke.Alpha = n[0]
			}
		case "LW", "LC", "LJ", "ML":
			operator := map[Name]string{"LW": "w", "LC": "J", "LJ": "j", "ML": "M"}[key]
			if err := p.operation(Operation{Operator: operator, Operands: []Object{value}}); err != nil {
				return err
			}
		case "BM":
			mode, err := p.reader.readBlendMode(value)
			if err != nil {
				return err
			}
			p.state.style.BlendMode = mode
		case "SMask":
			if value == Name("None") {
				p.state.style.SoftMask = nil
			} else {
				mask, err := p.readSoftMask(value)
				if err != nil {
					return err
				}
				p.state.style.SoftMask = mask
			}
		case "AIS":
			flag, ok := value.(Boolean)
			if !ok {
				return fmt.Errorf("invalid graphics state flag %q", key)
			}
			p.state.style.AlphaIsShape = bool(flag)
		case "SA", "OP", "op":
			flag, ok := value.(Boolean)
			if !ok {
				return fmt.Errorf("invalid graphics state flag %q", key)
			}
			switch key {
			case "SA":
				p.state.style.StrokeAdjust = bool(flag)
			case "OP":
				p.state.style.StrokeOverprint = bool(flag)
				if dict["op"] == nil {
					p.state.style.FillOverprint = bool(flag)
				}
			case "op":
				p.state.style.FillOverprint = bool(flag)
			}
		case "OPM":
			if value != Integer(0) && value != Integer(1) {
				return fmt.Errorf("invalid overprint mode")
			}
			p.state.style.OverprintMode = int(value.(Integer))
		case "SM":
			n, err := numbers([]Object{value}, 1)
			if err != nil || n[0] < 0 || n[0] > 1 {
				return fmt.Errorf("invalid smoothness tolerance")
			}
			p.state.style.Smoothness = &n[0]
		case "AAPL:AA":
			flag, ok := value.(Boolean)
			if !ok {
				return fmt.Errorf("invalid antialias flag")
			}
			v := bool(flag)
			p.state.style.Antialias = &v
		case "BG2", "UCR2":
			if value != Name("Default") {
				return &UnsupportedError{Feature: fmt.Sprintf("graphics state field %q", key)}
			}
		default:
			return &UnsupportedError{Feature: fmt.Sprintf("graphics state field %q", key)}
		}
	}
	return nil
}

// readBlendMode 读取标准混合模式，数组按优先顺序选择支持的名称
// 入参: value 名称或名称数组
// 返回: Name 标准模式, error 无效类型
func (r *Reader) readBlendMode(value Object) (Name, error) {
	values, array := value.(Array)
	if !array {
		values = Array{value}
	}
	for _, item := range values {
		item, err := r.Resolve(item)
		if err != nil {
			return "", err
		}
		mode, ok := item.(Name)
		if !ok {
			return "", fmt.Errorf("invalid blend mode")
		}
		switch mode {
		case "Normal", "Compatible", "Multiply", "Screen", "Overlay", "Darken", "Lighten", "ColorDodge", "ColorBurn", "HardLight", "SoftLight", "Difference", "Exclusion", "Hue", "Saturation", "Color", "Luminosity":
			return mode, nil
		}
	}
	return "Normal", nil
}
