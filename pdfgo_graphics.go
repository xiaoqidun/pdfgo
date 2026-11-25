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
	"io"
	"maps"
	"math"
	"os"
	"slices"
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

// Paint 保存颜色及不透明度，CMYK保留设备四色，Space与Values保留ICC源分量，SourceSpace保留原始空间族（图案用底层空间族）
// None表示丢弃着色且不参与挖空合成（有别于零Alpha），在ColorantDevice下仅描述备用显示而非原生色料可用性
// Process保留原生过程通道并区分未指定与零分量；Colorant保留专色定义与浓度快照，不随后续状态变化
// Shading保留着色图案内部状态，Alpha仍归属使用该图案的绘制对象
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
	Fill, Stroke           Paint
	LineWidth              float64
	Cap, Join              int
	MiterLimit             float64
	Dash                   []float64
	DashPhase              float64
	Clips                  []Path
	StrokeAdjust           bool
	FillOverprint          bool
	StrokeOverprint        bool
	OverprintMode          int
	RenderingIntent        Name
	BlackPointCompensation Name
	BlendMode              Name
	AlphaIsShape           bool
	SoftMask               *SoftMask
	Smoothness             *float64
	Antialias              *bool
	Halftone               *Halftone
	HalftoneOrigin         *Point
	Transfer               *TransferFunction
	ColorConversion        ColorConversion
}

// PathMark 表示一次路径绘制，路径及裁剪坐标始终位于页面用户空间
// StrokeMatrix非零时保留非等比描边变换，线宽及虚线参数属于其逆变换后的坐标空间
type PathMark struct {
	Sequence     *ContentSequence
	Path         Path
	Style        Style
	Fill, Stroke bool
	StrokeMatrix Matrix
}

// TextMark 保存文字字形及相对文字矩阵的基线位置，Positions始终使用横排原点（竖排已扣除竖排原点向量）
// StrokeMatrix保留图形状态变换且描边参数不受文字矩阵与字号影响；Mode为有效模式，Type3非模式3按0交付且不产裁剪
// Object由同一BT/ET内共享（不同文字对象独立）；Resources为所在页只读资源，供未声明资源的Type3字形查找
// Replacement指向所属替换文本区段且同区段共享实例，不改变Glyphs原始字符映射
type TextMark struct {
	Object                *TextObject
	Sequence              *ContentSequence
	Replacement           *ReplacementText
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
	page                  *Page
	depth                 int
	glyphStreams          []*Stream
	formStreams           []*Stream
	patternStreams        []*Stream
	halftones             map[string]*Halftone
	state                 *graphicsState
	maskGroup             *Stream
	blendingSpace         *ColorSpace
	opaqueGroup           bool
	patternMatrix         Matrix
	bounds                Rectangle
}

// TextObject 保留文字对象身份，Knockout为真时整段挖空，为假时逐字合成
type TextObject struct {
	Knockout bool
}

// ImageMark 保存图像资源及单位方形到页面坐标的变换
type ImageMark struct {
	Sequence *ContentSequence
	Image    *Image
	Matrix   Matrix
	Style    Style
}

// FormMark 保存表单的引用标识、局部边界、到页面坐标的变换、外层裁剪数量及可选透明组状态
type FormMark struct {
	Reference  Reference
	Stream     *Stream
	Bounds     Rectangle
	Matrix     Matrix
	OuterClips int
	Group      *GroupMark
}

// GroupMark 保存透明度组的边界不透明度和隔离方式，组内绘制使用独立状态
// Page区分页面初始组与内容流中的表单组，来自表单的透明组保留引用及局部坐标信息
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
	Reference       Reference
	Stream          *Stream
	Bounds          Rectangle
	Matrix          Matrix
	OuterClips      int
}

// MarkedContentMark 保存内容标记及其属性，结束标记沿用开始标签，Offset为当前解码流位置，Hidden表示不可见
// Sequence与含ActualText的Replacement在区段起止时共享实例（MP/DP不创建区段）；Reader用于解析Properties中的间接引用
// Page标识来源页面（独立构造输入可能为nil），Stream为nil表示页面合并内容，否则为Form/字形/图案流，均沿用阅读器生命周期
type MarkedContentMark struct {
	Reader      *Reader
	Page        *Page
	Stream      *Stream
	Sequence    *ContentSequence
	Tag         Name
	Properties  Dictionary
	Operator    string
	Offset      int64
	Replacement *ReplacementText
	Hidden      bool
}

// ContentSequence 保存绘制图元所属的只读内容区段，Parent指向外层区段，不随图形状态保存或恢复且相邻同标签区段为独立实例
// Reader用于解析Properties间接引用，来源对象沿用阅读器生命周期
// Object标识带StructParent的XObject调用或注解外观，此类区段无Tag与Properties
type ContentSequence struct {
	Reader     *Reader
	Page       *Page
	Stream     *Stream
	Object     Reference
	Tag        Name
	Properties Dictionary
	Parent     *ContentSequence
}

// ReplacementText 保存整个内容区段的替换文本，Parent指向外层替换区段
// Text允许为空，表示不提取该区段的原始字符；相邻区段之间不隐式增加空格
type ReplacementText struct {
	Text   string
	Parent *ReplacementText
}

// Visitor 按内容顺序接收页面绘制对象，缺少对应绘制回调时返回错误
// Form保留普通表单边界（缺省直接展开），透明表单交由Group；子访问器接收已变换裁剪的图元，未设标记、可选内容、PostScript或Warning时沿用上层回调
// OptionalContent覆盖可见性，Reference解析引用表单（缺省绘代理），ColorantDevice声明可用色料，Halftones提供优先匹配的只读设备命名网屏
// PostScript接收原始程序（缺省有Warning则诊断跳过，否则报错）；Warning非空时诊断无效指令、缺失资源、空Type3字形及可恢复外观，其他解析错误仍返回
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

// markedContentState 保存标记名称、外层区段、可见性及替换文本作用域
type markedContentState struct {
	tag         Name
	hidden      bool
	previous    *ReplacementText
	replacement *ReplacementText
	sequence    *ContentSequence
}

// pageInterpreter 按内容顺序解释页面或表单
type pageInterpreter struct {
	reader                 *Reader
	page                   *Page
	stream                 *Stream
	resources              Dictionary
	pageResources          Dictionary
	visitor                Visitor
	ctx                    context.Context
	state                  graphicsState
	stack                  []graphicsState
	marked                 []markedContentState
	sequence               *ContentSequence
	replacement            *ReplacementText
	hidden                 bool
	textClips              []*TextClip
	textMatrix, lineMatrix Matrix
	inText                 bool
	implicitText           bool
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
	formStreams            []*Stream
	patternStreams         []*Stream
	maskGroup              *Stream
	blendingSpace          *ColorSpace
	forms                  *formCache
	content                *formContent
}

