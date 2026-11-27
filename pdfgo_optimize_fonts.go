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
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"maps"
	"reflect"
	"slices"
	"strings"
	"sync"
)

// compactTrueType 裁剪静态TrueType轮廓并移除失效点阵与辅助表，保留字形依赖与提示并同步loca/head表
// 入参: ctx 取消上下文, data 字体程序, glyphs 可寻址字形
// 返回: []byte 更小的程序或原文, error 取消错误
func compactTrueType(ctx context.Context, data []byte, glyphs []uint16) ([]byte, error) {
	if len(data) < 12 || binary.BigEndian.Uint32(data) != 0x10000 {
		return data, nil
	}
	n := int(binary.BigEndian.Uint16(data[4:]))
	if n == 0 || n > (len(data)-12)/16 {
		return data, nil
	}
	tables := make(map[string][]byte, n)
	for i := 0; i < n; i++ {
		tag := string(data[12+i*16 : 16+i*16])
		if _, exists := tables[tag]; exists {
			return data, nil
		}
		switch tag {
		case "cmap", "head", "hhea", "hmtx", "maxp", "OS/2", "post", "name", "glyf", "loca", "cvt ", "fpgm", "prep", "gasp", "kern", "vhea", "vmtx", "DSIG", "hdmx", "LTSH", "VDMX", "GDEF", "GPOS", "GSUB", "BASE", "MATH", "meta", "PCLT", "FFTM", "STAT", "EBDT", "EBLC", "EBSC", "JSTF", "MERG":
		default:
			return data, nil
		}
		table, err := fontTable(data, tag)
		if err != nil {
			return data, nil
		}
		tables[tag] = table
	}
	head, maxp, loca, glyf := tables["head"], tables["maxp"], tables["loca"], tables["glyf"]
	if os2 := tables["OS/2"]; len(os2) >= 10 && binary.BigEndian.Uint16(os2[8:])&0x100 != 0 {
		return data, nil
	}
	if len(head) < 54 || len(maxp) < 6 || loca == nil || glyf == nil {
		return data, nil
	}
	count := int(binary.BigEndian.Uint16(maxp[4:]))
	format := binary.BigEndian.Uint16(head[50:])
	if format > 1 || len(loca) < (count+1)*(2+int(format)*2) {
		return data, nil
	}
	offsets := make([]int, count+1)
	for i := range offsets {
		if format == 0 {
			offsets[i] = int(binary.BigEndian.Uint16(loca[2*i:])) * 2
		} else {
			u := uint64(binary.BigEndian.Uint32(loca[4*i:]))
			if u > uint64(len(glyf)) {
				return data, nil
			}
			offsets[i] = int(u)
		}
		if offsets[i] > len(glyf) || i > 0 && offsets[i] < offsets[i-1] {
			return data, nil
		}
	}
	keep := make([]bool, count)
	queue := append([]uint16{0}, glyphs...)
	for len(queue) > 0 {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		id := int(queue[len(queue)-1])
		queue = queue[:len(queue)-1]
		if id >= count {
			return data, nil
		}
		if keep[id] {
			continue
		}
		keep[id] = true
		g := glyf[offsets[id]:offsets[id+1]]
		if len(g) == 0 {
			continue
		}
		if len(g) < 10 {
			return data, nil
		}
		if int16(binary.BigEndian.Uint16(g)) >= 0 {
			continue
		}
		for pos := 10; ; {
			if pos+4 > len(g) {
				return data, nil
			}
			flags := binary.BigEndian.Uint16(g[pos:])
			child := binary.BigEndian.Uint16(g[pos+2:])
			queue = append(queue, child)
			pos += 4
			if flags&1 != 0 {
				pos += 4
			} else {
				pos += 2
			}
			if flags&8 != 0 {
				pos += 2
			}
			if flags&64 != 0 {
				pos += 4
			}
			if flags&128 != 0 {
				pos += 8
			}
			if pos > len(g) {
				return data, nil
			}
			if flags&32 == 0 {
				break
			}
		}
	}
	var compact []byte
	newOffsets := make([]int, count+1)
	for id := 0; id <= count; id++ {
		newOffsets[id] = len(compact)
		if id < count && keep[id] {
			compact = append(compact, glyf[offsets[id]:offsets[id+1]]...)
			if len(compact)%2 != 0 {
				compact = append(compact, 0)
			}
		}
	}
	if len(compact) == 0 {
		return data, nil
	}
	newFormat := uint16(1)
	if len(compact) <= 0x1fffe {
		newFormat = 0
	}
	locations := make([]byte, 0, (count+1)*(2+int(newFormat)*2))
	for _, off := range newOffsets {
		if newFormat == 0 {
			locations = binary.BigEndian.AppendUint16(locations, uint16(off/2))
		} else {
			locations = binary.BigEndian.AppendUint32(locations, uint32(off))
		}
	}
	tables["glyf"], tables["loca"] = compact, locations
	tables["head"] = bytes.Clone(head)
	binary.BigEndian.PutUint16(tables["head"][50:], newFormat)
	clear(tables["head"][8:12])
	for _, tag := range []string{"DSIG", "EBDT", "EBLC", "EBSC", "hdmx", "LTSH", "VDMX", "JSTF", "MERG"} {
		delete(tables, tag)
	}
	tableCount := len(tables)
	entrySelector := 0
	for 1<<(entrySelector+1) <= tableCount {
		entrySelector++
	}
	searchRange := 1 << (entrySelector + 4)
	rangeShift := tableCount*16 - searchRange
	out := make([]byte, 12+tableCount*16)
	binary.BigEndian.PutUint32(out, 0x10000)
	binary.BigEndian.PutUint16(out[4:], uint16(tableCount))
	binary.BigEndian.PutUint16(out[6:], uint16(searchRange))
	binary.BigEndian.PutUint16(out[8:], uint16(entrySelector))
	binary.BigEndian.PutUint16(out[10:], uint16(rangeShift))
	headOffset := 0
	for i, tag := range slices.Sorted(maps.Keys(tables)) {
		payload := tables[tag]
		offset := len(out)
		out = append(out, payload...)
		for len(out)%4 != 0 {
			out = append(out, 0)
		}
		record := out[12+i*16:]
		copy(record[:4], tag)
		binary.BigEndian.PutUint32(record[4:], fontChecksum(out[offset:]))
		binary.BigEndian.PutUint32(record[8:], uint32(offset))
		binary.BigEndian.PutUint32(record[12:], uint32(len(payload)))
		if tag == "head" {
			headOffset = offset
		}
	}
	binary.BigEndian.PutUint32(out[headOffset+8:], 0xb1b0afba-fontChecksum(out))
	if len(out) >= len(data) {
		return data, ctx.Err()
	}
	return out, ctx.Err()
}

// fontChecksum 计算四字节对齐的SFNT校验和
// 入参: data 已补齐的数据
// 返回: uint32 校验和
func fontChecksum(data []byte) uint32 {
	var sum uint32
	for i := 0; i+4 <= len(data); i += 4 {
		sum += binary.BigEndian.Uint32(data[i:])
	}
	return sum
}

