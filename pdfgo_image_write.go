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
	"encoding/binary"
	"fmt"
	"image"
	"image/color"
	"io"
	"maps"
	"math"
	"os"
	"slices"
)

// ImageWriteOptions 保存图像创建与替换参数
// PreblendBinary对二值透明图像采用标准Matte预混合，保留可见颜色，透明像素的隐藏颜色不保留
type ImageWriteOptions struct {
	PreblendBinary bool
}

// lazyReplacementImage 保存延迟编码替换图像所需的元信息与像素源
type lazyReplacementImage struct {
	image       image.Image
	baseDict    Dictionary
	interpolate bool
	options     ImageWriteOptions
}

// encodeReplacementImage 按需编码单个延迟替换图像并与原字典属性合并
// 入参: ctx 取消上下文, item 延迟替换图像
// 返回: *Stream 替换后的图像流, *Stream 可选软遮罩流, error 编码错误
func encodeReplacementImage(ctx context.Context, item *lazyReplacementImage) (*Stream, *Stream, error) {
	stream, mask, err := encodePDFImage(ctx, item.image, item.options)
	if err != nil {
		return nil, nil, err
	}
	dict := maps.Clone(item.baseDict)
	maps.Copy(dict, stream.Dictionary)
	stream.Dictionary = dict
	if mask != nil {
		mask.Dictionary["Interpolate"] = Boolean(item.interpolate)
	}
	return stream, mask, nil
}

// replacementImageVersion 估算延迟替换图像所需的最低PDF版本，高版本文档直接跳过检查
// 入参: ctx 取消上下文, current 当前版本, lazy 延迟替换图像集合
// 返回: string 所需PDF版本, error 取消错误
func replacementImageVersion(ctx context.Context, current string, lazy map[Reference]*lazyReplacementImage) (string, error) {
	version := current
	if version >= "1.5" || len(lazy) == 0 {
		return version, ctx.Err()
	}
	for _, item := range lazy {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		img := unwrapImage(item.image)
		bits, _, _, alwaysOpaque := imageEncodingSpec(img)
		if bits == 16 && (img.ColorModel() != color.RGBAModel || alwaysOpaque) {
			return "1.5", nil
		}
		if alwaysOpaque || version >= "1.4" && img.ColorModel() != color.RGBAModel {
			continue
		}
		if rgba, ok := img.(*image.RGBA); ok {
			hasZero, hasPartial, err := rgbaAlphaKind(ctx, rgba)
			if err != nil {
				return "", err
			}
			if hasPartial {
				return "1.5", nil
			}
			if hasZero {
				version = max(version, "1.4")
			}
			continue
		}
		opaque, err := imageOpaqueContext(ctx, img)
		if err != nil {
			return "", err
		}
		if !opaque {
			if img.ColorModel() == color.RGBAModel {
				return "1.5", nil
			}
			version = max(version, "1.4")
		}
	}
	return version, ctx.Err()
}

// rgbaAlphaKind 扫描RGBA图像是否存在全透明及半透明像素
// 入参: ctx 取消上下文, rgba 预乘RGBA图像
// 返回: hasZero 是否存在全透明像素, hasPartial 是否存在半透明像素, err 取消错误
func rgbaAlphaKind(ctx context.Context, rgba *image.RGBA) (hasZero, hasPartial bool, err error) {
	b := rgba.Bounds()
	w := b.Dx()
	for y := b.Min.Y; y < b.Max.Y; y++ {
		if err := ctx.Err(); err != nil {
			return false, false, err
		}
		start := rgba.PixOffset(b.Min.X, y)
		row := rgba.Pix[start : start+w*4]
		for x := 0; x < w; x++ {
			a := row[x*4+3]
			if a == 255 {
				continue
			}
			if a == 0 {
				hasZero = true
			} else {
				return hasZero, true, nil
			}
		}
	}
	return hasZero, false, ctx.Err()
}

