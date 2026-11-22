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
	"errors"
	"fmt"
	"image"
	"image/jpeg"
	"io"
	"maps"
	"math"
	"slices"
)

// imagePlacementKey 保存图片在局部坐标系中的线性变换分量
type imagePlacementKey struct {
	ref Reference
	a   float64
	b   float64
	c   float64
	d   float64
}

// cachedFormImages 缓存具有独立资源字典的表单内部图片变换及异常状态
type cachedFormImages struct {
	placements  []imagePlacementKey
	unsafe      []Reference
	opaqueError bool
}

// imageAllGray 检查图像所有像素是否均满足红绿蓝分量相等
// 入参: ctx 取消上下文, img 待检测图像
// 返回: bool 是否全灰, error 取消错误
func imageAllGray(ctx context.Context, img image.Image) (bool, error) {
	switch img.(type) {
	case *image.Gray, *image.Gray16:
		return true, ctx.Err()
	}
	b := img.Bounds()
	for y := b.Min.Y; y < b.Max.Y; y++ {
		if err := ctx.Err(); err != nil {
			return false, err
		}
		for x := b.Min.X; x < b.Max.X; x++ {
			if (x-b.Min.X)&4095 == 4095 {
				if err := ctx.Err(); err != nil {
					return false, err
				}
			}
			c := imageNRGBAAt(img, x, y)
			if c.R != c.G || c.G != c.B {
				return false, nil
			}
		}
	}
	return true, ctx.Err()
}

// pdfImagePhotographic 判断PDF有损重编时图像是否适合采用JPEG编码
// 入参: ctx 取消上下文, img 图像, gray 是否灰度目标
// 返回: bool 是否适合JPEG, error 取消错误
func pdfImagePhotographic(ctx context.Context, img image.Image, gray bool) (bool, error) {
	if !gray {
		return photographicImage(ctx, img)
	}
	b := img.Bounds()
	var seen [256]bool
	unique, count, edges := 0, 0, 0
	for y := b.Min.Y; y < b.Max.Y; y += max(1, b.Dy()/128) {
		if err := ctx.Err(); err != nil {
			return false, err
		}
		for x := b.Min.X; x < b.Max.X; x += max(1, b.Dx()/128) {
			c := imageNRGBAAt(img, x, y).R
			if !seen[c] {
				seen[c] = true
				unique++
			}
			if x+1 < b.Max.X {
				d := imageNRGBAAt(img, x+1, y).R
				if math.Abs(float64(c)-float64(d)) > 40 {
					edges++
				}
				count++
			}
		}
	}
	return unique > 32 && edges*20 < count, ctx.Err()
}

// stripInlineStreams 将对象树中直嵌的流替换为12字节占位引用，避免体积估算重复计费
// 入参: value PDF对象, depth 嵌套深度
// 返回: Object 替换后的对象, bool 是否发生变更
func stripInlineStreams(value Object, depth int) (Object, bool) {
	if depth > 64 {
		return value, false
	}
	switch v := value.(type) {
	case *Stream:
		return Reference{Number: 10000000}, true
	case Dictionary:
		var out Dictionary
		for k, item := range v {
			replaced, changed := stripInlineStreams(item, depth+1)
			if changed {
				if out == nil {
					out = maps.Clone(v)
				}
				out[k] = replaced
			}
		}
		if out != nil {
			return out, true
		}
	case Array:
		var out Array
		for i, item := range v {
			replaced, changed := stripInlineStreams(item, depth+1)
			if changed {
				if out == nil {
					out = slices.Clone(v)
				}
				out[i] = replaced
			}
		}
		if out != nil {
			return out, true
		}
	}
	return value, false
}

// pdfStreamByteSize 估算PDF流写出后的总字节数，用于比较候选流与原流体积
// 入参: ctx 取消上下文, s 流对象
// 返回: int64 字节总数
func pdfStreamByteSize(ctx context.Context, s *Stream) int64 {
	if s == nil {
		return math.MaxInt64
	}
	dict := maps.Clone(s.Dictionary)
	dict["Length"] = Integer(len(s.Data))
	if stripped, changed := stripInlineStreams(dict, 0); changed {
		dict = stripped.(Dictionary)
	}
	out := pdfOutput{ctx: ctx, writer: io.Discard}
	if err := out.object(dict, Reference{}, 0, false); err != nil {
		return int64(len(s.Data))
	}
	return out.count + int64(len(s.Data)) + 18
}

// pdfObjectsEqual 递归比较两个PDF对象的结构等价性，超深或流对象不等时返回否
// 入参: a 对象一, b 对象二, depth 递归深度
// 返回: bool 是否结构等价
func pdfObjectsEqual(a, b Object, depth int) bool {
	if depth > 32 {
		return false
	}
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	switch va := a.(type) {
	case Boolean:
		vb, ok := b.(Boolean)
		return ok && va == vb
	case Integer:
		vb, ok := b.(Integer)
		return ok && va == vb
	case Real:
		vb, ok := b.(Real)
		return ok && va == vb
	case Name:
		vb, ok := b.(Name)
		return ok && va == vb
	case Reference:
		vb, ok := b.(Reference)
		return ok && va == vb
	case String:
		vb, ok := b.(String)
		return ok && bytes.Equal(va, vb)
	case Array:
		vb, ok := b.(Array)
		if !ok || len(va) != len(vb) {
			return false
		}
		for i := range va {
			if !pdfObjectsEqual(va[i], vb[i], depth+1) {
				return false
			}
		}
		return true
	case Dictionary:
		vb, ok := b.(Dictionary)
		if !ok || len(va) != len(vb) {
			return false
		}
		for k, av := range va {
			bv, exists := vb[k]
			if !exists || !pdfObjectsEqual(av, bv, depth+1) {
				return false
			}
		}
		return true
	case *Stream:
		vb, ok := b.(*Stream)
		return ok && va == vb
	default:
		return false
	}
}