// optimizationFontUpdates 准备更小的字体程序及稳定子集标识，支持有界并行并限制累计输出缓存
// 入参: ctx 取消上下文, glyphs 字体用字, cached 原有缓存, workers 并行度
// 返回: map[Reference]*Stream 字体输出, map[Reference]string 子集标识, error 读取错误
func (r *Reader) optimizationFontUpdates(ctx context.Context, glyphs map[Reference][]uint16, cached map[Reference]bool, workers int) (map[Reference]*Stream, map[Reference]string, error) {
	updates := make(map[Reference]*Stream)
	tags := make(map[Reference]string)
	if len(glyphs) == 0 {
		return updates, tags, ctx.Err()
	}
	fontRefs := slices.SortedFunc(maps.Keys(glyphs), func(a, b Reference) int {
		if a.Number < b.Number {
			return -1
		}
		if a.Number > b.Number {
			return 1
		}
		return 0
	})
	type fontResult struct {
		orig *Stream
		out  *Stream
		err  error
	}
	results := make([]fontResult, len(fontRefs))
	if workers <= 1 || len(fontRefs) == 1 {
		for idx, ref := range fontRefs {
			if err := ctx.Err(); err != nil {
				return nil, nil, err
			}
			value, err := r.Object(ref)
			if !cached[ref] {
				r.cache.delete(ref)
			}
			if err != nil {
				return nil, nil, err
			}
			stream, ok := value.(*Stream)
			if !ok {
				continue
			}
			out, err := r.optimizeFontStream(ctx, stream, glyphs[ref])
			if err != nil {
				return nil, nil, err
			}
			results[idx] = fontResult{orig: stream, out: out}
		}
	} else {
		workCtx, cancel := context.WithCancel(ctx)
		defer cancel()
		var firstErrOnce sync.Once
		var firstErr error
		jobs := make(chan int, len(fontRefs))
		for idx := range fontRefs {
			jobs <- idx
		}
		close(jobs)
		count := min(workers, len(fontRefs))
		sharedWarning := syncWarning(r.warning)
		wasShared := r.cache != nil && r.cache.shared.Swap(true)
		var wg sync.WaitGroup
		wg.Add(count)
		for range count {
			wr := r.workerReader(sharedWarning)
			go func() {
				defer wg.Done()
				for idx := range jobs {
					if err := workCtx.Err(); err != nil {
						results[idx].err = err
						continue
					}
					ref := fontRefs[idx]
					value, err := wr.Object(ref)
					if err != nil {
						results[idx].err = err
						if ctx.Err() == nil && !errors.Is(err, context.Canceled) {
							firstErrOnce.Do(func() { firstErr = err })
						}
						cancel()
						continue
					}
					stream, ok := value.(*Stream)
					if !ok {
						continue
					}
					out, err := wr.optimizeFontStream(workCtx, stream, glyphs[ref])
					if err != nil {
						results[idx].err = err
						if ctx.Err() == nil && !errors.Is(err, context.Canceled) {
							firstErrOnce.Do(func() { firstErr = err })
						}
						cancel()
						continue
					}
					results[idx] = fontResult{orig: stream, out: out}
				}
			}()
		}
		wg.Wait()
		if r.cache != nil && !wasShared {
			r.cache.shared.Store(false)
		}
		for _, res := range results {
			if res.err != nil {
				if errors.Is(res.err, context.Canceled) && ctx.Err() == nil && firstErr != nil {
					return nil, nil, firstErr
				}
				return nil, nil, res.err
			}
		}
	}
	total := 0
	for idx, ref := range fontRefs {
		res := results[idx]
		if res.out == nil || res.out == res.orig || len(res.out.Data) > optimizationBufferLimit-total {
			continue
		}
		total += len(res.out.Data)
		updates[ref] = res.out
		hash := sha256.Sum256(res.out.Data)
		var tag [6]byte
		for i := range tag {
			tag[i] = 'A' + hash[i]%26
		}
		tags[ref] = string(tag[:]) + "+"
	}
	return updates, tags, ctx.Err()
}

// optimizedFontNames 按写时复制同步字体和描述符中的子集名称，不改资源键、字符映射及文本内容
// 入参: value 输出对象, tags 子集标识, aliases 合并引用, depth 嵌套深度
// 返回: Object 输出对象
func (r *Reader) optimizedFontNames(value Object, tags map[Reference]string, aliases map[Reference]Reference, depth int) Object {
	if len(tags) == 0 || depth > 256 {
		return value
	}
	result, _ := r.optimizedFontNamesCOW(value, tags, aliases, depth)
	return result
}

// optimizedFontNamesCOW 递归改写子集字体名称，仅当当前节点或子树实际变更时才克隆容器
// 入参: value 输出对象, tags 子集标识, aliases 合并引用, depth 嵌套深度
// 返回: Object 输出对象, bool 是否发生变更
func (r *Reader) optimizedFontNamesCOW(value Object, tags map[Reference]string, aliases map[Reference]Reference, depth int) (Object, bool) {
	if depth > 256 {
		return value, false
	}
	switch v := value.(type) {
	case Array:
		var out Array
		for i, item := range v {
			updated, changed := r.optimizedFontNamesCOW(item, tags, aliases, depth+1)
			if changed {
				if out == nil {
					out = slices.Clone(v)
				}
				out[i] = updated
			}
		}
		if out != nil {
			return out, true
		}
		return value, false
	case *Stream:
		updated, changed := r.optimizedFontNamesCOW(v.Dictionary, tags, aliases, depth+1)
		if !changed {
			return value, false
		}
		out := *v
		out.Dictionary = updated.(Dictionary)
		return &out, true
	case Dictionary:
		var out Dictionary
		for key, item := range v {
			updated, changed := r.optimizedFontNamesCOW(item, tags, aliases, depth+1)
			if changed {
				if out == nil {
					out = maps.Clone(v)
				}
				out[key] = updated
			}
		}
		if v["BaseFont"] != nil || v["FontName"] != nil {
			font := v
			if subtype, err := r.Resolve(v["Subtype"]); err == nil && subtype == Name("Type0") {
				resolved, err := r.Resolve(v["DescendantFonts"])
				if a, ok := resolved.(Array); err == nil && ok && len(a) == 1 {
					resolved, err = r.Resolve(a[0])
					if d, ok := resolved.(Dictionary); err == nil && ok {
						font = d
					}
				}
			}
			descriptor := font
			if font["FontDescriptor"] != nil {
				if resolved, err := r.Resolve(font["FontDescriptor"]); err == nil {
					if d, ok := resolved.(Dictionary); ok {
						descriptor = d
					}
				}
			}
			ref, _ := descriptor["FontFile2"].(Reference)
			if canonical, ok := aliases[ref]; ok {
				ref = canonical
			}
			if tag := tags[ref]; tag != "" {
				for _, key := range []Name{"BaseFont", "FontName"} {
					if v[key] == nil {
						continue
					}
					resolvedName, err := r.Resolve(v[key])
					if err != nil {
						continue
					}
					if name, ok := resolvedName.(Name); ok {
						text := string(name)
						if len(text) > 7 && text[6] == '+' && strings.Trim(text[:6], "ABCDEFGHIJKLMNOPQRSTUVWXYZ") == "" {
							text = text[7:]
						}
						renamed := Name(tag + text)
						if renamed != name {
							if out == nil {
								out = maps.Clone(v)
							}
							out[key] = renamed
						}
					}
				}
			}
		}
		if out != nil {
			return out, true
		}
		return value, false
	}
	return value, false
}

