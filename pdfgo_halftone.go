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
	"encoding/binary"
	"errors"
	"fmt"
	"maps"
	"math"
	"os"
	"slices"
)

// maxHalftoneThresholdBytes 限制单次半色调解析展开为16位阈值表的累计总字节上限
const maxHalftoneThresholdBytes int64 = 64 << 20

// maxHalftoneCacheEntries 限制单个Reader常驻缓存的网屏最大条数
const maxHalftoneCacheEntries = 256

// maxHalftoneCacheBytes 限制单个Reader常驻缓存的网屏总字节上限
const maxHalftoneCacheBytes int64 = maxHalftoneThresholdBytes * 2

// errMissingHalftoneThreshold 标识缺失阈值流，不混淆引用、网屏类型或解码错误
var errMissingHalftoneThreshold = errors.New("missing halftone threshold stream")

// halftoneCacheEntry 保存单次Type 5解析中按间接引用复用的子网屏结果
type halftoneCacheEntry struct {
	halftone *Halftone
	used     bool
}

// halftoneReadState 保存单次半色调解析的累计阈值与完整内存预算及子网屏去重表
type halftoneReadState struct {
	limit          int64
	bytes          int64
	thresholdLimit int64
	thresholdBytes int64
	cached         map[Reference]halftoneCacheEntry
}

// newHalftoneReadState 创建受生效流限制约束的单次半色调解析状态
// 返回: *halftoneReadState 解析状态
func (r *Reader) newHalftoneReadState() *halftoneReadState {
	thresholdLimit := maxHalftoneThresholdBytes
	limit := maxHalftoneThresholdBytes
	if maxBytes, _ := effectiveStreamLimits(r, nil); maxBytes > 0 {
		if maxBytes < thresholdLimit {
			thresholdLimit = maxBytes
		}
		if maxBytes < limit {
			limit = max(maxBytes, 4096)
		}
	}
	return &halftoneReadState{limit: limit, thresholdLimit: thresholdLimit}
}

// halftoneMemoryBytes 估算单个网屏定义的完整内存占用，包含阈值表、名称、字典键值、分量网屏及固定开销
// 入参: h 网屏定义
// 返回: int64 估算字节数
func halftoneMemoryBytes(h *Halftone) int64 {
	return halftoneMemoryBytesSeen(h, nil)
}

// halftoneMemoryBytesSeen 递归估算网屏及其分量占用的近似字节数，跳过已统计的共享或循环引用
// 入参: h 网屏定义, seen 已统计网屏指针集合
// 返回: int64 估算字节数
func halftoneMemoryBytesSeen(h *Halftone, seen map[*Halftone]bool) int64 {
	if h == nil {
		return 0
	}
	cost := int64(256) + int64(len(h.Name)) + int64(len(h.Thresholds))*2
	for key, value := range h.Dictionary {
		cost += 64 + int64(len(key))
		switch v := value.(type) {
		case Name:
			cost += int64(len(v))
		case String:
			cost += int64(len(v))
		}
	}
	if len(h.Components) == 0 {
		return cost
	}
	if seen == nil {
		seen = map[*Halftone]bool{h: true}
	}
	for key, part := range h.Components {
		cost += 64 + int64(len(key))
		if part != nil && !seen[part] {
			seen[part] = true
			cost += halftoneMemoryBytesSeen(part, seen)
		}
	}
	return cost
}

// cacheHalftone 在条目数与总字节上限内缓存已解析网屏，超限时清空旧缓存
// 入参: ref 网屏间接引用, h 已解析网屏
// 返回: bool 是否已写入缓存
func (r *Reader) cacheHalftone(ref Reference, h *Halftone) bool {
	cost := halftoneMemoryBytes(h)
	if cost > maxHalftoneCacheBytes {
		return false
	}
	if r.halftones == nil {
		r.halftones = make(map[Reference]*Halftone)
	} else {
		oldCost := int64(0)
		entries := len(r.halftones)
		if old, exists := r.halftones[ref]; exists {
			oldCost = halftoneMemoryBytes(old)
		} else {
			entries++
		}
		if entries > maxHalftoneCacheEntries || cost > maxHalftoneCacheBytes-(r.halftoneCacheBytes-oldCost) {
			clear(r.halftones)
			r.halftoneCacheBytes = 0
			oldCost = 0
		}
		r.halftoneCacheBytes -= oldCost
	}
	r.halftones[ref] = h
	r.halftoneCacheBytes += cost
	return true
}

