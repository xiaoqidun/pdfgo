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
	"crypto/aes"
	"crypto/cipher"
	"crypto/md5"
	"crypto/rand"
	"crypto/rc4"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"image"
	"image/jpeg"
	"io"
	"maps"
	"math"
	"slices"
	"strconv"
	"unicode/utf16"
)

// OptimizeOptions 配置原生PDF重写，不经过页面渲染
// 未指定压缩或包含签名时保留源文件；加密文档保留原安全处理器、密码和权限
// Creator非空时设置重写结果的制作软件；OnProgress回报scan、write阶段，支持取消
type OptimizeOptions struct {
	Compression CompressionOptions
	Creator     string
	OnProgress  func(stage string, completed, total int) error
}

// OptimizeReport 汇总原生PDF优化结果，Preserved表示源文件原样写出
type OptimizeReport struct {
	Bytes     int64
	Objects   int
	Streams   int
	Preserved bool
}

// pdfOutput 写出间接对象并累计文件偏移
type pdfOutput struct {
	ctx      context.Context
	writer   io.Writer
	reader   *Reader
	security *standardSecurity
	encrypt  Reference
	count    int64
}

// OptimizeTo 重写PDF对象及资源，保留未知字典、表单、附件、导航和加密配置
// 无损模式只改变表示与压缩；有损模式可重新编码可安全识别的RGB或灰度图片
// 签名文件原样交付，不修改签名；出错时应丢弃本次输出，Reader须保持源数据可读
// 入参: ctx 取消上下文, writer 输出流, options 优化配置
// 返回: OptimizeReport 优化结果, error 读写错误
func (r *Reader) OptimizeTo(ctx context.Context, writer io.Writer, options OptimizeOptions) (report OptimizeReport, err error) {
	if err := options.Compression.Validate(); err != nil {
		return report, err
	}
	if writer == nil {
		return report, fmt.Errorf("missing output writer")
	}
	out := &pdfOutput{ctx: ctx, writer: writer, reader: r, security: r.security}
	out.encrypt, _ = r.Trailer["Encrypt"].(Reference)
	defer func() { report.Bytes = out.count }()
	progress := func(stage string, done, total int) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if options.OnProgress != nil {
			return options.OnProgress(stage, done, total)
		}
		return nil
	}
	copySource := func() error {
		report.Preserved = true
		if err := progress("write", 0, 1); err != nil {
			return err
		}
		source, size := r.originalSource, r.originalSize
		if source == nil {
			source, size = r.source, r.size
		}
		written, err := io.Copy(out, io.NewSectionReader(source, 0, size))
		if err != nil {
			return err
		}
		if written != size {
			return io.ErrUnexpectedEOF
		}
		return progress("write", 1, 1)
	}
	if options.Compression.Mode == CompressionUnchanged {
		return report, copySource()
	}
	trailer := maps.Clone(r.Trailer)
	for _, name := range []Name{"Prev", "XRefStm", "Type", "W", "Index", "Length", "Filter", "DecodeParms"} {
		delete(trailer, name)
	}
	seen := make(map[Reference]bool)
	var refs []Reference
	var maskRoots []Object
	var maskRefs []Reference
	lossless := make(map[Reference]bool)
	protecting := false
	signed := false
	var collect func(Object, int) error
	collect = func(value Object, depth int) error {
		if depth > 256 {
			return fmt.Errorf("excessive PDF object nesting")
		}
		switch v := value.(type) {
		case Reference:
			if v.Number <= 0 || v.Generation < 0 || v.Generation > 65535 {
				return fmt.Errorf("invalid PDF reference")
			}
			if !seen[v] {
				seen[v] = true
				refs = append(refs, v)
			}
			if protecting && !lossless[v] {
				lossless[v] = true
				maskRefs = append(maskRefs, v)
			}
		case Array:
			for _, item := range v {
				if err := collect(item, depth+1); err != nil {
					return err
				}
			}
		case Dictionary:
			if !protecting && options.Compression.Mode == CompressionLossy {
				for _, key := range []Name{"SMask", "Mask"} {
					if v[key] != nil {
						maskRoots = append(maskRoots, v[key])
					}
				}
			}
			if v["Type"] == Name("Sig") || v["Type"] == Name("DocTimeStamp") || v["ByteRange"] != nil && v["Contents"] != nil {
				signed = true
			}
			for _, key := range slices.Sorted(maps.Keys(v)) {
				if err := collect(v[key], depth+1); err != nil {
					return err
				}
			}
		case *Stream:
			return collect(v.Dictionary, depth+1)
		}
		return nil
	}
	if err := collect(trailer, 0); err != nil {
		return report, err
	}
	originalCache := make(map[Reference]bool, len(r.cache))
	originalObjectStream := r.objectStream
	defer func() { r.objectStream = originalObjectStream }()
	for ref := range r.cache {
		originalCache[ref] = true
	}
	defer func() {
		for ref := range r.cache {
			if !originalCache[ref] {
				delete(r.cache, ref)
			}
		}
	}()
	for i := 0; i < len(refs); i++ {
		if err := progress("scan", i, len(refs)); err != nil {
			return report, err
		}
		value, err := r.Object(refs[i])
		if err != nil {
			return report, err
		}
		if err = collect(value, 0); err != nil {
			return report, err
		}
		if !originalCache[refs[i]] {
			delete(r.cache, refs[i])
		}
		if signed {
			return report, copySource()
		}
	}
	protecting = true
	for _, root := range maskRoots {
		if err := collect(root, 0); err != nil {
			return report, err
		}
	}
	for i := 0; i < len(maskRefs); i++ {
		if err := progress("scan", i, len(maskRefs)); err != nil {
			return report, err
		}
		value, err := r.Object(maskRefs[i])
		if err != nil {
			return report, err
		}
		if err := collect(value, 0); err != nil {
			return report, err
		}
		if !originalCache[maskRefs[i]] {
			delete(r.cache, maskRefs[i])
		}
	}
	slices.SortFunc(refs, func(a, b Reference) int {
		if a.Number < b.Number {
			return -1
		}
		if a.Number > b.Number {
			return 1
		}
		if a.Generation < b.Generation {
			return -1
		}
		if a.Generation > b.Generation {
			return 1
		}
		return 0
	})
	for i := 1; i < len(refs); i++ {
		if refs[i-1].Number == refs[i].Number {
			return report, fmt.Errorf("conflicting PDF object generations")
		}
	}
	report.Objects = len(refs)
	maxID := int64(0)
	for _, ref := range refs {
		maxID = max(maxID, ref.Number)
	}
	if maxID > math.MaxInt32-1024 {
		return report, fmt.Errorf("PDF object number exceeds output limit")
	}
	infoRef, _ := trailer["Info"].(Reference)
	var newInfo Dictionary
	if options.Creator != "" {
		value, resolveErr := r.Resolve(trailer["Info"])
		if resolveErr != nil {
			return report, resolveErr
		}
		if dict, ok := value.(Dictionary); ok {
			newInfo = maps.Clone(dict)
		} else {
			newInfo = Dictionary{}
		}
		encoded := []byte{0xfe, 0xff}
		for _, unit := range utf16.Encode([]rune(options.Creator)) {
			encoded = binary.BigEndian.AppendUint16(encoded, unit)
		}
		newInfo["Creator"], newInfo["Producer"] = String(encoded), String(encoded)
		if infoRef.Number == 0 {
			maxID++
			infoRef = Reference{Number: maxID}
			refs = append(refs, infoRef)
			trailer["Info"] = infoRef
		}
	}
	if _, err := fmt.Fprintf(out, "%%PDF-%s\n%%\xE2\xE3\xCF\xD3\n", r.Version); err != nil {
		return report, err
	}
	type entry struct {
		kind       byte
		position   int64
		generation int64
	}
	entries := map[int64]entry{0: {0, 0, 65535}}
	packed := r.Version >= "1.5" && (r.security == nil || r.security.stringFilter == r.security.streamFilter)
	var group []Reference
	var values [][]byte
	groupSize := 0
	writeObject := func(ref Reference, value Object) error {
		entries[ref.Number] = entry{1, out.count, ref.Generation}
		if _, err := fmt.Fprintf(out, "%d %d obj\n", ref.Number, ref.Generation); err != nil {
			return err
		}
		if err := out.object(value, ref, 0, true); err != nil {
			return err
		}
		_, err := io.WriteString(out, "\nendobj\n")
		return err
	}
	flush := func() error {
		if len(group) == 0 {
			return nil
		}
		var header, body bytes.Buffer
		for i, ref := range group {
			fmt.Fprintf(&header, "%d %d ", ref.Number, body.Len())
			body.Write(values[i])
			body.WriteByte('\n')
		}
		first := header.Len()
		header.Write(body.Bytes())
		data, err := compressPDFBytes(ctx, header.Bytes())
		if err != nil {
			return err
		}
		maxID++
		ref := Reference{Number: maxID}
		for i, item := range group {
			entries[item.Number] = entry{2, ref.Number, int64(i)}
		}
		stream := &Stream{Dictionary: Dictionary{"Type": Name("ObjStm"), "N": Integer(len(group)), "First": Integer(first), "Filter": Name("FlateDecode")}, Data: data}
		err = writeObject(ref, stream)
		group, values = nil, nil
		groupSize = 0
		return err
	}
	for i, ref := range refs {
		if err := progress("write", i, len(refs)); err != nil {
			return report, err
		}
		var value Object
		if ref == infoRef && newInfo != nil {
			value = newInfo
		} else {
			value, err = r.Object(ref)
			if err != nil {
				return report, err
			}
		}
		if stream, ok := value.(*Stream); ok {
			compression := options.Compression
			if lossless[ref] {
				compression.Mode = CompressionLossless
			}
			value, err = r.optimizeStream(ctx, stream, compression)
			if err != nil {
				return report, err
			}
			report.Streams++
		}
		if _, stream := value.(*Stream); packed && !stream && ref.Generation == 0 && ref != out.encrypt {
			var buffer bytes.Buffer
			plain := *out
			plain.writer = &buffer
			plain.security = nil
			plain.count = 0
			if err := plain.object(value, ref, 0, false); err != nil {
				return report, err
			}
			group = append(group, ref)
			values = append(values, buffer.Bytes())
			groupSize += buffer.Len()
			if len(group) >= 100 || groupSize >= 1<<20 {
				if err := flush(); err != nil {
					return report, err
				}
			}
		} else if err := writeObject(ref, value); err != nil {
			return report, err
		}
		if !originalCache[ref] {
			delete(r.cache, ref)
		}
	}
	if err := flush(); err != nil {
		return report, err
	}
	offset := out.count
	if packed {
		maxID++
		entries[maxID] = entry{1, offset, 0}
		ids := slices.Sorted(maps.Keys(entries))
		index := Array{}
		var data bytes.Buffer
		for i := 0; i < len(ids); {
			j := i + 1
			for j < len(ids) && ids[j] == ids[j-1]+1 {
				j++
			}
			index = append(index, Integer(ids[i]), Integer(j-i))
			for _, id := range ids[i:j] {
				entry := entries[id]
				var item [11]byte
				item[0] = entry.kind
				binary.BigEndian.PutUint64(item[1:9], uint64(entry.position))
				binary.BigEndian.PutUint16(item[9:], uint16(entry.generation))
				data.Write(item[:])
			}
			i = j
		}
		trailer["Type"], trailer["W"], trailer["Index"], trailer["Size"] = Name("XRef"), Array{Integer(1), Integer(8), Integer(2)}, index, Integer(maxID+1)
		encoded, err := compressPDFBytes(ctx, data.Bytes())
		if err != nil {
			return report, err
		}
		trailer["Filter"] = Name("FlateDecode")
		if err := writeObject(Reference{Number: maxID}, &Stream{Dictionary: trailer, Data: encoded}); err != nil {
			return report, err
		}
	} else {
		if _, err := io.WriteString(out, "xref\n"); err != nil {
			return report, err
		}
		ids := slices.Sorted(maps.Keys(entries))
		for i := 0; i < len(ids); {
			j := i + 1
			for j < len(ids) && ids[j] == ids[j-1]+1 {
				j++
			}
			if _, err := fmt.Fprintf(out, "%d %d\n", ids[i], j-i); err != nil {
				return report, err
			}
			for _, id := range ids[i:j] {
				entry := entries[id]
				kind := "n"
				if id == 0 {
					kind = "f"
				}
				if entry.position > 9999999999 {
					return report, fmt.Errorf("PDF cross-reference offset exceeds classic limit")
				}
				if _, err := fmt.Fprintf(out, "%010d %05d %s \n", entry.position, entry.generation, kind); err != nil {
					return report, err
				}
			}
			i = j
		}
		trailer["Size"] = Integer(maxID + 1)
		if _, err := io.WriteString(out, "trailer\n"); err != nil {
			return report, err
		}
		if err := out.object(trailer, Reference{}, 0, false); err != nil {
			return report, err
		}
	}
	if _, err := fmt.Fprintf(out, "\nstartxref\n%d\n%%%%EOF\n", offset); err != nil {
		return report, err
	}
	return report, progress("write", len(refs), len(refs))
}

