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
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"hash"
	"maps"
	"math"
	"reflect"
	"slices"
)

// maxCanonicalDigestNodes 限制颜色空间规范摘要允许遍历的最大对象节点数
const maxCanonicalDigestNodes = 4096

// maxCanonicalDigestBytes 限制颜色空间规范摘要允许参与SHA-256计算的最大总字节数
const maxCanonicalDigestBytes int64 = 4 << 20

// canonicalMemoKey 标识内存中可共享复用的复合对象指针与长度
type canonicalMemoKey struct {
	kind   uint8
	ptr    uintptr
	length int
}

// errICCSourceProfileClass 区分非源配置类别，不将设备链接或抽象变换替换为设备色
var errICCSourceProfileClass = errors.New("invalid ICC source profile class")

// validateICCSourceHeader 按PDF要求校验源配置类别、模型与分量数
// 入参: data 配置数据, count PDF声明的分量数
// 返回: error 配置格式或声明冲突
func validateICCSourceHeader(data []byte, count int) error {
	if len(data) < 132 || string(data[36:40]) != "acsp" || uint64(binary.BigEndian.Uint32(data)) != uint64(len(data)) {
		return fmt.Errorf("invalid ICC profile header")
	}
	switch string(data[12:16]) {
	case "scnr", "mntr", "prtr", "spac":
	default:
		return errICCSourceProfileClass
	}
	model := string(data[16:20])
	components := map[string]int{"GRAY": 1, "RGB ": 3, "CMYK": 4, "Lab ": 3}[model]
	if components == 0 || components != count {
		return fmt.Errorf("invalid ICC source profile components")
	}
	if string(data[20:24]) != "XYZ " && string(data[20:24]) != "Lab " {
		return fmt.Errorf("invalid ICC profile connection space")
	}
	return nil
}

// remapICCAlternate 仅重映射实际使用的备用设备色，保留原流与源范围
// 入参: ctx 取消上下文, object ICCBased数组, resources 当前资源, remap 是否替换设备色, depth 嵌套深度, state 重映射状态
// 返回: Object 有效ICC定义, error 配置、资源、预算或取消错误
func (r *Reader) remapICCAlternate(ctx context.Context, object Array, resources Dictionary, remap bool, depth int, state *colorRemapState) (Object, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	stream, count, err := r.iccProfileStream(object)
	if err != nil {
		if r.warning != nil && len(object) == 2 {
			value, resolveErr := r.Resolve(object[1])
			if resolveErr != nil {
				return nil, resolveErr
			}
			if profile, ok := value.(*Stream); ok && profile != nil {
				value, resolveErr = r.Resolve(profile.Dictionary["N"])
				if resolveErr != nil {
					return nil, resolveErr
				}
				if value == nil {
					r.warning(Diagnostic{Message: "ICCBased color space without N; PDF viewer DeviceGray default used without inferring profile components"})
					return Name("DeviceGray"), nil
				}
			}
		}
		return nil, err
	}
	streamKey := iccStreamKey{stream: stream, count: int(count)}
	if state.iccValid[streamKey] {
		return object, nil
	}
	altKey := iccAlternateKey{stream: stream, count: int(count), remap: remap}
	if cached, ok := state.iccAlternates[altKey]; ok {
		return cached, nil
	}
	var cause error
	if !state.iccV5[streamKey] {
		var data []byte
		data, cause = r.DecodeStreamContext(ctx, stream)
		if cause == nil {
			var profile *iccColorSpace
			profile, cause = r.readICCProfileData(data, int(count))
			if cause == nil && profile != nil {
				if state.iccValid == nil {
					state.iccValid = make(map[iccStreamKey]bool)
				}
				state.iccValid[streamKey] = true
				return object, nil
			}
			if cause == nil && profile == nil {
				if state.iccV5 == nil {
					state.iccV5 = make(map[iccStreamKey]bool)
				}
				state.iccV5[streamKey] = true
			}
		}
	}
	if cause != nil {
		if ctx.Err() != nil || r.warning == nil || errors.Is(cause, errICCSourceProfileClass) {
			return nil, cause
		}
		if state.iccV5 == nil {
			state.iccV5 = make(map[iccStreamKey]bool)
		}
		state.iccV5[streamKey] = true
	}
	if remap {
		if _, err := r.iccRanges(stream.Dictionary["Range"], int(count)); err != nil {
			return nil, err
		}
	}
	rawAlt := stream.Dictionary["Alternate"]
	resolvedAlt, err := r.Resolve(rawAlt)
	if err != nil {
		return nil, err
	}
	if resolvedAlt == nil {
		rawAlt = map[int]Name{1: "DeviceGray", 3: "DeviceRGB", 4: "DeviceCMYK"}[int(count)]
	}
	alternate, err := r.remapColorSpaceState(ctx, rawAlt, resources, remap, depth+1, state)
	if err != nil {
		return nil, err
	}
	if remap {
		switch colorSpaceFamily(alternate) {
		case "DeviceGray", "CalGray", "Separation", "Indexed":
			if count != 1 {
				return nil, fmt.Errorf("invalid ICC alternate component count")
			}
		case "DeviceRGB", "CalRGB", "Lab":
			if count != 3 {
				return nil, fmt.Errorf("invalid ICC alternate component count")
			}
		case "DeviceCMYK":
			if count != 4 {
				return nil, fmt.Errorf("invalid ICC alternate component count")
			}
		case "ICCBased":
			array, ok := alternate.(Array)
			if !ok {
				return nil, fmt.Errorf("invalid ICC alternate color space")
			}
			_, altCount, err := r.iccProfileStream(array)
			if err != nil {
				return nil, err
			}
			if altCount != count {
				return nil, fmt.Errorf("invalid ICC alternate component count")
			}
		case "DeviceN":
			array, ok := alternate.(Array)
			if !ok {
				return nil, fmt.Errorf("invalid ICC alternate color space")
			}
			names, _, err := r.deviceNColorants(array)
			if err != nil {
				return nil, err
			}
			if len(names) != int(count) {
				return nil, fmt.Errorf("invalid ICC alternate component count")
			}
		default:
			return nil, fmt.Errorf("invalid ICC alternate color space")
		}
	}
	copy := *stream
	copy.Dictionary = maps.Clone(stream.Dictionary)
	copy.Dictionary["Alternate"] = alternate
	result := Array{Name("ICCBased"), &copy}
	if state.iccAlternates == nil {
		state.iccAlternates = make(map[iccAlternateKey]Object)
	}
	state.iccAlternates[altKey] = result
	return result, nil
}