// reserveMemory 从半色调解析内存预算中扣减指定字节数
// 入参: cost 待扣减字节数
// 返回: error 超出预算时返回包装ErrStreamLimitExceeded的错误
func (s *halftoneReadState) reserveMemory(cost int64) error {
	if cost < 0 || cost > s.limit-s.bytes {
		return fmt.Errorf("%w: halftone memory limit exceeded", ErrStreamLimitExceeded)
	}
	s.bytes += cost
	return nil
}

// Halftone 保存设备空间半色调定义，连续色调显示不需要网屏化
// Thresholds保持标准中的数组顺序，类型16使用高位优先的16位阈值
type Halftone struct {
	Type                           int
	Name                           []byte
	Dictionary                     Dictionary
	Frequency, Angle               float64
	SpotFunction                   Object
	TransferFunction               Object
	AccurateScreens                bool
	Width, Height, Width2, Height2 int
	Xsquare, Ysquare               int
	Thresholds                     []uint16
	Components                     map[Name]*Halftone
	reader                         *Reader
	cache                          *objectCache
}

// ReadHalftone 读取类型1、5、6、10和16的网屏定义，Default返回空值
// 入参: ctx 取消上下文, object 半色调字典、数据流、Default或间接引用
// 返回: *Halftone 网屏参数, error 类型、尺寸、阈值数据、预算或取消错误
func (r *Reader) ReadHalftone(ctx context.Context, object Object) (*Halftone, error) {
	if ctx == nil {
		return nil, fmt.Errorf("invalid halftone context")
	}
	if r == nil {
		return nil, fmt.Errorf("invalid reader")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if r.closed {
		return nil, os.ErrClosed
	}
	ref, indirect := object.(Reference)
	if indirect {
		if cached, ok := r.halftones[ref]; ok {
			return cloneHalftoneContext(ctx, cached)
		}
	}
	h, err := r.readHalftone(ctx, object, false, nil, nil, r.newHalftoneReadState())
	if err == nil && indirect && r.cacheHalftone(ref, h) {
		return cloneHalftoneContext(ctx, h)
	}
	return h, err
}

// ReadHalftoneForDevice 优先选择设备命名网屏，未匹配时解析文件中的备用定义
// 入参: ctx 取消上下文, object 网屏对象, named 只读设备网屏，匹配后不解析其他字段或阈值流
// 返回: *Halftone 有效网屏, error 名称、备用定义、预算或取消错误
func (r *Reader) ReadHalftoneForDevice(ctx context.Context, object Object, named map[string]*Halftone) (*Halftone, error) {
	if ctx == nil {
		return nil, fmt.Errorf("invalid halftone context")
	}
	if r == nil {
		return nil, fmt.Errorf("invalid reader")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if r.closed {
		return nil, os.ErrClosed
	}
	if len(named) == 0 {
		return r.ReadHalftone(ctx, object)
	}
	ref, indirect := object.(Reference)
	if indirect {
		if cached, ok := r.halftones[ref]; ok && !cached.namedOverride(named) {
			return cloneHalftoneContext(ctx, cached)
		}
	}
	used := false
	h, err := r.readHalftone(ctx, object, false, named, &used, r.newHalftoneReadState())
	if err == nil && indirect && !used && r.cacheHalftone(ref, h) {
		return cloneHalftoneContext(ctx, h)
	}
	return h, err
}

// cloneHalftoneContext 在取消上下文内深拷贝网屏的可变字段，隔离内部缓存与外部修改
// 入参: ctx 取消上下文, h 源网屏定义
// 返回: *Halftone 独立副本, error 取消错误
func cloneHalftoneContext(ctx context.Context, h *Halftone) (*Halftone, error) {
	if h == nil {
		return nil, nil
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	cloned := *h
	cloned.Name = bytes.Clone(h.Name)
	if h.Dictionary != nil {
		cloned.Dictionary = make(Dictionary, len(h.Dictionary))
		steps := 0
		for key, value := range h.Dictionary {
			if steps&255 == 255 {
				if err := ctx.Err(); err != nil {
					return nil, err
				}
			}
			cloned.Dictionary[key] = value
			steps++
		}
	}
	cloned.Thresholds = slices.Clone(h.Thresholds)
	if h.Components != nil {
		cloned.Components = make(map[Name]*Halftone, len(h.Components))
		for key, part := range h.Components {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			copied, err := cloneHalftoneContext(ctx, part)
			if err != nil {
				return nil, err
			}
			cloned.Components[key] = copied
		}
	}
	return &cloned, nil
}

// namedOverride 检查缓存定义及其单层分量是否被设备名称覆盖
// 入参: named 只读设备命名网屏
// 返回: bool 是否存在名称匹配
func (h *Halftone) namedOverride(named map[string]*Halftone) bool {
	if h == nil {
		return false
	}
	if h.Name != nil && named[string(h.Name)] != nil {
		return true
	}
	for _, part := range h.Components {
		if part != nil && part.Name != nil && named[string(part.Name)] != nil {
			return true
		}
	}
	return false
}

// displayHalftone 在连续色调解释中容忍无效或不支持的网屏定义并记录诊断
// 入参: object 网屏定义
// 返回: *Halftone 网屏或连续色调缺省, error 严格校验、读取、解密、认证、关闭或取消错误
func (p *pageInterpreter) displayHalftone(object Object) (*Halftone, error) {
	h, cause := p.reader.ReadHalftoneForDevice(p.ctx, object, p.visitor.Halftones)
	if cause == nil || p.visitor.Warning == nil {
		return h, cause
	}
	if err := p.ctx.Err(); err != nil {
		return nil, err
	}
	if isFatalReaderError(cause) {
		return nil, cause
	}
	if errors.Is(cause, errMissingHalftoneThreshold) {
		value, err := p.reader.Resolve(object)
		if err != nil {
			if isFatalReaderError(err) {
				return nil, err
			}
		} else if dict, ok := value.(Dictionary); ok {
			kind, err := p.reader.Resolve(dict["HalftoneType"])
			if err != nil {
				if isFatalReaderError(err) {
					return nil, err
				}
			} else if kind == Integer(6) || kind == Integer(10) || kind == Integer(16) {
				transfer, err := p.reader.Resolve(dict["TransferFunction"])
				if err != nil {
					if isFatalReaderError(err) {
						return nil, err
					}
				} else {
					candidate := &Halftone{Type: int(kind.(Integer)), Dictionary: dict, TransferFunction: transfer, reader: p.reader, cache: p.reader.cache}
					identity, err := candidate.identityTransferContext(p.ctx, p.reader)
					if err != nil {
						if isFatalReaderError(err) {
							return nil, err
						}
					} else if identity {
						p.visitor.Warning(Diagnostic{Message: "missing halftone threshold stream unused by continuous-tone display; identity transfer retained"})
						return nil, p.ctx.Err()
					}
				}
			}
		}
	}
	p.visitor.Warning(Diagnostic{Message: "invalid or unsupported halftone ignored: " + cause.Error()})
	return nil, p.ctx.Err()
}

// readHalftone 读取单色或分量网屏，禁止嵌套类型5
// 入参: ctx 取消上下文, object 网屏对象, component 是否为分量网屏, named 只读设备命名网屏, used 记录名称匹配, state 解析预算与去重状态
// 返回: *Halftone 网屏参数, error 定义、预算或取消错误
func (r *Reader) readHalftone(ctx context.Context, object Object, component bool, named map[string]*Halftone, used *bool, state *halftoneReadState) (*Halftone, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	value, err := r.Resolve(object)
	if err != nil {
		return nil, err
	}
	if value == Name("Default") && !component {
		return nil, nil
	}
	dict, ok := value.(Dictionary)
	stream, isStream := value.(*Stream)
	if isStream && stream != nil {
		dict, ok = stream.Dictionary, true
	}
	if !ok {
		return nil, fmt.Errorf("invalid halftone dictionary")
	}
	value, err = r.Resolve(dict["HalftoneName"])
	if err != nil {
		return nil, err
	}
	var name []byte
	if value != nil {
		raw, ok := value.(String)
		if !ok {
			return nil, fmt.Errorf("invalid halftone name")
		}
		if device := named[string(raw)]; device != nil {
			if component && device.Type == 5 {
				return nil, fmt.Errorf("invalid component halftone type")
			}
			*used = true
			return device, nil
		}
		name = append([]byte{}, raw...)
	}
	owner := r.owner
	if owner == nil {
		owner = r
	}
	h := &Halftone{Dictionary: dict, Name: name, reader: owner, cache: r.cache}
	value, err = r.Resolve(dict["HalftoneType"])
	if err != nil {
		return nil, err
	}
	kind, ok := value.(Integer)
	if !ok && !(value == nil && h.Name != nil) {
		return nil, fmt.Errorf("invalid halftone type")
	}
	h.Type = int(kind)
	value, err = r.Resolve(dict["Type"])
	if err != nil {
		return nil, err
	}
	if value != nil && value != Name("Halftone") {
		return nil, fmt.Errorf("invalid halftone object type")
	}
	if !ok {
		if err := state.reserveMemory(halftoneMemoryBytes(h)); err != nil {
			return nil, err
		}
		return h, nil
	}
	if kind == 5 {
		if component || isStream {
			return nil, fmt.Errorf("invalid component halftone type")
		}
		if err := state.reserveMemory(halftoneMemoryBytes(h)); err != nil {
			return nil, err
		}
		h.Components = make(map[Name]*Halftone)
		for _, key := range slices.Sorted(maps.Keys(dict)) {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			if key == "Type" || key == "HalftoneType" || key == "HalftoneName" {
				continue
			}
			item := dict[key]
			ref, isRef := item.(Reference)
			if isRef {
				if cached, hit := state.cached[ref]; hit {
					if cached.used {
						*used = true
						h.Components[key] = cached.halftone
						continue
					}
					thresholdCost := int64(len(cached.halftone.Thresholds)) * 2
					if thresholdCost > state.thresholdLimit-state.thresholdBytes {
						return nil, fmt.Errorf("%w: halftone threshold memory limit exceeded", ErrStreamLimitExceeded)
					}
					memCost := halftoneMemoryBytes(cached.halftone)
					if err := state.reserveMemory(memCost); err != nil {
						return nil, err
					}
					state.thresholdBytes += thresholdCost
					copied, err := cloneHalftoneContext(ctx, cached.halftone)
					if err != nil {
						return nil, err
					}
					h.Components[key] = copied
					continue
				}
			}
			partUsed := false
			var usedPtr *bool
			if used != nil {
				usedPtr = &partUsed
			}
			part, err := r.readHalftone(ctx, item, true, named, usedPtr, state)
			if err != nil {
				return nil, err
			}
			if partUsed {
				*used = true
			}
			if isRef {
				if state.cached == nil {
					state.cached = make(map[Reference]halftoneCacheEntry)
				}
				state.cached[ref] = halftoneCacheEntry{halftone: part, used: partUsed}
			}
			h.Components[key] = part
		}
		if h.Components["Default"] == nil {
			return nil, fmt.Errorf("missing default component halftone")
		}
		return h, nil
	}
	h.TransferFunction, err = r.Resolve(dict["TransferFunction"])
	if err != nil {
		return nil, err
	}
	if kind == 1 {
		if isStream {
			return nil, fmt.Errorf("invalid spot halftone stream")
		}
		h.Frequency, err = r.number(dict["Frequency"])
		if err != nil || h.Frequency <= 0 || math.IsInf(h.Frequency, 0) || math.IsNaN(h.Frequency) {
			return nil, fmt.Errorf("invalid screen frequency")
		}
		h.Angle, err = r.number(dict["Angle"])
		if err != nil || math.IsInf(h.Angle, 0) || math.IsNaN(h.Angle) {
			return nil, fmt.Errorf("invalid screen angle")
		}
		h.SpotFunction, err = r.Resolve(dict["SpotFunction"])
		if err != nil {
			return nil, err
		}
		switch h.SpotFunction.(type) {
		case Name, Dictionary, *Stream:
		default:
			return nil, fmt.Errorf("invalid spot function")
		}
		value, err = r.Resolve(dict["AccurateScreens"])
		if err != nil {
			return nil, err
		}
		if value != nil {
			flag, ok := value.(Boolean)
			if !ok {
				return nil, fmt.Errorf("invalid accurate screens flag")
			}
			h.AccurateScreens = bool(flag)
		}
		if err := state.reserveMemory(halftoneMemoryBytes(h)); err != nil {
			return nil, err
		}
		return h, nil
	}
	if kind != 6 && kind != 10 && kind != 16 {
		return nil, &UnsupportedError{Feature: fmt.Sprintf("halftone type %d", kind)}
	}
	if !isStream {
		return nil, errMissingHalftoneThreshold
	}
	fields := []struct {
		key    Name
		target *int
	}{{"Width", &h.Width}, {"Height", &h.Height}}
	if kind == 10 {
		fields = []struct {
			key    Name
			target *int
		}{{"Xsquare", &h.Xsquare}, {"Ysquare", &h.Ysquare}}
	}
	if kind == 16 {
		width2, err := r.Resolve(dict["Width2"])
		if err != nil {
			return nil, err
		}
		height2, err := r.Resolve(dict["Height2"])
		if err != nil {
			return nil, err
		}
		if width2 != nil || height2 != nil {
			fields = append(fields, struct {
				key    Name
				target *int
			}{"Width2", &h.Width2}, struct {
				key    Name
				target *int
			}{"Height2", &h.Height2})
		}
	}
	for _, field := range fields {
		value, err := r.Resolve(dict[field.key])
		if err != nil {
			return nil, err
		}
		n, ok := value.(Integer)
		if !ok || n <= 0 || uint64(n) > uint64(^uint(0)>>1) {
			return nil, fmt.Errorf("invalid halftone %s", field.key)
		}
		*field.target = int(n)
	}
	pairs := [][2]int{{h.Width, h.Height}, {h.Width2, h.Height2}}
	if kind == 10 {
		pairs = [][2]int{{h.Xsquare, h.Xsquare}, {h.Ysquare, h.Ysquare}}
	}
	if err := state.reserveMemory(halftoneMemoryBytes(h)); err != nil {
		return nil, err
	}
	var count uint64
	remainingBytes := min(state.thresholdLimit-state.thresholdBytes, state.limit-state.bytes)
	remainingPixels := uint64(remainingBytes / 2)
	for _, pair := range pairs {
		if pair[0] == 0 {
			continue
		}
		if uint64(pair[1]) > remainingPixels/uint64(pair[0]) {
			return nil, fmt.Errorf("%w: halftone threshold memory limit exceeded", ErrStreamLimitExceeded)
		}
		area := uint64(pair[0]) * uint64(pair[1])
		if area > remainingPixels-count {
			return nil, fmt.Errorf("%w: halftone threshold memory limit exceeded", ErrStreamLimitExceeded)
		}
		count += area
	}
	data, err := r.DecodeStreamContext(ctx, stream)
	if err != nil {
		return nil, err
	}
	width := uint64(1)
	if kind == 16 {
		width = 2
	}
	limit := uint64(len(data)) / width
	if count > limit {
		return nil, fmt.Errorf("truncated halftone thresholds")
	}
	if count != limit || uint64(len(data))%width != 0 {
		return nil, fmt.Errorf("invalid halftone threshold length")
	}
	thresholdCost := int64(count) * 2
	state.thresholdBytes += thresholdCost
	state.bytes += thresholdCost
	h.Thresholds = make([]uint16, int(count))
	for i := range h.Thresholds {
		if kind == 16 {
			h.Thresholds[i] = binary.BigEndian.Uint16(data[2*i:])
		} else {
			h.Thresholds[i] = uint16(data[i]) * 257
		}
	}
	return h, nil
}

// IdentityTransfer 判断网屏是否仅使用恒等传递函数，分量网屏逐项检查，nil接收者返回true和nil
// 入参: reader 同源Fork视图或备用阅读器，同源时优先使用以保证并发安全
// 返回: bool 是否恒等, error 函数解析或关闭错误
func (h *Halftone) IdentityTransfer(reader *Reader) (bool, error) {
	return h.identityTransferContext(context.Background(), reader)
}

// identityTransferContext 在取消上下文内检查网屏及其分量是否仅使用恒等传递函数
// 入参: ctx 取消上下文, reader 备用阅读器
// 返回: bool 是否恒等, error 引用、递归、关闭或取消错误
func (h *Halftone) identityTransferContext(ctx context.Context, reader *Reader) (bool, error) {
	var check func(*Halftone, *Reader, int) (bool, error)
	check = func(node *Halftone, r *Reader, depth int) (bool, error) {
		if err := ctx.Err(); err != nil {
			return false, err
		}
		if node == nil {
			return true, nil
		}
		if depth >= 32 {
			return false, fmt.Errorf("halftone recursion limit exceeded")
		}
		if !r.ownsObject(node.reader, node.cache) {
			forked := node.reader.Fork(ForkOptions{})
			defer forked.Close()
			r = forked
		}
		if r == nil {
			return false, fmt.Errorf("missing halftone reader")
		}
		if r.closed {
			return false, os.ErrClosed
		}
		if node.TransferFunction != nil {
			function, _, err := r.halftoneTransferContext(ctx, node)
			if err != nil {
				return false, err
			}
			if function != nil {
				return false, nil
			}
		}
		for _, part := range node.Components {
			identity, err := check(part, r, depth+1)
			if err != nil || !identity {
				return identity, err
			}
		}
		return true, nil
	}
	return check(h, reader, 0)
}

// component 选择原生分量网屏，缺省使用Default且禁止嵌套类型5
// 入参: colorant 原生分量名称
// 返回: *Halftone 分量网屏, error 定义错误
func (h *Halftone) component(colorant Name) (*Halftone, error) {
	if h == nil || h.Type != 5 {
		return h, nil
	}
	if h.Components["Default"] == nil || h.Components["Default"].Type == 5 {
		return nil, fmt.Errorf("invalid default halftone component")
	}
	part := h.Components[colorant]
	if part == nil {
		part = h.Components["Default"]
	}
	if part == nil || part.Type == 5 {
		return nil, fmt.Errorf("invalid halftone component")
	}
	return part, nil
}
