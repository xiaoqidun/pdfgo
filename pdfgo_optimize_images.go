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
	"image/color"
	"image/jpeg"
	"io"
	"maps"
	"math"
	"os"
	"reflect"
	"slices"
)

// pixelsWithinBudget 判断宽高像素数是否合法且不超过上限，避免整数乘法溢出
// 入参: width 宽度, height 高度, limit 像素上限
// 返回: bool 是否满足预算要求
func pixelsWithinBudget(width, height, limit int64) bool {
	return width > 0 && height > 0 && limit > 0 && width <= limit/height
}

// isFatalOptimizationReadError 判断错误是否属于必须向上传播的取消、关闭、认证、读取或安全错误
// 入参: ctx 取消上下文, err 待检查错误
// 返回: bool 是否为不可吞噬的致命读取错误
func isFatalOptimizationReadError(ctx context.Context, err error) bool {
	if err == nil {
		return false
	}
	if ctx != nil && ctx.Err() != nil {
		return true
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) || errors.Is(err, os.ErrClosed) || errors.Is(err, ErrPassword) {
		return true
	}
	var readErr *ReadError
	var security *objectSecurityError
	return errors.As(err, &readErr) || errors.As(err, &security)
}

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
	w, h := b.Dx(), b.Dy()
	if b.Empty() || w <= 0 || h <= 0 {
		return false, ctx.Err()
	}
	var seen [256]bool
	unique, count, edges := 0, 0, 0
	stepY, stepX := max(1, h/128), max(1, w/128)
	for dy := 0; ; dy += stepY {
		if err := ctx.Err(); err != nil {
			return false, err
		}
		y := b.Min.Y + dy
		for dx := 0; ; dx += stepX {
			x := b.Min.X + dx
			c := imageNRGBAAt(img, x, y).R
			if !seen[c] {
				seen[c] = true
				unique++
			}
			if dx+1 < w {
				d := imageNRGBAAt(img, x+1, y).R
				if math.Abs(float64(c)-float64(d)) > 40 {
					edges++
				}
				count++
			}
			if dx >= w-stepX {
				break
			}
		}
		if dy >= h-stepY {
			break
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
	embeddedMask, err := source.jpxMaskMode(ctx)
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
	if !convertible || lossyColorConflict(source.ColorSpace, targetSpace, colorResources) {
		return stream, ctx.Err()
	}
	downscale := false
	if size.X > 0 && size.Y > 0 && source.Width > 0 && source.Height > 0 {
		scale := math.Max(float64(size.X)/float64(source.Width), float64(size.Y)/float64(source.Height))
		if scale > 0 && scale < 1 {
			downscale = true
			w, h := math.Ceil(float64(source.Width)*scale), math.Ceil(float64(source.Height)*scale)
			if !(w > 0 && h > 0 && w <= float64(optimizationBufferLimit/8)/h) {
				return stream, ctx.Err()
			}
		}
	}
	if !downscale {
		size = image.Point{}
	}
	if !pixelsWithinBudget(int64(source.Width), int64(source.Height), optimizationBufferLimit/8) {
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
	b := decoded.Bounds()
	if !pixelsWithinBudget(int64(b.Dx()), int64(b.Dy()), optimizationBufferLimit/4) {
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
	candidate, err := r.encodePDFImageLossy(ctx, stream, decoded, wantGray, options, false, colorResources)
	if err != nil {
		return nil, err
	}
	if candidate != nil && pdfStreamByteSize(ctx, candidate) < pdfStreamByteSize(ctx, stream) {
		return candidate, ctx.Err()
	}
	return stream, ctx.Err()
}

// readOptimizationImage 读取图像描述并仅应用文档自带的默认颜色空间资源，不将查看器设备配置文件烘焙进输出
// 入参: stream 图像流, colorResources 可选颜色空间资源字典
// 返回: *Image 图像描述, error 解析错误
func (r *Reader) readOptimizationImage(stream *Stream, colorResources Dictionary) (*Image, error) {
	view := r
	if r.deviceProfile != nil {
		copied := *r
		copied.deviceProfile = nil
		if copied.owner == nil {
			copied.owner = r
		}
		copied.ownerHasWarning = r.rootHasWarning()
		view = &copied
	}
	if len(colorResources) != 0 {
		return view.ReadImageWithResources(stream, Dictionary{"ColorSpace": colorResources})
	}
	return view.ReadImage(stream)
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

// defaultColorConflict 检查裸设备色输出是否会被当前资源字典中的Default色空间劫持
// 入参: space 待输出的设备颜色空间, colorResources 可选默认颜色空间资源
// 返回: bool 是否存在Default色空间冲突
func defaultColorConflict(space Name, colorResources Dictionary) bool {
	if len(colorResources) == 0 {
		return false
	}
	return space == Name("DeviceRGB") && colorResources["DefaultRGB"] != nil || space == Name("DeviceGray") && colorResources["DefaultGray"] != nil
}

// lossyColorConflict 判断有损重编的目标设备色空间是否会与当前Default色空间冲突
// 入参: space 源颜色空间, targetSpace 目标设备色空间, colorResources 可选默认颜色空间资源
// 返回: bool 是否存在Default色空间冲突
func lossyColorConflict(space Object, targetSpace Name, colorResources Dictionary) bool {
	if targetSpace == Name("DeviceRGB") {
		return defaultColorConflict("DeviceRGB", colorResources)
	}
	return targetSpace == Name("DeviceGray") && defaultColorConflict("DeviceGray", colorResources) && (space == Name("DeviceGray") || defaultColorConflict("DeviceRGB", colorResources))
}

// encodePDFImageLossy 结合默认颜色空间资源编码8位目标图像，目标裸设备色与Default色空间冲突时返回空值
// 入参: ctx 取消上下文, orig 原始流, img 已缩放图像, wantGray 是否目标灰度, options 压缩配置, sourceJPEG 原流是否为JPEG编码, colorResources 可选默认颜色空间资源
// 返回: *Stream 编码后的图像流或空值, error 取消或编码错误
func (r *Reader) encodePDFImageLossy(ctx context.Context, orig *Stream, img image.Image, wantGray bool, options CompressionOptions, sourceJPEG bool, colorResources Dictionary) (*Stream, error) {
	b := img.Bounds()
	if !pixelsWithinBudget(int64(b.Dx()), int64(b.Dy()), optimizationBufferLimit/4) {
		return nil, ctx.Err()
	}
	if wantGray {
		if defaultColorConflict("DeviceGray", colorResources) {
			return nil, ctx.Err()
		}
	} else if _, isGray := img.(*image.Gray); !isGray && defaultColorConflict("DeviceRGB", colorResources) {
		return nil, ctx.Err()
	}
	var encodeImg image.Image = img
	channels := 3
	if wantGray {
		grayImg, err := imageToGray8(ctx, img)
		if err != nil {
			return nil, err
		}
		encodeImg = grayImg
		channels = 1
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
		outSpace := Name("DeviceRGB")
		if wantGray {
			outSpace = Name("DeviceGray")
		}
		if defaultColorConflict(outSpace, colorResources) {
			return nil, ctx.Err()
		}
		base := *orig
		base.Dictionary = maps.Clone(orig.Dictionary)
		base.Dictionary["BitsPerComponent"] = Integer(8)
		delete(base.Dictionary, "Decode")
		delete(base.Dictionary, "DecodeParms")
		delete(base.Dictionary, "SMaskInData")
		base.Dictionary["ColorSpace"] = outSpace
		candidate, err := r.encodeLosslessPixels(ctx, &base, orig, encodeImg, channels)
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
	outSpace := Name("DeviceRGB")
	if _, isGray := jpegSource.(*image.Gray); isGray {
		outSpace = Name("DeviceGray")
	}
	if defaultColorConflict(outSpace, colorResources) {
		return nil, ctx.Err()
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
	result, err := r.reencodedStream(orig, encoded, Name("DCTDecode"))
	if err != nil {
		return nil, err
	}
	result.Dictionary["BitsPerComponent"] = Integer(8)
	delete(result.Dictionary, "Decode")
	delete(result.Dictionary, "SMaskInData")
	result.Dictionary["ColorSpace"] = outSpace
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
	if !pixelsWithinBudget(int64(maskImg.Width), int64(maskImg.Height), optimizationBufferLimit/4) {
		return false, ctx.Err()
	}
	decoded, err := maskImg.DecodeImageContext(ctx)
	if err != nil {
		return false, ctx.Err()
	}
	b := decoded.Bounds()
	if !pixelsWithinBudget(int64(b.Dx()), int64(b.Dy()), optimizationBufferLimit/4) {
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
	limit := optimizationBufferLimit
	if maxBytes, _ := effectiveStreamLimits(r, stream); maxBytes > 0 && maxBytes < int64(limit) {
		limit = int(maxBytes)
	}
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
			data, err = io.ReadAll(io.LimitReader(&contextInput{ctx: ctx, reader: input}, int64(limit)+1))
			input.Close()
			if err == nil && len(data) <= limit {
				data, err = restorePredictorContext(ctx, data, dict)
			}
		case Name("LZWDecode"):
			early, e := integerDefault(dict, "EarlyChange", 1)
			if e != nil {
				return nil, nil
			}
			data, _, err = decodeLZWBytesLimitContext(ctx, data, early, int64(limit))
			if err == nil {
				data, err = restorePredictorContext(ctx, data, dict)
			}
		case Name("ASCIIHexDecode"):
			_, size, e := asciiHexSize(ctx, data, nil)
			if e != nil || size > limit {
				return nil, ctx.Err()
			}
			data, err = decodeASCIIHexContext(ctx, data, nil)
		case Name("ASCII85Decode"):
			_, size, e := ascii85Size(ctx, data, nil)
			if e != nil || size > limit {
				return nil, ctx.Err()
			}
			data, err = decodeASCII85Context(ctx, data, nil)
		case Name("RunLengthDecode"):
			size, e := runLengthSize(ctx, data, nil)
			if e != nil || size > limit {
				return nil, ctx.Err()
			}
			data, err = decodeRunLength(ctx, data, nil)
		default:
			return nil, nil
		}
		if err != nil || len(data) > limit {
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

// resolveOptimizationObjectRef 解析压缩阶段间接引用并维护原有缓存快照
// 入参: value 待解析对象, cached 原缓存快照, cycleErr 循环引用错误文案
// 返回: Object 解析结果, Reference 最终对象引用, error 循环或读取错误
func (r *Reader) resolveOptimizationObjectRef(value Object, cached map[Reference]bool, cycleErr string) (Object, Reference, error) {
	var ref Reference
	for n := 0; ; n++ {
		next, ok := value.(Reference)
		if !ok {
			return value, ref, nil
		}
		if n > 32 {
			return nil, Reference{}, fmt.Errorf("%s", cycleErr)
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

// walkImageAlternates 遍历图像Alternates备用图像数组并回调有效备用图像流引用，检测循环、超深与损坏结构
// 入参: ctx 取消上下文, stream 主图像流, rootRef 主图像引用, resolveObjectRef 间接引用解析函数, visit 备用图像引用回调
// 返回: bool 遍历是否出现环路, bool 结构是否完整可解析, error 回调、致命读取或取消错误
func (r *Reader) walkImageAlternates(ctx context.Context, stream *Stream, rootRef Reference, resolveObjectRef func(Object) (Object, Reference, error), visit func(Reference) error) (bool, bool, error) {
	if stream == nil || stream.Dictionary["Alternates"] == nil {
		return false, true, nil
	}
	type altNode struct {
		stream *Stream
		depth  int
	}
	seen := make(map[Reference]bool)
	if rootRef.Number > 0 {
		seen[rootRef] = true
	}
	queue := []altNode{{stream: stream, depth: 0}}
	cyclic := false
	for i := 0; i < len(queue); i++ {
		if err := ctx.Err(); err != nil {
			return cyclic, false, err
		}
		cur := queue[i]
		if cur.stream == nil || cur.stream.Dictionary["Alternates"] == nil {
			continue
		}
		if cur.depth >= 32 {
			return cyclic, false, nil
		}
		altObj, _, err := resolveObjectRef(cur.stream.Dictionary["Alternates"])
		if err != nil {
			if isFatalOptimizationReadError(ctx, err) {
				return cyclic, false, err
			}
			return cyclic, false, nil
		}
		if altObj == nil {
			continue
		}
		alts, ok := altObj.(Array)
		if !ok {
			return cyclic, false, nil
		}
		for _, entry := range alts {
			if err := ctx.Err(); err != nil {
				return cyclic, false, err
			}
			dictObj, _, err := resolveObjectRef(entry)
			if err != nil {
				if isFatalOptimizationReadError(ctx, err) {
					return cyclic, false, err
				}
				return cyclic, false, nil
			}
			if dictObj == nil {
				continue
			}
			dict, ok := dictObj.(Dictionary)
			if !ok {
				return cyclic, false, nil
			}
			imgObj, imgRef, err := resolveObjectRef(dict["Image"])
			if err != nil {
				if isFatalOptimizationReadError(ctx, err) {
					return cyclic, false, err
				}
				return cyclic, false, nil
			}
			imgStream, ok := imgObj.(*Stream)
			if !ok || imgStream == nil {
				return cyclic, false, nil
			}
			sub, _, err := resolveObjectRef(imgStream.Dictionary["Subtype"])
			if err != nil {
				if isFatalOptimizationReadError(ctx, err) {
					return cyclic, false, err
				}
				return cyclic, false, nil
			}
			if sub != Name("Image") {
				return cyclic, false, nil
			}
			if imgRef.Number > 0 {
				if seen[imgRef] {
					if cur.depth > 0 || imgRef == rootRef {
						cyclic = true
					}
					continue
				}
				if len(seen) >= 4096 {
					return cyclic, false, nil
				}
				seen[imgRef] = true
				if err := visit(imgRef); err != nil {
					return cyclic, false, err
				}
			}
			if imgStream.Dictionary["Alternates"] != nil {
				if len(queue) >= 4096 {
					return cyclic, false, nil
				}
				queue = append(queue, altNode{stream: imgStream, depth: cur.depth + 1})
			}
		}
	}
	return cyclic, true, ctx.Err()
}

// imageOutputSizes 汇总页面、表单、注释外观、媒体、渲染动作及平铺图案中的图片最大显示尺寸，Type3字形图仅禁降采样
// 入参: ctx 取消上下文, dpi 分辨率上限
// 返回: map[Reference]image.Point 图片像素上限, error 取消或致命读取错误
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
	type type3OwnResult struct {
		refs        []Reference
		opaqueError bool
	}
	type type3ProcsResult struct {
		names    []Name
		fallback bool
	}
	formCache := make(map[Reference]*cachedFormImages)
	formActive := make(map[Reference]bool)
	patternCache := make(map[Reference]*cachedFormImages)
	patternActive := make(map[Reference]uint8)
	type3OwnCache := make(map[Reference]type3OwnResult)
	type3FontProcsCache := make(map[Reference]type3ProcsResult)
	type3ProcCache := make(map[Reference]type3ProcsResult)
	resolveObjectRef := func(value Object) (Object, Reference, error) {
		return r.resolveOptimizationObjectRef(value, cached, "cyclic image reference")
	}
	errUnsafe := errors.New("global unsafe")
	errPlacementLimit := errors.New("image placement limit exceeded")
	markImageUnsafe := func(stream *Stream, ref Reference, target map[Reference]bool) (bool, error) {
		if ref.Number > 0 {
			target[ref] = true
		}
		_, ok, err := r.walkImageAlternates(ctx, stream, ref, resolveObjectRef, func(altRef Reference) error {
			target[altRef] = true
			return nil
		})
		if err != nil {
			return false, err
		}
		return ok, nil
	}
	var markResourcesUnsafe func(Dictionary, map[Reference]bool, map[Reference]bool, int) (bool, error)
	markResourcesUnsafe = func(res Dictionary, target map[Reference]bool, visited map[Reference]bool, depth int) (bool, error) {
		if err := ctx.Err(); err != nil {
			return false, err
		}
		if depth > 32 {
			return false, nil
		}
		if res == nil {
			return true, nil
		}
		if res["XObject"] != nil {
			val, err := r.Resolve(res["XObject"])
			if err != nil {
				if isFatalOptimizationReadError(ctx, err) {
					return false, err
				}
				return false, nil
			}
			if val != nil {
				objects, ok := val.(Dictionary)
				if !ok {
					return false, nil
				}
				for _, entry := range objects {
					if err := ctx.Err(); err != nil {
						return false, err
					}
					obj, ref, err := resolveObjectRef(entry)
					if err != nil {
						if isFatalOptimizationReadError(ctx, err) {
							return false, err
						}
						return false, nil
					}
					stream, ok := obj.(*Stream)
					if !ok {
						continue
					}
					subtype, err := r.Resolve(stream.Dictionary["Subtype"])
					if err != nil {
						if isFatalOptimizationReadError(ctx, err) {
							return false, err
						}
						return false, nil
					}
					switch subtype {
					case Name("Image"):
						ok, err := markImageUnsafe(stream, ref, target)
						if err != nil {
							return false, err
						}
						if !ok {
							return false, nil
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
								if isFatalOptimizationReadError(ctx, err) {
									return false, err
								}
								return false, nil
							}
							if childVal != nil {
								childDict, ok := childVal.(Dictionary)
								if !ok {
									return false, nil
								}
								ok, err := markResourcesUnsafe(childDict, target, visited, depth+1)
								if err != nil {
									return false, err
								}
								if !ok {
									return false, nil
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
				if isFatalOptimizationReadError(ctx, err) {
					return false, err
				}
				return false, nil
			}
			if patterns, ok := val.(Dictionary); ok {
				for _, entry := range patterns {
					if err := ctx.Err(); err != nil {
						return false, err
					}
					obj, ref, err := resolveObjectRef(entry)
					if err != nil {
						if isFatalOptimizationReadError(ctx, err) {
							return false, err
						}
						return false, nil
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
							if isFatalOptimizationReadError(ctx, err) {
								return false, err
							}
							return false, nil
						}
						if childDict, ok := childVal.(Dictionary); ok {
							ok, err := markResourcesUnsafe(childDict, target, visited, depth+1)
							if err != nil {
								return false, err
							}
							if !ok {
								return false, nil
							}
						}
					}
				}
			}
		}
		if res["Font"] != nil {
			val, err := r.Resolve(res["Font"])
			if err != nil {
				if isFatalOptimizationReadError(ctx, err) {
					return false, err
				}
				return false, nil
			}
			if fonts, ok := val.(Dictionary); ok {
				for _, entry := range fonts {
					if err := ctx.Err(); err != nil {
						return false, err
					}
					obj, ref, err := resolveObjectRef(entry)
					if err != nil {
						if isFatalOptimizationReadError(ctx, err) {
							return false, err
						}
						return false, nil
					}
					dict, ok := obj.(Dictionary)
					if !ok {
						continue
					}
					subtype, err := r.Resolve(dict["Subtype"])
					if err != nil {
						if isFatalOptimizationReadError(ctx, err) {
							return false, err
						}
						continue
					}
					if subtype != Name("Type3") {
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
							if isFatalOptimizationReadError(ctx, err) {
								return false, err
							}
							return false, nil
						}
						if childDict, ok := childVal.(Dictionary); ok {
							ok, err := markResourcesUnsafe(childDict, target, visited, depth+1)
							if err != nil {
								return false, err
							}
							if !ok {
								return false, nil
							}
						}
					}
				}
			}
		}
		return true, ctx.Err()
	}
	markType3Fonts := func(res Dictionary, target map[Reference]bool, seenRes map[uintptr]Dictionary, budget *int) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if res == nil || res["Font"] == nil {
			return nil
		}
		if ptr := reflect.ValueOf(res).Pointer(); ptr != 0 {
			if _, seen := seenRes[ptr]; seen {
				return nil
			}
			if len(seenRes) < 65536 {
				seenRes[ptr] = res
			}
		}
		*budget--
		if *budget < 0 {
			return errUnsafe
		}
		val, err := r.Resolve(res["Font"])
		if err != nil {
			if isFatalOptimizationReadError(ctx, err) {
				return err
			}
			return nil
		}
		fonts, ok := val.(Dictionary)
		if !ok {
			return nil
		}
		var xobjects Dictionary
		xobjectsResolved := false
		for _, entry := range fonts {
			if err := ctx.Err(); err != nil {
				return err
			}
			*budget--
			if *budget < 0 {
				return errUnsafe
			}
			obj, ref, err := resolveObjectRef(entry)
			if err != nil {
				if isFatalOptimizationReadError(ctx, err) {
					return err
				}
				continue
			}
			dict, ok := obj.(Dictionary)
			if !ok {
				continue
			}
			subtype, err := r.Resolve(dict["Subtype"])
			if err != nil {
				if isFatalOptimizationReadError(ctx, err) {
					return err
				}
				continue
			}
			if subtype != Name("Type3") {
				continue
			}
			if dict["Resources"] != nil {
				if ref.Number > 0 {
					if cachedEntry, hit := type3OwnCache[ref]; hit {
						if cachedEntry.opaqueError {
							return errUnsafe
						}
						for i, item := range cachedEntry.refs {
							if i&255 == 0 {
								if err := ctx.Err(); err != nil {
									return err
								}
							}
							*budget--
							if *budget < 0 {
								return errUnsafe
							}
							target[item] = true
						}
						continue
					}
				}
				childVal, err := r.Resolve(dict["Resources"])
				if err != nil {
					if isFatalOptimizationReadError(ctx, err) {
						return err
					}
					continue
				}
				if childDict, ok := childVal.(Dictionary); ok {
					sub := make(map[Reference]bool)
					ok, err := markResourcesUnsafe(childDict, sub, map[Reference]bool{ref: true}, 0)
					if err != nil {
						return err
					}
					resEntry := type3OwnResult{opaqueError: !ok}
					if ok {
						resEntry.refs = slices.Collect(maps.Keys(sub))
					}
					if ref.Number > 0 && len(type3OwnCache) < 65536 {
						type3OwnCache[ref] = resEntry
					}
					if !ok {
						return errUnsafe
					}
					for item := range sub {
						target[item] = true
					}
				}
				continue
			}
			var procsRes type3ProcsResult
			cachedProcs := false
			if ref.Number > 0 {
				procsRes, cachedProcs = type3FontProcsCache[ref]
			}
			if !cachedProcs {
				procsVal, err := r.Resolve(dict["CharProcs"])
				if err != nil {
					if isFatalOptimizationReadError(ctx, err) {
						return err
					}
					continue
				}
				procs, ok := procsVal.(Dictionary)
				if !ok {
					continue
				}
				nameSet := make(map[Name]struct{})
				for _, procEntry := range procs {
					if err := ctx.Err(); err != nil {
						return err
					}
					*budget--
					if *budget < 0 {
						return errUnsafe
					}
					procObj, procRef, err := resolveObjectRef(procEntry)
					if err != nil {
						if isFatalOptimizationReadError(ctx, err) {
							return err
						}
						continue
					}
					if procRef.Number > 0 {
						if cachedProc, hit := type3ProcCache[procRef]; hit {
							if cachedProc.fallback {
								procsRes.fallback = true
								break
							}
							for _, name := range cachedProc.names {
								nameSet[name] = struct{}{}
							}
							continue
						}
					}
					procStream, ok := procObj.(*Stream)
					if !ok {
						continue
					}
					data, err := r.optimizationContentData(ctx, procStream, optimizationBufferLimit)
					if err != nil {
						if isFatalOptimizationReadError(ctx, err) {
							return err
						}
						procsRes.fallback = true
						if procRef.Number > 0 && len(type3ProcCache) < 65536 {
							type3ProcCache[procRef] = type3ProcsResult{fallback: true}
						}
						break
					}
					procNames := make(map[Name]struct{})
					walkErr := walkOperationsBorrowed(ctx, data, func(op Operation) error {
						if op.Operator == "Do" && len(op.Operands) == 1 {
							if name, ok := op.Operands[0].(Name); ok {
								procNames[name] = struct{}{}
								nameSet[name] = struct{}{}
							}
						}
						return nil
					})
					if walkErr != nil {
						if isFatalOptimizationReadError(ctx, walkErr) {
							return walkErr
						}
						procsRes.fallback = true
						if procRef.Number > 0 && len(type3ProcCache) < 65536 {
							type3ProcCache[procRef] = type3ProcsResult{fallback: true}
						}
						break
					}
					if procRef.Number > 0 && len(type3ProcCache) < 65536 {
						type3ProcCache[procRef] = type3ProcsResult{names: slices.Collect(maps.Keys(procNames))}
					}
				}
				if !procsRes.fallback && len(nameSet) > 0 {
					procsRes.names = slices.Collect(maps.Keys(nameSet))
				}
				if ref.Number > 0 && len(type3FontProcsCache) < 65536 {
					type3FontProcsCache[ref] = procsRes
				}
			}
			if procsRes.fallback {
				ok, err := markResourcesUnsafe(res, target, map[Reference]bool{ref: true}, 0)
				if err != nil {
					return err
				}
				if !ok {
					return errUnsafe
				}
				continue
			}
			if len(procsRes.names) == 0 {
				continue
			}
			if !xobjectsResolved {
				xobjectsResolved = true
				xobjVal, err := r.Resolve(res["XObject"])
				if err != nil {
					if isFatalOptimizationReadError(ctx, err) {
						return err
					}
					continue
				}
				xobjects, _ = xobjVal.(Dictionary)
			}
			if xobjects == nil {
				continue
			}
			for _, name := range procsRes.names {
				if err := ctx.Err(); err != nil {
					return err
				}
				*budget--
				if *budget < 0 {
					return errUnsafe
				}
				xobj, xref, err := resolveObjectRef(xobjects[name])
				if err != nil {
					if isFatalOptimizationReadError(ctx, err) {
						return err
					}
					continue
				}
				s, ok := xobj.(*Stream)
				if !ok {
					continue
				}
				sub, err := r.Resolve(s.Dictionary["Subtype"])
				if err != nil {
					if isFatalOptimizationReadError(ctx, err) {
						return err
					}
					continue
				}
				if sub == Name("Image") {
					ok, err := markImageUnsafe(s, xref, target)
					if err != nil {
						return err
					}
					if !ok {
						return errUnsafe
					}
				} else if sub == Name("Form") {
					fResVal, err := r.Resolve(s.Dictionary["Resources"])
					if err != nil {
						if isFatalOptimizationReadError(ctx, err) {
							return err
						}
						continue
					}
					if fRes, ok := fResVal.(Dictionary); ok {
						ok, err := markResourcesUnsafe(fRes, target, map[Reference]bool{xref: true}, 0)
						if err != nil {
							return err
						}
						if !ok {
							return errUnsafe
						}
					}
				}
			}
		}
		return ctx.Err()
	}
	recordPlacement := func(ref Reference, matrix Matrix, collector map[imagePlacementKey]struct{}, localUnsafe map[Reference]bool) bool {
		if ref.Number <= 0 {
			return true
		}
		for _, value := range matrix[:4] {
			if math.IsNaN(value) || math.IsInf(value, 0) {
				localUnsafe[ref] = true
				return true
			}
		}
		if collector != nil {
			key := imagePlacementKey{ref: ref, a: matrix[0], b: matrix[1], c: matrix[2], d: matrix[3]}
			if _, exists := collector[key]; !exists {
				if len(collector) >= 10000 {
					return false
				}
				collector[key] = struct{}{}
			}
			return true
		}
		w := math.Ceil(math.Nextafter(math.Hypot(matrix[0], matrix[1])*float64(dpi)/72, math.Inf(-1)))
		h := math.Ceil(math.Nextafter(math.Hypot(matrix[2], matrix[3])*float64(dpi)/72, math.Inf(-1)))
		if math.IsNaN(w) || math.IsNaN(h) || math.IsInf(w, 0) || math.IsInf(h, 0) || w > 1<<30 || h > 1<<30 {
			localUnsafe[ref] = true
			return true
		}
		old := sizes[ref]
		sizes[ref] = image.Pt(max(old.X, max(1, int(w))), max(old.Y, max(1, int(h))))
		return true
	}
	recordImagePlacement := func(stream *Stream, ref Reference, matrix Matrix, collector map[imagePlacementKey]struct{}, localUnsafe map[Reference]bool) error {
		if !recordPlacement(ref, matrix, collector, localUnsafe) {
			return errPlacementLimit
		}
		var altRefs []Reference
		cyclic, ok, err := r.walkImageAlternates(ctx, stream, ref, resolveObjectRef, func(altRef Reference) error {
			altRefs = append(altRefs, altRef)
			if !recordPlacement(altRef, matrix, collector, localUnsafe) {
				return errPlacementLimit
			}
			return nil
		})
		if err != nil {
			return err
		}
		if cyclic {
			if ref.Number > 0 {
				localUnsafe[ref] = true
			}
			for _, altRef := range altRefs {
				localUnsafe[altRef] = true
			}
		}
		if !ok {
			return errUnsafe
		}
		return nil
	}
	applyCachedForm := func(entry *cachedFormImages, parentMatrix Matrix, budget *int, collector map[imagePlacementKey]struct{}, localUnsafe map[Reference]bool) error {
		if entry.opaqueError {
			return errUnsafe
		}
		for i, ref := range entry.unsafe {
			if i&255 == 0 {
				if err := ctx.Err(); err != nil {
					return err
				}
			}
			localUnsafe[ref] = true
		}
		for i, item := range entry.placements {
			if i&255 == 0 {
				if err := ctx.Err(); err != nil {
					return err
				}
			}
			*budget--
			if *budget < 0 {
				return errUnsafe
			}
			combined := parentMatrix.Mul(Matrix{item.a, item.b, item.c, item.d, 0, 0})
			if !recordPlacement(item.ref, combined, collector, localUnsafe) {
				return errPlacementLimit
			}
		}
		return ctx.Err()
	}
	var walk func([]byte, Dictionary, Matrix, int, *int, map[imagePlacementKey]struct{}, map[Reference]bool, map[uintptr]Dictionary) error
	walkPatterns := func(resources Dictionary, initialMatrix Matrix, depth int, budget *int, collector map[imagePlacementKey]struct{}, localUnsafe map[Reference]bool, type3Seen map[uintptr]Dictionary) error {
		if resources == nil || resources["Pattern"] == nil {
			return nil
		}
		if depth > 32 {
			return errUnsafe
		}
		val, err := r.Resolve(resources["Pattern"])
		if err != nil {
			if isFatalOptimizationReadError(ctx, err) {
				return err
			}
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
			if *budget <= 0 {
				return errUnsafe
			}
			*budget--
			if patRef, ok := entry.(Reference); ok && patRef.Number > 0 {
				if active := patternActive[patRef]; active != 0 {
					if active == 2 {
						return errUnsafe
					}
					continue
				}
				if cachedEntry, ok := patternCache[patRef]; ok {
					if err := applyCachedForm(cachedEntry, initialMatrix, budget, collector, localUnsafe); err != nil {
						return err
					}
					continue
				}
			}
			obj, ref, err := resolveObjectRef(entry)
			if err != nil {
				if isFatalOptimizationReadError(ctx, err) {
					return err
				}
				continue
			}
			if ref.Number > 0 {
				if active := patternActive[ref]; active != 0 {
					if active == 2 {
						return errUnsafe
					}
					continue
				}
				if cachedEntry, ok := patternCache[ref]; ok {
					if err := applyCachedForm(cachedEntry, initialMatrix, budget, collector, localUnsafe); err != nil {
						return err
					}
					continue
				}
			}
			patStream, ok := obj.(*Stream)
			if !ok {
				continue
			}
			kind, err := r.Resolve(patStream.Dictionary["PatternType"])
			if err != nil {
				if isFatalOptimizationReadError(ctx, err) {
					return err
				}
				continue
			}
			if kind != Integer(1) {
				continue
			}
			patRes := resources
			hasOwnRes := false
			if patStream.Dictionary["Resources"] != nil {
				resVal, err := r.Resolve(patStream.Dictionary["Resources"])
				if err != nil {
					if isFatalOptimizationReadError(ctx, err) {
						return err
					}
					return errUnsafe
				}
				if resDict, ok := resVal.(Dictionary); ok {
					patRes = resDict
					hasOwnRes = true
				} else if resVal != nil {
					return errUnsafe
				}
			}
			patMatrix := Identity()
			matrixErr := false
			if patStream.Dictionary["Matrix"] != nil {
				mVal, err := r.Resolve(patStream.Dictionary["Matrix"])
				if err != nil {
					if isFatalOptimizationReadError(ctx, err) {
						return err
					}
					matrixErr = true
				} else if mVal != nil {
					values, err := r.numberArray(mVal, 6)
					if err != nil {
						matrixErr = true
					} else {
						patMatrix = Matrix(values)
					}
				}
			}
			if hasOwnRes && ref.Number > 0 {
				if matrixErr {
					subUnsafe := make(map[Reference]bool)
					cachedEntry := &cachedFormImages{}
					ok, markErr := markResourcesUnsafe(patRes, subUnsafe, map[Reference]bool{ref: true}, 0)
					if markErr != nil {
						return markErr
					}
					if !ok {
						cachedEntry.opaqueError = true
					} else {
						cachedEntry.unsafe = slices.Collect(maps.Keys(subUnsafe))
					}
					patternCache[ref] = cachedEntry
					if err := applyCachedForm(cachedEntry, initialMatrix, budget, collector, localUnsafe); err != nil {
						return err
					}
					continue
				}
				patternActive[ref] = 2
				patternBudget := 100000
				subCollector := make(map[imagePlacementKey]struct{})
				subUnsafe := make(map[Reference]bool)
				content, decodeErr := r.optimizationContentData(ctx, patStream, optimizationBufferLimit)
				walkErr := decodeErr
				if walkErr == nil {
					walkErr = walk(content, patRes, patMatrix, depth+1, &patternBudget, subCollector, subUnsafe, make(map[uintptr]Dictionary))
				}
				delete(patternActive, ref)
				if isFatalOptimizationReadError(ctx, walkErr) || errors.Is(walkErr, errUnsafe) {
					return walkErr
				}
				var cachedEntry *cachedFormImages
				if walkErr != nil {
					cachedEntry = &cachedFormImages{}
					ok, markErr := markResourcesUnsafe(patRes, subUnsafe, map[Reference]bool{ref: true}, 0)
					if markErr != nil {
						return markErr
					}
					if !ok {
						cachedEntry.opaqueError = true
					} else {
						cachedEntry.unsafe = slices.Collect(maps.Keys(subUnsafe))
					}
				} else {
					cachedEntry = &cachedFormImages{
						placements: slices.Collect(maps.Keys(subCollector)),
						unsafe:     slices.Collect(maps.Keys(subUnsafe)),
					}
				}
				patternCache[ref] = cachedEntry
				if err := applyCachedForm(cachedEntry, initialMatrix, budget, collector, localUnsafe); err != nil {
					return err
				}
				continue
			}
			if matrixErr {
				ok, markErr := markResourcesUnsafe(patRes, localUnsafe, map[Reference]bool{ref: true}, 0)
				if markErr != nil {
					return markErr
				}
				if !ok {
					return errUnsafe
				}
				continue
			}
			if ref.Number > 0 {
				patternActive[ref] = 1
			}
			content, decodeErr := r.optimizationContentData(ctx, patStream, optimizationBufferLimit)
			walkErr := decodeErr
			if walkErr == nil {
				walkErr = walk(content, patRes, initialMatrix.Mul(patMatrix), depth+1, budget, collector, localUnsafe, type3Seen)
			}
			if ref.Number > 0 {
				delete(patternActive, ref)
			}
			if isFatalOptimizationReadError(ctx, walkErr) {
				return walkErr
			}
			if walkErr != nil {
				if errors.Is(walkErr, errUnsafe) || errors.Is(walkErr, errPlacementLimit) {
					return walkErr
				}
				ok, markErr := markResourcesUnsafe(patRes, localUnsafe, map[Reference]bool{ref: true}, 0)
				if markErr != nil {
					return markErr
				}
				if !ok {
					return errUnsafe
				}
			}
		}
		return ctx.Err()
	}
	walk = func(data []byte, resources Dictionary, matrix Matrix, depth int, budget *int, collector map[imagePlacementKey]struct{}, localUnsafe map[Reference]bool, type3Seen map[uintptr]Dictionary) error {
		if depth > 32 {
			return errUnsafe
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
					if err := recordImagePlacement(stream, ref, matrix, collector, localUnsafe); err != nil {
						return err
					}
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
							return applyCachedForm(entry, matrix, budget, collector, localUnsafe)
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
						content, decodeErr := r.optimizationContentData(ctx, stream, optimizationBufferLimit)
						walkErr := decodeErr
						if walkErr == nil {
							walkErr = walk(content, childResources, formMatrix, depth+1, &formBudget, subCollector, subUnsafe, make(map[uintptr]Dictionary))
						}
						delete(formActive, ref)
						if isFatalOptimizationReadError(ctx, walkErr) || errors.Is(walkErr, errUnsafe) {
							return walkErr
						}
						var entry *cachedFormImages
						if walkErr != nil {
							entry = &cachedFormImages{}
							ok, markErr := markResourcesUnsafe(childResources, subUnsafe, map[Reference]bool{ref: true}, 0)
							if markErr != nil {
								return markErr
							}
							if !ok {
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
						return applyCachedForm(entry, matrix, budget, collector, localUnsafe)
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
					content, err := r.optimizationContentData(ctx, stream, optimizationBufferLimit)
					if err != nil {
						return err
					}
					return walk(content, childResources, child, depth+1, budget, collector, localUnsafe, type3Seen)
				}
			}
			return nil
		})
		if err != nil {
			return err
		}
		if err := markType3Fonts(resources, localUnsafe, type3Seen, budget); err != nil {
			return err
		}
		return walkPatterns(resources, initialMatrix, depth, budget, collector, localUnsafe, type3Seen)
	}
	resolveFormResources := func(stream *Stream, fallback Dictionary) (Dictionary, error) {
		res := fallback
		if stream.Dictionary["Resources"] != nil {
			resVal, err := r.Resolve(stream.Dictionary["Resources"])
			if err != nil {
				if isFatalOptimizationReadError(ctx, err) {
					return nil, err
				}
				return nil, errUnsafe
			}
			if resVal != nil {
				dict, ok := resVal.(Dictionary)
				if !ok {
					return nil, errUnsafe
				}
				res = dict
			}
		}
		return res, nil
	}
	walkAnnotationStream := func(page *Page, unit float64, rect Rectangle, rectValid bool, apStream *Stream) error {
		apRes, err := resolveFormResources(apStream, page.Resources)
		if err != nil {
			return err
		}
		failSafe := func(cause error) error {
			if isFatalOptimizationReadError(ctx, cause) {
				return cause
			}
			ok, markErr := markResourcesUnsafe(apRes, unsafe, map[Reference]bool{}, 0)
			if markErr != nil {
				return markErr
			}
			if !ok {
				return errUnsafe
			}
			return nil
		}
		if !rectValid || rect.XMin == rect.XMax || rect.YMin == rect.YMax {
			return failSafe(nil)
		}
		box, err := r.rectangle(apStream.Dictionary["BBox"])
		if err != nil {
			return failSafe(err)
		}
		formMatrix := Identity()
		if apStream.Dictionary["Matrix"] != nil {
			mVal, err := r.Resolve(apStream.Dictionary["Matrix"])
			if err != nil {
				return failSafe(err)
			}
			if mVal != nil {
				values, err := r.numberArray(mVal, 6)
				if err != nil {
					return failSafe(err)
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
			return failSafe(nil)
		}
		scaleX := (rect.XMax - rect.XMin) / (maxX - minX)
		scaleY := (rect.YMax - rect.YMin) / (maxY - minY)
		apMatrix := Matrix{unit * scaleX, 0, 0, unit * scaleY, 0, 0}.Mul(formMatrix)
		content, err := r.optimizationContentData(ctx, apStream, optimizationBufferLimit)
		if err != nil {
			return failSafe(err)
		}
		apBudget := 100000
		if err := walk(content, apRes, apMatrix, 0, &apBudget, nil, unsafe, make(map[uintptr]Dictionary)); err != nil {
			if isFatalOptimizationReadError(ctx, err) {
				return err
			}
			if errors.Is(err, errUnsafe) {
				return errUnsafe
			}
			return failSafe(err)
		}
		return ctx.Err()
	}
	walkAnnotationMedia := func(page *Page, unit float64, rect Rectangle, rectValid bool, rotate int64, unbounded bool, mediaObj Object) error {
		if mediaObj == nil {
			return nil
		}
		obj, ref, err := resolveObjectRef(mediaObj)
		if err != nil {
			if isFatalOptimizationReadError(ctx, err) {
				return err
			}
			return nil
		}
		stream, ok := obj.(*Stream)
		if !ok {
			return nil
		}
		sub, err := r.Resolve(stream.Dictionary["Subtype"])
		if err != nil {
			if isFatalOptimizationReadError(ctx, err) {
				return err
			}
			if ref.Number > 0 {
				unsafe[ref] = true
			}
			return nil
		}
		targetRect := rect
		if rectValid && rotate%180 != 0 {
			targetRect = Rectangle{XMin: 0, YMin: 0, XMax: math.Abs(rect.YMax - rect.YMin), YMax: math.Abs(rect.XMax - rect.XMin)}
		}
		switch sub {
		case Name("Image"):
			if unbounded || !rectValid || targetRect.XMin == targetRect.XMax || targetRect.YMin == targetRect.YMax {
				ok, err := markImageUnsafe(stream, ref, unsafe)
				if err != nil {
					return err
				}
				if !ok {
					return errUnsafe
				}
				return nil
			}
			w := math.Abs(targetRect.XMax - targetRect.XMin)
			h := math.Abs(targetRect.YMax - targetRect.YMin)
			if err := recordImagePlacement(stream, ref, Matrix{unit * w, 0, 0, unit * h, 0, 0}, nil, unsafe); err != nil {
				return err
			}
		case Name("Form"):
			if unbounded {
				res, err := resolveFormResources(stream, page.Resources)
				if err != nil {
					return err
				}
				ok, markErr := markResourcesUnsafe(res, unsafe, map[Reference]bool{ref: true}, 0)
				if markErr != nil {
					return markErr
				}
				if !ok {
					return errUnsafe
				}
				return nil
			}
			return walkAnnotationStream(page, unit, targetRect, rectValid, stream)
		}
		return nil
	}
	mediaWalker := mediaActionWalker{
		reader:           r,
		ctx:              ctx,
		resolveObjectRef: resolveObjectRef,
		onStream: func(v *Stream, ref Reference, pageRes Dictionary) error {
			sub, err := r.Resolve(v.Dictionary["Subtype"])
			if err != nil {
				if isFatalOptimizationReadError(ctx, err) {
					return err
				}
				if ref.Number > 0 {
					unsafe[ref] = true
				}
				return nil
			}
			switch sub {
			case Name("Image"):
				ok, err := markImageUnsafe(v, ref, unsafe)
				if err != nil {
					return err
				}
				if !ok {
					return errUnsafe
				}
			case Name("Form"):
				clipRes, err := resolveFormResources(v, pageRes)
				if err != nil {
					return err
				}
				if clipRes == nil {
					return errUnsafe
				}
				ok, markErr := markResourcesUnsafe(clipRes, unsafe, map[Reference]bool{ref: true}, 0)
				if markErr != nil {
					return markErr
				}
				if !ok {
					return errUnsafe
				}
			}
			return nil
		},
	}
	resolveOptionalInt := func(obj Object) (int64, error) {
		if obj == nil {
			return 0, nil
		}
		val, err := r.Resolve(obj)
		if err != nil {
			if isFatalOptimizationReadError(ctx, err) {
				return 0, err
			}
			return 0, nil
		}
		if n, ok := val.(Integer); ok {
			return int64(n), nil
		}
		return 0, nil
	}
	walkPageAnnotations := func(page *Page, unit float64) error {
		if page.Dictionary["Annots"] == nil {
			return nil
		}
		val, err := r.Resolve(page.Dictionary["Annots"])
		if err != nil {
			if isFatalOptimizationReadError(ctx, err) {
				return err
			}
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
				if isFatalOptimizationReadError(ctx, err) {
					return err
				}
				continue
			}
			annotDict, ok := annotObj.(Dictionary)
			if !ok {
				continue
			}
			rect, rectErr := r.rectangle(annotDict["Rect"])
			if isFatalOptimizationReadError(ctx, rectErr) {
				return rectErr
			}
			rectValid := rectErr == nil
			if annotDict["AP"] != nil {
				apVal, err := r.Resolve(annotDict["AP"])
				if err != nil {
					if isFatalOptimizationReadError(ctx, err) {
						return err
					}
				} else if apDict, ok := apVal.(Dictionary); ok {
					for _, apKey := range []Name{"N", "R", "D"} {
						entryVal, err := r.Resolve(apDict[apKey])
						if err != nil {
							if isFatalOptimizationReadError(ctx, err) {
								return err
							}
							continue
						}
						if entryVal == nil {
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
									if isFatalOptimizationReadError(ctx, err) {
										return err
									}
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
			}
			if err := walkAnnotationMedia(page, unit, rect, rectValid, 0, false, annotDict["Poster"]); err != nil {
				return err
			}
			if annotDict["Movie"] != nil {
				movieVal, err := r.Resolve(annotDict["Movie"])
				if err != nil {
					if isFatalOptimizationReadError(ctx, err) {
						return err
					}
				} else if movieDict, ok := movieVal.(Dictionary); ok && movieDict["Poster"] != nil {
					rotate, err := resolveOptionalInt(movieDict["Rotate"])
					if err != nil {
						return err
					}
					if err := walkAnnotationMedia(page, unit, rect, rectValid, rotate, false, movieDict["Poster"]); err != nil {
						return err
					}
				}
			}
			if annotDict["MK"] != nil {
				mkVal, err := r.Resolve(annotDict["MK"])
				if err != nil {
					if isFatalOptimizationReadError(ctx, err) {
						return err
					}
				} else if mkDict, ok := mkVal.(Dictionary); ok {
					rotate, err := resolveOptionalInt(mkDict["R"])
					if err != nil {
						return err
					}
					unbounded := false
					if mkDict["IF"] != nil {
						ifVal, ifErr := r.Resolve(mkDict["IF"])
						if ifErr != nil {
							if isFatalOptimizationReadError(ctx, ifErr) {
								return ifErr
							}
							unbounded = true
						} else if ifVal != nil {
							ifDict, ok := ifVal.(Dictionary)
							if !ok {
								unbounded = true
							} else if ifDict["SW"] != nil {
								swVal, swErr := r.Resolve(ifDict["SW"])
								if swErr != nil {
									if isFatalOptimizationReadError(ctx, swErr) {
										return swErr
									}
									unbounded = true
								} else if swVal == Name("N") || swVal == Name("S") {
									unbounded = true
								}
							}
						}
					}
					for _, mkKey := range []Name{"I", "RI", "IX"} {
						if err := walkAnnotationMedia(page, unit, rect, rectValid, rotate, unbounded, mkDict[mkKey]); err != nil {
							return err
						}
					}
				}
			}
			if annotDict["A"] != nil {
				if err := mediaWalker.walkAction(annotDict["A"], page.Resources, map[Reference]bool{}, 0); err != nil {
					return err
				}
			}
			if err := mediaWalker.walkAdditionalActions(annotDict["AA"], page.Resources); err != nil {
				return err
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
		if err := mediaWalker.walkAdditionalActions(page.Dictionary["AA"], page.Resources); err != nil {
			return err
		}
		var content bytes.Buffer
		if _, err := page.WriteContent(ctx, &optimizationBuffer{buffer: &content, limit: optimizationBufferLimit}); err != nil {
			if isFatalOptimizationReadError(ctx, err) {
				return err
			}
			ok, markErr := markResourcesUnsafe(page.Resources, unsafe, map[Reference]bool{}, 0)
			if markErr != nil {
				return markErr
			}
			if !ok {
				return errUnsafe
			}
			return nil
		}
		pageBudget := 100000
		if err := walk(content.Bytes(), page.Resources, Matrix{unit, 0, 0, unit, 0, 0}, 0, &pageBudget, nil, unsafe, make(map[uintptr]Dictionary)); err != nil {
			if isFatalOptimizationReadError(ctx, err) {
				return err
			}
			if errors.Is(err, errUnsafe) {
				return errUnsafe
			}
			ok, markErr := markResourcesUnsafe(page.Resources, unsafe, map[Reference]bool{}, 0)
			if markErr != nil {
				return markErr
			}
			if !ok {
				return errUnsafe
			}
			return nil
		}
		return nil
	})
	if err == nil && r.Trailer["Root"] != nil {
		if rootVal, rootErr := r.Resolve(r.Trailer["Root"]); rootErr != nil {
			if isFatalOptimizationReadError(ctx, rootErr) {
				err = rootErr
			}
		} else if catalog, ok := rootVal.(Dictionary); ok {
			err = mediaWalker.walkCatalog(catalog)
		}
	}
	if err != nil {
		if isFatalOptimizationReadError(ctx, err) {
			return nil, err
		}
		return nil, ctx.Err()
	}
	for ref := range unsafe {
		delete(sizes, ref)
	}
	return sizes, ctx.Err()
}

// resolveAnnotationPageResources 解析注解所属页面及其父节点链上可继承的资源字典
// 入参: ctx 取消上下文, anObj 注解对象或引用
// 返回: Dictionary 可继承的资源字典，不可解析时为空, error 致命读取或取消错误
func (r *Reader) resolveAnnotationPageResources(ctx context.Context, anObj Object) (Dictionary, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	anVal, anErr := r.Resolve(anObj)
	if anErr != nil {
		if cErr := ctx.Err(); cErr != nil {
			return nil, cErr
		}
		if isFatalOptimizationReadError(ctx, anErr) {
			return nil, anErr
		}
		return nil, nil
	}
	anDict, ok := anVal.(Dictionary)
	if !ok || anDict["P"] == nil {
		return nil, ctx.Err()
	}
	nodeObj := anDict["P"]
	for step := 0; step < 32 && nodeObj != nil; step++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		pVal, pErr := r.Resolve(nodeObj)
		if pErr != nil {
			if cErr := ctx.Err(); cErr != nil {
				return nil, cErr
			}
			if isFatalOptimizationReadError(ctx, pErr) {
				return nil, pErr
			}
			break
		}
		pDict, ok := pVal.(Dictionary)
		if !ok {
			break
		}
		if pDict["Resources"] != nil {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			rVal, rErr := r.Resolve(pDict["Resources"])
			if rErr != nil {
				if cErr := ctx.Err(); cErr != nil {
					return nil, cErr
				}
				if isFatalOptimizationReadError(ctx, rErr) {
					return nil, rErr
				}
				break
			}
			if res, ok := rVal.(Dictionary); ok {
				return res, ctx.Err()
			}
			break
		}
		nodeObj = pDict["Parent"]
	}
	return nil, ctx.Err()
}

// mediaActionWalker 遍历媒体片段、渲染方案、动作链及目录入口并向回调交付可达流对象
type mediaActionWalker struct {
	reader           *Reader
	ctx              context.Context
	resolveObjectRef func(Object) (Object, Reference, error)
	onStream         func(*Stream, Reference, Dictionary) error
}

// walkMediaClip 递归展开媒体片段字典链并回调底层流对象
// 入参: clipObj 媒体片段或流对象, pageRes 宿主页面资源, visited 已访问引用集合, depth 递归深度
// 返回: error 回调、致命读取或取消错误
func (w mediaActionWalker) walkMediaClip(clipObj Object, pageRes Dictionary, visited map[Reference]bool, depth int) error {
	if clipObj == nil || depth > 32 {
		return nil
	}
	if err := w.ctx.Err(); err != nil {
		return err
	}
	obj, ref, err := w.resolveObjectRef(clipObj)
	if err != nil {
		if isFatalOptimizationReadError(w.ctx, err) {
			return err
		}
		return nil
	}
	if visited == nil {
		visited = map[Reference]bool{}
	}
	if ref.Number > 0 {
		if visited[ref] {
			return nil
		}
		visited[ref] = true
	}
	switch v := obj.(type) {
	case *Stream:
		return w.onStream(v, ref, pageRes)
	case Dictionary:
		return w.walkMediaClip(v["D"], pageRes, visited, depth+1)
	}
	return nil
}

// walkRendition 递归展开渲染方案及其子渲染数组，遍历关联媒体片段
// 入参: rendObj 渲染对象, pageRes 宿主页面资源, visited 已访问引用集合, depth 递归深度
// 返回: error 回调、致命读取或取消错误
func (w mediaActionWalker) walkRendition(rendObj Object, pageRes Dictionary, visited map[Reference]bool, depth int) error {
	if rendObj == nil || depth > 32 {
		return nil
	}
	if err := w.ctx.Err(); err != nil {
		return err
	}
	obj, ref, err := w.resolveObjectRef(rendObj)
	if err != nil {
		if isFatalOptimizationReadError(w.ctx, err) {
			return err
		}
		return nil
	}
	if ref.Number > 0 {
		if visited[ref] {
			return nil
		}
		visited[ref] = true
	}
	dict, ok := obj.(Dictionary)
	if !ok {
		return nil
	}
	if err := w.walkMediaClip(dict["C"], pageRes, nil, 0); err != nil {
		return err
	}
	if dict["R"] != nil {
		arrVal, err := w.reader.Resolve(dict["R"])
		if err != nil {
			if isFatalOptimizationReadError(w.ctx, err) {
				return err
			}
			return nil
		}
		if arr, ok := arrVal.(Array); ok {
			for _, item := range arr {
				if err := w.walkRendition(item, pageRes, visited, depth+1); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

// walkAction 遍历动作及Next链，提取Rendition动作关联的媒体片段并回溯注解页面资源
// 入参: actionObj 动作对象或数组, pageRes 宿主页面资源, visited 已访问引用集合, depth 递归深度
// 返回: error 回调、致命读取或取消错误
func (w mediaActionWalker) walkAction(actionObj Object, pageRes Dictionary, visited map[Reference]bool, depth int) error {
	if actionObj == nil || depth > 32 {
		return nil
	}
	if err := w.ctx.Err(); err != nil {
		return err
	}
	obj, ref, err := w.resolveObjectRef(actionObj)
	if err != nil {
		if isFatalOptimizationReadError(w.ctx, err) {
			return err
		}
		return nil
	}
	if ref.Number > 0 {
		if visited[ref] {
			return nil
		}
		visited[ref] = true
	}
	switch v := obj.(type) {
	case Array:
		for _, item := range v {
			if err := w.walkAction(item, pageRes, visited, depth+1); err != nil {
				return err
			}
		}
	case Dictionary:
		s, err := w.reader.Resolve(v["S"])
		if err != nil {
			if isFatalOptimizationReadError(w.ctx, err) {
				return err
			}
			return nil
		}
		if s == Name("Rendition") && v["R"] != nil {
			fallbackRes := pageRes
			if fallbackRes == nil && v["AN"] != nil {
				var resErr error
				fallbackRes, resErr = w.reader.resolveAnnotationPageResources(w.ctx, v["AN"])
				if resErr != nil {
					return resErr
				}
			}
			if err := w.walkRendition(v["R"], fallbackRes, map[Reference]bool{}, 0); err != nil {
				return err
			}
		}
		if v["Next"] != nil {
			if err := w.walkAction(v["Next"], pageRes, visited, depth+1); err != nil {
				return err
			}
		}
	}
	return nil
}

// walkAdditionalActions 遍历附加动作字典中的全部触发动作
// 入参: aaObj 附加动作字典或引用, pageRes 宿主页面资源
// 返回: error 回调、致命读取或取消错误
func (w mediaActionWalker) walkAdditionalActions(aaObj Object, pageRes Dictionary) error {
	if aaObj == nil {
		return nil
	}
	aaVal, err := w.reader.Resolve(aaObj)
	if err != nil {
		if isFatalOptimizationReadError(w.ctx, err) {
			return err
		}
		return nil
	}
	aaDict, ok := aaVal.(Dictionary)
	if !ok {
		return nil
	}
	for _, entry := range aaDict {
		if err := w.walkAction(entry, pageRes, map[Reference]bool{}, 0); err != nil {
			return err
		}
	}
	return nil
}

// walkCatalog 遍历目录字典中的OpenAction、AA、Names/Renditions名称树及Outlines书签动作
// 入参: catalog 文档目录字典
// 返回: error 回调、致命读取或取消错误
func (w mediaActionWalker) walkCatalog(catalog Dictionary) error {
	if catalog["OpenAction"] != nil {
		if err := w.walkAction(catalog["OpenAction"], nil, map[Reference]bool{}, 0); err != nil {
			return err
		}
	}
	if err := w.walkAdditionalActions(catalog["AA"], nil); err != nil {
		return err
	}
	if catalog["Names"] != nil {
		if namesVal, namesErr := w.reader.Resolve(catalog["Names"]); namesErr != nil {
			if isFatalOptimizationReadError(w.ctx, namesErr) {
				return namesErr
			}
		} else if namesDict, ok := namesVal.(Dictionary); ok && namesDict["Renditions"] != nil {
			var walkRenditionTree func(Object, map[Reference]bool, int) error
			walkRenditionTree = func(nodeObj Object, seen map[Reference]bool, depth int) error {
				if nodeObj == nil || depth > 32 {
					return nil
				}
				if err := w.ctx.Err(); err != nil {
					return err
				}
				obj, ref, nodeErr := w.resolveObjectRef(nodeObj)
				if nodeErr != nil {
					if isFatalOptimizationReadError(w.ctx, nodeErr) {
						return nodeErr
					}
					return nil
				}
				if ref.Number > 0 {
					if seen[ref] {
						return nil
					}
					seen[ref] = true
				}
				dict, ok := obj.(Dictionary)
				if !ok {
					return nil
				}
				if dict["Names"] != nil {
					if pairsVal, pairErr := w.reader.Resolve(dict["Names"]); pairErr != nil {
						if isFatalOptimizationReadError(w.ctx, pairErr) {
							return pairErr
						}
					} else if pairs, ok := pairsVal.(Array); ok {
						for i := 1; i < len(pairs); i += 2 {
							if rErr := w.walkRendition(pairs[i], nil, map[Reference]bool{}, 0); rErr != nil {
								return rErr
							}
						}
					}
				}
				if dict["Kids"] != nil {
					if kidsVal, kidErr := w.reader.Resolve(dict["Kids"]); kidErr != nil {
						if isFatalOptimizationReadError(w.ctx, kidErr) {
							return kidErr
						}
					} else if kids, ok := kidsVal.(Array); ok {
						for _, kid := range kids {
							if kErr := walkRenditionTree(kid, seen, depth+1); kErr != nil {
								return kErr
							}
						}
					}
				}
				return nil
			}
			if err := walkRenditionTree(namesDict["Renditions"], map[Reference]bool{}, 0); err != nil {
				return err
			}
		}
	}
	if catalog["Outlines"] != nil {
		outlineSeen := map[Reference]bool{}
		outlineQueue := []Object{catalog["Outlines"]}
		for i := 0; i < len(outlineQueue) && i < 100000; i++ {
			if cErr := w.ctx.Err(); cErr != nil {
				return cErr
			}
			obj, ref, oErr := w.resolveObjectRef(outlineQueue[i])
			if oErr != nil {
				if isFatalOptimizationReadError(w.ctx, oErr) {
					return oErr
				}
				continue
			}
			if ref.Number > 0 {
				if outlineSeen[ref] {
					continue
				}
				outlineSeen[ref] = true
			}
			dict, ok := obj.(Dictionary)
			if !ok {
				continue
			}
			if dict["A"] != nil {
				if aErr := w.walkAction(dict["A"], nil, map[Reference]bool{}, 0); aErr != nil {
					return aErr
				}
			}
			if dict["First"] != nil {
				outlineQueue = append(outlineQueue, dict["First"])
			}
			if dict["Next"] != nil {
				outlineQueue = append(outlineQueue, dict["Next"])
			}
		}
	}
	return nil
}

// imageDefaultColorResources 在文档存在Default设备色资源时映射各图像引用的默认色空间，多页冲突时拒绝转换
// 入参: ctx 取消上下文, refs 可达对象, metadata 对象元数据快照, aliases 对象别名映射
// 返回: map[Reference]Dictionary 图像到默认色空间资源的映射, map[Reference]bool 存在默认色空间冲突的图像集合, error 取消或致命读取错误
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
		return r.resolveOptimizationObjectRef(value, cached, "cyclic color resource reference")
	}
	var fatalErr error
	extractDefaults := func(res Dictionary) Dictionary {
		if res == nil || res["ColorSpace"] == nil {
			return nil
		}
		val, err := r.Resolve(res["ColorSpace"])
		if err != nil {
			if isFatalOptimizationReadError(ctx, err) {
				fatalErr = err
			}
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
	type colorResVisitKey struct {
		ref    Reference
		resPtr uintptr
	}
	visitedOwn := make(map[Reference]bool)
	visitedInherited := make(map[colorResVisitKey]Dictionary)
	active := make(map[Reference]bool)
	budget := 100000
	overflow := false
	recordImage := func(stream *Stream, ref Reference, defs Dictionary) {
		record(ref, defs)
		_, ok, err := r.walkImageAlternates(ctx, stream, ref, resolveObjectRef, func(altRef Reference) error {
			record(altRef, defs)
			return nil
		})
		if err != nil {
			fatalErr = err
			return
		}
		if !ok {
			overflow = true
		}
	}
	var walkRes func(Dictionary, int)
	walkStreamRes := func(stream *Stream, ref Reference, inheritedRes Dictionary, nextDepth int) {
		if canonical, aliased := aliases[ref]; aliased {
			ref = canonical
		}
		childRes := inheritedRes
		hasOwnRes := false
		if stream.Dictionary["Resources"] != nil {
			rVal, err := r.Resolve(stream.Dictionary["Resources"])
			if err != nil {
				if isFatalOptimizationReadError(ctx, err) {
					fatalErr = err
					return
				}
			} else if rDict, ok := rVal.(Dictionary); ok {
				childRes = rDict
				hasOwnRes = true
			}
		}
		if hasOwnRes {
			if ref.Number > 0 {
				if visitedOwn[ref] {
					return
				}
				visitedOwn[ref] = true
			}
			walkRes(childRes, nextDepth)
			return
		}
		if ref.Number > 0 {
			if active[ref] {
				return
			}
			key := colorResVisitKey{ref: ref, resPtr: reflect.ValueOf(inheritedRes).Pointer()}
			if _, seen := visitedInherited[key]; seen {
				return
			}
			if len(visitedInherited) >= 65536 {
				overflow = true
				return
			}
			visitedInherited[key] = inheritedRes
			active[ref] = true
		}
		walkRes(childRes, nextDepth)
		if ref.Number > 0 {
			delete(active, ref)
		}
	}
	walkRes = func(res Dictionary, depth int) {
		if res == nil || overflow || fatalErr != nil || ctx.Err() != nil {
			return
		}
		if depth > 32 {
			overflow = true
			return
		}
		budget--
		if budget < 0 {
			overflow = true
			return
		}
		defs := extractDefaults(res)
		if fatalErr != nil {
			return
		}
		if res["XObject"] != nil {
			val, err := r.Resolve(res["XObject"])
			if err != nil {
				if isFatalOptimizationReadError(ctx, err) {
					fatalErr = err
					return
				}
			} else if xobjs, ok := val.(Dictionary); ok {
				for _, entry := range xobjs {
					if overflow || fatalErr != nil || ctx.Err() != nil {
						return
					}
					obj, ref, err := resolveObjectRef(entry)
					if err != nil {
						if isFatalOptimizationReadError(ctx, err) {
							fatalErr = err
							return
						}
						continue
					}
					stream, ok := obj.(*Stream)
					if !ok {
						continue
					}
					sub, err := r.Resolve(stream.Dictionary["Subtype"])
					if err != nil {
						if isFatalOptimizationReadError(ctx, err) {
							fatalErr = err
							return
						}
						continue
					}
					switch sub {
					case Name("Image"):
						recordImage(stream, ref, defs)
					case Name("Form"):
						walkStreamRes(stream, ref, res, depth+1)
					}
				}
			}
		}
		if res["Pattern"] != nil {
			val, err := r.Resolve(res["Pattern"])
			if err != nil {
				if isFatalOptimizationReadError(ctx, err) {
					fatalErr = err
					return
				}
			} else if pats, ok := val.(Dictionary); ok {
				for _, entry := range pats {
					if overflow || fatalErr != nil || ctx.Err() != nil {
						return
					}
					obj, ref, err := resolveObjectRef(entry)
					if err != nil {
						if isFatalOptimizationReadError(ctx, err) {
							fatalErr = err
							return
						}
						continue
					}
					stream, ok := obj.(*Stream)
					if !ok {
						continue
					}
					walkStreamRes(stream, ref, res, depth+1)
				}
			}
		}
		if res["Font"] != nil {
			val, err := r.Resolve(res["Font"])
			if err != nil {
				if isFatalOptimizationReadError(ctx, err) {
					fatalErr = err
					return
				}
			} else if fonts, ok := val.(Dictionary); ok {
				for _, entry := range fonts {
					if overflow || fatalErr != nil || ctx.Err() != nil {
						return
					}
					obj, ref, err := resolveObjectRef(entry)
					if err != nil {
						if isFatalOptimizationReadError(ctx, err) {
							fatalErr = err
							return
						}
						continue
					}
					dict, ok := obj.(Dictionary)
					if !ok {
						continue
					}
					sub, err := r.Resolve(dict["Subtype"])
					if err != nil {
						if isFatalOptimizationReadError(ctx, err) {
							fatalErr = err
							return
						}
						continue
					}
					if sub != Name("Type3") {
						continue
					}
					if canonical, aliased := aliases[ref]; aliased {
						ref = canonical
					}
					if ref.Number > 0 && visitedOwn[ref] {
						continue
					}
					if ref.Number > 0 {
						visitedOwn[ref] = true
					}
					if dict["Resources"] == nil {
						continue
					}
					rVal, err := r.Resolve(dict["Resources"])
					if err != nil {
						if isFatalOptimizationReadError(ctx, err) {
							fatalErr = err
							return
						}
						continue
					}
					if rDict, ok := rVal.(Dictionary); ok {
						walkRes(rDict, depth+1)
					}
				}
			}
		}
	}
	walkAPStream := func(pageRes Dictionary, apStream *Stream) {
		apRes := pageRes
		if apStream.Dictionary["Resources"] != nil {
			rVal, err := r.Resolve(apStream.Dictionary["Resources"])
			if err != nil {
				if isFatalOptimizationReadError(ctx, err) {
					fatalErr = err
					return
				}
			} else if rDict, ok := rVal.(Dictionary); ok {
				apRes = rDict
			}
		}
		walkRes(apRes, 0)
	}
	walkMediaStream := func(stream *Stream, ref Reference, pageRes Dictionary) error {
		sub, err := r.Resolve(stream.Dictionary["Subtype"])
		if err != nil {
			if isFatalOptimizationReadError(ctx, err) {
				return err
			}
			return nil
		}
		switch sub {
		case Name("Image"):
			defs := extractDefaults(pageRes)
			if fatalErr != nil {
				return fatalErr
			}
			recordImage(stream, ref, defs)
			return fatalErr
		case Name("Form"):
			walkStreamRes(stream, ref, pageRes, 0)
			return fatalErr
		}
		return nil
	}
	walkMediaObject := func(mediaObj Object, pageRes Dictionary) error {
		if mediaObj == nil {
			return nil
		}
		obj, ref, err := resolveObjectRef(mediaObj)
		if err != nil {
			if isFatalOptimizationReadError(ctx, err) {
				return err
			}
			return nil
		}
		stream, ok := obj.(*Stream)
		if !ok {
			return nil
		}
		return walkMediaStream(stream, ref, pageRes)
	}
	mediaWalker := mediaActionWalker{
		reader:           r,
		ctx:              ctx,
		resolveObjectRef: resolveObjectRef,
		onStream:         walkMediaStream,
	}
	err := r.WalkPages(ctx, func(_ int, page *Page) error {
		defer r.cache.prune(cached)
		defer clear(visitedInherited)
		walkRes(page.Resources, 0)
		if fatalErr != nil {
			return fatalErr
		}
		if page.Dictionary["Annots"] != nil {
			if val, err := r.Resolve(page.Dictionary["Annots"]); err != nil {
				if isFatalOptimizationReadError(ctx, err) {
					return err
				}
			} else if annots, ok := val.(Array); ok {
				for _, item := range annots {
					if err := ctx.Err(); err != nil {
						return err
					}
					annotObj, _, err := resolveObjectRef(item)
					if err != nil {
						if isFatalOptimizationReadError(ctx, err) {
							return err
						}
						continue
					}
					annotDict, ok := annotObj.(Dictionary)
					if !ok {
						continue
					}
					if annotDict["AP"] != nil {
						if apVal, err := r.Resolve(annotDict["AP"]); err != nil {
							if isFatalOptimizationReadError(ctx, err) {
								return err
							}
						} else if apDict, ok := apVal.(Dictionary); ok {
							for _, apKey := range []Name{"N", "R", "D"} {
								entryVal, err := r.Resolve(apDict[apKey])
								if err != nil {
									if isFatalOptimizationReadError(ctx, err) {
										return err
									}
									continue
								}
								if entryVal == nil {
									continue
								}
								switch v := entryVal.(type) {
								case *Stream:
									walkAPStream(page.Resources, v)
									if fatalErr != nil {
										return fatalErr
									}
								case Dictionary:
									for _, stateEntry := range v {
										stateObj, _, err := resolveObjectRef(stateEntry)
										if err != nil {
											if isFatalOptimizationReadError(ctx, err) {
												return err
											}
											continue
										}
										if stateStream, ok := stateObj.(*Stream); ok {
											walkAPStream(page.Resources, stateStream)
											if fatalErr != nil {
												return fatalErr
											}
										}
									}
								}
							}
						}
					}
					if err := walkMediaObject(annotDict["Poster"], page.Resources); err != nil {
						return err
					}
					if annotDict["Movie"] != nil {
						if movieVal, err := r.Resolve(annotDict["Movie"]); err != nil {
							if isFatalOptimizationReadError(ctx, err) {
								return err
							}
						} else if movieDict, ok := movieVal.(Dictionary); ok && movieDict["Poster"] != nil {
							if err := walkMediaObject(movieDict["Poster"], page.Resources); err != nil {
								return err
							}
						}
					}
					if annotDict["MK"] != nil {
						if mkVal, err := r.Resolve(annotDict["MK"]); err != nil {
							if isFatalOptimizationReadError(ctx, err) {
								return err
							}
						} else if mkDict, ok := mkVal.(Dictionary); ok {
							for _, mkKey := range []Name{"I", "RI", "IX"} {
								if err := walkMediaObject(mkDict[mkKey], page.Resources); err != nil {
									return err
								}
							}
						}
					}
					if annotDict["A"] != nil {
						if err := mediaWalker.walkAction(annotDict["A"], page.Resources, map[Reference]bool{}, 0); err != nil {
							return err
						}
					}
					if err := mediaWalker.walkAdditionalActions(annotDict["AA"], page.Resources); err != nil {
						return err
					}
				}
			}
		}
		if err := mediaWalker.walkAdditionalActions(page.Dictionary["AA"], page.Resources); err != nil {
			return err
		}
		return ctx.Err()
	})
	if err != nil && isFatalOptimizationReadError(ctx, err) {
		return nil, nil, err
	}
	if fatalErr != nil {
		return nil, nil, fatalErr
	}
	if r.Trailer["Root"] != nil {
		if rootVal, rootErr := r.Resolve(r.Trailer["Root"]); rootErr != nil {
			if isFatalOptimizationReadError(ctx, rootErr) {
				return nil, nil, rootErr
			}
		} else if catalog, ok := rootVal.(Dictionary); ok {
			if catErr := mediaWalker.walkCatalog(catalog); catErr != nil && isFatalOptimizationReadError(ctx, catErr) {
				return nil, nil, catErr
			}
		}
	}
	if fatalErr != nil {
		return nil, nil, fatalErr
	}
	if overflow {
		clear(result)
		for _, ref := range refs {
			if canonical, aliased := aliases[ref]; aliased {
				ref = canonical
			}
			if ref.Number > 0 {
				conflict[ref] = true
			}
		}
	}
	return result, conflict, ctx.Err()
}

// optimizePDFDecodedImage 解码通用色彩空间图片并执行缩放与有损重编，更小时返回候选流
// 入参: ctx 取消上下文, orig 原始流, baseline 体积比较基准流, w 宽度, h 高度, space 源颜色空间, targetSpace 目标设备色空间, options 压缩配置, size 像素需求, colorResources 可选默认颜色空间资源, ignoreSMask 是否忽略外部软遮罩, sourceJPEG 原流是否为JPEG编码
// 返回: *Stream 更小的候选流或空值, error 取消或编码错误
func (r *Reader) optimizePDFDecodedImage(ctx context.Context, orig, baseline *Stream, w, h Integer, space Object, targetSpace Name, options CompressionOptions, size image.Point, colorResources Dictionary, ignoreSMask, sourceJPEG bool) (*Stream, error) {
	if !pixelsWithinBudget(int64(w), int64(h), optimizationBufferLimit/8) {
		return nil, ctx.Err()
	}
	if lossyColorConflict(space, targetSpace, colorResources) {
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
	b := resized.Bounds()
	if !pixelsWithinBudget(int64(b.Dx()), int64(b.Dy()), optimizationBufferLimit/4) {
		return nil, ctx.Err()
	}
	wantGray := targetSpace == Name("DeviceGray")
	if wantGray && space != Name("DeviceGray") {
		gray, err := imageAllGray(ctx, resized)
		if err != nil {
			return nil, err
		}
		wantGray = gray
	}
	cand, err := r.encodePDFImageLossy(ctx, orig, resized, wantGray, options, sourceJPEG, colorResources)
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
	expectedComp, expectedModel := 3, color.YCbCrModel
	if space == Name("DeviceGray") {
		expectedComp, expectedModel = 1, color.GrayModel
	}
	if !hasDefaultOverride && (space == Name("DeviceRGB") || space == Name("DeviceGray")) && r.isIdentityDecode(dict["Decode"], expectedComp) && dctParams["ColorTransform"] == nil && configErr == nil && config.ColorModel == expectedModel {
		frame, adobe, tagged, frameErr := jpegColorInfoContext(ctx, bestData)
		if frameErr == nil && frame.components == expectedComp && !(frame.components == 3 && tagged && adobe != 1) {
			downscale := size.X > 0 && size.Y > 0 && size.X < config.Width && size.Y < config.Height
			if !downscale {
				sufficient, err := jpegQualitySufficient(ctx, bestData, options.ImageQuality())
				if err != nil {
					return nil, err
				}
				if sufficient {
					return losslessResult, ctx.Err()
				}
			}
			if !pixelsWithinBudget(int64(config.Width), int64(config.Height), optimizationBufferLimit/4) {
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
			cand, err := r.encodePDFImageLossy(ctx, orig, img, space == Name("DeviceGray"), options, true, colorResources)
			if err != nil {
				return nil, err
			}
			if cand != nil && pdfStreamByteSize(ctx, cand) < pdfStreamByteSize(ctx, losslessResult) {
				return cand, ctx.Err()
			}
			return losslessResult, ctx.Err()
		}
	}
	if !hasDefaultOverride && space == Name("DeviceCMYK") && !defaultColorConflict("DeviceRGB", colorResources) {
		invertedCMYK := r.isInvertedCMYKDecodeValue(dict["Decode"])
		if invertedCMYK || r.isIdentityDecode(dict["Decode"], 4) {
			if source, err := r.ReadImage(orig); err == nil {
				jpegData, frame, transform, tagged, err := source.jpegReadTransformContext(ctx, bestData, dctParams)
				if err == nil && frame.components == 4 && pixelsWithinBudget(int64(frame.width), int64(frame.height), optimizationBufferLimit/4) {
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
								cand, err := r.encodePDFImageLossy(ctx, orig, resized, false, options, true, colorResources)
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
		case Name("FlateDecode"), Name("BrotliDecode"), Name("LZWDecode"), Name("ASCII85Decode"), Name("ASCIIHexDecode"), Name("RunLengthDecode"):
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
	limit := optimizationBufferLimit
	if maxBytes, _ := effectiveStreamLimits(r, orig); maxBytes > 0 && maxBytes < int64(limit) {
		limit = int(maxBytes)
	}
	if !hasDefaultOverride && dict["BitsPerComponent"] == Integer(8) && (space == Name("DeviceRGB") || space == Name("DeviceGray")) && r.isIdentityDecode(dict["Decode"], channels) && (len(filters) == 0 || len(filters) == 1 && filters[0] == Name("FlateDecode")) && pixelsWithinBudget(int64(w), int64(h), optimizationBufferLimit/4) && pixelsWithinBudget(int64(w), int64(h), int64(limit)/int64(channels)) {
		pixels := orig.Data
		var decodeErr error
		if len(filters) == 1 {
			input, openErr := zlib.NewReader(bytes.NewReader(orig.Data))
			decodeErr = openErr
			if decodeErr == nil {
				pixels, decodeErr = io.ReadAll(io.LimitReader(&contextInput{ctx: ctx, reader: input}, int64(limit)+1))
				input.Close()
			}
			if decodeErr == nil && len(pixels) <= limit {
				pdict, _ := params[0].(Dictionary)
				pixels, decodeErr = restorePredictorContext(ctx, pixels, pdict)
			}
		}
		if decodeErr == nil && len(pixels) <= limit && int64(len(pixels)) == int64(w)*int64(h)*int64(channels) {
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
			cand, err := r.encodePDFImageLossy(ctx, orig, resized, channels == 1, options, false, colorResources)
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
	matteVal, matteErr := r.optimizationValue(smaskStream.Dictionary["Matte"])
	if matteErr != nil {
		if isFatalOptimizationReadError(ctx, matteErr) {
			return nil, nil, false, matteErr
		}
		baseMain, err := r.optimizeStreamSizeWithContext(ctx, mainStream, losslessOpts, image.Point{}, colorResources, false)
		if err != nil {
			return nil, nil, false, err
		}
		return finish(baseMain, baseSMask, false)
	}
	if matteVal != nil {
		baseMain, err := r.optimizeStreamSizeWithContext(ctx, mainStream, losslessOpts, image.Point{}, colorResources, false)
		if err != nil {
			return nil, nil, false, err
		}
		if !pixelsWithinBudget(int64(mw), int64(mh), optimizationBufferLimit/8) || mainDict["Mask"] != nil || mainDict["ImageMask"] == Boolean(true) {
			return finish(baseMain, baseSMask, false)
		}
		source, err := r.readOptimizationImage(mainStream, colorResources)
		if err != nil {
			return finish(baseMain, baseSMask, false)
		}
		targetSpace, convertible := r.lossyConvertibleColorSpace(source.ColorSpace)
		if !convertible || lossyColorConflict(source.ColorSpace, targetSpace, colorResources) {
			return finish(baseMain, baseSMask, false)
		}
		decoded, err := source.DecodeImageContext(ctx)
		if err != nil {
			return finish(baseMain, baseSMask, false)
		}
		if downscale {
			decoded, err = ResizeImage(ctx, decoded, size)
			if err != nil {
				return nil, nil, false, err
			}
		}
		b := decoded.Bounds()
		if !pixelsWithinBudget(int64(b.Dx()), int64(b.Dy()), optimizationBufferLimit/8) {
			return finish(baseMain, baseSMask, false)
		}
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
		wantGray := targetSpace == Name("DeviceGray")
		if wantGray {
			wantGray, err = imageAllGray(ctx, mainImg)
			if err != nil {
				return nil, nil, false, err
			}
		}
		mainFilters, _, _ := mainStream.filterChain(r)
		sourceJPEG := len(mainFilters) > 0 && mainFilters[len(mainFilters)-1] == Name("DCTDecode")
		candMain, err := r.encodePDFImageLossy(ctx, mainStream, mainImg, wantGray, options, sourceJPEG, colorResources)
		if err != nil {
			return nil, nil, false, err
		}
		if candMain == nil {
			return finish(baseMain, baseSMask, false)
		}
		smaskBase := *smaskStream
		smaskBase.Dictionary = maps.Clone(smaskStream.Dictionary)
		smaskBase.Dictionary["ColorSpace"] = Name("DeviceGray")
		smaskBase.Dictionary["BitsPerComponent"] = Integer(8)
		delete(smaskBase.Dictionary, "Matte")
		delete(smaskBase.Dictionary, "Decode")
		delete(smaskBase.Dictionary, "DecodeParms")
		candSMask, err := r.encodeLosslessPixels(ctx, &smaskBase, smaskStream, maskImg, 1)
		if err != nil {
			return nil, nil, false, err
		}
		if pdfStreamByteSize(ctx, candMain)+pdfStreamByteSize(ctx, candSMask) < pdfStreamByteSize(ctx, baseMain)+pdfStreamByteSize(ctx, baseSMask) {
			return finish(candMain, candSMask, false)
		}
		return finish(baseMain, baseSMask, false)
	}
	if downscale && pixelsWithinBudget(int64(sw), int64(sh), optimizationBufferLimit/4) {
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
					scaledSMask, err := r.encodeLosslessPixels(ctx, &smaskBase, smaskStream, resizedMask, 1)
					if err != nil {
						if !errors.Is(err, io.ErrShortBuffer) {
							return nil, nil, false, err
						}
					} else if pdfStreamByteSize(ctx, scaledMain)+pdfStreamByteSize(ctx, scaledSMask) < pdfStreamByteSize(ctx, mainStream)+pdfStreamByteSize(ctx, baseSMask) {
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
