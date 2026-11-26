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

// shadingPaintBytes 估算着色画刷的内存占用
// 入参: paint 着色画刷
// 返回: int64 估算字节数
func shadingPaintBytes(paint Paint) int64 {
	if paint.Shading != nil {
		return 256 + shadingPaintBytes(paint.Shading.Paint)
	}
	if m := paint.Mesh; m != nil {
		size := int64(256)
		if len(m.Triangles) > 0 {
			size += int64(len(m.Triangles)) * (144 + int64(len(m.Triangles[0].Colors[0]))*24)
		}
		if len(m.Patches) > 0 {
			size += int64(len(m.Patches)) * (400 + int64(len(m.Patches[0].Colors[0]))*32)
		}
		return size
	}
	if paint.Axial != nil {
		return 256 + int64(len(paint.Axial.Stops))*64
	}
	if paint.Radial != nil {
		return 256 + int64(len(paint.Radial.Stops))*64
	}
	if paint.Function != nil {
		return 256 + coordinateFunctionBytes(paint.Function.function)
	}
	return 256
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
	if object != nil {
		state, ok := object.(Dictionary)
		if !ok {
			return Paint{}, fmt.Errorf("invalid shading pattern graphics state")
		}
		parameters := make(Dictionary)
		for key, value := range state {
			switch key {
			case "Type", "RI", "ca", "BM", "SMask", "AIS", "OP", "op", "OPM", "TR", "TR2", "BG", "BG2", "UCR", "UCR2", "HT", "HTO", "SM", "AAPL:AA":
				parameters[key] = value
			}
		}
		if err := child.applyExtState(parameters); err != nil {
			return Paint{}, err
		}
	}
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
			paint, err = child.shadingPatternPaint(shading, matrix)
			if err != nil {
				return Paint{}, err
			}
			if !paint.None {
				if p.shadingPatterns == nil {
					p.shadingPatterns = &shadingPatternCache{}
				}
				cost := shadingPaintBytes(paint)
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
		paint, err = child.shadingPatternPaint(shading, matrix)
		if err != nil {
			return Paint{}, err
		}
	}
	if paint.None {
		return paint, nil
	}
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
			background = &color
		}
		result = Paint{Alpha: 1, Shading: &ShadingPattern{Paint: paint, Style: child.state.style, Background: background}}
	}
	if p.shadingPatterns == nil {
		p.shadingPatterns = &shadingPatternCache{}
	}
	cost := int64(256)
	if !streamBudgeted {
		cost = shadingPaintBytes(result)
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
