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
	"context"
	"fmt"
	"math"
)

// Matrix 表示PDF二维仿射矩阵，使用列向量坐标约定
type Matrix [6]float64

// Point 表示用户空间坐标
type Point struct{ X, Y float64 }

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
// None表示丢弃着色输出，与零透明度不同，不参与挖空合成
// Process保留原生过程通道，区分未指定通道和已指定的零分量
// Colorant保留专色定义和源浓度快照，不随后续图形状态变化
// ColorantDevice访问器可接收备用色为None但原生色料可用的画刷，None仅描述其备用显示
// SourceSpace保留普通颜色的来源空间族，图案基色使用底层空间族，不以转换后的Space或CMYK代替
// Shading保留着色图案的内部状态，Alpha仍属于使用图案的对象
type Paint struct {
	RGB         [3]float64
	CMYK        *[4]float64
	Space       *ColorSpace
	SourceSpace Name
	Process     *ProcessColorants
	Colorant    *ColorantPaint
	Values      [4]float64
	Alpha       float64
	None        bool
	Axial       *AxialGradient
	Radial      *RadialGradient
	Function    *FunctionGradient
	Mesh        *MeshGradient
	Tiling      *TilingPattern
	Shading     *ShadingPattern
}

// Style 保存绘制状态及按顺序相交的裁剪路径
// Transfer在最终设备颜色转换及透明合成后应用，不预先改变Fill和Stroke
// HalftoneOrigin保存设置时的页面用户坐标，后续变换不移动原点，空值采用设备默认值
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
	Halftone        *Halftone
	HalftoneOrigin  *Point
	Transfer        *TransferFunction
	ColorConversion ColorConversion
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
// Mode保留有效绘制模式，Type3除不可见模式3外均按0交付，不产生文字裁剪
// Object只读共享同一BT与ET边界，不同文字对象使用不同实例
// Resources保留所在页面的只读资源，供未声明资源字典的Type3字形查找
type TextMark struct {
	Object                *TextObject
	Font                  *Font
	Resources             Dictionary
	Glyphs                []Glyph
	Positions             []Point
	Matrix                Matrix
	StrokeMatrix          Matrix
	Size, HorizontalScale float64
	Style                 Style
	Mode                  int
	Clip                  *TextClip
	glyphStreams          []*Stream
	halftones             map[string]*Halftone
}

// TextObject 保留文字对象身份，Knockout为真时整段挖空，为假时逐字合成
type TextObject struct {
	Knockout bool
}

// ImageMark 保存图像资源及单位方形到页面坐标的变换
type ImageMark struct {
	Image  *Image
	Matrix Matrix
	Style  Style
}

// FormMark 保存普通表单的局部边界及到页面坐标的变换，不代表透明组
type FormMark struct {
	Bounds Rectangle
	Matrix Matrix
}

// GroupMark 保存透明度组的边界不透明度和隔离方式，组内绘制使用独立状态
// Page区分页面初始组与内容流中的表单组
type GroupMark struct {
	Page            bool
	Alpha           float64
	AlphaIsShape    bool
	Isolated        bool
	Knockout        bool
	BlendMode       Name
	SoftMask        *SoftMask
	ColorSpace      *ColorSpace
	ColorConversion ColorConversion
	RenderingIntent Name
}

// MarkedContentMark 保存内容标记及其属性，结束标记沿用开始标记的标签
type MarkedContentMark struct {
	Tag        Name
	Properties Dictionary
	Operator   string
}

// Visitor 按内容顺序接收页面绘制对象，缺少对应绘制回调时返回错误
// Warning非空时报告空Type3字形、缺失的ExtGState或Shading资源及未保留的内容语义，其他解析错误仍返回错误
// OptionalContent可覆盖内容区段及XObject的可选内容状态，缺省使用文档默认配置
// Reference可提供引用表单的目标页面，缺省或返回nil时绘制代理内容
// ColorantDevice声明输出设备，保留其可用色料，实际分色求值与合成由访问器完成
// Halftones提供只读设备命名网屏，优先于文件中的同名备用定义
// Form可保留普通表单边界，缺省直接展开；透明表单仍由Group接收
// Form的子访问器接收已变换并裁剪的图元，不应再次应用表单矩阵
// Form的子访问器未指定内容标记、可选内容及警告回调时沿用上层回调
type Visitor struct {
	Path            func(PathMark) error
	Text            func(TextMark) error
	Image           func(ImageMark) error
	Form            func(FormMark, func(Visitor) error) error
	Group           func(GroupMark, func(Visitor) error) error
	MarkedContent   func(MarkedContentMark) error
	OptionalContent func(Object) (bool, error)
	Reference       ReferenceResolver
	PostScript      func(PostScriptMark) error
	ColorantDevice  *ColorantDevice
	Halftones       map[string]*Halftone
	Warning         func(Diagnostic)
}

// graphicsState 保存图形和文字操作的当前状态
type graphicsState struct {
	matrix                                                Matrix
	style                                                 Style
	font                                                  *Font
	fontSize, spacing, wordSpacing, hscale, leading, rise float64
	mode                                                  int
	nonKnockout                                           bool
	fillSpace, strokeSpace                                Name
	fillPatternBase, strokePatternBase                    *patternColorSpace
	fillICC, strokeICC                                    *iccColorSpace
	fillICCValues, strokeICCValues                        [4]float64
	fillSeparation, strokeSeparation                      *separationSpace
	fillDeviceN, strokeDeviceN                            *deviceNSpace
	fillColor, strokeColor                                *graphicsColorSpace
}

// markedContentState 保存标记名称及进入区段前的可见性
type markedContentState struct {
	tag    Name
	hidden bool
}

