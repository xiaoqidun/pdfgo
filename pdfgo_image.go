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
	"image"
	"image/color"
	"io"
	"math"

	_ "github.com/mububoki/jpeg2000/j2k"
	_ "github.com/mububoki/jpeg2000/jp2"
	"github.com/xiaoqidun/jbig2"
	"golang.org/x/image/ccitt"
)

var jbig2FileHeader = []byte{0x97, 0x4a, 0x42, 0x32, 0x0d, 0x0a, 0x1a, 0x0a, 3}

// Image 保存图像原始属性，颜色空间和遮罩保持为PDF对象
// Warning非空时允许按声明尺寸读取含多余样本的图像，并报告恢复原因
// Intent为空时继承绘图状态，单独解码使用相对色度
type Image struct {
	Width               int
	Height              int
	BitsPerComponent    int
	ColorSpace          Object
	Intent              Name
	Decode              Array
	ImageMask           bool
	Interpolate         bool
	Mask                Object
	SoftMask            Object
	Stream              *Stream
	Warning             func(Diagnostic)
	reader              *Reader
	effectiveColorSpace bool
}

// HasSoftMask 判断显式及JPEG2000内嵌软遮罩，不将硬遮罩当作软遮罩
// 返回: bool 是否具有软遮罩, error 引用或遮罩参数错误
func (i *Image) HasSoftMask() (bool, error) {
	mode, err := i.jpxMaskMode()
	if err != nil || mode != 0 {
		return mode != 0, err
	}
	value, err := i.reader.Resolve(i.SoftMask)
	return value != nil, err
}

// deviceSampleImage 按需组合设备色或索引色样本与遮罩，不重复展开像素缓冲
type deviceSampleImage struct {
	source    image.Image
	mask      *imageResample
	inverted  bool
	palette   *imagePalette
	maximum   float64
	byteExact bool
	embedded  bool
}

// imagePalette 保存索引色的原始分量和显示颜色
type imagePalette struct {
	space   *ColorSpace
	process *ProcessColorants
	values  [][4]float64
	tints   [][]float64
	colors  []color.NRGBA64
}

// imageResample 在统一图像坐标中采样，不缩减任一轴的原始采样数
type imageResample struct {
	source      image.Image
	bounds      image.Rectangle
	interpolate bool
}

// cmykSample 保存未经过色彩管理的十六位四色分量
type cmykSample [4]uint16

// packedCMYKImage 保留逐行对齐的四色样本，按需无损展开分量
type packedCMYKImage struct {
	data          []byte
	rect          image.Rectangle
	stride, depth int
}

// packedGrayImage 保留低位深灰度或索引样本的逐行紧凑布局
type packedGrayImage struct {
	data   []byte
	rect   image.Rectangle
	stride int
	depth  int
}

// emptySampleImage 保留None图像的声明边界，不分配无效颜色样本
type emptySampleImage struct {
	rect image.Rectangle
}

// ColorModel 返回透明图像的非预乘颜色模型
// 返回: color.Model 颜色模型
func (s *emptySampleImage) ColorModel() color.Model { return color.NRGBA64Model }

// Bounds 返回声明的图像边界
// 返回: image.Rectangle 图像边界
func (s *emptySampleImage) Bounds() image.Rectangle { return s.rect }

// At 返回None色料的零覆盖率
// 入参: x 横坐标, y 纵坐标
// 返回: color.Color 透明颜色
func (s *emptySampleImage) At(x, y int) color.Color { return color.NRGBA64{} }

// Opaque 返回图像没有不透明像素
// 返回: bool 是否完全不透明
func (s *emptySampleImage) Opaque() bool { return false }

// JBIG2File 无损封装可直接复用的单页JBIG2图像及全局段
// 不适合直接复用的图像返回nil，调用DecodeImage可得到已应用颜色与遮罩的像素
// 返回: []byte 完整JBIG2文件, error 解码参数或资源错误
func (i *Image) JBIG2File() ([]byte, error) {
	if i.ImageMask || i.BitsPerComponent != 1 || i.ColorSpace != Name("DeviceGray") || len(i.Decode) != 0 || i.Mask != nil || i.SoftMask != nil || i.Stream.Dictionary["SMaskInData"] != nil || i.Stream.Dictionary["Matte"] != nil {
		return nil, nil
	}
	filter, err := i.reader.Resolve(i.Stream.Dictionary["Filter"])
	if err != nil {
		return nil, err
	}
	if filter != Name("JBIG2Decode") {
		return nil, nil
	}
	var globals []byte
	if i.Stream.Dictionary["DecodeParms"] != nil {
		value, err := i.reader.Resolve(i.Stream.Dictionary["DecodeParms"])
		if err != nil {
			return nil, err
		}
		params, ok := value.(Dictionary)
		if !ok {
			return nil, fmt.Errorf("invalid JBIG2 decode parameters")
		}
		if params["JBIG2Globals"] != nil {
			value, err := i.reader.Resolve(params["JBIG2Globals"])
			if err != nil {
				return nil, err
			}
			stream, ok := value.(*Stream)
			if !ok {
				return nil, fmt.Errorf("invalid JBIG2 globals")
			}
			globals, err = stream.Decode()
			if err != nil {
				return nil, err
			}
		}
	}
	data := make([]byte, 0, len(jbig2FileHeader)+len(globals)+len(i.Stream.Data))
	data = append(data, jbig2FileHeader...)
	data = append(data, globals...)
	data = append(data, i.Stream.Data...)
	config, err := jbig2.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	if config.Width != i.Width || config.Height != i.Height {
		return nil, fmt.Errorf("JBIG2 dimensions differ from image dictionary")
	}
	return data, nil
}

// ReadImage 读取图像XObject描述，不隐式转换颜色或丢弃遮罩
// 入参: object 图像对象或引用
// 返回: *Image 图像描述, error 错误信息
func (r *Reader) ReadImage(object Object) (*Image, error) {
	resolved, err := r.Resolve(object)
	if err != nil {
		return nil, err
	}
	stream, ok := resolved.(*Stream)
	if !ok || stream == nil {
		return nil, fmt.Errorf("expected image XObject")
	}
	kind, err := r.Resolve(stream.Dictionary["Subtype"])
	if err != nil {
		return nil, err
	}
	if kind != Name("Image") {
		return nil, fmt.Errorf("expected image XObject")
	}
	dict := stream.Dictionary
	readInteger := func(key Name) (int64, error) {
		value, err := r.Resolve(dict[key])
		if err != nil {
			return 0, err
		}
		n, ok := value.(Integer)
		if !ok {
			return 0, fmt.Errorf("invalid image %s", key)
		}
		return int64(n), nil
	}
	w, err := readInteger("Width")
	if err != nil {
		return nil, err
	}
	h, err := readInteger("Height")
	if err != nil {
		return nil, err
	}
	if w <= 0 || h <= 0 || uint64(w) > uint64(^uint(0)>>1) || uint64(h) > uint64(^uint(0)>>1) {
		return nil, fmt.Errorf("invalid image dimensions or platform integer overflow")
	}
	maskObject, err := r.Resolve(dict["ImageMask"])
	if err != nil {
		return nil, err
	}
	mask := false
	if maskObject != nil {
		b, ok := maskObject.(Boolean)
		if !ok {
			return nil, fmt.Errorf("invalid image mask flag")
		}
		mask = bool(b)
	}
	bits := int64(1)
	var embeddedSpace Object
	var jpxDepth int
	if !mask {
		jpxDepth, embeddedSpace, err = r.jpxDefaults(stream)
		if err != nil {
			return nil, err
		}
		if jpxDepth != 0 {
			bits = int64(jpxDepth)
		}
	}
	if jpxDepth == 0 && (dict["BitsPerComponent"] != nil || !mask) {
		bits, err = readInteger("BitsPerComponent")
		if err != nil {
			return nil, err
		}
	}
	if bits != 1 && bits != 2 && bits != 4 && bits != 8 && bits != 16 {
		return nil, fmt.Errorf("invalid image component depth")
	}
	if mask && bits != 1 {
		return nil, fmt.Errorf("invalid stencil component depth")
	}
	space, err := r.resolveColorSpace(dict["ColorSpace"])
	if err != nil {
		return nil, err
	}
	if space == nil {
		space = embeddedSpace
	}
	var decodeObject Object
	if jpxDepth == 0 {
		decodeObject, err = r.Resolve(dict["Decode"])
		if err != nil {
			return nil, err
		}
	}
	decode, ok := decodeObject.(Array)
	if decodeObject != nil && !ok {
		return nil, fmt.Errorf("invalid image decode array")
	}
	interpolate, err := r.Resolve(dict["Interpolate"])
	if err != nil {
		return nil, err
	}
	if interpolate != nil && interpolate != Boolean(true) && interpolate != Boolean(false) {
		return nil, fmt.Errorf("invalid image interpolation flag")
	}
	var intent Name
	value, err := r.Resolve(dict["Intent"])
	if err != nil {
		return nil, err
	}
	if value != nil {
		var ok bool
		intent, ok = value.(Name)
		if !ok {
			return nil, fmt.Errorf("invalid image rendering intent")
		}
		intent = normalizeRenderingIntent(intent)
	}
	return &Image{Width: int(w), Height: int(h), BitsPerComponent: int(bits), ColorSpace: space, Intent: intent, Decode: decode, ImageMask: mask, Interpolate: interpolate == Boolean(true), Mask: dict["Mask"], SoftMask: dict["SMask"], Stream: stream, reader: r}, nil
}

