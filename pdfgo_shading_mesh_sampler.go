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
	"fmt"
	"hash/maphash"
	"math"
)

// MeshSampler 复用网格颜色函数，备用色与原始浓度各缓存最多1024项
// 使用期间网格应保持只读；并发采样应分别创建采样器
type MeshSampler struct {
	mesh      *MeshGradient
	seed      maphash.Seed
	entries   []meshSampleEntry
	colorants []meshSampleEntry
}

// meshSampleEntry 保存函数输入的位模式及成功求值的颜色分量
type meshSampleEntry struct {
	input  uint64
	values [4]float64
	valid  bool
}

// NewSampler 创建独立的网格采样器，不修改网格或共享函数
// 返回: *MeshSampler 网格采样器
func (g *MeshGradient) NewSampler() *MeshSampler {
	return &MeshSampler{mesh: g}
}

// ValuesAt 插值网格分量并复用完全相同的函数输入，不缓存失败结果
// 入参: patch 网格序号, u 横向参数或第二顶点权重, v 纵向参数或第三顶点权重
// 返回: [4]float64 Space颜色空间分量, error 参数或颜色错误
func (s *MeshSampler) ValuesAt(patch int, u, v float64) ([4]float64, error) {
	if s == nil || s.mesh == nil {
		return [4]float64{}, fmt.Errorf("invalid mesh sampler")
	}
	g := s.mesh
	if g.function == nil {
		return g.ValuesAt(patch, u, v)
	}
	weights, colors, err := g.sourceColorsAt(patch, u, v)
	if err != nil {
		return [4]float64{}, err
	}
	x, err := meshFunctionInput(weights, colors)
	if err != nil {
		return [4]float64{}, err
	}
	if s.entries == nil {
		if s.colorants == nil {
			s.seed = maphash.MakeSeed()
		}
		s.entries = make([]meshSampleEntry, 1024)
	}
	input := math.Float64bits(x)
	entry := &s.entries[maphash.Comparable(s.seed, input)%uint64(len(s.entries))]
	if entry.valid && entry.input == input {
		return entry.values, nil
	}
	values, err := g.functionValues(x)
	if err == nil {
		*entry = meshSampleEntry{input: input, values: values, valid: true}
	}
	return values, err
}

// ColorantValuesAt 复用常见色料函数的精确输入结果，不混用备用颜色缓存
// 入参: patch 网格序号, u 横向参数, v 纵向参数, out 与色料数等长的输出缓冲
// 返回: error 非色料渐变、参数或函数错误，错误时不保证缓冲内容
func (s *MeshSampler) ColorantValuesAt(patch int, u, v float64, out []float64) error {
	if s == nil || s.mesh == nil {
		return fmt.Errorf("invalid mesh sampler")
	}
	g := s.mesh
	space := g.ColorantSpace()
	if space == nil || len(out) != len(space.Names) {
		return fmt.Errorf("invalid mesh colorant component count")
	}
	if g.function == nil || len(out) > 4 {
		return g.ColorantValuesAt(patch, u, v, out)
	}
	weights, colors, err := g.sourceColorsAt(patch, u, v)
	if err != nil {
		return err
	}
	x, err := meshFunctionInput(weights, colors)
	if err != nil {
		return err
	}
	if len(g.function.parts) != len(out) {
		return fmt.Errorf("invalid mesh colorant component count")
	}
	if s.colorants == nil {
		if s.entries == nil {
			s.seed = maphash.MakeSeed()
		}
		s.colorants = make([]meshSampleEntry, 1024)
	}
	input := math.Float64bits(x)
	entry := &s.colorants[maphash.Comparable(s.seed, input)%uint64(len(s.colorants))]
	if entry.valid && entry.input == input {
		copy(out, entry.values[:])
		return nil
	}
	if err := g.function.calculate(x, out); err != nil {
		return err
	}
	if err := clampColorantValues(out); err != nil {
		return err
	}
	*entry = meshSampleEntry{input: input, valid: true}
	copy(entry.values[:], out)
	return nil
}
