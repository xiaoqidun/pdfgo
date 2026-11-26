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

// FunctionGradient 保存二维函数着色的定义域、源颜色及坐标变换
// Matrix将定义域映射到页面，PatternMatrix将着色坐标映射到页面
// Background及Bounds仅用于着色图案，Bounds使用着色坐标
type FunctionGradient struct {
	Domain        Rectangle
	Matrix        Matrix
	PatternMatrix Matrix
	Space         *ColorSpace
	Intent        Name
	Background    *[4]float64
	Bounds        *Rectangle
	function      *coordinateFunction
	tint          *deviceNSpace
	lab           *labSpace
}

// coordinateFunction 保存二维采样或计算器函数及各分量的独立函数
type coordinateFunction struct {
	domain  [4]float64
	bounds  []float64
	outputs int
	sampled *sampledFunction
	program *calculatorFunction
	parts   []*coordinateFunction
}

// ValuesAt 按定义域内坐标计算源颜色分量，定义域外使用边界坐标
// 入参: point 定义域坐标，不含Matrix变换
// 返回: [4]float64 源颜色或备用空间分量, error 坐标或求值错误
func (g *FunctionGradient) ValuesAt(point Point) ([4]float64, error) {
	if g == nil || g.function == nil || math.IsNaN(point.X) || math.IsNaN(point.Y) || math.IsInf(point.X, 0) || math.IsInf(point.Y, 0) {
		return [4]float64{}, fmt.Errorf("invalid function shading evaluation")
	}
	point.X = math.Max(g.Domain.XMin, math.Min(g.Domain.XMax, point.X))
	point.Y = math.Max(g.Domain.YMin, math.Min(g.Domain.YMax, point.Y))
	var buffer [32]float64
	values := buffer[:min(g.function.outputs, len(buffer))]
	if g.function.outputs > len(buffer) {
		values = make([]float64, g.function.outputs)
	}
	if err := g.function.evaluate([2]float64{point.X, point.Y}, values); err != nil {
		return [4]float64{}, err
	}
	return shadingColorValues(g.Space, g.tint, g.lab, values)
}

// ColorAt 计算定义域坐标的颜色，再按渲染意图转换为sRGB
// 入参: point 定义域坐标，不含Matrix变换
// 返回: [3]float64 sRGB颜色, error 坐标或颜色变换错误
func (g *FunctionGradient) ColorAt(point Point) ([3]float64, error) {
	if g == nil {
		return [3]float64{}, fmt.Errorf("invalid function shading evaluation")
	}
	values, err := g.ValuesAt(point)
	return gradientRGB(values, err, g.Space, g.Intent)
}

// evaluate 将二维函数结果写入分量缓冲区，独立分量保留各自定义域及范围
// 入参: point 二维输入, out 输出分量缓冲区
// 返回: error 计算器或非有限结果错误
func (f *coordinateFunction) evaluate(point [2]float64, out []float64) error {
	if f.parts != nil {
		for i, part := range f.parts {
			if err := part.evaluate(point, out[i:i+1]); err != nil {
				return err
			}
		}
		return nil
	}
	for i := range point {
		point[i] = math.Max(f.domain[2*i], math.Min(f.domain[2*i+1], point[i]))
	}
	if f.sampled != nil {
		f.sampled.evaluate(point[:], out)
	} else if err := f.program.evaluate(point[:], out); err != nil {
		return err
	}
	for i, value := range out {
		if math.IsNaN(value) || math.IsInf(value, 0) {
			return fmt.Errorf("nonfinite coordinate function result")
		}
		out[i] = math.Max(f.bounds[2*i], math.Min(f.bounds[2*i+1], value))
	}
	return nil
}

// coordinateFunctionState 保存单次二维函数着色解析的节点预算、解码字节预算及间接引用记忆表
type coordinateFunctionState struct {
	nodes  int
	bytes  int64
	limit  int64
	cached map[Reference]*coordinateFunction
}

// coordinateFunctionBytes 估算二维函数及其去重分量的采样内存占用
// 入参: f 二维函数定义
// 返回: int64 估算字节数
func coordinateFunctionBytes(f *coordinateFunction) int64 {
	if f == nil {
		return 0
	}
	seen := make(map[*coordinateFunction]bool)
	var walk func(*coordinateFunction) int64
	walk = func(node *coordinateFunction) int64 {
		if node == nil || seen[node] {
			return 0
		}
		seen[node] = true
		size := int64(256)
		if node.sampled != nil {
			size += int64(len(node.sampled.table.samples))
		}
		for _, part := range node.parts {
			size += walk(part)
		}
		return size
	}
	return walk(f)
}