// Identity 返回单位矩阵
// 返回: Matrix 单位矩阵
func Identity() Matrix { return Matrix{1, 0, 0, 1, 0, 0} }

// matrixFinite 判断仿射矩阵各分量是否全部为有限数值
// 入参: m 仿射矩阵
// 返回: bool 是否全部有限
func matrixFinite(m Matrix) bool {
	for _, value := range m {
		if math.IsNaN(value) || math.IsInf(value, 0) {
			return false
		}
	}
	return true
}

// rectFinite 判断矩形边界是否全部为有限数值
// 入参: box 矩形边界
// 返回: bool 是否全部有限
func rectFinite(box Rectangle) bool {
	return !math.IsNaN(box.XMin) && !math.IsInf(box.XMin, 0) && !math.IsNaN(box.YMin) && !math.IsInf(box.YMin, 0) && !math.IsNaN(box.XMax) && !math.IsInf(box.XMax, 0) && !math.IsNaN(box.YMax) && !math.IsInf(box.YMax, 0)
}

// pointFinite 判断二维坐标点各分量是否全部为有限数值
// 入参: point 坐标点
// 返回: bool 是否全部有限
func pointFinite(point Point) bool {
	return !math.IsNaN(point.X) && !math.IsInf(point.X, 0) && !math.IsNaN(point.Y) && !math.IsInf(point.Y, 0)
}

// pathFinite 判断路径全部顶点坐标是否为有限数值
// 入参: path 路径对象
// 返回: bool 是否全部有限
func pathFinite(path Path) bool {
	for _, segment := range path.Segments {
		for _, point := range segment.Points {
			if !pointFinite(point) {
				return false
			}
		}
	}
	return true
}

// resetTransient 清理拷贝自外层解释器的瞬时解析状态，不改变图形状态与递归链路
func (p *pageInterpreter) resetTransient() {
	p.stack = nil
	p.marked = nil
	p.sequence = nil
	p.replacement = nil
	p.hidden = false
	p.textClips = nil
	p.textMatrix, p.lineMatrix = Identity(), Identity()
	p.inText, p.implicitText = false, false
	p.textObject = nil
	p.path = Path{}
	p.pathPoints = nil
	p.current, p.start = Point{}, Point{}
	p.hasPoint = false
	p.pendingClip, p.clipEvenOdd = false, false
	p.compatibility = 0
	p.opaqueGroup = false
	p.uncoloredPattern = false
	p.type3, p.uncoloredType3 = false, false
	p.patternState = nil
	p.tilingPatterns = nil
	p.forms, p.content = nil, nil
}