// imageEncodingSpec 根据解包后的图像类型确定位深、通道数、色彩空间与静态不透明属性
// 入参: img 已解包的像素图像
// 返回: bits 初始位深, channels 通道数, space 色彩空间名称, alwaysOpaque 是否天然不透明
func imageEncodingSpec(img image.Image) (bits, channels int, space Name, alwaysOpaque bool) {
	bits, channels, space = 16, 3, Name("DeviceRGB")
	switch img.ColorModel() {
	case color.GrayModel:
		bits, channels, space = 8, 1, Name("DeviceGray")
	case color.Gray16Model:
		channels, space = 1, Name("DeviceGray")
	case color.CMYKModel:
		bits, channels, space = 8, 4, Name("DeviceCMYK")
	case color.YCbCrModel:
		bits = 8
	case color.NRGBAModel, color.AlphaModel:
		bits = 8
	}
	switch source := img.(type) {
	case *image.Gray, *image.Gray16, *image.CMYK, *image.YCbCr, *mappedGrayImage:
		alwaysOpaque = true
	case *packedGrayImage:
		bits, channels, space, alwaysOpaque = source.depth, 1, Name("DeviceGray"), true
	case *image.Paletted:
		if palettedByteExact(source.Palette) {
			bits = palettedBitDepth(len(source.Palette))
		}
	case *packedCMYKImage:
		channels, space, alwaysOpaque = 4, Name("DeviceCMYK"), true
		if source.depth <= 8 {
			bits = 8
		}
	case *jpxSampleImage:
		if source.cmyk {
			channels, space = 4, Name("DeviceCMYK")
		}
		if source.alpha < 0 {
			alwaysOpaque = true
		}
	case *deviceSampleImage:
		if source.mask == nil && !source.embedded && source.palette == nil {
			alwaysOpaque = true
		}
	}
	return bits, channels, space, alwaysOpaque
}

// palettedByteExact 检查调色板中所有颜色分量及透明度是否可精确表示为8位
// 入参: palette 颜色调色板
// 返回: bool 是否全为8位精确颜色
func palettedByteExact(palette color.Palette) bool {
	if len(palette) == 0 || len(palette) > 256 {
		return false
	}
	for _, entry := range palette {
		if entry == nil {
			return false
		}
		c := imageNRGBASample(entry)
		if c.R%257 != 0 || c.G%257 != 0 || c.B%257 != 0 || c.A%257 != 0 {
			return false
		}
	}
	return true
}

// palettedBitDepth 按调色板项数返回PDF索引色最小位深
// 入参: count 调色板项数
// 返回: int 位深（1、2、4或8）
func palettedBitDepth(count int) int {
	switch {
	case count <= 2:
		return 1
	case count <= 4:
		return 2
	case count <= 16:
		return 4
	default:
		return 8
	}
}

// detectGrayBits 检测8位灰度图像是否可精确打包为1、2或4位样本
// 入参: ctx 取消上下文, img 8位灰度图像
// 返回: int 最小精确位深, byte 量化步长, error 取消错误
func detectGrayBits(ctx context.Context, img *image.Gray) (int, byte, error) {
	b := img.Bounds()
	w := b.Dx()
	step, bits := byte(255), 1
	for y := b.Min.Y; y < b.Max.Y; y++ {
		if err := ctx.Err(); err != nil {
			return 0, 0, err
		}
		start := img.PixOffset(b.Min.X, y)
		row := img.Pix[start : start+w]
		for _, value := range row {
			if value%step == 0 {
				continue
			}
			if step == 255 && value%85 == 0 {
				step, bits = 85, 2
			} else if value%17 == 0 {
				step, bits = 17, 4
			} else {
				return 8, 1, nil
			}
		}
	}
	return bits, step, ctx.Err()
}

// writePNGUpRow 对单行样本应用PNG Up预测滤波并写入压缩器，同时更新前一行缓存
// 入参: writer 压缩输出, row 当前原始行, prev 上一行原始数据, out 滤波工作缓冲
// 返回: error 写入错误
func writePNGUpRow(writer io.Writer, row, prev, out []byte) error {
	out[0] = 2
	for i, b := range row {
		out[i+1] = b - prev[i]
	}
	copy(prev, row)
	_, err := writer.Write(out[:len(row)+1])
	return err
}

// encodePDFPackedGray 保留1/2/4/8位紧凑灰度并启用PNG Up预测器编码PDF图像流
// 入参: ctx 取消上下文, width 宽度, height 高度, bits 位深, fillRow 填充单行样本回调
// 返回: *Stream 灰度图像流, error 编码或取消错误
func encodePDFPackedGray(ctx context.Context, width, height, bits int, fillRow func(rowIndex int, dst []byte)) (*Stream, error) {
	rowBytes, _, err := imageSampleSize(width, height, 1, bits)
	if err != nil || rowBytes >= imageBufferLimit() {
		return nil, fmt.Errorf("invalid replacement image dimensions")
	}
	var pixels bytes.Buffer
	colorWriter := imageCompressor(&pixels)
	defer releaseImageCompressor(colorWriter)
	row := make([]byte, rowBytes)
	prev := make([]byte, rowBytes)
	filtered := make([]byte, rowBytes+1)
	for y := 0; y < height; y++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		clear(row)
		fillRow(y, row)
		if err := writePNGUpRow(colorWriter, row, prev, filtered); err != nil {
			return nil, err
		}
	}
	if err := colorWriter.Close(); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	dict := Dictionary{
		"Type":             Name("XObject"),
		"Subtype":          Name("Image"),
		"Width":            Integer(width),
		"Height":           Integer(height),
		"ColorSpace":       Name("DeviceGray"),
		"BitsPerComponent": Integer(bits),
		"Filter":           Name("FlateDecode"),
		"DecodeParms":      Dictionary{"Predictor": Integer(15), "Colors": Integer(1), "BitsPerComponent": Integer(bits), "Columns": Integer(width)},
	}
	return &Stream{Dictionary: dict, Data: pixels.Bytes()}, nil
}