// resolveOptimizationObject 解析间接引用链并检测循环引用，按需复用元数据且不污染原有对象缓存
// 入参: value PDF对象, metadata 对象属性映射(可为nil), cached 原有缓存集合
// 返回: Object 解析后的对象, Reference 最后解析的间接引用, error 循环引用或读取错误
func (r *Reader) resolveOptimizationObject(value Object, metadata map[Reference]Object, cached map[Reference]bool) (Object, Reference, error) {
	seen := make(map[Reference]bool)
	var lastRef Reference
	for {
		ref, ok := value.(Reference)
		if !ok {
			return value, lastRef, nil
		}
		if seen[ref] {
			return nil, Reference{}, fmt.Errorf("cyclic indirect reference")
		}
		seen[ref] = true
		lastRef = ref
		if metadata != nil {
			if entry, ok := metadata[ref]; ok {
				value = entry
				continue
			}
		}
		var err error
		value, err = r.Object(ref)
		if !cached[ref] {
			r.cache.delete(ref)
		}
		if err != nil {
			return nil, Reference{}, err
		}
	}
}

// classifyType0Font 判断Type0字体及其后代是否为Identity收集目标或Stream映射字体
// 入参: type0 Type0字体字典, desc 后代CIDFont字典, metadata 对象属性, cached 原有缓存
// 返回: isIdentity 是否按Identity收集, isStream 是否为CIDToGIDMap流, ok 结构及引用是否有效
func (r *Reader) classifyType0Font(type0, desc Dictionary, metadata map[Reference]Object, cached map[Reference]bool) (isIdentity, isStream, ok bool) {
	subtype, _, err := r.resolveOptimizationObject(desc["Subtype"], metadata, cached)
	if err != nil || subtype != Name("CIDFontType2") {
		return false, false, false
	}
	encoding, _, err := r.resolveOptimizationObject(type0["Encoding"], metadata, cached)
	if err != nil {
		return false, false, false
	}
	mapping, _, err := r.resolveOptimizationObject(desc["CIDToGIDMap"], nil, cached)
	if err != nil {
		return false, false, false
	}
	if _, ok := mapping.(*Stream); ok {
		return false, true, true
	}
	if (mapping == nil || mapping == Name("Identity")) && (encoding == Name("Identity-H") || encoding == Name("Identity-V")) {
		return true, false, true
	}
	return false, false, false
}

// optimizationFontGlyphs 收集CID字体可寻址及Identity内容流用字，合并共享程序，异常时保守跳过
// 入参: ctx 取消上下文, metadata 对象属性, cached 原有缓存
// 返回: map[Reference][]uint16 字体程序用字, error 读取或取消错误
func (r *Reader) optimizationFontGlyphs(ctx context.Context, metadata map[Reference]Object, cached map[Reference]bool) (map[Reference][]uint16, error) {
	used := make(map[Reference]map[uint16]bool)
	unsafe := make(map[Reference]bool)
	other := make(map[Reference]bool)
	identityFonts := make(map[Reference]bool)
	claimedCID := make(map[Reference]bool)
	needAppearances := false
	resolve := func(value Object, stream bool) (Object, error) {
		var meta map[Reference]Object
		if !stream {
			meta = metadata
		}
		obj, _, err := r.resolveOptimizationObject(value, meta, cached)
		return obj, err
	}
	fontFileRef := func(dict Dictionary) (Reference, bool) {
		descriptor, err := resolve(dict["FontDescriptor"], false)
		if err != nil {
			return Reference{}, false
		}
		descDict, ok := descriptor.(Dictionary)
		if !ok {
			return Reference{}, false
		}
		ref, ok := descDict["FontFile2"].(Reference)
		return ref, ok
	}
	var scanType0 func(Object, int)
	scanType0 = func(value Object, depth int) {
		if depth > 256 {
			return
		}
		switch v := value.(type) {
		case Array:
			for _, item := range v {
				scanType0(item, depth+1)
			}
		case *Stream:
			scanType0(v.Dictionary, depth+1)
		case Dictionary:
			if v["NeedAppearances"] != nil {
				if na, err := resolve(v["NeedAppearances"], false); err != nil || na == Boolean(true) {
					needAppearances = true
				}
			}
			if v["DR"] != nil {
				if drVal, err := resolve(v["DR"], false); err == nil {
					if drDict, ok := drVal.(Dictionary); ok {
						_ = r.markResourceFontsUnsafe(drDict, metadata, cached, unsafe)
					}
				}
			}
			if subtype, err := resolve(v["Subtype"], false); err == nil && subtype == Name("Type0") {
				if arrVal, err := resolve(v["DescendantFonts"], false); err == nil {
					if arr, ok := arrVal.(Array); ok && len(arr) == 1 {
						descRef, isRef := arr[0].(Reference)
						if descVal, err := resolve(arr[0], false); err == nil {
							if descDict, ok := descVal.(Dictionary); ok {
								if fontRef, ok := fontFileRef(descDict); ok {
									if used[fontRef] == nil {
										used[fontRef] = map[uint16]bool{0: true}
									}
									isIdentity, isStream, valid := r.classifyType0Font(v, descDict, metadata, cached)
									if !valid {
										unsafe[fontRef] = true
									} else if isIdentity {
										identityFonts[fontRef] = true
										if isRef {
											claimedCID[descRef] = true
										}
									} else if !isStream {
										unsafe[fontRef] = true
									}
								}
							}
						}
					}
				}
			}
			for _, item := range v {
				scanType0(item, depth+1)
			}
		}
	}
	for _, value := range metadata {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		scanType0(value, 0)
	}
	var walk func(Object, int, bool)
	walk = func(value Object, depth int, skipClaimed bool) {
		if depth > 256 {
			return
		}
		switch v := value.(type) {
		case Reference:
			other[v] = true
		case Array:
			for _, item := range v {
				walk(item, depth+1, false)
			}
		case *Stream:
			walk(v.Dictionary, depth+1, false)
		case Dictionary:
			if !skipClaimed {
				if ref, ok := fontFileRef(v); ok {
					if used[ref] == nil {
						used[ref] = map[uint16]bool{0: true}
					}
					subtype, subErr := resolve(v["Subtype"], false)
					mapping, err := resolve(v["CIDToGIDMap"], true)
					stream, ok := mapping.(*Stream)
					if subErr != nil || subtype != Name("CIDFontType2") || err != nil || !ok {
						unsafe[ref] = true
					} else {
						data, err := r.optimizationFontData(ctx, stream, 131072)
						if err != nil || len(data)%2 != 0 {
							unsafe[ref] = true
						} else {
							for i := 0; i < len(data); i += 2 {
								used[ref][binary.BigEndian.Uint16(data[i:])] = true
							}
						}
					}
				}
			}
			subVal, subErr := resolve(v["Subtype"], false)
			inlineType0 := subErr == nil && subVal == Name("Type0")
			for key, item := range v {
				if key == "FontFile2" {
					continue
				}
				if inlineType0 && key == "DescendantFonts" {
					if arr, ok := item.(Array); ok && len(arr) == 1 {
						if _, isRef := arr[0].(Reference); !isRef {
							if descDict, ok := arr[0].(Dictionary); ok {
								if fontRef, ok := fontFileRef(descDict); ok && identityFonts[fontRef] && !unsafe[fontRef] {
									if isIdentity, _, valid := r.classifyType0Font(v, descDict, metadata, cached); valid && isIdentity {
										walk(descDict, depth+1, true)
										continue
									}
								}
							}
						}
					}
				}
				walk(item, depth+1, false)
			}
		}
	}
	for ref, value := range metadata {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		walk(value, 0, claimedCID[ref])
	}
	if needAppearances {
		for ref := range identityFonts {
			unsafe[ref] = true
		}
	}
	targets := make(map[Reference]bool)
	hasCandidate := false
	for ref := range used {
		if !unsafe[ref] && !other[ref] {
			hasCandidate = true
			if identityFonts[ref] {
				targets[ref] = true
			}
		}
	}
	if len(targets) > 0 {
		if err := r.collectIdentityFontGlyphs(ctx, metadata, cached, targets, used, unsafe); err != nil {
			return nil, err
		}
	} else if hasCandidate {
		if err := r.protectAnnotationPageFonts(ctx, metadata, cached, used, unsafe); err != nil {
			return nil, err
		}
	}
	result := make(map[Reference][]uint16)
	for ref, glyphs := range used {
		if !unsafe[ref] && !other[ref] {
			result[ref] = slices.Sorted(maps.Keys(glyphs))
		}
	}
	return result, ctx.Err()
}