// optimizeJPXImage 在有损模式下优化JPEG2000图片，支持设备色、校准色与索引色转sRGB或灰度
// 入参: ctx 取消上下文, stream 原始流, options 有损配置, size 两轴像素需求, colorResources 可选默认颜色空间资源, ignoreSMask 是否忽略外部软遮罩
// 返回: *Stream 更小输出或原始流, error 取消或编码错误
func (r *Reader) optimizeJPXImage(ctx context.Context, stream *Stream, options CompressionOptions, size image.Point, colorResources Dictionary, ignoreSMask bool) (*Stream, error) {
	if options.Mode != CompressionLossy {
		return stream, ctx.Err()
	}
	source, err := r.readOptimizationImage(stream, colorResources)
	if err != nil || source.ImageMask {
		return stream, ctx.Err()
	}
	embeddedMask, err := source.jpxMaskMode()
	if err != nil || embeddedMask != 0 {
		return stream, ctx.Err()
	}
	if ignoreSMask {
		source.SoftMask = nil
	}
	masked, err := source.HasMask()
	if err != nil || masked {
		return stream, ctx.Err()
	}
	targetSpace, convertible := r.lossyConvertibleColorSpace(source.ColorSpace)
	if !convertible {
		return stream, ctx.Err()
	}
	downscale := false
	if size.X > 0 && size.Y > 0 && source.Width > 0 && source.Height > 0 {
		scale := math.Max(float64(size.X)/float64(source.Width), float64(size.Y)/float64(source.Height))
		if scale > 0 && scale < 1 {
			downscale = true
			w, h := math.Ceil(float64(source.Width)*scale), math.Ceil(float64(source.Height)*scale)
			if w*h > optimizationBufferLimit/8 {
				return stream, ctx.Err()
			}
		}
	}
	if !downscale {
		size = image.Point{}
	}
	if int64(source.Width)*int64(source.Height) > optimizationBufferLimit/8 {
		if !downscale {
			return stream, ctx.Err()
		}
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
	wantGray := targetSpace == Name("DeviceGray")
	if wantGray && source.ColorSpace != Name("DeviceGray") {
		gray, err := imageAllGray(ctx, decoded)
		if err != nil {
			return nil, err
		}
		wantGray = gray
	}
	candidate, err := r.encodePDFImageLossy(ctx, stream, decoded, wantGray, options, false)
	if err != nil {
		return nil, err
	}
	if candidate != nil && pdfStreamByteSize(ctx, candidate) < pdfStreamByteSize(ctx, stream) {
		return candidate, ctx.Err()
	}
	return stream, ctx.Err()
}

// readOptimizationImage 读取图像描述并在存在默认颜色空间资源时应用映射
// 入参: stream 图像流, colorResources 可选颜色空间资源字典
// 返回: *Image 图像描述, error 解析错误
func (r *Reader) readOptimizationImage(stream *Stream, colorResources Dictionary) (*Image, error) {
	if len(colorResources) != 0 {
		return r.ReadImageWithResources(stream, Dictionary{"ColorSpace": colorResources})
	}
	return r.ReadImage(stream)
}

// lossyConvertibleColorSpace 判断PDF颜色空间在有损模式下能否安全转为sRGB或DeviceGray
// 入参: space 颜色空间对象
// 返回: Name 目标设备色空间, bool 是否允许有损转换
func (r *Reader) lossyConvertibleColorSpace(space Object) (Name, bool) {
	resolved, err := r.resolveColorSpace(space)
	if err != nil || resolved == nil {
		return "", false
	}
	return r.lossyResolvedColorSpace(resolved)
}

// lossyResolvedColorSpace 判断已解析的颜色空间对象在有损模式下能否安全转为sRGB或DeviceGray
// 入参: resolved 已解析的颜色空间对象
// 返回: Name 目标设备色空间, bool 是否允许有损转换
func (r *Reader) lossyResolvedColorSpace(resolved Object) (Name, bool) {
	if arr, ok := resolved.(Array); ok && len(arr) == 1 {
		resolved = arr[0]
	}
	switch v := resolved.(type) {
	case Name:
		switch v {
		case "DeviceGray":
			return "DeviceGray", true
		case "DeviceRGB", "DeviceCMYK":
			return "DeviceRGB", true
		}
	case Array:
		if len(v) == 0 {
			return "", false
		}
		switch v[0] {
		case Name("CalGray"):
			return "DeviceGray", true
		case Name("CalRGB"):
			return "DeviceRGB", true
		case Name("ICCBased"):
			if len(v) != 2 {
				return "", false
			}
			obj, err := r.Resolve(v[1])
			if err != nil {
				return "", false
			}
			s, ok := obj.(*Stream)
			if !ok {
				return "", false
			}
			n, err := r.Resolve(s.Dictionary["N"])
			if err != nil {
				return "", false
			}
			switch n {
			case Integer(1):
				return "DeviceGray", true
			case Integer(3), Integer(4):
				return "DeviceRGB", true
			}
		case Name("Indexed"):
			if len(v) != 4 {
				return "", false
			}
			return r.lossyConvertibleColorSpace(v[1])
		}
	}
	return "", false
}

// isIdentityDecode 判断Decode数组是否为各通道0到1的恒等映射
// 入参: value Decode对象, components 分量数
// 返回: bool 是否为默认恒等映射
func (r *Reader) isIdentityDecode(value Object, components int) bool {
	if value == nil {
		return true
	}
	arr, ok := value.(Array)
	if !ok || len(arr) != components*2 {
		return false
	}
	values, err := r.numberArray(arr, components*2)
	if err != nil {
		return false
	}
	for i := 0; i < components; i++ {
		if values[i*2] != 0 || values[i*2+1] != 1 {
			return false
		}
	}
	return true
}

// isInvertedCMYKDecodeValue 判断Decode数组是否为四通道反相映射
// 入参: value Decode对象
// 返回: bool 是否为反相CMYK映射
func (r *Reader) isInvertedCMYKDecodeValue(value Object) bool {
	arr, ok := value.(Array)
	if !ok || len(arr) != 8 {
		return false
	}
	values, err := r.numberArray(arr, 8)
	if err != nil {
		return false
	}
	return values[0] == 1 && values[1] == 0 && values[2] == 1 && values[3] == 0 && values[4] == 1 && values[5] == 0 && values[6] == 1 && values[7] == 0
}

// encodePDFImageLossy 将已转换的8位目标图像编码为PDF流，原JPEG直接重编，严格保证通道数与ColorSpace一致
// 入参: ctx 取消上下文, orig 原始流, img 已缩放图像, wantGray 是否目标灰度, options 压缩配置, sourceJPEG 原流是否为JPEG编码
// 返回: *Stream 编码后的图像流或空值, error 取消或编码错误
func (r *Reader) encodePDFImageLossy(ctx context.Context, orig *Stream, img image.Image, wantGray bool, options CompressionOptions, sourceJPEG bool) (*Stream, error) {
	base := *orig
	base.Dictionary = maps.Clone(orig.Dictionary)
	base.Dictionary["BitsPerComponent"] = Integer(8)
	delete(base.Dictionary, "Decode")
	delete(base.Dictionary, "DecodeParms")
	delete(base.Dictionary, "SMaskInData")
	var encodeImg image.Image = img
	channels := 3
	if wantGray {
		grayImg, err := imageToGray8(ctx, img)
		if err != nil {
			return nil, err
		}
		encodeImg = grayImg
		channels = 1
		base.Dictionary["ColorSpace"] = Name("DeviceGray")
	} else {
		base.Dictionary["ColorSpace"] = Name("DeviceRGB")
	}
	useJPEG := sourceJPEG
	if !useJPEG {
		var err error
		useJPEG, err = pdfImagePhotographic(ctx, encodeImg, wantGray)
		if err != nil {
			return nil, err
		}
	}
	if !useJPEG {
		candidate, err := r.encodeLosslessPixels(ctx, &base, encodeImg, channels)
		if err != nil {
			return nil, err
		}
		return candidate, ctx.Err()
	}
	jpegSource := encodeImg
	if !wantGray {
		var err error
		jpegSource, err = jpegEncodingImage(ctx, encodeImg)
		if err != nil {
			return nil, err
		}
	}
	var data bytes.Buffer
	limit := &optimizationBuffer{buffer: &data, limit: optimizationBufferLimit}
	if err := jpeg.Encode(&pdfOutput{ctx: ctx, writer: limit}, jpegSource, &jpeg.Options{Quality: options.ImageQuality()}); err != nil {
		if limit.exceeded {
			return nil, ctx.Err()
		}
		return nil, err
	}
	encoded, err := OptimizeJPEG(ctx, data.Bytes())
	if err != nil {
		return nil, err
	}
	result, err := r.reencodedStream(&base, encoded, Name("DCTDecode"))
	if err != nil {
		return nil, err
	}
	if _, isGray := jpegSource.(*image.Gray); isGray {
		result.Dictionary["ColorSpace"] = Name("DeviceGray")
	} else {
		result.Dictionary["ColorSpace"] = Name("DeviceRGB")
	}
	result.Dictionary["Width"] = Integer(jpegSource.Bounds().Dx())
	result.Dictionary["Height"] = Integer(jpegSource.Bounds().Dy())
	return result, ctx.Err()
}

// isSMaskOpaque 检测软遮罩流是否全部不透明，超限或无法解码时保守返回否
// 入参: ctx 取消上下文, smask 软遮罩流
// 返回: bool 是否全不透明, error 取消错误
func (r *Reader) isSMaskOpaque(ctx context.Context, smask *Stream) (bool, error) {
	if smask == nil {
		return false, ctx.Err()
	}
	maskImg, err := r.ReadImage(smask)
	if err != nil || maskImg.ImageMask || maskImg.ColorSpace != Name("DeviceGray") || maskImg.Mask != nil || maskImg.SoftMask != nil {
		return false, ctx.Err()
	}
	if int64(maskImg.Width)*int64(maskImg.Height) > optimizationBufferLimit/4 {
		return false, ctx.Err()
	}
	decoded, err := maskImg.DecodeImageContext(ctx)
	if err != nil {
		return false, ctx.Err()
	}
	return imageMaskOpaque(ctx, decoded, false)
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
		cached := r.cache.has(ref)
		var err error
		value, err = r.Object(ref)
		if !cached {
			r.cache.delete(ref)
		}
		if err != nil {
			return nil, err
		}
	}
}