// pageInterpreter 按内容顺序解释页面或表单
type pageInterpreter struct {
	reader                 *Reader
	resources              Dictionary
	pageResources          Dictionary
	visitor                Visitor
	ctx                    context.Context
	state                  graphicsState
	stack                  []graphicsState
	marked                 []markedContentState
	hidden                 bool
	textClips              []*TextClip
	textMatrix, lineMatrix Matrix
	inText                 bool
	textObject             *TextObject
	path                   Path
	pathPoints             []Point
	current, start         Point
	hasPoint               bool
	pendingClip            bool
	clipEvenOdd            bool
	depth                  int
	compatibility          int
	opaqueGroup            bool
	patternMatrix          Matrix
	patternState           *graphicsState
	tilingPatterns         map[Name]*TilingPattern
	bounds                 Rectangle
	uncoloredPattern       bool
	type3                  bool
	uncoloredType3         bool
	glyphStreams           []*Stream
	maskGroup              *Stream
	blendingSpace          *ColorSpace
	forms                  *formCache
	content                *formContent
}

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
	for _, value := range m {
		if math.IsNaN(value) || math.IsInf(value, 0) {
			return Matrix{}, false
		}
	}
	a, b := m[0]*m[3], m[1]*m[2]
	d := a - b
	if d != 0 && !math.IsInf(d, 0) && math.Abs(d) >= 0x1p-1022 && math.Abs(d) >= 0x1p-48*math.Max(math.Abs(a), math.Abs(b)) {
		n := Matrix{m[3] / d, -m[1] / d, -m[2] / d, m[0] / d, (m[2]*m[5] - m[3]*m[4]) / d, (m[1]*m[4] - m[0]*m[5]) / d}
		valid := true
		for _, value := range n {
			valid = valid && !math.IsNaN(value) && !math.IsInf(value, 0)
		}
		if valid {
			return n, true
		}
	}
	determinant, exponent := matrixProductSum(m[0], m[3], -m[1], m[2])
	if determinant == 0 {
		return Matrix{}, false
	}
	divide := func(value float64, power int) float64 {
		mantissa, shift := math.Frexp(value)
		return math.Ldexp(mantissa/determinant, power+shift-exponent)
	}
	x, xe := matrixProductSum(m[2], m[5], -m[3], m[4])
	y, ye := matrixProductSum(m[1], m[4], -m[0], m[5])
	n := Matrix{divide(m[3], 0), divide(-m[1], 0), divide(-m[2], 0), divide(m[0], 0), divide(x, xe), divide(y, ye)}
	for _, v := range n {
		if math.IsNaN(v) || math.IsInf(v, 0) {
			return Matrix{}, false
		}
	}
	return n, true
}

// MaxScale 返回线性部分的最大伸长率，包含剪切且不受平移影响
// 返回: float64 最大奇异值，非有限变换返回非有限值
func (m Matrix) MaxScale() float64 {
	scale := math.Max(math.Max(math.Abs(m[0]), math.Abs(m[1])), math.Max(math.Abs(m[2]), math.Abs(m[3])))
	if scale == 0 || math.IsInf(scale, 0) || math.IsNaN(scale) {
		return scale
	}
	a, b, c, d := m[0]/scale, m[1]/scale, m[2]/scale, m[3]/scale
	return scale * ((math.Hypot(a+d, b-c) + math.Hypot(a-d, b+c)) / 2)
}

// matrixProductSum 以尾数及指数累加两个乘积，保留乘法舍入残差
// 入参: a 第一个乘数, b 第二个乘数, c 第三个乘数, d 第四个乘数
// 返回: float64 结果尾数, int 二进制指数
func matrixProductSum(a, b, c, d float64) (float64, int) {
	a, ae := math.Frexp(a)
	b, be := math.Frexp(b)
	c, ce := math.Frexp(c)
	d, de := math.Frexp(d)
	x, y := a*b, c*d
	xe, ye := ae+be, ce+de
	power := max(xe, ye)
	if x == 0 {
		power = ye
	} else if y == 0 {
		power = xe
	}
	value := math.Ldexp(x, xe-power) + math.Ldexp(y, ye-power)
	value += math.Ldexp(math.FMA(a, b, -x), xe-power) + math.Ldexp(math.FMA(c, d, -y), ye-power)
	value, shift := math.Frexp(value)
	return value, power + shift
}

// WalkPage 解释页面内容并按绘制顺序访问可准确表达的图元，注解由Page.Annotations读取
// 入参: ctx 取消上下文, page 页面, visitor 图元访问器
// 返回: error 错误信息
func (r *Reader) WalkPage(ctx context.Context, page *Page, visitor Visitor) error {
	if page.reader != r {
		return fmt.Errorf("page belongs to another reader")
	}
	var groupSpace *ColorSpace
	var knockout bool
	value, err := r.Resolve(page.Dictionary["Group"])
	if err != nil {
		return err
	}
	if value != nil {
		group, ok := value.(Dictionary)
		if !ok {
			return fmt.Errorf("invalid page group")
		}
		kind, err := r.Resolve(group["S"])
		if err != nil {
			return err
		}
		if kind != Name("Transparency") {
			return &UnsupportedError{Feature: "page group subtype"}
		}
		for key, value := range group {
			value, err = r.Resolve(value)
			if err != nil {
				return err
			}
			if value == nil {
				continue
			}
			switch key {
			case "Type":
				if value != Name("Group") {
					return &UnsupportedError{Feature: "page group type"}
				}
			case "S":
			case "CS":
				groupSpace, err = r.resourceBlendingSpace(value, page.Resources)
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
				flag, ok := value.(Boolean)
				if !ok {
					return fmt.Errorf("invalid page knockout flag")
				}
				knockout = bool(flag)
			default:
				return &UnsupportedError{Feature: fmt.Sprintf("page group field %q", key)}
			}
		}
	}
	if value, err := r.Resolve(page.Dictionary["Trans"]); err != nil {
		return err
	} else if value != nil {
		transition, ok := value.(Dictionary)
		if !ok {
			return fmt.Errorf("invalid page transition")
		}
		style, err := r.Resolve(transition["S"])
		if err != nil {
			return err
		}
		if style != nil && style != Name("R") {
			return &UnsupportedError{Feature: "page transition " + fmt.Sprint(style)}
		}
	}
	var data bytes.Buffer
	if _, err := page.WriteContent(ctx, &data); err != nil {
		return err
	}
	interpreter := pageInterpreter{reader: r, resources: page.Resources, pageResources: page.Resources, visitor: visitor, ctx: ctx, bounds: page.CropBox}
	interpreter.blendingSpace = groupSpace
	interpreter.patternMatrix = Identity()
	interpreter.state = graphicsState{matrix: Identity(), hscale: 1, fillSpace: "DeviceGray", strokeSpace: "DeviceGray", style: Style{Fill: Paint{SourceSpace: "DeviceGray", Alpha: 1}, Stroke: Paint{SourceSpace: "DeviceGray", Alpha: 1}, LineWidth: 1, MiterLimit: 10}}
	for _, operator := range []string{"g", "G"} {
		if err := interpreter.operation(Operation{Operator: operator, Operands: []Object{Integer(0)}}); err != nil {
			return err
		}
	}
	if knockout && visitor.Group == nil {
		return &UnsupportedError{Feature: "page knockout visitor missing"}
	}
	if (groupSpace != nil || knockout) && visitor.Group != nil {
		return visitor.Group(GroupMark{Page: true, Alpha: 1, Isolated: true, Knockout: knockout, ColorSpace: groupSpace}, func(v Visitor) error {
			child := interpreter
			if v.Reference == nil {
				v.Reference = visitor.Reference
			}
			if v.ColorantDevice == nil {
				v.ColorantDevice = visitor.ColorantDevice
			}
			if v.Halftones == nil {
				v.Halftones = visitor.Halftones
			}
			child.visitor = v
			return child.run(data.Bytes())
		})
	}
	return interpreter.run(data.Bytes())
}

