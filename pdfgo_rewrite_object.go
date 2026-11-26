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
	"fmt"
	"maps"
	"math"
	"reflect"
	"slices"
)

// rewriteObjectIdentity 区分共享字典、数组和流，避免展开重复对象
type rewriteObjectIdentity struct {
	kind    byte
	pointer uintptr
	length  int
}

// rewriteRefIdentity 区分源阅读器与间接对象引用，避免共享或循环大流重复展开
type rewriteRefIdentity struct {
	source *Reader
	ref    Reference
}

// rewriteObjectCopy 将资源参数复制到独立输出对象，不保留源文件引用
type rewriteObjectCopy struct {
	ctx       context.Context
	source    *Reader
	objects   map[Reference]Object
	number    *int64
	copied    map[rewriteObjectIdentity]Reference
	refCopied map[rewriteRefIdentity]Reference
	pinned    map[rewriteObjectIdentity]Object
}

// ensureMaps 惰性初始化引用与内联容器去重映射，保证值拷贝共享底层表
func (c *rewriteObjectCopy) ensureMaps() {
	if c.copied == nil {
		c.copied = make(map[rewriteObjectIdentity]Reference)
	}
	if c.refCopied == nil {
		c.refCopied = make(map[rewriteRefIdentity]Reference)
	}
	if c.pinned == nil {
		c.pinned = make(map[rewriteObjectIdentity]Object)
	}
}

// copy 保留资源图中的共享、循环及流编码，移除源文件的加密过滤器
// 入参: value 源对象, depth 直接嵌套深度
// 返回: Object 独立对象或输出引用, error 引用、类型、过滤器或取消错误
func (c *rewriteObjectCopy) copy(value Object, depth int) (Object, error) {
	if err := c.ctx.Err(); err != nil {
		return nil, err
	}
	if depth > 256 {
		return nil, fmt.Errorf("excessive PDF resource nesting")
	}
	c.ensureMaps()
	sourceKey := c.source
	if sourceKey != nil && sourceKey.owner != nil {
		sourceKey = sourceKey.owner
	}
	var sourceRefs []Reference
	if ref, ok := value.(Reference); ok {
		if c.source == nil {
			return nil, fmt.Errorf("PDF resource reference has no source reader")
		}
		cur := ref
		var seen map[Reference]bool
		for {
			if err := c.ctx.Err(); err != nil {
				return nil, err
			}
			if outRef, exists := c.refCopied[rewriteRefIdentity{source: sourceKey, ref: cur}]; exists {
				for _, prev := range sourceRefs {
					c.refCopied[rewriteRefIdentity{source: sourceKey, ref: prev}] = outRef
				}
				return outRef, nil
			}
			if seen[cur] {
				return nil, fmt.Errorf("cyclic indirect reference %d %d", cur.Number, cur.Generation)
			}
			if seen == nil {
				seen = make(map[Reference]bool)
			}
			seen[cur] = true
			sourceRefs = append(sourceRefs, cur)
			resolved, err := c.source.Object(cur)
			if err != nil {
				return nil, err
			}
			next, isNextRef := resolved.(Reference)
			if !isNextRef {
				value = resolved
				break
			}
			cur = next
		}
	}
	var id rewriteObjectIdentity
	switch v := value.(type) {
	case nil, Boolean, Integer:
		return value, nil
	case Name:
		if !validPDFName(v) {
			return nil, fmt.Errorf("null byte in PDF name")
		}
		return v, nil
	case Real:
		if math.IsNaN(float64(v)) || math.IsInf(float64(v), 0) {
			return nil, fmt.Errorf("invalid PDF resource number")
		}
		return v, nil
	case String:
		return String(bytes.Clone(v)), nil
	case Dictionary:
		id = rewriteObjectIdentity{kind: 'd', pointer: reflect.ValueOf(v).Pointer()}
	case Array:
		id = rewriteObjectIdentity{kind: 'a', pointer: reflect.ValueOf(v).Pointer(), length: len(v)}
	case *Stream:
		if v == nil {
			return nil, fmt.Errorf("missing PDF resource stream")
		}
		id = rewriteObjectIdentity{kind: 's', pointer: reflect.ValueOf(v).Pointer()}
	default:
		return nil, fmt.Errorf("invalid PDF resource object")
	}
	if ref, exists := c.copied[id]; exists {
		for _, prev := range sourceRefs {
			c.refCopied[rewriteRefIdentity{source: sourceKey, ref: prev}] = ref
		}
		return ref, nil
	}
	ref, err := appendRewriteObject(c.objects, c.number, nil)
	if err != nil {
		return nil, err
	}
	if len(sourceRefs) > 0 {
		for _, prev := range sourceRefs {
			c.refCopied[rewriteRefIdentity{source: sourceKey, ref: prev}] = ref
		}
	} else {
		c.copied[id] = ref
		c.pinned[id] = value
	}
	switch v := value.(type) {
	case Dictionary:
		result := make(Dictionary, len(v))
		for _, key := range slices.Sorted(maps.Keys(v)) {
			if !validPDFName(key) {
				return nil, fmt.Errorf("null byte in PDF name")
			}
			result[key], err = c.copy(v[key], depth+1)
			if err != nil {
				return nil, err
			}
		}
		c.objects[ref] = result
	case Array:
		result := make(Array, len(v))
		for i, value := range v {
			result[i], err = c.copy(value, depth+1)
			if err != nil {
				return nil, err
			}
		}
		c.objects[ref] = result
	case *Stream:
		c.objects[ref], err = c.copyStream(v, depth)
		if err != nil {
			return nil, err
		}
	}
	return ref, c.ctx.Err()
}

