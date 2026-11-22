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
	"image"
	"image/color"
	"maps"
)

// hasStreamIdentityKeys 检查流字典是否包含不可合并的身份或外部引用属性
// 入参: dict 流字典
// 返回: bool 是否含受保护属性
func hasStreamIdentityKeys(dict Dictionary) bool {
	return dict["StructParent"] != nil || dict["StructParents"] != nil || dict["ID"] != nil || dict["OPI"] != nil || dict["F"] != nil || dict["Ref"] != nil
}

// hasDictIdentityKeys 检查非流字典是否包含页面、结构、交互或外部身份语义键
// 入参: dict 对象字典
// 返回: bool 是否含受保护属性
func hasDictIdentityKeys(dict Dictionary) bool {
	return hasStreamIdentityKeys(dict) || dict["Parent"] != nil || dict["Kids"] != nil || dict["Annots"] != nil || dict["P"] != nil || dict["T"] != nil || dict["V"] != nil || dict["Rect"] != nil
}

// hashLazyReplacementImage 流式计算延迟替换图像的字典、配置及像素内容摘要，不分配整图编码缓冲
// 入参: ctx 取消上下文, item 延迟替换图像, aliases 已有合并引用
// 返回: [32]byte 内容摘要, error 哈希或取消错误
func hashLazyReplacementImage(ctx context.Context, item *lazyReplacementImage, aliases map[Reference]Reference) ([32]byte, error) {
	hash := sha256.New()
	hash.Write([]byte("lazyimage\x00"))
	out := pdfOutput{ctx: ctx, writer: hash, aliases: aliases}
	if err := out.object(item.baseDict, Reference{}, 0, false); err != nil {
		return [32]byte{}, err
	}
	var flags [2]byte
	if item.interpolate {
		flags[0] = 1
	}
	if item.options.PreblendBinary {
		flags[1] = 1
	}
	hash.Write(flags[:])
	img := unwrapImage(item.image)
	bits, channels, space, alwaysOpaque := imageEncodingSpec(img)
	var spec [6]byte
	spec[0] = byte(bits)
	spec[1] = byte(channels)
	if alwaysOpaque {
		spec[2] = 1
	}
	if img.ColorModel() == color.RGBAModel {
		spec[3] = 1
	}
	binary.BigEndian.PutUint16(spec[4:6], uint16(len(space)))
	hash.Write(spec[:])
	hash.Write([]byte(space))
	b := img.Bounds()
	var dims [12]byte
	binary.BigEndian.PutUint32(dims[0:4], uint32(b.Dx()))
	binary.BigEndian.PutUint32(dims[4:8], uint32(b.Dy()))
	switch src := img.(type) {
	case *image.RGBA:
		binary.BigEndian.PutUint32(dims[8:12], 1)
		hash.Write(dims[:])
		for y := b.Min.Y; y < b.Max.Y; y++ {
			if err := ctx.Err(); err != nil {
				return [32]byte{}, err
			}
			off := src.PixOffset(b.Min.X, y)
			hash.Write(src.Pix[off : off+b.Dx()*4])
		}
	case *image.NRGBA:
		binary.BigEndian.PutUint32(dims[8:12], 2)
		hash.Write(dims[:])
		for y := b.Min.Y; y < b.Max.Y; y++ {
			if err := ctx.Err(); err != nil {
				return [32]byte{}, err
			}
			off := src.PixOffset(b.Min.X, y)
			hash.Write(src.Pix[off : off+b.Dx()*4])
		}
	case *image.Gray:
		binary.BigEndian.PutUint32(dims[8:12], 3)
		hash.Write(dims[:])
		for y := b.Min.Y; y < b.Max.Y; y++ {
			if err := ctx.Err(); err != nil {
				return [32]byte{}, err
			}
			off := src.PixOffset(b.Min.X, y)
			hash.Write(src.Pix[off : off+b.Dx()])
		}
	case *image.Gray16:
		binary.BigEndian.PutUint32(dims[8:12], 4)
		hash.Write(dims[:])
		for y := b.Min.Y; y < b.Max.Y; y++ {
			if err := ctx.Err(); err != nil {
				return [32]byte{}, err
			}
			off := src.PixOffset(b.Min.X, y)
			hash.Write(src.Pix[off : off+b.Dx()*2])
		}
	case *image.CMYK:
		binary.BigEndian.PutUint32(dims[8:12], 5)
		hash.Write(dims[:])
		for y := b.Min.Y; y < b.Max.Y; y++ {
			if err := ctx.Err(); err != nil {
				return [32]byte{}, err
			}
			off := src.PixOffset(b.Min.X, y)
			hash.Write(src.Pix[off : off+b.Dx()*4])
		}
	case *image.Paletted:
		binary.BigEndian.PutUint32(dims[8:12], 6)
		hash.Write(dims[:])
		var palLen [4]byte
		binary.BigEndian.PutUint32(palLen[:], uint32(len(src.Palette)))
		hash.Write(palLen[:])
		var entry [8]byte
		for _, colorEntry := range src.Palette {
			c := imageNRGBASample(colorEntry)
			binary.BigEndian.PutUint16(entry[0:2], c.R)
			binary.BigEndian.PutUint16(entry[2:4], c.G)
			binary.BigEndian.PutUint16(entry[4:6], c.B)
			binary.BigEndian.PutUint16(entry[6:8], c.A)
			hash.Write(entry[:])
		}
		for y := b.Min.Y; y < b.Max.Y; y++ {
			if err := ctx.Err(); err != nil {
				return [32]byte{}, err
			}
			off := src.PixOffset(b.Min.X, y)
			hash.Write(src.Pix[off : off+b.Dx()])
		}
	default:
		binary.BigEndian.PutUint32(dims[8:12], 7)
		hash.Write(dims[:])
		line := make([]byte, b.Dx()*8)
		for y := b.Min.Y; y < b.Max.Y; y++ {
			if err := ctx.Err(); err != nil {
				return [32]byte{}, err
			}
			for x := 0; x < b.Dx(); x++ {
				c := imageNRGBA64At(img, b.Min.X+x, y)
				off := x * 8
				binary.BigEndian.PutUint16(line[off:off+2], c.R)
				binary.BigEndian.PutUint16(line[off+2:off+4], c.G)
				binary.BigEndian.PutUint16(line[off+4:off+6], c.B)
				binary.BigEndian.PutUint16(line[off+6:off+8], c.A)
			}
			hash.Write(line)
		}
	}
	var key [32]byte
	copy(key[:], hash.Sum(nil))
	return key, nil
}