// markFontDescriptorUnsafe 将字体字典描述符中的FontFile2流引用标为不可裁剪
// 入参: dict 字体字典, metadata 对象属性, cached 原有缓存, unsafe 不安全集合
// 返回: error 描述符解析或引用结构异常错误
func (r *Reader) markFontDescriptorUnsafe(dict Dictionary, metadata map[Reference]Object, cached map[Reference]bool, unsafe map[Reference]bool) error {
	if dict["FontDescriptor"] == nil {
		return nil
	}
	fdVal, _, err := r.resolveOptimizationObject(dict["FontDescriptor"], metadata, cached)
	if err != nil {
		return err
	}
	if fdVal == nil {
		return nil
	}
	fdDict, ok := fdVal.(Dictionary)
	if !ok {
		return fmt.Errorf("invalid font descriptor")
	}
	if fdDict["FontFile2"] != nil {
		ref, ok := fdDict["FontFile2"].(Reference)
		if !ok {
			return fmt.Errorf("invalid font program reference")
		}
		unsafe[ref] = true
	}
	return nil
}

// markFontObjectUnsafe 标记字体对象及其Type0后代中的全部FontFile2为不可裁剪
// 入参: value 字体对象, metadata 对象属性, cached 原有缓存, unsafe 不安全集合
// 返回: error 字体或后代结构解析异常错误
func (r *Reader) markFontObjectUnsafe(value Object, metadata map[Reference]Object, cached map[Reference]bool, unsafe map[Reference]bool) error {
	resolved, _, err := r.resolveOptimizationObject(value, metadata, cached)
	if err != nil {
		return err
	}
	if resolved == nil {
		return nil
	}
	dict, ok := resolved.(Dictionary)
	if !ok {
		return fmt.Errorf("invalid font resource")
	}
	firstErr := r.markFontDescriptorUnsafe(dict, metadata, cached, unsafe)
	record := func(err error) {
		if firstErr == nil {
			firstErr = err
		}
	}
	subtype, _, err := r.resolveOptimizationObject(dict["Subtype"], metadata, cached)
	if err != nil {
		record(err)
		return firstErr
	}
	if subtype != Name("Type0") {
		return firstErr
	}
	arrVal, _, err := r.resolveOptimizationObject(dict["DescendantFonts"], metadata, cached)
	if err != nil {
		record(err)
		return firstErr
	}
	arr, ok := arrVal.(Array)
	if !ok {
		record(fmt.Errorf("invalid descendant fonts"))
		return firstErr
	}
	for _, item := range arr {
		descVal, _, err := r.resolveOptimizationObject(item, metadata, cached)
		if err != nil {
			record(err)
			continue
		}
		descDict, ok := descVal.(Dictionary)
		if !ok {
			record(fmt.Errorf("invalid descendant font"))
			continue
		}
		record(r.markFontDescriptorUnsafe(descDict, metadata, cached, unsafe))
	}
	return firstErr
}

// markResourceFontsUnsafe 将资源字典Font与ExtGState引用的全部FontFile2标为不可裁剪
// 入参: res 资源字典, metadata 对象属性, cached 原有缓存, unsafe 不安全集合
// 返回: error 资源字典或字体引用异常错误
func (r *Reader) markResourceFontsUnsafe(res Dictionary, metadata map[Reference]Object, cached map[Reference]bool, unsafe map[Reference]bool) error {
	if res == nil {
		return nil
	}
	var firstErr error
	record := func(err error) {
		if firstErr == nil {
			firstErr = err
		}
	}
	if res["Font"] != nil {
		fontsVal, _, err := r.resolveOptimizationObject(res["Font"], metadata, cached)
		if err != nil {
			record(err)
		} else if fontsVal != nil {
			if fontsDict, ok := fontsVal.(Dictionary); !ok {
				record(fmt.Errorf("invalid font resources"))
			} else {
				for _, fontObj := range fontsDict {
					record(r.markFontObjectUnsafe(fontObj, metadata, cached, unsafe))
				}
			}
		}
	}
	if res["ExtGState"] == nil {
		return firstErr
	}
	gsVal, _, err := r.resolveOptimizationObject(res["ExtGState"], metadata, cached)
	if err != nil {
		record(err)
		return firstErr
	}
	if gsVal == nil {
		return firstErr
	}
	gsDict, ok := gsVal.(Dictionary)
	if !ok {
		record(fmt.Errorf("invalid graphics state resources"))
		return firstErr
	}
	for _, gsObj := range gsDict {
		stateVal, _, err := r.resolveOptimizationObject(gsObj, metadata, cached)
		if err != nil {
			record(err)
			continue
		}
		if stateVal == nil {
			continue
		}
		stateDict, ok := stateVal.(Dictionary)
		if !ok {
			record(fmt.Errorf("invalid graphics state"))
			continue
		}
		if stateDict["Font"] == nil {
			continue
		}
		fontArrVal, _, err := r.resolveOptimizationObject(stateDict["Font"], metadata, cached)
		if err != nil {
			record(err)
			continue
		}
		if fontArrVal == nil {
			continue
		}
		fontArr, ok := fontArrVal.(Array)
		if !ok {
			record(fmt.Errorf("invalid graphics state font"))
			continue
		}
		if len(fontArr) > 0 {
			record(r.markFontObjectUnsafe(fontArr[0], metadata, cached, unsafe))
		}
	}
	return firstErr
}

// annotHasText 判断注释文本字段是否非空
// 入参: value 注释文本对象, metadata 对象属性, cached 原有缓存
// 返回: bool 是否含非空文本, error 解析错误
func (r *Reader) annotHasText(value Object, metadata map[Reference]Object, cached map[Reference]bool) (bool, error) {
	if value == nil {
		return false, nil
	}
	resolved, _, err := r.resolveOptimizationObject(value, metadata, cached)
	if err != nil {
		return false, err
	}
	if resolved == nil {
		return false, nil
	}
	if s, ok := resolved.(String); ok && len(s) == 0 {
		return false, nil
	}
	return true, nil
}

// annotNeedsPageFonts 判断无正常外观的注释在合成外观时是否会使用页面资源字体
// 入参: annotDict 注释字典, metadata 对象属性, cached 原有缓存
// 返回: bool 是否依赖页面资源字体, error 解析错误
func (r *Reader) annotNeedsPageFonts(annotDict Dictionary, metadata map[Reference]Object, cached map[Reference]bool) (bool, error) {
	subtype, _, err := r.resolveOptimizationObject(annotDict["Subtype"], metadata, cached)
	if err != nil {
		return false, err
	}
	switch subtype {
	case Name("FreeText"):
	case Name("Line"):
		capVal, _, err := r.resolveOptimizationObject(annotDict["Cap"], metadata, cached)
		if err != nil {
			return false, err
		}
		if capVal != Boolean(true) {
			return false, nil
		}
	default:
		return false, nil
	}
	if has, err := r.annotHasText(annotDict["Contents"], metadata, cached); err != nil || has {
		return has, err
	}
	return r.annotHasText(annotDict["RC"], metadata, cached)
}