// ReadImageWithResources 读取图像并按当前资源解析颜色空间及默认设备色
// 入参: object 图像对象或引用, resources 当前资源字典
// 返回: *Image 图像描述, error 图像或颜色空间错误
func (r *Reader) ReadImageWithResources(object Object, resources Dictionary) (*Image, error) {
	image, err := r.ReadImage(object)
	if err != nil || image.ImageMask {
		return image, err
	}
	image.ColorSpace, err = r.resourceColorSpace(image.ColorSpace, resources)
	if err != nil {
		return nil, err
	}
	image.effectiveColorSpace = true
	return image, nil
}

// renderingIntent 取得图像有效渲染意图，支持直接构造的图像及间接字典值
// 返回: Name 渲染意图, error 解析或类型错误
func (i *Image) renderingIntent() (Name, error) {
	if i.Intent != "" {
		return normalizeRenderingIntent(i.Intent), nil
	}
	value, err := i.reader.Resolve(i.Stream.Dictionary["Intent"])
	if err != nil {
		return "", err
	}
	if value == nil {
		return "RelativeColorimetric", nil
	}
	intent, ok := value.(Name)
	if !ok {
		return "", fmt.Errorf("invalid image rendering intent")
	}
	return normalizeRenderingIntent(intent), nil
}

// DecodeImage 应用Decode映射及颜色空间变换，合成遮罩后返回非预乘透明图像
// 模板图像返回映射后的灰度样本，当前填充色由页面使用方应用
// 异尺寸遮罩按单位方形对齐，输出尺寸取各轴较大的采样数以保留边缘精度
// 返回: image.Image 解码图像, error 错误信息
func (i *Image) DecodeImage() (image.Image, error) {
	return i.DecodeImageContext(context.Background())
}

// DecodeImageContext 解码图像及遮罩，在读取和逐行映射之间检查取消
// 返回图像的后续延迟采样由调用方管理，不持有取消上下文
// 入参: ctx 取消上下文
// 返回: image.Image 解码图像, error 解码或取消错误
func (i *Image) DecodeImageContext(ctx context.Context) (image.Image, error) {
	result, err := i.decodeImage(ctx, nil)
	if canceled := ctx.Err(); canceled != nil {
		return nil, canceled
	}
	return result, err
}

// ColorModel 返回可无损表达原始采样精度的非预乘颜色模型
// 返回: color.Model 颜色模型
func (s *deviceSampleImage) ColorModel() color.Model {
	if s.byteExact {
		return color.NRGBAModel
	}
	return color.NRGBA64Model
}

// Bounds 返回对齐后的图像边界
// 返回: image.Rectangle 图像边界
func (s *deviceSampleImage) Bounds() image.Rectangle { return s.source.Bounds() }

// Opaque 对无透明度来源的图像直接返回结果，其余图像按覆盖率检查
// 返回: bool 是否完全不透明
func (s *deviceSampleImage) Opaque() bool {
	if s.mask == nil {
		opaque := !s.embedded
		if s.embedded {
			if source, ok := s.source.(interface{ Opaque() bool }); ok {
				opaque = source.Opaque()
			}
		}
		if opaque && s.palette != nil {
			for _, pixel := range s.palette.colors {
				if pixel.A != 65535 {
					opaque = false
					break
				}
			}
		}
		if opaque {
			return true
		}
	}
	bounds := s.Bounds()
	for y := bounds.Min.Y; y < bounds.Max.Y; y++ {
		for x := bounds.Min.X; x < bounds.Max.X; x++ {
			if s.NRGBA64At(x, y).A != 65535 {
				return false
			}
		}
	}
	return true
}

// At 按原始采样精度返回非预乘颜色，边界外透明
// 入参: x 横向坐标, y 纵向坐标
// 返回: color.Color 像素颜色
func (s *deviceSampleImage) At(x, y int) color.Color {
	pixel := s.NRGBA64At(x, y)
	if s.byteExact {
		return color.NRGBA{R: uint8(pixel.R >> 8), G: uint8(pixel.G >> 8), B: uint8(pixel.B >> 8), A: uint8(pixel.A >> 8)}
	}
	return pixel
}

// NRGBA64At 直接读取非预乘颜色，避免逐像素接口分配
// 入参: x 横向坐标, y 纵向坐标
// 返回: color.NRGBA64 像素颜色
func (s *deviceSampleImage) NRGBA64At(x, y int) color.NRGBA64 {
	if !image.Pt(x, y).In(s.Bounds()) {
		return color.NRGBA64{}
	}
	pixel := imageNRGBA64At(s.source, x, y)
	return s.combine(pixel, x, y)
}

// combine 将已采样颜色与索引色、内嵌透明度和显式遮罩合成
// 入参: pixel 非预乘颜色, x 横向坐标, y 纵向坐标
// 返回: color.NRGBA64 合成颜色
func (s *deviceSampleImage) combine(pixel color.NRGBA64, x, y int) color.NRGBA64 {
	if !s.embedded {
		pixel.A = 65535
	}
	if s.palette != nil {
		index := int(math.Max(0, math.Min(float64(len(s.palette.colors)-1), math.Round(float64(pixel.R)/65535*s.maximum))))
		alpha := pixel.A
		pixel = s.palette.colors[index]
		if s.embedded {
			pixel.A = uint16(uint32(pixel.A) * uint32(alpha) / 65535)
		}
	}
	alpha := uint32(65535)
	if s.mask != nil {
		gray := imageRGBA64At(s.mask, x, y)
		alpha = uint32(gray.R)
		if s.inverted {
			alpha = 65535 - alpha
		}
	}
	pixel.A = uint16(uint32(pixel.A) * alpha / 65535)
	if s.byteExact {
		pixel = color.NRGBA64{R: pixel.R >> 8 * 257, G: pixel.G >> 8 * 257, B: pixel.B >> 8 * 257, A: pixel.A >> 8 * 257}
	}
	return pixel
}

// ColorModel 返回原始图像的颜色模型
// 返回: color.Model 颜色模型
func (s *imageResample) ColorModel() color.Model { return s.source.ColorModel() }

// Bounds 返回合成图像的采样边界
// 返回: image.Rectangle 采样边界
func (s *imageResample) Bounds() image.Rectangle { return s.bounds }

// At 按像素中心执行最近邻或双线性采样，保留原始设备色和专色分量
// 入参: x 横向坐标, y 纵向坐标
// 返回: color.Color 采样颜色
func (s *imageResample) At(x, y int) color.Color {
	b := s.source.Bounds()
	if b == s.bounds {
		return s.source.At(x, y)
	}
	sx := (float64(x-s.bounds.Min.X)+.5)*float64(b.Dx())/float64(s.bounds.Dx()) - .5
	sy := (float64(y-s.bounds.Min.Y)+.5)*float64(b.Dy())/float64(s.bounds.Dy()) - .5
	if !s.interpolate {
		return s.source.At(b.Min.X+min(b.Dx()-1, max(0, int(math.Floor(sx+.5)))), b.Min.Y+min(b.Dy()-1, max(0, int(math.Floor(sy+.5)))))
	}
	x0, y0 := int(math.Floor(sx)), int(math.Floor(sy))
	fx, fy := sx-float64(x0), sy-float64(y0)
	if _, ok := s.source.At(b.Min.X, b.Min.Y).(imageComponentSample); ok {
		var sample resampledDeviceNSample
		for dy := 0; dy < 2; dy++ {
			for dx := 0; dx < 2; dx++ {
				weightX, weightY := 1-fx, 1-fy
				if dx == 1 {
					weightX = fx
				}
				if dy == 1 {
					weightY = fy
				}
				index := 2*dy + dx
				sample.samples[index] = s.source.At(b.Min.X+min(b.Dx()-1, max(0, x0+dx)), b.Min.Y+min(b.Dy()-1, max(0, y0+dy))).(imageComponentSample)
				sample.weights[index] = weightX * weightY
			}
		}
		return sample
	}
	var values [4]float64
	_, cmyk := imageCMYKSample(s.source.At(b.Min.X, b.Min.Y))
	for dy := 0; dy < 2; dy++ {
		for dx := 0; dx < 2; dx++ {
			weightX, weightY := 1-fx, 1-fy
			if dx == 1 {
				weightX = fx
			}
			if dy == 1 {
				weightY = fy
			}
			pixel := s.source.At(b.Min.X+min(b.Dx()-1, max(0, x0+dx)), b.Min.Y+min(b.Dy()-1, max(0, y0+dy)))
			r, g, b, a := pixel.RGBA()
			if cmyk {
				c, _ := imageCMYKSample(pixel)
				r, g, b, a = uint32(c[0]), uint32(c[1]), uint32(c[2]), uint32(c[3])
			}
			for i, v := range [4]uint32{r, g, b, a} {
				values[i] += float64(v) * weightX * weightY
			}
		}
	}
	if cmyk {
		return cmykSample{uint16(math.Round(values[0])), uint16(math.Round(values[1])), uint16(math.Round(values[2])), uint16(math.Round(values[3]))}
	}
	return color.RGBA64{R: uint16(math.Round(values[0])), G: uint16(math.Round(values[1])), B: uint16(math.Round(values[2])), A: uint16(math.Round(values[3]))}
}

