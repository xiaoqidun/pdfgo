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
	"maps"
)

// optimizationAliases 合并字典及编码完全相同的图片和字体程序，保留颜色、蒙版及身份语义
// 入参: ctx 取消上下文, refs 可达对象, cached 原有缓存, metadata 已读取的对象属性
// 返回: map[Reference]Reference 重复引用, error 读取或取消错误
func (r *Reader) optimizationAliases(ctx context.Context, refs []Reference, cached map[Reference]bool, metadata map[Reference]Object) (map[Reference]Reference, error) {
	aliases := make(map[Reference]Reference)
	seen := make(map[[32]byte]Reference)
	fonts := make(map[Reference]bool)
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
			for _, key := range []Name{"FontFile", "FontFile2", "FontFile3"} {
				if ref, ok := v[key].(Reference); ok && v["Type"] == Name("FontDescriptor") {
					fonts[ref] = true
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
	for _, ref := range refs {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if !fonts[ref] {
			dict, ok := metadata[ref].(Dictionary)
			if !ok || dict["Subtype"] != Name("Image") {
				continue
			}
		}
		value, err := r.Object(ref)
		if err != nil {
			return nil, err
		}
		if !cached[ref] {
			delete(r.cache, ref)
		}
		s, ok := value.(*Stream)
		if !ok || s.Dictionary["Subtype"] != Name("Image") && !fonts[ref] || s.Dictionary["StructParent"] != nil || s.Dictionary["ID"] != nil || s.Dictionary["OPI"] != nil || s.Dictionary["F"] != nil {
			continue
		}
		dict := maps.Clone(s.Dictionary)
		delete(dict, "Length")
		hash := sha256.New()
		if fonts[ref] {
			hash.Write([]byte("font"))
		} else {
			hash.Write([]byte("image"))
		}
		out := pdfOutput{ctx: ctx, writer: hash}
		if err := out.object(dict, Reference{}, 0, false); err != nil {
			return nil, err
		}
		if _, err := out.Write(s.Data); err != nil {
			return nil, err
		}
		var key [32]byte
		copy(key[:], hash.Sum(nil))
		if first, ok := seen[key]; ok {
			aliases[ref] = first
		} else {
			seen[key] = ref
		}
	}
	return aliases, nil
}