// matrixProductDifference 补偿乘积相减的舍入残差，检查溢出、下溢及严重相消
// 入参: a 第一个乘数, b 第二个乘数, c 第三个乘数, d 第四个乘数
// 返回: float64 乘积之差, bool 是否可直接用于求逆
func matrixProductDifference(a, b, c, d float64) (float64, bool) {
	x, y := a*b, c*d
	value := x - y
	if math.IsNaN(value) || math.IsInf(value, 0) || x == 0 && a != 0 && b != 0 || y == 0 && c != 0 && d != 0 {
		return value, false
	}
	if x != 0 && y != 0 {
		value = math.FMA(a, b, -y) + math.FMA(-c, d, y)
	}
	return value, x == 0 && y == 0 || math.Abs(value) >= 0x1p-1022 && math.Abs(value) >= 0x1p-48*math.Max(math.Abs(x), math.Abs(y))
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
	d, ds := matrixProductDifference(m[0], m[3], m[1], m[2])
	x, xs := matrixProductDifference(m[2], m[5], m[3], m[4])
	y, ys := matrixProductDifference(m[1], m[4], m[0], m[5])
	if d != 0 && ds && xs && ys {
		n := Matrix{m[3] / d, -m[1] / d, -m[2] / d, m[0] / d, x / d, y / d}
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

// ForkOptions 配置工作视图的数据源、诊断回调与文字整形回调
// Source为空时沿用原阅读器数据源，调用方须在视图使用期间保持源Reader未关闭
type ForkOptions struct {
	Source    io.ReaderAt
	Warning   func(Diagnostic)
	ShapeText TextShaper
}

// Fork 创建共享只读底层数据与并发安全缓存的独立工作视图，供多页并发读取使用
// 视图固定内部图像解码并发度为1，避免外层并行调度过度订阅
// 入参: options 工作视图选项
// 返回: *Reader 独立工作视图
func (r *Reader) Fork(options ForkOptions) *Reader {
	if r == nil || r.closed {
		return &Reader{closed: true}
	}
	source := options.Source
	originalSource := r.originalSource
	if source == nil {
		source = r.source
	} else {
		originalSource = source
		header := r.originalSize - r.size
		source = io.NewSectionReader(source, header, r.size)
	}
	var security *standardSecurity
	if r.security != nil {
		copied := *r.security
		copied.key = bytes.Clone(r.security.key)
		security = &copied
	}
	owner := r.owner
	if owner == nil {
		owner = r
	}
	if r.cache != nil {
		r.cache.shared.Store(true)
	}
	return &Reader{
		Version:         r.Version,
		Trailer:         r.Trailer,
		source:          source,
		size:            r.size,
		originalSource:  originalSource,
		originalSize:    r.originalSize,
		xref:            maps.Clone(r.xref),
		cache:           r.cache,
		loading:         make(map[Reference]bool),
		deviceProfile:   r.deviceProfile,
		security:        security,
		warning:         options.Warning,
		textShaper:      options.ShapeText,
		decodeWorkers:   1,
		maxStreamBytes:  r.maxStreamBytes,
		maxFilterChain:  r.maxFilterChain,
		jbig2Globals:    r.jbig2Globals,
		owner:           owner,
		ownerHasWarning: r.rootHasWarning(),
	}
}

// BindPage 将来源同一文档的页面浅拷贝绑定到当前工作视图，不修改原页面对象
// 阅读器为空或已关闭，或页面不属于同源文档时返回nil
// 入参: page 原阅读器页面
// 返回: *Page 绑定到当前视图的页面副本
func (r *Reader) BindPage(page *Page) *Page {
	if r == nil || r.closed || page == nil {
		return nil
	}
	if page.reader != nil && !r.ownsObject(page.reader, page.cache) {
		return nil
	}
	bound := *page
	bound.reader = r
	bound.cache = r.cache
	return &bound
}

// WalkPage 解释页面内容并按绘制顺序访问可准确表达的图元，注解由Page.Annotations读取
// 入参: ctx 取消上下文, page 页面, visitor 图元访问器
// 返回: error 错误信息
func (r *Reader) WalkPage(ctx context.Context, page *Page, visitor Visitor) error {
	if r == nil || ctx == nil {
		return fmt.Errorf("invalid page context")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if r.closed {
		return os.ErrClosed
	}
	if page == nil || page.reader != r {
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
				groupSpace, err = r.resourceBlendingSpaceContext(ctx, value, page.Resources)
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
				if visitor.Warning == nil {
					return &UnsupportedError{Feature: fmt.Sprintf("page group field %q", key)}
				}
				visitor.Warning(Diagnostic{Message: fmt.Sprintf("unsupported page group field %q ignored", key)})
				if err := ctx.Err(); err != nil {
					return err
				}
			}
		}
	}
	if value, err := r.Resolve(page.Dictionary["Trans"]); err != nil {
		if visitor.Warning == nil || isFatalReaderError(err) {
			return err
		}
		visitor.Warning(Diagnostic{Message: fmt.Sprintf("invalid page transition ignored: %v", err)})
		if err := ctx.Err(); err != nil {
			return err
		}
	} else if value != nil {
		transition, ok := value.(Dictionary)
		if !ok {
			if visitor.Warning == nil {
				return fmt.Errorf("invalid page transition")
			}
			visitor.Warning(Diagnostic{Message: "invalid page transition ignored"})
			if err := ctx.Err(); err != nil {
				return err
			}
		} else {
			style, err := r.Resolve(transition["S"])
			if err != nil {
				if visitor.Warning == nil || isFatalReaderError(err) {
					return err
				}
				visitor.Warning(Diagnostic{Message: fmt.Sprintf("invalid page transition ignored: %v", err)})
				if err := ctx.Err(); err != nil {
					return err
				}
			} else if style != nil && style != Name("R") {
				if visitor.Warning == nil {
					return &UnsupportedError{Feature: "page transition " + fmt.Sprint(style)}
				}
				visitor.Warning(Diagnostic{Message: "unsupported page transition " + fmt.Sprint(style) + " ignored"})
				if err := ctx.Err(); err != nil {
					return err
				}
			}
		}
	}
	data, err := page.readContentBytes(ctx)
	if err != nil {
		return err
	}
	interpreter := pageInterpreter{reader: r, page: page, resources: page.Resources, pageResources: page.Resources, visitor: visitor, ctx: ctx, textMatrix: Identity(), lineMatrix: Identity(), bounds: page.CropBox}
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
			if v.MarkedContent == nil {
				v.MarkedContent = visitor.MarkedContent
			}
			if v.OptionalContent == nil {
				v.OptionalContent = visitor.OptionalContent
			}
			if v.Reference == nil {
				v.Reference = visitor.Reference
			}
			if v.ColorantDevice == nil {
				v.ColorantDevice = visitor.ColorantDevice
			}
			if v.Halftones == nil {
				v.Halftones = visitor.Halftones
			}
			if v.PostScript == nil {
				v.PostScript = visitor.PostScript
			}
			if v.Warning == nil {
				v.Warning = visitor.Warning
			}
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
	if ctx == nil {
		return fmt.Errorf("invalid Type3 glyph context")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if r == nil {
		return fmt.Errorf("invalid Type3 glyph reader")
	}
	if r.closed {
		return os.ErrClosed
	}
	if visitor.Halftones == nil {
		visitor.Halftones = mark.halftones
	}
	font := mark.Font
	if font == nil || font.Subtype != Name("Type3") || index < 0 || index >= len(mark.Glyphs) || index >= len(mark.Positions) {
		return fmt.Errorf("invalid Type3 glyph")
	}
	if !r.ownsObject(font.reader, font.cache) {
		r = font.reader
	}
	if r == nil {
		return fmt.Errorf("invalid Type3 glyph reader")
	}
	if r.closed {
		return os.ErrClosed
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
			message := fmt.Sprintf("recursive Type3 character procedure %q", mark.Glyphs[index].Name)
			if visitor.Warning == nil {
				return fmt.Errorf("%s", message)
			}
			visitor.Warning(Diagnostic{Message: message})
			return ctx.Err()
		}
	}
	if len(mark.glyphStreams) >= 64 || mark.depth >= 1024 {
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
	data, err := r.DecodeStreamContext(ctx, stream)
	if err != nil {
		return err
	}
	position := mark.Positions[index]
	text := Matrix{mark.Size * mark.HorizontalScale, 0, 0, mark.Size, position.X, position.Y}
	matrix := mark.Matrix.Mul(text).Mul(font.type3Matrix)
	if !matrixFinite(matrix) {
		return fmt.Errorf("nonfinite Type3 glyph matrix")
	}
	resources := font.type3Resources
	if resources == nil {
		resources = mark.Resources
	}
	interpreter := pageInterpreter{reader: r, page: mark.page, stream: stream, resources: resources, pageResources: mark.Resources, visitor: visitor, ctx: ctx, textMatrix: Identity(), lineMatrix: Identity(), depth: mark.depth + 1, type3: true, replacement: mark.Replacement, sequence: mark.Sequence}
	interpreter.glyphStreams = append(append([]*Stream(nil), mark.glyphStreams...), stream)
	interpreter.formStreams = mark.formStreams
	interpreter.patternStreams = mark.patternStreams
	interpreter.maskGroup = mark.maskGroup
	interpreter.blendingSpace = mark.blendingSpace
	interpreter.opaqueGroup = mark.opaqueGroup
	interpreter.patternMatrix = mark.patternMatrix
	if interpreter.patternMatrix == (Matrix{}) {
		interpreter.patternMatrix = Identity()
	}
	interpreter.bounds = mark.bounds
	if mark.state != nil {
		interpreter.state = *mark.state
		interpreter.state.matrix = matrix
		interpreter.state.hscale = 1
		interpreter.state.style = mark.Style
	} else {
		fillSpace, strokeSpace := Name("DeviceGray"), Name("DeviceGray")
		if mark.Style.Fill.SourceSpace == "Pattern" {
			fillSpace = "Pattern"
		}
		if mark.Style.Stroke.SourceSpace == "Pattern" {
			strokeSpace = "Pattern"
		}
		interpreter.state = graphicsState{matrix: matrix, hscale: 1, fillSpace: fillSpace, strokeSpace: strokeSpace, style: mark.Style}
	}
	interpreter.state.nonKnockout = mark.Object != nil && !mark.Object.Knockout
	if font.type3Bounds != nil {
		bounds := transformedBounds(*font.type3Bounds, interpreter.state.matrix)
		if !rectFinite(bounds) {
			return fmt.Errorf("nonfinite Type3 glyph bounds")
		}
		if bounds.XMax > bounds.XMin && bounds.YMax > bounds.YMin {
			interpreter.bounds = bounds
		}
	}
	return interpreter.run(data)
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
		if p.visitor.Warning == nil {
			return fmt.Errorf("unbalanced graphics, text or marked content state")
		}
		p.visitor.Warning(Diagnostic{Offset: int64(len(data)), Message: "unbalanced graphics, text or marked content state at content end; completed painting retained"})
		if err := p.ctx.Err(); err != nil {
			return err
		}
		if p.inText {
			if err := p.operation(Operation{Operator: "ET", Offset: int64(len(data))}); err != nil {
				return err
			}
		}
		for len(p.marked) != 0 {
			if err := p.markedContent(Operation{Operator: "EMC", Offset: int64(len(data))}); err != nil {
				return err
			}
		}
		if len(p.stack) != 0 {
			p.state = p.stack[0]
			p.stack = nil
		}
	}
	if p.compatibility != 0 {
		if p.visitor.Warning == nil {
			return fmt.Errorf("unbalanced compatibility section")
		}
		p.visitor.Warning(Diagnostic{Offset: int64(len(data)), Message: "unbalanced compatibility section"})
		p.compatibility = 0
	}
	if err := p.ctx.Err(); err != nil {
		return err
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
	mark := MarkedContentMark{Reader: p.reader, Page: p.page, Stream: p.stream, Operator: op.Operator, Offset: op.Offset, Hidden: p.hidden}
	optional := false
	if op.Operator == "EMC" {
		if len(op.Operands) != 0 || len(p.marked) == 0 {
			return p.operationWarning(op, "unmatched marked content end")
		}
		state := p.marked[len(p.marked)-1]
		mark.Tag, p.hidden = state.tag, state.hidden
		mark.Replacement, p.replacement = state.replacement, state.previous
		mark.Sequence, p.sequence = p.sequence, state.sequence
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
			if err := p.operationWarning(op, "invalid marked content tag; enclosed drawing retained"); err != nil {
				return err
			}
			if op.Operator == "BMC" || op.Operator == "BDC" {
				p.marked = append(p.marked, markedContentState{hidden: p.hidden, previous: p.replacement, sequence: p.sequence})
			}
			return p.ctx.Err()
		}
		if count == 2 {
			object := op.Operands[1]
			if name, ok := object.(Name); ok {
				value, err := p.reader.Resolve(p.resources["Properties"])
				if err != nil {
					return err
				}
				properties, ok := value.(Dictionary)
				if !ok || properties[name] == nil {
					if err := p.operationWarning(op, "missing Properties resource "+string(name)+"; enclosed drawing retained"); err != nil {
						return err
					}
					object = Dictionary{}
				} else {
					object = properties[name]
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
			if op.Operator == "BDC" && mark.Tag == "Span" {
				var err error
				mark.Replacement, err = p.replacementText(mark.Properties)
				if err != nil {
					if p.visitor.Warning == nil || p.ctx.Err() != nil {
						return err
					}
					p.visitor.Warning(Diagnostic{Offset: op.Offset, Message: "invalid PDF ActualText ignored: " + err.Error()})
				}
			}
			p.marked = append(p.marked, markedContentState{tag: mark.Tag, hidden: hidden, previous: p.replacement, replacement: mark.Replacement, sequence: p.sequence})
			p.sequence = &ContentSequence{Reader: p.reader, Page: p.page, Stream: p.stream, Tag: mark.Tag, Properties: mark.Properties, Parent: p.sequence}
			mark.Sequence = p.sequence
			if mark.Replacement != nil {
				p.replacement = mark.Replacement
			}
		}
		mark.Hidden = p.hidden
	}
	if mark.Tag == "" {
		return p.ctx.Err()
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

// operation 执行内容操作，诊断模式跳过无效状态及未知指令
// 入参: op 内容操作
// 返回: error 错误信息
func (p *pageInterpreter) operation(op Operation) error {
	a := op.Operands
	if p.uncoloredPattern {
		switch op.Operator {
		case "g", "G", "rg", "RG", "k", "K", "cs", "CS", "sc", "SC", "scn", "SCN", "sh", "ri":
			return p.operationWarning(op, "color operator in uncolored pattern")
		}
	}
	var v []float64
	if n := graphicsOperandCount(op.Operator); n >= 0 {
		if len(a) > n && p.visitor.Warning != nil {
			if err := p.operationWarning(op, "excess numeric operands; trailing operands retained"); err != nil {
				return err
			}
			a = a[len(a)-n:]
		}
		var err error
		v, err = numbers(a, n)
		if err != nil {
			return p.operationWarning(op, "invalid operands for "+op.Operator+": "+err.Error())
		}
	}
	if p.uncoloredType3 {
		switch op.Operator {
		case "g", "G", "rg", "RG", "k", "K", "cs", "CS", "sc", "SC", "scn", "SCN", "sh", "ri":
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
			return p.operationWarning(op, `unmatched operator "EX"`)
		}
		p.compatibility--
	case "d0", "d1":
		if !p.type3 {
			return &UnsupportedError{Feature: "Type3 glyph metrics outside character procedure"}
		}
		p.uncoloredType3 = op.Operator == "d1"
		if op.Operator == "d1" {
			bounds := transformedBounds(Rectangle{v[2], v[3], v[4], v[5]}, p.state.matrix)
			if !rectFinite(bounds) {
				return p.operationWarning(op, "nonfinite Type3 glyph bounds")
			}
			p.bounds = bounds
		}
	case "q":
		if len(p.stack) >= 256 {
			return fmt.Errorf("graphics stack limit exceeded")
		}
		p.stack = append(p.stack, p.state)
	case "Q":
		if len(p.stack) == 0 {
			return p.operationWarning(op, "unmatched operator Q")
		}
		p.state = p.stack[len(p.stack)-1]
		p.stack = p.stack[:len(p.stack)-1]
	case "cm":
		next := p.state.matrix.Mul(Matrix(v))
		if !matrixFinite(next) {
			return p.operationWarning(op, "nonfinite transform matrix")
		}
		p.state.matrix = next
	case "w":
		if v[0] < 0 {
			if err := p.operationWarning(op, "negative line width uses hairline default"); err != nil {
				return err
			}
			v[0] = 0
		}
		p.state.style.LineWidth = v[0]
	case "J", "j":
		if v[0] < 0 || v[0] > 2 || v[0] != math.Trunc(v[0]) {
			return p.operationWarning(op, "invalid line style ignored")
		}
		if op.Operator == "J" {
			p.state.style.Cap = int(v[0])
		} else {
			p.state.style.Join = int(v[0])
		}
	case "M":
		if v[0] < 1 {
			if err := p.operationWarning(op, "miter limit below PDF minimum"); err != nil {
				return err
			}
			if v[0] <= 0 {
				return nil
			}
		}
		p.state.style.MiterLimit = v[0]
	case "d":
		if len(a) != 2 {
			if p.visitor.Warning == nil {
				return fmt.Errorf("invalid dash operands")
			}
			p.state.style.Dash, p.state.style.DashPhase = nil, 0
			return p.operationWarning(op, "invalid dash operands; solid line default used")
		}
		values, ok := a[0].(Array)
		if !ok {
			if p.visitor.Warning == nil {
				return fmt.Errorf("invalid dash pattern")
			}
			p.state.style.Dash, p.state.style.DashPhase = nil, 0
			return p.operationWarning(op, "invalid dash pattern; solid line default used")
		}
		dash, err := numbers(values, len(values))
		if err != nil {
			if p.visitor.Warning == nil {
				return err
			}
			p.state.style.Dash, p.state.style.DashPhase = nil, 0
			return p.operationWarning(op, "invalid dash pattern; solid line default used")
		}
		sum := 0.0
		for _, n := range dash {
			if n < 0 {
				if p.visitor.Warning == nil {
					return fmt.Errorf("negative dash")
				}
				p.state.style.Dash, p.state.style.DashPhase = nil, 0
				return p.operationWarning(op, "negative dash; solid line default used")
			}
			sum += n
		}
		if len(dash) > 0 && sum == 0 {
			if p.visitor.Warning == nil {
				return fmt.Errorf("empty dash cycle")
			}
			p.state.style.Dash, p.state.style.DashPhase = nil, 0
			return p.operationWarning(op, "empty dash cycle; solid line default used")
		}
		phase, err := numbers(a[1:], 1)
		if err != nil {
			if p.visitor.Warning == nil {
				return err
			}
			p.state.style.Dash, p.state.style.DashPhase = nil, 0
			return p.operationWarning(op, "invalid dash phase; solid line default used")
		}
		p.state.style.Dash = dash
		p.state.style.DashPhase = phase[0]
	case "m":
		pt := point(v[0], v[1])
		if !pointFinite(pt) {
			return p.operationWarning(op, "nonfinite path coordinate")
		}
		p.current = pt
		p.start = pt
		p.hasPoint = true
		add("M", pt)
	case "l":
		if !p.hasPoint {
			return p.operationWarning(op, "line without current point")
		}
		pt := point(v[0], v[1])
		if !pointFinite(pt) {
			return p.operationWarning(op, "nonfinite path coordinate")
		}
		p.current = pt
		add("L", pt)
	case "c", "v", "y":
		if !p.hasPoint {
			return p.operationWarning(op, "curve without current point")
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
		if !pointFinite(c1) || !pointFinite(c2) || !pointFinite(end) {
			return p.operationWarning(op, "nonfinite path coordinate")
		}
		add("B", c1, c2, end)
		p.current = end
	case "h":
		if !p.hasPoint {
			return p.operationWarning(op, "close without current point")
		}
		add("C")
		p.current = p.start
	case "re":
		x2, y2 := v[0]+v[2], v[1]+v[3]
		p0, p1, p2, p3 := point(v[0], v[1]), point(x2, v[1]), point(x2, y2), point(v[0], y2)
		if math.IsNaN(x2) || math.IsInf(x2, 0) || math.IsNaN(y2) || math.IsInf(y2, 0) || !pointFinite(p0) || !pointFinite(p1) || !pointFinite(p2) || !pointFinite(p3) {
			return p.operationWarning(op, "nonfinite path coordinate")
		}
		p.start = p0
		p.current = p0
		p.hasPoint = true
		add("M", p0)
		add("L", p1)
		add("L", p2)
		add("L", p3)
		add("C")
	case "W", "W*":
		p.pendingClip = true
		p.clipEvenOdd = op.Operator == "W*"
	case "S", "s", "f", "F", "f*", "B", "B*", "b", "b*", "n":
		if (op.Operator == "s" || op.Operator == "b" || op.Operator == "b*") && p.hasPoint {
			add("C")
		}
		fill := op.Operator == "f" || op.Operator == "F" || op.Operator == "f*" || op.Operator == "B" || op.Operator == "B*" || op.Operator == "b" || op.Operator == "b*"
		stroke := op.Operator == "S" || op.Operator == "s" || op.Operator == "B" || op.Operator == "B*" || op.Operator == "b" || op.Operator == "b*"
		p.path.EvenOdd = op.Operator == "f*" || op.Operator == "B*" || op.Operator == "b*"
		fill, stroke = p.visiblePaint(fill, stroke)
		if (fill || stroke) && len(p.path.Segments) != 0 && !p.hasPoint {
			if p.visitor.Warning == nil {
				return fmt.Errorf("empty painted path")
			}
			p.visitor.Warning(Diagnostic{Offset: op.Offset, Message: "empty painted path ignored"})
			if err := p.ctx.Err(); err != nil {
				return err
			}
			fill, stroke = false, false
		}
		if (fill || stroke) && len(p.path.Segments) > 0 {
			if err := p.validatePaint(fill, stroke); err != nil {
				return err
			}
			style := p.state.style
			var strokeMatrix Matrix
			if stroke {
				m := p.state.matrix
				sx, sy := math.Hypot(m[0], m[1]), math.Hypot(m[2], m[3])
				uniform := sx > 0 && sy > 0 && !math.IsNaN(sx) && !math.IsInf(sx, 0) && !math.IsNaN(sy) && !math.IsInf(sy, 0)
				if uniform {
					cos := math.Abs((m[0]/sx)*(m[2]/sy) + (m[1]/sx)*(m[3]/sy))
					uniform = cos <= 1e-8 && math.Abs(sx-sy) <= 1e-8*math.Max(sx, sy)
				}
				if uniform {
					lineWidth, dashPhase := style.LineWidth*sx, style.DashPhase*sx
					var scaledDash []float64
					finite := !math.IsNaN(lineWidth) && !math.IsInf(lineWidth, 0) && !math.IsNaN(dashPhase) && !math.IsInf(dashPhase, 0)
					if finite && len(style.Dash) != 0 {
						scaledDash = make([]float64, len(style.Dash))
						for n, value := range style.Dash {
							scaledDash[n] = value * sx
							if math.IsNaN(scaledDash[n]) || math.IsInf(scaledDash[n], 0) {
								finite = false
								break
							}
						}
					}
					if finite {
						style.LineWidth, style.Dash, style.DashPhase = lineWidth, scaledDash, dashPhase
					} else {
						uniform = false
					}
				}
				if !uniform {
					strokeMatrix = Matrix{m[0], m[1], m[2], m[3], 0, 0}
					if _, ok := strokeMatrix.Inverse(); !ok {
						if p.visitor.Warning == nil {
							return &UnsupportedError{Feature: "singular path stroke"}
						}
						p.visitor.Warning(Diagnostic{Offset: op.Offset, Message: "singular path stroke ignored"})
						if err := p.ctx.Err(); err != nil {
							return err
						}
						stroke = false
						strokeMatrix = Matrix{}
					}
				}
			}
			if fill || stroke {
				if p.visitor.Path == nil {
					return fmt.Errorf("path visitor missing")
				}
				if err := p.visitor.Path(PathMark{Sequence: p.sequence, Path: p.path, Style: style, Fill: fill, Stroke: stroke, StrokeMatrix: strokeMatrix}); err != nil {
					return err
				}
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
		if p.resources["ColorSpace"] == nil && p.reader.deviceProfile == nil {
			return p.setDeviceColor(v, name, fill)
		}
		object, err := p.reader.resourceColorSpaceContext(p.ctx, name, p.resources)
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
		original, err := p.reader.remapColorSpaceContext(p.ctx, name, p.resources, false, 0)
		if err != nil {
			return err
		}
		object, err := p.reader.resourceColorSpaceContext(p.ctx, original, p.resources)
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
			pattern, err := p.tilingPattern(name)
			if err != nil {
				return err
			}
			if patternBase != nil && (pattern == nil || pattern.PaintType == 1) {
				if err := p.operationWarning(op, "colored pattern does not use its declared base color space"); err != nil {
					return err
				}
				patternBase = nil
				a = a[len(a)-1:]
			}
			if patternBase != nil {
				if err := patternBaseColor(&paint, patternBase, a[:len(a)-1], p.state.style.RenderingIntent); err != nil {
					return err
				}
			} else if len(a) != 1 {
				return fmt.Errorf("invalid colored pattern operands")
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
		count := 1
		if space == "DeviceRGB" {
			count = 3
		} else if space == "DeviceCMYK" {
			count = 4
		}
		if _, err := numbers(a, count); err != nil {
			return p.operationWarning(op, "invalid color operands: "+err.Error())
		}
		return p.deviceColor(a, operator == "g")
	case "BT":
		if p.inText {
			if !p.implicitText {
				return fmt.Errorf("nested text object")
			}
			if err := p.operation(Operation{Operator: "ET"}); err != nil {
				return err
			}
		}
		p.inText = true
		p.textObject = &TextObject{Knockout: !p.state.nonKnockout}
		p.textClips = nil
		p.textMatrix = Identity()
		p.lineMatrix = Identity()
	case "ET":
		if !p.inText {
			return p.operationWarning(op, "unmatched operator ET")
		}
		p.inText = false
		p.implicitText = false
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
			return p.operationWarning(op, "invalid text rendering mode")
		}
		p.state.mode = int(v[0])
	case "Ts":
		p.state.rise = v[0]
	case "Tm":
		p.textMatrix = Matrix(v)
		p.lineMatrix = p.textMatrix
	case "Td", "TD":
		next := p.lineMatrix.Mul(Matrix{1, 0, 0, 1, v[0], v[1]})
		if !matrixFinite(next) {
			return p.operationWarning(op, "nonfinite text matrix")
		}
		if op.Operator == "TD" {
			p.state.leading = -v[1]
		}
		p.lineMatrix = next
		p.textMatrix = next
	case "T*":
		next := p.lineMatrix.Mul(Matrix{1, 0, 0, 1, 0, -p.state.leading})
		if !matrixFinite(next) {
			return p.operationWarning(op, "nonfinite text matrix")
		}
		p.lineMatrix = next
		p.textMatrix = next
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
				if err := p.textState(); err != nil {
					return err
				}
				shift := Matrix{1, 0, 0, 1, 0, 0}
				if p.state.font.Vertical {
					shift[5] = -adjustment[0] / 1000 * p.state.fontSize
				} else {
					shift[4] = -adjustment[0] / 1000 * p.state.fontSize * p.state.hscale
				}
				next := p.textMatrix.Mul(shift)
				if !matrixFinite(next) {
					return p.operationWarning(op, "nonfinite text matrix")
				}
				p.textMatrix = next
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
			if p.visitor.Warning == nil {
				return err
			}
			return p.operationWarning(op, "invalid flatness ignored")
		}
		if n[0] < 0 || n[0] > 100 {
			if p.visitor.Warning == nil {
				return fmt.Errorf("invalid flatness")
			}
			return p.operationWarning(op, "flatness out of range clamped")
		}
	case "BMC", "BDC", "EMC", "MP", "DP":
		return p.markedContent(op)
	default:
		if p.compatibility != 0 {
			return nil
		}
		if p.visitor.Warning != nil {
			return p.operationWarning(op, "unsupported content operator "+op.Operator)
		}
		return &UnsupportedError{Feature: "content operator " + op.Operator}
	}
	return nil
}

// operationWarning 发送内容诊断，严格模式返回错误
// 入参: op 原始指令, message 诊断原因
// 返回: error 严格校验或取消错误
func (p *pageInterpreter) operationWarning(op Operation, message string) error {
	if err := p.ctx.Err(); err != nil {
		return err
	}
	if p.visitor.Warning == nil {
		return fmt.Errorf("%s", message)
	}
	p.visitor.Warning(Diagnostic{Offset: op.Offset, Message: message})
	return p.ctx.Err()
}

// textState 验证活动字体，诊断模式为缺失文字对象保留已有文字矩阵
// 返回: error 字体缺失、严格校验或取消错误
func (p *pageInterpreter) textState() error {
	if p.state.font == nil {
		return fmt.Errorf("text without active font")
	}
	if !p.inText {
		if err := p.operationWarning(Operation{Operator: "Tj"}, "text outside text object; existing text matrix retained"); err != nil {
			return err
		}
		p.inText, p.implicitText = true, true
		p.textObject = &TextObject{Knockout: !p.state.nonKnockout}
	}
	if p.textObject == nil || p.textObject.Knockout == p.state.nonKnockout {
		p.textObject = &TextObject{Knockout: !p.state.nonKnockout}
	}
	return p.ctx.Err()
}

// showText 保留逐字定位并更新文字矩阵
// 入参: data 编码后的文字字节
// 返回: error 错误信息
func (p *pageInterpreter) showText(data []byte) error {
	if err := p.textState(); err != nil {
		return err
	}
	mode := p.state.mode
	if p.state.font.Subtype == "Type3" {
		if p.hidden {
			mode = 3
		} else if mode != 3 {
			fill, _ := p.visiblePaint(true, false)
			if err := p.validatePaint(fill, false); err != nil {
				return err
			}
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
		if !pointFinite(positions[n]) || !pointFinite(advance) {
			return p.operationWarning(Operation{Operator: "Tj"}, "nonfinite text position")
		}
	}
	if len(glyphs) > 0 && (!p.hidden || mode >= 4) {
		if err := p.ctx.Err(); err != nil {
			return err
		}
		if p.visitor.Text == nil {
			return fmt.Errorf("text visitor missing")
		}
		combined := p.state.matrix.Mul(p.textMatrix)
		if !matrixFinite(combined) {
			return p.operationWarning(Operation{Operator: "Tj"}, "nonfinite text matrix")
		}
		scaledSize := p.state.fontSize * p.state.hscale
		if math.IsNaN(scaledSize) || math.IsInf(scaledSize, 0) {
			return p.operationWarning(Operation{Operator: "Tj"}, "nonfinite text position")
		}
		for _, pos := range positions {
			if !pointFinite(combined.Apply(pos)) {
				return p.operationWarning(Operation{Operator: "Tj"}, "nonfinite text position")
			}
		}
		mark := TextMark{Object: p.textObject, Replacement: p.replacement, Font: p.state.font, Glyphs: glyphs, Positions: positions, Matrix: combined, StrokeMatrix: p.state.matrix, Size: p.state.fontSize, HorizontalScale: p.state.hscale, Style: p.state.style, Mode: mode}
		mark.Sequence = p.sequence
		if mark.Font.Subtype == "Type3" {
			mark.page = p.page
			mark.Resources = p.pageResources
			stateCopy := p.state
			mark.state = &stateCopy
			mark.maskGroup = p.maskGroup
			mark.blendingSpace = p.blendingSpace
			mark.opaqueGroup = p.opaqueGroup
			mark.patternMatrix = p.patternMatrix
			mark.bounds = p.bounds
		}
		mark.depth = p.depth
		mark.glyphStreams = p.glyphStreams
		mark.formStreams = p.formStreams
		mark.patternStreams = p.patternStreams
		mark.halftones = p.visitor.Halftones
		if mark.Mode >= 4 {
			mark.Clip = &TextClip{Font: mark.Font, Glyphs: glyphs, Positions: positions, Matrix: mark.Matrix, Size: mark.Size, HorizontalScale: mark.HorizontalScale}
			p.textClips = append(p.textClips, mark.Clip)
		}
		if err := p.visitor.Text(mark); err != nil {
			return err
		}
	}
	next := p.textMatrix.Mul(Matrix{1, 0, 0, 1, advance.X, advance.Y})
	if !matrixFinite(next) {
		return p.operationWarning(Operation{Operator: "Tj"}, "nonfinite text matrix")
	}
	p.textMatrix = next
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
	resources, err := p.reader.Resolve(p.resources["XObject"])
	if err != nil {
		return err
	}
	dict, ok := resources.(Dictionary)
	if !ok || dict[name] == nil {
		return p.operationWarning(Operation{Operator: "Do"}, "missing XObject resource "+string(name))
	}
	object := dict[name]
	value, err := p.reader.Resolve(object)
	if err != nil {
		return err
	}
	stream, ok := value.(*Stream)
	if !ok || stream == nil {
		return p.operationWarning(Operation{Operator: "Do"}, "invalid XObject stream "+string(name))
	}
	kind, err := p.reader.Resolve(stream.Dictionary["Subtype"])
	if err != nil {
		return err
	}
	if ref, ok := object.(Reference); ok && stream.Dictionary["StructParent"] != nil {
		previous := p.sequence
		p.sequence = &ContentSequence{Reader: p.reader, Page: p.page, Stream: stream, Object: ref, Parent: previous}
		defer func() { p.sequence = previous }()
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
	formRef, _ := object.(Reference)
	return p.formContentRef(stream, stream, formRef)
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
	style := p.state.style
	if style.SoftMask != nil {
		soft, err := image.HasSoftMask()
		if err != nil {
			return err
		}
		if soft {
			style.SoftMask = nil
		}
	}
	image.Warning = p.visitor.Warning
	if err := p.recoverLZWImage(image); err != nil {
		return err
	}
	return p.visitor.Image(ImageMark{Sequence: p.sequence, Image: image, Matrix: p.state.matrix, Style: style})
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
	return p.formContent(stream, stream)
}

// formContent 解释表单绘制数据，保留原始流作为内容来源
// 入参: stream 绘制流, source 来源流
// 返回: error 解析或访问错误
func (p *pageInterpreter) formContent(stream, source *Stream) error {
	return p.formContentRef(stream, source, Reference{})
}

// formContentRef 解释表单绘制数据并保留间接对象引用及外层裁剪计数
// 入参: stream 绘制流, source 来源流, formRef 表单间接引用
// 返回: error 解析或访问错误
func (p *pageInterpreter) formContentRef(stream, source *Stream, formRef Reference) error {
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
	for _, ancestor := range p.formStreams {
		if ancestor == source {
			return p.operationWarning(Operation{Operator: "Do"}, "form recursion detected")
		}
	}
	if p.depth >= 1024 {
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
	child.formStreams = append(append([]*Stream(nil), p.formStreams...), source)
	child.stream = source
	child.content = nil
	if target == nil {
		resources, err := p.reader.Resolve(stream.Dictionary["Resources"])
		if err != nil {
			return err
		}
		if resources != nil {
			dict, ok := resources.(Dictionary)
			if !ok {
				if err := p.operationWarning(Operation{Operator: "Do"}, "invalid form resources; enclosing resources retained"); err != nil {
					return err
				}
			} else {
				child.resources = dict
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
		if p.maskGroup == stream {
			groupMark.Isolated = true
		}
		space, err := groupReader.Resolve(group["CS"])
		if err != nil {
			return err
		}
		if space != nil {
			space, err = groupReader.resourceColorSpaceContext(p.ctx, space, groupResources)
			if err != nil {
				return err
			}
			groupMark.ColorSpace, err = groupReader.readBlendingSpaceContext(p.ctx, space)
			if err != nil {
				return err
			}
		}
		if p.visitor.Group == nil && groupMark.ColorSpace != nil {
			if err := groupReader.validateRGBGroupSpaceContext(p.ctx, space); err != nil {
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
	child.implicitText = false
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
		if !matrixFinite(child.state.matrix) {
			return fmt.Errorf("nonfinite form matrix")
		}
	}
	box, err := p.reader.rectangle(stream.Dictionary["BBox"])
	if err != nil {
		return err
	}
	m := child.state.matrix
	child.bounds = transformedBounds(box, m)
	if !rectFinite(child.bounds) {
		return fmt.Errorf("nonfinite form bounds")
	}
	if p.bounds.XMax > p.bounds.XMin && p.bounds.YMax > p.bounds.YMin {
		child.bounds.XMin, child.bounds.YMin = math.Max(child.bounds.XMin, p.bounds.XMin), math.Max(child.bounds.YMin, p.bounds.YMin)
		child.bounds.XMax, child.bounds.YMax = math.Min(child.bounds.XMax, p.bounds.XMax), math.Min(child.bounds.YMax, p.bounds.YMax)
		if !rectFinite(child.bounds) || child.bounds.XMax <= child.bounds.XMin || child.bounds.YMax <= child.bounds.YMin {
			return p.ctx.Err()
		}
	}
	outerClips := len(child.state.style.Clips)
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
		child.page, child.stream = target, nil
	}
	walk := func(interpreter *pageInterpreter) error {
		if target != nil {
			return interpreter.referencePage(target)
		}
		return interpreter.run(data)
	}
	if groupMark != nil && p.visitor.Group != nil {
		mark := *groupMark
		if target == nil {
			mark.Reference = formRef
			mark.Stream = source
		}
		mark.Bounds = box
		mark.Matrix = m
		mark.OuterClips = outerClips
		return p.visitor.Group(mark, func(visitor Visitor) error {
			group := child
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
			if visitor.PostScript == nil {
				visitor.PostScript = child.visitor.PostScript
			}
			group.visitor = visitor
			return walk(&group)
		})
	}
	if groupMark == nil && p.visitor.Form != nil {
		mark := FormMark{Bounds: box, Matrix: m, OuterClips: outerClips}
		if target == nil {
			mark.Reference = formRef
			mark.Stream = source
		}
		return p.visitor.Form(mark, func(visitor Visitor) error {
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
			if visitor.PostScript == nil {
				visitor.PostScript = child.visitor.PostScript
			}
			form.visitor = visitor
			return walk(&form)
		})
	}
	return walk(&child)
}

// extState 读取已支持的外部图形状态，未列出的私有或扩展键按规范忽略
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
	for _, key := range slices.Sorted(maps.Keys(dict)) {
		value := dict[key]
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
				if err := p.operationWarning(Operation{Operator: "gs"}, "invalid graphics state object type; state entries retained"); err != nil {
					return err
				}
			}
		case "TK":
			flag, ok := value.(Boolean)
			if !ok || p.inText && p.visitor.Warning == nil {
				return fmt.Errorf("invalid text knockout state")
			}
			if p.inText {
				if err := p.operationWarning(Operation{Operator: "gs"}, "invalid text knockout state inside text object; subsequent text uses updated state"); err != nil {
					return err
				}
			}
			p.state.nonKnockout = !bool(flag)
		case "HT":
			p.state.style.Halftone, err = p.displayHalftone(dict[key])
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
		case "UseBlackPtComp":
			mode, ok := value.(Name)
			if !ok || mode != "ON" && mode != "OFF" && mode != "Default" {
				return fmt.Errorf("invalid black point compensation")
			}
			p.state.style.BlackPointCompensation = mode
		case "D":
			values, ok := value.(Array)
			if !ok || len(values) != 2 {
				if p.visitor.Warning == nil {
					return fmt.Errorf("invalid graphics state dash pattern")
				}
				p.state.style.Dash, p.state.style.DashPhase = nil, 0
				if err := p.operationWarning(Operation{Operator: "gs"}, "invalid graphics state dash pattern; solid line default used"); err != nil {
					return err
				}
				continue
			}
			values = append(Array(nil), values...)
			for i := range values {
				values[i], err = p.reader.Resolve(values[i])
				if err != nil {
					return err
				}
			}
			if arr, ok := values[0].(Array); ok {
				resolved := make(Array, len(arr))
				for i := range arr {
					resolved[i], err = p.reader.Resolve(arr[i])
					if err != nil {
						return err
					}
				}
				values[0] = resolved
			}
			if err := p.operation(Operation{Operator: "d", Operands: values}); err != nil {
				return err
			}
		case "FL":
			if err := p.operation(Operation{Operator: "i", Operands: []Object{value}}); err != nil {
				return err
			}
		case "TR", "TR2":
			if value == Name("Default") {
				if key == "TR" {
					if err := p.operationWarning(Operation{Operator: "gs"}, "TR Default uses device transfer default"); err != nil {
						return err
					}
				}
				p.state.style.Transfer = nil
				continue
			}
			p.state.style.Transfer, err = p.reader.readTransferFunctionContext(p.ctx, value)
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
				if err := p.operationWarning(Operation{Operator: "gs"}, "invalid alpha; current opacity retained"); err != nil {
					return err
				}
				continue
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
			if err != nil {
				if p.visitor.Warning == nil {
					return fmt.Errorf("invalid smoothness tolerance")
				}
				if err := p.operationWarning(Operation{Operator: "gs"}, "invalid smoothness tolerance ignored"); err != nil {
					return err
				}
				continue
			}
			if n[0] < 0 || n[0] > 1 {
				if p.visitor.Warning == nil {
					return fmt.Errorf("invalid smoothness tolerance")
				}
				if err := p.operationWarning(Operation{Operator: "gs"}, "smoothness tolerance out of range clamped"); err != nil {
					return err
				}
				clamped := math.Max(0, math.Min(1, n[0]))
				p.state.style.Smoothness = &clamped
				continue
			}
			p.state.style.Smoothness = &n[0]
		case "AAPL:AA":
			flag, ok := value.(Boolean)
			if !ok {
				if p.visitor.Warning == nil {
					return fmt.Errorf("invalid antialias flag")
				}
				if err := p.operationWarning(Operation{Operator: "gs"}, "invalid antialias flag ignored"); err != nil {
					return err
				}
				continue
			}
			v := bool(flag)
			p.state.style.Antialias = &v
		case "BG", "BG2", "UCR", "UCR2":
			var function *ColorFunction
			if value == Name("Identity") {
				if err := p.operationWarning(Operation{Operator: "gs"}, "named identity color function uses device default"); err != nil {
					return err
				}
			} else if (key != "BG2" && key != "UCR2") || value != Name("Default") {
				function, err = p.reader.readColorFunctionContext(p.ctx, value)
				if err != nil {
					return err
				}
			}
			if key == "BG" || key == "BG2" {
				p.state.style.ColorConversion.BlackGeneration = function
			} else {
				p.state.style.ColorConversion.UndercolorRemoval = function
			}
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
