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
	"image/jpeg"
	"io"
	"maps"
	"math"
)

// optimizeJPXImage 按已知显示尺寸缩小普通JPEG2000图片，不改遮罩及专色语义
// 入参: ctx 取消上下文, stream 原始流, options 有损配置, size 两轴像素需求
// 返回: *Stream 更小输出或原始流, error 取消或编码错误
func (r *Reader) optimizeJPXImage(ctx context.Context, stream *Stream, options CompressionOptions, size image.Point) (*Stream, error) {
	if options.Mode != CompressionLossy || size.X <= 0 || size.Y <= 0 {
		return stream, ctx.Err()
	}
	source, err := r.ReadImage(stream)
	if err != nil {
		return stream, ctx.Err()
	}
	mode, err := source.jpxMaskMode()
	if err != nil || mode != 0 || source.ImageMask || source.Mask != nil || source.SoftMask != nil || source.ColorSpace != Name("DeviceRGB") && source.ColorSpace != Name("DeviceGray") {
		return stream, ctx.Err()
	}
	scale := math.Max(float64(size.X)/float64(source.Width), float64(size.Y)/float64(source.Height))
	if scale >= 1 || scale <= 0 {
		return stream, ctx.Err()
	}
	w, h := math.Ceil(float64(source.Width)*scale), math.Ceil(float64(source.Height)*scale)
	if w*h > optimizationBufferLimit/8 {
		return stream, ctx.Err()
	}
	if int64(source.Width)*int64(source.Height) > optimizationBufferLimit/8 {
		data, _, err := source.encodedFileContext(ctx, "JPXDecode")
		if err != nil {
			return stream, ctx.Err()
		}
		code, err := jpxCodestream(data)
		if err != nil {
			return stream, ctx.Err()
		}
		reduce, err := jpxReduction(ctx, data, code, size)
		if err != nil || reduce == 0 {
			return stream, ctx.Err()
		}
	}
	decoded, err := source.DecodeImageSizeContext(ctx, size)
	if err != nil {
		return stream, ctx.Err()
	}
	base := *stream
	base.Dictionary = maps.Clone(stream.Dictionary)
	base.Dictionary["ColorSpace"] = source.ColorSpace
	base.Dictionary["BitsPerComponent"] = Integer(8)
	delete(base.Dictionary, "Decode")
	delete(base.Dictionary, "SMaskInData")
	channels := 3
	if source.ColorSpace == Name("DeviceGray") {
		channels = 1
	}
	photo, err := photographicImage(ctx, decoded)
	if err != nil {
		return nil, err
	}
	if !photo {
		candidate, err := r.encodeLosslessPixels(ctx, &base, decoded, channels)
		if err != nil {
			return nil, err
		}
		if len(candidate.Data)+100 < len(stream.Data) {
			return candidate, nil
		}
		return stream, ctx.Err()
	}
	decoded, err = jpegEncodingImage(ctx, decoded)
	if err != nil {
		return nil, err
	}
	var data bytes.Buffer
	limit := &optimizationBuffer{buffer: &data, limit: min(len(stream.Data), optimizationBufferLimit)}
	if err := jpeg.Encode(&pdfOutput{ctx: ctx, writer: limit}, decoded, &jpeg.Options{Quality: options.ImageQuality()}); err != nil {
		if limit.exceeded {
			return stream, ctx.Err()
		}
		return nil, err
	}
	encoded, err := OptimizeJPEG(ctx, data.Bytes())
	if err != nil {
		return nil, err
	}
	if len(encoded)+40 >= len(stream.Data) {
		return stream, ctx.Err()
	}
	result, err := r.reencodedStream(&base, encoded, Name("DCTDecode"))
	if err != nil {
		return nil, err
	}
	result.Dictionary["Width"] = Integer(decoded.Bounds().Dx())
	result.Dictionary["Height"] = Integer(decoded.Bounds().Dy())
	return result, ctx.Err()
}

// optimizationImageDictionary 解析图片属性的间接值，空值按标准缺省处理
// 入参: stream 原始图片流
// 返回: Dictionary 只读属性快照, error 引用解析错误
func (r *Reader) optimizationImageDictionary(stream *Stream) (Dictionary, error) {
	subtype, err := r.optimizationValue(stream.Dictionary["Subtype"])
	if err != nil {
		return nil, err
	}
	if subtype != Name("Image") {
		return stream.Dictionary, nil
	}
	dict := maps.Clone(stream.Dictionary)
	for _, key := range []Name{"Subtype", "Width", "Height", "BitsPerComponent", "ColorSpace", "ImageMask", "Decode", "Mask", "SMask", "SMaskInData", "F"} {
		value, err := r.optimizationValue(dict[key])
		if err != nil {
			return nil, err
		}
		if value == nil {
			delete(dict, key)
		} else if _, stream := value.(*Stream); !stream {
			dict[key] = value
		}
	}
	return dict, nil
}

// optimizationValue 解析压缩所需引用，不保留本次读取的图片及蒙版缓存
// 入参: value 属性值
// 返回: Object 解析值, error 引用错误
func (r *Reader) optimizationValue(value Object) (Object, error) {
	var seen map[Reference]bool
	for {
		ref, ok := value.(Reference)
		if !ok {
			return value, nil
		}
		if seen[ref] {
			return nil, fmt.Errorf("cyclic indirect reference")
		}
		if seen == nil {
			seen = make(map[Reference]bool)
		}
		seen[ref] = true
		_, cached := r.cache[ref]
		var err error
		value, err = r.Object(ref)
		if !cached {
			delete(r.cache, ref)
		}
		if err != nil {
			return nil, err
		}
	}
}

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
				data, err = decodePredictorContext(ctx, data, dict)
			}
		case Name("ASCIIHexDecode"), Name("ASCII85Decode"):
			wrapper := &Stream{Dictionary: Dictionary{"Filter": filter, "DecodeParms": params[i]}, Data: data}
			data, err = wrapper.DecodeContext(ctx)
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
				if len(stack) >= 100000 {
					return fmt.Errorf("image placement state limit exceeded")
				}
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
				for _, value := range matrix {
					if math.IsNaN(value) || math.IsInf(value, 0) {
						return fmt.Errorf("invalid image placement matrix")
					}
				}
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
				subtype, err := r.Resolve(stream.Dictionary["Subtype"])
				if err != nil {
					return err
				}
				switch subtype {
				case Name("Image"):
					w := math.Ceil(math.Nextafter(math.Hypot(matrix[0], matrix[1])*float64(dpi)/72, math.Inf(-1)))
					h := math.Ceil(math.Nextafter(math.Hypot(matrix[2], matrix[3])*float64(dpi)/72, math.Inf(-1)))
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
					value, err := r.Resolve(stream.Dictionary["Matrix"])
					if err != nil {
						return err
					}
					if value != nil {
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
					value, err = r.Resolve(stream.Dictionary["Resources"])
					if err != nil {
						return err
					}
					if value != nil {
						var ok bool
						childResources, ok = value.(Dictionary)
						if !ok {
							return fmt.Errorf("invalid image form resources")
						}
					}
					content, err := stream.DecodeContext(ctx)
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