// Write 交付输出并检查取消及短写
// 入参: data 待写数据
// 返回: int 已写字节数, error 写入错误
func (w *pdfOutput) Write(data []byte) (int, error) {
	if err := w.ctx.Err(); err != nil {
		return 0, err
	}
	n, err := w.writer.Write(data)
	w.count += int64(n)
	if err == nil && n != len(data) {
		err = io.ErrShortWrite
	}
	return n, err
}

// object 编码PDF对象，字符串使用十六进制保持原始字节
// 入参: value 对象, ref 所属间接对象, depth 嵌套深度, encrypt 是否加密
// 返回: error 编码或写入错误
func (w *pdfOutput) object(value Object, ref Reference, depth int, encrypt bool) error {
	if depth > 256 {
		return fmt.Errorf("excessive PDF object nesting")
	}
	if ref == w.encrypt {
		encrypt = false
	}
	var text string
	switch v := value.(type) {
	case nil:
		text = "null"
	case Boolean:
		text = strconv.FormatBool(bool(v))
	case Integer:
		text = strconv.FormatInt(int64(v), 10)
	case Real:
		if math.IsNaN(float64(v)) || math.IsInf(float64(v), 0) {
			return fmt.Errorf("invalid PDF real")
		}
		text = strconv.FormatFloat(float64(v), 'f', -1, 64)
	case Name:
		var b bytes.Buffer
		b.WriteByte('/')
		for _, c := range []byte(v) {
			if c < 33 || c > 126 || bytes.ContainsRune([]byte("()<>[]{}/%#"), rune(c)) {
				fmt.Fprintf(&b, "#%02X", c)
			} else {
				b.WriteByte(c)
			}
		}
		text = b.String()
	case String:
		data := []byte(v)
		if encrypt && w.security != nil {
			var err error
			data, err = w.security.encryptBytes(data, ref, w.security.stringFilter)
			if err != nil {
				return err
			}
		}
		text = "<" + hex.EncodeToString(data) + ">"
	case Reference:
		text = fmt.Sprintf("%d %d R", v.Number, v.Generation)
	case Array:
		if _, err := io.WriteString(w, "["); err != nil {
			return err
		}
		for i, item := range v {
			if i > 0 {
				if _, err := io.WriteString(w, " "); err != nil {
					return err
				}
			}
			if err := w.object(item, ref, depth+1, encrypt); err != nil {
				return err
			}
		}
		text = "]"
	case Dictionary:
		if _, err := io.WriteString(w, "<<"); err != nil {
			return err
		}
		for _, key := range slices.Sorted(maps.Keys(v)) {
			if err := w.object(key, ref, depth+1, false); err != nil {
				return err
			}
			if _, err := io.WriteString(w, " "); err != nil {
				return err
			}
			if err := w.object(v[key], ref, depth+1, encrypt); err != nil {
				return err
			}
			if _, err := io.WriteString(w, "\n"); err != nil {
				return err
			}
		}
		text = ">>"
	case *Stream:
		data := v.Data
		dict := maps.Clone(v.Dictionary)
		if dict["Type"] == Name("XRef") {
			encrypt = false
		}
		if encrypt && w.security != nil {
			name, err := w.security.outputStreamFilter(v, w.reader)
			if err != nil {
				return err
			}
			data, err = w.security.encryptBytes(data, ref, name)
			if err != nil {
				return err
			}
		}
		dict["Length"] = Integer(len(data))
		if err := w.object(dict, ref, depth+1, encrypt); err != nil {
			return err
		}
		if _, err := io.WriteString(w, "\nstream\n"); err != nil {
			return err
		}
		if _, err := w.Write(data); err != nil {
			return err
		}
		text = "\nendstream"
	default:
		return fmt.Errorf("unsupported PDF object %T", value)
	}
	_, err := io.WriteString(w, text)
	return err
}

