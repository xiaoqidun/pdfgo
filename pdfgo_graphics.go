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
func Identity() Matrix { return Matrix{1, 0, 0, 1, 0, 0} }

// Mul 按当前矩阵乘以右侧矩阵进行坐标组合
func (m Matrix) Mul(n Matrix) Matrix {
	return Matrix{
		m[0]*n[0] + m[2]*n[1], m[1]*n[0] + m[3]*n[1],
		m[0]*n[2] + m[2]*n[3], m[1]*n[2] + m[3]*n[3],
		m[0]*n[4] + m[2]*n[5] + m[4], m[1]*n[4] + m[3]*n[5] + m[5],
	}
}

// Apply 将点变换到目标坐标空间
func (m Matrix) Apply(p Point) Point {
	return Point{m[0]*p.X + m[2]*p.Y + m[4], m[1]*p.X + m[3]*p.Y + m[5]}
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
}

// Paint 保存设备颜色及不透明度，CMYK非空时保留原始四色分量
type Paint struct {
	RGB   [3]float64
	CMYK  *[4]float64
	Alpha float64
}

// Style 保存绘制状态及按顺序相交的裁剪路径
type Style struct {
	Fill, Stroke Paint
	LineWidth    float64
	Cap, Join    int
	MiterLimit   float64
	Dash         []float64
	DashPhase    float64
	Clips        []Path
}

// PathMark 表示一次路径绘制
type PathMark struct {
	Path         Path
	Style        Style
	Fill, Stroke bool
}

// TextMark 保存文字字形及其相对于文字矩阵的基线位置
type TextMark struct {
	Font                  *Font
	Glyphs                []Glyph
	Positions             []Point
	Matrix                Matrix
	Size, HorizontalScale float64
	Style                 Style
	Mode                  int
}

// ImageMark 保存图像资源及单位方形到页面坐标的变换
type ImageMark struct {
	Image  *Image
	Matrix Matrix
	Style  Style
}

// Visitor 按内容顺序接收页面绘制对象，未提供的回调不会丢弃对应对象
type Visitor struct {
	Path  func(PathMark) error
	Text  func(TextMark) error
	Image func(ImageMark) error
}

// graphicsState 保存图形和文字操作的当前状态
type graphicsState struct {
	matrix                                                Matrix
	style                                                 Style
	font                                                  *Font
	fontSize, spacing, wordSpacing, hscale, leading, rise float64
	mode                                                  int
	fillSpace, strokeSpace                                Name
}

// pageInterpreter 按内容顺序解释页面或表单
type pageInterpreter struct {
	reader                 *Reader
	resources              Dictionary
	visitor                Visitor
	ctx                    context.Context
	state                  graphicsState
	stack                  []graphicsState
	textMatrix, lineMatrix Matrix
	inText                 bool
	path                   Path
	current, start         Point
	hasPoint               bool
	pendingClip            bool
	clipEvenOdd            bool
	depth                  int
	fonts                  map[Reference]*Font
}