// DecodeSamples 解码原始图像样本，不应用Decode数组、遮罩或色彩管理
// 返回的灰度值用于样本解释，不代表图像已完成页面合成
// CMYK图像还原DCT分量，取消通用JPEG解码器的Adobe反相处理
// 返回: image.Image 样本图像, error 错误信息
func (i *Image) DecodeSamples() (image.Image, error) {
	return i.DecodeSamplesContext(context.Background())
}

// DecodeSamplesContext 解码原始样本，在流读取及样本处理之间检查取消
// JBIG2及JPEG2000解码器的内部运算完成后才检查取消
// 入参: ctx 取消上下文
// 返回: image.Image 独立原始样本, error 解码或取消错误
func (i *Image) DecodeSamplesContext(ctx context.Context) (image.Image, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if i.Width <= 0 || i.Height <= 0 {
		return nil, fmt.Errorf("invalid image dimensions")
	}
	filters, parameters, err := i.Stream.filterChain(i.reader)
	if err != nil {
		return nil, err
	}
	terminal := Name("")
	var terminalParams Dictionary
	if len(filters) > 0 {
		last := filters[len(filters)-1]
		if last == Name("DCTDecode") || last == Name("JBIG2Decode") || last == Name("JPXDecode") || last == Name("CCITTFaxDecode") {
			terminal = last.(Name)
			if parameters[len(filters)-1] != nil {
				var ok bool
				terminalParams, ok = parameters[len(filters)-1].(Dictionary)
				if !ok {
					return nil, fmt.Errorf("invalid image decode parameters")
				}
			}
			filters = filters[:len(filters)-1]
			parameters = parameters[:len(parameters)-1]
		}
	}
	data, err := (&Stream{Dictionary: Dictionary{"Filter": filters, "DecodeParms": parameters}, Data: i.Stream.Data}).DecodeContext(ctx)
	if err != nil {
		return nil, err
	}
	var result image.Image
	switch terminal {
	case "CCITTFaxDecode":
		result, err = i.ccittSamplesContext(ctx, data, terminalParams)
	case "DCTDecode":
		result, err = i.jpegSamplesContext(ctx, data, terminalParams)
		if err != nil {
			return nil, err
		}
	case "JBIG2Decode":
		if i.BitsPerComponent != 1 {
			return nil, fmt.Errorf("invalid JBIG2 component depth")
		}
		if _, _, err := imageBufferSize(i.Width, i.Height, 1); err != nil {
			return nil, err
		}
		var globals []byte
		if terminalParams["JBIG2Globals"] != nil {
			object, err := i.reader.Resolve(terminalParams["JBIG2Globals"])
			if err != nil {
				return nil, err
			}
			stream, ok := object.(*Stream)
			if !ok {
				return nil, fmt.Errorf("invalid JBIG2 globals")
			}
			globals, err = stream.DecodeContext(ctx)
			if err != nil {
				return nil, err
			}
		}
		decoder, err := jbig2.NewDecoderWithGlobals(&contextInput{ctx: ctx, reader: bytes.NewReader(data)}, globals)
		if err != nil {
			return nil, err
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		result, err = decoder.Decode()
		if err != nil {
			return nil, err
		}
	case "JPXDecode":
		cmyk := i.ColorSpace == Name("DeviceCMYK")
		result, err = i.jpxSamples(data, cmyk)
	default:
		result, err = i.rawSamples(ctx, data)
	}
	if canceled := ctx.Err(); canceled != nil {
		return nil, canceled
	}
	if err != nil {
		return nil, err
	}
	if result.Bounds().Dx() != i.Width || result.Bounds().Dy() != i.Height {
		return nil, fmt.Errorf("decoded image dimensions differ from dictionary")
	}
	return result, nil
}

// RGBA 返回不透明的设备色预览，原始分量由图像解码流程解释
// 返回: uint32 红、绿、蓝及透明度
func (c cmykSample) RGBA() (uint32, uint32, uint32, uint32) {
	k := uint64(65535 - c[3])
	return uint32(uint64(65535-c[0]) * k / 65535), uint32(uint64(65535-c[1]) * k / 65535), uint32(uint64(65535-c[2]) * k / 65535), 65535
}

// ColorModel 返回保留十六位四色样本的颜色模型
// 返回: color.Model 颜色模型
func (s *packedCMYKImage) ColorModel() color.Model {
	return color.ModelFunc(func(c color.Color) color.Color {
		if sample, ok := imageCMYKSample(c); ok {
			return sample
		}
		r, g, b, _ := c.RGBA()
		white := max(r, g, b)
		if white == 0 {
			return cmykSample{0, 0, 0, 65535}
		}
		return cmykSample{uint16((white - r) * 65535 / white), uint16((white - g) * 65535 / white), uint16((white - b) * 65535 / white), uint16(65535 - white)}
	})
}

// Bounds 返回原始样本边界
// 返回: image.Rectangle 图像边界
func (s *packedCMYKImage) Bounds() image.Rectangle { return s.rect }

// At 读取逐行打包的四色分量，边界外返回零透明度
// 入参: x 横向坐标, y 纵向坐标
// 返回: color.Color 样本颜色
func (s *packedCMYKImage) At(x, y int) color.Color {
	if !image.Pt(x, y).In(s.rect) {
		return color.RGBA64{}
	}
	return s.sample(x, y)
}

// sample 读取已校验边界内的打包四色，供解码循环复用而不创建颜色接口
// 入参: x 横向坐标, y 纵向坐标
// 返回: cmykSample 原始四色分量
func (s *packedCMYKImage) sample(x, y int) cmykSample {
	line := s.data[(y-s.rect.Min.Y)*s.stride:]
	var sample cmykSample
	for c := range sample {
		value := uint32(packedSample(line, (x-s.rect.Min.X)*4+c, s.depth))
		sample[c] = uint16(value * 65535 / ((uint32(1) << s.depth) - 1))
	}
	return sample
}

// ColorModel 返回十六位灰度模型
// 返回: color.Model 颜色模型
func (s *packedGrayImage) ColorModel() color.Model { return color.Gray16Model }

// Bounds 返回原始采样边界
// 返回: image.Rectangle 图像边界
func (s *packedGrayImage) Bounds() image.Rectangle { return s.rect }

// At 将单个紧凑样本无损展开为十六位灰度，边界外返回零值
// 入参: x 横向坐标, y 纵向坐标
// 返回: color.Color 灰度样本
func (s *packedGrayImage) At(x, y int) color.Color {
	if !image.Pt(x, y).In(s.rect) {
		return color.Gray16{}
	}
	line := s.data[(y-s.rect.Min.Y)*s.stride:]
	value := uint32(packedSample(line, x-s.rect.Min.X, s.depth))
	return color.Gray16{Y: uint16(value * 65535 / ((uint32(1) << s.depth) - 1))}
}

// decodeImage 共用解码、遮罩和预混合恢复流程，按需保留颜色分量
// 入参: ctx 取消上下文, target 可选分量输出
// 返回: image.Image 显示图像，分量模式下为空, error 解码错误
func (i *Image) decodeImage(ctx context.Context, target *ImageComponents) (image.Image, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if i.Width <= 0 || i.Height <= 0 {
		return nil, fmt.Errorf("invalid image dimensions")
	}
	if target == nil && !i.ImageMask {
		none, err := i.reader.colorSpaceNone(i.ColorSpace, i.effectiveColorSpace)
		if err != nil {
			return nil, err
		}
		if none {
			return &emptySampleImage{rect: image.Rect(0, 0, i.Width, i.Height)}, nil
		}
	}
	palette, err := i.palette()
	if err != nil {
		return nil, err
	}
	components := 0
	var calibrated *calRGBSpace
	var lab *labSpace
	var profile *iccColorSpace
	var separation *separationSpace
	var deviceN *deviceNSpace
	switch i.ColorSpace {
	case Name("DeviceGray"):
		components = 1
	case Name("DeviceRGB"):
		components = 3
	case Name("DeviceCMYK"):
		components = 4
	}
	if space, ok := i.ColorSpace.(Array); ok && len(space) == 2 && space[0] == Name("CalRGB") {
		calibrated, err = i.reader.readCalRGB(space)
		if err != nil {
			return nil, err
		}
		components = 3
	}
	if space, ok := i.ColorSpace.(Array); ok && len(space) == 2 && space[0] == Name("Lab") {
		lab, err = i.reader.readLab(space)
		if err != nil {
			return nil, err
		}
		components = 3
	}
	if space, ok := i.ColorSpace.(Array); ok && len(space) == 2 && space[0] == Name("CalGray") {
		calibrated, err = i.reader.readCalGray(space)
		if err != nil {
			return nil, err
		}
		components = 1
	}
	if space, ok := i.ColorSpace.(Array); ok && len(space) == 2 && space[0] == Name("ICCBased") {
		profile, err = i.reader.readICCSourceSpace(space, i.effectiveColorSpace)
		if err != nil {
			return nil, err
		}
		components = profile.components()
	}
	if space, ok := i.ColorSpace.(Array); ok && len(space) == 4 && space[0] == Name("Separation") {
		separation, err = i.reader.readSeparationSpace(space, i.effectiveColorSpace, 0)
		if err != nil {
			return nil, err
		}
		components = 1
	}
	if i.ImageMask {
		components = 1
	}
	if space, ok := i.ColorSpace.(Array); ok && len(space) > 0 && space[0] == Name("DeviceN") {
		deviceN, err = i.reader.readDeviceNSpace(space, i.effectiveColorSpace, 0)
		if err != nil {
			return nil, err
		}
		components = deviceN.components
	}
	if palette != nil {
		components = 1
	}
	if components == 0 {
		return nil, &UnsupportedError{Feature: "image color space"}
	}
	embeddedMask, err := i.jpxMaskMode()
	if err != nil {
		return nil, err
	}
	intent, err := i.renderingIntent()
	if err != nil {
		return nil, err
	}
	ranges := make([]float64, components*2)
	for c := 0; c < components; c++ {
		ranges[c*2+1] = 1
	}
	if lab != nil {
		copy(ranges, []float64{0, 100, lab.rangeAB[0], lab.rangeAB[1], lab.rangeAB[2], lab.rangeAB[3]})
	}
	if profile != nil {
		bounds := profile.sourceRanges()
		copy(ranges, bounds[:components*2])
	}
	limits := append([]float64(nil), ranges...)
	if palette != nil {
		ranges[1] = float64((uint32(1) << i.BitsPerComponent) - 1)
	}
	defaultDecode := true
	decode := i.Decode
	if len(decode) != 0 && !i.ImageMask {
		filters, _, err := i.Stream.filterChain(i.reader)
		if err != nil {
			return nil, err
		}
		if len(filters) != 0 && filters[len(filters)-1] == Name("JPXDecode") {
			decode = nil
		}
	}
	if len(decode) != 0 {
		if len(decode) != len(ranges) {
			return nil, fmt.Errorf("invalid image Decode array")
		}
		for n, value := range decode {
			v, err := i.reader.number(value)
			if err != nil || math.IsNaN(v) || math.IsInf(v, 0) {
				return nil, fmt.Errorf("invalid image Decode value")
			}
			defaultDecode = defaultDecode && v == ranges[n]
			ranges[n] = v
		}
	}
	samples, err := i.DecodeSamplesContext(ctx)
	if err != nil {
		return nil, err
	}
	var mask *imageResample
	var keys, matte []float64
	var inverted bool
	if embeddedMask == 0 {
		mask, keys, inverted, matte, err = i.decodeMask(ctx, components)
		if err != nil {
			return nil, err
		}
	}
	if embeddedMask != 0 {
		if embeddedMask == 2 {
			matte = make([]float64, components)
			if value := i.Stream.Dictionary["Matte"]; value != nil {
				matte, err = i.reader.numberArray(value, components)
				if err != nil {
					return nil, fmt.Errorf("invalid JPEG2000 Matte: %w", err)
				}
			}
		}
	}
	bounds := samples.Bounds()
	if mask != nil {
		bounds = image.Rect(0, 0, max(bounds.Dx(), mask.Bounds().Dx()), max(bounds.Dy(), mask.Bounds().Dy()))
	}
	if target == nil && i.ColorSpace == Name("DeviceGray") && !i.ImageMask && embeddedMask == 0 && mask == nil && len(keys) == 0 && len(matte) == 0 {
		gray, err := decodeGrayImageContext(ctx, samples, ranges[0], ranges[1])
		if err != nil {
			return nil, err
		}
		if gray != nil {
			return gray, nil
		}
	}
	if samples.Bounds() != bounds {
		switch samples.(type) {
		case *image.NRGBA, *image.NRGBA64:
			samples = &deviceSampleImage{source: samples, byteExact: imageByteExact(samples)}
		}
		samples = &imageResample{source: samples, bounds: bounds, interpolate: i.Interpolate && palette == nil}
	}
	if mask != nil {
		mask.bounds = bounds
	}
	if target == nil && calibrated == nil && lab == nil && profile == nil && separation == nil && deviceN == nil && components != 4 && len(keys) == 0 && len(matte) == 0 && defaultDecode {
		byteExact := imageByteExact(samples)
		if mask != nil {
			byteExact = byteExact && imageByteExact(mask)
		}
		if palette != nil {
			for _, c := range palette.colors {
				if c.R%257 != 0 || c.G%257 != 0 || c.B%257 != 0 || c.A%257 != 0 {
					byteExact = false
					break
				}
			}
		}
		return &deviceSampleImage{source: samples, mask: mask, inverted: inverted, palette: palette, maximum: float64((uint32(1) << i.BitsPerComponent) - 1), byteExact: byteExact, embedded: embeddedMask != 0}, nil
	}
	var out *image.NRGBA64
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if target == nil {
		if _, _, err := imageBufferSize(bounds.Dx(), bounds.Dy(), 8); err != nil {
			return nil, err
		}
		out = image.NewNRGBA64(bounds)
	} else {
		if palette != nil {
			target.Space = palette.space
			target.Process = palette.process
		} else if deviceN != nil {
			target.Space = deviceN.alternate
			target.Process = deviceN.process
		} else if separation != nil {
			target.Space = separation.space
		} else if profile != nil {
			target.Space = &ColorSpace{Model: map[int]Name{1: "DeviceGray", 3: "DeviceRGB", 4: "DeviceCMYK"}[components], profile: profile}
			if profile.alternate != nil {
				paint, err := profile.paint(make([]float64, components), intent)
				if err != nil {
					return nil, err
				}
				target.Space = paint.Space
				if target.Space == nil {
					target.Space = &ColorSpace{Model: "DeviceRGB", mapped: true}
				}
			}
		} else if calibrated != nil {
			target.Space, err = i.reader.readCalibratedSpace(i.ColorSpace.(Array))
			if err != nil {
				return nil, err
			}
		} else if lab != nil {
			target.Space = &ColorSpace{Model: "DeviceRGB", mapped: true}
		} else {
			target.Space = &ColorSpace{Model: map[int]Name{1: "DeviceGray", 3: "DeviceRGB", 4: "DeviceCMYK"}[components]}
		}
		if target.Space == nil {
			return nil, &UnsupportedError{Feature: "image source color components"}
		}
		target.Rect = bounds
		target.Colorants, err = i.Colorants()
		if err != nil {
			return nil, err
		}
		stride := target.Space.Components() + 1
		_, size, err := imageSampleSize(bounds.Dx(), bounds.Dy(), stride, 16)
		if err != nil {
			return nil, err
		}
		if len(target.Colorants) > 0 {
			_, tintSize, err := imageSampleSize(bounds.Dx(), bounds.Dy(), len(target.Colorants), 16)
			if err != nil {
				return nil, err
			}
			target.Tints = make([]uint16, tintSize/2)
		}
		target.Pix = make([]uint16, size/2)
	}
	maximum := float64((uint32(1) << i.BitsPerComponent) - 1)
	var componentBuffer [4]float64
	inputs := componentBuffer[:min(components, len(componentBuffer))]
	if components > len(componentBuffer) {
		inputs = make([]float64, components)
	}
	for y := 0; y < bounds.Dy(); y++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		for x := 0; x < bounds.Dx(); x++ {
			alpha := uint16(65535)
			if mask != nil {
				value, _, _, _ := mask.At(x, y).RGBA()
				if inverted {
					value = 65535 - value
				}
				alpha = uint16(value)
			}
			var sampleColor color.Color
			if components != 4 || embeddedMask != 0 {
				sampleColor = samples.At(x, y)
			}
			if embeddedMask != 0 {
				_, _, _, value := sampleColor.RGBA()
				alpha = uint16(value)
			}
			var values [4]float64
			if components > 4 {
				sample, ok := sampleColor.(imageComponentSample)
				if !ok {
					return nil, fmt.Errorf("invalid DeviceN image samples")
				}
				for c := range inputs {
					inputs[c] = float64(sample.component(c)) / 65535
				}
			} else if components == 4 {
				var cmyk cmykSample
				var ok bool
				if embeddedMask == 0 {
					cmyk, ok = imageCMYKAt(samples, x, y)
				} else {
					cmyk, ok = imageCMYKSample(sampleColor)
				}
				if !ok {
					return nil, fmt.Errorf("invalid CMYK image samples")
				}
				for c := range cmyk {
					values[c] = float64(cmyk[c]) / 65535
				}
			} else {
				pixel := imageNRGBASample(sampleColor)
				values = [4]float64{float64(pixel.R) / 65535, float64(pixel.G) / 65535, float64(pixel.B) / 65535}
			}
			if components <= 4 {
				copy(inputs, values[:components])
			}
			transparent := len(keys) != 0
			for c := 0; c < components; c++ {
				if transparent {
					sample := math.Round(inputs[c] * maximum)
					transparent = sample >= keys[c*2] && sample <= keys[c*2+1]
				}
				inputs[c] = functionValue(inputs[c], ranges[c*2], ranges[c*2+1])
				if palette == nil {
					inputs[c] = math.Max(limits[c*2], math.Min(limits[c*2+1], inputs[c]))
				}
				if len(matte) != 0 {
					if alpha == 0 {
						inputs[c] = matte[c]
					} else {
						inputs[c] = math.Max(limits[c*2], math.Min(limits[c*2+1], matte[c]+(inputs[c]-matte[c])*65535/float64(alpha)))
					}
				}
			}
			copy(values[:], inputs)
			if profile != nil {
				values = profile.normalize(values[:components])
			}
			if components == 1 {
				values[1], values[2] = values[0], values[0]
			}
			if target != nil && len(target.Colorants) > 0 {
				tints := inputs
				if palette != nil {
					index := int(math.Max(0, math.Min(float64(len(palette.tints)-1), math.Round(values[0]))))
					tints = palette.tints[index]
				}
				offset := (y*bounds.Dx() + x) * len(target.Colorants)
				for c := range target.Colorants {
					target.Tints[offset+c] = uint16(math.Round(math.Max(0, math.Min(1, tints[c])) * 65535))
				}
			}
			if deviceN != nil {
				values, err = deviceN.values(inputs...)
				if err != nil {
					return nil, err
				}
			}
			if target != nil {
				if profile != nil && profile.alternate != nil {
					paint, err := profile.paint(inputs, intent)
					if err != nil {
						return nil, err
					}
					values = paint.Values
					if paint.Space == nil {
						copy(values[:], paint.RGB[:])
					}
					if paint.None {
						alpha = 0
					}
				}
				if palette != nil {
					index := int(math.Max(0, math.Min(float64(len(palette.values)-1), math.Round(values[0]))))
					values = palette.values[index]
					alpha = uint16(uint32(alpha) * uint32(palette.colors[index].A) / 65535)
				} else if lab != nil {
					pixel := lab.color(values[0], values[1], values[2])
					values = [4]float64{float64(pixel.R) / 65535, float64(pixel.G) / 65535, float64(pixel.B) / 65535}
				} else if separation != nil {
					values, err = separation.values(values[0])
					if err != nil {
						return nil, err
					}
					if separation.name == "None" || separation.none {
						alpha = 0
					}
				}
				channels := target.Space.Components()
				offset := (y*bounds.Dx() + x) * (channels + 1)
				for c := 0; c < channels; c++ {
					target.Pix[offset+c] = uint16(math.Round(values[c] * 65535))
				}
				if transparent || deviceN != nil && deviceN.none {
					alpha = 0
				}
				target.Pix[offset+channels] = alpha
				continue
			}
			var pixel color.NRGBA64
			if palette != nil {
				index := int(math.Max(0, math.Min(float64(len(palette.colors)-1), math.Round(values[0]))))
				pixel = palette.colors[index]
			} else if deviceN != nil {
				rgb, err := deviceN.alternate.RGB(values[:deviceN.alternate.Components()], intent)
				if err != nil {
					return nil, err
				}
				pixel = color.NRGBA64{R: uint16(math.Round(rgb[0] * 65535)), G: uint16(math.Round(rgb[1] * 65535)), B: uint16(math.Round(rgb[2] * 65535)), A: 65535}
				if deviceN.none {
					pixel.A = 0
				}
			} else if separation != nil {
				paint, err := separation.paint(values[0], intent)
				if err != nil {
					return nil, err
				}
				if paint.CMYK != nil {
					rgb := deviceCMYKRGB(paint.CMYK[:])
					pixel = color.NRGBA64{R: uint16(math.Round(rgb[0] * 65535)), G: uint16(math.Round(rgb[1] * 65535)), B: uint16(math.Round(rgb[2] * 65535)), A: 65535}
				} else {
					pixel = color.NRGBA64{R: uint16(math.Round(paint.RGB[0] * 65535)), G: uint16(math.Round(paint.RGB[1] * 65535)), B: uint16(math.Round(paint.RGB[2] * 65535)), A: 65535}
				}
				if separation.name == "None" || separation.none {
					pixel.A = 0
				}
			} else if profile != nil {
				converted, err := profile.color(values[:components], intent)
				if err != nil {
					return nil, err
				}
				pixel = color.NRGBA64{R: uint16(math.Round(converted[0] * 65535)), G: uint16(math.Round(converted[1] * 65535)), B: uint16(math.Round(converted[2] * 65535)), A: 65535}
				if profile.alternate != nil && profile.alternate.invisible {
					pixel.A = 0
				}
			} else if calibrated != nil {
				pixel = calibrated.color(values[0], values[1], values[2])
			} else if lab != nil {
				pixel = lab.color(values[0], values[1], values[2])
			} else if components == 4 {
				rgb := deviceCMYKRGB(values[:])
				pixel = color.NRGBA64{R: uint16(math.Round(rgb[0] * 65535)), G: uint16(math.Round(rgb[1] * 65535)), B: uint16(math.Round(rgb[2] * 65535)), A: 65535}
			} else {
				pixel = color.NRGBA64{R: uint16(math.Round(values[0] * 65535)), G: uint16(math.Round(values[1] * 65535)), B: uint16(math.Round(values[2] * 65535)), A: 65535}
			}
			if transparent {
				pixel.A = 0
			}
			if mask != nil || embeddedMask != 0 {
				pixel.A = uint16(uint32(pixel.A) * uint32(alpha) / 65535)
			}
			out.SetNRGBA64(x, y, pixel)
		}
	}
	if out == nil {
		return nil, nil
	}
	return out, nil
}