// compressPDFBytes 以最高无损等级编码单个流
// 入参: ctx 取消上下文, data 原始数据
// 返回: []byte 压缩数据, error 压缩错误
func compressPDFBytes(ctx context.Context, data []byte) ([]byte, error) {
	var result bytes.Buffer
	writer, _ := zlib.NewWriterLevel(&result, zlib.BestCompression)
	for len(data) > 0 {
		if err := ctx.Err(); err != nil {
			writer.Close()
			return nil, err
		}
		n := min(len(data), 64<<10)
		if _, err := writer.Write(data[:n]); err != nil {
			return nil, err
		}
		data = data[n:]
	}
	if err := writer.Close(); err != nil {
		return nil, err
	}
	return result.Bytes(), nil
}

// optimizeStream 在保留过滤器语义的前提下选择更小的流编码
// 入参: ctx 取消上下文, s 原始流, options 压缩配置
// 返回: *Stream 输出流，不修改原始对象, error 优化错误
func (r *Reader) optimizeStream(ctx context.Context, s *Stream, options CompressionOptions) (*Stream, error) {
	if s.Dictionary["F"] != nil || len(s.Data) > optimizationBufferLimit {
		return s, nil
	}
	filters, params, err := s.filterChain(r)
	if err != nil {
		return s, nil
	}
	result := *s
	result.Dictionary = maps.Clone(s.Dictionary)
	replace := func(data []byte, filter Name) {
		result.Data = data
		result.Dictionary["Filter"] = filter
		delete(result.Dictionary, "DecodeParms")
		if s.decrypted {
			name, e := r.security.outputStreamFilter(s, r)
			if e == nil {
				result.Dictionary["Filter"] = Array{Name("Crypt"), filter}
				result.Dictionary["DecodeParms"] = Array{Dictionary{"Name": name}, nil}
			}
		}
	}
	if options.Mode == CompressionLossy && s.Dictionary["Subtype"] == Name("Image") && s.Dictionary["BitsPerComponent"] == Integer(8) && s.Dictionary["Decode"] == nil && s.Dictionary["Mask"] == nil && s.Dictionary["SMask"] == nil && s.Dictionary["ImageMask"] != Boolean(true) {
		space, _ := r.Resolve(s.Dictionary["ColorSpace"])
		width, _ := s.Dictionary["Width"].(Integer)
		height, _ := s.Dictionary["Height"].(Integer)
		if (space == Name("DeviceRGB") || space == Name("DeviceGray")) && width > 0 && height > 0 && width <= 1<<20 && height <= 1<<20 && width*height <= optimizationBufferLimit/4 && (len(filters) == 0 || len(filters) == 1 && filters[0] == Name("FlateDecode")) {
			pixels := s.Data
			var e error
			if len(filters) == 1 {
				input, openErr := zlib.NewReader(bytes.NewReader(s.Data))
				e = openErr
				if e == nil {
					pixels, e = io.ReadAll(io.LimitReader(&contextInput{ctx: ctx, reader: input}, optimizationBufferLimit+1))
					input.Close()
				}
				if e == nil && len(pixels) <= optimizationBufferLimit {
					dict, _ := params[0].(Dictionary)
					pixels, e = decodePredictor(pixels, dict)
				}
			}
			channels := int64(3)
			if space == Name("DeviceGray") {
				channels = 1
			}
			if e == nil && len(pixels) <= optimizationBufferLimit && int64(len(pixels)) == int64(width*height)*channels {
				var img image.Image
				if channels == 1 {
					img = &image.Gray{Pix: pixels, Stride: int(width), Rect: image.Rect(0, 0, int(width), int(height))}
				} else {
					p := image.NewNRGBA(image.Rect(0, 0, int(width), int(height)))
					for i, j := 0, 0; i < len(pixels); i, j = i+3, j+4 {
						copy(p.Pix[j:j+3], pixels[i:i+3])
						p.Pix[j+3] = 255
					}
					img = p
				}
				var b bytes.Buffer
				if e = jpeg.Encode(&b, img, &jpeg.Options{Quality: options.ImageQuality()}); e != nil {
					return nil, e
				}
				encoded, e := OptimizeJPEG(ctx, b.Bytes())
				if e != nil {
					return nil, e
				}
				if len(encoded)+40 < len(s.Data) {
					replace(encoded, Name("DCTDecode"))
					return &result, nil
				}
			}
		}
	}
	if len(filters) == 1 && filters[0] == Name("DCTDecode") {
		imageOptions := options
		space, _ := r.Resolve(s.Dictionary["ColorSpace"])
		parameters, _ := params[0].(Dictionary)
		if s.Dictionary["Subtype"] != Name("Image") || s.Dictionary["Decode"] != nil || s.Dictionary["Mask"] != nil || s.Dictionary["SMask"] != nil || parameters["ColorTransform"] != nil || (space != Name("DeviceRGB") && space != Name("DeviceGray")) {
			imageOptions.Mode = CompressionLossless
		}
		data, err := OptimizeImage(ctx, s.Data, imageOptions)
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			return s, nil
		}
		result.Data = data
		return &result, nil
	}
	if len(filters) > 0 && filters[0] == Name("FlateDecode") {
		input, e := zlib.NewReader(bytes.NewReader(s.Data))
		if e != nil {
			return s, nil
		}
		data, e := io.ReadAll(io.LimitReader(&contextInput{ctx: ctx, reader: input}, optimizationBufferLimit+1))
		input.Close()
		if e != nil {
			return s, nil
		}
		if len(data) > optimizationBufferLimit {
			return s, nil
		}
		encoded, e := compressPDFBytes(ctx, data)
		if e != nil {
			return nil, e
		}
		if len(encoded) < len(result.Data) {
			result.Data = encoded
		}
		return &result, nil
	}
	if len(filters) == 0 {
		data, e := compressPDFBytes(ctx, s.Data)
		if e != nil {
			return nil, e
		}
		if len(data)+40 < len(s.Data) {
			replace(data, Name("FlateDecode"))
		}
		return &result, nil
	}
	// 图像、外部及未知过滤器保持原样；通用编码可等价替换为Flate。
	decodedLimit := int64(len(s.Data))
	for _, filter := range filters {
		if filter != Name("LZWDecode") && filter != Name("ASCII85Decode") && filter != Name("ASCIIHexDecode") && filter != Name("RunLengthDecode") {
			return s, nil
		}
		factor := int64(2)
		switch filter {
		case Name("LZWDecode"):
			factor = 4096
		case Name("RunLengthDecode"):
			factor = 128
		}
		if decodedLimit > optimizationBufferLimit/factor {
			return s, nil
		}
		decodedLimit *= factor
	}
	data, e := s.Decode()
	if e != nil {
		return s, nil
	}
	encoded, e := compressPDFBytes(ctx, data)
	if e != nil {
		return nil, e
	}
	if len(encoded)+40 < len(s.Data) {
		replace(encoded, Name("FlateDecode"))
	}
	return &result, nil
}