// annotHasNormalAP 判断注释是否携带有效的正常外观流
// 入参: annotDict 注释字典, metadata 对象属性, cached 原有缓存
// 返回: bool 是否含有效正常外观流, error 解析错误
func (r *Reader) annotHasNormalAP(annotDict Dictionary, metadata map[Reference]Object, cached map[Reference]bool) (bool, error) {
	if annotDict["AP"] == nil {
		return false, nil
	}
	apVal, _, err := r.resolveOptimizationObject(annotDict["AP"], metadata, cached)
	if err != nil {
		return false, err
	}
	apDict, ok := apVal.(Dictionary)
	if !ok || apDict["N"] == nil {
		return false, nil
	}
	entryVal, _, err := r.resolveOptimizationObject(apDict["N"], nil, cached)
	if err != nil {
		return false, err
	}
	switch v := entryVal.(type) {
	case *Stream:
		return true, nil
	case Dictionary:
		for _, stateEntry := range v {
			stateObj, _, err := r.resolveOptimizationObject(stateEntry, nil, cached)
			if err != nil {
				return false, err
			}
			if _, ok := stateObj.(*Stream); ok {
				return true, nil
			}
		}
	}
	return false, nil
}

// protectAnnotationPageFonts 在无Identity扫描目标时遍历页面注释并保护无正常外观注释引用的页面资源字体
// 入参: ctx 取消上下文, metadata 对象属性, cached 原有缓存, used 候选字体集合, unsafe 不安全集合
// 返回: error 仅返回取消错误，结构异常时保守保护全部候选字体
func (r *Reader) protectAnnotationPageFonts(ctx context.Context, metadata map[Reference]Object, cached map[Reference]bool, used map[Reference]map[uint16]bool, unsafe map[Reference]bool) error {
	cleanup := func() {
		r.cache.prune(cached)
	}
	defer cleanup()
	err := r.WalkPages(ctx, func(_ int, page *Page) error {
		defer cleanup()
		if page.Dictionary["Annots"] == nil {
			return nil
		}
		annotsVal, _, err := r.resolveOptimizationObject(page.Dictionary["Annots"], metadata, cached)
		if err != nil {
			return err
		}
		if annotsVal == nil {
			return nil
		}
		annots, ok := annotsVal.(Array)
		if !ok {
			return fmt.Errorf("invalid page annotations")
		}
		for _, item := range annots {
			annotVal, _, err := r.resolveOptimizationObject(item, metadata, cached)
			if err != nil {
				return err
			}
			annotDict, ok := annotVal.(Dictionary)
			if !ok {
				continue
			}
			needs, err := r.annotNeedsPageFonts(annotDict, metadata, cached)
			if err != nil {
				return err
			}
			if !needs {
				continue
			}
			hasNormalAP, err := r.annotHasNormalAP(annotDict, metadata, cached)
			if err != nil {
				return err
			}
			if !hasNormalAP {
				return r.markResourceFontsUnsafe(page.Resources, metadata, cached, unsafe)
			}
		}
		return nil
	})
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		for ref := range used {
			unsafe[ref] = true
		}
	}
	return ctx.Err()
}

