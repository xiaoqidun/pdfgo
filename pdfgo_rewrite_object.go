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
	"math"
	"reflect"
)

// rewriteObjectIdentity 区分共享字典、数组和流，避免展开重复对象
type rewriteObjectIdentity struct {
	kind    byte
	pointer uintptr
	length  int
}

// rewriteObjectCopy 将资源参数复制到独立输出对象，不保留源文件引用
type rewriteObjectCopy struct {
	ctx     context.Context
	source  *Reader
	objects map[Reference]Object
	number  *int64
	copied  map[rewriteObjectIdentity]Reference
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
	if ref, ok := value.(Reference); ok {
		if c.source == nil {
			return nil, fmt.Errorf("PDF resource reference has no source reader")
		}
		var err error
		value, err = c.source.Resolve(ref)
		if err != nil {
			return nil, err
		}
	}
	var id rewriteObjectIdentity
	switch v := value.(type) {
	case nil, Boolean, Integer, Name:
		return value, nil
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
		return ref, nil
	}
	ref, err := appendRewriteObject(c.objects, c.number, nil)
	if err != nil {
		return nil, err
	}
	c.copied[id] = ref
	switch v := value.(type) {
	case Dictionary:
		result := make(Dictionary, len(v))
		for key, value := range v {
			result[key], err = c.copy(value, depth+1)
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
	dict := make(Dictionary, len(stream.Dictionary))
	nested := *c
	if stream.reader != nil {
		nested.source = stream.reader
	}
	filters, parameters, err := stream.filterChain(nested.source)
	if err != nil {
		return nil, err
	}
	for i, filter := range filters {
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
	for key, value := range stream.Dictionary {
		if key == "Length" || key == "Filter" || key == "DecodeParms" {
			continue
		}
		dict[key], err = nested.copy(value, depth+1)
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