// encodePDFPaletted 保留索引调色板及最小位深并启用PNG Up预测器编码PDF图像与可选软遮罩
// 入参: ctx 取消上下文, img 调色板图像, options 编码参数
// 返回: *Stream 索引色图像流, *Stream 可选软遮罩流, error 编码或取消错误
func encodePDFPaletted(ctx context.Context, img *image.Paletted, options ImageWriteOptions) (*Stream, *Stream, error) {
	bounds := img.Bounds()
	width, height := bounds.Dx(), bounds.Dy()
	count := len(img.Palette)
	bits := palettedBitDepth(count)
	rowBytes, _, err := imageSampleSize(width, height, 1, bits)
	if err != nil || width >= imageBufferLimit() {
		return nil, nil, fmt.Errorf("invalid replacement image dimensions")
	}
	table := make([]byte, count*3)
	alphas := make([]byte, count)
	for i, entry := range img.Palette {
		c := imageNRGBASample(entry)
		table[i*3], table[i*3+1], table[i*3+2] = byte(c.R>>8), byte(c.G>>8), byte(c.B>>8)
		alphas[i] = byte(c.A >> 8)
	}
	var pixels, alpha bytes.Buffer
	colorWriter := imageCompressor(&pixels)
	defer releaseImageCompressor(colorWriter)
	var alphaWriter *zlib.Writer
	defer func() {
		if alphaWriter != nil {
			releaseImageCompressor(alphaWriter)
		}
	}()
	row := make([]byte, rowBytes)
	prev := make([]byte, rowBytes)
	filtered := make([]byte, rowBytes+1)
	var maskRow, prevMask, filteredMask []byte
	startAlpha := func(rowIndex, colIndex int) error {
		alphaWriter = imageCompressor(&alpha)
		maskRow = make([]byte, width)
		prevMask = make([]byte, width)
		filteredMask = make([]byte, width+1)
		if rowIndex > 0 {
			for i := range maskRow {
				maskRow[i] = 255
			}
			if err := writePNGUpRow(alphaWriter, maskRow, prevMask, filteredMask); err != nil {
				return err
			}
			filteredMask[0] = 2
			clear(filteredMask[1:])
			for range rowIndex - 1 {
				if err := ctx.Err(); err != nil {
					return err
				}
				if _, err := alphaWriter.Write(filteredMask); err != nil {
					return err
				}
			}
		}
		for i := 0; i < colIndex; i++ {
			maskRow[i] = 255
		}
		return nil
	}
	hasZeroAlpha, hasPartialAlpha := false, false
	for y := 0; y < height; y++ {
		if err := ctx.Err(); err != nil {
			return nil, nil, err
		}
		start := img.PixOffset(bounds.Min.X, bounds.Min.Y+y)
		src := img.Pix[start : start+width]
		if bits == 8 {
			copy(row, src)
		} else {
			clear(row)
			perByte := 8 / bits
			for x, idx := range src {
				if int(idx) >= count {
					return nil, nil, fmt.Errorf("palette index out of range")
				}
				row[x/perByte] |= idx << uint(8-bits-(x%perByte)*bits)
			}
		}
		for x, idx := range src {
			if int(idx) >= count {
				return nil, nil, fmt.Errorf("palette index out of range")
			}
			a := alphas[idx]
			if a != 255 {
				if a == 0 {
					hasZeroAlpha = true
				} else {
					hasPartialAlpha = true
				}
				if alphaWriter == nil {
					if err := startAlpha(y, x); err != nil {
						return nil, nil, err
					}
				}
			}
			if alphaWriter != nil {
				maskRow[x] = a
			}
		}
		if err := writePNGUpRow(colorWriter, row, prev, filtered); err != nil {
			return nil, nil, err
		}
		if alphaWriter != nil {
			if err := writePNGUpRow(alphaWriter, maskRow, prevMask, filteredMask); err != nil {
				return nil, nil, err
			}
		}
	}
	if err := colorWriter.Close(); err != nil {
		return nil, nil, err
	}
	if alphaWriter != nil {
		if err := alphaWriter.Close(); err != nil {
			return nil, nil, err
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	preblend := options.PreblendBinary && hasZeroAlpha && !hasPartialAlpha
	if preblend {
		for i, a := range alphas {
			if a == 0 {
				table[i*3], table[i*3+1], table[i*3+2] = 255, 255, 255
			}
		}
	}
	stream := &Stream{
		Dictionary: Dictionary{
			"Type":             Name("XObject"),
			"Subtype":          Name("Image"),
			"Width":            Integer(width),
			"Height":           Integer(height),
			"ColorSpace":       Array{Name("Indexed"), Name("DeviceRGB"), Integer(count - 1), String(table)},
			"BitsPerComponent": Integer(bits),
			"Filter":           Name("FlateDecode"),
			"DecodeParms":      Dictionary{"Predictor": Integer(15), "Colors": Integer(1), "BitsPerComponent": Integer(bits), "Columns": Integer(width)},
		},
		Data: pixels.Bytes(),
	}
	var mask *Stream
	if alphaWriter != nil {
		mask = &Stream{
			Dictionary: Dictionary{
				"Type":             Name("XObject"),
				"Subtype":          Name("Image"),
				"Width":            Integer(width),
				"Height":           Integer(height),
				"ColorSpace":       Name("DeviceGray"),
				"BitsPerComponent": Integer(8),
				"Filter":           Name("FlateDecode"),
				"DecodeParms":      Dictionary{"Predictor": Integer(15), "Colors": Integer(1), "BitsPerComponent": Integer(8), "Columns": Integer(width)},
			},
			Data: alpha.Bytes(),
		}
	}
	return stream, mask, nil
}

// encodePDFImage 单遍逐行编码设备色像素和独立软遮罩，保留紧凑灰度与调色板并启用PNG Up预测器
// 入参: ctx 取消上下文, img 像素图像, options 替换参数
// 返回: *Stream 颜色流, *Stream 软遮罩或nil, error 编码错误
func encodePDFImage(ctx context.Context, img image.Image, options ImageWriteOptions) (*Stream, *Stream, error) {
	if img == nil {
		return nil, nil, fmt.Errorf("missing replacement image")
	}
	img = unwrapImage(img)
	bounds := img.Bounds()
	width, height := bounds.Dx(), bounds.Dy()
	if width <= 0 || height <= 0 {
		return nil, nil, fmt.Errorf("invalid replacement image dimensions")
	}
	if err := validatePalettedImage(ctx, img); err != nil {
		return nil, nil, err
	}
	switch source := img.(type) {
	case *packedGrayImage:
		if source.depth == 1 || source.depth == 2 || source.depth == 4 || source.depth == 8 {
			stream, err := encodePDFPackedGray(ctx, width, height, source.depth, func(y int, dst []byte) {
				copy(dst, source.data[y*source.stride:y*source.stride+len(dst)])
			})
			return stream, nil, err
		}
	case *image.Gray:
		bits, step, err := detectGrayBits(ctx, source)
		if err != nil {
			return nil, nil, err
		}
		if bits < 8 {
			perByte := 8 / bits
			stream, err := encodePDFPackedGray(ctx, width, height, bits, func(y int, dst []byte) {
				start := source.PixOffset(bounds.Min.X, bounds.Min.Y+y)
				src := source.Pix[start : start+width]
				for x, v := range src {
					dst[x/perByte] |= (v / step) << uint(8-bits-(x%perByte)*bits)
				}
			})
			return stream, nil, err
		}
	case *image.Paletted:
		if palettedByteExact(source.Palette) {
			return encodePDFPaletted(ctx, source, options)
		}
	}
	bits, channels, space, alwaysOpaque := imageEncodingSpec(img)
	upgradeRGBA16 := img.ColorModel() == color.RGBAModel && !alwaysOpaque
	if upgradeRGBA16 {
		bits = 8
	}
	maxBytesPerPixel := channels * (bits / 8)
	if upgradeRGBA16 {
		maxBytesPerPixel = channels * 2
	}
	if width > (imageBufferLimit()-1)/maxBytesPerPixel {
		return nil, nil, fmt.Errorf("invalid replacement image dimensions")
	}
	allowPreblend := options.PreblendBinary && channels == 3 && !alwaysOpaque
	for {
		stream, mask, retryRGBA16, retryNoPreblend, err := encodePDFImagePass(ctx, img, bounds, width, height, bits, channels, space, alwaysOpaque, allowPreblend, upgradeRGBA16)
		if err != nil {
			return nil, nil, err
		}
		if retryRGBA16 {
			bits = 16
			upgradeRGBA16 = false
			continue
		}
		if retryNoPreblend {
			allowPreblend = false
			continue
		}
		return stream, mask, nil
	}
}

// encodePDFImagePass 执行单遍像素编码，启用PNG Up预测并在首次出现透明像素时延迟启动软遮罩压缩
// 入参: ctx 取消上下文, img 解包后图像, bounds 图像范围, width 宽, height 高, bits 位深, channels 通道数, space 色彩空间, alwaysOpaque 是否天然不透明, allowPreblend 是否允许二值预混合, upgradeRGBA16 是否在发现半透明像素时切换16位
// 返回: *Stream 颜色流, *Stream 软遮罩, bool 需切换16位重试, bool 需关闭预混合重试, error 错误信息
func encodePDFImagePass(ctx context.Context, img image.Image, bounds image.Rectangle, width, height, bits, channels int, space Name, alwaysOpaque, allowPreblend, upgradeRGBA16 bool) (*Stream, *Stream, bool, bool, error) {
	var pixels, alpha bytes.Buffer
	colorWriter := imageCompressor(&pixels)
	defer releaseImageCompressor(colorWriter)
	var alphaWriter *zlib.Writer
	defer func() {
		if alphaWriter != nil {
			releaseImageCompressor(alphaWriter)
		}
	}()
	bytesPerSample := bits / 8
	rowSize := width * channels * bytesPerSample
	prevRow := make([]byte, rowSize)
	filteredRow := make([]byte, rowSize+1)
	var row, maskRow, prevMaskRow, filteredMaskRow []byte
	var directPix []byte
	var directStride int
	switch source := img.(type) {
	case *image.Gray:
		directPix, directStride = source.Pix, source.Stride
	case *image.Gray16:
		directPix, directStride = source.Pix, source.Stride
	case *image.CMYK:
		directPix, directStride = source.Pix, source.Stride
	default:
		row = make([]byte, rowSize)
	}
	startAlpha := func(rowIndex, colIndex int) error {
		alphaWriter = imageCompressor(&alpha)
		maskLen := width * bytesPerSample
		maskRow = make([]byte, maskLen)
		prevMaskRow = make([]byte, maskLen)
		filteredMaskRow = make([]byte, maskLen+1)
		if rowIndex > 0 {
			for i := range maskRow {
				maskRow[i] = 255
			}
			if err := writePNGUpRow(alphaWriter, maskRow, prevMaskRow, filteredMaskRow); err != nil {
				return err
			}
			filteredMaskRow[0] = 2
			clear(filteredMaskRow[1:])
			for range rowIndex - 1 {
				if err := ctx.Err(); err != nil {
					return err
				}
				if _, err := alphaWriter.Write(filteredMaskRow); err != nil {
					return err
				}
			}
		}
		for i := 0; i < colIndex*bytesPerSample; i++ {
			maskRow[i] = 255
		}
		return nil
	}
	preblend := allowPreblend
	preblendedZero := false
	ycbcr, _ := img.(*image.YCbCr)
	nrgba, _ := img.(*image.NRGBA)
	nrgba64, _ := img.(*image.NRGBA64)
	rgba, _ := img.(*image.RGBA)
	rgba64, _ := img.(*image.RGBA64)
	indexed, _ := img.(*image.Paletted)
	var palette [256]color.NRGBA64
	var paletteReady [256]bool
	for y := bounds.Min.Y; y < bounds.Max.Y; y++ {
		if err := ctx.Err(); err != nil {
			return nil, nil, false, false, err
		}
		rowIndex := y - bounds.Min.Y
		switch {
		case directPix != nil:
			offset := rowIndex * directStride
			row = directPix[offset : offset+rowSize]
		case ycbcr != nil:
			for x := 0; x < width; x++ {
				if x&4095 == 0 {
					if err := ctx.Err(); err != nil {
						return nil, nil, false, false, err
					}
				}
				yi := ycbcr.YOffset(bounds.Min.X+x, y)
				ci := ycbcr.COffset(bounds.Min.X+x, y)
				r8, g8, b8 := color.YCbCrToRGB(ycbcr.Y[yi], ycbcr.Cb[ci], ycbcr.Cr[ci])
				p := x * 3
				row[p], row[p+1], row[p+2] = r8, g8, b8
			}
		case nrgba != nil:
			src := nrgba.Pix[rowIndex*nrgba.Stride : rowIndex*nrgba.Stride+width*4]
			for x := 0; x < width; x++ {
				if x&4095 == 0 {
					if err := ctx.Err(); err != nil {
						return nil, nil, false, false, err
					}
				}
				si, di := x*4, x*3
				a := src[si+3]
				if a == 255 {
					row[di], row[di+1], row[di+2] = src[si], src[si+1], src[si+2]
					if alphaWriter != nil {
						maskRow[x] = 255
					}
					continue
				}
				if a != 0 && preblend {
					if preblendedZero {
						return nil, nil, false, true, nil
					}
					preblend = false
				}
				if alphaWriter == nil {
					if err := startAlpha(rowIndex, x); err != nil {
						return nil, nil, false, false, err
					}
				}
				maskRow[x] = a
				if preblend && a == 0 {
					row[di], row[di+1], row[di+2] = 255, 255, 255
					preblendedZero = true
				} else {
					row[di], row[di+1], row[di+2] = src[si], src[si+1], src[si+2]
				}
			}
		case nrgba64 != nil:
			src := nrgba64.Pix[rowIndex*nrgba64.Stride : rowIndex*nrgba64.Stride+width*8]
			for x := 0; x < width; x++ {
				if x&4095 == 0 {
					if err := ctx.Err(); err != nil {
						return nil, nil, false, false, err
					}
				}
				si, di := x*8, x*6
				a := binary.BigEndian.Uint16(src[si+6:])
				if a == 65535 {
					copy(row[di:di+6], src[si:si+6])
					if alphaWriter != nil {
						maskRow[x*2], maskRow[x*2+1] = 255, 255
					}
					continue
				}
				if a != 0 && preblend {
					if preblendedZero {
						return nil, nil, false, true, nil
					}
					preblend = false
				}
				if alphaWriter == nil {
					if err := startAlpha(rowIndex, x); err != nil {
						return nil, nil, false, false, err
					}
				}
				maskRow[x*2], maskRow[x*2+1] = src[si+6], src[si+7]
				if preblend && a == 0 {
					for i := di; i < di+6; i++ {
						row[i] = 255
					}
					preblendedZero = true
				} else {
					copy(row[di:di+6], src[si:si+6])
				}
			}
		case rgba != nil && bits == 8:
			src := rgba.Pix[rowIndex*rgba.Stride : rowIndex*rgba.Stride+width*4]
			for x := 0; x < width; x++ {
				if x&4095 == 0 {
					if err := ctx.Err(); err != nil {
						return nil, nil, false, false, err
					}
				}
				si, di := x*4, x*3
				a8 := src[si+3]
				if a8 == 255 {
					row[di], row[di+1], row[di+2] = src[si], src[si+1], src[si+2]
					if alphaWriter != nil {
						maskRow[x] = 255
					}
					continue
				}
				if a8 != 0 {
					return nil, nil, true, false, nil
				}
				if alphaWriter == nil {
					if err := startAlpha(rowIndex, x); err != nil {
						return nil, nil, false, false, err
					}
				}
				maskRow[x] = 0
				if preblend {
					row[di], row[di+1], row[di+2] = 255, 255, 255
					preblendedZero = true
				} else {
					row[di], row[di+1], row[di+2] = 0, 0, 0
				}
			}
		case rgba != nil:
			src := rgba.Pix[rowIndex*rgba.Stride : rowIndex*rgba.Stride+width*4]
			for x := 0; x < width; x++ {
				if x&4095 == 0 {
					if err := ctx.Err(); err != nil {
						return nil, nil, false, false, err
					}
				}
				si, di := x*4, x*6
				a8 := src[si+3]
				if a8 == 255 {
					r8, g8, b8 := src[si], src[si+1], src[si+2]
					row[di], row[di+1], row[di+2], row[di+3], row[di+4], row[di+5] = r8, r8, g8, g8, b8, b8
					if alphaWriter != nil {
						maskRow[x*2], maskRow[x*2+1] = 255, 255
					}
					continue
				}
				if a8 != 0 && preblend {
					if preblendedZero {
						return nil, nil, false, true, nil
					}
					preblend = false
				}
				if alphaWriter == nil {
					if err := startAlpha(rowIndex, x); err != nil {
						return nil, nil, false, false, err
					}
				}
				maskRow[x*2], maskRow[x*2+1] = a8, a8
				if a8 == 0 {
					fill := byte(0)
					if preblend {
						fill = 255
						preblendedZero = true
					}
					for i := di; i < di+6; i++ {
						row[i] = fill
					}
				} else {
					a32 := uint32(a8)
					r16 := uint16(min(uint32(src[si])*65535/a32, 65535))
					g16 := uint16(min(uint32(src[si+1])*65535/a32, 65535))
					b16 := uint16(min(uint32(src[si+2])*65535/a32, 65535))
					binary.BigEndian.PutUint16(row[di:], r16)
					binary.BigEndian.PutUint16(row[di+2:], g16)
					binary.BigEndian.PutUint16(row[di+4:], b16)
				}
			}
		case rgba64 != nil:
			src := rgba64.Pix[rowIndex*rgba64.Stride : rowIndex*rgba64.Stride+width*8]
			for x := 0; x < width; x++ {
				if x&4095 == 0 {
					if err := ctx.Err(); err != nil {
						return nil, nil, false, false, err
					}
				}
				si, di := x*8, x*6
				a16 := binary.BigEndian.Uint16(src[si+6:])
				if a16 == 65535 {
					copy(row[di:di+6], src[si:si+6])
					if alphaWriter != nil {
						maskRow[x*2], maskRow[x*2+1] = 255, 255
					}
					continue
				}
				if a16 != 0 && preblend {
					if preblendedZero {
						return nil, nil, false, true, nil
					}
					preblend = false
				}
				if alphaWriter == nil {
					if err := startAlpha(rowIndex, x); err != nil {
						return nil, nil, false, false, err
					}
				}
				maskRow[x*2], maskRow[x*2+1] = src[si+6], src[si+7]
				if a16 == 0 {
					fill := byte(0)
					if preblend {
						fill = 255
						preblendedZero = true
					}
					for i := di; i < di+6; i++ {
						row[i] = fill
					}
				} else {
					a32 := uint32(a16)
					r16 := uint16(min(uint32(binary.BigEndian.Uint16(src[si:]))*65535/a32, 65535))
					g16 := uint16(min(uint32(binary.BigEndian.Uint16(src[si+2:]))*65535/a32, 65535))
					b16 := uint16(min(uint32(binary.BigEndian.Uint16(src[si+4:]))*65535/a32, 65535))
					binary.BigEndian.PutUint16(row[di:], r16)
					binary.BigEndian.PutUint16(row[di+2:], g16)
					binary.BigEndian.PutUint16(row[di+4:], b16)
				}
			}
		default:
			var indexes []uint8
			if indexed != nil {
				offset := indexed.PixOffset(bounds.Min.X, y)
				indexes = indexed.Pix[offset : offset+width]
			}
			for x := 0; x < width; x++ {
				if x&4095 == 0 {
					if err := ctx.Err(); err != nil {
						return nil, nil, false, false, err
					}
				}
				var values [4]uint16
				a := uint16(65535)
				switch channels {
				case 1:
					c := imageRGBA64At(img, bounds.Min.X+x, y)
					values[0], a = c.R, c.A
				case 4:
					if c, ok := imageCMYKAt(img, bounds.Min.X+x, y); ok {
						values = [4]uint16(c)
					} else {
						c := color.CMYKModel.Convert(img.At(bounds.Min.X+x, y)).(color.CMYK)
						values = [4]uint16{uint16(c.C) * 257, uint16(c.M) * 257, uint16(c.Y) * 257, uint16(c.K) * 257}
					}
					if !alwaysOpaque {
						_, _, _, alphaValue := img.At(bounds.Min.X+x, y).RGBA()
						a = uint16(alphaValue)
					}
				default:
					var c color.NRGBA64
					if indexes != nil {
						index := indexes[x]
						if !paletteReady[index] {
							palette[index] = imageNRGBASample(indexed.Palette[index])
							paletteReady[index] = true
						}
						c = palette[index]
					} else {
						c = imageNRGBA64At(img, bounds.Min.X+x, y)
					}
					values[0], values[1], values[2], a = c.R, c.G, c.B, c.A
				}
				if !alwaysOpaque && a != 65535 {
					if upgradeRGBA16 && a != 0 {
						return nil, nil, true, false, nil
					}
					if a != 0 && preblend {
						if preblendedZero {
							return nil, nil, false, true, nil
						}
						preblend = false
					}
					if alphaWriter == nil {
						if err := startAlpha(rowIndex, x); err != nil {
							return nil, nil, false, false, err
						}
					}
					if channels == 3 && preblend && a == 0 {
						values[0], values[1], values[2] = 65535, 65535, 65535
						preblendedZero = true
					}
				}
				for c, value := range values[:channels] {
					index := (x*channels + c) * bytesPerSample
					if bits == 16 {
						binary.BigEndian.PutUint16(row[index:], value)
					} else {
						row[index] = byte(value >> 8)
					}
				}
				if alphaWriter != nil {
					if bits == 16 {
						binary.BigEndian.PutUint16(maskRow[x*2:], a)
					} else {
						maskRow[x] = byte(a >> 8)
					}
				}
			}
		}
		if err := writePNGUpRow(colorWriter, row, prevRow, filteredRow); err != nil {
			return nil, nil, false, false, err
		}
		if alphaWriter != nil {
			if err := writePNGUpRow(alphaWriter, maskRow, prevMaskRow, filteredMaskRow); err != nil {
				return nil, nil, false, false, err
			}
		}
	}
	if err := colorWriter.Close(); err != nil {
		return nil, nil, false, false, err
	}
	if alphaWriter != nil {
		if err := alphaWriter.Close(); err != nil {
			return nil, nil, false, false, err
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, nil, false, false, err
	}
	stream := &Stream{
		Dictionary: Dictionary{
			"Type":             Name("XObject"),
			"Subtype":          Name("Image"),
			"Width":            Integer(width),
			"Height":           Integer(height),
			"ColorSpace":       space,
			"BitsPerComponent": Integer(bits),
			"Filter":           Name("FlateDecode"),
			"DecodeParms":      Dictionary{"Predictor": Integer(15), "Colors": Integer(channels), "BitsPerComponent": Integer(bits), "Columns": Integer(width)},
		},
		Data: pixels.Bytes(),
	}
	var mask *Stream
	if alphaWriter != nil {
		mask = &Stream{
			Dictionary: Dictionary{
				"Type":             Name("XObject"),
				"Subtype":          Name("Image"),
				"Width":            Integer(width),
				"Height":           Integer(height),
				"ColorSpace":       Name("DeviceGray"),
				"BitsPerComponent": Integer(bits),
				"Filter":           Name("FlateDecode"),
				"DecodeParms":      Dictionary{"Predictor": Integer(15), "Colors": Integer(1), "BitsPerComponent": Integer(bits), "Columns": Integer(width)},
			},
			Data: alpha.Bytes(),
		}
		if preblend {
			mask.Dictionary["Matte"] = Array{Integer(1), Integer(1), Integer(1)}
		}
	}
	return stream, mask, false, false, nil
}

// CreateImage 从像素创建独立PDF图像及软遮罩，不修改文档或分配间接对象编号
// 入参: ctx 取消上下文, source 像素图像, options 编码参数
// 返回: *Image 图像描述及独立编码数据, error 参数、编码或关闭错误
func (r *Reader) CreateImage(ctx context.Context, source image.Image, options ImageWriteOptions) (*Image, error) {
	if ctx == nil || r == nil {
		return nil, fmt.Errorf("invalid image creation context")
	}
	if r.closed {
		return nil, os.ErrClosed
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	stream, mask, err := encodePDFImage(ctx, source, options)
	if err != nil {
		return nil, err
	}
	if mask != nil {
		stream.Dictionary["SMask"] = mask
	}
	return r.ReadImage(stream)
}

// ReplaceImagesTo 无损替换间接图像并保留布局、其余对象及加密配置，默认保留8/16位非预乘精度，不修改源对象且不替换遮罩或签名文档
// 出错须丢弃输出，调用期间Reader及替换图片须保持可读
// 入参: ctx 取消上下文, writer 输出流, images 图像引用与替换像素, options 替换参数
// 返回: OptimizeReport 写出结果, error 参数、编码或写入错误
func (r *Reader) ReplaceImagesTo(ctx context.Context, writer io.Writer, images map[Reference]image.Image, options ImageWriteOptions) (OptimizeReport, error) {
	return r.RewriteTo(ctx, writer, RewriteOptions{Images: images, ImageOptions: options})
}

// prepareImageReplacements 校验替换图像并准备延迟编码元信息，不提前压缩像素
// 入参: ctx 取消上下文, images 替换图像, settings 编码参数
// 返回: map[Reference]*lazyReplacementImage 延迟替换图像, error 参数错误
func (r *Reader) prepareImageReplacements(ctx context.Context, images map[Reference]image.Image, settings ImageWriteOptions) (map[Reference]*lazyReplacementImage, error) {
	lazy := make(map[Reference]*lazyReplacementImage, len(images))
	if len(images) == 0 {
		return lazy, nil
	}
	maximum := r.maxRewriteObjectID(nil)
	if maximum > math.MaxInt32-int64(len(images))-1024 {
		return nil, fmt.Errorf("PDF object number exceeds output limit")
	}
	refs := slices.Collect(maps.Keys(images))
	slices.SortFunc(refs, compareReference)
	for _, ref := range refs {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if ref.Number <= 0 || ref.Generation < 0 || ref.Generation > 65535 {
			return nil, fmt.Errorf("invalid image reference")
		}
		original, err := r.ReadImage(ref)
		if err != nil {
			return nil, err
		}
		if original.ImageMask {
			return nil, fmt.Errorf("cannot replace a stencil image")
		}
		source := images[ref]
		if source == nil {
			return nil, fmt.Errorf("missing replacement image")
		}
		bounds := source.Bounds()
		if bounds.Dx() <= 0 || bounds.Dy() <= 0 {
			return nil, fmt.Errorf("invalid replacement image dimensions")
		}
		if err := validatePalettedImage(ctx, source); err != nil {
			return nil, err
		}
		dict := maps.Clone(original.Stream.Dictionary)
		for _, key := range []Name{"Length", "Filter", "DecodeParms", "Decode", "Mask", "SMask", "SMaskInData", "ImageMask", "Matte", "F", "FFilter", "FDecodeParms", "Alternates"} {
			delete(dict, key)
		}
		lazy[ref] = &lazyReplacementImage{image: source, baseDict: dict, interpolate: original.Interpolate, options: settings}
	}
	return lazy, nil
}