// WalkType3Glyph 按文字位置解释Type3字形内容流并访问其中的图元
// 入参: ctx 取消上下文, mark 文字绘制信息, index 字形下标, visitor 图元访问器
// 返回: error 解析或访问错误
func (r *Reader) WalkType3Glyph(ctx context.Context, mark TextMark, index int, visitor Visitor) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if visitor.Halftones == nil {
		visitor.Halftones = mark.halftones
	}
	font := mark.Font
	if font == nil || font.Subtype != Name("Type3") || index < 0 || index >= len(mark.Glyphs) || index >= len(mark.Positions) {
		return fmt.Errorf("invalid Type3 glyph")
	}
	if font.reader != nil {
		r = font.reader
	}
	if mark.Mode == 3 {
		return nil
	}
	procedure, present := font.type3Procs[Name(mark.Glyphs[index].Name)]
	if !present {
		return nil
	}
	object, err := r.Resolve(procedure)
	if err != nil {
		return err
	}
	if object == nil {
		return nil
	}
	stream, ok := object.(*Stream)
	if !ok {
		return fmt.Errorf("missing Type3 character procedure")
	}
	for _, active := range mark.glyphStreams {
		if active == stream {
			return fmt.Errorf("recursive Type3 character procedure %q", mark.Glyphs[index].Name)
		}
	}
	if len(mark.glyphStreams) >= 64 {
		return fmt.Errorf("Type3 character procedure depth exceeded")
	}
	if len(stream.Data) == 0 {
		message := fmt.Sprintf("empty Type3 character procedure %q", mark.Glyphs[index].Name)
		if visitor.Warning == nil {
			return fmt.Errorf("%s", message)
		}
		visitor.Warning(Diagnostic{Message: message})
		return nil
	}
	data, err := stream.DecodeContext(ctx)
	if err != nil {
		return err
	}
	position := mark.Positions[index]
	text := Matrix{mark.Size * mark.HorizontalScale, 0, 0, mark.Size, position.X, position.Y}
	resources := font.type3Resources
	if resources == nil {
		resources = mark.Resources
	}
	interpreter := pageInterpreter{reader: r, resources: resources, pageResources: mark.Resources, visitor: visitor, ctx: ctx, type3: true}
	interpreter.glyphStreams = append(append([]*Stream(nil), mark.glyphStreams...), stream)
	interpreter.patternMatrix = Identity()
	interpreter.state = graphicsState{matrix: mark.Matrix.Mul(text).Mul(font.type3Matrix), hscale: 1, fillSpace: "DeviceGray", strokeSpace: "DeviceGray", style: mark.Style}
	interpreter.state.nonKnockout = mark.Object != nil && !mark.Object.Knockout
	if font.type3Bounds != nil {
		interpreter.bounds = transformedBounds(*font.type3Bounds, interpreter.state.matrix)
	}
	return interpreter.run(data)
}

// transformedBounds 计算仿射变换后矩形的轴对齐边界
// 入参: box 原始矩形, matrix 坐标变换
// 返回: Rectangle 变换后的边界
func transformedBounds(box Rectangle, matrix Matrix) Rectangle {
	result := Rectangle{XMin: math.Inf(1), YMin: math.Inf(1), XMax: math.Inf(-1), YMax: math.Inf(-1)}
	for _, point := range [4]Point{{box.XMin, box.YMin}, {box.XMax, box.YMin}, {box.XMax, box.YMax}, {box.XMin, box.YMax}} {
		point = matrix.Apply(point)
		result.XMin, result.YMin = math.Min(result.XMin, point.X), math.Min(result.YMin, point.Y)
		result.XMax, result.YMax = math.Max(result.XMax, point.X), math.Max(result.YMax, point.Y)
	}
	return result
}

// run 解释单个页面或表单的完整内容
// 入参: data 解码后的内容流
// 返回: error 错误信息
func (p *pageInterpreter) run(data []byte) error {
	p.tilingPatterns = nil
	if p.patternState == nil && p.resources["Pattern"] != nil {
		initial := p.state
		p.patternState = &initial
	}
	operations, size, err := p.walkContent(data)
	if err != nil {
		return err
	}
	if len(p.stack) != 0 || p.inText || len(p.marked) != 0 {
		return fmt.Errorf("unbalanced graphics, text or marked content state")
	}
	if p.compatibility != 0 {
		return fmt.Errorf("unbalanced compatibility section")
	}
	if p.content != nil {
		p.content.parsed = true
		if operations != nil && size <= formCacheLimit-p.forms.bytes {
			p.content.operations = operations
			p.forms.bytes += size
		}
	}
	return nil
}

