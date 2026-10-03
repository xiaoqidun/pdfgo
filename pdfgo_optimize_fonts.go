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
	"compress/zlib"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"io"
	"maps"
	"slices"
	"strings"
)

// optimizationFontUpdates 准备更小的字体程序及稳定子集标识，限制累计输出缓存
// 入参: ctx 取消上下文, glyphs 字体用字, cached 原有缓存
// 返回: map[Reference]*Stream 字体输出, map[Reference]string 子集标识, error 读取错误
func (r *Reader) optimizationFontUpdates(ctx context.Context, glyphs map[Reference][]uint16, cached map[Reference]bool) (map[Reference]*Stream, map[Reference]string, error) {
	updates := make(map[Reference]*Stream)
	tags := make(map[Reference]string)
	total := 0
	for _, ref := range slices.SortedFunc(maps.Keys(glyphs), func(a, b Reference) int {
		if a.Number < b.Number {
			return -1
		}
		if a.Number > b.Number {
			return 1
		}
		return 0
	}) {
		if err := ctx.Err(); err != nil {
			return nil, nil, err
		}
		value, err := r.Object(ref)
		if !cached[ref] {
			delete(r.cache, ref)
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
		if out == stream || len(out.Data) > optimizationBufferLimit-total {
			continue
		}
		total += len(out.Data)
		updates[ref] = out
		hash := sha256.Sum256(out.Data)
		var tag [6]byte
		for i := range tag {
			tag[i] = 'A' + hash[i]%26
		}
		tags[ref] = string(tag[:]) + "+"
	}
	return updates, tags, ctx.Err()
}

// optimizedFontNames 同步字体和描述符中的子集名称，不改资源键、字符映射及文本内容
// 入参: value 输出对象, tags 子集标识, aliases 合并引用, depth 嵌套深度
// 返回: Object 输出对象
func (r *Reader) optimizedFontNames(value Object, tags map[Reference]string, aliases map[Reference]Reference, depth int) Object {
	if len(tags) == 0 || depth > 256 {
		return value
	}
	switch v := value.(type) {
	case Array:
		out := slices.Clone(v)
		for i, item := range v {
			out[i] = r.optimizedFontNames(item, tags, aliases, depth+1)
		}
		return out
	case *Stream:
		out := *v
		out.Dictionary = r.optimizedFontNames(v.Dictionary, tags, aliases, depth+1).(Dictionary)
		return &out
	case Dictionary:
		out := maps.Clone(v)
		for key, item := range v {
			out[key] = r.optimizedFontNames(item, tags, aliases, depth+1)
		}
		font := v
		if v["Subtype"] == Name("Type0") {
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
				if name, ok := v[key].(Name); ok {
					text := string(name)
					if len(text) > 7 && text[6] == '+' && strings.Trim(text[:6], "ABCDEFGHIJKLMNOPQRSTUVWXYZ") == "" {
						text = text[7:]
					}
					out[key] = Name(tag + text)
				}
			}
		}
		return out
	}
	return value
}