// collectIdentityFontGlyphs 扫描页面、表单、注释外观、图案与Type3内容流收集Identity字体的2字节CID字形
// 入参: ctx 取消上下文, metadata 对象属性, cached 原有缓存, targets 目标字体流集合, used 收集结果, unsafe 不安全集合
// 返回: error 仅返回取消错误，页面阶段异常保护全部候选字体，后续阶段异常保护目标字体
func (r *Reader) collectIdentityFontGlyphs(ctx context.Context, metadata map[Reference]Object, cached map[Reference]bool, targets map[Reference]bool, used map[Reference]map[uint16]bool, unsafe map[Reference]bool) error {
	cleanup := func() {
		r.cache.prune(cached)
	}
	defer cleanup()
	resolveMeta := func(value Object) (Object, error) {
		obj, _, err := r.resolveOptimizationObject(value, metadata, cached)
		return obj, err
	}
	resolveFontTarget := func(fontObj Object) (Reference, bool, bool) {
		val, err := resolveMeta(fontObj)
		if err != nil {
			return Reference{}, false, false
		}
		dict, ok := val.(Dictionary)
		if !ok {
			return Reference{}, false, false
		}
		subtype, err := resolveMeta(dict["Subtype"])
		if err != nil {
			return Reference{}, false, false
		}
		if subtype != Name("Type0") {
			return Reference{}, false, true
		}
		arrVal, err := resolveMeta(dict["DescendantFonts"])
		if err != nil {
			return Reference{}, false, false
		}
		arr, ok := arrVal.(Array)
		if !ok || len(arr) != 1 {
			return Reference{}, false, true
		}
		descVal, err := resolveMeta(arr[0])
		if err != nil {
			return Reference{}, false, false
		}
		descDict, ok := descVal.(Dictionary)
		if !ok {
			return Reference{}, false, true
		}
		fdVal, err := resolveMeta(descDict["FontDescriptor"])
		if err != nil {
			return Reference{}, false, false
		}
		fdDict, ok := fdVal.(Dictionary)
		if !ok {
			return Reference{}, false, true
		}
		ref, ok := fdDict["FontFile2"].(Reference)
		if !ok || !targets[ref] {
			return Reference{}, false, true
		}
		isIdentity, isStream, valid := r.classifyType0Font(dict, descDict, metadata, cached)
		if !valid {
			return Reference{}, false, false
		}
		if isIdentity {
			return ref, true, true
		}
		if isStream {
			return Reference{}, false, true
		}
		return Reference{}, false, false
	}
	type fontState struct {
		ref     Reference
		unknown bool
	}
	type formVisitKey struct {
		form  Reference
		state fontState
	}
	type charProcVisitKey struct {
		proc Reference
		res  Reference
	}
	visitedForms := make(map[Reference]bool)
	visitedOwnForms := make(map[formVisitKey]bool)
	visitedCharProcs := make(map[charProcVisitKey]bool)
	visitedOwnType3 := make(map[Reference]bool)
	visitedOwnType3Ptr := make(map[uintptr]Dictionary)
	visitedType3Res := make(map[uintptr]Dictionary)
	visitedContextType3 := make(map[Reference]bool)
	visitedContextProcs := make(map[Reference]bool)
	activeProcs := make(map[Reference]bool)
	budget := 100000
	var scan func([]byte, Dictionary, int, fontState) error
	var scanResourcesType3 func(Dictionary, int) error
	var scanType3Dict func(Dictionary, Reference, Dictionary, int) error
	scanType3Dict = func(dict Dictionary, fontRef Reference, fallbackRes Dictionary, depth int) error {
		budget--
		if budget < 0 {
			return fmt.Errorf("type3 operation limit exceeded")
		}
		if depth > 32 {
			return fmt.Errorf("type3 recursion limit exceeded")
		}
		if fontRef.Number != 0 && visitedOwnType3[fontRef] {
			return nil
		}
		procsVal, err := resolveMeta(dict["CharProcs"])
		if err != nil {
			return err
		}
		procs, ok := procsVal.(Dictionary)
		if !ok {
			return nil
		}
		resDict := fallbackRes
		var resRef Reference
		resInline := false
		hasOwnRes := false
		if dict["Resources"] != nil {
			resVal, ref, err := r.resolveOptimizationObject(dict["Resources"], metadata, cached)
			if err != nil {
				return err
			}
			if resVal != nil {
				ownRes, ok := resVal.(Dictionary)
				if !ok {
					return fmt.Errorf("invalid type3 resources")
				}
				resDict = ownRes
				resRef = ref
				resInline = ref.Number == 0
				hasOwnRes = true
			}
		}
		if hasOwnRes {
			if fontRef.Number != 0 {
				visitedOwnType3[fontRef] = true
			} else if ptr := reflect.ValueOf(dict).Pointer(); ptr != 0 {
				if _, seen := visitedOwnType3Ptr[ptr]; seen {
					return nil
				}
				if len(visitedOwnType3Ptr) < 65536 {
					visitedOwnType3Ptr[ptr] = dict
				}
			}
		} else {
			if fallbackRes == nil && fontRef.Number != 0 && visitedContextType3[fontRef] {
				return nil
			}
			if fallbackRes != nil && fontRef.Number != 0 {
				visitedContextType3[fontRef] = true
			}
		}
		seenLocal := make(map[Reference]bool)
		for _, procObj := range procs {
			if err := ctx.Err(); err != nil {
				return err
			}
			budget--
			if budget < 0 {
				return fmt.Errorf("type3 operation limit exceeded")
			}
			objVal, procRef, err := r.resolveOptimizationObject(procObj, nil, cached)
			if err != nil {
				return err
			}
			if procRef.Number != 0 {
				if hasOwnRes {
					if resInline {
						if seenLocal[procRef] {
							continue
						}
						seenLocal[procRef] = true
					} else {
						key := charProcVisitKey{proc: procRef, res: resRef}
						if visitedCharProcs[key] {
							continue
						}
						visitedCharProcs[key] = true
					}
				} else {
					if fallbackRes == nil && fontRef.Number == 0 && visitedContextProcs[procRef] {
						continue
					}
					if activeProcs[procRef] {
						continue
					}
				}
			}
			stream, ok := objVal.(*Stream)
			if !ok {
				continue
			}
			content, err := r.optimizationContentData(ctx, stream, optimizationBufferLimit)
			if err != nil {
				return err
			}
			if !hasOwnRes && procRef.Number != 0 {
				activeProcs[procRef] = true
				if fallbackRes != nil {
					visitedContextProcs[procRef] = true
				}
			}
			scanErr := scan(content, resDict, depth+1, fontState{unknown: true})
			if !hasOwnRes && procRef.Number != 0 {
				delete(activeProcs, procRef)
			}
			if scanErr != nil {
				return scanErr
			}
		}
		if hasOwnRes {
			return scanResourcesType3(resDict, depth+1)
		}
		return nil
	}
	scanResourcesType3 = func(res Dictionary, depth int) error {
		if res == nil || res["Font"] == nil {
			return nil
		}
		budget--
		if budget < 0 {
			return fmt.Errorf("type3 operation limit exceeded")
		}
		if depth > 32 {
			return fmt.Errorf("type3 recursion limit exceeded")
		}
		if ptr := reflect.ValueOf(res).Pointer(); ptr != 0 {
			if _, seen := visitedType3Res[ptr]; seen {
				return nil
			}
			if len(visitedType3Res) < 65536 {
				visitedType3Res[ptr] = res
			}
		}
		fontsVal, err := resolveMeta(res["Font"])
		if err != nil {
			return err
		}
		fontsDict, ok := fontsVal.(Dictionary)
		if !ok {
			return nil
		}
		for _, fontObj := range fontsDict {
			if err := ctx.Err(); err != nil {
				return err
			}
			budget--
			if budget < 0 {
				return fmt.Errorf("type3 operation limit exceeded")
			}
			objVal, fontRef, err := r.resolveOptimizationObject(fontObj, metadata, cached)
			if err != nil {
				return err
			}
			fontDict, ok := objVal.(Dictionary)
			if !ok {
				continue
			}
			subtype, err := resolveMeta(fontDict["Subtype"])
			if err != nil {
				return err
			}
			if subtype == Name("Type3") && fontDict["CharProcs"] != nil {
				if err := scanType3Dict(fontDict, fontRef, res, depth); err != nil {
					return err
				}
			}
		}
		return nil
	}
	scan = func(data []byte, resources Dictionary, depth int, initial fontState) error {
		if depth > 32 {
			return fmt.Errorf("form recursion limit exceeded")
		}
		state := initial
		var stack []fontState
		recordBytes := func(raw String) error {
			if state.unknown {
				return fmt.Errorf("unknown font for text operation")
			}
			if state.ref.Number == 0 {
				return nil
			}
			if len(raw)%2 != 0 {
				unsafe[state.ref] = true
				return nil
			}
			glyphs := used[state.ref]
			for i := 0; i < len(raw); i += 2 {
				glyphs[uint16(raw[i])<<8|uint16(raw[i+1])] = true
			}
			return nil
		}
		return walkOperationsBorrowed(ctx, data, func(op Operation) error {
			switch op.Operator {
			case "q":
				if len(stack) >= 100000 {
					return fmt.Errorf("graphics state limit exceeded")
				}
				stack = append(stack, state)
			case "Q":
				if len(stack) > 0 {
					state = stack[len(stack)-1]
					stack = stack[:len(stack)-1]
				}
			case "Tf":
				if len(op.Operands) < 2 {
					state = fontState{unknown: true}
					return nil
				}
				name, ok := op.Operands[0].(Name)
				if !ok || resources == nil {
					state = fontState{unknown: true}
					return nil
				}
				fontsVal, err := resolveMeta(resources["Font"])
				if err != nil {
					return err
				}
				fontsDict, ok := fontsVal.(Dictionary)
				if !ok {
					state = fontState{unknown: true}
					return nil
				}
				fontObj, exists := fontsDict[name]
				if !exists {
					state = fontState{unknown: true}
					return nil
				}
				ref, isTarget, valid := resolveFontTarget(fontObj)
				if !valid {
					state = fontState{unknown: true}
					return nil
				}
				if isTarget {
					state = fontState{ref: ref}
				} else {
					state = fontState{}
				}
			case "gs":
				if len(op.Operands) != 1 || resources == nil {
					state = fontState{unknown: true}
					return nil
				}
				name, ok := op.Operands[0].(Name)
				if !ok {
					state = fontState{unknown: true}
					return nil
				}
				gsVal, err := resolveMeta(resources["ExtGState"])
				if err != nil {
					return err
				}
				gsDict, ok := gsVal.(Dictionary)
				if !ok {
					state = fontState{unknown: true}
					return nil
				}
				entry, exists := gsDict[name]
				if !exists {
					state = fontState{unknown: true}
					return nil
				}
				stateVal, err := resolveMeta(entry)
				if err != nil {
					return err
				}
				stateDict, ok := stateVal.(Dictionary)
				if !ok {
					state = fontState{unknown: true}
					return nil
				}
				if stateDict["Font"] == nil {
					return nil
				}
				fontArrVal, err := resolveMeta(stateDict["Font"])
				if err != nil {
					return err
				}
				fontArr, ok := fontArrVal.(Array)
				if !ok || len(fontArr) < 1 {
					state = fontState{unknown: true}
					return nil
				}
				ref, isTarget, valid := resolveFontTarget(fontArr[0])
				if !valid {
					state = fontState{unknown: true}
					return nil
				}
				if isTarget {
					state = fontState{ref: ref}
				} else {
					state = fontState{}
				}
			case "Tj", "'":
				if len(op.Operands) < 1 {
					return nil
				}
				if s, ok := op.Operands[len(op.Operands)-1].(String); ok {
					return recordBytes(s)
				}
			case "\"":
				if len(op.Operands) < 3 {
					return nil
				}
				if s, ok := op.Operands[len(op.Operands)-1].(String); ok {
					return recordBytes(s)
				}
			case "TJ":
				if len(op.Operands) < 1 {
					return nil
				}
				arr, ok := op.Operands[0].(Array)
				if !ok {
					return nil
				}
				for _, item := range arr {
					if s, ok := item.(String); ok {
						if err := recordBytes(s); err != nil {
							return err
						}
					}
				}
			case "Do":
				budget--
				if budget < 0 {
					return fmt.Errorf("form operation limit exceeded")
				}
				if resources == nil {
					return fmt.Errorf("missing resources for form operation")
				}
				if len(op.Operands) != 1 {
					return nil
				}
				name, ok := op.Operands[0].(Name)
				if !ok {
					return nil
				}
				xobjVal, err := resolveMeta(resources["XObject"])
				if err != nil {
					return err
				}
				xobjDict, ok := xobjVal.(Dictionary)
				if !ok {
					return nil
				}
				entry, exists := xobjDict[name]
				if !exists {
					return nil
				}
				objVal, formRef, err := r.resolveOptimizationObject(entry, nil, cached)
				if err != nil {
					return err
				}
				stream, ok := objVal.(*Stream)
				if !ok {
					return nil
				}
				subtype, err := resolveMeta(stream.Dictionary["Subtype"])
				if err != nil || subtype != Name("Form") {
					return err
				}
				childRes := resources
				hasOwnRes := false
				if stream.Dictionary["Resources"] != nil {
					resVal, err := resolveMeta(stream.Dictionary["Resources"])
					if err != nil {
						return err
					}
					if resVal != nil {
						resDict, ok := resVal.(Dictionary)
						if !ok {
							return fmt.Errorf("invalid form resources")
						}
						childRes = resDict
						hasOwnRes = true
					}
				}
				if hasOwnRes && formRef.Number != 0 {
					visitKey := formVisitKey{form: formRef, state: state}
					if visitedOwnForms[visitKey] {
						return nil
					}
					visitedOwnForms[visitKey] = true
				}
				if formRef.Number != 0 {
					visitedForms[formRef] = true
				}
				content, err := r.optimizationContentData(ctx, stream, optimizationBufferLimit)
				if err != nil {
					return err
				}
				if err := scan(content, childRes, depth+1, state); err != nil {
					return err
				}
				if hasOwnRes {
					return scanResourcesType3(childRes, depth+1)
				}
				return nil
			}
			return nil
		})
	}
	scanAPStream := func(pageRes Dictionary, stream *Stream, apRef Reference) error {
		if apRef.Number != 0 {
			visitedForms[apRef] = true
		}
		apRes := pageRes
		hasOwnRes := false
		if stream.Dictionary["Resources"] != nil {
			resVal, err := resolveMeta(stream.Dictionary["Resources"])
			if err != nil {
				return err
			}
			if resVal != nil {
				resDict, ok := resVal.(Dictionary)
				if !ok {
					return fmt.Errorf("invalid appearance resources")
				}
				apRes = resDict
				hasOwnRes = true
			}
		}
		if hasOwnRes && apRef.Number != 0 {
			visitKey := formVisitKey{form: apRef, state: fontState{unknown: true}}
			if visitedOwnForms[visitKey] {
				return nil
			}
			visitedOwnForms[visitKey] = true
		}
		content, err := r.optimizationContentData(ctx, stream, optimizationBufferLimit)
		if err != nil {
			return err
		}
		if err := scan(content, apRes, 0, fontState{unknown: true}); err != nil {
			return err
		}
		if hasOwnRes {
			return scanResourcesType3(apRes, 0)
		}
		return nil
	}
	var scanType3 func(Object, Reference, int) error
	scanType3 = func(value Object, objRef Reference, depth int) error {
		if depth > 256 {
			return fmt.Errorf("font metadata recursion limit exceeded")
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		switch v := value.(type) {
		case Array:
			for _, item := range v {
				if err := scanType3(item, Reference{}, depth+1); err != nil {
					return err
				}
			}
		case *Stream:
			return scanType3(v.Dictionary, Reference{}, depth+1)
		case Dictionary:
			subtype, err := resolveMeta(v["Subtype"])
			if err != nil {
				return err
			}
			if subtype == Name("Type3") && v["CharProcs"] != nil {
				if err := scanType3Dict(v, objRef, nil, 0); err != nil {
					return err
				}
			}
			for _, item := range v {
				if err := scanType3(item, Reference{}, depth+1); err != nil {
					return err
				}
			}
		}
		return nil
	}
	pagesScanned := false
	run := func() error {
		if err := r.WalkPages(ctx, func(_ int, page *Page) error {
			defer cleanup()
			var content bytes.Buffer
			if _, err := page.WriteContent(ctx, &optimizationBuffer{buffer: &content, limit: optimizationBufferLimit}); err != nil {
				return err
			}
			if err := scan(content.Bytes(), page.Resources, 0, fontState{unknown: true}); err != nil {
				return err
			}
			if err := scanResourcesType3(page.Resources, 0); err != nil {
				return err
			}
			if page.Dictionary["Annots"] != nil {
				annotsVal, err := resolveMeta(page.Dictionary["Annots"])
				if err != nil {
					return err
				}
				if annotsVal != nil {
					annots, ok := annotsVal.(Array)
					if !ok {
						return fmt.Errorf("invalid page annotations")
					}
					pageFontsUnsafe := false
					for _, item := range annots {
						annotVal, err := resolveMeta(item)
						if err != nil {
							return err
						}
						annotDict, ok := annotVal.(Dictionary)
						if !ok {
							continue
						}
						hasNormalAP := false
						if annotDict["AP"] != nil {
							apVal, err := resolveMeta(annotDict["AP"])
							if err != nil {
								return err
							}
							if apDict, ok := apVal.(Dictionary); ok {
								for _, apKey := range []Name{"N", "R", "D"} {
									if apDict[apKey] == nil {
										continue
									}
									entryVal, apRef, err := r.resolveOptimizationObject(apDict[apKey], nil, cached)
									if err != nil {
										return err
									}
									switch v := entryVal.(type) {
									case *Stream:
										if apKey == "N" {
											hasNormalAP = true
										}
										if err := scanAPStream(page.Resources, v, apRef); err != nil {
											return err
										}
									case Dictionary:
										for _, stateEntry := range v {
											stateObj, stateRef, err := r.resolveOptimizationObject(stateEntry, nil, cached)
											if err != nil {
												return err
											}
											if stateStream, ok := stateObj.(*Stream); ok {
												if apKey == "N" {
													hasNormalAP = true
												}
												if err := scanAPStream(page.Resources, stateStream, stateRef); err != nil {
													return err
												}
											}
										}
									}
								}
							}
						}
						if !hasNormalAP && !pageFontsUnsafe {
							needs, err := r.annotNeedsPageFonts(annotDict, metadata, cached)
							if err != nil {
								return err
							}
							if needs {
								if err := r.markResourceFontsUnsafe(page.Resources, metadata, cached, unsafe); err != nil {
									return err
								}
								pageFontsUnsafe = true
							}
						}
					}
				}
			}
			return nil
		}); err != nil {
			return err
		}
		pagesScanned = true
		for ref, value := range metadata {
			if err := ctx.Err(); err != nil {
				return err
			}
			if dict, ok := value.(Dictionary); ok {
				subtype, err := resolveMeta(dict["Subtype"])
				if err != nil {
					return err
				}
				patternType, err := resolveMeta(dict["PatternType"])
				if err != nil {
					return err
				}
				isForm := subtype == Name("Form")
				isPattern := patternType == Integer(1)
				if (isForm && !visitedForms[ref]) || isPattern {
					obj, err := r.Object(ref)
					if !cached[ref] {
						r.cache.delete(ref)
					}
					if err != nil {
						return err
					}
					if stream, ok := obj.(*Stream); ok {
						var resDict Dictionary
						if stream.Dictionary["Resources"] != nil {
							resVal, err := resolveMeta(stream.Dictionary["Resources"])
							if err != nil {
								return err
							}
							if resVal != nil {
								var ok bool
								resDict, ok = resVal.(Dictionary)
								if !ok {
									return fmt.Errorf("invalid form resources")
								}
							}
						}
						content, err := r.optimizationContentData(ctx, stream, optimizationBufferLimit)
						if err != nil {
							return err
						}
						if err := scan(content, resDict, 0, fontState{unknown: true}); err != nil {
							return err
						}
						if err := scanResourcesType3(resDict, 0); err != nil {
							return err
						}
					}
				}
			}
			if err := scanType3(value, ref, 0); err != nil {
				return err
			}
		}
		return ctx.Err()
	}
	if err := run(); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if !pagesScanned {
			for ref := range used {
				unsafe[ref] = true
			}
		} else {
			for ref := range targets {
				unsafe[ref] = true
			}
		}
	}
	return ctx.Err()
}