// recoverICCAlternate 仅在备用空间及分量范围有效时恢复损坏配置，保留原始配置
// 入参: ctx 取消上下文, stream 配置流, count 源分量数, ranges 源范围, effective 是否已重映射, cause 配置错误
// 返回: *iccColorSpace 备用变换, error 严格校验、备用空间或取消错误
func (r *Reader) recoverICCAlternate(ctx context.Context, stream *Stream, count int, ranges *[8]float64, effective bool, cause error) (*iccColorSpace, error) {
	if ctx.Err() != nil || r.warning == nil || errors.Is(cause, errICCSourceProfileClass) {
		return nil, cause
	}
	space, err := r.readICCAlternate(ctx, stream, count, ranges, effective)
	if err != nil {
		if ctx.Err() != nil {
			return nil, err
		}
		return nil, fmt.Errorf("%w; ICC alternate: %v", cause, err)
	}
	r.warning(Diagnostic{Message: "invalid ICC profile recovered using valid alternate color space: " + cause.Error()})
	return space, nil
}

// iccAlternateObject 读取备用颜色空间，缺省按N选择设备空间
// 入参: stream 配置流, count 源分量数
// 返回: Object 备用定义, error 引用错误
func (r *Reader) iccAlternateObject(stream *Stream, count int) (Object, error) {
	value, err := r.Resolve(stream.Dictionary["Alternate"])
	if err != nil {
		return nil, err
	}
	if value == nil {
		value = map[int]Name{1: "DeviceGray", 3: "DeviceRGB", 4: "DeviceCMYK"}[count]
	}
	return value, nil
}