// markedContent 更新可选区段可见性并交付内容标记
// 入参: op 内容标记操作
// 返回: error 错误信息
func (p *pageInterpreter) markedContent(op Operation) error {
	mark := MarkedContentMark{Operator: op.Operator}
	optional := false
	if op.Operator == "EMC" {
		if len(op.Operands) != 0 || len(p.marked) == 0 {
			return fmt.Errorf("unmatched marked content end")
		}
		state := p.marked[len(p.marked)-1]
		mark.Tag, p.hidden = state.tag, state.hidden
		p.marked = p.marked[:len(p.marked)-1]
	} else {
		hidden := p.hidden
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
			if mark.Tag == "OC" {
				kind, err := p.reader.Resolve(mark.Properties["Type"])
				if err != nil {
					return err
				}
				optional = kind == Name("OCG") || kind == Name("OCMD")
				if optional && op.Operator == "BDC" {
					visible, err := p.optionalVisible(object)
					if err != nil {
						return err
					}
					p.hidden = hidden || !visible
				}
			}
		}
		if op.Operator == "BMC" || op.Operator == "BDC" {
			p.marked = append(p.marked, markedContentState{mark.Tag, hidden})
		}
	}
	if p.visitor.MarkedContent != nil {
		return p.visitor.MarkedContent(mark)
	}
	if op.Operator != "EMC" && !optional {
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
		if math.IsNaN(values[n]) || math.IsInf(values[n], 0) {
			return nil, fmt.Errorf("nonfinite operand")
		}
	}
	return values, nil
}

// addPathSegment 分块保存路径坐标，各段限制容量且不复用已交付的存储
// 入参: name 路径操作, points 坐标点
func (p *pageInterpreter) addPathSegment(name string, points ...Point) {
	var stored []Point
	if n := len(points); n != 0 {
		if len(p.pathPoints) < n {
			p.pathPoints = make([]Point, max(n, min(256, max(4, len(p.path.Segments)*2))))
		}
		stored = p.pathPoints[:n:n]
		copy(stored, points)
		p.pathPoints = p.pathPoints[n:]
	}
	p.path.Segments = append(p.path.Segments, Segment{name, stored})
}