// readCoordinateFunctionContext 在本次取消上下文内读取二维函数及独立分量
// 入参: ctx 取消上下文, object 函数或函数数组, outputs 输出分量数, domain 着色定义域
// 返回: *coordinateFunction 函数定义, error 格式、解码、预算或取消错误
func (r *Reader) readCoordinateFunctionContext(ctx context.Context, object Object, outputs int, domain Rectangle) (*coordinateFunction, error) {
	limit, _ := effectiveStreamLimits(r, nil)
	return r.readCoordinateFunctionState(ctx, object, outputs, domain, true, &coordinateFunctionState{limit: limit})
}

// readCoordinateFunctionState 携带节点预算、解码预算与间接引用记忆表解析二维函数
// 入参: ctx 取消上下文, object 函数或函数数组, outputs 输出分量数, domain 着色定义域, allowArray 是否允许函数数组, state 解析状态
// 返回: *coordinateFunction 函数定义, error 格式、解码、预算或取消错误
func (r *Reader) readCoordinateFunctionState(ctx context.Context, object Object, outputs int, domain Rectangle, allowArray bool, state *coordinateFunctionState) (*coordinateFunction, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	state.nodes++
	if state.nodes > maxGradientFunctionNodes {
		return nil, fmt.Errorf("%w: coordinate function node limit exceeded", ErrStreamLimitExceeded)
	}
	ref, isRef := object.(Reference)
	if !allowArray && isRef {
		if cached := state.cached[ref]; cached != nil {
			return cached, nil
		}
	}
	value, err := r.Resolve(object)
	if err != nil {
		return nil, err
	}
	f := &coordinateFunction{outputs: outputs}
	if array, ok := value.(Array); ok {
		if !allowArray {
			return nil, fmt.Errorf("nested coordinate function array")
		}
		if len(array) != outputs {
			return nil, fmt.Errorf("invalid coordinate function count")
		}
		f.parts = make([]*coordinateFunction, outputs)
		for i, item := range array {
			f.parts[i], err = r.readCoordinateFunctionState(ctx, item, 1, domain, false, state)
			if err != nil {
				return nil, err
			}
		}
		return f, nil
	}
	stream, ok := value.(*Stream)
	if !ok {
		return nil, fmt.Errorf("missing two-input function stream")
	}
	dict := stream.Dictionary
	values, err := r.numberArray(dict["Domain"], 4)
	if err != nil {
		return nil, err
	}
	f.domain = [4]float64(values)
	if values[0] > domain.XMin || values[1] < domain.XMax || values[2] > domain.YMin || values[3] < domain.YMax {
		return nil, fmt.Errorf("coordinate function domain excludes shading domain")
	}
	f.bounds, err = r.numberArray(dict["Range"], 2*outputs)
	if err != nil {
		return nil, err
	}
	for i := range outputs {
		if f.bounds[2*i] > f.bounds[2*i+1] {
			return nil, fmt.Errorf("invalid coordinate function range")
		}
	}
	kind, err := r.Resolve(dict["FunctionType"])
	if err != nil {
		return nil, err
	}
	switch kind {
	case Integer(0):
		if state.limit > 0 {
			if err := r.checkCoordinateSampledBudget(dict, outputs, state); err != nil {
				return nil, err
			}
		}
		f.sampled, err = r.readSampledFunctionContext(ctx, stream, 2, outputs)
		if err == nil {
			cost := int64(len(f.sampled.table.samples))
			if state.limit > 0 && cost > state.limit-state.bytes {
				return nil, fmt.Errorf("%w: coordinate function memory limit exceeded", ErrStreamLimitExceeded)
			}
			state.bytes += cost
		}
	case Integer(4):
		var data []byte
		data, err = r.DecodeStreamContext(ctx, stream)
		if err == nil {
			cost := int64(len(data))
			if state.limit > 0 && cost > state.limit-state.bytes {
				return nil, fmt.Errorf("%w: coordinate function memory limit exceeded", ErrStreamLimitExceeded)
			}
			state.bytes += cost
			f.program, err = newCalculatorFunction(data, r.warning)
		}
	default:
		return nil, fmt.Errorf("invalid two-input function type")
	}
	if err == nil {
		err = ctx.Err()
	}
	if err != nil {
		return nil, err
	}
	if !allowArray && isRef {
		if state.cached == nil {
			state.cached = make(map[Reference]*coordinateFunction)
		}
		state.cached[ref] = f
	}
	return f, nil
}