// readICCAlternate 编译同分量备用空间，不预先转换原始分量或共享配置缓存
// 入参: ctx 取消上下文, stream 配置流, count 源分量数, ranges 源范围, effective 是否已按资源校验并替换
// 返回: *iccColorSpace 备用源变换, error 定义、分量或取消错误
func (r *Reader) readICCAlternate(ctx context.Context, stream *Stream, count int, ranges *[8]float64, effective bool) (*iccColorSpace, error) {
	object, err := r.iccAlternateObject(stream, count)
	if err != nil {
		return nil, err
	}
	alternate, err := r.readPatternColorSpaceContext(ctx, object, effective)
	if err != nil {
		return nil, err
	}
	if alternate.components != count {
		return nil, fmt.Errorf("invalid ICC alternate component count")
	}
	profile := &iccColorSpace{alternate: alternate, alternateCount: count, ranges: ranges, alternateBlending: alternate.blending}
	if alternate.blending == nil {
		profile.alternateDigest, profile.hasAlternateDigest, err = r.canonicalObjectDigest(ctx, object)
		if err != nil {
			return nil, err
		}
	}
	return profile, nil
}

// canonicalObjectDigest 在取消上下文与节点字节预算内计算颜色空间定义对象的内容摘要，复用共享内存DAG子摘要
// 入参: ctx 取消上下文, object 颜色空间定义对象
// 返回: [32]byte 内容摘要, bool 是否生成有效摘要, error 上下文取消错误
func (r *Reader) canonicalObjectDigest(ctx context.Context, object Object) ([32]byte, bool, error) {
	nodes := 0
	var totalBytes int64
	memo := make(map[canonicalMemoKey][32]byte)
	active := make(map[canonicalMemoKey]bool)
	refMemo := make(map[Reference][32]byte)
	refActive := make(map[Reference]bool)
	writeBytes := func(h hash.Hash, data []byte) (bool, error) {
		if int64(len(data)) > maxCanonicalDigestBytes-totalBytes {
			return false, nil
		}
		for offset := 0; offset < len(data); offset += 64 << 10 {
			if err := ctx.Err(); err != nil {
				return false, err
			}
			end := min(offset+(64<<10), len(data))
			h.Write(data[offset:end])
			totalBytes += int64(end - offset)
		}
		return true, nil
	}
	writeU64 := func(h hash.Hash, v uint64) (bool, error) {
		var buf [8]byte
		binary.LittleEndian.PutUint64(buf[:], v)
		return writeBytes(h, buf[:])
	}
	var digest func(Object, int) ([32]byte, bool, error)
	digest = func(value Object, depth int) ([32]byte, bool, error) {
		if err := ctx.Err(); err != nil {
			return [32]byte{}, false, err
		}
		if depth > 64 {
			return [32]byte{}, false, nil
		}
		nodes++
		if nodes > maxCanonicalDigestNodes {
			return [32]byte{}, false, nil
		}
		switch v := value.(type) {
		case Reference:
			if r != nil && !refActive[v] {
				if cached, ok := refMemo[v]; ok {
					return cached, true, nil
				}
				refActive[v] = true
				resolved, err := r.Object(v)
				if err != nil {
					delete(refActive, v)
					return [32]byte{}, false, nil
				}
				sum, ok, derr := digest(resolved, depth+1)
				delete(refActive, v)
				if ok {
					refMemo[v] = sum
				}
				return sum, ok, derr
			}
			h := sha256.New()
			if ok, err := writeBytes(h, []byte{9}); !ok || err != nil {
				return [32]byte{}, false, err
			}
			if ok, err := writeU64(h, uint64(v.Number)); !ok || err != nil {
				return [32]byte{}, false, err
			}
			if ok, err := writeU64(h, uint64(v.Generation)); !ok || err != nil {
				return [32]byte{}, false, err
			}
			var sum [32]byte
			h.Sum(sum[:0])
			return sum, true, nil
		case Array:
			var key canonicalMemoKey
			if len(v) > 0 {
				key = canonicalMemoKey{kind: 6, ptr: reflect.ValueOf(v).Pointer(), length: len(v)}
				if active[key] {
					return [32]byte{}, false, nil
				}
				if cached, ok := memo[key]; ok {
					return cached, true, nil
				}
				active[key] = true
				defer delete(active, key)
			}
			h := sha256.New()
			if ok, err := writeBytes(h, []byte{6}); !ok || err != nil {
				return [32]byte{}, false, err
			}
			if ok, err := writeU64(h, uint64(len(v))); !ok || err != nil {
				return [32]byte{}, false, err
			}
			for _, item := range v {
				child, ok, err := digest(item, depth+1)
				if !ok || err != nil {
					return [32]byte{}, false, err
				}
				if ok, err := writeBytes(h, child[:]); !ok || err != nil {
					return [32]byte{}, false, err
				}
			}
			var sum [32]byte
			h.Sum(sum[:0])
			if len(v) > 0 {
				memo[key] = sum
			}
			return sum, true, nil
		case Dictionary:
			var key canonicalMemoKey
			if len(v) > 0 {
				key = canonicalMemoKey{kind: 7, ptr: reflect.ValueOf(v).Pointer()}
				if active[key] {
					return [32]byte{}, false, nil
				}
				if cached, ok := memo[key]; ok {
					return cached, true, nil
				}
				active[key] = true
				defer delete(active, key)
			}
			h := sha256.New()
			if ok, err := writeBytes(h, []byte{7}); !ok || err != nil {
				return [32]byte{}, false, err
			}
			if ok, err := writeU64(h, uint64(len(v))); !ok || err != nil {
				return [32]byte{}, false, err
			}
			for _, name := range slices.Sorted(maps.Keys(v)) {
				if ok, err := writeU64(h, uint64(len(name))); !ok || err != nil {
					return [32]byte{}, false, err
				}
				if ok, err := writeBytes(h, []byte(name)); !ok || err != nil {
					return [32]byte{}, false, err
				}
				child, ok, err := digest(v[name], depth+1)
				if !ok || err != nil {
					return [32]byte{}, false, err
				}
				if ok, err := writeBytes(h, child[:]); !ok || err != nil {
					return [32]byte{}, false, err
				}
			}
			var sum [32]byte
			h.Sum(sum[:0])
			if len(v) > 0 {
				memo[key] = sum
			}
			return sum, true, nil
		case *Stream:
			if v == nil {
				h := sha256.New()
				if ok, err := writeBytes(h, []byte{0}); !ok || err != nil {
					return [32]byte{}, false, err
				}
				var sum [32]byte
				h.Sum(sum[:0])
				return sum, true, nil
			}
			key := canonicalMemoKey{kind: 8, ptr: reflect.ValueOf(v).Pointer()}
			if active[key] {
				return [32]byte{}, false, nil
			}
			if cached, ok := memo[key]; ok {
				return cached, true, nil
			}
			active[key] = true
			defer delete(active, key)
			h := sha256.New()
			if ok, err := writeBytes(h, []byte{8}); !ok || err != nil {
				return [32]byte{}, false, err
			}
			dictSum, ok, err := digest(v.Dictionary, depth+1)
			if !ok || err != nil {
				return [32]byte{}, false, err
			}
			if ok, err := writeBytes(h, dictSum[:]); !ok || err != nil {
				return [32]byte{}, false, err
			}
			if ok, err := writeU64(h, uint64(len(v.Data))); !ok || err != nil {
				return [32]byte{}, false, err
			}
			if ok, err := writeBytes(h, v.Data); !ok || err != nil {
				return [32]byte{}, false, err
			}
			var sum [32]byte
			h.Sum(sum[:0])
			memo[key] = sum
			return sum, true, nil
		}
		h := sha256.New()
		switch v := value.(type) {
		case nil:
			if ok, err := writeBytes(h, []byte{0}); !ok || err != nil {
				return [32]byte{}, false, err
			}
		case Boolean:
			if v {
				if ok, err := writeBytes(h, []byte{1, 1}); !ok || err != nil {
					return [32]byte{}, false, err
				}
			} else {
				if ok, err := writeBytes(h, []byte{1, 0}); !ok || err != nil {
					return [32]byte{}, false, err
				}
			}
		case Integer:
			if ok, err := writeBytes(h, []byte{2}); !ok || err != nil {
				return [32]byte{}, false, err
			}
			if ok, err := writeU64(h, uint64(v)); !ok || err != nil {
				return [32]byte{}, false, err
			}
		case Real:
			f := float64(v)
			if f == 0 {
				f = 0
			}
			if ok, err := writeBytes(h, []byte{3}); !ok || err != nil {
				return [32]byte{}, false, err
			}
			if ok, err := writeU64(h, math.Float64bits(f)); !ok || err != nil {
				return [32]byte{}, false, err
			}
		case Name:
			if ok, err := writeBytes(h, []byte{4}); !ok || err != nil {
				return [32]byte{}, false, err
			}
			if ok, err := writeU64(h, uint64(len(v))); !ok || err != nil {
				return [32]byte{}, false, err
			}
			if ok, err := writeBytes(h, []byte(v)); !ok || err != nil {
				return [32]byte{}, false, err
			}
		case String:
			if ok, err := writeBytes(h, []byte{5}); !ok || err != nil {
				return [32]byte{}, false, err
			}
			if ok, err := writeU64(h, uint64(len(v))); !ok || err != nil {
				return [32]byte{}, false, err
			}
			if ok, err := writeBytes(h, v); !ok || err != nil {
				return [32]byte{}, false, err
			}
		default:
			return [32]byte{}, false, nil
		}
		var sum [32]byte
		h.Sum(sum[:0])
		return sum, true, nil
	}
	return digest(object, 0)
}

