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

// ShadingPattern 保留着色图案的源画刷和内部状态，遍历时生成非隔离挖空组
// Paint及Style只读，Background使用原始背景色，不经过渐变函数
type ShadingPattern struct {
	Paint      Paint
	Style      Style
	Background *Paint
}

// Walk 在页面坐标范围内遍历背景和着色，不应用外层对象的几何或透明度
// 入参: ctx 取消上下文, bounds 求值范围, visitor 组及路径访问器
// 返回: error 参数、访问或取消错误
func (p *ShadingPattern) Walk(ctx context.Context, bounds Rectangle, visitor Visitor) error {
	if ctx == nil || p == nil || p.Paint.Shading != nil {
		return fmt.Errorf("invalid shading pattern evaluation")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	for _, value := range [4]float64{bounds.XMin, bounds.YMin, bounds.XMax, bounds.YMax} {
		if math.IsNaN(value) || math.IsInf(value, 0) {
			return fmt.Errorf("invalid shading pattern bounds")
		}
	}
	if bounds.XMax <= bounds.XMin || bounds.YMax <= bounds.YMin {
		return fmt.Errorf("invalid shading pattern bounds")
	}
	if visitor.Group == nil {
		return &UnsupportedError{Feature: "shading pattern group visitor missing"}
	}
	paint, clip, background, err := p.source()
	if err != nil {
		return err
	}
	style := p.Style
	if clip != nil {
		style.Clips = append(append([]Path(nil), style.Clips...), *clip)
	}
	paint.Alpha = style.Fill.Alpha
	path := shadingRectangle(bounds, Identity())
	return visitor.Group(GroupMark{Alpha: 1, Knockout: true, BlendMode: "Normal"}, func(v Visitor) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if v.Path == nil {
			return fmt.Errorf("shading pattern path visitor missing")
		}
		if background != nil {
			bg := *background
			bg.Alpha = style.Fill.Alpha
			initial := style
			initial.Fill = bg
			if err := v.Path(PathMark{Path: path, Style: initial, Fill: true}); err != nil {
				return err
			}
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		shading := style
		shading.Fill = paint
		if err := v.Path(PathMark{Path: path, Style: shading, Fill: true}); err != nil {
			return err
		}
		return ctx.Err()
	})
}

// source 分离渐变的背景及边界，不修改共享的源画刷
// 返回: Paint 原始着色, *Path 独立边界, *Paint 背景, error 无效画刷
func (p *ShadingPattern) source() (Paint, *Path, *Paint, error) {
	paint := p.Paint
	var background *[4]float64
	var bounds *Rectangle
	var matrix Matrix
	var space *ColorSpace
	switch {
	case paint.Axial != nil:
		g := *paint.Axial
		background, bounds, matrix, space = g.Background, g.Bounds, g.Matrix, g.Space
		g.Background, g.Bounds = nil, nil
		paint.Axial = &g
	case paint.Radial != nil:
		g := *paint.Radial
		background, bounds, matrix, space = g.Background, g.Bounds, g.Matrix, g.Space
		g.Background, g.Bounds = nil, nil
		paint.Radial = &g
	case paint.Function != nil:
		g := *paint.Function
		background, bounds, matrix, space = g.Background, g.Bounds, g.PatternMatrix, g.Space
		g.Background, g.Bounds = nil, nil
		paint.Function = &g
	case paint.Mesh != nil:
		g := *paint.Mesh
		background, bounds, matrix, space = g.Background, g.Bounds, g.Matrix, g.Space
		g.Background, g.Bounds = nil, nil
		paint.Mesh = &g
	default:
		return Paint{}, nil, nil, fmt.Errorf("invalid shading pattern paint")
	}
	var clip *Path
	if bounds != nil {
		if matrix == (Matrix{}) {
			matrix = Identity()
		}
		path := shadingRectangle(*bounds, matrix)
		if !pathFinite(path) {
			return Paint{}, nil, nil, fmt.Errorf("nonfinite shading pattern bounds")
		}
		clip = &path
	}
	bg := p.Background
	if bg == nil && background != nil {
		bg = &Paint{Space: space, Values: *background, Alpha: 1}
	}
	return paint, clip, bg, nil
}

// shadingPatternKey 标识当前内容流作用域内可复用的着色图案结果
type shadingPatternKey struct {
	ref      Reference
	name     Name
	matrix   Matrix
	blending *ColorSpace
}

// shadingStreamKey 标识当前内容流作用域内可复用的网格着色流结果
type shadingStreamKey struct {
	stream   *Stream
	matrix   Matrix
	intent   Name
	blending *ColorSpace
}

// shadingPatternCache 有界缓存着色图案与底层网格着色结果，超大结果通过单槽复用避免重复分配
type shadingPatternCache struct {
	byPattern  map[shadingPatternKey]Paint
	byStream   map[shadingStreamKey]Paint
	lastKey    shadingPatternKey
	lastPaint  Paint
	hasLast    bool
	lastStream shadingStreamKey
	lastMesh   Paint
	hasMesh    bool
	bytes      int64
}

// shadingByteTracker 按节点指针去重统计着色画刷及其关联状态的子对象内存
type shadingByteTracker struct {
	functions   map[*gradientFunction]bool
	vectors     map[*gradientVector]bool
	tints       map[*deviceNSpace]bool
	separations map[*separationSpace]bool
	colorants   map[*ColorantSpace]bool
	alternates  map[*tintAlternate]bool
	profiles    map[*iccColorSpace]bool
	halftones   map[*Halftone]bool
	palettes    map[*imagePalette]bool
	sampled     map[*sampledFunction]bool
	programs    map[*calculatorFunction]bool
}

// shadingPaintBytes 估算着色画刷及其底层函数、专色、色彩配置和图案状态的完整内存占用
// 入参: paint 着色画刷
// 返回: int64 估算字节数
func shadingPaintBytes(paint Paint) int64 {
	return shadingPaintBytesWith(paint, &shadingByteTracker{})
}

// shadingPaintBytesWith 按去重表递归估算着色画刷内存占用
// 入参: paint 着色画刷, seen 节点去重表
// 返回: int64 估算字节数
func shadingPaintBytesWith(paint Paint, seen *shadingByteTracker) int64 {
	size := colorSpaceMemoryBytes(paint.Space, seen) + colorantPaintBytes(paint.Colorant, seen)
	if paint.Process != nil {
		size += colorSpaceMemoryBytes(paint.Process.Space, seen) + int64(len(paint.Process.Channels))*8
	}
	if paint.Shading != nil {
		size += 256 + shadingPaintBytesWith(paint.Shading.Paint, seen) + shadingStyleBytes(paint.Shading.Style, seen) + backgroundPaintBytes(paint.Shading.Background, seen)
		return size
	}
	if m := paint.Mesh; m != nil {
		size += 256
		if len(m.Triangles) > 0 {
			size += int64(len(m.Triangles)) * (144 + int64(len(m.Triangles[0].Colors[0]))*24)
		}
		if len(m.Patches) > 0 {
			size += int64(len(m.Patches)) * (400 + int64(len(m.Patches[0].Colors[0]))*32)
		}
		return size + gradientVectorMemoryBytes(m.function, seen) + tintMemoryBytes(m.tint, seen) + colorSpaceMemoryBytes(m.Space, seen)
	}
	if a := paint.Axial; a != nil {
		return size + 256 + int64(len(a.Stops))*64 + gradientFunctionMemoryBytes(a.function, seen) + colorSpaceMemoryBytes(a.Space, seen)
	}
	if r := paint.Radial; r != nil {
		return size + 256 + int64(len(r.Stops))*64 + gradientFunctionMemoryBytes(r.function, seen) + colorSpaceMemoryBytes(r.Space, seen)
	}
	if f := paint.Function; f != nil {
		return size + 256 + coordinateFunctionBytes(f.function) + tintMemoryBytes(f.tint, seen) + colorSpaceMemoryBytes(f.Space, seen)
	}
	return size + 256
}

// colorantPaintBytes 按指针去重估算专色画刷及其色料空间的内存占用
// 入参: c 专色画刷, seen 节点去重表
// 返回: int64 估算字节数
func colorantPaintBytes(c *ColorantPaint, seen *shadingByteTracker) int64 {
	if c == nil {
		return 0
	}
	size := int64(64) + colorantSpaceBytes(c.Space, seen)
	if len(c.Tints) > len(c.values) {
		size += int64(len(c.Tints)) * 8
	}
	return size
}

// backgroundPaintBytes 按指针去重估算着色背景画刷的空间、色料与过程通道内存占用
// 入参: bg 背景画刷, seen 节点去重表
// 返回: int64 估算字节数
func backgroundPaintBytes(bg *Paint, seen *shadingByteTracker) int64 {
	if bg == nil {
		return 0
	}
	size := int64(128) + colorSpaceMemoryBytes(bg.Space, seen) + colorantPaintBytes(bg.Colorant, seen)
	if bg.Process != nil {
		size += colorSpaceMemoryBytes(bg.Process.Space, seen) + int64(len(bg.Process.Channels))*8
	}
	return size
}

// shadingStyleBytes 估算着色图案状态持有的网屏、传递函数、软蒙版及颜色转换内存
// 入参: style 图案图形状态, seen 节点去重表
// 返回: int64 估算字节数
func shadingStyleBytes(style Style, seen *shadingByteTracker) int64 {
	var size int64
	if style.Halftone != nil {
		size += halftoneTreeBytes(style.Halftone, seen)
	}
	if style.Transfer != nil {
		size += 64
		for _, fn := range style.Transfer.functions {
			if fn != nil {
				size += gradientFunctionMemoryBytes(fn.function, seen)
			}
		}
	}
	if style.SoftMask != nil {
		size += 256 + int64(len(style.SoftMask.Backdrop))*8 + colorSpaceMemoryBytes(style.SoftMask.ColorSpace, seen) + gradientFunctionMemoryBytes(style.SoftMask.transfer, seen)
	}
	if style.ColorConversion.BlackGeneration != nil {
		size += gradientFunctionMemoryBytes(style.ColorConversion.BlackGeneration.function, seen)
	}
	if style.ColorConversion.UndercolorRemoval != nil {
		size += gradientFunctionMemoryBytes(style.ColorConversion.UndercolorRemoval.function, seen)
	}
	return size
}

// halftoneTreeBytes 按指针去重递归估算网屏及复合子网屏的完整内存占用
// 入参: h 网屏定义, seen 节点去重表
// 返回: int64 估算字节数
func halftoneTreeBytes(h *Halftone, seen *shadingByteTracker) int64 {
	if h == nil {
		return 0
	}
	if seen.halftones == nil {
		seen.halftones = make(map[*Halftone]bool)
	}
	if seen.halftones[h] {
		return 0
	}
	seen.halftones[h] = true
	return halftoneMemoryBytesSeen(h, seen.halftones)
}

// colorSpaceMemoryBytes 按指针去重估算颜色空间所持ICC配置及其备用空间的内存占用
// 入参: space 颜色空间, seen 节点去重表
// 返回: int64 估算字节数
func colorSpaceMemoryBytes(space *ColorSpace, seen *shadingByteTracker) int64 {
	if space == nil || space.profile == nil {
		return 0
	}
	return iccProfileMemoryBytes(space.profile, seen)
}

// imagePaletteBytes 按指针去重估算索引色调色板及其基础空间和色料定义的内存占用
// 入参: p 索引色调色板, seen 节点去重表
// 返回: int64 估算字节数
func imagePaletteBytes(p *imagePalette, seen *shadingByteTracker) int64 {
	if p == nil {
		return 0
	}
	if seen.palettes == nil {
		seen.palettes = make(map[*imagePalette]bool)
	}
	if seen.palettes[p] {
		return 0
	}
	seen.palettes[p] = true
	size := int64(128) + int64(len(p.values))*32 + int64(len(p.colors))*8 + int64(len(p.tints))*24
	for _, tint := range p.tints {
		size += int64(len(tint)) * 8
	}
	size += colorSpaceMemoryBytes(p.space, seen) + colorantSpaceBytes(p.colorants, seen)
	if p.process != nil {
		size += colorSpaceMemoryBytes(p.process.Space, seen) + int64(len(p.process.Channels))*8
	}
	return size
}

// deviceNMemoryBytes 估算专色空间及其采样表、计算器、嵌套备用与色料定义的内存占用
// 入参: tint 专色空间定义
// 返回: int64 估算字节数
func deviceNMemoryBytes(tint *deviceNSpace) int64 {
	return tintMemoryBytes(tint, &shadingByteTracker{})
}

// separationMemoryBytes 估算分色空间及其备用变换和色料定义的内存占用
// 入参: s 分色空间定义
// 返回: int64 估算字节数
func separationMemoryBytes(s *separationSpace) int64 {
	return separationSpaceBytes(s, &shadingByteTracker{})
}

// gradientFunctionMemoryBytes 按指针去重估算一维渐变函数及其关联色料空间的内存占用
// 入参: f 渐变函数, seen 节点去重表
// 返回: int64 估算字节数
func gradientFunctionMemoryBytes(f *gradientFunction, seen *shadingByteTracker) int64 {
	if f == nil {
		return 0
	}
	if seen.functions == nil {
		seen.functions = make(map[*gradientFunction]bool)
	}
	if seen.functions[f] {
		return 0
	}
	seen.functions[f] = true
	return max(int64(128), f.bytes) + colorantSpaceBytes(f.colorants, seen)
}

// gradientVectorMemoryBytes 按指针去重估算多分量渐变函数的内存占用
// 入参: v 多分量渐变函数, seen 节点去重表
// 返回: int64 估算字节数
func gradientVectorMemoryBytes(v *gradientVector, seen *shadingByteTracker) int64 {
	if v == nil {
		return 0
	}
	if seen.vectors == nil {
		seen.vectors = make(map[*gradientVector]bool)
	}
	if seen.vectors[v] {
		return 0
	}
	seen.vectors[v] = true
	size := max(int64(128), v.bytes)
	if v.scalar != nil {
		if seen.functions == nil {
			seen.functions = make(map[*gradientFunction]bool)
		}
		if !seen.functions[v.scalar] {
			seen.functions[v.scalar] = true
			if v.bytes == 0 {
				size += v.scalar.bytes
			}
			size += colorantSpaceBytes(v.scalar.colorants, seen)
		}
	}
	return size
}

// tintMemoryBytes 按指针去重估算专色空间及其采样表、计算器、嵌套备用与色料定义内存
// 入参: tint 专色空间定义, seen 节点去重表
// 返回: int64 估算字节数
func tintMemoryBytes(tint *deviceNSpace, seen *shadingByteTracker) int64 {
	if tint == nil {
		return 0
	}
	if seen.tints == nil {
		seen.tints = make(map[*deviceNSpace]bool)
	}
	if seen.tints[tint] {
		return 0
	}
	seen.tints[tint] = true
	size := int64(256) + int64(len(tint.input)+len(tint.output))*8 + int64(len(tint.processInputs))*8
	for _, expr := range tint.expressions {
		size += 24 + int64(len(expr))*8
	}
	size += colorSpaceMemoryBytes(tint.alternate, seen) + gradientFunctionMemoryBytes(tint.transform, seen)
	if tint.program != nil {
		if seen.programs == nil {
			seen.programs = make(map[*calculatorFunction]bool)
		}
		if !seen.programs[tint.program] {
			seen.programs[tint.program] = true
			size += calculatorProgramBytes(tint.program)
		}
	}
	if tint.sampled != nil {
		if seen.sampled == nil {
			seen.sampled = make(map[*sampledFunction]bool)
		}
		if !seen.sampled[tint.sampled] {
			seen.sampled[tint.sampled] = true
			size += sampledMemoryBytes(tint.sampled)
		}
	}
	if tint.process != nil {
		size += colorSpaceMemoryBytes(tint.process.Space, seen) + int64(len(tint.process.Channels))*8
	}
	return size + tintAlternateBytes(tint.nested, seen) + colorantSpaceBytes(tint.colorants, seen)
}

// separationSpaceBytes 按指针去重估算分色空间及其备用变换和色料定义内存
// 入参: s 分色空间定义, seen 节点去重表
// 返回: int64 估算字节数
func separationSpaceBytes(s *separationSpace, seen *shadingByteTracker) int64 {
	if s == nil {
		return 0
	}
	if seen.separations == nil {
		seen.separations = make(map[*separationSpace]bool)
	}
	if seen.separations[s] {
		return 0
	}
	seen.separations[s] = true
	return 256 + colorSpaceMemoryBytes(s.space, seen) + iccProfileMemoryBytes(s.icc, seen) + gradientFunctionMemoryBytes(s.transform, seen) + tintAlternateBytes(s.nested, seen) + colorantSpaceBytes(s.colorants, seen)
}

// colorantSpaceBytes 按指针去重估算色料空间及其独立过程色与专色表内存
// 入参: cs 色料空间定义, seen 节点去重表
// 返回: int64 估算字节数
func colorantSpaceBytes(cs *ColorantSpace, seen *shadingByteTracker) int64 {
	if cs == nil {
		return 0
	}
	if seen.colorants == nil {
		seen.colorants = make(map[*ColorantSpace]bool)
	}
	if seen.colorants[cs] {
		return 0
	}
	seen.colorants[cs] = true
	size := int64(128) + int64(len(cs.Names))*16
	if cs.Process != nil {
		size += colorSpaceMemoryBytes(cs.Process.Space, seen)
	}
	size += separationSpaceBytes(cs.separation, seen) + tintMemoryBytes(cs.tint, seen)
	for _, spot := range cs.spots {
		size += separationSpaceBytes(spot, seen)
	}
	return size
}

// tintAlternateBytes 按指针去重估算嵌套备用颜色空间的内存占用
// 入参: alt 嵌套备用空间, seen 节点去重表
// 返回: int64 估算字节数
func tintAlternateBytes(alt *tintAlternate, seen *shadingByteTracker) int64 {
	if alt == nil {
		return 0
	}
	if seen.alternates == nil {
		seen.alternates = make(map[*tintAlternate]bool)
	}
	if seen.alternates[alt] {
		return 0
	}
	seen.alternates[alt] = true
	return 128 + colorSpaceMemoryBytes(alt.space, seen) + separationSpaceBytes(alt.separation, seen) + tintMemoryBytes(alt.deviceN, seen)
}

// shadingPattern 读取定义时的状态，仅解释影响sh的内部参数
// 入参: name 图案资源名
// 返回: Paint 图案画刷, error 状态或着色错误
func (p *pageInterpreter) shadingPattern(name Name) (Paint, error) {
	object, err := p.resource("Pattern", name)
	if err != nil {
		return Paint{}, err
	}
	key := shadingPatternKey{name: name, matrix: p.patternMatrix, blending: p.blendingSpace}
	if ref, ok := object.(Reference); ok {
		key.ref, key.name = ref, ""
	}
	if cache := p.shadingPatterns; cache != nil {
		if cached, ok := cache.byPattern[key]; ok {
			return cached, nil
		}
		if cache.hasLast && cache.lastKey == key {
			return cache.lastPaint, nil
		}
	}
	value, err := p.reader.Resolve(object)
	if err != nil {
		return Paint{}, err
	}
	dict, ok := value.(Dictionary)
	if !ok {
		return Paint{}, &UnsupportedError{Feature: "shading pattern type"}
	}
	kind, err := p.reader.Resolve(dict["PatternType"])
	if err != nil {
		return Paint{}, err
	}
	if kind != Integer(2) {
		return Paint{}, &UnsupportedError{Feature: "shading pattern type"}
	}
	child := *p
	child.ctx = withGradientParseBudget(child.ctx, child.reader)
	child.resetTransient()
	child.state = p.patternInitialState()
	matrix := p.patternMatrix
	object, err = p.reader.Resolve(dict["Matrix"])
	if err != nil {
		return Paint{}, err
	}
	if object != nil {
		values, err := p.reader.numberArray(object, 6)
		if err != nil {
			return Paint{}, err
		}
		matrix = matrix.Mul(Matrix(values))
	}
	if _, ok := matrix.Inverse(); !ok {
		return Paint{}, fmt.Errorf("singular shading pattern matrix")
	}
	child.state.matrix = matrix
	object, err = p.reader.Resolve(dict["ExtGState"])
	if err != nil {
		return Paint{}, err
	}
	beforeExt := gradientParsedBytes(child.ctx)
	if object != nil {
		state, ok := object.(Dictionary)
		if !ok {
			return Paint{}, fmt.Errorf("invalid shading pattern graphics state")
		}
		parameters := make(Dictionary)
		for key, value := range state {
			switch key {
			case "Type", "RI", "ca", "BM", "SMask", "AIS", "OP", "op", "OPM", "TR", "TR2", "BG", "BG2", "UCR", "UCR2", "HT", "HTO", "UseBlackPtComp", "SM", "AAPL:AA":
				parameters[key] = value
			}
		}
		if err := child.applyExtState(parameters); err != nil {
			return Paint{}, err
		}
	}
	extBytes := gradientParsedBytes(child.ctx) - beforeExt
	shading, err := p.reader.Resolve(dict["Shading"])
	if err != nil {
		return Paint{}, err
	}
	budget := int64(formCacheLimit)
	if maxBytes, _ := effectiveStreamLimits(p.reader, nil); maxBytes > 0 && maxBytes < budget {
		budget = maxBytes
	}
	var paint Paint
	streamHit, streamBudgeted := false, false
	var paintParsed int64
	if stream, isStream := shading.(*Stream); isStream {
		sKey := shadingStreamKey{stream: stream, matrix: matrix, intent: child.state.style.RenderingIntent, blending: child.blendingSpace}
		if cache := p.shadingPatterns; cache != nil {
			if cached, hit := cache.byStream[sKey]; hit {
				paint, streamHit, streamBudgeted = cached, true, true
			} else if cache.hasMesh && cache.lastStream == sKey {
				paint, streamHit = cache.lastMesh, true
			}
		}
		if !streamHit {
			beforePaint := gradientParsedBytes(child.ctx)
			paint, err = child.shadingPatternPaint(shading, matrix)
			if err != nil {
				return Paint{}, err
			}
			paintParsed = gradientParsedBytes(child.ctx) - beforePaint
			if !paint.None {
				if p.shadingPatterns == nil {
					p.shadingPatterns = &shadingPatternCache{}
				}
				cost := max(shadingPaintBytes(paint), paintParsed)
				if len(p.shadingPatterns.byStream) < 1024 && cost <= budget-p.shadingPatterns.bytes {
					if p.shadingPatterns.byStream == nil {
						p.shadingPatterns.byStream = make(map[shadingStreamKey]Paint)
					}
					p.shadingPatterns.byStream[sKey] = paint
					p.shadingPatterns.bytes += cost
					streamBudgeted = true
				} else {
					p.shadingPatterns.lastStream, p.shadingPatterns.lastMesh, p.shadingPatterns.hasMesh = sKey, paint, true
				}
			}
		}
	} else {
		beforePaint := gradientParsedBytes(child.ctx)
		paint, err = child.shadingPatternPaint(shading, matrix)
		if err != nil {
			return Paint{}, err
		}
		paintParsed = gradientParsedBytes(child.ctx) - beforePaint
	}
	if paint.None {
		return paint, nil
	}
	var bgParsed int64
	result := paint
	if object != nil {
		definition, ok := shading.(Dictionary)
		if stream, isStream := shading.(*Stream); isStream {
			definition, ok = stream.Dictionary, true
		}
		if !ok {
			return Paint{}, fmt.Errorf("invalid shading pattern definition")
		}
		var background *Paint
		value, err = p.reader.Resolve(definition["Background"])
		if err != nil {
			return Paint{}, err
		}
		if value != nil {
			beforeBg := gradientParsedBytes(child.ctx)
			space, err := child.shadingColorSpace(definition["ColorSpace"])
			if err != nil {
				return Paint{}, err
			}
			base, err := child.reader.readPatternColorSpaceContext(child.ctx, space, true)
			if err != nil {
				return Paint{}, err
			}
			values, err := child.reader.numberArray(value, base.components)
			if err != nil {
				return Paint{}, err
			}
			color, err := base.convert(values, child.state.style.RenderingIntent)
			if err != nil {
				return Paint{}, err
			}
			bgParsed = gradientParsedBytes(child.ctx) - beforeBg
			background = &color
		}
		result = Paint{Alpha: 1, Shading: &ShadingPattern{Paint: paint, Style: child.state.style, Background: background}}
	}
	if p.shadingPatterns == nil {
		p.shadingPatterns = &shadingPatternCache{}
	}
	cost := int64(256)
	if !streamBudgeted {
		cost = max(shadingPaintBytes(result), extBytes+paintParsed+bgParsed)
	} else if result.Shading != nil {
		seen := &shadingByteTracker{}
		cost += max(shadingStyleBytes(result.Shading.Style, seen), extBytes) + max(backgroundPaintBytes(result.Shading.Background, seen), bgParsed)
	}
	if len(p.shadingPatterns.byPattern) < 1024 && cost <= budget-p.shadingPatterns.bytes {
		if p.shadingPatterns.byPattern == nil {
			p.shadingPatterns.byPattern = make(map[shadingPatternKey]Paint)
		}
		p.shadingPatterns.byPattern[key] = result
		p.shadingPatterns.bytes += cost
	} else {
		p.shadingPatterns.lastKey, p.shadingPatterns.lastPaint, p.shadingPatterns.hasLast = key, result, true
	}
	return result, nil
}
