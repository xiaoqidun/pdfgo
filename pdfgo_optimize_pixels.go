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
	"image"
	"image/color"
	"io"
	"maps"
)

// encodeLosslessPixels 编码降采样后的线稿，避免JPEG引入边缘振铃
// 入参: ctx 取消上下文, source 原始流, img 图片, channels 通道数
// 返回: *Stream 输出流, error 编码错误
func (r *Reader) encodeLosslessPixels(ctx context.Context, source *Stream, img image.Image, channels int) (*Stream, error) {
	b := img.Bounds()
	pixels := make([]byte, b.Dx()*b.Dy()*channels)
	for y := 0; y < b.Dy(); y++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		for x := 0; x < b.Dx(); x++ {
			c := color.NRGBAModel.Convert(img.At(b.Min.X+x, b.Min.Y+y)).(color.NRGBA)
			i := (y*b.Dx() + x) * channels
			pixels[i] = c.R
			if channels == 3 {
				pixels[i+1], pixels[i+2] = c.G, c.B
			}
		}
	}
	result := *source
	result.Dictionary = maps.Clone(source.Dictionary)
	result.Data = pixels
	result.Dictionary["Width"], result.Dictionary["Height"] = Integer(b.Dx()), Integer(b.Dy())
	delete(result.Dictionary, "Filter")
	delete(result.Dictionary, "DecodeParms")
	result.decrypted = false
	best, err := r.optimizeImagePixels(ctx, &result)
	if err != nil {
		return nil, err
	}
	if best == &result {
		encoded, err := compressPDFBytes(ctx, pixels)
		if err != nil {
			return nil, err
		}
		result.Data = encoded
		result.Dictionary["Filter"] = Name("FlateDecode")
	}
	if source.decrypted {
		name, err := r.security.outputStreamFilter(source, r)
		if err != nil {
			return nil, err
		}
		best.Dictionary["Filter"] = Array{Name("Crypt"), best.Dictionary["Filter"]}
		best.Dictionary["DecodeParms"] = Array{Dictionary{"Name": name}, best.Dictionary["DecodeParms"]}
		best.decrypted = true
	}
	return best, nil
}