// alternateValues 裁切源范围后原样传入备用空间，不先归一化源分量
// 入参: values PDF源分量
// 返回: []float64 裁切后的分量，未超出范围时复用输入, error 数量或非有限值错误
func (s *iccColorSpace) alternateValues(values []float64) ([]float64, error) {
	if len(values) != s.components() {
		return nil, fmt.Errorf("invalid ICC alternate input count")
	}
	result := values
	ranges := s.sourceRanges()
	for i, value := range values {
		if math.IsNaN(value) || math.IsInf(value, 0) {
			return nil, fmt.Errorf("invalid ICC alternate input")
		}
		clipped := math.Max(ranges[2*i], math.Min(ranges[2*i+1], value))
		if clipped != value {
			if &result[0] == &values[0] {
				result = slices.Clone(values)
			}
			result[i] = clipped
		}
	}
	return result, nil
}

// alternatePaint 从固定源分量构建备用颜色，独立保存最终输出空间
// 入参: input 实际源分量, intent 渲染意图
// 返回: Paint 备用空间结果, error 裁切或转换错误
func (s *iccColorSpace) alternatePaint(input [4]float64, intent Name) (Paint, error) {
	values, err := s.alternateValues(input[:s.components()])
	if err != nil {
		return Paint{}, err
	}
	paint, err := s.alternate.convert(values, intent)
	paint.SourceSpace = "ICCBased"
	return paint, err
}