// optimizationFontGlyphs 收集CID字体所有可寻址字形，合并共享程序，不依赖页面文字是否可见
// 入参: ctx 取消上下文, metadata 对象属性, cached 原有缓存
// 返回: map[Reference][]uint16 字体程序用字, error 读取或取消错误
func (r *Reader) optimizationFontGlyphs(ctx context.Context, metadata map[Reference]Object, cached map[Reference]bool) (map[Reference][]uint16, error) {
	used := make(map[Reference]map[uint16]bool)
	unsafe := make(map[Reference]bool)
	other := make(map[Reference]bool)
	resolve := func(value Object, stream bool) (Object, error) {
		seen := make(map[Reference]bool)
		for {
			ref, ok := value.(Reference)
			if !ok {
				return value, nil
			}
			if seen[ref] {
				return nil, fmt.Errorf("cyclic indirect reference")
			}
			seen[ref] = true
			if !stream {
				if entry, ok := metadata[ref]; ok {
					value = entry
					continue
				}
			}
			var err error
			value, err = r.Object(ref)
			if !cached[ref] {
				delete(r.cache, ref)
			}
			if err != nil {
				return nil, err
			}
		}
	}
	var walk func(Object, int) error
	walk = func(value Object, depth int) error {
		if depth > 256 {
			return nil
		}
		switch v := value.(type) {
		case Reference:
			other[v] = true
		case Array:
			for _, item := range v {
				if err := walk(item, depth+1); err != nil {
					return err
				}
			}
		case *Stream:
			return walk(v.Dictionary, depth+1)
		case Dictionary:
			if descriptor, err := resolve(v["FontDescriptor"], false); err == nil {
				if dict, ok := descriptor.(Dictionary); ok {
					if ref, ok := dict["FontFile2"].(Reference); ok {
						if used[ref] == nil {
							used[ref] = map[uint16]bool{0: true}
						}
						mapping, err := resolve(v["CIDToGIDMap"], true)
						stream, ok := mapping.(*Stream)
						if v["Subtype"] != Name("CIDFontType2") || err != nil || !ok {
							unsafe[ref] = true
						} else {
							data, err := r.optimizationFontData(ctx, stream, 131072)
							if err != nil || len(data)%2 != 0 || len(data) > 131072 {
								unsafe[ref] = true
							} else {
								for i := 0; i < len(data); i += 2 {
									used[ref][binary.BigEndian.Uint16(data[i:])] = true
								}
							}
						}
					}
				}
			}
			for key, item := range v {
				if key == "FontFile2" {
					continue
				}
				if err := walk(item, depth+1); err != nil {
					return err
				}
			}
		}
		return nil
	}
	for _, value := range metadata {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if err := walk(value, 0); err != nil {
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

// compactTrueType 裁剪静态TrueType轮廓并保留编号、复合依赖、字符表和提示指令
// 未知表、签名字体及异常结构不改写
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
		case "cmap", "head", "hhea", "hmtx", "maxp", "OS/2", "post", "name", "glyf", "loca", "cvt ", "fpgm", "prep", "gasp", "kern", "vhea", "vmtx", "hdmx", "LTSH", "VDMX", "GDEF", "GPOS", "FFTM":
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
	var compact, locations []byte
	for id := 0; id <= count; id++ {
		if format == 0 {
			locations = binary.BigEndian.AppendUint16(locations, uint16(len(compact)/2))
		} else {
			locations = binary.BigEndian.AppendUint32(locations, uint32(len(compact)))
		}
		if id < count && keep[id] {
			compact = append(compact, glyf[offsets[id]:offsets[id+1]]...)
			if len(compact)%2 != 0 {
				compact = append(compact, 0)
			}
		}
	}
	if len(compact) >= len(glyf) {
		return data, nil
	}
	tables["glyf"], tables["loca"] = compact, locations
	tables["head"] = bytes.Clone(head)
	clear(tables["head"][8:12])
	out := bytes.Clone(data[:12])
	out = append(out, make([]byte, n*16)...)
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

// optimizationFontData 有界解码字体相关流，不展开未知或多重过滤器
// 入参: ctx 取消上下文, stream 字体或映射流, limit 解码上限
// 返回: []byte 解码数据, error 解码或容量错误
func (r *Reader) optimizationFontData(ctx context.Context, stream *Stream, limit int) ([]byte, error) {
	filters, params, err := stream.filterChain(r)
	if err != nil {
		return nil, err
	}
	if stream.Dictionary["F"] != nil || len(filters) > 1 || len(filters) == 1 && filters[0] != Name("FlateDecode") {
		return nil, io.ErrUnexpectedEOF
	}
	data := stream.Data
	if len(filters) == 1 {
		input, err := zlib.NewReader(bytes.NewReader(data))
		if err != nil {
			return nil, err
		}
		data, err = io.ReadAll(io.LimitReader(&contextInput{ctx: ctx, reader: input}, int64(limit)+1))
		input.Close()
		if err != nil {
			return nil, err
		}
		if len(data) > limit {
			return nil, io.ErrShortBuffer
		}
		parameters, _ := params[0].(Dictionary)
		data, err = decodePredictorContext(ctx, data, parameters)
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
	if err != nil || len(data) > optimizationBufferLimit {
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
	result.Dictionary["Length1"] = Integer(len(compact))
	result.Dictionary["Filter"] = Name("FlateDecode")
	delete(result.Dictionary, "DecodeParms")
	if stream.decrypted {
		name, err := r.security.outputStreamFilter(stream, r)
		if err != nil {
			return stream, nil
		}
		result.Dictionary["Filter"] = Array{Name("Crypt"), Name("FlateDecode")}
		result.Dictionary["DecodeParms"] = Array{Dictionary{"Name": name}, nil}
	}
	return &result, nil
}