// unwrapImageStream 去除图像末级编码之外的通用包装过滤器，保留末级参数和显式加密策略
// 入参: ctx 取消上下文, stream 原始流, filters 过滤器链, params 解码参数链
// 返回: *Stream 剥壳后的内层流，不支持时为空, error 取消错误
func (r *Reader) unwrapImageStream(ctx context.Context, stream *Stream, filters, params Array) (*Stream, error) {
	data := stream.Data
	for i, filter := range filters[:len(filters)-1] {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		var err error
		dict, _ := params[i].(Dictionary)
		switch filter {
		case Name("FlateDecode"):
			input, e := zlib.NewReader(bytes.NewReader(data))
			if e != nil {
				return nil, nil
			}
			data, err = io.ReadAll(io.LimitReader(&contextInput{ctx: ctx, reader: input}, optimizationBufferLimit+1))
			input.Close()
			if err == nil && len(data) <= optimizationBufferLimit {
				data, err = restorePredictorContext(ctx, data, dict)
			}
		case Name("LZWDecode"):
			early, e := integerDefault(dict, "EarlyChange", 1)
			if e != nil {
				return nil, nil
			}
			data, _, err = decodeLZWBytesLimitContext(ctx, data, early, optimizationBufferLimit)
			if err == nil {
				data, err = restorePredictorContext(ctx, data, dict)
			}
		case Name("ASCIIHexDecode"):
			_, size, e := asciiHexSize(ctx, data, nil)
			if e != nil || size > optimizationBufferLimit {
				return nil, ctx.Err()
			}
			data, err = decodeASCIIHexContext(ctx, data, nil)
		case Name("ASCII85Decode"):
			_, size, e := ascii85Size(ctx, data, nil)
			if e != nil || size > optimizationBufferLimit {
				return nil, ctx.Err()
			}
			data, err = decodeASCII85Context(ctx, data, nil)
		case Name("RunLengthDecode"):
			size, e := runLengthSize(ctx, data, nil)
			if e != nil || size > optimizationBufferLimit {
				return nil, ctx.Err()
			}
			data, err = decodeRunLength(ctx, data, nil)
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
	inner.Dictionary["Filter"] = filters[len(filters)-1]
	lastParam := params[len(params)-1]
	if lastParam != nil {
		inner.Dictionary["DecodeParms"] = lastParam
	} else {
		delete(inner.Dictionary, "DecodeParms")
	}
	if err := r.applyExplicitCryptFilter(stream, &inner); err != nil {
		return nil, nil
	}
	return &inner, ctx.Err()
}

// imageOutputSizes 汇总页面、表单、注释外观及平铺图案中的图片最大显示尺寸，Type3字形图仅禁降采样
// 入参: ctx 取消上下文, dpi 分辨率上限
// 返回: map[Reference]image.Point 图片像素上限, error 取消错误
func (r *Reader) imageOutputSizes(ctx context.Context, dpi int) (map[Reference]image.Point, error) {
	if dpi == 0 {
		return nil, nil
	}
	sizes := make(map[Reference]image.Point)
	unsafe := make(map[Reference]bool)
	cached := r.cache.snapshot()
	cleanup := func() {
		r.cache.prune(cached)
	}
	defer cleanup()
	formCache := make(map[Reference]*cachedFormImages)
	formActive := make(map[Reference]bool)
	patternActive := make(map[Reference]bool)
	resolveObjectRef := func(value Object) (Object, Reference, error) {
		var ref Reference
		for n := 0; ; n++ {
			next, ok := value.(Reference)
			if !ok {
				return value, ref, nil
			}
			if n > 32 {
				return nil, Reference{}, fmt.Errorf("cyclic image reference")
			}
			ref = next
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
	var markResourcesUnsafe func(Dictionary, map[Reference]bool, map[Reference]bool, int) bool
	markResourcesUnsafe = func(res Dictionary, target map[Reference]bool, visited map[Reference]bool, depth int) bool {
		if depth > 32 {
			return false
		}
		if res == nil {
			return true
		}
		if res["XObject"] != nil {
			val, err := r.Resolve(res["XObject"])
			if err != nil {
				return false
			}
			if val != nil {
				objects, ok := val.(Dictionary)
				if !ok {
					return false
				}
				for _, entry := range objects {
					obj, ref, err := resolveObjectRef(entry)
					if err != nil {
						return false
					}
					stream, ok := obj.(*Stream)
					if !ok {
						continue
					}
					subtype, err := r.Resolve(stream.Dictionary["Subtype"])
					if err != nil {
						return false
					}
					switch subtype {
					case Name("Image"):
						if ref.Number > 0 {
							target[ref] = true
						}
					case Name("Form"):
						if ref.Number > 0 && visited[ref] {
							continue
						}
						if ref.Number > 0 {
							visited[ref] = true
						}
						if stream.Dictionary["Resources"] != nil {
							childVal, err := r.Resolve(stream.Dictionary["Resources"])
							if err != nil {
								return false
							}
							if childVal != nil {
								childDict, ok := childVal.(Dictionary)
								if !ok {
									return false
								}
								if !markResourcesUnsafe(childDict, target, visited, depth+1) {
									return false
								}
							}
						}
					}
				}
			}
		}
		if res["Pattern"] != nil {
			val, err := r.Resolve(res["Pattern"])
			if err != nil {
				return false
			}
			if patterns, ok := val.(Dictionary); ok {
				for _, entry := range patterns {
					obj, ref, err := resolveObjectRef(entry)
					if err != nil {
						return false
					}
					stream, ok := obj.(*Stream)
					if !ok {
						continue
					}
					if ref.Number > 0 && visited[ref] {
						continue
					}
					if ref.Number > 0 {
						visited[ref] = true
					}
					if stream.Dictionary["Resources"] != nil {
						childVal, err := r.Resolve(stream.Dictionary["Resources"])
						if err != nil {
							return false
						}
						if childDict, ok := childVal.(Dictionary); ok {
							if !markResourcesUnsafe(childDict, target, visited, depth+1) {
								return false
							}
						}
					}
				}
			}
		}
		if res["Font"] != nil {
			val, err := r.Resolve(res["Font"])
			if err != nil {
				return false
			}
			if fonts, ok := val.(Dictionary); ok {
				for _, entry := range fonts {
					obj, ref, err := resolveObjectRef(entry)
					if err != nil {
						return false
					}
					dict, ok := obj.(Dictionary)
					if !ok {
						continue
					}
					subtype, err := r.Resolve(dict["Subtype"])
					if err != nil || subtype != Name("Type3") {
						continue
					}
					if ref.Number > 0 && visited[ref] {
						continue
					}
					if ref.Number > 0 {
						visited[ref] = true
					}
					if dict["Resources"] != nil {
						childVal, err := r.Resolve(dict["Resources"])
						if err != nil {
							return false
						}
						if childDict, ok := childVal.(Dictionary); ok {
							if !markResourcesUnsafe(childDict, target, visited, depth+1) {
								return false
							}
						}
					}
				}
			}
		}
		return true
	}
	markType3Fonts := func(res Dictionary, target map[Reference]bool) {
		if res == nil || res["Font"] == nil {
			return
		}
		val, err := r.Resolve(res["Font"])
		if err != nil {
			return
		}
		fonts, ok := val.(Dictionary)
		if !ok {
			return
		}
		for _, entry := range fonts {
			obj, ref, err := resolveObjectRef(entry)
			if err != nil {
				continue
			}
			dict, ok := obj.(Dictionary)
			if !ok {
				continue
			}
			subtype, err := r.Resolve(dict["Subtype"])
			if err != nil || subtype != Name("Type3") {
				continue
			}
			if dict["Resources"] != nil {
				childVal, err := r.Resolve(dict["Resources"])
				if err == nil {
					if childDict, ok := childVal.(Dictionary); ok {
						markResourcesUnsafe(childDict, target, map[Reference]bool{ref: true}, 0)
					}
				}
				continue
			}
			procsVal, err := r.Resolve(dict["CharProcs"])
			if err != nil {
				continue
			}
			procs, ok := procsVal.(Dictionary)
			if !ok {
				continue
			}
			xobjVal, _ := r.Resolve(res["XObject"])
			xobjects, _ := xobjVal.(Dictionary)
			for _, procEntry := range procs {
				procObj, _, err := resolveObjectRef(procEntry)
				if err != nil {
					continue
				}
				procStream, ok := procObj.(*Stream)
				if !ok {
					continue
				}
				data, err := r.DecodeStreamContext(ctx, procStream)
				if err != nil {
					markResourcesUnsafe(res, target, map[Reference]bool{ref: true}, 0)
					break
				}
				_ = walkOperationsBorrowed(ctx, data, func(op Operation) error {
					if op.Operator == "Do" && len(op.Operands) == 1 {
						if name, ok := op.Operands[0].(Name); ok && xobjects != nil {
							xobj, xref, err := resolveObjectRef(xobjects[name])
							if err == nil {
								if s, ok := xobj.(*Stream); ok {
									sub, _ := r.Resolve(s.Dictionary["Subtype"])
									if sub == Name("Image") && xref.Number > 0 {
										target[xref] = true
									} else if sub == Name("Form") {
										if fResVal, err := r.Resolve(s.Dictionary["Resources"]); err == nil {
											if fRes, ok := fResVal.(Dictionary); ok {
												markResourcesUnsafe(fRes, target, map[Reference]bool{xref: true}, 0)
											}
										}
									}
								}
							}
						}
					}
					return nil
				})
			}
		}
	}
	recordPlacement := func(ref Reference, matrix Matrix, collector map[imagePlacementKey]struct{}, localUnsafe map[Reference]bool) {
		if ref.Number <= 0 {
			return
		}
		for _, value := range matrix[:4] {
			if math.IsNaN(value) || math.IsInf(value, 0) {
				localUnsafe[ref] = true
				return
			}
		}
		if collector != nil {
			collector[imagePlacementKey{ref: ref, a: matrix[0], b: matrix[1], c: matrix[2], d: matrix[3]}] = struct{}{}
			return
		}
		w := math.Ceil(math.Nextafter(math.Hypot(matrix[0], matrix[1])*float64(dpi)/72, math.Inf(-1)))
		h := math.Ceil(math.Nextafter(math.Hypot(matrix[2], matrix[3])*float64(dpi)/72, math.Inf(-1)))
		if math.IsNaN(w) || math.IsNaN(h) || math.IsInf(w, 0) || math.IsInf(h, 0) || w > 1<<30 || h > 1<<30 {
			localUnsafe[ref] = true
			return
		}
		old := sizes[ref]
		sizes[ref] = image.Pt(max(old.X, max(1, int(w))), max(old.Y, max(1, int(h))))
	}
	applyCachedForm := func(entry *cachedFormImages, parentMatrix Matrix, collector map[imagePlacementKey]struct{}, localUnsafe map[Reference]bool) bool {
		if entry.opaqueError {
			return false
		}
		for _, ref := range entry.unsafe {
			localUnsafe[ref] = true
		}
		for _, item := range entry.placements {
			combined := parentMatrix.Mul(Matrix{item.a, item.b, item.c, item.d, 0, 0})
			recordPlacement(item.ref, combined, collector, localUnsafe)
		}
		return true
	}
	errUnsafe := errors.New("global unsafe")
	var walk func([]byte, Dictionary, Matrix, int, *int, map[imagePlacementKey]struct{}, map[Reference]bool) error
	walkPatterns := func(resources Dictionary, initialMatrix Matrix, depth int, budget *int, collector map[imagePlacementKey]struct{}, localUnsafe map[Reference]bool) error {
		if resources == nil || resources["Pattern"] == nil || depth > 32 {
			return nil
		}
		val, err := r.Resolve(resources["Pattern"])
		if err != nil {
			return nil
		}
		patterns, ok := val.(Dictionary)
		if !ok {
			return nil
		}
		for _, entry := range patterns {
			if err := ctx.Err(); err != nil {
				return err
			}
			obj, ref, err := resolveObjectRef(entry)
			if err != nil {
				continue
			}
			patStream, ok := obj.(*Stream)
			if !ok {
				continue
			}
			kind, err := r.Resolve(patStream.Dictionary["PatternType"])
			if err != nil || kind != Integer(1) {
				continue
			}
			if ref.Number > 0 && patternActive[ref] {
				continue
			}
			patRes := resources
			if patStream.Dictionary["Resources"] != nil {
				resVal, err := r.Resolve(patStream.Dictionary["Resources"])
				if err != nil {
					if ctx.Err() != nil {
						return ctx.Err()
					}
					return errUnsafe
				}
				if resDict, ok := resVal.(Dictionary); ok {
					patRes = resDict
				} else if resVal != nil {
					return errUnsafe
				}
			}
			patMatrix := Identity()
			if patStream.Dictionary["Matrix"] != nil {
				mVal, err := r.Resolve(patStream.Dictionary["Matrix"])
				if err != nil {
					if ctx.Err() != nil {
						return ctx.Err()
					}
					if !markResourcesUnsafe(patRes, localUnsafe, map[Reference]bool{ref: true}, 0) {
						return errUnsafe
					}
					continue
				}
				if mVal != nil {
					values, err := r.numberArray(mVal, 6)
					if err != nil {
						if !markResourcesUnsafe(patRes, localUnsafe, map[Reference]bool{ref: true}, 0) {
							return errUnsafe
						}
						continue
					}
					patMatrix = Matrix(values)
				}
			}
			if ref.Number > 0 {
				patternActive[ref] = true
			}
			content, decodeErr := r.DecodeStreamContext(ctx, patStream)
			walkErr := decodeErr
			if walkErr == nil {
				walkErr = walk(content, patRes, initialMatrix.Mul(patMatrix), depth+1, budget, collector, localUnsafe)
			}
			if ref.Number > 0 {
				delete(patternActive, ref)
			}
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if walkErr != nil {
				if errors.Is(walkErr, errUnsafe) || !markResourcesUnsafe(patRes, localUnsafe, map[Reference]bool{ref: true}, 0) {
					return errUnsafe
				}
			}
		}
		return ctx.Err()
	}
	walk = func(data []byte, resources Dictionary, matrix Matrix, depth int, budget *int, collector map[imagePlacementKey]struct{}, localUnsafe map[Reference]bool) error {
		if depth > 32 {
			return fmt.Errorf("image placement recursion limit exceeded")
		}
		initialMatrix := matrix
		var stack []Matrix
		err := walkOperationsBorrowed(ctx, data, func(op Operation) error {
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
				*budget--
				if *budget < 0 {
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
				obj, ref, err := resolveObjectRef(objects[name])
				if err != nil {
					return err
				}
				stream, ok := obj.(*Stream)
				if !ok {
					return fmt.Errorf("invalid image resource")
				}
				subtype, err := r.Resolve(stream.Dictionary["Subtype"])
				if err != nil {
					return err
				}
				switch subtype {
				case Name("Image"):
					recordPlacement(ref, matrix, collector, localUnsafe)
				case Name("Form"):
					formMatrix := Matrix{1, 0, 0, 1, 0, 0}
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
						formMatrix = Matrix(values)
					}
					resValue, err := r.Resolve(stream.Dictionary["Resources"])
					if err != nil {
						if ref.Number > 0 {
							formCache[ref] = &cachedFormImages{opaqueError: true}
						}
						return err
					}
					if resValue != nil && ref.Number > 0 {
						if entry, ok := formCache[ref]; ok {
							if !applyCachedForm(entry, matrix, collector, localUnsafe) {
								return fmt.Errorf("cached form analysis failed")
							}
							return nil
						}
						if formActive[ref] {
							return fmt.Errorf("cyclic image form reference")
						}
						childResources, ok := resValue.(Dictionary)
						if !ok {
							formCache[ref] = &cachedFormImages{opaqueError: true}
							return fmt.Errorf("invalid image form resources")
						}
						formActive[ref] = true
						formBudget := 100000
						subCollector := make(map[imagePlacementKey]struct{})
						subUnsafe := make(map[Reference]bool)
						content, decodeErr := r.DecodeStreamContext(ctx, stream)
						walkErr := decodeErr
						if walkErr == nil {
							walkErr = walk(content, childResources, formMatrix, depth+1, &formBudget, subCollector, subUnsafe)
						}
						delete(formActive, ref)
						if ctx.Err() != nil {
							return ctx.Err()
						}
						var entry *cachedFormImages
						if walkErr != nil || len(subCollector) > 10000 {
							entry = &cachedFormImages{}
							if !markResourcesUnsafe(childResources, subUnsafe, map[Reference]bool{ref: true}, 0) {
								entry.opaqueError = true
							} else {
								entry.unsafe = slices.Collect(maps.Keys(subUnsafe))
							}
						} else {
							entry = &cachedFormImages{
								placements: slices.Collect(maps.Keys(subCollector)),
								unsafe:     slices.Collect(maps.Keys(subUnsafe)),
							}
						}
						formCache[ref] = entry
						if !applyCachedForm(entry, matrix, collector, localUnsafe) {
							return fmt.Errorf("form resource analysis failed")
						}
						return nil
					}
					childResources := resources
					if resValue != nil {
						dict, ok := resValue.(Dictionary)
						if !ok {
							return fmt.Errorf("invalid image form resources")
						}
						childResources = dict
					}
					child := matrix.Mul(formMatrix)
					content, err := r.DecodeStreamContext(ctx, stream)
					if err != nil {
						return err
					}
					return walk(content, childResources, child, depth+1, budget, collector, localUnsafe)
				}
			}
			return nil
		})
		if err != nil {
			return err
		}
		markType3Fonts(resources, localUnsafe)
		return walkPatterns(resources, initialMatrix, depth, budget, collector, localUnsafe)
	}
	walkAnnotationStream := func(page *Page, unit float64, rect Rectangle, rectValid bool, apStream *Stream) error {
		apRes := page.Resources
		if apStream.Dictionary["Resources"] != nil {
			resVal, err := r.Resolve(apStream.Dictionary["Resources"])
			if err != nil {
				if ctx.Err() != nil {
					return ctx.Err()
				}
				return errUnsafe
			}
			if resVal != nil {
				dict, ok := resVal.(Dictionary)
				if !ok {
					return errUnsafe
				}
				apRes = dict
			}
		}
		failSafe := func() error {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if !markResourcesUnsafe(apRes, unsafe, map[Reference]bool{}, 0) {
				return errUnsafe
			}
			return nil
		}
		if !rectValid || rect.XMin == rect.XMax || rect.YMin == rect.YMax {
			return failSafe()
		}
		box, err := r.rectangle(apStream.Dictionary["BBox"])
		if err != nil {
			return failSafe()
		}
		formMatrix := Identity()
		if apStream.Dictionary["Matrix"] != nil {
			mVal, err := r.Resolve(apStream.Dictionary["Matrix"])
			if err != nil {
				return failSafe()
			}
			if mVal != nil {
				values, err := r.numberArray(mVal, 6)
				if err != nil {
					return failSafe()
				}
				formMatrix = Matrix(values)
			}
		}
		minX, minY, maxX, maxY := math.Inf(1), math.Inf(1), math.Inf(-1), math.Inf(-1)
		for _, pt := range []Point{{box.XMin, box.YMin}, {box.XMax, box.YMin}, {box.XMax, box.YMax}, {box.XMin, box.YMax}} {
			pt = formMatrix.Apply(pt)
			minX, minY = math.Min(minX, pt.X), math.Min(minY, pt.Y)
			maxX, maxY = math.Max(maxX, pt.X), math.Max(maxY, pt.Y)
		}
		if math.IsInf(minX, 0) || math.IsInf(minY, 0) || math.IsInf(maxX, 0) || math.IsInf(maxY, 0) || math.IsNaN(minX) || math.IsNaN(minY) || math.IsNaN(maxX) || math.IsNaN(maxY) || minX == maxX || minY == maxY {
			return failSafe()
		}
		scaleX := (rect.XMax - rect.XMin) / (maxX - minX)
		scaleY := (rect.YMax - rect.YMin) / (maxY - minY)
		apMatrix := Matrix{unit * scaleX, 0, 0, unit * scaleY, 0, 0}.Mul(formMatrix)
		content, err := r.DecodeStreamContext(ctx, apStream)
		if err != nil {
			return failSafe()
		}
		apBudget := 100000
		if err := walk(content, apRes, apMatrix, 0, &apBudget, nil, unsafe); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if errors.Is(err, errUnsafe) {
				return errUnsafe
			}
			return failSafe()
		}
		return ctx.Err()
	}
	walkPageAnnotations := func(page *Page, unit float64) error {
		if page.Dictionary["Annots"] == nil {
			return nil
		}
		val, err := r.Resolve(page.Dictionary["Annots"])
		if err != nil {
			return ctx.Err()
		}
		annots, ok := val.(Array)
		if !ok {
			return nil
		}
		for _, item := range annots {
			if err := ctx.Err(); err != nil {
				return err
			}
			annotObj, _, err := resolveObjectRef(item)
			if err != nil {
				continue
			}
			annotDict, ok := annotObj.(Dictionary)
			if !ok || annotDict["AP"] == nil {
				continue
			}
			apVal, err := r.Resolve(annotDict["AP"])
			if err != nil {
				continue
			}
			apDict, ok := apVal.(Dictionary)
			if !ok {
				continue
			}
			rect, rectErr := r.rectangle(annotDict["Rect"])
			rectValid := rectErr == nil
			for _, apKey := range []Name{"N", "R", "D"} {
				entryVal, err := r.Resolve(apDict[apKey])
				if err != nil || entryVal == nil {
					continue
				}
				switch v := entryVal.(type) {
				case *Stream:
					if err := walkAnnotationStream(page, unit, rect, rectValid, v); err != nil {
						return err
					}
				case Dictionary:
					for _, stateEntry := range v {
						stateObj, _, err := resolveObjectRef(stateEntry)
						if err != nil {
							continue
						}
						if stateStream, ok := stateObj.(*Stream); ok {
							if err := walkAnnotationStream(page, unit, rect, rectValid, stateStream); err != nil {
								return err
							}
						}
					}
				}
			}
		}
		return ctx.Err()
	}
	err := r.WalkPages(ctx, func(_ int, page *Page) error {
		defer cleanup()
		unit := page.UserUnit
		if err := walkPageAnnotations(page, unit); err != nil {
			return err
		}
		content, err := page.Content()
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if !markResourcesUnsafe(page.Resources, unsafe, map[Reference]bool{}, 0) {
				return errUnsafe
			}
			return nil
		}
		pageBudget := 100000
		if err := walk(content, page.Resources, Matrix{unit, 0, 0, unit, 0, 0}, 0, &pageBudget, nil, unsafe); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if errors.Is(err, errUnsafe) || !markResourcesUnsafe(page.Resources, unsafe, map[Reference]bool{}, 0) {
				return errUnsafe
			}
			return nil
		}
		return nil
	})
	if err != nil {
		return nil, ctx.Err()
	}
	for ref := range unsafe {
		delete(sizes, ref)
	}
	return sizes, ctx.Err()
}