// outputStreamFilter 获取原流的加密过滤器，保留显式Identity及嵌入文件策略
// 入参: stream 原始流, r 阅读器
// 返回: Name 过滤器名称, error 解析错误
func (s *standardSecurity) outputStreamFilter(stream *Stream, r *Reader) (Name, error) {
	name := s.streamFilter
	if stream.Dictionary["Type"] == Name("EmbeddedFile") {
		name = s.embeddedFilter
	}
	if stream.Dictionary["Type"] == Name("Metadata") && !s.encryptMetadata || stream.Dictionary["F"] != nil {
		name = "Identity"
	}
	copy := *stream
	copy.decrypted = false
	filters, params, err := copy.filterChain(r)
	if err != nil {
		return "", err
	}
	if len(filters) > 0 && filters[0] == Name("Crypt") {
		name = "Identity"
		if dict, ok := params[0].(Dictionary); ok && dict["Name"] != nil {
			var valid bool
			name, valid = dict["Name"].(Name)
			if !valid {
				return "", fmt.Errorf("invalid stream crypt filter")
			}
		}
	}
	return name, nil
}

// encryptBytes 使用现有文件密钥和对象编号重新加密输出数据
// 入参: data 原始数据, ref 对象引用, name 加密过滤器
// 返回: []byte 加密数据, error 加密错误
func (s *standardSecurity) encryptBytes(data []byte, ref Reference, name Name) ([]byte, error) {
	method, err := s.cryptMethod(name)
	if err != nil || method == "Identity" {
		return data, err
	}
	key := s.key
	if method != "AESV3" {
		seed := append(bytes.Clone(s.key), byte(ref.Number), byte(ref.Number>>8), byte(ref.Number>>16), byte(ref.Generation), byte(ref.Generation>>8))
		if method == "AESV2" {
			seed = append(seed, 's', 'A', 'l', 'T')
		}
		sum := md5.Sum(seed)
		clear(seed)
		key = sum[:min(len(s.key)+5, 16)]
	}
	if method == "V2" {
		c, err := rc4.NewCipher(key)
		if err != nil {
			return nil, err
		}
		result := make([]byte, len(data))
		c.XORKeyStream(result, data)
		return result, nil
	}
	c, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	padding := aes.BlockSize - len(data)%aes.BlockSize
	result := make([]byte, aes.BlockSize+len(data)+padding)
	if _, err := rand.Read(result[:aes.BlockSize]); err != nil {
		return nil, err
	}
	copy(result[aes.BlockSize:], data)
	for i := aes.BlockSize + len(data); i < len(result); i++ {
		result[i] = byte(padding)
	}
	cipher.NewCBCEncrypter(c, result[:aes.BlockSize]).CryptBlocks(result[aes.BlockSize:], result[aes.BlockSize:])
	return result, nil
}
