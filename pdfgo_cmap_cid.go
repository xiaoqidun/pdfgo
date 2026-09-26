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
	"fmt"
	"maps"
	"slices"
	"sort"
	"strconv"
	"strings"
)

// CIDMap 保存字符码到CID的映射
type CIDMap map[string]uint16

// cidCodeRange 保存逐字节判定的字符码范围
type cidCodeRange struct {
	first String
	last  String
}

// cidNotdefRange 保存未定义字符到单个替代CID的映射
type cidNotdefRange struct {
	first uint64
	last  uint64
	size  int
	cid   uint16
}

// cidMappedRange 保留连续CID区间及定义顺序，避免展开大量逐字符映射
type cidMappedRange struct {
	first uint64
	last  uint64
	end   uint64
	cid   uint16
	order int
}

// cidCMap 保存编码范围、字形选择和只读继承关系
type cidCMap struct {
	ranges   []cidMappedRange
	spaces   []cidCodeRange
	notdef   []cidNotdefRange
	registry string
	ordering string
	vertical bool
	modeSet  bool
	use      Name
	base     *cidCMap
}

// ParseCIDMap 读取CMap中的字符码到CID的直接映射及连续范围
// 入参: data CMap数据
// 返回: CIDMap 字符映射, error 错误信息
func ParseCIDMap(data []byte) (CIDMap, error) {
	mapping, err := parseCIDCMap(data)
	if err != nil {
		return nil, err
	}
	if mapping.use != "" {
		mapping.base, err = loadCIDCMap(mapping.use, nil)
		if err != nil {
			return nil, err
		}
	}
	result := CIDMap{}
	for current := mapping; current != nil; current = current.base {
		for _, entry := range current.ranges {
			for code := entry.first; code <= entry.last; code++ {
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
	return result, nil
}

// parseCIDCMap 读取编码元数据及映射，不执行PostScript程序
// 入参: data CMap数据
// 返回: *cidCMap 本层映射, error 格式错误
func parseCIDCMap(data []byte) (*cidCMap, error) {
	p := objectParser{data: data}
	result := &cidCMap{}
	previous := ""
	var name Name
	for {
		p.skipSpace()
		if p.pos == len(data) {
			slices.SortFunc(result.ranges, func(a, b cidMappedRange) int { return cmp.Compare(a.first, b.first) })
			var end uint64
			for i := range result.ranges {
				end = max(end, result.ranges[i].last)
				result.ranges[i].end = end
			}
			return result, nil
		}
		if b := p.data[p.pos]; b == '/' || b == '(' || b == '<' || b == '[' {
			object, err := p.object()
			if err != nil {
				return nil, err
			}
			name, _ = object.(Name)
			if name == "WMode" || name == "Registry" || name == "Ordering" || name == "CMapType" {
				value, err := p.object()
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
						return nil, p.fail("invalid CID CMap type")
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
		if token == "usefont" && previous != "0" || token == "beginbfchar" || token == "beginbfrange" || token == "beginrearrangedfont" || token == "beginusematrix" {
			return nil, &UnsupportedError{Feature: "CID CMap operator " + token}
		}
		if token != "begincidchar" && token != "begincidrange" && token != "begincodespacerange" && token != "beginnotdefchar" && token != "beginnotdefrange" {
			previous = token
			name = ""
			continue
		}
		count, err := strconv.Atoi(previous)
		if err != nil || count < 0 {
			return nil, p.fail("invalid CMap mapping count")
		}
		for range count {
			from, err := p.object()
			if err != nil {
				return nil, err
			}
			first, ok := from.(String)
			if !ok || len(first) == 0 || len(first) > 4 {
				return nil, p.fail("invalid CMap source code")
			}
			last := first
			if strings.HasSuffix(token, "range") {
				to, err := p.object()
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
				result.spaces = append(result.spaces, cidCodeRange{first, last})
				continue
			}
			value, err := p.object()
			if err != nil {
				return nil, err
			}
			cid, ok := value.(Integer)
			if !ok || cid < 0 || cid > 65535 {
				return nil, p.fail("invalid CMap CID")
			}
			if token == "beginnotdefchar" || token == "beginnotdefrange" {
				result.notdef = append(result.notdef, cidNotdefRange{start, end, len(first), uint16(cid)})
				continue
			}
			if uint64(cid)+end-start > 65535 {
				return nil, p.fail("invalid CMap CID range")
			}
			size := uint64(len(first)) << 32
			result.ranges = append(result.ranges, cidMappedRange{first: size | start, last: size | end, cid: uint16(cid), order: len(result.ranges)})
		}
		end := "end" + strings.TrimPrefix(token, "begin")
		if p.token() != end {
			return nil, p.fail("missing CMap block terminator")
		}
		previous = ""
	}
}

// inherit 补齐基础映射的编码范围、字符集及书写方向
func (c *cidCMap) inherit() {
	if c.base == nil {
		return
	}
	if len(c.spaces) == 0 {
		c.spaces = c.base.spaces
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

// next 按编码范围取出字符，无效编码依标准选择最长部分匹配的消耗长度
// 入参: data 剩余字符串
// 返回: int 字节数, bool 是否有效字符码
func (c *cidCMap) next(data []byte) (int, bool) {
	length, longest := 5, -1
	for _, space := range c.spaces {
		matched := 0
		for matched < len(space.first) && matched < len(data) && data[matched] >= space.first[matched] && data[matched] <= space.last[matched] {
			matched++
		}
		if matched == len(space.first) {
			return matched, true
		}
		if matched > longest || matched == longest && len(space.first) < length {
			length, longest = len(space.first), matched
		}
	}
	return min(length, len(data)), false
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
	code := uint64(len(raw))<<32 | codeNumber(raw)
	index := sort.Search(len(c.ranges), func(i int) bool { return c.ranges[i].first > code })
	order := -1
	var cid uint16
	for i := index - 1; i >= 0 && c.ranges[i].end >= code; i-- {
		entry := c.ranges[i]
		if code <= entry.last && entry.order > order {
			cid, order = entry.cid+uint16(code-entry.first), entry.order
		}
	}
	return cid, order >= 0
}

// undefined 查找未定义字符的替代标识
// 入参: raw 字符码
// 返回: uint16 替代标识，未提供时为零
func (c *cidCMap) undefined(raw []byte) uint16 {
	code := codeNumber(raw)
	for current := c; current != nil; current = current.base {
		for i := len(current.notdef) - 1; i >= 0; i-- {
			entry := current.notdef[i]
			if len(raw) == entry.size && code >= entry.first && code <= entry.last {
				return entry.cid
			}
		}
	}
	return 0
}

// readCIDCMap 读取命名或文档内嵌编码，处理流字典中的继承关系
// 入参: object 编码对象, active 当前流继承链
// 返回: *cidCMap 编码映射, error 解析错误
func (r *Reader) readCIDCMap(object Object, active map[*Stream]bool) (*cidCMap, error) {
	value, err := r.Resolve(object)
	if err != nil {
		return nil, err
	}
	if name, ok := value.(Name); ok {
		return loadCIDCMap(name, nil)
	}
	stream, ok := value.(*Stream)
	if !ok {
		return nil, fmt.Errorf("invalid composite font encoding")
	}
	if active[stream] {
		return nil, fmt.Errorf("cyclic CMap stream inheritance")
	}
	if active == nil {
		active = map[*Stream]bool{}
	}
	active[stream] = true
	defer delete(active, stream)
	data, err := stream.Decode()
	if err != nil {
		return nil, err
	}
	mapping, err := parseCIDCMap(data)
	if err != nil {
		return nil, err
	}
	base, err := r.Resolve(stream.Dictionary["UseCMap"])
	if err != nil {
		return nil, err
	}
	if base == nil && mapping.use != "" {
		base = mapping.use
	}
	if base != nil {
		mapping.base, err = r.readCIDCMap(base, active)
		if err != nil {
			return nil, err
		}
	}
	if mode, err := r.Resolve(stream.Dictionary["WMode"]); err != nil {
		return nil, err
	} else if mode != nil {
		if mode != Integer(0) && mode != Integer(1) {
			return nil, fmt.Errorf("invalid CMap writing mode")
		}
		mapping.vertical, mapping.modeSet = mode == Integer(1), true
	}
	mapping.inherit()
	if len(mapping.spaces) == 0 {
		return nil, fmt.Errorf("missing CMap codespace")
	}
	return mapping, nil
}

// readUnicodeCMap 读取ToUnicode流及基础映射，派生映射覆盖同码定义
// 入参: object 映射对象, active 当前流继承链
// 返回: UnicodeMap 独立映射, error 解析错误
func (r *Reader) readUnicodeCMap(object Object, active map[*Stream]bool) (UnicodeMap, error) {
	value, err := r.Resolve(object)
	if err != nil {
		return nil, err
	}
	if name, ok := value.(Name); ok {
		mapping, err := loadUnicodeCMap(name, nil)
		return maps.Clone(mapping), err
	}
	stream, ok := value.(*Stream)
	if !ok {
		return nil, fmt.Errorf("invalid ToUnicode")
	}
	if active[stream] {
		return nil, fmt.Errorf("cyclic ToUnicode stream inheritance")
	}
	if active == nil {
		active = map[*Stream]bool{}
	}
	active[stream] = true
	defer delete(active, stream)
	data, err := stream.Decode()
	if err != nil {
		return nil, err
	}
	result, err := ParseUnicodeMap(data)
	if err != nil {
		return nil, err
	}
	base, err := r.Resolve(stream.Dictionary["UseCMap"])
	if err != nil || base == nil {
		return result, err
	}
	mapping, err := r.readUnicodeCMap(base, active)
	if err != nil {
		return nil, err
	}
	maps.Copy(mapping, result)
	return mapping, nil
}