// optimizationContentData 有界解码内容流并检查取消，拒绝外部流或超限膨胀
// 入参: ctx 取消上下文, stream 内容流, limit 解码上限
// 返回: []byte 解码数据, error 解码、超限或取消错误
func (r *Reader) optimizationContentData(ctx context.Context, stream *Stream, limit int) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if maxBytes, _ := effectiveStreamLimits(r, stream); maxBytes > 0 && int64(limit) > maxBytes {
		limit = int(maxBytes)
	}
	if stream.Dictionary["F"] != nil || len(stream.Data) > limit {
		return nil, io.ErrShortBuffer
	}
	filters, params, err := stream.filterChain(r)
	if err != nil {
		return nil, err
	}
	data := stream.Data
	for i, filter := range filters {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		name := filter.(Name)
		dict, _ := params[i].(Dictionary)
		switch name {
		case "Crypt":
			if i != 0 || dict["Name"] != nil && dict["Name"] != Name("Identity") {
				return nil, &UnsupportedError{Feature: "stream crypt filter"}
			}
		case "FlateDecode":
			input, err := acquireZlibReader(bytes.NewReader(data))
			if err != nil {
				return nil, err
			}
			data, err = readAllEstimated(io.LimitReader(&contextInput{ctx: ctx, reader: input}, int64(limit)+1), estimateFlateCapacity(len(data), int64(limit)))
			closeErr := input.Close()
			releaseZlibReader(input)
			if len(data) > limit {
				return nil, io.ErrShortBuffer
			}
			if err == nil {
				err = closeErr
			}
			if err != nil {
				return nil, err
			}
			data, err = restorePredictorContext(ctx, data, dict)
			if err != nil {
				return nil, err
			}
		case "LZWDecode":
			early, err := integerDefault(dict, "EarlyChange", 1)
			if err != nil {
				return nil, err
			}
			data, _, err = decodeLZWBytesLimitContext(ctx, data, early, int64(limit))
			if err != nil {
				return nil, err
			}
			data, err = restorePredictorContext(ctx, data, dict)
			if err != nil {
				return nil, err
			}
		case "ASCIIHexDecode":
			_, size, err := asciiHexSize(ctx, data, nil)
			if err != nil {
				return nil, err
			}
			if size > limit {
				return nil, io.ErrShortBuffer
			}
			data, err = decodeASCIIHexContext(ctx, data, nil)
			if err != nil {
				return nil, err
			}
		case "ASCII85Decode":
			_, size, err := ascii85Size(ctx, data, nil)
			if err != nil {
				return nil, err
			}
			if size > limit {
				return nil, io.ErrShortBuffer
			}
			data, err = decodeASCII85Context(ctx, data, nil)
			if err != nil {
				return nil, err
			}
		case "RunLengthDecode":
			size, err := runLengthSize(ctx, data, nil)
			if err != nil {
				return nil, err
			}
			if size > limit {
				return nil, io.ErrShortBuffer
			}
			data, err = decodeRunLength(ctx, data, nil)
			if err != nil {
				return nil, err
			}
		default:
			return nil, &UnsupportedError{Feature: "stream filter " + string(name)}
		}
		if len(data) > limit {
			return nil, io.ErrShortBuffer
		}
	}
	return data, ctx.Err()
}

