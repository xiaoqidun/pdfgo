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
	"encoding/binary"
	"fmt"
	"math"
	"slices"
)

// EncodeGlyphs 将内嵌字体的字形编号转换为源字体字符码，不修改字体资源
// 字形须由源编码直接访问，不使用缺省字形；原文语义和字形定位由调用方保留
// 入参: glyphs 字形编号序列
// 返回: []byte 文字显示操作的字符码, error 缺少编码或字形错误
func (f *Font) EncodeGlyphs(glyphs []uint16) ([]byte, error) {
	return f.EncodeGlyphsContext(context.Background(), glyphs)
}

// EncodeGlyphsContext 按源字体编码字形序列，失败或取消时不返回部分结果
// 入参: ctx 取消上下文, glyphs 字形编号序列
// 返回: []byte 文字显示操作的字符码, error 编码或取消错误
func (f *Font) EncodeGlyphsContext(ctx context.Context, glyphs []uint16) ([]byte, error) {
	if f == nil || ctx == nil {
		return nil, fmt.Errorf("missing font or encoding context")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if len(glyphs) == 0 {
		return nil, nil
	}
	if len(f.Program) == 0 {
		return nil, &UnsupportedError{Feature: "glyph encoding without embedded font"}
	}
	wanted := make(map[uint16]int)
	for _, glyph := range glyphs {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if glyph == 0 {
			return nil, fmt.Errorf("cannot encode undefined font glyph")
		}
		wanted[glyph]++
	}
	lookup, err := f.glyphLookup(ctx, wanted)
	if err != nil {
		return nil, err
	}
	size := 0
	for glyph, count := range wanted {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		entry, ok := lookup[glyph]
		if !ok {
			return nil, &UnsupportedError{Feature: fmt.Sprintf("source font encoding for glyph %d", glyph)}
		}
		if count > (math.MaxInt-size)/len(entry.code) {
			return nil, fmt.Errorf("font encoding exceeds byte range")
		}
		size += count * len(entry.code)
	}
	result := make([]byte, 0, size)
	for _, glyph := range glyphs {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		result = append(result, lookup[glyph].code...)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return result, nil
}

// glyphLookup 反查字形可用的源字符码及度量，优先选择较短且字节序较小的编码
// 入参: ctx 取消上下文, wanted 字形计数
// 返回: map[uint16]fontEncodedGlyph 编码与字形, error 取消错误
func (f *Font) glyphLookup(ctx context.Context, wanted map[uint16]int) (map[uint16]fontEncodedGlyph, error) {
	var codes []string
	if f.composite {
		cids, err := f.glyphCIDs(ctx, wanted)
		if err != nil {
			return nil, err
		}
		if f.cidMap != nil {
			codes, err = f.cidMap.characterCodes(ctx, cids)
			if err != nil {
				return nil, err
			}
		} else {
			for _, cid := range cids {
				codes = append(codes, string([]byte{byte(cid >> 8), byte(cid)}))
			}
		}
	} else {
		for code := 0; code < 256; code++ {
			codes = append(codes, string([]byte{byte(code)}))
		}
	}
	lookup := make(map[uint16]fontEncodedGlyph, len(wanted))
	for _, code := range codes {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		glyph, ok, _ := f.encodingGlyph(ctx, code)
		if !ok || !glyph.HasID || wanted[glyph.ID] == 0 {
			continue
		}
		previous, exists := lookup[glyph.ID]
		if !exists || len(code) < len(previous.code) || len(code) == len(previous.code) && code < previous.code {
			lookup[glyph.ID] = fontEncodedGlyph{code: code, glyph: glyph}
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return lookup, nil
}

// glyphCIDs 收集可直接访问所需字形的CID，不将未定义字符映射作为候选
// 入参: ctx 取消上下文, wanted 字形计数
// 返回: []uint16 升序CID, error 取消错误
func (f *Font) glyphCIDs(ctx context.Context, wanted map[uint16]int) ([]uint16, error) {
	var cids []uint16
	switch {
	case f.cffGlyphs != nil:
		for cid, glyph := range f.cffGlyphs {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			if cid != 0 && cid <= 65535 && wanted[glyph] != 0 {
				cids = append(cids, uint16(cid))
			}
		}
	case f.glyphMap != nil:
		for cid := 1; cid < min(65536, len(f.glyphMap)/2); cid++ {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			if wanted[binary.BigEndian.Uint16(f.glyphMap[cid*2:])] != 0 && f.hasCIDGlyph(uint32(cid)) {
				cids = append(cids, uint16(cid))
			}
		}
	default:
		for glyph := range wanted {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			if glyph != 0 && f.hasCIDGlyph(uint32(glyph)) {
				cids = append(cids, glyph)
			}
		}
	}
	slices.Sort(cids)
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return cids, nil
}