// optimizeImagePixels 比较预测编码与精确索引色，保留颜色解释和透明蒙版
// 入参: ctx 取消上下文, stream 原始图片流
// 返回: *Stream 更小的流, error 取消或编码错误
func (r *Reader) optimizeImagePixels(ctx context.Context, stream *Stream) (*Stream, error) {
	d := stream.Dictionary
	if d["Subtype"] != Name("Image") || d["BitsPerComponent"] != Integer(8) || d["ImageMask"] == Boolean(true) || d["F"] != nil {
		return stream, nil
	}
	space, err := r.Resolve(d["ColorSpace"])
	if err != nil {
		return stream, nil
	}
	channels := 0
	switch space {
	case Name("DeviceRGB"):
		channels = 3
	case Name("DeviceGray"):
		channels = 1
	case Name("DeviceCMYK"):
		channels = 4
	default:
		return stream, nil
	}
	w, _ := d["Width"].(Integer)
	h, _ := d["Height"].(Integer)
	if w <= 0 || h <= 0 || w > 1<<20 || h > 1<<20 || int64(w*h) > optimizationBufferLimit/int64(channels) {
		return stream, nil
	}
	filters, params, err := stream.filterChain(r)
	if err != nil || len(filters) > 1 || len(filters) == 1 && filters[0] != Name("FlateDecode") {
		return stream, nil
	}
	pixels := stream.Data
	if len(filters) == 1 {
		input, err := zlib.NewReader(bytes.NewReader(pixels))
		if err != nil {
			return stream, nil
		}
		pixels, err = io.ReadAll(io.LimitReader(&contextInput{ctx: ctx, reader: input}, optimizationBufferLimit+1))
		input.Close()
		if err != nil || len(pixels) > optimizationBufferLimit {
			return stream, ctx.Err()
		}
		dict, _ := params[0].(Dictionary)
		pixels, err = decodePredictor(pixels, dict)
		if err != nil {
			return stream, nil
		}
	}
	if int64(len(pixels)) != int64(w*h)*int64(channels) {
		return stream, nil
	}
	best := stream
	encode := func(data []byte, row, bpp, bits, colors int, cs Object) error {
		var buffer bytes.Buffer
		limit := &optimizationBuffer{buffer: &buffer, limit: len(best.Data)}
		zw, _ := zlib.NewWriterLevel(limit, zlib.BestCompression)
		prior := make([]byte, row)
		candidate := make([]byte, row+1)
		selected := make([]byte, row+1)
		for start := 0; start < len(data); start += row {
			if err := ctx.Err(); err != nil {
				zw.Close()
				return err
			}
			line := data[start : start+row]
			bestScore := int64(1 << 62)
			for filter := byte(0); filter <= 4; filter++ {
				candidate[0] = filter
				score := int64(0)
				for i, v := range line {
					var left, upLeft byte
					if i >= bpp {
						left = line[i-bpp]
						upLeft = prior[i-bpp]
					}
					prediction := byte(0)
					switch filter {
					case 1:
						prediction = left
					case 2:
						prediction = prior[i]
					case 3:
						prediction = byte((int(left) + int(prior[i])) / 2)
					case 4:
						prediction = paeth(left, prior[i], upLeft)
					}
					v -= prediction
					candidate[i+1] = v
					if v < 128 {
						score += int64(v)
					} else {
						score += int64(256 - int(v))
					}
				}
				if score < bestScore {
					bestScore = score
					copy(selected, candidate)
				}
			}
			if _, err := zw.Write(selected); err != nil {
				zw.Close()
				if limit.exceeded {
					return nil
				}
				return err
			}
			copy(prior, line)
		}
		if err := zw.Close(); err != nil {
			if limit.exceeded {
				return nil
			}
			return err
		}
		overhead := 100
		if palette, ok := cs.(Array); ok {
			if table, ok := palette[3].(String); ok {
				overhead += len(table) * 2
			}
		}
		if buffer.Len()+overhead >= len(best.Data) {
			return nil
		}
		result := *stream
		result.Dictionary = maps.Clone(d)
		result.Data = buffer.Bytes()
		result.Dictionary["ColorSpace"] = cs
		result.Dictionary["BitsPerComponent"] = Integer(bits)
		parameters := Dictionary{"Predictor": Integer(15), "Colors": Integer(colors), "BitsPerComponent": Integer(bits), "Columns": w}
		result.Dictionary["Filter"] = Name("FlateDecode")
		result.Dictionary["DecodeParms"] = parameters
		if stream.decrypted {
			name, err := r.security.outputStreamFilter(stream, r)
			if err != nil {
				return nil
			}
			result.Dictionary["Filter"] = Array{Name("Crypt"), Name("FlateDecode")}
			result.Dictionary["DecodeParms"] = Array{Dictionary{"Name": name}, parameters}
		}
		best = &result
		return nil
	}
	if err := encode(pixels, int(w)*channels, channels, 8, channels, d["ColorSpace"]); err != nil {
		return nil, err
	}
	if channels == 3 && d["Decode"] == nil && d["Mask"] == nil && d["SMask"] == nil {
		palette := make(map[[3]byte]byte)
		var table []byte
		indices := make([]byte, int(w*h))
		for i := range indices {
			if i%65536 == 0 {
				if err := ctx.Err(); err != nil {
					return nil, err
				}
			}
			key := [3]byte(pixels[i*3 : i*3+3])
			index, ok := palette[key]
			if !ok {
				if len(palette) == 256 {
					return best, nil
				}
				index = byte(len(palette))
				palette[key] = index
				table = append(table, key[:]...)
			}
			indices[i] = index
		}
		bits := 8
		if len(palette) <= 2 {
			bits = 1
		} else if len(palette) <= 4 {
			bits = 2
		} else if len(palette) <= 16 {
			bits = 4
		}
		row := (int(w)*bits + 7) / 8
		packed := make([]byte, row*int(h))
		for y := 0; y < int(h); y++ {
			for x := 0; x < int(w); x++ {
				packed[y*row+x*bits/8] |= indices[y*int(w)+x] << uint(8-bits-x*bits%8)
			}
		}
		if err := encode(packed, row, 1, bits, 1, Array{Name("Indexed"), space, Integer(len(palette) - 1), String(table)}); err != nil {
			return nil, err
		}
	}
	return best, ctx.Err()
}