// optimizationFontData 有界解码字体相关流，不展开未知或多重过滤器
// 入参: ctx 取消上下文, stream 字体或映射流, limit 解码上限
// 返回: []byte 解码数据, error 解码或容量错误
func (r *Reader) optimizationFontData(ctx context.Context, stream *Stream, limit int) ([]byte, error) {
	if maxBytes, _ := effectiveStreamLimits(r, stream); maxBytes > 0 && int64(limit) > maxBytes {
		limit = int(maxBytes)
	}
	filters, params, err := stream.filterChain(r)
	if err != nil {
		return nil, err
	}
	if stream.Dictionary["F"] != nil || len(filters) > 1 || len(filters) == 1 && filters[0] != Name("FlateDecode") {
		return nil, io.ErrUnexpectedEOF
	}
	data := stream.Data
	if len(filters) == 1 {
		input, err := acquireZlibReader(bytes.NewReader(data))
		if err != nil {
			return nil, err
		}
		data, err = readAllEstimated(io.LimitReader(&contextInput{ctx: ctx, reader: input}, int64(limit)+1), estimateFlateCapacity(len(data), int64(limit)))
		input.Close()
		releaseZlibReader(input)
		if err != nil {
			return nil, err
		}
		if len(data) > limit {
			return nil, io.ErrShortBuffer
		}
		parameters, _ := params[0].(Dictionary)
		data, err = restorePredictorContext(ctx, data, parameters)
		if err != nil {
			return nil, err
		}
	}
	if len(data) > limit {
		return nil, io.ErrShortBuffer
	}
	return data, ctx.Err()
}

// optimizeFontStream 裁剪可完整确认映射的字体，保留原流加密策略及其他属性
// 入参: ctx 取消上下文, stream 字体流, glyphs 可寻址字形
// 返回: *Stream 更小的字体流, error 取消错误
func (r *Reader) optimizeFontStream(ctx context.Context, stream *Stream, glyphs []uint16) (*Stream, error) {
	data, err := r.optimizationFontData(ctx, stream, optimizationBufferLimit)
	if err != nil {
		return stream, ctx.Err()
	}
	compact, err := compactTrueType(ctx, data, glyphs)
	if err != nil {
		return nil, err
	}
	if len(compact) >= len(data) {
		return stream, nil
	}
	encoded, err := compressPDFBytes(ctx, compact)
	if err != nil {
		return nil, err
	}
	if len(encoded)+40 >= len(stream.Data) {
		return stream, nil
	}
	result := *stream
	result.Dictionary = maps.Clone(stream.Dictionary)
	result.Data = encoded
	result.compressed = true
	result.Dictionary["Length1"] = Integer(len(compact))
	result.Dictionary["Filter"] = Name("FlateDecode")
	delete(result.Dictionary, "DecodeParms")
	if err := r.applyExplicitCryptFilter(stream, &result); err != nil {
		return stream, nil
	}
	return &result, nil
}