// WalkPage 解释页面内容并按绘制顺序访问可准确表达的图元
// 入参: ctx 取消上下文, page 页面, visitor 图元访问器
// 返回: error 错误信息
func (r *Reader) WalkPage(ctx context.Context, page *Page, visitor Visitor) error {
	if page.reader != r {
		return fmt.Errorf("page belongs to another reader")
	}
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
				if value != Name("DeviceRGB") {
					return &UnsupportedError{Feature: "page group color space"}
				}
			case "I":
				if value != Boolean(true) {
					return &UnsupportedError{Feature: "non-isolated page group"}
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
	for _, key := range []Name{"Annots", "Trans"} {
		if page.Dictionary[key] != nil {
			return &UnsupportedError{Feature: fmt.Sprintf("page field %q", key)}
		}
	}
	data, err := page.Content()
	if err != nil {
		return err
	}
	interpreter := pageInterpreter{reader: r, resources: page.Resources, visitor: visitor, ctx: ctx, fonts: map[Reference]*Font{}}
	interpreter.state = graphicsState{matrix: Identity(), hscale: 1, fillSpace: "DeviceGray", strokeSpace: "DeviceGray", style: Style{Fill: Paint{Alpha: 1}, Stroke: Paint{Alpha: 1}, LineWidth: 1, MiterLimit: 10}}
	return interpreter.run(data)
}

// run 解释单个页面或表单的完整内容
func (p *pageInterpreter) run(data []byte) error {
	err := WalkOperations(p.ctx, data, p.operation)
	if err != nil {
		return err
	}
	if len(p.stack) != 0 || p.inText {
		return fmt.Errorf("unbalanced graphics or text state")
	}
	return nil
}

// numbers 检查操作数数量并读取有限数值
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
func (p *pageInterpreter) operation(op Operation) error {
	a := op.Operands
	count := map[string]int{"q": 0, "Q": 0, "cm": 6, "w": 1, "J": 1, "j": 1, "M": 1, "m": 2, "l": 2, "c": 6, "v": 4, "y": 4, "h": 0, "re": 4, "S": 0, "s": 0, "f": 0, "F": 0, "f*": 0, "B": 0, "B*": 0, "b": 0, "b*": 0, "n": 0, "W": 0, "W*": 0, "g": 1, "G": 1, "rg": 3, "RG": 3, "k": 4, "K": 4, "BT": 0, "ET": 0, "Tc": 1, "Tw": 1, "Tz": 1, "TL": 1, "Tr": 1, "Ts": 1, "Td": 2, "TD": 2, "Tm": 6, "T*": 0}
	var v []float64
	if n, ok := count[op.Operator]; ok {
		var err error
		v, err = numbers(a, n)
		if err != nil {
			return err
		}
	}
	point := func(x, y float64) Point { return p.state.matrix.Apply(Point{x, y}) }
	add := func(name string, points ...Point) { p.path.Segments = append(p.path.Segments, Segment{name, points}) }
	switch op.Operator {
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
			style := p.state.style
			if stroke {
				m := p.state.matrix
				sx, sy := math.Hypot(m[0], m[1]), math.Hypot(m[2], m[3])
				if math.Abs(sx-sy) > 1e-8*math.Max(1, sx) || math.Abs(m[0]*m[2]+m[1]*m[3]) > 1e-8*math.Max(1, sx*sy) {
					return &UnsupportedError{Feature: "anisotropic path stroke"}
				}
				if style.LineWidth == 0 {
					return &UnsupportedError{Feature: "device-dependent hairline stroke"}
				}
				style.LineWidth *= sx
				style.Dash = append([]float64(nil), style.Dash...)
				for n := range style.Dash {
					style.Dash[n] *= sx
				}
				style.DashPhase *= sx
			}
			if p.visitor.Path == nil {
				return fmt.Errorf("path visitor missing")
			}
			if err := p.visitor.Path(PathMark{p.path, style, fill, stroke}); err != nil {
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
			p.state.style.Stroke.RGB = rgb
			p.state.style.Stroke.CMYK = cmyk
			if len(v) == 1 {
				p.state.strokeSpace = "DeviceGray"
			} else if cmyk != nil {
				p.state.strokeSpace = "DeviceCMYK"
			} else {
				p.state.strokeSpace = "DeviceRGB"
			}
		} else {
			p.state.style.Fill.RGB = rgb
			p.state.style.Fill.CMYK = cmyk
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
		if !ok || (name != "DeviceRGB" && name != "DeviceGray" && name != "DeviceCMYK") {
			return &UnsupportedError{Feature: "non-device color space"}
		}
		if op.Operator == "cs" {
			p.state.fillSpace = name
			p.state.style.Fill.RGB = [3]float64{}
			p.state.style.Fill.CMYK = nil
			if name == "DeviceCMYK" {
				p.state.style.Fill.CMYK = &[4]float64{0, 0, 0, 1}
			}
		} else {
			p.state.strokeSpace = name
			p.state.style.Stroke.RGB = [3]float64{}
			p.state.style.Stroke.CMYK = nil
			if name == "DeviceCMYK" {
				p.state.style.Stroke.CMYK = &[4]float64{0, 0, 0, 1}
			}
		}
	case "sc", "scn", "SC", "SCN":
		space := p.state.fillSpace
		operator := "g"
		if op.Operator == "SC" || op.Operator == "SCN" {
			space = p.state.strokeSpace
			operator = "G"
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
		p.textMatrix = Identity()
		p.lineMatrix = Identity()
	case "ET":
		if !p.inText {
			return fmt.Errorf("unmatched operator %q", "ET")
		}
		p.inText = false
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
		ref, indirect := object.(Reference)
		font := p.fonts[ref]
		if !indirect || font == nil {
			font, err = p.reader.ReadFont(object)
			if err != nil {
				return err
			}
			if indirect {
				p.fonts[ref] = font
			}
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
		if v[0] < 0 || v[0] > 3 || v[0] != math.Trunc(v[0]) {
			return &UnsupportedError{Feature: "text clipping mode"}
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
				p.textMatrix = p.textMatrix.Mul(Matrix{1, 0, 0, 1, -adjustment[0] / 1000 * p.state.fontSize * p.state.hscale, 0})
			}
		}
	case "Do":
		return p.xobject(a)
	case "gs":
		return p.extState(a)
	case "ri":
		if len(a) != 1 || a[0] != Name("RelativeColorimetric") {
			return &UnsupportedError{Feature: "rendering intent"}
		}
	case "i":
		n, err := numbers(a, 1)
		if err != nil {
			return err
		}
		if n[0] < 0 || n[0] > 100 {
			return fmt.Errorf("invalid flatness")
		}
	case "BMC", "BDC", "EMC", "MP", "DP":
		return &UnsupportedError{Feature: "marked content requiring semantic preservation"}
	default:
		return &UnsupportedError{Feature: "content operator " + op.Operator}
	}
	return nil
}

// showText 保留逐字定位并更新文字矩阵
func (p *pageInterpreter) showText(data []byte) error {
	if !p.inText || p.state.font == nil {
		return fmt.Errorf("text without active font")
	}
	if p.state.mode == 1 || p.state.mode == 2 {
		m := p.textMatrix
		if math.Abs(math.Hypot(m[0], m[1])-1) > 1e-8 || math.Abs(math.Hypot(m[2], m[3])-1) > 1e-8 || math.Abs(m[0]*m[2]+m[1]*m[3]) > 1e-8 {
			return &UnsupportedError{Feature: "scaled text stroke matrix"}
		}
	}
	glyphs, err := p.state.font.Decode(data)
	if err != nil {
		return err
	}
	positions := make([]Point, len(glyphs))
	advance := 0.0
	for n, glyph := range glyphs {
		positions[n] = Point{advance, p.state.rise}
		width := glyph.Width/1000*p.state.fontSize + p.state.spacing
		if glyph.WordSpace {
			width += p.state.wordSpacing
		}
		advance += width * p.state.hscale
	}
	if len(glyphs) > 0 {
		if p.visitor.Text == nil {
			return fmt.Errorf("text visitor missing")
		}
		mark := TextMark{Font: p.state.font, Glyphs: glyphs, Positions: positions, Matrix: p.state.matrix.Mul(p.textMatrix), Size: p.state.fontSize, HorizontalScale: p.state.hscale, Style: p.state.style, Mode: p.state.mode}
		if err := p.visitor.Text(mark); err != nil {
			return err
		}
	}
	p.textMatrix = p.textMatrix.Mul(Matrix{1, 0, 0, 1, advance, 0})
	return nil
}

// resource 从当前作用域读取资源引用
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
		if p.visitor.Image == nil {
			return fmt.Errorf("image visitor missing")
		}
		return p.visitor.Image(ImageMark{image, p.state.matrix, p.state.style})
	}
	if stream.Dictionary["Subtype"] != Name("Form") {
		return &UnsupportedError{Feature: "XObject subtype"}
	}
	if p.depth >= 32 {
		return fmt.Errorf("form recursion limit exceeded")
	}
	for _, key := range []Name{"Group", "Ref", "OC"} {
		if stream.Dictionary[key] != nil {
			return &UnsupportedError{Feature: fmt.Sprintf("form field %q", key)}
		}
	}
	child := *p
	child.depth++
	child.stack = nil
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
	data, err := stream.Decode()
	if err != nil {
		return err
	}
	return child.run(data)
}

// extState 读取可表达的外部图形状态，不忽略未知绘制效果
func (p *pageInterpreter) extState(a []Object) error {
	if len(a) != 1 {
		return fmt.Errorf("invalid graphics state operands")
	}
	name, ok := a[0].(Name)
	if !ok {
		return fmt.Errorf("invalid graphics state name")
	}
	object, err := p.resource("ExtGState", name)
	if err != nil {
		return err
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
		case "ca", "CA":
			n, err := numbers([]Object{value}, 1)
			if err != nil {
				return err
			}
			if n[0] < 0 || n[0] > 1 {
				return fmt.Errorf("invalid alpha")
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
			if value != Name("Normal") && value != Name("Compatible") {
				return &UnsupportedError{Feature: "blend mode"}
			}
		case "SMask":
			if value != Name("None") {
				return &UnsupportedError{Feature: "soft mask graphics state"}
			}
		case "AIS", "OP", "op":
			if value != Boolean(false) {
				return &UnsupportedError{Feature: fmt.Sprintf("graphics state field %q", key)}
			}
		default:
			return &UnsupportedError{Feature: fmt.Sprintf("graphics state field %q", key)}
		}
	}
	return nil
}