// alternateColor 将单位源分量恢复后交给备用空间，不影响原ICC快速路径的分配
// 入参: input 单位源分量, intent 渲染意图
// 返回: [3]float64 显示颜色, error 备用转换错误
func (s *iccColorSpace) alternateColor(input [4]float64, intent Name) ([3]float64, error) {
	device := s.deviceValues(input[:s.components()])
	paint, err := s.alternatePaint(device, intent)
	return paint.RGB, err
}

// alternateXYZ 保留实际备用空间的色度计算，不先截断到显示RGB
// 入参: input 单位源分量, intent 渲染意图
// 返回: [3]float64 D50色度, error 转换错误
func (s *iccColorSpace) alternateXYZ(input [4]float64, intent Name) ([3]float64, error) {
	device := s.deviceValues(input[:s.components()])
	paint, err := s.alternatePaint(device, intent)
	if err != nil {
		return [3]float64{}, err
	}
	if paint.Space != nil {
		return paint.Space.xyz(paint.Values[:paint.Space.Components()], intent)
	}
	return (&ColorSpace{Model: "DeviceRGB"}).xyz(paint.RGB[:], intent)
}

// alternateFromXYZ 仅将合法可逆的备用混合空间用于目标转换
// 入参: xyz D50色度, intent 渲染意图
// 返回: [4]float64 目标分量, error 混合约束或逆变换错误
func (s *iccColorSpace) alternateFromXYZ(xyz [3]float64, intent Name) ([4]float64, error) {
	if err := s.validateBlending(); err != nil {
		return [4]float64{}, err
	}
	if p := s.alternateBlending.profile; p != nil {
		return p.fromXYZ(xyz, intent)
	}
	rgb := iccXYZRGB(xyz)
	return s.alternateBlending.Convert(rgb[:], &ColorSpace{Model: "DeviceRGB"}, intent)
}