// imageByteExact 判断样本及重采样结果是否可由八位分量无损表达
// 入参: source 样本图像
// 返回: bool 是否可无损表达
func imageByteExact(source image.Image) bool {
	switch s := source.(type) {
	case *image.NRGBA:
		return s.Opaque()
	case *image.RGBA, *image.Gray, *image.YCbCr:
		return true
	case *packedGrayImage:
		return s.depth <= 8
	case *mappedGrayImage:
		return s.byteExact
	case *deviceSampleImage:
		return s.byteExact
	case *jpxSampleImage:
		if s.ycc {
			return false
		}
		for _, n := range s.channels {
			bits := s.precision(n)
			if bits != 1 && bits != 2 && bits != 4 && bits != 8 {
				return false
			}
		}
		if s.alpha >= 0 {
			bits := s.precision(s.alpha)
			return bits == 1 || bits == 2 || bits == 4 || bits == 8
		}
		return true
	case *imageResample:
		return (!s.interpolate || s.source.Bounds() == s.bounds) && imageByteExact(s.source)
	default:
		return false
	}
}

// palette 解析索引色查找表，保留原始索引样本供Decode和色键遮罩使用
// 返回: *imagePalette 调色板，非索引色时为空, error 错误信息
func (i *Image) palette() (*imagePalette, error) {
	array, ok := i.ColorSpace.(Array)
	if !ok {
		return nil, nil
	}
	if len(array) > 0 && array[0] == Name("DeviceN") {
		return nil, nil
	}
	if len(array) == 2 && (array[0] == Name("CalRGB") || array[0] == Name("CalGray") || array[0] == Name("Lab") || array[0] == Name("ICCBased")) || len(array) == 4 && array[0] == Name("Separation") {
		return nil, nil
	}
	if len(array) != 4 || array[0] != Name("Indexed") {
		return nil, &UnsupportedError{Feature: "image color space"}
	}
	base, err := i.reader.resolveColorSpace(array[1])
	if err != nil {
		return nil, err
	}
	components := 0
	if a, ok := base.(Array); ok && len(a) == 1 {
		base = a[0]
	}
	var calibrated *calRGBSpace
	var profile *iccColorSpace
	var deviceN *deviceNSpace
	var separation *separationSpace
	var lab *labSpace
	switch base {
	case Name("DeviceGray"):
		components = 1
	case Name("DeviceRGB"):
		components = 3
	case Name("DeviceCMYK"):
		components = 4
	default:
		if space, ok := base.(Array); ok && len(space) > 0 && space[0] == Name("Separation") {
			separation, err = i.reader.readSeparationSpace(space, i.effectiveColorSpace, 0)
			if err != nil {
				return nil, err
			}
			components = 1
		} else if space, ok := base.(Array); ok && len(space) > 0 && space[0] == Name("DeviceN") {
			deviceN, err = i.reader.readDeviceNSpace(space, i.effectiveColorSpace, 0)
			if err != nil {
				return nil, err
			}
			components = deviceN.components
		} else if space, ok := base.(Array); ok && len(space) == 2 && space[0] == Name("ICCBased") {
			profile, err = i.reader.readICCSourceSpace(space, i.effectiveColorSpace)
			if err != nil {
				return nil, err
			}
			components = profile.components()
		} else if space, ok := base.(Array); ok && len(space) == 2 && space[0] == Name("Lab") {
			lab, err = i.reader.readLab(space)
			if err != nil {
				return nil, err
			}
			components = 3
		} else if space, ok := base.(Array); ok && len(space) == 2 && space[0] == Name("CalGray") {
			calibrated, err = i.reader.readCalGray(space)
			if err != nil {
				return nil, err
			}
			components = 1
		} else {
			calibrated, err = i.reader.readCalRGB(base)
			if err != nil {
				return nil, err
			}
			components = 3
		}
	}
	high, err := i.reader.Resolve(array[2])
	if err != nil {
		return nil, err
	}
	n, ok := high.(Integer)
	if !ok || n < 0 || n > 255 {
		return nil, fmt.Errorf("invalid Indexed high value")
	}
	lookup, err := i.reader.Resolve(array[3])
	if err != nil {
		return nil, err
	}
	var data []byte
	switch value := lookup.(type) {
	case String:
		data = value
	case *Stream:
		data, err = value.Decode()
		if err != nil {
			return nil, err
		}
	default:
		return nil, fmt.Errorf("invalid Indexed lookup")
	}
	if len(data) != int(n+1)*components {
		return nil, fmt.Errorf("invalid Indexed lookup length")
	}
	palette := make([]color.NRGBA64, int(n+1))
	result := &imagePalette{colors: palette, values: make([][4]float64, len(palette))}
	if deviceN != nil || separation != nil {
		result.tints = make([][]float64, len(palette))
	}
	if deviceN != nil {
		result.space = deviceN.alternate
		result.process = deviceN.process
	} else if separation != nil {
		result.space = separation.space
	} else if calibrated != nil {
		result.space, err = i.reader.readCalibratedSpace(base.(Array))
		if err != nil {
			return nil, err
		}
	} else if lab != nil {
		result.space = &ColorSpace{Model: "DeviceRGB", mapped: true}
	} else {
		result.space = &ColorSpace{Model: map[int]Name{1: "DeviceGray", 3: "DeviceRGB", 4: "DeviceCMYK"}[components], profile: profile}
	}
	intent, err := i.renderingIntent()
	if err != nil {
		return nil, err
	}
	if profile != nil && profile.alternate != nil {
		paint, err := profile.paint(make([]float64, components), intent)
		if err != nil {
			return nil, err
		}
		result.space = paint.Space
		if result.space == nil {
			result.space = &ColorSpace{Model: "DeviceRGB", mapped: true}
		}
	}
	for index := range palette {
		v := data[index*components:]
		for c := 0; c < min(components, 4); c++ {
			result.values[index][c] = float64(v[c]) / 255
		}
		if separation != nil {
			result.tints[index] = append([]float64(nil), result.values[index][:1]...)
			paint, err := separation.paint(result.values[index][0], intent)
			if err != nil {
				return nil, err
			}
			rgb := paint.RGB
			if paint.CMYK != nil {
				rgb = deviceCMYKRGB(paint.CMYK[:])
			}
			result.values[index], err = separation.values(result.tints[index][0])
			if err != nil {
				return nil, err
			}
			alpha := uint16(65535)
			if separation.name == "None" || separation.none {
				alpha = 0
			}
			palette[index] = color.NRGBA64{R: uint16(math.Round(rgb[0] * 65535)), G: uint16(math.Round(rgb[1] * 65535)), B: uint16(math.Round(rgb[2] * 65535)), A: alpha}
			continue
		}
		if lab != nil {
			values := [4]float64{float64(v[0]) * 100 / 255, lab.rangeAB[0] + float64(v[1])*(lab.rangeAB[1]-lab.rangeAB[0])/255, lab.rangeAB[2] + float64(v[2])*(lab.rangeAB[3]-lab.rangeAB[2])/255}
			palette[index] = lab.color(values[0], values[1], values[2])
			pixel := palette[index]
			result.values[index] = [4]float64{float64(pixel.R) / 65535, float64(pixel.G) / 65535, float64(pixel.B) / 65535}
			continue
		}
		if deviceN != nil {
			result.tints[index] = make([]float64, components)
			for c := range result.tints[index] {
				result.tints[index][c] = float64(v[c]) / 255
			}
			values, err := deviceN.values(result.tints[index]...)
			if err != nil {
				return nil, err
			}
			result.values[index] = values
			rgb, err := deviceN.alternate.RGB(values[:deviceN.alternate.Components()], intent)
			if err != nil {
				return nil, err
			}
			palette[index] = color.NRGBA64{R: uint16(math.Round(rgb[0] * 65535)), G: uint16(math.Round(rgb[1] * 65535)), B: uint16(math.Round(rgb[2] * 65535)), A: 65535}
			if deviceN.none {
				palette[index].A = 0
			}
			continue
		}
		if profile != nil {
			values := result.values[index]
			if profile.alternate != nil {
				device := profile.deviceValues(values[:components])
				paint, err := profile.paint(device[:components], intent)
				if err != nil {
					return nil, err
				}
				result.values[index] = paint.Values
				if paint.Space == nil {
					copy(result.values[index][:], paint.RGB[:])
				}
				alpha := uint16(65535)
				if paint.None {
					alpha = 0
				}
				palette[index] = color.NRGBA64{R: uint16(math.Round(paint.RGB[0] * 65535)), G: uint16(math.Round(paint.RGB[1] * 65535)), B: uint16(math.Round(paint.RGB[2] * 65535)), A: alpha}
				continue
			}
			rgb, err := profile.color(values[:components], intent)
			if err != nil {
				return nil, err
			}
			palette[index] = color.NRGBA64{R: uint16(math.Round(rgb[0] * 65535)), G: uint16(math.Round(rgb[1] * 65535)), B: uint16(math.Round(rgb[2] * 65535)), A: 65535}
			continue
		}
		switch components {
		case 1:
			palette[index] = color.NRGBA64{R: uint16(v[0]) * 257, G: uint16(v[0]) * 257, B: uint16(v[0]) * 257, A: 65535}
			if calibrated != nil {
				palette[index] = calibrated.color(float64(v[0])/255, float64(v[0])/255, float64(v[0])/255)
			}
		case 3:
			palette[index] = color.NRGBA64{R: uint16(v[0]) * 257, G: uint16(v[1]) * 257, B: uint16(v[2]) * 257, A: 65535}
			if calibrated != nil {
				palette[index] = calibrated.color(float64(v[0])/255, float64(v[1])/255, float64(v[2])/255)
			}
		case 4:
			rgb := deviceCMYKRGB(result.values[index][:])
			palette[index] = color.NRGBA64{R: uint16(math.Round(rgb[0] * 65535)), G: uint16(math.Round(rgb[1] * 65535)), B: uint16(math.Round(rgb[2] * 65535)), A: 65535}
		}
	}
	return result, nil
}

