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
	"cmp"
	"context"
	"fmt"
	"slices"
	"sort"
	"strconv"
	"strings"
)

// maxCMapStreamDepth 限制CMap流级UseCMap的最大继承深度
const maxCMapStreamDepth = 64

// maxCMapCodespaceRanges 限制单条CID CMap继承链去重后的最大编码范围条数
const maxCMapCodespaceRanges = 4096

// maxCIDReverseSteps 限制CID到字符码反查允许检验的最大候选步数，防止重复覆盖区间耗尽CPU
const maxCIDReverseSteps = 1 << 20

// maxSpaceSecondBucketEntries 限制编码范围二级分桶索引允许分配的最大槽位总数
const maxSpaceSecondBucketEntries = 65536

// CIDMap 保存字符码到CID的映射
type CIDMap map[string]uint16

// cidCodeRange 保存逐字节判定的字符码范围
type cidCodeRange struct {
	first [4]byte
	last  [4]byte
	size  uint8
}

// cidNotdefRange 保存未定义字符到单个替代CID的映射
type cidNotdefRange struct {
	first uint64
	last  uint64
	cid   uint16
	order int
}

// cidCMapBudget 跟踪单条CID CMap继承链的累计条目预算
type cidCMapBudget struct {
	spaces       uint64
	uniqueSpaces map[cidCodeRange]struct{}
	notdef       uint64
	ranges       uint64
}

// cidMappedRange 保留连续CID区间及定义顺序，避免展开大量逐字符映射
type cidMappedRange struct {
	first   uint64
	last    uint64
	cid     uint16
	order   int
	invalid bool
}

// cidSecondBucket 保存大首字节桶按次字节划分的有序范围索引
type cidSecondBucket struct {
	bySecond [256][]uint16
}

// cidCMap 保存编码范围、字形选择和只读继承关系
type cidCMap struct {
	ranges          []cidMappedRange
	spaces          []cidCodeRange
	spaceByFirst    [256][]uint16
	spaceMinByFirst [256]uint8
	spaceSingle     [256]uint16
	spaceSecondMask *[256][4]uint64
	spaceSecond     map[byte]*cidSecondBucket
	minSpace        uint8
	notdef          []cidNotdefRange
	notdefFlat      bool
	registry        string
	ordering        string
	vertical        bool
	modeSet         bool
	use             Name
	base            *cidCMap
	recovered       bool
}

// makeCIDCodeRange 将原始编码范围压缩为固定字节值类型以便去重和比较
// 入参: first 起始字节序列, last 结束字节序列
// 返回: cidCodeRange 压缩后的编码范围
func makeCIDCodeRange(first, last String) cidCodeRange {
	var entry cidCodeRange
	entry.size = uint8(len(first))
	copy(entry.first[:], first)
	copy(entry.last[:], last)
	return entry
}