// checkCoordinateSampledBudget 解码前按采样维数和位宽预检累计采样字节预算
// 入参: dict 采样函数字典, outputs 输出分量数, state 解析状态
// 返回: error 预算超限错误
func (r *Reader) checkCoordinateSampledBudget(dict Dictionary, outputs int, state *coordinateFunctionState) error {
	sizes, err := r.numberArray(dict["Size"], 2)
	if err != nil || !finiteFloats(sizes) {
		return nil
	}
	bits, err := r.number(dict["BitsPerSample"])
	if err != nil || bits != 1 && bits != 2 && bits != 4 && bits != 8 && bits != 12 && bits != 16 && bits != 24 && bits != 32 {
		return nil
	}
	remaining := uint64(state.limit - state.bytes)
	if sizes[0] < 1 || sizes[1] < 1 || sizes[0] > float64(math.MaxInt64) || sizes[1] > float64(math.MaxInt64) || math.Trunc(sizes[0]) != sizes[0] || math.Trunc(sizes[1]) != sizes[1] {
		return nil
	}
	s0, s1 := uint64(sizes[0]), uint64(sizes[1])
	limitBits := remaining * 8
	if remaining > math.MaxUint64/8 {
		limitBits = math.MaxUint64
	}
	perSample := uint64(outputs) * uint64(bits)
	if s0 > limitBits/perSample || s1 > limitBits/(s0*perSample) {
		return fmt.Errorf("%w: coordinate function memory limit exceeded", ErrStreamLimitExceeded)
	}
	needed := (s0*s1*perSample-1)/8 + 1
	if needed > remaining {
		return fmt.Errorf("%w: coordinate function memory limit exceeded", ErrStreamLimitExceeded)
	}
	return nil
}

// functionShading 解析函数着色的定义域、颜色空间及双层坐标变换
// 入参: shading 着色字典, matrix 着色坐标到页面的变换
// 返回: Paint 函数画刷, error 格式、能力、超限或取消错误
func (p *pageInterpreter) functionShading(shading Dictionary, matrix Matrix) (Paint, error) {
	g := &FunctionGradient{Domain: Rectangle{0, 0, 1, 1}, Matrix: matrix, PatternMatrix: matrix, Intent: p.state.style.RenderingIntent}
	value, err := p.reader.Resolve(shading["Domain"])
	if err != nil {
		return Paint{}, err
	}
	if value != nil {
		v, err := p.reader.numberArray(value, 4)
		if err != nil {
			return Paint{}, err
		}
		if v[0] > v[1] || v[2] > v[3] {
			return Paint{}, fmt.Errorf("invalid function shading domain")
		}
		g.Domain = Rectangle{v[0], v[2], v[1], v[3]}
	}
	value, err = p.reader.Resolve(shading["Matrix"])
	if err != nil {
		return Paint{}, err
	}
	if value != nil {
		v, err := p.reader.numberArray(value, 6)
		if err != nil {
			return Paint{}, err
		}
		g.Matrix = matrix.Mul(Matrix(v))
	}
	if _, ok := g.Matrix.Inverse(); !ok {
		if p.visitor.Warning == nil {
			return Paint{}, fmt.Errorf("singular function shading transform")
		}
		p.visitor.Warning(Diagnostic{Message: "singular function shading transform ignored"})
		return Paint{None: true}, p.ctx.Err()
	}
	object, err := p.shadingColorSpace(shading["ColorSpace"])
	if err != nil {
		return Paint{}, err
	}
	if array, ok := object.(Array); ok && len(array) > 0 && (array[0] == Name("Indexed") || array[0] == Name("Pattern")) {
		return Paint{}, fmt.Errorf("invalid function shading color space")
	}
	var components int
	g.Space, g.tint, g.lab, components, err = p.reader.readShadingColorSpaceContext(p.ctx, object, true)
	if err != nil {
		return Paint{}, err
	}
	g.function, err = p.reader.readCoordinateFunctionContext(p.ctx, shading["Function"], components, g.Domain)
	if err != nil {
		return Paint{}, err
	}
	paint := Paint{Function: g}
	if g.tint != nil {
		paint.Process = g.tint.process
	}
	return paint, nil
}