// decodeMask 读取有效遮罩，软遮罩优先于显式遮罩和色键遮罩
// 入参: ctx 取消上下文, components 原始图像分量数
// 返回: *imageResample 遮罩图像, []float64 色键范围, bool 是否反转遮罩灰度, []float64 预混合底色, error 错误信息
func (i *Image) decodeMask(ctx context.Context, components int) (*imageResample, []float64, bool, []float64, error) {
	if err := ctx.Err(); err != nil {
		return nil, nil, false, nil, err
	}
	object, err := i.reader.Resolve(i.SoftMask)
	if err != nil {
		return nil, nil, false, nil, err
	}
	soft := object != nil && object != Name("None")
	if !soft {
		object, err = i.reader.Resolve(i.Mask)
		if err != nil {
			return nil, nil, false, nil, err
		}
	}
	if object == nil {
		return nil, nil, false, nil, nil
	}
	if i.ImageMask {
		return nil, nil, false, nil, fmt.Errorf("stencil image cannot have a mask")
	}
	if keys, ok := object.(Array); ok && !soft {
		if len(keys) != components*2 {
			return nil, nil, false, nil, fmt.Errorf("invalid color key mask")
		}
		values := make([]float64, len(keys))
		maximum := (int64(1) << i.BitsPerComponent) - 1
		for n, key := range keys {
			value, err := i.reader.Resolve(key)
			if err != nil {
				return nil, nil, false, nil, err
			}
			v, ok := value.(Integer)
			if !ok || v < 0 || int64(v) > maximum || n%2 == 1 && float64(v) < values[n-1] {
				return nil, nil, false, nil, fmt.Errorf("invalid color key range")
			}
			values[n] = float64(v)
		}
		return nil, values, false, nil, nil
	}
	mask, err := i.reader.ReadImage(object)
	if err != nil {
		return nil, nil, false, nil, err
	}
	mask.Warning = i.Warning
	if mask.Mask != nil || mask.SoftMask != nil || soft && (mask.ImageMask || mask.ColorSpace != Name("DeviceGray")) || !soft && !mask.ImageMask {
		return nil, nil, false, nil, fmt.Errorf("invalid image mask dictionary")
	}
	var matte []float64
	if value := mask.Stream.Dictionary["Matte"]; value != nil {
		if !soft {
			return nil, nil, false, nil, fmt.Errorf("invalid image Matte array")
		}
		resolved, err := i.reader.Resolve(value)
		if err != nil {
			return nil, nil, false, nil, err
		}
		array, ok := resolved.(Array)
		if !ok || len(array) != components {
			return nil, nil, false, nil, fmt.Errorf("invalid image Matte array")
		}
		matte = make([]float64, len(array))
		for index, component := range array {
			matte[index], err = i.reader.number(component)
			if err != nil || math.IsNaN(matte[index]) || math.IsInf(matte[index], 0) {
				return nil, nil, false, nil, fmt.Errorf("invalid image Matte value")
			}
		}
	}
	decoded, err := mask.DecodeImageContext(ctx)
	if err != nil {
		return nil, nil, false, nil, err
	}
	return &imageResample{source: decoded, bounds: decoded.Bounds(), interpolate: mask.Interpolate}, nil, !soft, matte, nil
}