// operation 执行内容操作，未知可见操作返回明确错误
// 入参: op 内容操作
// 返回: error 错误信息
func (p *pageInterpreter) operation(op Operation) error {
	a := op.Operands
	if p.uncoloredPattern {
		switch op.Operator {
		case "g", "G", "rg", "RG", "k", "K", "cs", "CS", "sc", "SC", "scn", "SCN", "sh", "ri":
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
	if p.uncoloredType3 {
		switch op.Operator {
		case "g", "G", "rg", "RG", "k", "K", "cs", "CS", "sc", "SC", "scn", "SCN":
			return nil
		}
	}
	point := func(x, y float64) Point { return p.state.matrix.Apply(Point{x, y}) }
	add := p.addPathSegment
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
		p.uncoloredType3 = op.Operator == "d1"
		if op.Operator == "d1" {
			p.bounds = transformedBounds(Rectangle{v[2], v[3], v[4], v[5]}, p.state.matrix)
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
		fill, stroke = p.visiblePaint(fill, stroke)
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
		p.pathPoints = nil
		p.hasPoint = false
		p.pendingClip = false
	case "g", "G", "rg", "RG", "k", "K":
		for n := range v {
			v[n] = math.Max(0, math.Min(1, v[n]))
		}
		name := map[int]Name{1: "DeviceGray", 3: "DeviceRGB", 4: "DeviceCMYK"}[len(v)]
		fill := op.Operator == "g" || op.Operator == "rg" || op.Operator == "k"
		if p.resources["ColorSpace"] == nil {
			return p.setDeviceColor(v, name, fill)
		}
		object, err := p.reader.resourceColorSpace(name, p.resources)
		if err != nil {
			return err
		}
		if model, ok := object.(Name); ok && (model == "DeviceGray" || model == "DeviceRGB" || model == "DeviceCMYK") {
			return p.setDeviceColor(v, model, fill)
		}
		if err := p.selectColorSpace(object, fill); err != nil {
			return err
		}
		operands := make([]Object, len(v))
		for i, value := range v {
			operands[i] = Real(value)
		}
		operator := "scn"
		if !fill {
			operator = "SCN"
		}
		return p.operation(Operation{Operator: operator, Operands: operands})
	case "cs", "CS":
		if len(a) != 1 {
			return fmt.Errorf("invalid color space")
		}
		name, ok := a[0].(Name)
		if !ok {
			return fmt.Errorf("invalid color space name")
		}
		original, err := p.reader.remapColorSpace(name, p.resources, false, 0)
		if err != nil {
			return err
		}
		object, err := p.reader.resourceColorSpace(original, p.resources)
		if err != nil {
			return err
		}
		fill := op.Operator == "cs"
		if err := p.selectColorSpace(object, fill); err != nil {
			return err
		}
		if model := colorSpaceFamily(original); model == "DeviceGray" || model == "DeviceRGB" || model == "DeviceCMYK" {
			operands := make([]Object, (&ColorSpace{Model: model}).Components())
			for i := range operands {
				operands[i] = Integer(0)
			}
			if model == "DeviceCMYK" {
				operands[3] = Integer(1)
			}
			operator := "scn"
			if !fill {
				operator = "SCN"
			}
			return p.operation(Operation{Operator: operator, Operands: operands})
		}
	case "sc", "scn", "SC", "SCN":
		space := p.state.fillSpace
		patternBase := p.state.fillPatternBase
		profile := p.state.fillICC
		separation := p.state.fillSeparation
		deviceN := p.state.fillDeviceN
		calibrated := p.state.fillColor
		operator := "g"
		if op.Operator == "SC" || op.Operator == "SCN" {
			space = p.state.strokeSpace
			patternBase = p.state.strokePatternBase
			profile = p.state.strokeICC
			separation = p.state.strokeSeparation
			deviceN = p.state.strokeDeviceN
			calibrated = p.state.strokeColor
			operator = "G"
		}
		if space == "Lab" || space == "CalRGB" || space == "CalGray" || space == "Indexed" {
			count := 3
			if space == "Indexed" || space == "CalGray" {
				count = 1
			}
			values, err := numbers(a, count)
			if err != nil {
				return err
			}
			paint, err := calibrated.paint(values)
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
		if space == "DeviceN" {
			values, err := numbers(a, deviceN.components)
			if err != nil {
				return err
			}
			paint, err := deviceN.paint(values, p.state.style.RenderingIntent)
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
				paint.Alpha = p.state.style.Fill.Alpha
				p.state.style.Fill = paint
			} else {
				paint.Alpha = p.state.style.Stroke.Alpha
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
				copy(p.state.fillICCValues[:], values)
				paint.Alpha = p.state.style.Fill.Alpha
				p.state.style.Fill = paint
			} else {
				copy(p.state.strokeICCValues[:], values)
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
			if patternBase != nil {
				if err := patternBaseColor(&paint, patternBase, a[:len(a)-1], p.state.style.RenderingIntent); err != nil {
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
				if pattern.PaintType == 2 && patternBase == nil || pattern.PaintType == 1 && patternBase != nil {
					return fmt.Errorf("pattern paint type does not match color space")
				}
				paint.Tiling, paint.Axial, paint.Radial, paint.Mesh, paint.Function, paint.Shading = pattern, nil, nil, nil, nil, nil
				if operator == "g" {
					p.state.style.Fill = paint
				} else {
					p.state.style.Stroke = paint
				}
				return nil
			}
			if patternBase != nil {
				return fmt.Errorf("shading pattern cannot use a base color space")
			}
			gradient, err := p.shadingPattern(name)
			if err != nil {
				return err
			}
			if operator == "g" {
				gradient.Alpha = p.state.style.Fill.Alpha
				p.state.style.Fill = gradient
			} else {
				gradient.Alpha = p.state.style.Stroke.Alpha
				p.state.style.Stroke = gradient
			}
			return nil
		}
		return p.deviceColor(a, operator == "g")
	case "BT":
		if p.inText {
			return fmt.Errorf("nested text object")
		}
		p.inText = true
		p.textObject = &TextObject{Knockout: !p.state.nonKnockout}
		p.textClips = nil
		p.textMatrix = Identity()
		p.lineMatrix = Identity()
	case "ET":
		if !p.inText {
			return fmt.Errorf("unmatched operator %q", "ET")
		}
		p.inText = false
		p.textObject = nil
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
		object, err := p.resource("Font", name)
		if err != nil {
			return err
		}
		font, err := p.reader.ReadFontContext(p.ctx, object)
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
		return p.shadingFill(a, op.Offset)
	case "Do":
		return p.xobject(a)
	case "BI":
		if len(a) != 1 {
			return fmt.Errorf("invalid inline image operands")
		}
		stream, ok := a[0].(*Stream)
		if !ok {
			return fmt.Errorf("invalid inline image stream")
		}
		return p.image(stream)
	case "gs":
		return p.extState(a, op.Offset)
	case "ri":
		if len(a) != 1 {
			return fmt.Errorf("invalid rendering intent operands")
		}
		intent, ok := a[0].(Name)
		if !ok {
			return fmt.Errorf("invalid rendering intent")
		}
		p.state.style.RenderingIntent = normalizeRenderingIntent(intent)
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
	mode := p.state.mode
	if p.state.font.Subtype == "Type3" {
		if p.hidden {
			mode = 3
		} else if mode != 3 {
			mode = 0
		}
	} else {
		paintMode := mode % 4
		fill, stroke := p.visiblePaint(paintMode == 0 || paintMode == 2, paintMode == 1 || paintMode == 2)
		if err := p.validatePaint(fill, stroke); err != nil {
			return err
		}
		switch {
		case fill && stroke:
			paintMode = 2
		case fill:
			paintMode = 0
		case stroke:
			paintMode = 1
		default:
			paintMode = 3
		}
		mode = mode/4*4 + paintMode
	}
	glyphs, err := p.state.font.DecodeContext(p.ctx, data)
	if err != nil {
		return err
	}
	positions := make([]Point, len(glyphs))
	advance := Point{}
	for n, glyph := range glyphs {
		if n&255 == 0 {
			if err := p.ctx.Err(); err != nil {
				return err
			}
		}
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
	if len(glyphs) > 0 && (!p.hidden || mode >= 4) {
		if err := p.ctx.Err(); err != nil {
			return err
		}
		if p.visitor.Text == nil {
			return fmt.Errorf("text visitor missing")
		}
		mark := TextMark{Object: p.textObject, Font: p.state.font, Glyphs: glyphs, Positions: positions, Matrix: p.state.matrix.Mul(p.textMatrix), StrokeMatrix: p.state.matrix, Size: p.state.fontSize, HorizontalScale: p.state.hscale, Style: p.state.style, Mode: mode}
		if mark.Font.Subtype == "Type3" {
			mark.Resources = p.pageResources
		}
		mark.glyphStreams = p.glyphStreams
		mark.halftones = p.visitor.Halftones
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

// visiblePaint 排除隐藏区段及None分色的着色，不改变透明度或裁剪
// 入参: fill 填充标志, stroke 描边标志
// 返回: bool 是否填充, bool 是否描边
func (p *pageInterpreter) visiblePaint(fill, stroke bool) (bool, bool) {
	if p.hidden {
		return false, false
	}
	fill, stroke = fill && p.paintVisible(p.state.style.Fill), stroke && p.paintVisible(p.state.style.Stroke)
	if s := p.state.fillPatternBase; p.state.fillSpace == "Pattern" && s != nil && s.invisible {
		fill = fill && p.state.style.Fill.Colorant != nil && p.paintVisible(p.state.style.Fill)
	}
	if s := p.state.strokePatternBase; p.state.strokeSpace == "Pattern" && s != nil && s.invisible {
		stroke = stroke && p.state.style.Stroke.Colorant != nil && p.paintVisible(p.state.style.Stroke)
	}
	if s := p.state.fillSeparation; p.state.fillSpace == "Separation" && s != nil && s.name == "None" {
		fill = false
	}
	if s := p.state.strokeSeparation; p.state.strokeSpace == "Separation" && s != nil && s.name == "None" {
		stroke = false
	}
	return fill, stroke
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
	if p.hidden {
		return nil
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
	if !ok || stream == nil {
		return fmt.Errorf("invalid XObject stream")
	}
	kind, err := p.reader.Resolve(stream.Dictionary["Subtype"])
	if err != nil {
		return err
	}
	if kind == Name("Image") {
		return p.image(stream)
	}
	if kind == Name("PS") {
		return p.postscript(stream)
	}
	if kind != Name("Form") {
		return &UnsupportedError{Feature: "XObject subtype"}
	}
	return p.form(stream)
}

// image 解释外部或内联图像，统一遮罩、可见性和诊断处理
// 入参: stream 图像数据流
// 返回: error 解析或访问错误
func (p *pageInterpreter) image(stream *Stream) error {
	if p.hidden {
		return nil
	}
	visible, err := p.optionalVisible(stream.Dictionary["OC"])
	if err != nil || !visible {
		return err
	}
	image, err := p.reader.ReadImageWithResources(stream, p.resources)
	if err != nil {
		return err
	}
	if image.Intent == "" {
		image.Intent = normalizeRenderingIntent(p.state.style.RenderingIntent)
	}
	if !image.ImageMask {
		visible, err := p.colorSpaceVisible(image.ColorSpace)
		if err != nil || !visible {
			return err
		}
	}
	if p.opaqueGroup && (image.Mask != nil || image.SoftMask != nil || image.ImageMask) {
		return &UnsupportedError{Feature: "masked image in isolated group"}
	}
	if image.ImageMask {
		if fill, _ := p.visiblePaint(true, false); !fill {
			return nil
		}
		if err := p.validatePaint(true, false); err != nil {
			return err
		}
	}
	if p.visitor.Image == nil {
		return fmt.Errorf("image visitor missing")
	}
	image.Warning = p.visitor.Warning
	return p.visitor.Image(ImageMark{image, p.state.matrix, p.state.style})
}

// validatePaint 检查实际使用的颜色状态，避免未指定图案或未支持的色彩意图被静默替换
// 入参: fill 是否填充, stroke 是否描边
// 返回: error 颜色状态错误
func (p *pageInterpreter) validatePaint(fill, stroke bool) error {
	if fill && p.state.fillSpace == "Pattern" && p.state.style.Fill.Axial == nil && p.state.style.Fill.Radial == nil && p.state.style.Fill.Mesh == nil && p.state.style.Fill.Tiling == nil && p.state.style.Fill.Function == nil && p.state.style.Fill.Shading == nil || stroke && p.state.strokeSpace == "Pattern" && p.state.style.Stroke.Axial == nil && p.state.style.Stroke.Radial == nil && p.state.style.Stroke.Mesh == nil && p.state.style.Stroke.Tiling == nil && p.state.style.Stroke.Function == nil && p.state.style.Stroke.Shading == nil {
		return fmt.Errorf("missing pattern color")
	}
	for _, target := range []struct {
		used    bool
		profile *iccColorSpace
		paint   *Paint
		values  [4]float64
	}{{fill, p.state.fillICC, &p.state.style.Fill, p.state.fillICCValues}, {stroke, p.state.strokeICC, &p.state.style.Stroke, p.state.strokeICCValues}} {
		if !target.used || target.paint.None || target.paint.Axial != nil || target.paint.Radial != nil || target.paint.Function != nil || target.paint.Mesh != nil || target.paint.Tiling != nil || target.paint.Shading != nil {
			continue
		}
		profile := target.profile
		if profile == nil && target.paint.Space != nil {
			profile = target.paint.Space.profile
		}
		if profile == nil {
			continue
		}
		if target.profile != nil && profile.alternate != nil {
			paint, err := profile.paint(target.values[:profile.components()], p.state.style.RenderingIntent)
			if err != nil {
				return err
			}
			paint.Alpha = target.paint.Alpha
			*target.paint = paint
			continue
		}
		rgb, err := profile.color(target.paint.Values[:profile.components()], p.state.style.RenderingIntent)
		if err != nil {
			return err
		}
		target.paint.RGB = rgb
	}
	return nil
}

// form 在独立图形状态中解释表单内容及透明度组
// 入参: stream 表单内容流
// 返回: error 解析或访问错误
func (p *pageInterpreter) form(stream *Stream) error {
	if p.hidden {
		return nil
	}
	subtype, err := p.reader.Resolve(stream.Dictionary["Subtype2"])
	if err != nil {
		return err
	}
	if subtype == Name("PS") {
		return p.postscript(stream)
	}
	if p.depth >= 32 {
		return fmt.Errorf("form recursion limit exceeded")
	}
	visible, err := p.optionalVisible(stream.Dictionary["OC"])
	if err != nil || !visible {
		return err
	}
	var target *Page
	ref, err := p.reader.Resolve(stream.Dictionary["Ref"])
	if err != nil {
		return err
	}
	if ref != nil {
		reference, err := p.reader.ReadReferenceXObject(ref)
		if err != nil {
			return err
		}
		if p.visitor.Reference != nil {
			target, err = p.visitor.Reference(p.ctx, p.reader, reference)
			if err != nil {
				return err
			}
			if err := p.ctx.Err(); err != nil {
				return err
			}
		}
		if target != nil {
			if target.reader == nil || target.reader.closed {
				return fmt.Errorf("reference XObject target reader unavailable")
			}
			matches, err := target.reader.ReferenceIDMatches(reference)
			if err != nil {
				return err
			}
			if !matches && p.visitor.Warning != nil {
				p.visitor.Warning(Diagnostic{Message: "reference XObject target file identifier changed"})
			}
		}
	}
	child := *p
	child.content = nil
	if target == nil {
		resources, err := p.reader.Resolve(stream.Dictionary["Resources"])
		if err != nil {
			return err
		}
		if resources != nil {
			var ok bool
			child.resources, ok = resources.(Dictionary)
			if !ok {
				return fmt.Errorf("invalid form resources")
			}
		}
	}
	var groupMark *GroupMark
	var groupObject Object
	groupReader := p.reader
	groupResources := child.resources
	if stream.Dictionary["Group"] != nil {
		groupObject, err = p.reader.Resolve(stream.Dictionary["Group"])
		if err != nil {
			return err
		}
	}
	if groupObject == nil && target != nil {
		groupReader = target.reader
		groupResources = target.Resources
		groupObject, err = groupReader.Resolve(target.Dictionary["Group"])
		if err != nil {
			return err
		}
	}
	if groupObject != nil {
		group, ok := groupObject.(Dictionary)
		if !ok {
			return &UnsupportedError{Feature: "form transparency group"}
		}
		kind, err := groupReader.Resolve(group["S"])
		if err != nil {
			return err
		}
		if kind != Name("Transparency") {
			return &UnsupportedError{Feature: "form transparency group"}
		}
		groupMark = &GroupMark{Alpha: p.state.style.Fill.Alpha, AlphaIsShape: p.state.style.AlphaIsShape, BlendMode: p.state.style.BlendMode, SoftMask: p.state.style.SoftMask, ColorConversion: p.state.style.ColorConversion, RenderingIntent: p.state.style.RenderingIntent}
		for _, flag := range []struct {
			name   Name
			target *bool
		}{{"I", &groupMark.Isolated}, {"K", &groupMark.Knockout}} {
			value, err := groupReader.Resolve(group[flag.name])
			if err != nil {
				return err
			}
			if value == nil {
				continue
			}
			boolean, ok := value.(Boolean)
			if !ok {
				return fmt.Errorf("invalid form group flag %q", flag.name)
			}
			*flag.target = bool(boolean)
		}
		var space Object
		if groupMark.Isolated || p.maskGroup == stream {
			space, err = groupReader.Resolve(group["CS"])
			if err != nil {
				return err
			}
		}
		if space != nil {
			space, err = groupReader.resourceColorSpace(space, groupResources)
			if err != nil {
				return err
			}
			groupMark.ColorSpace, err = groupReader.readBlendingSpace(space)
			if err != nil {
				return err
			}
		}
		if p.visitor.Group == nil && groupMark.ColorSpace != nil {
			if err := groupReader.validateRGBGroupSpace(space); err != nil {
				return err
			}
		}
		if p.visitor.Group == nil && (groupMark.Knockout || groupMark.Alpha != 1 || !groupMark.Isolated || groupMark.SoftMask != nil || groupMark.BlendMode != "" && groupMark.BlendMode != "Normal" && groupMark.BlendMode != "Compatible") {
			return &UnsupportedError{Feature: "transparency group visitor missing"}
		}
		child.state.style.Fill.Alpha, child.state.style.Stroke.Alpha = 1, 1
		child.state.style.SoftMask = nil
		child.state.style.AlphaIsShape = false
		child.state.style.BlendMode = "Normal"
		child.opaqueGroup = p.visitor.Group == nil
		if groupMark.ColorSpace != nil {
			child.blendingSpace = groupMark.ColorSpace
		}
	}
	child.depth++
	child.stack = nil
	child.compatibility = 0
	child.marked = nil
	child.hidden = false
	child.path = Path{}
	child.pathPoints = nil
	child.hasPoint = false
	child.pendingClip = false
	child.inText = false
	child.textObject = nil
	value, err := p.reader.Resolve(stream.Dictionary["Matrix"])
	if err != nil {
		return err
	}
	if value != nil {
		array, ok := value.(Array)
		if !ok {
			return fmt.Errorf("invalid form matrix")
		}
		m, err := p.reader.numberArray(array, 6)
		if err != nil {
			return err
		}
		child.state.matrix = child.state.matrix.Mul(Matrix(m))
	}
	box, err := p.reader.rectangle(stream.Dictionary["BBox"])
	if err != nil {
		return err
	}
	m := child.state.matrix
	child.bounds = transformedBounds(box, m)
	if p.bounds.XMax > p.bounds.XMin && p.bounds.YMax > p.bounds.YMin {
		child.bounds.XMin, child.bounds.YMin = math.Max(child.bounds.XMin, p.bounds.XMin), math.Max(child.bounds.YMin, p.bounds.YMin)
		child.bounds.XMax, child.bounds.YMax = math.Min(child.bounds.XMax, p.bounds.XMax), math.Min(child.bounds.YMax, p.bounds.YMax)
		if child.bounds.XMax <= child.bounds.XMin || child.bounds.YMax <= child.bounds.YMin {
			return p.ctx.Err()
		}
	}
	clip := Path{Segments: []Segment{{"M", []Point{m.Apply(Point{box.XMin, box.YMin})}}, {"L", []Point{m.Apply(Point{box.XMax, box.YMin})}}, {"L", []Point{m.Apply(Point{box.XMax, box.YMax})}}, {"L", []Point{m.Apply(Point{box.XMin, box.YMax})}}, {"C", nil}}}
	child.state.style.Clips = append(append([]Path(nil), child.state.style.Clips...), clip)
	child.patternMatrix = child.state.matrix
	child.patternState = nil
	var data []byte
	if target == nil {
		data, child.content, err = p.formData(stream)
		if err != nil {
			return err
		}
		child.forms = p.forms
	} else {
		child.reader, child.resources, child.pageResources = target.reader, target.Resources, target.Resources
	}
	walk := func(interpreter *pageInterpreter) error {
		if target != nil {
			return interpreter.referencePage(target)
		}
		return interpreter.run(data)
	}
	if groupMark != nil && p.visitor.Group != nil {
		return p.visitor.Group(*groupMark, func(visitor Visitor) error {
			group := child
			if visitor.Reference == nil {
				visitor.Reference = child.visitor.Reference
			}
			if visitor.ColorantDevice == nil {
				visitor.ColorantDevice = child.visitor.ColorantDevice
			}
			if visitor.Halftones == nil {
				visitor.Halftones = child.visitor.Halftones
			}
			group.visitor = visitor
			return walk(&group)
		})
	}
	if groupMark == nil && p.visitor.Form != nil {
		return p.visitor.Form(FormMark{Bounds: box, Matrix: m}, func(visitor Visitor) error {
			form := child
			if visitor.MarkedContent == nil {
				visitor.MarkedContent = child.visitor.MarkedContent
			}
			if visitor.OptionalContent == nil {
				visitor.OptionalContent = child.visitor.OptionalContent
			}
			if visitor.Warning == nil {
				visitor.Warning = child.visitor.Warning
			}
			if visitor.Reference == nil {
				visitor.Reference = child.visitor.Reference
			}
			if visitor.ColorantDevice == nil {
				visitor.ColorantDevice = child.visitor.ColorantDevice
			}
			if visitor.Halftones == nil {
				visitor.Halftones = child.visitor.Halftones
			}
			form.visitor = visitor
			return walk(&form)
		})
	}
	return walk(&child)
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
	value, err := p.reader.Resolve(dictionary[name])
	if err != nil {
		return err
	}
	if value == nil {
		message := fmt.Sprintf("undefined ExtGState resource %s; current graphics state retained", name)
		if p.visitor.Warning == nil {
			return fmt.Errorf("%s", message)
		}
		p.visitor.Warning(Diagnostic{Offset: offset, Message: message})
		return nil
	}
	dict, ok := value.(Dictionary)
	if !ok {
		return fmt.Errorf("invalid external graphics state")
	}
	return p.applyExtState(dict)
}

// applyExtState 解释图形状态字典，保留间接空值和字段覆盖顺序
// 入参: dict 图形状态字典
// 返回: error 参数或资源错误
func (p *pageInterpreter) applyExtState(dict Dictionary) error {
	if p.uncoloredPattern || p.uncoloredType3 {
		for _, key := range []Name{"TR", "TR2", "BG", "BG2", "UCR", "UCR2", "HT"} {
			value, err := p.reader.Resolve(dict[key])
			if err != nil {
				return err
			}
			if value != nil {
				return fmt.Errorf("color graphics state in uncolored content")
			}
		}
	}
	transfer, err := p.reader.Resolve(dict["TR2"])
	if err != nil {
		return err
	}
	nonstrokingOverprint, err := p.reader.Resolve(dict["op"])
	if err != nil {
		return err
	}
	for key, value := range dict {
		switch key {
		case "Type", "TK", "HT", "HTO", "UseBlackPtComp", "D", "FL", "TR", "TR2", "Font", "RI", "ca", "CA", "LW", "LC", "LJ", "ML", "BM", "SMask", "AIS", "SA", "OP", "op", "OPM", "SM", "AAPL:AA", "BG", "BG2", "UCR", "UCR2":
		default:
			continue
		}
		if key == "TR" && transfer != nil {
			continue
		}
		if key == "BG" || key == "UCR" {
			replacement, err := p.reader.Resolve(dict[key+"2"])
			if err != nil {
				return err
			}
			if replacement != nil {
				continue
			}
		}
		value, err = p.reader.Resolve(value)
		if err != nil {
			return err
		}
		if value == nil {
			continue
		}
		switch key {
		case "Type":
			if value != Name("ExtGState") {
				return fmt.Errorf("invalid graphics state object type")
			}
		case "TK":
			flag, ok := value.(Boolean)
			if !ok || p.inText {
				return fmt.Errorf("invalid text knockout state")
			}
			p.state.nonKnockout = !bool(flag)
		case "HT":
			p.state.style.Halftone, err = p.reader.ReadHalftoneForDevice(dict[key], p.visitor.Halftones)
			if err != nil {
				return err
			}
		case "HTO":
			values, err := p.reader.numberArray(value, 2)
			if err != nil {
				return err
			}
			origin := p.state.matrix.Apply(Point{values[0], values[1]})
			if math.IsNaN(origin.X) || math.IsInf(origin.X, 0) || math.IsNaN(origin.Y) || math.IsInf(origin.Y, 0) {
				return fmt.Errorf("invalid halftone origin")
			}
			p.state.style.HalftoneOrigin = &origin
		case "D":
			values, ok := value.(Array)
			if !ok || len(values) != 2 {
				return fmt.Errorf("invalid graphics state dash pattern")
			}
			values = append(Array(nil), values...)
			for i := range values {
				values[i], err = p.reader.Resolve(values[i])
				if err != nil {
					return err
				}
			}
			if err := p.operation(Operation{Operator: "d", Operands: values}); err != nil {
				return err
			}
		case "FL":
			if err := p.operation(Operation{Operator: "i", Operands: []Object{value}}); err != nil {
				return err
			}
		case "TR", "TR2":
			if key == "TR2" && value == Name("Default") {
				p.state.style.Transfer = nil
				continue
			}
			p.state.style.Transfer, err = p.reader.ReadTransferFunction(value)
			if err != nil {
				return err
			}
		case "Font":
			array, ok := value.(Array)
			if !ok || len(array) != 2 {
				return fmt.Errorf("invalid graphics state font")
			}
			size, err := p.reader.number(array[1])
			if err != nil {
				return err
			}
			if math.IsNaN(size) || math.IsInf(size, 0) {
				return fmt.Errorf("invalid font size")
			}
			font, err := p.reader.ReadFontContext(p.ctx, array[0])
			if err != nil {
				return err
			}
			p.state.font, p.state.fontSize = font, size
		case "RI":
			intent, ok := value.(Name)
			if !ok {
				return fmt.Errorf("invalid rendering intent")
			}
			p.state.style.RenderingIntent = normalizeRenderingIntent(intent)
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
				if nonstrokingOverprint == nil {
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
		case "BG", "BG2", "UCR", "UCR2":
			var function *ColorFunction
			if (key != "BG2" && key != "UCR2") || value != Name("Default") {
				function, err = p.reader.ReadColorFunction(value)
				if err != nil {
					return err
				}
			}
			if key == "BG" || key == "BG2" {
				p.state.style.ColorConversion.BlackGeneration = function
			} else {
				p.state.style.ColorConversion.UndercolorRemoval = function
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