// copyStream 复制流编码及参数，将资源引用迁移到输出对象
// 入参: stream 源数据流, depth 直接嵌套深度
// 返回: *Stream 独立数据流, error 参数、引用或取消错误
func (c *rewriteObjectCopy) copyStream(stream *Stream, depth int) (*Stream, error) {
	if err := c.ctx.Err(); err != nil {
		return nil, err
	}
	c.ensureMaps()
	dict := make(Dictionary, len(stream.Dictionary))
	nested := *c
	if !c.source.ownsObject(stream.reader, stream.cache) {
		nested.source = stream.reader
	}
	filters, parameters, err := stream.filterChain(nested.source)
	if err != nil {
		return nil, err
	}
	for i, filter := range filters {
		if name, ok := filter.(Name); ok && !validPDFName(name) {
			return nil, fmt.Errorf("null byte in PDF name")
		}
		if filter != Name("Crypt") {
			continue
		}
		param, _ := parameters[i].(Dictionary)
		if i != 0 || param["Name"] != nil && param["Name"] != Name("Identity") {
			return nil, &UnsupportedError{Feature: "stream crypt filter"}
		}
	}
	if len(filters) != 0 && filters[0] == Name("Crypt") {
		filters, parameters = filters[1:], parameters[1:]
	}
	for _, key := range slices.Sorted(maps.Keys(stream.Dictionary)) {
		if !validPDFName(key) {
			return nil, fmt.Errorf("null byte in PDF name")
		}
		if key == "Length" || key == "Filter" || key == "DecodeParms" {
			continue
		}
		dict[key], err = nested.copy(stream.Dictionary[key], depth+1)
		if err != nil {
			return nil, err
		}
	}
	if len(filters) == 1 {
		dict["Filter"] = filters[0]
		if parameters[0] != nil {
			dict["DecodeParms"], err = nested.copy(parameters[0], depth+1)
		}
	} else if len(filters) > 1 {
		dict["Filter"] = filters
		dict["DecodeParms"], err = nested.copy(parameters, depth+1)
	}
	if err != nil {
		return nil, err
	}
	return &Stream{Dictionary: dict, Data: bytes.Clone(stream.Data)}, c.ctx.Err()
}