// ccittSamples 按PDF参数解码CCITT传真样本，保留黑白映射及行边界
// 入参: data 编码数据, params 解码参数
// 返回: image.Image 样本图像, error 错误信息
func (i *Image) ccittSamples(data []byte, params Dictionary) (image.Image, error) {
	return i.ccittSamplesContext(context.Background(), data, params)
}

// ccittSamplesContext 按行边界读取CCITT样本并检查取消
// 入参: ctx 取消上下文, data 编码数据, params 解码参数
// 返回: image.Image 样本图像, error 解码或取消错误
func (i *Image) ccittSamplesContext(ctx context.Context, data []byte, params Dictionary) (image.Image, error) {
	if i.BitsPerComponent != 1 {
		return nil, fmt.Errorf("invalid CCITT component depth")
	}
	_, expected, err := imageSampleSize(i.Width, i.Height, 1, 1)
	if err != nil {
		return nil, err
	}
	reader, endOfBlock, err := ccittImageReader(bytes.NewReader(data), i.Width, i.Height, params)
	if err != nil {
		return nil, err
	}
	reader = &contextInput{ctx: ctx, reader: reader}
	samples, err := io.ReadAll(io.LimitReader(reader, int64(expected)))
	if err != nil {
		return nil, err
	}
	if len(samples) != expected {
		return nil, io.ErrUnexpectedEOF
	}
	if endOfBlock {
		var extra [1]byte
		n, err := io.ReadFull(reader, extra[:])
		if n != 0 {
			return nil, fmt.Errorf("CCITT dimensions differ from image dictionary")
		}
		if err != io.EOF {
			return nil, err
		}
	}
	return i.rawSamples(ctx, samples)
}