// imageDefaultColorResources 在文档存在Default设备色资源时映射各图像引用的默认色空间，多页冲突时拒绝转换
// 入参: ctx 取消上下文, refs 可达对象, metadata 对象元数据快照, aliases 对象别名映射
// 返回: map[Reference]Dictionary 图像到默认色空间资源的映射, map[Reference]bool 存在默认色空间冲突的图像集合, error 取消错误
func (r *Reader) imageDefaultColorResources(ctx context.Context, refs []Reference, metadata map[Reference]Object, aliases map[Reference]Reference) (map[Reference]Dictionary, map[Reference]bool, error) {
	hasDefault := false
	var scanDict func(Object, int)
	scanDict = func(value Object, depth int) {
		if hasDefault || depth > 32 {
			return
		}
		switch v := value.(type) {
		case Dictionary:
			if v["DefaultCMYK"] != nil || v["DefaultRGB"] != nil || v["DefaultGray"] != nil {
				hasDefault = true
				return
			}
			for _, item := range v {
				scanDict(item, depth+1)
				if hasDefault {
					return
				}
			}
		case Array:
			for _, item := range v {
				scanDict(item, depth+1)
				if hasDefault {
					return
				}
			}
		}
	}
	for _, ref := range refs {
		if err := ctx.Err(); err != nil {
			return nil, nil, err
		}
		scanDict(metadata[ref], 0)
		if hasDefault {
			break
		}
	}
	if !hasDefault {
		return nil, nil, ctx.Err()
	}
	result := make(map[Reference]Dictionary)
	conflict := make(map[Reference]bool)
	cached := r.cache.snapshot()
	defer r.cache.prune(cached)
	resolveObjectRef := func(value Object) (Object, Reference, error) {
		var ref Reference
		for n := 0; ; n++ {
			next, ok := value.(Reference)
			if !ok {
				return value, ref, nil
			}
			if n > 32 {
				return nil, Reference{}, fmt.Errorf("cyclic color resource reference")
			}
			ref = next
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
	extractDefaults := func(res Dictionary) Dictionary {
		if res == nil || res["ColorSpace"] == nil {
			return nil
		}
		val, err := r.Resolve(res["ColorSpace"])
		if err != nil {
			return nil
		}
		cs, ok := val.(Dictionary)
		if !ok {
			return nil
		}
		var out Dictionary
		for _, key := range []Name{"DefaultGray", "DefaultRGB", "DefaultCMYK"} {
			if cs[key] != nil {
				if out == nil {
					out = Dictionary{}
				}
				out[key] = cs[key]
			}
		}
		return out
	}
	record := func(ref Reference, defs Dictionary) {
		if canonical, aliased := aliases[ref]; aliased {
			ref = canonical
		}
		if ref.Number <= 0 || conflict[ref] {
			return
		}
		prev, seen := result[ref]
		if !seen {
			result[ref] = defs
			return
		}
		if !pdfObjectsEqual(prev, defs, 0) {
			conflict[ref] = true
			delete(result, ref)
		}
	}
	var walkRes func(Dictionary, map[Reference]bool, int)
	walkRes = func(res Dictionary, visited map[Reference]bool, depth int) {
		if res == nil || depth > 32 || ctx.Err() != nil {
			return
		}
		defs := extractDefaults(res)
		if res["XObject"] != nil {
			val, err := r.Resolve(res["XObject"])
			if err == nil {
				if xobjs, ok := val.(Dictionary); ok {
					for _, entry := range xobjs {
						obj, ref, err := resolveObjectRef(entry)
						if err != nil {
							continue
						}
						stream, ok := obj.(*Stream)
						if !ok {
							continue
						}
						sub, _ := r.Resolve(stream.Dictionary["Subtype"])
						switch sub {
						case Name("Image"):
							record(ref, defs)
						case Name("Form"):
							if canonical, aliased := aliases[ref]; aliased {
								ref = canonical
							}
							if ref.Number > 0 && visited[ref] {
								continue
							}
							if ref.Number > 0 {
								visited[ref] = true
							}
							childRes := res
							if stream.Dictionary["Resources"] != nil {
								if rVal, err := r.Resolve(stream.Dictionary["Resources"]); err == nil {
									if rDict, ok := rVal.(Dictionary); ok {
										childRes = rDict
									}
								}
							}
							walkRes(childRes, visited, depth+1)
						}
					}
				}
			}
		}
		if res["Pattern"] != nil {
			val, err := r.Resolve(res["Pattern"])
			if err == nil {
				if pats, ok := val.(Dictionary); ok {
					for _, entry := range pats {
						obj, ref, err := resolveObjectRef(entry)
						if err != nil {
							continue
						}
						stream, ok := obj.(*Stream)
						if !ok {
							continue
						}
						if canonical, aliased := aliases[ref]; aliased {
							ref = canonical
						}
						if ref.Number > 0 && visited[ref] {
							continue
						}
						if ref.Number > 0 {
							visited[ref] = true
						}
						childRes := res
						if stream.Dictionary["Resources"] != nil {
							if rVal, err := r.Resolve(stream.Dictionary["Resources"]); err == nil {
								if rDict, ok := rVal.(Dictionary); ok {
									childRes = rDict
								}
							}
						}
						walkRes(childRes, visited, depth+1)
					}
				}
			}
		}
	}
	walkAPStream := func(pageRes Dictionary, apStream *Stream) {
		apRes := pageRes
		if apStream.Dictionary["Resources"] != nil {
			if rVal, err := r.Resolve(apStream.Dictionary["Resources"]); err == nil {
				if rDict, ok := rVal.(Dictionary); ok {
					apRes = rDict
				}
			}
		}
		walkRes(apRes, map[Reference]bool{}, 0)
	}
	_ = r.WalkPages(ctx, func(_ int, page *Page) error {
		defer r.cache.prune(cached)
		walkRes(page.Resources, map[Reference]bool{}, 0)
		if page.Dictionary["Annots"] != nil {
			if val, err := r.Resolve(page.Dictionary["Annots"]); err == nil {
				if annots, ok := val.(Array); ok {
					for _, item := range annots {
						if err := ctx.Err(); err != nil {
							return err
						}
						annotObj, _, err := resolveObjectRef(item)
						if err != nil {
							continue
						}
						annotDict, ok := annotObj.(Dictionary)
						if !ok || annotDict["AP"] == nil {
							continue
						}
						apVal, err := r.Resolve(annotDict["AP"])
						if err != nil {
							continue
						}
						apDict, ok := apVal.(Dictionary)
						if !ok {
							continue
						}
						for _, apKey := range []Name{"N", "R", "D"} {
							entryVal, err := r.Resolve(apDict[apKey])
							if err != nil || entryVal == nil {
								continue
							}
							switch v := entryVal.(type) {
							case *Stream:
								walkAPStream(page.Resources, v)
							case Dictionary:
								for _, stateEntry := range v {
									stateObj, _, err := resolveObjectRef(stateEntry)
									if err != nil {
										continue
									}
									if stateStream, ok := stateObj.(*Stream); ok {
										walkAPStream(page.Resources, stateStream)
									}
								}
							}
						}
					}
				}
			}
		}
		return ctx.Err()
	})
	return result, conflict, ctx.Err()
}

// optimizePDFDecodedImage 解码通用色彩空间图片并执行缩放与有损重编，更小时返回候选流
// 入参: ctx 取消上下文, orig 原始流, baseline 体积比较基准流, w 宽度, h 高度, space 源颜色空间, targetSpace 目标设备色空间, options 压缩配置, size 像素需求, colorResources 可选默认颜色空间资源, ignoreSMask 是否忽略外部软遮罩, sourceJPEG 原流是否为JPEG编码
// 返回: *Stream 更小的候选流或空值, error 取消或编码错误
func (r *Reader) optimizePDFDecodedImage(ctx context.Context, orig, baseline *Stream, w, h Integer, space Object, targetSpace Name, options CompressionOptions, size image.Point, colorResources Dictionary, ignoreSMask, sourceJPEG bool) (*Stream, error) {
	if w <= 0 || h <= 0 || int64(w)*int64(h) > optimizationBufferLimit/8 {
		return nil, ctx.Err()
	}
	source, err := r.readOptimizationImage(orig, colorResources)
	if err != nil {
		return nil, ctx.Err()
	}
	if ignoreSMask {
		source.SoftMask = nil
	}
	decoded, err := source.DecodeImageContext(ctx)
	if err != nil {
		return nil, ctx.Err()
	}
	resized, err := ResizeImage(ctx, decoded, size)
	if err != nil {
		return nil, err
	}
	wantGray := targetSpace == Name("DeviceGray")
	if wantGray && space != Name("DeviceGray") {
		gray, err := imageAllGray(ctx, resized)
		if err != nil {
			return nil, err
		}
		wantGray = gray
	}
	cand, err := r.encodePDFImageLossy(ctx, orig, resized, wantGray, options, sourceJPEG)
	if err != nil {
		return nil, err
	}
	if cand != nil && pdfStreamByteSize(ctx, cand) < pdfStreamByteSize(ctx, baseline) {
		return cand, ctx.Err()
	}
	return nil, ctx.Err()
}

// optimizePDFDCTImage 优化PDF内的DCTDecode流，剥离Exif与ICC并在有损模式下支持CMYK/ICC/Cal/Indexed转换与降采样
// 入参: ctx 取消上下文, orig 原始流, dict 已解析属性字典, dctParams DCT解码参数, options 压缩配置, size 像素需求, colorResources 可选默认颜色空间资源, ignoreSMask 是否忽略外部软遮罩
// 返回: *Stream 优化后的流或原始流, error 取消或编码错误
func (r *Reader) optimizePDFDCTImage(ctx context.Context, orig *Stream, dict, dctParams Dictionary, options CompressionOptions, size image.Point, colorResources Dictionary, ignoreSMask bool) (*Stream, error) {
	raw := orig.Data
	stripped := raw
	if frame, _, _, err := jpegColorInfoContext(ctx, raw); err == nil && frame.displayMetadata {
		if cleaned, cleanErr := jpegDisplayFileContext(ctx, raw); cleanErr == nil {
			stripped = cleaned
		}
	}
	bestData := stripped
	if opt, optErr := OptimizeJPEG(ctx, stripped); optErr == nil && len(opt) < len(bestData) {
		bestData = opt
	} else if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	losslessResult := orig
	if len(bestData) < len(orig.Data) {
		copyStream := *orig
		copyStream.Dictionary = maps.Clone(orig.Dictionary)
		copyStream.Data = bestData
		losslessResult = &copyStream
	}
	if options.Mode != CompressionLossy || dict["Subtype"] != Name("Image") || dict["Mask"] != nil || dict["ImageMask"] == Boolean(true) || !ignoreSMask && dict["SMask"] != nil {
		return losslessResult, ctx.Err()
	}
	space, err := r.resolveColorSpace(dict["ColorSpace"])
	if err != nil {
		return losslessResult, ctx.Err()
	}
	if arr, ok := space.(Array); ok && len(arr) == 1 {
		space = arr[0]
	}
	targetSpace, convertible := r.lossyResolvedColorSpace(space)
	if !convertible {
		return losslessResult, ctx.Err()
	}
	hasDefaultOverride := space == Name("DeviceRGB") && colorResources["DefaultRGB"] != nil || space == Name("DeviceGray") && colorResources["DefaultGray"] != nil || space == Name("DeviceCMYK") && colorResources["DefaultCMYK"] != nil
	w, _ := dict["Width"].(Integer)
	h, _ := dict["Height"].(Integer)
	config, configErr := jpeg.DecodeConfig(bytes.NewReader(bestData))
	if configErr != nil || Integer(config.Width) != w || Integer(config.Height) != h {
		size = image.Point{}
	}
	expectedComp := 3
	if space == Name("DeviceGray") {
		expectedComp = 1
	}
	if !hasDefaultOverride && (space == Name("DeviceRGB") || space == Name("DeviceGray")) && r.isIdentityDecode(dict["Decode"], expectedComp) && dctParams["ColorTransform"] == nil && configErr == nil {
		frame, adobe, tagged, frameErr := jpegColorInfoContext(ctx, bestData)
		if frameErr == nil && frame.components == expectedComp && !(frame.components == 3 && tagged && adobe == 0) {
			downscale := size.X > 0 && size.Y > 0 && (size.X < config.Width || size.Y < config.Height)
			if !downscale {
				sufficient, err := jpegQualitySufficient(ctx, bestData, options.ImageQuality())
				if err != nil {
					return nil, err
				}
				if sufficient {
					return losslessResult, ctx.Err()
				}
			}
			if int64(config.Width)*int64(config.Height) > optimizationBufferLimit/4 {
				return losslessResult, ctx.Err()
			}
			img, err := jpeg.Decode(&contextInput{ctx: ctx, reader: bytes.NewReader(bestData)})
			if err != nil {
				return losslessResult, ctx.Err()
			}
			img, err = ResizeImage(ctx, img, size)
			if err != nil {
				return nil, err
			}
			cand, err := r.encodePDFImageLossy(ctx, orig, img, space == Name("DeviceGray"), options, true)
			if err != nil {
				return nil, err
			}
			if cand != nil && pdfStreamByteSize(ctx, cand) < pdfStreamByteSize(ctx, losslessResult) {
				return cand, ctx.Err()
			}
			return losslessResult, ctx.Err()
		}
	}
	if !hasDefaultOverride && space == Name("DeviceCMYK") {
		invertedCMYK := r.isInvertedCMYKDecodeValue(dict["Decode"])
		if invertedCMYK || r.isIdentityDecode(dict["Decode"], 4) {
			if source, err := r.ReadImage(orig); err == nil {
				jpegData, frame, transform, tagged, err := source.jpegReadTransformContext(ctx, bestData, dctParams)
				if err == nil && frame.components == 4 && int64(frame.width)*int64(frame.height) <= optimizationBufferLimit/4 {
					var reader io.Reader = bytes.NewReader(jpegData)
					if !tagged {
						marker := jpegAdobeMarker(transform)
						reader = io.MultiReader(bytes.NewReader(jpegData[:2]), bytes.NewReader(marker), bytes.NewReader(jpegData[2:]))
					}
					if decoded, err := jpeg.Decode(&contextInput{ctx: ctx, reader: reader}); err == nil {
						if cmykImg, ok := decoded.(*image.CMYK); ok {
							if !invertedCMYK {
								for n := range cmykImg.Pix {
									if n&4095 == 0 {
										if err := ctx.Err(); err != nil {
											return nil, err
										}
									}
									cmykImg.Pix[n] = 255 - cmykImg.Pix[n]
								}
							}
							rgba, ok, err := convertCMYKJPEGToRGBA(ctx, cmykImg, nil)
							if err != nil {
								return nil, err
							}
							if ok {
								resized, err := ResizeImage(ctx, rgba, size)
								if err != nil {
									return nil, err
								}
								cand, err := r.encodePDFImageLossy(ctx, orig, resized, false, options, true)
								if err != nil {
									return nil, err
								}
								if cand != nil && pdfStreamByteSize(ctx, cand) < pdfStreamByteSize(ctx, losslessResult) {
									return cand, ctx.Err()
								}
								return losslessResult, ctx.Err()
							}
						}
					}
				}
			}
		}
	}
	cand, err := r.optimizePDFDecodedImage(ctx, orig, losslessResult, w, h, space, targetSpace, options, size, colorResources, ignoreSMask, true)
	if err != nil {
		return nil, err
	}
	if cand != nil {
		return cand, ctx.Err()
	}
	return losslessResult, ctx.Err()
}

// optimizePDFPixelImage 在有损模式下优化未压缩或通用压缩的像素图像，支持CMYK/ICC/Cal/Indexed及16位图像
// 入参: ctx 取消上下文, orig 原始流, dict 已解析属性字典, filters 过滤器链, params 参数链, options 压缩配置, size 像素需求, colorResources 可选默认颜色空间资源, ignoreSMask 是否忽略外部软遮罩
// 返回: *Stream 优化后的流或空值表示未优化, error 取消或编码错误
func (r *Reader) optimizePDFPixelImage(ctx context.Context, orig *Stream, dict Dictionary, filters, params Array, options CompressionOptions, size image.Point, colorResources Dictionary, ignoreSMask bool) (*Stream, error) {
	if options.Mode != CompressionLossy || dict["Subtype"] != Name("Image") || dict["Mask"] != nil || dict["ImageMask"] == Boolean(true) || !ignoreSMask && dict["SMask"] != nil {
		return nil, ctx.Err()
	}
	for _, filter := range filters {
		switch filter {
		case Name("FlateDecode"), Name("LZWDecode"), Name("ASCII85Decode"), Name("ASCIIHexDecode"), Name("RunLengthDecode"):
		default:
			return nil, ctx.Err()
		}
	}
	space, err := r.resolveColorSpace(dict["ColorSpace"])
	if err != nil {
		return nil, ctx.Err()
	}
	if arr, ok := space.(Array); ok && len(arr) == 1 {
		space = arr[0]
	}
	targetSpace, convertible := r.lossyResolvedColorSpace(space)
	if !convertible {
		return nil, ctx.Err()
	}
	w, _ := dict["Width"].(Integer)
	h, _ := dict["Height"].(Integer)
	if w <= 0 || h <= 0 || w > 1<<20 || h > 1<<20 {
		return nil, ctx.Err()
	}
	hasDefaultOverride := space == Name("DeviceRGB") && colorResources["DefaultRGB"] != nil || space == Name("DeviceGray") && colorResources["DefaultGray"] != nil || space == Name("DeviceCMYK") && colorResources["DefaultCMYK"] != nil
	channels := 3
	if space == Name("DeviceGray") {
		channels = 1
	}
	if !hasDefaultOverride && dict["BitsPerComponent"] == Integer(8) && (space == Name("DeviceRGB") || space == Name("DeviceGray")) && r.isIdentityDecode(dict["Decode"], channels) && (len(filters) == 0 || len(filters) == 1 && filters[0] == Name("FlateDecode")) && int64(w)*int64(h) <= optimizationBufferLimit/4 {
		pixels := orig.Data
		var decodeErr error
		if len(filters) == 1 {
			input, openErr := zlib.NewReader(bytes.NewReader(orig.Data))
			decodeErr = openErr
			if decodeErr == nil {
				pixels, decodeErr = io.ReadAll(io.LimitReader(&contextInput{ctx: ctx, reader: input}, optimizationBufferLimit+1))
				input.Close()
			}
			if decodeErr == nil && len(pixels) <= optimizationBufferLimit {
				pdict, _ := params[0].(Dictionary)
				pixels, decodeErr = restorePredictorContext(ctx, pixels, pdict)
			}
		}
		if decodeErr == nil && len(pixels) <= optimizationBufferLimit && int64(len(pixels)) == int64(w)*int64(h)*int64(channels) {
			var img image.Image
			if channels == 1 {
				img = &image.Gray{Pix: pixels, Stride: int(w), Rect: image.Rect(0, 0, int(w), int(h))}
			} else {
				p := image.NewNRGBA(image.Rect(0, 0, int(w), int(h)))
				for i, j := 0, 0; i < len(pixels); i, j = i+3, j+4 {
					if j&16383 == 0 {
						if err := ctx.Err(); err != nil {
							return nil, err
						}
					}
					copy(p.Pix[j:j+3], pixels[i:i+3])
					p.Pix[j+3] = 255
				}
				img = p
			}
			resized, err := ResizeImage(ctx, img, size)
			if err != nil {
				return nil, err
			}
			cand, err := r.encodePDFImageLossy(ctx, orig, resized, channels == 1, options, false)
			if err != nil {
				return nil, err
			}
			if cand != nil && pdfStreamByteSize(ctx, cand) < pdfStreamByteSize(ctx, orig) {
				return cand, ctx.Err()
			}
			return nil, ctx.Err()
		}
	}
	return r.optimizePDFDecodedImage(ctx, orig, orig, w, h, space, targetSpace, options, size, colorResources, ignoreSMask, false)
}

// optimizeStreamWithOwnedSMask 优化带独享软遮罩的主图，全不透明直接剔除，半透明支持同比例同步降采样
// 入参: ctx 取消上下文, mainStream 主图像流, smaskStream 独享软遮罩流, options 压缩配置, size 主图显示像素需求, colorResources 可选默认颜色空间资源
// 返回: *Stream 主图输出流, *Stream 软遮罩输出流，被剔除时为空, bool 是否已剔除软遮罩, error 取消或编码错误
func (r *Reader) optimizeStreamWithOwnedSMask(ctx context.Context, mainStream, smaskStream *Stream, options CompressionOptions, size image.Point, colorResources Dictionary) (*Stream, *Stream, bool, error) {
	finish := func(mainOut, smaskOut *Stream, dropped bool) (*Stream, *Stream, bool, error) {
		if err := ctx.Err(); err != nil {
			return nil, nil, false, err
		}
		return mainOut, smaskOut, dropped, nil
	}
	opaque, err := r.isSMaskOpaque(ctx, smaskStream)
	if err != nil {
		return nil, nil, false, err
	}
	if opaque {
		stripped := *mainStream
		stripped.Dictionary = maps.Clone(mainStream.Dictionary)
		delete(stripped.Dictionary, "SMask")
		optMain, err := r.optimizeStreamSizeWithContext(ctx, &stripped, options, size, colorResources, false)
		if err != nil {
			return nil, nil, false, err
		}
		if pdfStreamByteSize(ctx, optMain) < pdfStreamByteSize(ctx, mainStream)+pdfStreamByteSize(ctx, smaskStream) {
			return finish(optMain, nil, true)
		}
	}
	losslessOpts := options
	losslessOpts.Mode = CompressionLossless
	baseSMask, err := r.optimizeStreamSize(ctx, smaskStream, losslessOpts, image.Point{})
	if err != nil {
		return nil, nil, false, err
	}
	mainDict, err := r.optimizationImageDictionary(mainStream)
	if err != nil {
		return finish(mainStream, baseSMask, false)
	}
	smaskDict, err := r.optimizationImageDictionary(smaskStream)
	if err != nil {
		return finish(mainStream, baseSMask, false)
	}
	mw, _ := mainDict["Width"].(Integer)
	mh, _ := mainDict["Height"].(Integer)
	sw, _ := smaskDict["Width"].(Integer)
	sh, _ := smaskDict["Height"].(Integer)
	var scale float64
	if size.X > 0 && size.Y > 0 && mw > 0 && mh > 0 {
		scale = math.Max(float64(size.X)/float64(mw), float64(size.Y)/float64(mh))
	}
	downscale := scale > 0 && scale < 1 && sw > 0 && sh > 0
	smaskTarget := image.Point{}
	if downscale {
		smaskTarget = image.Pt(max(1, int(math.Ceil(float64(sw)*scale))), max(1, int(math.Ceil(float64(sh)*scale))))
	}
	matteVal, _ := r.optimizationValue(smaskStream.Dictionary["Matte"])
	if matteVal != nil {
		baseMain, err := r.optimizeStreamSizeWithContext(ctx, mainStream, losslessOpts, image.Point{}, colorResources, false)
		if err != nil {
			return nil, nil, false, err
		}
		if mw <= 0 || mh <= 0 || int64(mw)*int64(mh) > optimizationBufferLimit/8 || mainDict["Mask"] != nil || mainDict["ImageMask"] == Boolean(true) {
			return finish(baseMain, baseSMask, false)
		}
		source, err := r.readOptimizationImage(mainStream, colorResources)
		if err != nil {
			return finish(baseMain, baseSMask, false)
		}
		targetSpace, convertible := r.lossyConvertibleColorSpace(source.ColorSpace)
		if !convertible {
			return finish(baseMain, baseSMask, false)
		}
		decoded, err := source.DecodeImageContext(ctx)
		if err != nil {
			return finish(baseMain, baseSMask, false)
		}
		b := decoded.Bounds()
		rgbImg := image.NewNRGBA(image.Rect(0, 0, b.Dx(), b.Dy()))
		alphaImg := image.NewGray(image.Rect(0, 0, b.Dx(), b.Dy()))
		for y := 0; y < b.Dy(); y++ {
			if err := ctx.Err(); err != nil {
				return nil, nil, false, err
			}
			for x := 0; x < b.Dx(); x++ {
				if x&4095 == 4095 {
					if err := ctx.Err(); err != nil {
						return nil, nil, false, err
					}
				}
				c := imageNRGBAAt(decoded, b.Min.X+x, b.Min.Y+y)
				off := y*rgbImg.Stride + x*4
				rgbImg.Pix[off], rgbImg.Pix[off+1], rgbImg.Pix[off+2], rgbImg.Pix[off+3] = c.R, c.G, c.B, 255
				alphaImg.Pix[y*alphaImg.Stride+x] = c.A
			}
		}
		var mainImg image.Image = rgbImg
		var maskImg image.Image = alphaImg
		if downscale {
			mainImg, err = ResizeImage(ctx, mainImg, size)
			if err != nil {
				return nil, nil, false, err
			}
			maskImg, err = ResizeImage(ctx, maskImg, mainImg.Bounds().Size())
			if err != nil {
				return nil, nil, false, err
			}
		}
		wantGray := targetSpace == Name("DeviceGray")
		if wantGray {
			wantGray, err = imageAllGray(ctx, mainImg)
			if err != nil {
				return nil, nil, false, err
			}
		}
		mainFilters, _, _ := mainStream.filterChain(r)
		sourceJPEG := len(mainFilters) > 0 && mainFilters[len(mainFilters)-1] == Name("DCTDecode")
		candMain, err := r.encodePDFImageLossy(ctx, mainStream, mainImg, wantGray, options, sourceJPEG)
		if err != nil {
			return nil, nil, false, err
		}
		smaskBase := *smaskStream
		smaskBase.Dictionary = maps.Clone(smaskStream.Dictionary)
		smaskBase.Dictionary["ColorSpace"] = Name("DeviceGray")
		smaskBase.Dictionary["BitsPerComponent"] = Integer(8)
		delete(smaskBase.Dictionary, "Matte")
		delete(smaskBase.Dictionary, "Decode")
		delete(smaskBase.Dictionary, "DecodeParms")
		candSMask, err := r.encodeLosslessPixels(ctx, &smaskBase, maskImg, 1)
		if err != nil {
			return nil, nil, false, err
		}
		if candMain != nil && pdfStreamByteSize(ctx, candMain)+pdfStreamByteSize(ctx, candSMask) < pdfStreamByteSize(ctx, baseMain)+pdfStreamByteSize(ctx, baseSMask) {
			return finish(candMain, candSMask, false)
		}
		return finish(baseMain, baseSMask, false)
	}
	if downscale && int64(sw)*int64(sh) <= optimizationBufferLimit/4 {
		scaledMain, err := r.optimizeStreamSizeWithContext(ctx, mainStream, options, size, colorResources, true)
		if err != nil {
			return nil, nil, false, err
		}
		newW, _ := scaledMain.Dictionary["Width"].(Integer)
		newH, _ := scaledMain.Dictionary["Height"].(Integer)
		if newW == mw && newH == mh {
			return finish(scaledMain, baseSMask, false)
		}
		if newW > 0 && newH > 0 && (newW < mw || newH < mh) {
			if maskImg, err := r.ReadImage(smaskStream); err == nil {
				decodedMask, err := maskImg.DecodeImageContext(ctx)
				if err != nil {
					if ctx.Err() != nil {
						return nil, nil, false, ctx.Err()
					}
				} else {
					resizedMask, err := ResizeImage(ctx, decodedMask, smaskTarget)
					if err != nil {
						return nil, nil, false, err
					}
					smaskBase := *smaskStream
					smaskBase.Dictionary = maps.Clone(smaskStream.Dictionary)
					smaskBase.Dictionary["ColorSpace"] = Name("DeviceGray")
					smaskBase.Dictionary["BitsPerComponent"] = Integer(8)
					delete(smaskBase.Dictionary, "Decode")
					delete(smaskBase.Dictionary, "DecodeParms")
					scaledSMask, err := r.encodeLosslessPixels(ctx, &smaskBase, resizedMask, 1)
					if err != nil {
						return nil, nil, false, err
					}
					if pdfStreamByteSize(ctx, scaledMain)+pdfStreamByteSize(ctx, scaledSMask) < pdfStreamByteSize(ctx, mainStream)+pdfStreamByteSize(ctx, baseSMask) {
						return finish(scaledMain, scaledSMask, false)
					}
				}
			}
		}
	}
	sameSizeMain, err := r.optimizeStreamSizeWithContext(ctx, mainStream, options, image.Point{}, colorResources, true)
	if err != nil {
		return nil, nil, false, err
	}
	return finish(sameSizeMain, baseSMask, false)
}