// buildSpaceIndex 重建分桶索引、最小编码长度及大桶二级次字节索引，加速范围匹配与剪枝
// 入参: ctx 取消上下文
// 返回: error 取消错误
func (c *cidCMap) buildSpaceIndex(ctx context.Context) error {
	c.spaceByFirst = [256][]uint16{}
	c.spaceMinByFirst = [256]uint8{}
	c.spaceSingle = [256]uint16{}
	c.spaceSecondMask = nil
	c.spaceSecond = nil
	c.minSpace = 0
	if len(c.spaces) == 0 {
		return ctx.Err()
	}
	var counts [256]int
	total := 0
	for i, space := range c.spaces {
		if i&255 == 0 {
			if err := ctx.Err(); err != nil {
				return err
			}
		}
		if c.minSpace == 0 || space.size < c.minSpace {
			c.minSpace = space.size
		}
		lo, hi := int(space.first[0]), int(space.last[0])
		total += hi - lo + 1
		for b := lo; b <= hi; b++ {
			counts[b]++
			if c.spaceMinByFirst[b] == 0 || space.size < c.spaceMinByFirst[b] {
				c.spaceMinByFirst[b] = space.size
			}
			if space.size == 1 && c.spaceSingle[b] == 0 {
				c.spaceSingle[b] = uint16(i + 1)
			}
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	backing := make([]uint16, total)
	offset := 0
	for b, n := range counts {
		if n > 0 {
			c.spaceByFirst[b] = backing[offset : offset : offset+n]
			offset += n
		}
	}
	for i, space := range c.spaces {
		if i&255 == 0 {
			if err := ctx.Err(); err != nil {
				return err
			}
		}
		idx := uint16(i)
		for b := int(space.first[0]); b <= int(space.last[0]); b++ {
			c.spaceByFirst[b] = append(c.spaceByFirst[b], idx)
		}
	}
	secondUsed := 0
	for b0, bucket := range c.spaceByFirst {
		if len(bucket) <= 16 {
			continue
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if c.spaceSecondMask == nil {
			c.spaceSecondMask = new([256][4]uint64)
		}
		var secCounts [256]int
		secTotal := 0
		for i, idx := range bucket {
			if i&255 == 0 {
				if err := ctx.Err(); err != nil {
					return err
				}
			}
			space := c.spaces[idx]
			if space.size < 2 {
				continue
			}
			lo, hi := int(space.first[1]), int(space.last[1])
			secTotal += hi - lo + 1
			for b1 := lo; b1 <= hi; b1++ {
				c.spaceSecondMask[b0][b1>>6] |= 1 << (b1 & 63)
				secCounts[b1]++
			}
		}
		if secTotal == 0 || secTotal > maxSpaceSecondBucketEntries-secondUsed {
			continue
		}
		if c.spaceSecond == nil {
			c.spaceSecond = make(map[byte]*cidSecondBucket)
		}
		secBacking := make([]uint16, secTotal)
		sb := &cidSecondBucket{}
		secOffset := 0
		for b1, n := range secCounts {
			if n > 0 {
				sb.bySecond[b1] = secBacking[secOffset : secOffset : secOffset+n]
				secOffset += n
			}
		}
		for i, idx := range bucket {
			if i&255 == 0 {
				if err := ctx.Err(); err != nil {
					return err
				}
			}
			space := c.spaces[idx]
			if space.size < 2 {
				continue
			}
			for b1 := int(space.first[1]); b1 <= int(space.last[1]); b1++ {
				sb.bySecond[b1] = append(sb.bySecond[b1], idx)
			}
		}
		c.spaceSecond[byte(b0)] = sb
		secondUsed += secTotal
	}
	return ctx.Err()
}

// ParseCIDMap 读取CMap中的字符码到CID的直接映射及连续范围
// 入参: data CMap数据
// 返回: CIDMap 字符映射, error 错误信息
func ParseCIDMap(data []byte) (CIDMap, error) {
	return ParseCIDMapContext(context.Background(), data)
}

// ParseCIDMapContext 读取并展开CID映射，继承及范围展开可取消
// 入参: ctx 取消上下文, data CMap数据
// 返回: CIDMap 完整字符映射, error 解析或取消错误
func ParseCIDMapContext(ctx context.Context, data []byte) (CIDMap, error) {
	if ctx == nil {
		return nil, fmt.Errorf("invalid CMap context")
	}
	mapping, err := parseCIDCMapContext(ctx, data)
	if err != nil {
		return nil, err
	}
	if mapping.use != "" {
		mapping.base, err = loadCIDCMapContext(ctx, mapping.use, nil)
		if err != nil {
			return nil, err
		}
	}
	result := CIDMap{}
	var expanded uint64
	for current := mapping; current != nil; current = current.base {
		for _, entry := range current.ranges {
			if entry.last < entry.first {
				continue
			}
			span := entry.last - entry.first + 1
			if span > maxCMapEntries-expanded {
				return nil, fmt.Errorf("CMap mapping entries exceed limit")
			}
			expanded += span
			for code := entry.first; code <= entry.last; code++ {
				if (code-entry.first)&255 == 0 {
					if err := ctx.Err(); err != nil {
						return nil, err
					}
				}
				raw := make([]byte, code>>32)
				for n := range raw {
					raw[len(raw)-n-1] = byte(code >> (n * 8))
				}
				if _, exists := result[string(raw)]; !exists {
					cid, _ := current.mapped(raw)
					result[string(raw)] = cid
				}
			}
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return result, nil
}

// parseCIDCMapContext 解析CID元数据和范围，取消时不保留部分映射
// 入参: ctx 取消上下文, data CMap数据
// 返回: *cidCMap 本层映射, error 格式、超限或取消错误
func parseCIDCMapContext(ctx context.Context, data []byte) (*cidCMap, error) {
	return parseCIDCMapBudgetIndexContext(ctx, data, nil, nil, true)
}

// parseCIDCMapBudgetIndexContext 在共享条目预算内解析单层CID映射，按需延迟首字节索引构建
// 入参: ctx 取消上下文, data CMap数据, warning 恢复诊断，空值严格校验, budget 跨继承链条目预算, buildIndex 是否立即构建首字节分桶索引
// 返回: *cidCMap 本层映射, error 格式、超限或取消错误
func parseCIDCMapBudgetIndexContext(ctx context.Context, data []byte, warning func(Diagnostic), budget *cidCMapBudget, buildIndex bool) (*cidCMap, error) {
	if budget == nil {
		budget = &cidCMapBudget{}
	}
	if budget.uniqueSpaces == nil {
		budget.uniqueSpaces = make(map[cidCodeRange]struct{})
	}
	p := objectParser{data: data}
	result := &cidCMap{}
	seenSpaces := make(map[cidCodeRange]struct{})
	previous := ""
	var name Name
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		p.skipSpace()
		if p.pos == len(data) {
			ranges, err := normalizeCIDMappedRanges(ctx, result.ranges)
			if err != nil {
				return nil, err
			}
			result.ranges = ranges
			notdef, err := normalizeCIDNotdefRanges(ctx, result.notdef)
			if err != nil {
				return nil, err
			}
			result.notdef = notdef
			if buildIndex {
				if err := result.buildSpaceIndex(ctx); err != nil {
					return nil, err
				}
			}
			return result, nil
		}
		if b := p.data[p.pos]; b == '/' || b == '(' || b == '<' || b == '[' {
			object, err := p.objectContext(ctx)
			if err != nil {
				return nil, err
			}
			name, _ = object.(Name)
			if name == "WMode" || name == "Registry" || name == "Ordering" || name == "CMapType" {
				value, err := p.objectContext(ctx)
				if err != nil {
					return nil, err
				}
				switch name {
				case "WMode":
					if value != Integer(0) && value != Integer(1) {
						return nil, p.fail("invalid CMap writing mode")
					}
					result.vertical, result.modeSet = value == Integer(1), true
				case "Registry", "Ordering":
					text, ok := value.(String)
					if !ok {
						return nil, p.fail("invalid CMap character collection")
					}
					if name == "Registry" {
						result.registry = string(text)
					} else {
						result.ordering = string(text)
					}
				case "CMapType":
					if value != Integer(1) {
						if warning == nil || value != Integer(0) {
							return nil, p.fail("invalid CID CMap type")
						}
						result.recovered = true
						warning(Diagnostic{Offset: int64(p.pos), Message: "CID CMap declares type zero; explicit CID operators retained"})
					}
				}
			}
			if info, ok := object.(Dictionary); ok {
				if registry, ok := info["Registry"].(String); ok {
					result.registry = string(registry)
				}
				if ordering, ok := info["Ordering"].(String); ok {
					result.ordering = string(ordering)
				}
			}
			previous = ""
			continue
		}
		token := p.token()
		if token == "" {
			p.pos++
			previous = ""
			continue
		}
		if token == "usecmap" {
			if name == "" || result.use != "" {
				return nil, p.fail("invalid CMap inheritance")
			}
			result.use = name
			name = ""
			continue
		}
		binaryCID := token == "beginbfchar" && warning != nil
		if token == "usefont" && previous != "0" || token == "beginbfchar" && !binaryCID || token == "beginbfrange" || token == "beginrearrangedfont" || token == "beginusematrix" {
			return nil, &UnsupportedError{Feature: "CID CMap operator " + token}
		}
		if !binaryCID && token != "begincidchar" && token != "begincidrange" && token != "begincodespacerange" && token != "beginnotdefchar" && token != "beginnotdefrange" {
			previous = token
			name = ""
			continue
		}
		count, err := strconv.Atoi(previous)
		if err != nil || count < 0 {
			return nil, p.fail("invalid CMap mapping count")
		}
		if binaryCID {
			result.recovered = true
			warning(Diagnostic{Offset: int64(p.pos), Message: "CID CMap uses bfchar; bounded binary CID destinations retained, not Unicode"})
		}
		for index := range count {
			if index&255 == 0 {
				if err := ctx.Err(); err != nil {
					return nil, err
				}
			}
			from, err := p.objectContext(ctx)
			if err != nil {
				return nil, err
			}
			first, ok := from.(String)
			if !ok || len(first) == 0 || len(first) > 4 {
				return nil, p.fail("invalid CMap source code")
			}
			last := first
			if strings.HasSuffix(token, "range") {
				to, err := p.objectContext(ctx)
				if err != nil {
					return nil, err
				}
				last, ok = to.(String)
				if !ok || len(last) != len(first) {
					return nil, p.fail("invalid CMap source range")
				}
			}
			start, end := codeNumber(first), codeNumber(last)
			if end < start {
				return nil, p.fail("invalid CMap source range")
			}
			if token == "begincodespacerange" {
				for i := range first {
					if first[i] > last[i] {
						return nil, p.fail("invalid CMap codespace range")
					}
				}
				if budget.spaces >= maxCMapEntries {
					return nil, fmt.Errorf("%w: CID CMap codespace limit exceeded", ErrStreamLimitExceeded)
				}
				budget.spaces++
				entry := makeCIDCodeRange(first, last)
				if _, exists := budget.uniqueSpaces[entry]; !exists {
					if len(budget.uniqueSpaces) >= maxCMapCodespaceRanges {
						return nil, fmt.Errorf("%w: CID CMap codespace limit exceeded", ErrStreamLimitExceeded)
					}
					budget.uniqueSpaces[entry] = struct{}{}
				}
				if _, exists := seenSpaces[entry]; !exists {
					seenSpaces[entry] = struct{}{}
					result.spaces = append(result.spaces, entry)
				}
				continue
			}
			value, err := p.objectContext(ctx)
			if err != nil {
				return nil, err
			}
			cid, ok := value.(Integer)
			if binaryCID {
				bytes, valid := value.(String)
				if !valid || len(bytes) == 0 || len(bytes) > 2 {
					return nil, p.fail("invalid binary CMap CID")
				}
				cid, ok = Integer(codeNumber(bytes)), true
			}
			if !ok {
				return nil, p.fail("invalid CMap CID")
			}
			invalid := cid < 0 || cid > 65535
			if token == "beginnotdefchar" || token == "beginnotdefrange" {
				if invalid {
					return nil, p.fail("invalid CMap CID")
				}
				if budget.notdef >= maxCMapEntries {
					return nil, fmt.Errorf("%w: CID CMap notdef limit exceeded", ErrStreamLimitExceeded)
				}
				budget.notdef++
				size := uint64(len(first)) << 32
				result.notdef = append(result.notdef, cidNotdefRange{first: size | start, last: size | end, cid: uint16(cid), order: len(result.notdef)})
				continue
			}
			invalid = invalid || uint64(cid)+end-start > 65535
			if invalid {
				if warning == nil {
					return nil, p.fail("invalid CMap CID range")
				}
				result.recovered = true
				warning(Diagnostic{Offset: int64(p.pos), Message: "out-of-range CMap CID destination retained as invalid; use is rejected"})
			}
			if budget.ranges >= maxCMapEntries {
				return nil, fmt.Errorf("%w: CID CMap range limit exceeded", ErrStreamLimitExceeded)
			}
			budget.ranges++
			size := uint64(len(first)) << 32
			result.ranges = append(result.ranges, cidMappedRange{first: size | start, last: size | end, cid: uint16(cid), order: len(result.ranges), invalid: invalid})
		}
		end := "end" + strings.TrimPrefix(token, "begin")
		if p.token() != end {
			return nil, p.fail("missing CMap block terminator")
		}
		previous = ""
	}
}

// normalizeCIDNotdefRanges 将可能重叠的未定义字符区间按后定义覆盖先定义规范化为有序互不相交区间
// 入参: ctx 取消上下文, ranges 原始未定义区间列表
// 返回: []cidNotdefRange 规范化区间列表, error 取消错误
func normalizeCIDNotdefRanges(ctx context.Context, ranges []cidNotdefRange) ([]cidNotdefRange, error) {
	if len(ranges) <= 1 {
		return ranges, ctx.Err()
	}
	slices.SortFunc(ranges, func(a, b cidNotdefRange) int {
		if d := cmp.Compare(a.first, b.first); d != 0 {
			return d
		}
		return cmp.Compare(b.order, a.order)
	})
	var heap []cidNotdefRange
	push := func(entry cidNotdefRange) {
		heap = append(heap, entry)
		for idx := len(heap) - 1; idx > 0; {
			parent := (idx - 1) / 2
			if heap[parent].order >= heap[idx].order {
				break
			}
			heap[parent], heap[idx] = heap[idx], heap[parent]
			idx = parent
		}
	}
	pop := func() {
		last := len(heap) - 1
		heap[0] = heap[last]
		heap = heap[:last]
		for idx := 0; ; {
			left := idx*2 + 1
			if left >= len(heap) {
				break
			}
			best := left
			if right := left + 1; right < len(heap) && heap[right].order > heap[left].order {
				best = right
			}
			if heap[idx].order >= heap[best].order {
				break
			}
			heap[idx], heap[best] = heap[best], heap[idx]
			idx = best
		}
	}
	out := make([]cidNotdefRange, 0, len(ranges))
	var pos uint64
	i, steps := 0, 0
	for i < len(ranges) || len(heap) > 0 {
		if steps&255 == 0 {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
		}
		steps++
		if len(heap) == 0 {
			pos = ranges[i].first
		}
		for i < len(ranges) && ranges[i].first == pos {
			push(ranges[i])
			i++
		}
		for len(heap) > 0 && heap[0].last < pos {
			pop()
		}
		if len(heap) == 0 {
			continue
		}
		top := heap[0]
		segEnd := top.last
		if i < len(ranges) && ranges[i].first <= segEnd {
			segEnd = ranges[i].first - 1
		}
		if n := len(out); n > 0 && out[n-1].first>>32 == pos>>32 && out[n-1].last+1 == pos && out[n-1].cid == top.cid {
			out[n-1].last = segEnd
		} else {
			out = append(out, cidNotdefRange{first: pos, last: segEnd, cid: top.cid})
		}
		if segEnd == top.last {
			pop()
		}
		pos = segEnd + 1
	}
	return out, ctx.Err()
}

// normalizeCIDMappedRanges 将可能重叠的CID区间按后定义覆盖先定义规范化为有序互不相交区间
// 入参: ctx 取消上下文, ranges 原始CID区间列表
// 返回: []cidMappedRange 规范化区间列表, error 取消错误
func normalizeCIDMappedRanges(ctx context.Context, ranges []cidMappedRange) ([]cidMappedRange, error) {
	if len(ranges) <= 1 {
		return ranges, ctx.Err()
	}
	slices.SortFunc(ranges, func(a, b cidMappedRange) int {
		if d := cmp.Compare(a.first, b.first); d != 0 {
			return d
		}
		return cmp.Compare(b.order, a.order)
	})
	var heap []cidMappedRange
	push := func(entry cidMappedRange) {
		heap = append(heap, entry)
		for idx := len(heap) - 1; idx > 0; {
			parent := (idx - 1) / 2
			if heap[parent].order >= heap[idx].order {
				break
			}
			heap[parent], heap[idx] = heap[idx], heap[parent]
			idx = parent
		}
	}
	pop := func() {
		last := len(heap) - 1
		heap[0] = heap[last]
		heap = heap[:last]
		for idx := 0; ; {
			left := idx*2 + 1
			if left >= len(heap) {
				break
			}
			best := left
			if right := left + 1; right < len(heap) && heap[right].order > heap[left].order {
				best = right
			}
			if heap[idx].order >= heap[best].order {
				break
			}
			heap[idx], heap[best] = heap[best], heap[idx]
			idx = best
		}
	}
	out := make([]cidMappedRange, 0, len(ranges))
	var pos uint64
	i, steps := 0, 0
	for i < len(ranges) || len(heap) > 0 {
		if steps&255 == 0 {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
		}
		steps++
		if len(heap) == 0 {
			pos = ranges[i].first
		}
		for i < len(ranges) && ranges[i].first == pos {
			push(ranges[i])
			i++
		}
		for len(heap) > 0 && heap[0].last < pos {
			pop()
		}
		if len(heap) == 0 {
			continue
		}
		top := heap[0]
		segEnd := top.last
		if i < len(ranges) && ranges[i].first <= segEnd {
			segEnd = ranges[i].first - 1
		}
		var segCID uint16
		if !top.invalid {
			segCID = top.cid + uint16(pos-top.first)
		}
		if n := len(out); n > 0 && out[n-1].first>>32 == pos>>32 && out[n-1].last+1 == pos && out[n-1].invalid == top.invalid && (top.invalid || uint64(out[n-1].cid)+(pos-out[n-1].first) == uint64(segCID)) {
			out[n-1].last = segEnd
		} else {
			out = append(out, cidMappedRange{first: pos, last: segEnd, cid: segCID, invalid: top.invalid})
		}
		if segEnd == top.last {
			pop()
		}
		pos = segEnd + 1
	}
	return out, ctx.Err()
}

// copyUnicodeMapContext 合并独立映射，在复制期间检查取消、总条目数及总字节上限
// 入参: ctx 取消上下文, target 目标映射, source 源映射
// 返回: uint64 合并后总字节数, error 取消或超限错误
func copyUnicodeMapContext(ctx context.Context, target, source UnicodeMap) (uint64, error) {
	totalBytes, err := unicodeMapBytes(ctx, target)
	if err != nil {
		return 0, err
	}
	index := 0
	for code, text := range source {
		if index&255 == 0 {
			if err := ctx.Err(); err != nil {
				return 0, err
			}
		}
		index++
		old, exists := target[code]
		if !exists && uint64(len(target)) >= maxCMapEntries {
			return 0, fmt.Errorf("%w: unicode cmap entry limit exceeded", ErrStreamLimitExceeded)
		}
		add := uint64(len(code) + len(text))
		if exists {
			if len(text) > len(old) {
				add = uint64(len(text) - len(old))
			} else {
				add = 0
			}
		}
		if add > maxCMapTotalBytes-totalBytes {
			return 0, fmt.Errorf("%w: unicode cmap byte limit exceeded", ErrStreamLimitExceeded)
		}
		totalBytes += add
		target[code] = text
	}
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	return totalBytes, nil
}

// inherit 补齐基础映射的编码范围、字符集及书写方向
func (c *cidCMap) inherit() {
	if c.base == nil {
		return
	}
	c.recovered = c.recovered || c.base.recovered
	if len(c.base.spaces) != 0 {
		if len(c.spaces) == 0 {
			c.spaces = c.base.spaces
			c.spaceByFirst = c.base.spaceByFirst
			c.spaceMinByFirst = c.base.spaceMinByFirst
			c.spaceSingle = c.base.spaceSingle
			c.spaceSecondMask = c.base.spaceSecondMask
			c.spaceSecond = c.base.spaceSecond
			c.minSpace = c.base.minSpace
		} else {
			seen := make(map[cidCodeRange]struct{}, len(c.spaces)+len(c.base.spaces))
			merged := make([]cidCodeRange, 0, len(c.spaces)+len(c.base.spaces))
			for _, space := range c.spaces {
				if _, exists := seen[space]; !exists {
					seen[space] = struct{}{}
					merged = append(merged, space)
				}
			}
			for _, space := range c.base.spaces {
				if _, exists := seen[space]; !exists {
					seen[space] = struct{}{}
					merged = append(merged, space)
				}
			}
			c.spaces = merged
			_ = c.buildSpaceIndex(context.Background())
		}
	}
	if c.registry == "" {
		c.registry = c.base.registry
	}
	if c.ordering == "" {
		c.ordering = c.base.ordering
	}
	if !c.modeSet {
		c.vertical = c.base.vertical
	}
}

// nextContext 按编码范围取出字符并周期性检查取消，防止大编码表长时间阻塞
// 入参: ctx 取消上下文, data 剩余字符串
// 返回: int 字节数, bool 是否有效字符码, error 取消错误
func (c *cidCMap) nextContext(ctx context.Context, data []byte) (int, bool, error) {
	return c.nextWithSteps(ctx, data, nil, 0)
}

// nextWithSteps 按一二级分桶与位图剪枝匹配字符码，可选累计范围扫描步数上限
// 入参: ctx 取消上下文, data 剩余字符串, steps 累计步数计数器，可为空, limit 最大步数上限
// 返回: int 字节数, bool 是否有效字符码, error 超限或取消错误
func (c *cidCMap) nextWithSteps(ctx context.Context, data []byte, steps *int, limit int) (int, bool, error) {
	if len(data) == 0 {
		return 0, false, ctx.Err()
	}
	b0 := data[0]
	bucket := c.spaceByFirst[b0]
	if len(bucket) == 0 {
		if c.minSpace != 0 {
			return min(int(c.minSpace), len(data)), false, nil
		}
		return min(5, len(data)), false, nil
	}
	if err := ctx.Err(); err != nil {
		return 0, false, err
	}
	singleIdx := int(c.spaceSingle[b0]) - 1
	if len(data) == 1 {
		if singleIdx >= 0 {
			return 1, true, nil
		}
		return 1, false, nil
	}
	if c.spaceSecondMask != nil && len(bucket) > 16 {
		b1 := data[1]
		if c.spaceSecondMask[b0][b1>>6]&(1<<(b1&63)) == 0 {
			if singleIdx >= 0 {
				return 1, true, nil
			}
			return min(int(c.spaceMinByFirst[b0]), len(data)), false, nil
		}
		if sb := c.spaceSecond[b0]; sb != nil {
			sub := sb.bySecond[b1]
			length, longest := 5, -1
			for i, idx := range sub {
				if i&255 == 0 {
					if err := ctx.Err(); err != nil {
						return 0, false, err
					}
				}
				if singleIdx >= 0 && int(idx) > singleIdx {
					return 1, true, nil
				}
				if i > 0 && steps != nil {
					*steps++
					if *steps > limit {
						return 0, false, fmt.Errorf("%w: CID CMap reverse lookup limit exceeded", ErrStreamLimitExceeded)
					}
				}
				space := c.spaces[idx]
				size := int(space.size)
				matched := 2
				for matched < size && matched < len(data) && data[matched] >= space.first[matched] && data[matched] <= space.last[matched] {
					matched++
				}
				if matched == size {
					return matched, true, nil
				}
				if matched > longest || matched == longest && size < length {
					length, longest = size, matched
				}
			}
			if singleIdx >= 0 {
				return 1, true, nil
			}
			return min(length, len(data)), false, nil
		}
	}
	length, longest := 5, -1
	for i, idx := range bucket {
		if i&255 == 0 {
			if err := ctx.Err(); err != nil {
				return 0, false, err
			}
		}
		if i > 0 && steps != nil {
			*steps++
			if *steps > limit {
				return 0, false, fmt.Errorf("%w: CID CMap reverse lookup limit exceeded", ErrStreamLimitExceeded)
			}
		}
		space := c.spaces[idx]
		size := int(space.size)
		matched := 1
		for matched < size && matched < len(data) && data[matched] >= space.first[matched] && data[matched] <= space.last[matched] {
			matched++
		}
		if matched == size {
			return matched, true, nil
		}
		if matched > longest || matched == longest && size < length {
			length, longest = size, matched
		}
	}
	return min(length, len(data)), false, nil
}

// lookup 按派生优先规则查找CID，未定义字符使用notdef或CID零
// 入参: raw 字符码, valid 是否匹配编码范围
// 返回: uint16 字符标识
func (c *cidCMap) lookup(raw []byte, valid bool) uint16 {
	if valid {
		for current := c; current != nil; current = current.base {
			if cid, ok := current.mapped(raw); ok {
				return cid
			}
		}
	}
	return c.undefined(raw)
}

// mapped 二分查找当前层的编码区间，重叠定义以后出现者为准
// 入参: raw 字符码
// 返回: uint16 字符标识, bool 是否存在映射
func (c *cidCMap) mapped(raw []byte) (uint16, bool) {
	entry := c.mappedRange(raw)
	if entry == nil {
		return 0, false
	}
	if entry.invalid {
		return 0, true
	}
	return entry.cid + uint16(codeNumber(raw)-uint64(uint32(entry.first))), true
}

// mappedRange 二分查找规范化后的匹配范围，不将无效目标回退到父映射
// 入参: raw 原始字符码
// 返回: *cidMappedRange 范围项，未匹配时为空
func (c *cidCMap) mappedRange(raw []byte) *cidMappedRange {
	code := uint64(len(raw))<<32 | codeNumber(raw)
	index := sort.Search(len(c.ranges), func(i int) bool { return c.ranges[i].last >= code })
	if index < len(c.ranges) && c.ranges[index].first <= code {
		return &c.ranges[index]
	}
	return nil
}

// validateCode 拒绝实际使用的无效CID范围，未引用的异常条目不影响已知字形
// 入参: raw 字符码, valid 是否匹配编码范围
// 返回: error 无效目标错误
func (c *cidCMap) validateCode(raw []byte, valid bool) error {
	if valid {
		for current := c; current != nil; current = current.base {
			if entry := current.mappedRange(raw); entry != nil {
				if entry.invalid {
					return fmt.Errorf("invalid CMap CID destination for character %x", raw)
				}
				return nil
			}
		}
	}
	return nil
}

// characterCodes 反查指定CID的最优有效字符码并跳过已显式覆写的字符码
// 入参: ctx 取消上下文, cids 升序排列的字符标识, skip 需跳过的显式Unicode映射，可为空
// 返回: []string 确定顺序的最优字符码, error 超限或取消错误
func (c *cidCMap) characterCodes(ctx context.Context, cids []uint16, skip UnicodeMap) ([]string, error) {
	if len(cids) == 0 {
		return nil, ctx.Err()
	}
	dup := false
	for i := 1; i < len(cids); i++ {
		if cids[i] == cids[i-1] {
			dup = true
			break
		}
	}
	if dup {
		cids = slices.Compact(slices.Clone(cids))
	}
	var validFirst [5][256]bool
	for _, space := range c.spaces {
		if space.size >= 1 && space.size <= 4 {
			for b := int(space.first[0]); b <= int(space.last[0]); b++ {
				validFirst[space.size][b] = true
			}
		}
	}
	best := make(map[uint16]uint64, min(len(cids), 256))
	var worstBest uint64
	steps := 0
	var layerCIDs []uint16
	var layerDone []bool
	for current := c; current != nil; current = current.base {
		if len(current.ranges) == 0 {
			continue
		}
		minFirst := current.ranges[0].first
		if len(best) == len(cids) && worstBest <= minFirst {
			continue
		}
		layerCIDs = layerCIDs[:0]
		if len(best) == 0 {
			layerCIDs = append(layerCIDs, cids...)
		} else {
			for _, cid := range cids {
				if prev, ok := best[cid]; !ok || minFirst < prev {
					layerCIDs = append(layerCIDs, cid)
				}
			}
		}
		if len(layerCIDs) == 0 {
			continue
		}
		if cap(layerDone) < len(layerCIDs) {
			layerDone = make([]bool, len(layerCIDs))
		} else {
			layerDone = layerDone[:len(layerCIDs)]
			clear(layerDone)
		}
		doneCount := 0
		improved := false
		for _, entry := range current.ranges {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			if entry.invalid {
				continue
			}
			if len(best) == len(cids) && entry.first >= worstBest {
				break
			}
			size := int(entry.first >> 32)
			if size < 1 || size > 4 {
				continue
			}
			shift := uint((size - 1) * 8)
			firstB0, lastB0 := int(byte(entry.first>>shift)), int(byte(entry.last>>shift))
			hasValidFirst := false
			for b := firstB0; b <= lastB0; b++ {
				if validFirst[size][b] {
					hasValidFirst = true
					break
				}
			}
			if !hasValidFirst {
				continue
			}
			start, _ := slices.BinarySearch(layerCIDs, entry.cid)
			last := uint64(entry.cid) + entry.last - entry.first
			for idx := start; idx < len(layerCIDs); idx++ {
				cid := layerCIDs[idx]
				if uint64(cid) > last {
					break
				}
				if layerDone[idx] {
					continue
				}
				code := entry.first + uint64(cid-entry.cid)
				if prev, ok := best[cid]; ok && code >= prev {
					layerDone[idx] = true
					doneCount++
					continue
				}
				if steps&255 == 0 {
					if err := ctx.Err(); err != nil {
						return nil, err
					}
				}
				steps++
				if steps > maxCIDReverseSteps {
					return nil, fmt.Errorf("%w: CID CMap reverse lookup limit exceeded", ErrStreamLimitExceeded)
				}
				if !validFirst[size][byte(code>>shift)] {
					continue
				}
				var raw [4]byte
				for i := 0; i < size; i++ {
					raw[size-i-1] = byte(code >> (i * 8))
				}
				data := raw[:size]
				if len(skip) != 0 {
					if _, overridden := skip[string(data)]; overridden {
						continue
					}
				}
				n, valid, err := c.nextWithSteps(ctx, data, &steps, maxCIDReverseSteps)
				if err != nil {
					return nil, err
				}
				if valid && n == size && c.validateCode(data, true) == nil && c.lookup(data, true) == cid {
					_, existed := best[cid]
					best[cid] = code
					improved = true
					layerDone[idx] = true
					doneCount++
					if !existed && len(best) == len(cids) {
						worstBest = 0
						for _, found := range best {
							if found > worstBest {
								worstBest = found
							}
						}
					}
				}
			}
			if doneCount > 0 && doneCount*2 >= len(layerCIDs) {
				kept := 0
				for i, cid := range layerCIDs {
					if !layerDone[i] {
						layerCIDs[kept] = cid
						kept++
					}
				}
				layerCIDs = layerCIDs[:kept]
				if len(layerCIDs) == 0 {
					break
				}
				layerDone = layerDone[:kept]
				clear(layerDone)
				doneCount = 0
			}
		}
		if improved && len(best) == len(cids) {
			worstBest = 0
			for _, found := range best {
				if found > worstBest {
					worstBest = found
				}
			}
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	codes := make([]string, 0, len(best))
	for _, cid := range cids {
		code, ok := best[cid]
		if !ok {
			continue
		}
		var raw [4]byte
		size := int(code >> 32)
		for i := 0; i < size; i++ {
			raw[size-i-1] = byte(code >> (i * 8))
		}
		codes = append(codes, string(raw[:size]))
	}
	return codes, nil
}

// undefined 二分查找未定义字符的替代标识，合并链仅查询一次
// 入参: raw 字符码
// 返回: uint16 替代标识，未提供时为零
func (c *cidCMap) undefined(raw []byte) uint16 {
	code := uint64(len(raw))<<32 | codeNumber(raw)
	for current := c; current != nil; current = current.base {
		index := sort.Search(len(current.notdef), func(i int) bool { return current.notdef[i].last >= code })
		if index < len(current.notdef) && current.notdef[index].first <= code {
			return current.notdef[index].cid
		}
		if current.notdefFlat {
			break
		}
	}
	return 0
}

// addCIDCMapBudget 将预定义基础映射的编码范围、未定义区间与映射区间计入继承链总预算
// 入参: mapping 预定义映射, budget 继承链条目预算
// 返回: error 超限错误
func addCIDCMapBudget(mapping *cidCMap, budget *cidCMapBudget) error {
	if uint64(len(mapping.spaces)) > maxCMapEntries-budget.spaces {
		return fmt.Errorf("%w: CID CMap codespace limit exceeded", ErrStreamLimitExceeded)
	}
	budget.spaces += uint64(len(mapping.spaces))
	if budget.uniqueSpaces == nil {
		budget.uniqueSpaces = make(map[cidCodeRange]struct{}, len(mapping.spaces))
	}
	for _, space := range mapping.spaces {
		if _, exists := budget.uniqueSpaces[space]; !exists {
			if len(budget.uniqueSpaces) >= maxCMapCodespaceRanges {
				return fmt.Errorf("%w: CID CMap codespace limit exceeded", ErrStreamLimitExceeded)
			}
			budget.uniqueSpaces[space] = struct{}{}
		}
	}
	for current := mapping; current != nil; current = current.base {
		if uint64(len(current.notdef)) > maxCMapEntries-budget.notdef {
			return fmt.Errorf("%w: CID CMap notdef limit exceeded", ErrStreamLimitExceeded)
		}
		budget.notdef += uint64(len(current.notdef))
		if uint64(len(current.ranges)) > maxCMapEntries-budget.ranges {
			return fmt.Errorf("%w: CID CMap range limit exceeded", ErrStreamLimitExceeded)
		}
		budget.ranges += uint64(len(current.ranges))
	}
	return nil
}

// finalizeCIDCMapChain 一次性按派生优先去重合并继承链的编码范围并规范化未定义区间，释放中间层冗余拷贝
// 入参: ctx 取消上下文, mapping 顶层映射, owned 本次流解析新建的节点列表
// 返回: error 超限或取消错误
func finalizeCIDCMapChain(ctx context.Context, mapping *cidCMap, owned []*cidCMap) error {
	ownedSet := make(map[*cidCMap]bool, len(owned))
	for _, node := range owned {
		ownedSet[node] = true
	}
	var chain []*cidCMap
	for current := mapping; current != nil; current = current.base {
		chain = append(chain, current)
	}
	seenSpaces := make(map[cidCodeRange]struct{})
	var mergedSpaces []cidCodeRange
	for _, node := range chain {
		for i, space := range node.spaces {
			if i&255 == 0 {
				if err := ctx.Err(); err != nil {
					return err
				}
			}
			if _, exists := seenSpaces[space]; !exists {
				if len(mergedSpaces) >= maxCMapCodespaceRanges {
					return fmt.Errorf("%w: CID CMap codespace limit exceeded", ErrStreamLimitExceeded)
				}
				seenSpaces[space] = struct{}{}
				mergedSpaces = append(mergedSpaces, space)
			}
		}
		if !ownedSet[node] {
			break
		}
	}
	var totalNotdef uint64
	notdefLimit := len(chain)
	for i, node := range chain {
		if uint64(len(node.notdef)) > maxCMapEntries-totalNotdef {
			return fmt.Errorf("%w: CID CMap notdef limit exceeded", ErrStreamLimitExceeded)
		}
		totalNotdef += uint64(len(node.notdef))
		if node.notdefFlat {
			notdefLimit = i + 1
			break
		}
	}
	if notdefLimit > 1 && totalNotdef > 0 {
		allNotdef := make([]cidNotdefRange, 0, totalNotdef)
		for i := notdefLimit - 1; i >= 0; i-- {
			for j, entry := range chain[i].notdef {
				if j&255 == 0 {
					if err := ctx.Err(); err != nil {
						return err
					}
				}
				entry.order = len(allNotdef)
				allNotdef = append(allNotdef, entry)
			}
		}
		normalized, err := normalizeCIDNotdefRanges(ctx, allNotdef)
		if err != nil {
			return err
		}
		mapping.notdef = normalized
	}
	mapping.spaces = mergedSpaces
	mapping.notdefFlat = true
	if err := mapping.buildSpaceIndex(ctx); err != nil {
		return err
	}
	for _, node := range owned {
		if node != mapping {
			node.spaces = nil
			node.spaceByFirst = [256][]uint16{}
			node.spaceMinByFirst = [256]uint8{}
			node.spaceSingle = [256]uint16{}
			node.spaceSecondMask = nil
			node.spaceSecond = nil
			node.notdef = nil
		}
	}
	return ctx.Err()
}

// readCIDCMapContext 读取文档编码并沿用本次上下文解析继承流
// 入参: ctx 取消上下文, object 编码对象
// 返回: *cidCMap 完整映射, error 解析、环路、超限或取消错误
func (r *Reader) readCIDCMapContext(ctx context.Context, object Object) (*cidCMap, error) {
	var owned []*cidCMap
	mapping, _, err := r.readCIDCMapChainContext(ctx, object, nil, nil, 0, &cidCMapBudget{}, &owned)
	if err != nil {
		return nil, err
	}
	if len(owned) != 0 {
		if err := finalizeCIDCMapChain(ctx, mapping, owned); err != nil {
			return nil, err
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return mapping, nil
}

// readCIDCMapChainContext 在共享预算内递归解析CID CMap流继承链，不在中间层重复拼接编码范围
// 入参: ctx 取消上下文, object 编码对象, activeRefs 当前流引用继承链, activeStreams 当前流指针继承链, depth 当前流继承深度, budget 跨继承链条目预算, owned 本次新建流节点列表
// 返回: *cidCMap 当前层映射, bool 当前层或基底是否含编码范围, error 解析、环路、超限或取消错误
func (r *Reader) readCIDCMapChainContext(ctx context.Context, object Object, activeRefs map[Reference]bool, activeStreams map[*Stream]bool, depth int, budget *cidCMapBudget, owned *[]*cidCMap) (*cidCMap, bool, error) {
	if err := ctx.Err(); err != nil {
		return nil, false, err
	}
	ref, err := r.resolveIndirectReference(object)
	if err != nil {
		return nil, false, err
	}
	if ref != (Reference{}) && activeRefs[ref] {
		return nil, false, fmt.Errorf("cyclic CMap stream inheritance")
	}
	value, err := r.Resolve(object)
	if err != nil {
		return nil, false, err
	}
	if name, ok := value.(Name); ok {
		loaded, err := loadCIDCMapContext(ctx, name, nil)
		if err != nil {
			return nil, false, err
		}
		if err := addCIDCMapBudget(loaded, budget); err != nil {
			return nil, false, err
		}
		return loaded, len(loaded.spaces) > 0, nil
	}
	stream, ok := value.(*Stream)
	if !ok || stream == nil {
		return nil, false, fmt.Errorf("invalid composite font encoding")
	}
	if activeStreams[stream] {
		return nil, false, fmt.Errorf("cyclic CMap stream inheritance")
	}
	if depth >= maxCMapStreamDepth {
		return nil, false, fmt.Errorf("%w: CMap stream inheritance depth limit exceeded", ErrStreamLimitExceeded)
	}
	if activeRefs == nil {
		activeRefs = map[Reference]bool{}
	}
	if activeStreams == nil {
		activeStreams = map[*Stream]bool{}
	}
	if ref != (Reference{}) {
		activeRefs[ref] = true
		defer delete(activeRefs, ref)
	}
	activeStreams[stream] = true
	defer delete(activeStreams, stream)
	data, err := r.DecodeStreamContext(ctx, stream)
	if err != nil {
		return nil, false, err
	}
	mapping, err := parseCIDCMapBudgetIndexContext(ctx, data, r.warning, budget, false)
	if err != nil {
		return nil, false, err
	}
	useRaw := stream.Dictionary["UseCMap"]
	base, err := r.Resolve(useRaw)
	if err != nil {
		return nil, false, err
	}
	var next Object
	if base != nil {
		next = useRaw
	} else if mapping.use != "" {
		next = mapping.use
	}
	baseHasSpace := false
	if next != nil {
		mapping.base, baseHasSpace, err = r.readCIDCMapChainContext(ctx, next, activeRefs, activeStreams, depth+1, budget, owned)
		if err != nil {
			return nil, false, err
		}
	}
	if mode, err := r.Resolve(stream.Dictionary["WMode"]); err != nil {
		return nil, false, err
	} else if mode != nil {
		if mode != Integer(0) && mode != Integer(1) {
			return nil, false, fmt.Errorf("invalid CMap writing mode")
		}
		mapping.vertical, mapping.modeSet = mode == Integer(1), true
	}
	if mapping.base != nil {
		mapping.recovered = mapping.recovered || mapping.base.recovered
		if mapping.registry == "" {
			mapping.registry = mapping.base.registry
		}
		if mapping.ordering == "" {
			mapping.ordering = mapping.base.ordering
		}
		if !mapping.modeSet {
			mapping.vertical = mapping.base.vertical
		}
	}
	hasSpace := len(mapping.spaces) > 0 || baseHasSpace
	if !hasSpace {
		return nil, false, fmt.Errorf("missing CMap codespace")
	}
	*owned = append(*owned, mapping)
	if err := ctx.Err(); err != nil {
		return nil, false, err
	}
	return mapping, true, nil
}

// readUnicodeCMapContext 读取独立Unicode映射，流继承及合并均检查取消与安全上限
// 入参: ctx 取消上下文, object 映射对象, activeRefs 当前流引用继承链, activeStreams 当前流指针继承链, depth 当前流继承深度
// 返回: UnicodeMap 完整映射, error 解析、环路、超限或取消错误
func (r *Reader) readUnicodeCMapContext(ctx context.Context, object Object, activeRefs map[Reference]bool, activeStreams map[*Stream]bool, depth int) (UnicodeMap, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	ref, err := r.resolveIndirectReference(object)
	if err != nil {
		return nil, err
	}
	if ref != (Reference{}) && activeRefs[ref] {
		return nil, fmt.Errorf("cyclic ToUnicode stream inheritance")
	}
	value, err := r.Resolve(object)
	if err != nil || value == nil {
		return nil, err
	}
	if name, ok := value.(Name); ok {
		mapping, err := loadUnicodeCMapContext(ctx, name, nil)
		if err != nil {
			return nil, err
		}
		result := make(UnicodeMap, len(mapping))
		if _, err := copyUnicodeMapContext(ctx, result, mapping); err != nil {
			return nil, err
		}
		return result, nil
	}
	stream, ok := value.(*Stream)
	if !ok || stream == nil {
		return nil, fmt.Errorf("invalid ToUnicode")
	}
	if activeStreams[stream] {
		return nil, fmt.Errorf("cyclic ToUnicode stream inheritance")
	}
	if depth >= maxCMapStreamDepth {
		return nil, fmt.Errorf("%w: ToUnicode stream inheritance depth limit exceeded", ErrStreamLimitExceeded)
	}
	if activeRefs == nil {
		activeRefs = map[Reference]bool{}
	}
	if activeStreams == nil {
		activeStreams = map[*Stream]bool{}
	}
	if ref != (Reference{}) {
		activeRefs[ref] = true
		defer delete(activeRefs, ref)
	}
	activeStreams[stream] = true
	defer delete(activeStreams, stream)
	useRaw := stream.Dictionary["UseCMap"]
	base, err := r.Resolve(useRaw)
	if err != nil {
		return nil, err
	}
	var mapping UnicodeMap
	var baseStreamName Name
	if base != nil {
		if baseStream, ok := base.(*Stream); ok && baseStream != nil {
			nameVal, err := r.Resolve(baseStream.Dictionary["CMapName"])
			if err != nil {
				return nil, err
			}
			if name, ok := nameVal.(Name); ok {
				baseStreamName = name
			}
		}
		mapping, err = r.readUnicodeCMapContext(ctx, useRaw, activeRefs, activeStreams, depth+1)
		if err != nil {
			return nil, err
		}
	} else {
		mapping = UnicodeMap{}
	}
	data, err := r.DecodeStreamContext(ctx, stream)
	if err != nil {
		return nil, err
	}
	if err := parseUnicodeMapIntoContext(ctx, mapping, data, nil, baseStreamName); err != nil {
		return nil, err
	}
	return mapping, nil
}
