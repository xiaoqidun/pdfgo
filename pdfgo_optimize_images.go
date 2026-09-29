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
	"fmt"
	"image"
	"io"
	"maps"
	"math"
)

// unwrapImageStream 去除JPEG外层通用编码，保留解码参数和显式加密策略
// 入参: ctx 取消上下文, stream 原始流, filters 过滤器, params 解码参数
// 返回: *Stream 内层JPEG流，不支持时为空, error 取消错误
func (r *Reader) unwrapImageStream(ctx context.Context, stream *Stream, filters, params Array) (*Stream, error) {
	data := stream.Data
	for i, filter := range filters[:len(filters)-1] {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		var err error
		switch filter {
		case Name("FlateDecode"):
			input, e := zlib.NewReader(bytes.NewReader(data))
			if e != nil {
				return nil, nil
			}
			data, err = io.ReadAll(io.LimitReader(&contextInput{ctx: ctx, reader: input}, optimizationBufferLimit+1))
			input.Close()
			if err == nil && len(data) <= optimizationBufferLimit {
				dict, _ := params[i].(Dictionary)
				data, err = decodePredictor(data, dict)
			}
		case Name("ASCIIHexDecode"), Name("ASCII85Decode"):
			wrapper := &Stream{Dictionary: Dictionary{"Filter": filter, "DecodeParms": params[i]}, Data: data}
			data, err = wrapper.Decode()
		default:
			return nil, nil
		}
		if err != nil || len(data) > optimizationBufferLimit {
			return nil, ctx.Err()
		}
	}
	inner := *stream
	inner.Dictionary = maps.Clone(stream.Dictionary)
	inner.Data = data
	inner.Dictionary["Filter"] = Name("DCTDecode")
	inner.Dictionary["DecodeParms"] = params[len(params)-1]
	if stream.decrypted {
		name, err := r.security.outputStreamFilter(stream, r)
		if err != nil {
			return nil, nil
		}
		inner.Dictionary["Filter"] = Array{Name("Crypt"), Name("DCTDecode")}
		inner.Dictionary["DecodeParms"] = Array{Dictionary{"Name": name}, params[len(params)-1]}
	}
	return &inner, ctx.Err()
}

// imageOutputSizes 汇总页面及表单中图片的最大显示尺寸，不完整时放弃降采样
// 入参: ctx 取消上下文, dpi 分辨率上限
// 返回: map[Reference]image.Point 图片像素上限, error 取消错误
func (r *Reader) imageOutputSizes(ctx context.Context, dpi int) (map[Reference]image.Point, error) {
	if dpi == 0 {
		return nil, nil
	}
	sizes := make(map[Reference]image.Point)
	cached := make(map[Reference]bool, len(r.cache))
	for ref := range r.cache {
		cached[ref] = true
	}
	cleanup := func() {
		for ref := range r.cache {
			if !cached[ref] {
				delete(r.cache, ref)
			}
		}
	}
	defer cleanup()
	budget := 100000
	var walk func([]byte, Dictionary, Matrix, int) error
	walk = func(data []byte, resources Dictionary, matrix Matrix, depth int) error {
		if depth > 32 {
			return fmt.Errorf("image placement recursion limit exceeded")
		}
		var stack []Matrix
		return WalkOperations(ctx, data, func(op Operation) error {
			switch op.Operator {
			case "q":
				stack = append(stack, matrix)
			case "Q":
				if len(stack) == 0 {
					return fmt.Errorf("unbalanced image placement state")
				}
				matrix = stack[len(stack)-1]
				stack = stack[:len(stack)-1]
			case "cm":
				values, err := numbers(op.Operands, 6)
				if err != nil {
					return err
				}
				matrix = matrix.Mul(Matrix(values))
			case "Do":
				budget--
				if budget < 0 {
					return fmt.Errorf("image placement operation limit exceeded")
				}
				if len(op.Operands) != 1 {
					return fmt.Errorf("invalid image placement")
				}
				name, ok := op.Operands[0].(Name)
				if !ok {
					return fmt.Errorf("invalid image name")
				}
				value, err := r.Resolve(resources["XObject"])
				if err != nil {
					return err
				}
				objects, ok := value.(Dictionary)
				if !ok {
					return fmt.Errorf("missing image resources")
				}
				value = objects[name]
				var ref Reference
				for n := 0; ; n++ {
					next, ok := value.(Reference)
					if !ok {
						break
					}
					if n > 32 {
						return fmt.Errorf("cyclic image reference")
					}
					ref = next
					value, err = r.Object(ref)
					if err != nil {
						return err
					}
					if !cached[ref] {
						delete(r.cache, ref)
					}
				}
				stream, ok := value.(*Stream)
				if !ok {
					return fmt.Errorf("invalid image resource")
				}
				switch stream.Dictionary["Subtype"] {
				case Name("Image"):
					w := math.Ceil(math.Hypot(matrix[0], matrix[1]) * float64(dpi) / 72)
					h := math.Ceil(math.Hypot(matrix[2], matrix[3]) * float64(dpi) / 72)
					if math.IsNaN(w) || math.IsNaN(h) || math.IsInf(w, 0) || math.IsInf(h, 0) || w > 1<<30 || h > 1<<30 {
						return fmt.Errorf("invalid image dimensions")
					}
					if ref.Number > 0 {
						old := sizes[ref]
						sizes[ref] = image.Pt(max(old.X, max(1, int(w))), max(old.Y, max(1, int(h))))
					}
				case Name("Form"):
					child := matrix
					childResources := resources
					if stream.Dictionary["Matrix"] != nil {
						value, err := r.Resolve(stream.Dictionary["Matrix"])
						if err != nil {
							return err
						}
						array, ok := value.(Array)
						if !ok {
							return fmt.Errorf("invalid image form matrix")
						}
						values, err := r.numberArray(array, 6)
						if err != nil {
							return err
						}
						child = child.Mul(Matrix(values))
					}
					if stream.Dictionary["Resources"] != nil {
						value, err := r.Resolve(stream.Dictionary["Resources"])
						if err != nil {
							return err
						}
						var ok bool
						childResources, ok = value.(Dictionary)
						if !ok {
							return fmt.Errorf("invalid image form resources")
						}
					}
					content, err := stream.Decode()
					if err != nil {
						return err
					}
					return walk(content, childResources, child, depth+1)
				}
			}
			return nil
		})
	}
	err := r.WalkPages(ctx, func(_ int, page *Page) error {
		defer cleanup()
		content, err := page.Content()
		if err != nil {
			return err
		}
		unit := page.UserUnit
		return walk(content, page.Resources, Matrix{unit, 0, 0, unit, 0, 0}, 0)
	})
	if err != nil {
		return nil, ctx.Err()
	}
	return sizes, ctx.Err()
}