// ccittImageReader 按图像尺寸和过滤器参数建立CCITT解码器
// 入参: source 编码数据, width 图像宽度, height 图像高度, params 解码参数
// 返回: io.Reader 样本读取器, bool 是否需要块结束标记, error 参数错误
func ccittImageReader(source io.Reader, width, height int, params Dictionary) (io.Reader, bool, error) {
	k, err := integerDefault(params, "K", 0)
	if err != nil {
		return nil, false, err
	}
	columns, err := integerDefault(params, "Columns", 1728)
	if err != nil {
		return nil, false, err
	}
	if columns != int64(width) || columns <= 0 || height <= 0 {
		return nil, false, fmt.Errorf("CCITT dimensions differ from image dictionary")
	}
	rows, err := integerDefault(params, "Rows", 0)
	if err != nil || rows < 0 {
		return nil, false, fmt.Errorf("invalid CCITT Rows")
	}
	endOfBlock, align, invert, endOfLine := true, false, false, false
	for _, flag := range []struct {
		name  Name
		value *bool
	}{{"EndOfBlock", &endOfBlock}, {"EncodedByteAlign", &align}, {"BlackIs1", &invert}, {"EndOfLine", &endOfLine}} {
		if value := params[flag.name]; value != nil {
			boolean, ok := value.(Boolean)
			if !ok {
				return nil, false, fmt.Errorf("invalid CCITT %s", flag.name)
			}
			*flag.value = bool(boolean)
		}
	}
	if endOfLine && k < 0 {
		return nil, false, &UnsupportedError{Feature: "CCITT Group 4 end-of-line markers"}
	}
	if k >= 0 {
		damage := int64(0)
		if endOfLine {
			damage, err = integerDefault(params, "DamagedRowsBeforeError", 0)
			if err != nil || damage < 0 || uint64(damage) > uint64(^uint(0)>>1) {
				return nil, false, fmt.Errorf("invalid CCITT DamagedRowsBeforeError")
			}
		}
		if !endOfBlock && rows != 0 {
			if uint64(rows) > uint64(^uint(0)>>1) {
				return nil, false, fmt.Errorf("invalid CCITT Rows")
			}
			height = int(rows)
		}
		row := make([]byte, width/8+min(1, width%8))
		return &faxGroup3Reader{source: source, width: width, height: height, row: row, previous: make([]byte, len(row)), offset: len(row), changes: []int{width}, mixed: k > 0, eol: endOfLine, align: align, invert: invert, eob: endOfBlock, allowDamage: int(damage)}, endOfBlock, nil
	}
	if !endOfBlock && rows != 0 && rows != int64(height) {
		return nil, false, &UnsupportedError{Feature: "CCITT Rows differing from image height"}
	}
	if endOfBlock {
		height = ccitt.AutoDetectHeight
	}
	return ccitt.NewReader(source, ccitt.MSB, ccitt.Group4, width, height, &ccitt.Options{Align: align, Invert: invert}), endOfBlock, nil
}

