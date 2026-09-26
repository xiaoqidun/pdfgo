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

// TilingPattern 保存平铺图案的单元几何及独立内容流
type TilingPattern struct {
	BBox      Rectangle
	Matrix    Matrix
	XStep     float64
	YStep     float64
	PaintType int
	reader    *Reader
	resources Dictionary
	data      []byte
	depth     int
}

// tilingPattern 读取平铺图案，着色图案交由shadingPattern处理
// 入参: name 图案资源名
// 返回: *TilingPattern 平铺图案, error 解析错误
func (p *pageInterpreter) tilingPattern(name Name) (*TilingPattern, error) {
	object, err := p.resource("Pattern", name)
	if err != nil {
		return nil, err
	}
	value, err := p.reader.Resolve(object)
	if err != nil {
		return nil, err
	}
	var dict Dictionary
	if stream, ok := value.(*Stream); ok {
		dict = stream.Dictionary
	} else if v, ok := value.(Dictionary); ok {
		dict = v
	} else {
		return nil, fmt.Errorf("invalid pattern resource")
	}
	if dict["PatternType"] == Integer(2) {
		return nil, nil
	}
	stream, ok := value.(*Stream)
	if !ok || dict["PatternType"] != Integer(1) {
		return nil, &UnsupportedError{Feature: "pattern type"}
	}
	paintType, ok := dict["PaintType"].(Integer)
	if !ok || paintType != 1 && paintType != 2 {
		return nil, fmt.Errorf("invalid tiling pattern paint type")
	}
	if tiling, ok := dict["TilingType"].(Integer); !ok || tiling < 1 || tiling > 3 {
		return nil, fmt.Errorf("invalid tiling pattern type")
	}
	box, err := p.reader.rectangle(dict["BBox"])
	if err != nil {
		return nil, err
	}
	xstep, err := p.reader.number(dict["XStep"])
	if err != nil {
		return nil, err
	}
	ystep, err := p.reader.number(dict["YStep"])
	if err != nil {
		return nil, err
	}
	if xstep <= 0 || ystep <= 0 || math.IsNaN(xstep) || math.IsNaN(ystep) {
		return nil, fmt.Errorf("invalid tiling pattern step")
	}
	matrix := Identity()
	if dict["Matrix"] != nil {
		values, err := p.reader.numberArray(dict["Matrix"], 6)
		if err != nil {
			return nil, err
		}
		matrix = Matrix(values)
	}
	resources, err := p.reader.Resolve(dict["Resources"])
	if err != nil {
		return nil, err
	}
	var resourceDict Dictionary
	if resources != nil {
		resourceDict, ok = resources.(Dictionary)
		if !ok {
			return nil, fmt.Errorf("invalid tiling pattern resources")
		}
	}
	data, err := stream.Decode()
	if err != nil {
		return nil, err
	}
	return &TilingPattern{BBox: box, Matrix: p.patternMatrix.Mul(matrix), XStep: xstep, YStep: ystep, PaintType: int(paintType), reader: p.reader, resources: resourceDict, data: data, depth: p.depth + 1}, nil
}

// Walk 独立解释图案单元内容，保留图案边界与无色图案的基色
// 入参: ctx 取消上下文, base 无色图案基色, visitor 绘制访问器
// 返回: error 解析或访问错误
func (p *TilingPattern) Walk(ctx context.Context, base Paint, visitor Visitor) error {
	if p.depth > 64 {
		return &UnsupportedError{Feature: "nested pattern depth"}
	}
	base.Tiling, base.Axial, base.Radial = nil, nil, nil
	base.Alpha = 1
	box := p.BBox
	clip := Path{Segments: []Segment{{"M", []Point{{box.XMin, box.YMin}}}, {"L", []Point{{box.XMax, box.YMin}}}, {"L", []Point{{box.XMax, box.YMax}}}, {"L", []Point{{box.XMin, box.YMax}}}, {"C", nil}}}
	style := Style{Fill: base, Stroke: base, LineWidth: 1, MiterLimit: 10, Clips: []Path{clip}}
	child := pageInterpreter{reader: p.reader, resources: p.resources, visitor: visitor, ctx: ctx, depth: p.depth, uncoloredPattern: p.PaintType == 2, patternMatrix: Identity(), bounds: box}
	child.state = graphicsState{matrix: Identity(), hscale: 1, fillSpace: "DeviceGray", strokeSpace: "DeviceGray", style: style}
	return child.run(p.data)
}

// patternBaseColor 按底层设备色空间解释无色图案的颜色分量
// 入参: paint 当前画刷, space 底层色空间, operands 颜色分量
// 返回: error 不支持的分量或解析错误
func patternBaseColor(paint *Paint, space Name, operands []Object) error {
	count := 1
	switch space {
	case "DeviceRGB":
		count = 3
	case "DeviceCMYK":
		count = 4
	case "DeviceGray":
	default:
		return &UnsupportedError{Feature: "uncolored pattern base color space"}
	}
	values, err := numbers(operands, count)
	if err != nil {
		return err
	}
	for n, value := range values {
		values[n] = math.Max(0, math.Min(1, value))
	}
	paint.CMYK = nil
	if count == 1 {
		paint.RGB = [3]float64{values[0], values[0], values[0]}
	} else if count == 3 {
		paint.RGB = [3]float64{values[0], values[1], values[2]}
	} else {
		paint.CMYK = &[4]float64{values[0], values[1], values[2], values[3]}
	}
	return nil
}