// optimizationAliases 合并完全一致的图片、字体、内容流、表单、ToUnicode、状态与资源字典及替换图像
// 复用扫描阶段已计算的流内容摘要，不重复读取原始流数据，并保持对象身份与语义边界
// 入参: ctx 取消上下文, refs 可达对象, isStream 流对象标记, metadata 已读取的对象属性, streamHashes 已计算的流内容摘要, skip 延迟编码替换图像集合, protected 不可合并的根级保护引用, lightOnly 是否仅执行轻量流与替换图去重
// 返回: map[Reference]Reference 重复引用映射, error 哈希或取消错误
func (r *Reader) optimizationAliases(ctx context.Context, refs []Reference, isStream map[Reference]bool, metadata map[Reference]Object, streamHashes map[Reference][32]byte, skip map[Reference]*lazyReplacementImage, protected map[Reference]bool, lightOnly bool) (map[Reference]Reference, error) {
	aliases := make(map[Reference]Reference)
	fonts := make(map[Reference]bool)
	contents := make(map[Reference]bool)
	toUnicode := make(map[Reference]bool)
	extGStates := make(map[Reference]bool)
	resources := make(map[Reference]bool)
	var collect func(Object, int)
	collect = func(value Object, depth int) {
		if depth > 256 {
			return
		}
		switch v := value.(type) {
		case Array:
			for _, item := range v {
				collect(item, depth+1)
			}
		case *Stream:
			collect(v.Dictionary, depth+1)
		case Dictionary:
			if v["Type"] == Name("FontDescriptor") {
				for _, key := range []Name{"FontFile", "FontFile2", "FontFile3"} {
					if ref, ok := v[key].(Reference); ok {
						fonts[ref] = true
					}
				}
			}
			if v["Type"] == Name("Page") {
				switch c := v["Contents"].(type) {
				case Reference:
					contents[c] = true
				case Array:
					for _, item := range c {
						if ref, ok := item.(Reference); ok {
							contents[ref] = true
						}
					}
				}
			}
			if ref, ok := v["ToUnicode"].(Reference); ok {
				toUnicode[ref] = true
			}
			if ref, ok := v["Resources"].(Reference); ok {
				resources[ref] = true
			}
			if gs, ok := v["ExtGState"].(Dictionary); ok {
				for _, item := range gs {
					if ref, ok := item.(Reference); ok {
						extGStates[ref] = true
					}
				}
			}
			for _, item := range v {
				collect(item, depth+1)
			}
		}
	}
	for _, ref := range refs {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		collect(metadata[ref], 0)
	}
	canonicalOf := func(ref Reference) Reference {
		for range 8 {
			next, ok := aliases[ref]
			if !ok || next == ref {
				break
			}
			ref = next
		}
		return ref
	}
	dedupStream := func(tag string, match func(Reference, Dictionary) bool) error {
		seen := make(map[[32]byte]Reference)
		for _, ref := range refs {
			if err := ctx.Err(); err != nil {
				return err
			}
			if aliases[ref].Number != 0 || protected[ref] || skip[ref] != nil || !isStream[ref] {
				continue
			}
			dict, ok := metadata[ref].(Dictionary)
			if !ok || !match(ref, dict) || hasStreamIdentityKeys(dict) {
				continue
			}
			dataHash, ok := streamHashes[ref]
			if !ok {
				continue
			}
			clean := maps.Clone(dict)
			delete(clean, "Length")
			hash := sha256.New()
			hash.Write([]byte(tag))
			out := pdfOutput{ctx: ctx, writer: hash, aliases: aliases}
			if err := out.object(clean, Reference{}, 0, false); err != nil {
				return err
			}
			hash.Write(dataHash[:])
			var key [32]byte
			copy(key[:], hash.Sum(nil))
			if first, ok := seen[key]; ok {
				aliases[ref] = canonicalOf(first)
			} else {
				seen[key] = ref
			}
		}
		return nil
	}
	if err := dedupStream("font\x00", func(ref Reference, _ Dictionary) bool { return fonts[ref] }); err != nil {
		return nil, err
	}
	if err := dedupStream("image\x00", func(ref Reference, dict Dictionary) bool { return !fonts[ref] && dict["Subtype"] == Name("Image") }); err != nil {
		return nil, err
	}
	if err := dedupStream("tounicode\x00", func(ref Reference, dict Dictionary) bool {
		return toUnicode[ref] && !fonts[ref] && dict["Subtype"] == nil
	}); err != nil {
		return nil, err
	}
	if err := dedupStream("contents\x00", func(ref Reference, dict Dictionary) bool {
		return contents[ref] && !fonts[ref] && !toUnicode[ref] && dict["Type"] == nil && dict["Subtype"] == nil
	}); err != nil {
		return nil, err
	}
	if len(skip) > 1 {
		seenLazy := make(map[[32]byte]Reference)
		for _, ref := range refs {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			lazy := skip[ref]
			if lazy == nil || protected[ref] || hasStreamIdentityKeys(lazy.baseDict) {
				continue
			}
			key, err := hashLazyReplacementImage(ctx, lazy, aliases)
			if err != nil {
				return nil, err
			}
			if first, ok := seenLazy[key]; ok {
				aliases[ref] = canonicalOf(first)
			} else {
				seenLazy[key] = ref
			}
		}
	}
	if !lightOnly {
		dedupDict := func(tag string, match func(Reference, Dictionary) bool) error {
			seen := make(map[[32]byte]Reference)
			for _, ref := range refs {
				if err := ctx.Err(); err != nil {
					return err
				}
				if aliases[ref].Number != 0 || protected[ref] || skip[ref] != nil || isStream[ref] {
					continue
				}
				dict, ok := metadata[ref].(Dictionary)
				if !ok || !match(ref, dict) || hasDictIdentityKeys(dict) {
					continue
				}
				hash := sha256.New()
				hash.Write([]byte(tag))
				out := pdfOutput{ctx: ctx, writer: hash, aliases: aliases}
				if err := out.object(dict, Reference{}, 0, false); err != nil {
					return err
				}
				var key [32]byte
				copy(key[:], hash.Sum(nil))
				if first, ok := seen[key]; ok {
					aliases[ref] = canonicalOf(first)
				} else {
					seen[key] = ref
				}
			}
			return nil
		}
		if err := dedupDict("extgstate\x00", func(ref Reference, dict Dictionary) bool {
			return extGStates[ref] && (dict["Type"] == nil || dict["Type"] == Name("ExtGState"))
		}); err != nil {
			return nil, err
		}
		if err := dedupDict("resources\x00", func(ref Reference, dict Dictionary) bool {
			return resources[ref] && dict["Type"] == nil
		}); err != nil {
			return nil, err
		}
	}
	if err := dedupStream("form\x00", func(ref Reference, dict Dictionary) bool {
		return !fonts[ref] && dict["Subtype"] == Name("Form")
	}); err != nil {
		return nil, err
	}
	if !lightOnly {
		seenRes := make(map[[32]byte]Reference)
		for _, ref := range refs {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			if aliases[ref].Number != 0 || protected[ref] || skip[ref] != nil || isStream[ref] || !resources[ref] {
				continue
			}
			dict, ok := metadata[ref].(Dictionary)
			if !ok || dict["Type"] != nil || hasDictIdentityKeys(dict) {
				continue
			}
			hash := sha256.New()
			hash.Write([]byte("resources2\x00"))
			out := pdfOutput{ctx: ctx, writer: hash, aliases: aliases}
			if err := out.object(dict, Reference{}, 0, false); err != nil {
				return nil, err
			}
			var key [32]byte
			copy(key[:], hash.Sum(nil))
			if first, ok := seenRes[key]; ok {
				aliases[ref] = canonicalOf(first)
			} else {
				seenRes[key] = ref
			}
		}
	}
	for ref, target := range aliases {
		aliases[ref] = canonicalOf(target)
	}
	return aliases, nil
}