// rawSamples 接管独立解压缓冲，将设备色彩样本或颜色索引解释为图像
// 入参: ctx 取消上下文, data 独立原始样本字节
// 返回: image.Image 样本图像, error 错误信息
func (i *Image) rawSamples(ctx context.Context, data []byte) (image.Image, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	components := 0
	if space, ok := i.ColorSpace.(Array); ok && len(space) > 0 && space[0] == Name("DeviceN") {
		deviceN, err := i.reader.readDeviceNSpace(space, i.effectiveColorSpace, 0)
		if err != nil {
			return nil, err
		}
		components = deviceN.components
	}
	switch i.ColorSpace {
	case Name("DeviceGray"):
		components = 1
	case Name("DeviceRGB"):
		components = 3
	case Name("DeviceCMYK"):
		components = 4
	}
	if i.ImageMask {
		components = 1
	}
	if space, ok := i.ColorSpace.(Array); ok && len(space) == 4 && space[0] == Name("Indexed") {
		components = 1
	}
	if space, ok := i.ColorSpace.(Array); ok && len(space) == 4 && space[0] == Name("Separation") {
		components = 1
	}
	if space, ok := i.ColorSpace.(Array); ok && len(space) == 2 && (space[0] == Name("CalRGB") || space[0] == Name("Lab")) {
		components = 3
	}
	if space, ok := i.ColorSpace.(Array); ok && len(space) == 2 && space[0] == Name("CalGray") {
		components = 1
	}
	if space, ok := i.ColorSpace.(Array); ok && len(space) == 2 && space[0] == Name("ICCBased") {
		profile, err := i.reader.Resolve(space[1])
		if err != nil {
			return nil, err
		}
		stream, ok := profile.(*Stream)
		if !ok {
			return nil, fmt.Errorf("invalid ICC profile stream")
		}
		switch stream.Dictionary["N"] {
		case Integer(1), Integer(3), Integer(4):
			components = int(stream.Dictionary["N"].(Integer))
		default:
			return nil, &UnsupportedError{Feature: "image ICC component count"}
		}
	}
	if components == 0 {
		return nil, &UnsupportedError{Feature: "raw image color space"}
	}
	stride, expected, err := imageSampleSize(i.Width, i.Height, components, i.BitsPerComponent)
	if err != nil {
		return nil, err
	}
	if expected > len(data) {
		return nil, fmt.Errorf("image sample size mismatch")
	}
	if expected < len(data) {
		for n, value := range data[expected:] {
			if n&4095 == 0 {
				if err := ctx.Err(); err != nil {
					return nil, err
				}
			}
			if value != 0 {
				if i.Warning == nil {
					return nil, fmt.Errorf("image sample size mismatch")
				}
				i.Warning(Diagnostic{Message: fmt.Sprintf("image has %d excess sample bytes; decoded using declared dimensions %dx%d", len(data)-expected, i.Width, i.Height)})
				break
			}
		}
		data = bytes.Clone(data[:expected])
	}
	if components > 4 {
		return &packedDeviceNImage{data: data, rect: image.Rect(0, 0, i.Width, i.Height), stride: stride, depth: i.BitsPerComponent, channels: components}, nil
	}
	if components == 4 {
		if i.BitsPerComponent != 8 {
			return &packedCMYKImage{data: data, rect: image.Rect(0, 0, i.Width, i.Height), stride: stride, depth: i.BitsPerComponent}, nil
		}
		return &image.CMYK{Pix: data, Stride: stride, Rect: image.Rect(0, 0, i.Width, i.Height)}, nil
	}
	if components == 3 && i.BitsPerComponent == 8 {
		if _, _, err := imageBufferSize(i.Width, i.Height, 4); err != nil {
			return nil, err
		}
		out := image.NewNRGBA(image.Rect(0, 0, i.Width, i.Height))
		for y := 0; y < i.Height; y++ {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			input, output := data[y*stride:], out.Pix[y*out.Stride:]
			for x := 0; x < i.Width; x++ {
				copy(output[x*4:x*4+3], input[x*3:x*3+3])
				output[x*4+3] = 255
			}
		}
		return out, nil
	}
	maximum := (uint32(1) << i.BitsPerComponent) - 1
	if components == 2 || components == 3 {
		if _, _, err := imageBufferSize(i.Width, i.Height, 8); err != nil {
			return nil, err
		}
		out := image.NewNRGBA64(image.Rect(0, 0, i.Width, i.Height))
		for y := 0; y < i.Height; y++ {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			line := data[y*stride : (y+1)*stride]
			for x := 0; x < i.Width; x++ {
				var values [3]uint16
				for c := 0; c < components; c++ {
					values[c] = uint16(uint32(packedSample(line, x*components+c, i.BitsPerComponent)) * 65535 / maximum)
				}
				out.SetNRGBA64(x, y, color.NRGBA64{R: values[0], G: values[1], B: values[2], A: 65535})
			}
		}
		return out, nil
	}
	if i.BitsPerComponent < 8 {
		return &packedGrayImage{data: data, rect: image.Rect(0, 0, i.Width, i.Height), stride: stride, depth: i.BitsPerComponent}, nil
	}
	if i.BitsPerComponent == 16 {
		return &image.Gray16{Pix: data, Stride: stride, Rect: image.Rect(0, 0, i.Width, i.Height)}, nil
	}
	return &image.Gray{Pix: data, Stride: stride, Rect: image.Rect(0, 0, i.Width, i.Height)}, nil
}

// imageNRGBASample 取得非预乘颜色，保留透明像素中仍有效的原始颜色分量
// 入参: pixel 样本颜色
// 返回: color.NRGBA64 非预乘颜色
func imageNRGBASample(pixel color.Color) color.NRGBA64 {
	switch c := pixel.(type) {
	case jpxSample:
		if !c.cmyk {
			return color.NRGBA64{R: c.values[0], G: c.values[1], B: c.values[2], A: c.alpha}
		}
	case color.NRGBA:
		return color.NRGBA64{R: uint16(c.R) * 257, G: uint16(c.G) * 257, B: uint16(c.B) * 257, A: uint16(c.A) * 257}
	case color.NRGBA64:
		return c
	}
	return color.NRGBA64Model.Convert(pixel).(color.NRGBA64)
}

// imageCMYKSample 统一取得八位或十六位四色样本，保留黑色分量
// 入参: pixel 样本颜色
// 返回: cmykSample 四色分量, bool 是否为四色样本
func imageCMYKSample(pixel color.Color) (cmykSample, bool) {
	if sample, ok := pixel.(jpxSample); ok && sample.cmyk {
		return sample.values, true
	}
	switch c := pixel.(type) {
	case color.CMYK:
		return cmykSample{uint16(c.C) * 257, uint16(c.M) * 257, uint16(c.Y) * 257, uint16(c.K) * 257}, true
	case cmykSample:
		return c, true
	}
	return cmykSample{}, false
}

// imageCMYKAt 直接读取四色及重采样分量，保留原有最近邻和双线性舍入
// 入参: source 样本图像, x 横向坐标, y 纵向坐标
// 返回: cmykSample 四色分量, bool 是否为四色样本
func imageCMYKAt(source image.Image, x, y int) (cmykSample, bool) {
	switch s := source.(type) {
	case *image.CMYK:
		pixel := s.CMYKAt(x, y)
		return cmykSample{uint16(pixel.C) * 257, uint16(pixel.M) * 257, uint16(pixel.Y) * 257, uint16(pixel.K) * 257}, true
	case *packedCMYKImage:
		if !image.Pt(x, y).In(s.rect) {
			return cmykSample{}, false
		}
		return s.sample(x, y), true
	case *imageResample:
		b := s.source.Bounds()
		if b == s.bounds {
			return imageCMYKAt(s.source, x, y)
		}
		sx := (float64(x-s.bounds.Min.X)+.5)*float64(b.Dx())/float64(s.bounds.Dx()) - .5
		sy := (float64(y-s.bounds.Min.Y)+.5)*float64(b.Dy())/float64(s.bounds.Dy()) - .5
		if !s.interpolate {
			return imageCMYKAt(s.source, b.Min.X+min(b.Dx()-1, max(0, int(math.Floor(sx+.5)))), b.Min.Y+min(b.Dy()-1, max(0, int(math.Floor(sy+.5)))))
		}
		x0, y0 := int(math.Floor(sx)), int(math.Floor(sy))
		fx, fy := sx-float64(x0), sy-float64(y0)
		var values [4]float64
		for dy := 0; dy < 2; dy++ {
			for dx := 0; dx < 2; dx++ {
				pixel, ok := imageCMYKAt(s.source, b.Min.X+min(b.Dx()-1, max(0, x0+dx)), b.Min.Y+min(b.Dy()-1, max(0, y0+dy)))
				if !ok {
					return cmykSample{}, false
				}
				wx, wy := 1-fx, 1-fy
				if dx == 1 {
					wx = fx
				}
				if dy == 1 {
					wy = fy
				}
				for c, value := range pixel {
					values[c] += float64(value) * wx * wy
				}
			}
		}
		return cmykSample{uint16(math.Round(values[0])), uint16(math.Round(values[1])), uint16(math.Round(values[2])), uint16(math.Round(values[3]))}, true
	default:
		return imageCMYKSample(source.At(x, y))
	}
}
