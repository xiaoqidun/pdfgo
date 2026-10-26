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
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
)

// objectStreamCacheLimit 限制多个对象流的解码缓存，单个超限流仅保留最近一次
const objectStreamCacheLimit = 16 << 20

// readerRecoveryLimit 限制损坏文档的顺序重建范围和单个解析窗口
const readerRecoveryLimit = 64 << 20

// readerTailLimit 限制文件尾部定位窗口
const readerTailLimit = 1 << 20

// Reader 读取PDF对象，方法不可并发调用，返回对象不可并发修改
type Reader struct {
	Version            string
	Trailer            Dictionary
	source             io.ReaderAt
	size               int64
	originalSource     io.ReaderAt
	originalSize       int64
	closer             io.Closer
	xref               map[int64]xrefEntry
	cache              map[Reference]Object
	loading            map[Reference]bool
	destinations       map[string]Object
	legacyDestinations Dictionary
	fonts              map[Reference]*Font
	halftones          map[Reference]*Halftone
	colorProfiles      map[[32]byte]*iccColorSpace
	deviceProfile      *Stream
	colorProfileDepth  int
	objectStreams      [4]*objectStream
	security           *standardSecurity
	warning            func(Diagnostic)
	textShaper         TextShaper
	closed             bool
	recoveryIndex      map[int64]xrefEntry
	recoveryTried      bool
}

// ReaderOptions 设置密码编码、结构诊断及生成注释外观时的文字整形
// Warning非空时诊断并恢复可确定的结构、字体及解码声明，不修改输入数据
// 源读取、取消及认证错误仍返回，绘制诊断由Visitor.Warning控制
// ShapeText用于横排纯文本字段、列表选项及注释标题，空值不生成需要整形的外观，已有外观不受影响
type ReaderOptions struct {
	Password     []byte
	PasswordUTF8 bool
	Warning      func(Diagnostic)
	ShapeText    TextShaper
}

// objectStream 保存对象流索引、解码数据及实际缓存字节数
type objectStream struct {
	reference Reference
	data      []byte
	ids       []int64
	indices   map[int64]int64
	offsets   []int64
	bytes     int64
}

// xrefEntry 保存交叉引用类型、文件偏移或对象流索引
type xrefEntry struct {
	kind       int64
	position   int64
	generation int64
}

// objectSecurityError 标记对象解密失败，防止上层边界恢复替换安全错误
type objectSecurityError struct {
	err error
}

// Open 打开PDF文件并读取交叉引用
// 入参: path 文件路径
// 返回: *Reader 阅读器, error 错误信息
func Open(path string) (*Reader, error) {
	return OpenWithPassword(path, nil)
}

// OpenWithPassword 使用已编码密码打开PDF，编码规则见NewReaderWithPassword
// 入参: path 文件路径, password 用户或所有者密码，nil表示空密码
// 返回: *Reader 阅读器, error 错误信息
func OpenWithPassword(path string, password []byte) (*Reader, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	info, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, err
	}
	r, err := NewReaderWithPassword(f, info.Size(), password)
	if err != nil {
		f.Close()
		return nil, err
	}
	r.closer = f
	return r, nil
}

// NewReader 从随机读取器读取PDF，源读取器仍由调用方管理
// 按交叉引用读取对象，源读取器须在使用期间保持可读
// 入参: source 随机读取器, size 文件字节数
// 返回: *Reader 阅读器, error 错误信息
func NewReader(source io.ReaderAt, size int64) (*Reader, error) {
	return NewReaderWithPassword(source, size, nil)
}

// NewReaderWithPassword 使用密码读取PDF，源读取器仍由调用方管理
// 支持RC4、AES-128和AES-256；R2至R4使用PDFDocEncoding，R5/R6使用经SASLprep处理的UTF-8
// 入参: source 随机读取器, size 文件字节数, password 用户或所有者密码
// 返回: *Reader 阅读器, error 错误信息
func NewReaderWithPassword(source io.ReaderAt, size int64, password []byte) (*Reader, error) {
	return NewReaderWithOptions(source, size, ReaderOptions{Password: password})
}

// NewReaderWithUTF8Password 自动按安全处理器版本转换UTF-8密码并读取PDF
// 入参: source 随机读取器, size 文件字节数, password 用户或所有者密码的UTF-8字节
// 返回: *Reader 阅读器, error 错误信息
func NewReaderWithUTF8Password(source io.ReaderAt, size int64, password []byte) (*Reader, error) {
	return NewReaderWithOptions(source, size, ReaderOptions{Password: password, PasswordUTF8: true})
}

// NewReaderWithOptions 按读取选项打开PDF，源读取器仍由调用方管理
// 入参: source 随机读取器, size 文件字节数, options 读取及外观选项
// 返回: *Reader 阅读器, error 错误信息
func NewReaderWithOptions(source io.ReaderAt, size int64, options ReaderOptions) (*Reader, error) {
	if size < 8 {
		return nil, fmt.Errorf("invalid file size")
	}
	data := make([]byte, min(size, 1024))
	if _, err := io.ReadFull(io.NewSectionReader(source, 0, size), data); err != nil {
		return nil, err
	}
	header := bytes.Index(data[:min(len(data), 1024)], []byte("%PDF-"))
	if header < 0 {
		if options.Warning == nil || !bytes.HasPrefix(data, []byte("%")) {
			return nil, fmt.Errorf("missing PDF header")
		}
		header = 0
		options.Warning(Diagnostic{Message: "missing PDF header; validating document structure"})
	}
	data = data[header:]
	end := bytes.IndexAny(data[:min(len(data), 32)], "\r\n")
	if end < 0 {
		return nil, fmt.Errorf("invalid PDF header")
	}
	version := "1.7"
	if end >= 5 && bytes.HasPrefix(data, []byte("%PDF-")) {
		version = string(bytes.Trim(data[5:end], "\x00\t\n\f\r "))
	}
	if version != "2.0" && (len(version) != 3 || version[0:2] != "1." || version[2] < '0' || version[2] > '7') {
		if options.Warning == nil {
			return nil, &UnsupportedError{Feature: "PDF version " + version}
		}
		options.Warning(Diagnostic{Message: "invalid PDF version " + version + "; using 1.7"})
		version = "1.7"
	}
	r := &Reader{Version: version, source: io.NewSectionReader(source, int64(header), size-int64(header)), size: size - int64(header), xref: map[int64]xrefEntry{}, cache: map[Reference]Object{}, loading: map[Reference]bool{}}
	r.originalSource, r.originalSize = source, size
	r.warning = options.Warning
	r.textShaper = options.ShapeText
	err := r.loadCrossReferences()
	if err != nil {
		if options.Warning == nil || !readerStructuralError(err) {
			return nil, err
		}
		index, trailer, scanErr := r.scanCrossReferences()
		if scanErr != nil {
			return nil, scanErr
		}
		r.xref, r.Trailer = index, trailer
		r.warning(Diagnostic{Message: "rebuilt cross-reference from validated object boundaries: " + err.Error()})
	}
	password := options.Password
	if options.PasswordUTF8 {
		password, err = r.encodePassword(password)
		if err != nil {
			return nil, err
		}
		defer clear(password)
	}
	if err := r.openSecurity(password); err != nil {
		return nil, err
	}
	root, err := r.Resolve(r.Trailer["Root"])
	if err != nil {
		r.Close()
		return nil, err
	}
	catalog, ok := root.(Dictionary)
	if ok && catalog["Type"] == nil && catalog["Pages"] != nil && r.warning != nil {
		r.warning(Diagnostic{Message: "missing catalog Type inferred from trailer Root and Pages"})
	} else if !ok || catalog["Type"] != Name("Catalog") {
		r.Close()
		return nil, fmt.Errorf("missing document catalog")
	}
	if v, ok := catalog["Version"].(Name); ok && string(v) > r.Version {
		r.Version = string(v)
	}
	return r, nil
}

// Close 释放源数据、对象缓存和解密密钥，关闭Open持有的文件
// 不关闭NewReader的外部数据源；关闭后不可继续读取，重复关闭不产生错误
// 返回: error 关闭错误
func (r *Reader) Close() error {
	closer := r.closer
	if r.security != nil {
		clear(r.security.key)
	}
	*r = Reader{closed: true}
	if closer != nil {
		return closer.Close()
	}
	return nil
}

// Resolve 解析间接引用，直接对象原样返回
// 入参: object 待解析对象
// 返回: Object 解析后的对象, error 错误信息
func (r *Reader) Resolve(object Object) (Object, error) {
	if r.closed {
		return nil, os.ErrClosed
	}
	ref, ok := object.(Reference)
	if !ok {
		return object, nil
	}
	object, err := r.Object(ref)
	if err != nil {
		return nil, err
	}
	if _, ok := object.(Reference); !ok {
		return object, nil
	}
	seen := map[Reference]bool{ref: true}
	for {
		ref, ok := object.(Reference)
		if !ok {
			return object, nil
		}
		if seen[ref] {
			return nil, fmt.Errorf("cyclic indirect reference")
		}
		seen[ref] = true
		var err error
		object, err = r.Object(ref)
		if err != nil {
			return nil, err
		}
	}
}

// Object 按编号与代数读取间接对象
// 不存在或已释放的引用按PDF规则返回空对象
// 入参: ref 间接引用
// 返回: Object 对象, error 错误信息
func (r *Reader) Object(ref Reference) (Object, error) {
	if r.closed {
		return nil, os.ErrClosed
	}
	if value, ok := r.cache[ref]; ok {
		return value, nil
	}
	entry, ok := r.xref[ref.Number]
	if !ok && r.warning != nil && ref.Number > 0 {
		index := r.recoveryIndex
		if !r.recoveryTried {
			var err error
			index, _, err = r.scanCrossReferences()
			if err != nil && !readerStructuralError(err) {
				return nil, err
			}
		}
		if recovered, exists := index[ref.Number]; exists && recovered.generation == ref.Generation && recovered.kind == 1 {
			entry, ok = recovered, true
			r.xref[ref.Number] = entry
			r.warning(Diagnostic{Offset: entry.position, Message: fmt.Sprintf("missing cross-reference entry recovered for object %d %d", ref.Number, ref.Generation)})
		}
	}
	if !ok || entry.kind == 0 || (entry.kind == 1 && entry.generation != ref.Generation) || (entry.kind == 2 && ref.Generation != 0) {
		return nil, nil
	}
	if r.loading[ref] || len(r.loading) >= 256 {
		return nil, fmt.Errorf("cyclic or excessive object dependency")
	}
	r.loading[ref] = true
	defer delete(r.loading, ref)
	var value Object
	var err error
	if entry.kind == 2 {
		value, err = r.compressedObject(ref, entry)
	} else {
		var actual Reference
		actual, value, err = r.indirect(entry.position)
		if err == nil && actual != ref {
			err = fmt.Errorf("cross-reference object mismatch")
		}
		if err != nil && r.warning != nil && readerStructuralError(err) {
			position, found, localErr := r.recoverObjectOffset(ref, entry.position)
			if localErr != nil {
				return nil, localErr
			}
			if found {
				actual, value, err = r.indirect(position)
				if err == nil && actual == ref {
					r.xref[ref.Number] = xrefEntry{kind: 1, position: position, generation: ref.Generation}
					r.warning(Diagnostic{Offset: position, Message: fmt.Sprintf("nearby cross-reference offset recovered for object %d %d", ref.Number, ref.Generation)})
				} else if err == nil {
					err = fmt.Errorf("recovered cross-reference object mismatch")
				}
			}
		}
		if err != nil && r.warning != nil && readerStructuralError(err) {
			index, _, scanErr := r.scanCrossReferences()
			if scanErr != nil && !readerStructuralError(scanErr) {
				return nil, scanErr
			}
			if recovered, ok := index[ref.Number]; ok && recovered.kind == 1 && recovered.generation == ref.Generation && recovered.position != entry.position {
				actual, value, err = r.indirect(recovered.position)
				if err == nil && actual != ref {
					err = fmt.Errorf("recovered cross-reference object mismatch")
				}
				if err == nil {
					r.xref[ref.Number] = recovered
					r.warning(Diagnostic{Offset: recovered.position, Message: fmt.Sprintf("recovered cross-reference offset for object %d %d", ref.Number, ref.Generation)})
				}
			}
		}
		if err == nil && r.security != nil {
			value, err = r.security.decryptObject(value, ref)
			if err != nil {
				err = &objectSecurityError{err: err}
			}
		}
	}
	if err != nil {
		return nil, err
	}
	r.cache[ref] = value
	return value, nil
}

// recoverObjectOffset 检查错误偏移附近唯一且完整的同编号对象，不接受多候选
// 入参: ref 目标引用, expected 声明的文件偏移
// 返回: int64 实际偏移, bool 是否找到唯一对象, error 源读取错误
func (r *Reader) recoverObjectOffset(ref Reference, expected int64) (int64, bool, error) {
	if expected < 0 || expected >= r.size {
		return 0, false, nil
	}
	start, stop := max(int64(0), expected-1024), min(r.size, expected+1024)
	data, err := r.readRange(start, stop-start)
	if err != nil {
		return 0, false, err
	}
	candidate := int64(-1)
	attempts := 0
	for pos, b := range data {
		if b < '1' || b > '9' || (pos > 0 && data[pos-1] != '\r' && data[pos-1] != '\n') {
			continue
		}
		p := objectParser{data: data[pos:min(len(data), pos+128)]}
		number, e1 := strconv.ParseInt(p.token(), 10, 64)
		generation, e2 := strconv.ParseInt(p.token(), 10, 64)
		if e1 != nil || e2 != nil || number != ref.Number || generation != ref.Generation || p.token() != "obj" {
			continue
		}
		at := start + int64(pos)
		if at == expected {
			continue
		}
		inside, err := r.insideIndexedStream(at, ref)
		if err != nil {
			return 0, false, err
		}
		if inside {
			continue
		}
		actual, _, err := r.indirect(at)
		attempts++
		if attempts > 256 {
			return 0, false, nil
		}
		if err != nil {
			if !readerStructuralError(err) {
				return 0, false, err
			}
			continue
		}
		if actual != ref {
			continue
		}
		if candidate >= 0 {
			return 0, false, nil
		}
		candidate = at
	}
	return candidate, candidate >= 0, nil
}

// insideIndexedStream 防止邻近偏移恢复误取已有流长度范围内的伪对象头
// 入参: offset 候选偏移, target 待恢复引用
// 返回: bool 是否落在已索引流内, error 源读取错误
func (r *Reader) insideIndexedStream(offset int64, target Reference) (bool, error) {
	previous := int64(-1)
	for number, entry := range r.xref {
		if number != target.Number && entry.kind == 1 && entry.position >= 0 && entry.position < offset {
			previous = max(previous, entry.position)
		}
	}
	if previous < 0 {
		return false, nil
	}
	var dict Dictionary
	var start int64
	err := r.parseAt(previous, func(p *objectParser) error {
		if _, err := strconv.ParseInt(p.token(), 10, 64); err != nil {
			return err
		}
		if _, err := strconv.ParseInt(p.token(), 10, 64); err != nil || p.token() != "obj" {
			return p.fail("invalid containing object header")
		}
		object, err := p.object()
		if err != nil {
			return err
		}
		dict, _ = object.(Dictionary)
		if p.token() == "stream" && dict != nil && p.streamDataStart() {
			start = previous + int64(p.pos)
		}
		return nil
	})
	if err != nil {
		if readerStructuralError(err) {
			return false, nil
		}
		return false, err
	}
	if start == 0 || offset < start {
		return false, nil
	}
	lengthObject, err := r.Resolve(dict["Length"])
	if err != nil {
		if readerStructuralError(err) {
			return true, nil
		}
		return false, err
	}
	length, ok := lengthObject.(Integer)
	return !ok || length < 0 || offset-start < int64(length), nil
}

// readerStructuralError 排除源读取、取消、关闭和安全错误，限定结构恢复范围
// 入参: err 原始错误
// 返回: bool 是否可以尝试结构恢复
func readerStructuralError(err error) bool {
	var read *ReadError
	var unsupported *UnsupportedError
	var security *objectSecurityError
	return err != nil && !errors.As(err, &read) && !errors.As(err, &unsupported) && !errors.As(err, &security) && !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) && !errors.Is(err, os.ErrClosed) && !errors.Is(err, ErrPassword)
}

// Error 返回原始对象解密错误描述
// 返回: string 错误描述
func (e *objectSecurityError) Error() string {
	return e.err.Error()
}

// Unwrap 保留原始对象解密错误供调用方检查
// 返回: error 原始错误
func (e *objectSecurityError) Unwrap() error {
	return e.err
}

// loadCrossReferences 按文件尾声明读取交叉引用链并保留最新条目
// 返回: error 声明、读取或交叉引用错误
func (r *Reader) loadCrossReferences() error {
	limit := min(r.size, int64(readerTailLimit))
	length := min(limit, int64(4096))
	var data []byte
	var err error
	for {
		data, err = r.readRange(r.size-length, length)
		if err != nil {
			return err
		}
		if bytes.Contains(data, []byte("startxref")) || length == limit {
			break
		}
		length = min(limit, length*2)
	}
	start := bytes.LastIndex(data, []byte("startxref"))
	if start < 0 {
		return fmt.Errorf("missing startxref")
	}
	p := objectParser{data: data, pos: start + 9, base: r.size - length}
	offset, err := strconv.ParseInt(p.token(), 10, 64)
	if err != nil {
		return p.fail("invalid startxref offset")
	}
	if !bytes.Equal(bytes.Trim(data[p.pos:], "\x00\t\n\f\r "), []byte("%%EOF")) {
		if r.warning == nil {
			return p.fail("missing final EOF marker")
		}
		r.warning(Diagnostic{Offset: p.base + int64(p.pos), Message: "missing final EOF marker or trailing data after EOF"})
	}
	visited := map[int64]bool{}
	for {
		if visited[offset] {
			if r.warning != nil && r.Trailer != nil {
				r.warning(Diagnostic{Offset: offset, Message: "cyclic previous cross-reference; latest entries retained"})
				break
			}
			return fmt.Errorf("cyclic cross-reference chain")
		}
		visited[offset] = true
		trailer, entries, err := r.readXref(offset)
		if err != nil {
			return err
		}
		if hybrid, ok := trailer["XRefStm"].(Integer); ok {
			if visited[int64(hybrid)] {
				return fmt.Errorf("cyclic hybrid cross-reference")
			}
			visited[int64(hybrid)] = true
			_, supplement, err := r.readXref(int64(hybrid))
			if err != nil {
				return err
			}
			for id, entry := range supplement {
				entries[id] = entry
			}
		}
		for id, entry := range entries {
			if _, exists := r.xref[id]; !exists {
				r.xref[id] = entry
			}
		}
		if r.Trailer == nil {
			r.Trailer = trailer
		}
		previous := trailer["Prev"]
		if previous == nil {
			break
		}
		prev, ok := previous.(Integer)
		if !ok {
			return fmt.Errorf("invalid previous cross-reference offset")
		}
		if r.warning != nil && prev == 0 {
			r.warning(Diagnostic{Message: "zero previous cross-reference offset treated as end of chain"})
			break
		}
		offset = int64(prev)
	}
	size, ok := r.Trailer["Size"].(Integer)
	if !ok || size <= 0 {
		return fmt.Errorf("invalid trailer size")
	}
	recovered := false
	for id := range r.xref {
		if id >= int64(size) {
			if r.warning == nil {
				delete(r.xref, id)
			} else {
				recovered = true
			}
		}
	}
	if recovered {
		r.warning(Diagnostic{Message: "cross-reference entries exceed trailer Size; existing entries retained"})
	}
	return nil
}

// scanCrossReferences 顺序验证对象封装后重建索引，不搜索流内容中的伪对象头
// 返回: map[int64]xrefEntry 对象索引, Dictionary 尾字典, error 边界或读取错误
func (r *Reader) scanCrossReferences() (map[int64]xrefEntry, Dictionary, error) {
	if r.recoveryTried {
		if r.recoveryIndex == nil {
			return nil, nil, fmt.Errorf("cross-reference recovery unavailable")
		}
		return r.recoveryIndex, r.Trailer, nil
	}
	r.recoveryTried = true
	if r.size > readerRecoveryLimit {
		return nil, nil, fmt.Errorf("cross-reference recovery scan limit exceeded")
	}
	index := map[int64]xrefEntry{}
	trailer := Dictionary{}
	lengths, err := r.recoveryLengths()
	if err != nil {
		return nil, nil, err
	}
	seen := map[Reference]bool{}
	generations := map[int64]int64{}
	damagedStreamIndex, hasObjectStreams := false, false
	var objectStreams []Reference
	var catalog Reference
	var offset int64
	for offset < r.size {
		var token string
		var ref Reference
		var object Object
		var start, end int64
		err := r.parseAt(offset, func(p *objectParser) error {
			p.skipSpace()
			start = offset + int64(p.pos)
			token = p.token()
			if token == "" && p.pos == len(p.data) {
				end = r.size
				return nil
			}
			if token == "xref" || token == "trailer" || token == "startxref" {
				end = offset + int64(p.pos)
				return nil
			}
			number, e1 := strconv.ParseInt(token, 10, 64)
			generation, e2 := strconv.ParseInt(p.token(), 10, 64)
			header := p.token()
			if e1 == nil && e2 == nil && number == 0 && generation > 0 && (generation <= r.size/18 || (generation <= 65535 && header == "f")) && header != "obj" && len(index) > 0 {
				token, end = "xref", start
				return nil
			}
			if e1 != nil || e2 != nil || number <= 0 || generation < 0 || generation > 65535 || header != "obj" {
				return p.fail("ambiguous object boundary during cross-reference recovery")
			}
			ref = Reference{number, generation}
			var err error
			object, err = p.object()
			if err != nil {
				return err
			}
			token = p.token()
			if token == "stream" {
				if !p.streamDataStart() {
					return p.fail("missing stream line ending during cross-reference recovery")
				}
			} else if token != "endobj" {
				p.pos -= len(token)
				saved := p.pos
				if !p.objectBoundary() {
					return p.fail("missing endobj during cross-reference recovery")
				}
				p.pos = saved
				token = "endobj"
			}
			end = offset + int64(p.pos)
			return nil
		})
		if err != nil {
			return nil, nil, err
		}
		if token == "" {
			break
		}
		if token == "xref" {
			_, entries, xrefErr := r.readXrefAt(start)
			if xrefErr != nil && !readerStructuralError(xrefErr) {
				return nil, nil, xrefErr
			}
			if xrefErr == nil {
				for id, entry := range entries {
					if entry.kind == 0 {
						index[id] = entry
					}
				}
			}
			tableStart := end
			err := r.parseAt(tableStart, func(p *objectParser) error {
				for count := 0; count < 1000000; count++ {
					token := p.token()
					if token == "trailer" {
						end = tableStart + int64(p.pos)
						return nil
					}
					if token == "n" || token == "f" {
						continue
					}
					if _, err := strconv.ParseInt(token, 10, 64); err != nil {
						return p.fail("invalid cross-reference recovery table token")
					}
				}
				return p.fail("cross-reference recovery table limit exceeded")
			})
			if err != nil {
				return nil, nil, err
			}
			token = "trailer"
			clear(seen)
		}
		if token == "trailer" {
			err = r.parseAt(end, func(p *objectParser) error {
				object, err := p.object()
				if err != nil {
					return err
				}
				dict, ok := object.(Dictionary)
				if !ok {
					return p.fail("invalid recovery trailer")
				}
				for key, value := range dict {
					trailer[key] = value
				}
				offset = end + int64(p.pos)
				return nil
			})
			if err != nil {
				return nil, nil, err
			}
			continue
		}
		if token == "startxref" {
			err := r.parseAt(end, func(p *objectParser) error {
				if _, err := strconv.ParseInt(p.token(), 10, 64); err != nil && p.pos != len(p.data) {
					return p.fail("invalid recovery startxref")
				}
				offset = end + int64(p.pos)
				return nil
			})
			if err != nil {
				return nil, nil, err
			}
			continue
		}
		if seen[ref] || len(index) >= 1000000 {
			return nil, nil, fmt.Errorf("ambiguous or excessive recovery objects")
		}
		seen[ref] = true
		if generation, exists := generations[ref.Number]; exists && generation != ref.Generation {
			generations[ref.Number] = -1
		} else if !exists {
			generations[ref.Number] = ref.Generation
		}
		index[ref.Number] = xrefEntry{kind: 1, position: start, generation: ref.Generation}
		if dict, ok := object.(Dictionary); ok {
			if dict["Type"] == Name("ObjStm") {
				hasObjectStreams = true
				objectStreams = append(objectStreams, ref)
			}
			if dict["Type"] == Name("Catalog") {
				if catalog.Number != 0 && catalog != ref {
					catalog = Reference{Number: -1}
				} else {
					catalog = ref
				}
			}
			if token == "stream" {
				length, ok := dict["Length"].(Integer)
				if !ok {
					length = -1
					if ref, ok := dict["Length"].(Reference); ok {
						if value, ok := lengths[ref]; ok {
							length = value
						}
					}
				}
				_, next, err := r.streamExtent(end, int64(length), ref)
				if err != nil {
					return nil, nil, err
				}
				end = next
				if dict["Type"] == Name("XRef") {
					_, entries, err := r.readXrefStream(start)
					if err != nil {
						if !readerStructuralError(err) {
							return nil, nil, err
						}
						damagedStreamIndex = true
						r.warning(Diagnostic{Offset: start, Message: "invalid xref stream index; validated uncompressed object boundaries retained"})
					}
					for id, entry := range entries {
						if entry.kind != 1 {
							index[id] = entry
						}
					}
					for key, value := range dict {
						trailer[key] = value
					}
					clear(seen)
				}
			}
		}
		if end <= offset {
			return nil, nil, fmt.Errorf("non-advancing cross-reference recovery")
		}
		offset = end
	}
	if trailer["Root"] == nil && catalog.Number > 0 {
		trailer["Root"] = catalog
	}
	if trailer["Root"] == nil {
		return nil, nil, fmt.Errorf("missing recovery document catalog")
	}
	if size, ok := trailer["Size"].(Integer); !ok || size <= 0 {
		var size int64
		for id := range index {
			size = max(size, id)
		}
		if size == 1<<63-1 {
			return nil, nil, fmt.Errorf("recovery object number overflow")
		}
		trailer["Size"] = Integer(size + 1)
	}
	for id, entry := range r.xref {
		if entry.kind == 0 {
			index[id] = entry
		}
	}
	if damagedStreamIndex && hasObjectStreams {
		if err := r.rebuildCompressedEntries(index, trailer, objectStreams); err != nil {
			return nil, nil, err
		}
	}
	if root, ok := trailer["Root"].(Reference); ok && root.Number == catalog.Number && root.Generation != catalog.Generation && generations[root.Number] == catalog.Generation {
		entry := index[root.Number]
		if entry.kind == 1 && entry.generation == catalog.Generation {
			trailer["Root"] = catalog
			r.warning(Diagnostic{Offset: entry.position, Message: fmt.Sprintf("stale catalog generation %d recovered as %d from unique object %d", root.Generation, catalog.Generation, catalog.Number)})
		}
	}
	r.recoveryIndex = index
	return index, trailer, nil
}

// rebuildCompressedEntries 从完整且无冲突的对象流头补回缺失索引，不覆盖显式条目或解码未认证数据
// 入参: index 已验证的索引, trailer 尾字典, containers 顺序扫描发现的对象流引用
// 返回: error 歧义、解码、超限或读取错误
func (r *Reader) rebuildCompressedEntries(index map[int64]xrefEntry, trailer Dictionary, containers []Reference) error {
	if trailer["Encrypt"] != nil {
		return fmt.Errorf("encrypted cross-reference recovery requires compressed object index")
	}
	local := *r
	local.xref, local.Trailer = index, trailer
	local.cache, local.loading = map[Reference]Object{}, map[Reference]bool{}
	local.recoveryIndex, local.recoveryTried = index, true
	local.objectStreams = [4]*objectStream{}
	candidates := map[int64]xrefEntry{}
	remaining := int64(readerRecoveryLimit)
	seen := map[Reference]bool{}
	for _, ref := range containers {
		entry := index[ref.Number]
		if entry.kind != 1 || entry.generation != ref.Generation || seen[ref] {
			continue
		}
		seen[ref] = true
		if remaining <= 0 {
			return fmt.Errorf("object stream recovery decode limit exceeded")
		}
		stream, err := local.readObjectStreamLimit(ref, remaining)
		if err != nil {
			return err
		}
		remaining -= stream.bytes
		for i, id := range stream.ids {
			if _, exists := index[id]; exists {
				continue
			}
			if _, exists := candidates[id]; exists {
				return fmt.Errorf("ambiguous recovered compressed object %d", id)
			}
			candidates[id] = xrefEntry{kind: 2, position: ref.Number, generation: int64(i)}
		}
		delete(local.cache, ref)
	}
	for id, entry := range candidates {
		index[id] = entry
	}
	r.warning(Diagnostic{Message: fmt.Sprintf("rebuilt %d missing compressed cross-reference entries from unique object stream indices", len(candidates))})
	return nil
}

// recoveryLengths 收集唯一且完整的整数对象候选，仅用于验证前向流长度引用
// 返回: map[Reference]Integer 无重复候选的长度, error 读取错误
func (r *Reader) recoveryLengths() (map[Reference]Integer, error) {
	values := map[Reference]Integer{}
	positions := map[Reference]int64{}
	for offset := int64(0); offset < r.size; {
		data, err := r.readRange(offset, min(int64(65536), r.size-offset))
		if err != nil {
			return nil, err
		}
		for pos, b := range data {
			if b < '1' || b > '9' || (pos > 0 && data[pos-1] != '\n' && data[pos-1] != '\r') {
				continue
			}
			p := objectParser{data: data[pos:min(len(data), pos+128)]}
			number, e1 := strconv.ParseInt(p.token(), 10, 64)
			generation, e2 := strconv.ParseInt(p.token(), 10, 64)
			if e1 != nil || e2 != nil || number <= 0 || generation < 0 || generation > 65535 || p.token() != "obj" {
				continue
			}
			value, e3 := strconv.ParseInt(p.token(), 10, 64)
			if e3 != nil || value < 0 || p.token() != "endobj" || p.exhausted {
				continue
			}
			ref := Reference{number, generation}
			at := offset + int64(pos)
			if len(positions) >= 1000000 {
				return nil, fmt.Errorf("stream length recovery candidate limit exceeded")
			}
			if previous, ok := positions[ref]; ok && previous != at {
				delete(values, ref)
			} else if !ok {
				positions[ref], values[ref] = at, Integer(value)
			}
		}
		if offset+int64(len(data)) == r.size {
			break
		}
		offset += int64(len(data)) - 256
	}
	return values, nil
}

// readRange 按需读取对象片段并检查文件边界和平台整数溢出
// 入参: offset 起始字节偏移, length 读取字节数
// 返回: []byte 读取的数据, error 范围或读取错误
func (r *Reader) readRange(offset, length int64) ([]byte, error) {
	if offset < 0 || length < 0 || offset > r.size || length > r.size-offset || uint64(length) > uint64(^uint(0)>>1) {
		return nil, fmt.Errorf("invalid file range")
	}
	data := make([]byte, int(length))
	n, err := io.ReadFull(io.NewSectionReader(r.source, offset, length), data)
	if err != nil {
		return data, &ReadError{Offset: offset + int64(n), Err: err}
	}
	return data, nil
}

// parseAt 按需扩展单个对象的解析窗口，不复制整个源文件
// 入参: offset 对象起始字节偏移, parse 解析回调，窗口扩展后从头重试
// 返回: error 读取或解析错误
func (r *Reader) parseAt(offset int64, parse func(*objectParser) error) error {
	if offset < 0 || offset >= r.size {
		return fmt.Errorf("object offset outside file")
	}
	remaining := r.size - offset
	remaining = min(remaining, int64(readerRecoveryLimit))
	var data []byte
	for length := min(remaining, int64(512)); ; length = min(remaining, length+min(length, remaining-length)) {
		if uint64(length) > uint64(^uint(0)>>1) {
			return fmt.Errorf("invalid file range")
		}
		previous := len(data)
		window := make([]byte, int(length))
		copy(window, data)
		if n, err := io.ReadFull(io.NewSectionReader(r.source, offset+int64(previous), length-int64(previous)), window[previous:]); err != nil {
			return &ReadError{Offset: offset + int64(previous+n), Err: err}
		}
		data = window
		p := objectParser{data: data, final: offset+length == r.size}
		var warnings []Diagnostic
		if r.warning != nil {
			p.warning = func(d Diagnostic) {
				d.Offset += offset
				warnings = append(warnings, d)
			}
		}
		err := parse(&p)
		if (p.exhausted || int64(p.pos) >= length-1) && length < remaining {
			continue
		}
		for _, warning := range warnings {
			r.warning(warning)
		}
		var syntax *SyntaxError
		if errors.As(err, &syntax) {
			return &SyntaxError{Offset: offset + syntax.Offset, Message: syntax.Message}
		}
		return err
	}
}

// indirect 从文件偏移读取完整间接对象
// 入参: offset 对象起始字节偏移
// 返回: Reference 对象引用, Object 对象内容, error 读取或解析错误
func (r *Reader) indirect(offset int64) (Reference, Object, error) {
	var p objectParser
	var ref Reference
	var value Object
	var keyword string
	var objectEnd int
	var boundary bool
	var streamStart bool
	err := r.parseAt(offset, func(input *objectParser) error {
		p = *input
		defer func() { input.pos, input.exhausted = p.pos, p.exhausted }()
		number, e1 := strconv.ParseInt(p.token(), 10, 64)
		generation, e2 := strconv.ParseInt(p.token(), 10, 64)
		if e1 != nil || e2 != nil || number <= 0 || generation < 0 || generation > 65535 || p.token() != "obj" {
			return p.fail("invalid indirect object header")
		}
		ref = Reference{number, generation}
		var err error
		value, err = p.object()
		if err != nil {
			return err
		}
		objectEnd = p.pos
		keyword = p.token()
		if keyword == "" && p.final && p.pos == len(p.data) && p.warning != nil {
			p.warning(Diagnostic{Offset: int64(p.pos), Message: "indirect object terminated at source EOF; parsed value retained"})
			keyword = "endobj"
		}
		boundary = false
		if keyword != "endobj" && keyword != "stream" && r.warning != nil {
			next := objectParser{data: p.data, pos: objectEnd}
			boundary = next.objectBoundary()
			p.exhausted = p.exhausted || next.exhausted
		}
		if keyword == "stream" {
			streamStart = p.streamDataStart()
		}
		return nil
	})
	if err != nil {
		return Reference{}, nil, err
	}
	if keyword == "stream" {
		dict, ok := value.(Dictionary)
		if !ok {
			return ref, nil, p.fail("stream requires dictionary")
		}
		if !streamStart {
			return ref, nil, p.fail("missing stream line ending")
		}
		lengthObject, err := r.Resolve(dict["Length"])
		if err != nil {
			return ref, nil, err
		}
		length, ok := lengthObject.(Integer)
		start := offset + int64(p.pos)
		if (!ok || length < 0 || int64(length) > r.size-start) && r.warning == nil {
			return ref, nil, p.fail("invalid stream length")
		}
		if !ok {
			length = -1
		}
		actualLength, _, err := r.streamExtent(start, int64(length), ref)
		if err != nil {
			return ref, nil, err
		}
		data, err := r.readRange(start, actualLength)
		if err != nil {
			return ref, nil, err
		}
		value = &Stream{Dictionary: dict, Data: data, reader: r}
		err = r.parseAt(start+actualLength, func(tail *objectParser) error {
			token := tail.token()
			if token != "endstream" && (r.warning == nil || !streamEndTypo(token)) {
				return tail.fail("missing endstream")
			}
			end := tail.pos
			if tail.token() != "endobj" {
				if tail.warning != nil {
					tail.pos = end
					if tail.objectBoundary() {
						tail.warning(Diagnostic{Offset: int64(end), Message: "missing endobj before the next object boundary"})
						return nil
					}
				}
				return tail.fail("missing endobj")
			}
			return nil
		})
		return ref, value, err
	}
	if keyword != "endobj" {
		if boundary {
			r.warning(Diagnostic{Offset: offset + int64(objectEnd), Message: "missing endobj before the next object boundary"})
			return ref, value, nil
		}
		return ref, nil, &SyntaxError{Offset: offset + int64(p.pos), Message: "missing endobj"}
	}
	return ref, value, nil
}

// streamExtent 校验声明的流长度，恢复时只接受有完整结束封装的唯一候选
// 入参: start 流数据起点, length 声明长度，负值表示缺失
// 返回: int64 实际长度, int64 封装末尾, error 读取或边界错误
func (r *Reader) streamExtent(start, length int64, ref Reference) (int64, int64, error) {
	var end int64
	var original error
	if length >= 0 && length < r.size-start {
		original = r.parseAt(start+length, func(p *objectParser) error {
			token := p.token()
			typo := token != "endstream" && r.warning != nil && streamEndTypo(token)
			if token != "endstream" && !typo {
				return p.fail("missing endstream")
			}
			saved := p.pos
			if p.token() != "endobj" {
				p.pos = saved
				if r.warning == nil || typo || !p.objectBoundary() {
					return p.fail("missing endobj")
				}
				p.pos = saved
			}
			end = start + length + int64(p.pos)
			if typo {
				p.warning(Diagnostic{Offset: int64(p.pos), Message: "misspelled endstream at declared Length with complete endobj boundary"})
			}
			return nil
		})
		if original == nil {
			return length, end, nil
		}
	} else {
		original = &SyntaxError{Offset: start, Message: "invalid stream length"}
	}
	if r.warning == nil || !readerStructuralError(original) {
		return 0, 0, original
	}
	limit := min(r.size, start+(16<<20))
	base := start
	if entry, ok := r.xref[ref.Number]; ok && entry.kind == 1 && entry.generation == ref.Generation {
		base = entry.position
	}
	var expected Reference
	nextPosition := int64(1<<63 - 1)
	for number, entry := range r.xref {
		if entry.kind == 1 && entry.position > base && entry.position < nextPosition {
			nextPosition = entry.position
			expected = Reference{number, entry.generation}
		}
	}
	if nextPosition > start && nextPosition < limit {
		limit = min(limit, nextPosition+4096)
	}
	var candidate int64 = -1
	var candidateEnd int64
	markers := 0
	for offset := start; offset < limit; {
		data, err := r.readRange(offset, min(int64(65536), limit-offset))
		if err != nil {
			return 0, 0, err
		}
		for pos := 0; pos < len(data); {
			found := bytes.Index(data[pos:], []byte("endstream"))
			if found < 0 {
				break
			}
			found += pos
			pos = found + 9
			markers++
			if markers > 4096 {
				return 0, 0, fmt.Errorf("stream boundary recovery candidate limit exceeded")
			}
			at := offset + int64(found)
			var next int64
			err := r.parseAt(at, func(p *objectParser) error {
				if p.token() != "endstream" || p.token() != "endobj" {
					return p.fail("invalid recovered stream boundary")
				}
				next = at + int64(p.pos)
				p.skipSpace()
				if p.pos == len(p.data) && at+int64(p.pos) == r.size {
					if expected.Number != 0 {
						return p.fail("expected indexed object after recovered stream")
					}
					return nil
				}
				if expected.Number == 0 && !p.objectBoundary() {
					return p.fail("invalid object after recovered stream boundary")
				}
				if expected.Number != 0 {
					number, e1 := strconv.ParseInt(p.token(), 10, 64)
					generation, e2 := strconv.ParseInt(p.token(), 10, 64)
					if e1 != nil || e2 != nil || number != expected.Number || generation != expected.Generation || p.token() != "obj" {
						return p.fail("recovered stream is not followed by the next indexed object")
					}
					object, err := p.object()
					if err != nil {
						return err
					}
					token := p.token()
					if token != "endobj" && token != "stream" {
						return p.fail("invalid next indexed object envelope")
					}
					if token == "stream" {
						dict, ok := object.(Dictionary)
						if !ok || !p.streamDataStart() {
							return p.fail("invalid next indexed stream header")
						}
						value, err := r.Resolve(dict["Length"])
						if err != nil {
							return err
						}
						length, ok := value.(Integer)
						dataStart := at + int64(p.pos)
						if !ok || length < 0 || int64(length) >= r.size-dataStart {
							return p.fail("unverified next indexed stream length")
						}
						return r.parseAt(dataStart+int64(length), func(tail *objectParser) error {
							if tail.token() != "endstream" || tail.token() != "endobj" {
								return tail.fail("unverified next indexed stream ending")
							}
							tail.skipSpace()
							if tail.pos == len(tail.data) && tail.final || tail.objectBoundary() {
								return nil
							}
							return tail.fail("unverified boundary after next indexed stream")
						})
					}
					p.skipSpace()
					if !(p.pos == len(p.data) && p.final) && !p.objectBoundary() {
						return p.fail("unverified boundary after next indexed object")
					}
				}
				return nil
			})
			if err != nil {
				if !readerStructuralError(err) {
					return 0, 0, err
				}
				continue
			}
			if candidate >= 0 && candidate != at {
				return 0, 0, fmt.Errorf("ambiguous recovered stream boundary")
			}
			candidate, candidateEnd = at, next
		}
		if offset+int64(len(data)) == limit {
			break
		}
		offset += int64(len(data)) - 16
	}
	if candidate < 0 {
		return 0, 0, original
	}
	actual := candidate - start
	if actual > 0 {
		data, err := r.readRange(start+max(int64(0), actual-2), min(int64(2), actual))
		if err != nil {
			return 0, 0, err
		}
		if len(data) > 0 && data[len(data)-1] == '\n' {
			actual--
			if len(data) > 1 && data[len(data)-2] == '\r' {
				actual--
			}
		} else if len(data) > 0 && data[len(data)-1] == '\r' {
			actual--
		}
	}
	r.warning(Diagnostic{Offset: start, Message: fmt.Sprintf("stream Length %d replaced by validated boundary length %d", length, actual)})
	return actual, candidateEnd, nil
}

// streamEndTypo 识别endstream关键字仅缺失一个字母的拼写，不匹配任意词
// 入参: token 实际流尾关键字
// 返回: bool 是否为单字母缺失
func streamEndTypo(token string) bool {
	const keyword = "endstream"
	if len(token) != len(keyword)-1 {
		return false
	}
	for i := range keyword {
		if token == keyword[:i]+keyword[i+1:] {
			return true
		}
	}
	return false
}

// objectBoundary 检查后续对象头或交叉引用边界，不解析后续对象内容
// 返回: bool 是否位于明确的对象边界
func (p *objectParser) objectBoundary() bool {
	token := p.token()
	if token == "xref" || token == "trailer" || token == "startxref" {
		return true
	}
	number, err := strconv.ParseInt(token, 10, 64)
	if err != nil || number <= 0 {
		return false
	}
	generation, err := strconv.ParseInt(p.token(), 10, 64)
	return err == nil && generation >= 0 && generation <= 65535 && p.token() == "obj"
}

// readXref 读取传统或流式交叉引用区段
// 入参: offset 交叉引用区段的字节偏移
// 返回: Dictionary 尾字典或交叉引用流字典, map[int64]xrefEntry 交叉引用条目, error 读取或解析错误
func (r *Reader) readXref(offset int64) (Dictionary, map[int64]xrefEntry, error) {
	trailer, entries, original := r.readXrefAt(offset)
	if original == nil || r.warning == nil || !readerStructuralError(original) {
		return trailer, entries, original
	}
	start, stop := max(int64(0), offset-1024), min(r.size, offset+1024)
	if offset < 0 || offset >= r.size {
		start, stop = max(int64(0), r.size-readerTailLimit), r.size
	}
	for _, region := range []struct {
		start, stop int64
		claim       bool
	}{{start, stop, false}, {max(int64(0), r.size-readerTailLimit), r.size, true}} {
		trailer, entries, candidate, err := r.findXrefOffset(offset, region.start, region.stop, region.claim)
		if err != nil {
			return nil, nil, err
		}
		if candidate >= 0 {
			if err := r.rebaseXrefEntries(offset, candidate, trailer, entries); err != nil {
				if !readerStructuralError(err) {
					return nil, nil, err
				}
				continue
			}
			r.warning(Diagnostic{Offset: candidate, Message: fmt.Sprintf("cross-reference offset %d recovered at %d", offset, candidate)})
			return trailer, entries, nil
		}
	}
	return nil, nil, original
}

// rebaseXrefEntries 验证嵌入PDF文件头及全部在用对象头后修正相对该文件头的xref偏移
// 入参: declared 声明xref偏移, actual 实际xref偏移, trailer 尾字典, entries 在用索引
// 返回: error 源错误或不能一致验证的偏移
func (r *Reader) rebaseXrefEntries(declared, actual int64, trailer Dictionary, entries map[int64]xrefEntry) error {
	base := actual - declared
	if base <= 0 || base > r.size-8 {
		return nil
	}
	header, err := r.readRange(base, 8)
	if err != nil {
		return err
	}
	if !bytes.HasPrefix(header, []byte("%PDF-")) {
		return nil
	}
	if len(entries) > 100000 {
		return fmt.Errorf("relative cross-reference validation limit exceeded")
	}
	for number, entry := range entries {
		if number == 0 || entry.kind != 1 {
			continue
		}
		if entry.position < 0 || entry.position >= r.size-base {
			return fmt.Errorf("relative cross-reference entry outside file")
		}
		data, err := r.readRange(base+entry.position, min(int64(96), r.size-base-entry.position))
		if err != nil {
			return err
		}
		p := objectParser{data: data}
		id, e1 := strconv.ParseInt(p.token(), 10, 64)
		generation, e2 := strconv.ParseInt(p.token(), 10, 64)
		if e1 != nil || e2 != nil || id != number || generation != entry.generation || p.token() != "obj" {
			return fmt.Errorf("inconsistent relative cross-reference object header")
		}
	}
	for number, entry := range entries {
		if entry.kind == 1 && number != 0 {
			entry.position += base
			entries[number] = entry
		}
	}
	for _, key := range []Name{"Prev", "XRefStm"} {
		if value, ok := trailer[key].(Integer); ok && value > 0 {
			if int64(value) >= r.size-base {
				return fmt.Errorf("relative cross-reference chain outside file")
			}
			trailer[key] = value + Integer(base)
		}
	}
	r.warning(Diagnostic{Offset: base, Message: "xref offsets relative to embedded PDF header validated and rebased"})
	return nil
}

// findXrefOffset 在限定范围校验唯一xref候选，远距离修复须与该区段的startxref声明相互印证
// 入参: offset 错误声明偏移, start 起点, stop 终点, claim 是否要求尾部声明相符
// 返回: Dictionary 尾字典, map[int64]xrefEntry 索引, int64 唯一位置或负值, error 源错误
func (r *Reader) findXrefOffset(offset, start, stop int64, claim bool) (Dictionary, map[int64]xrefEntry, int64, error) {
	data, err := r.readRange(start, stop-start)
	if err != nil {
		return nil, nil, -1, err
	}
	var trailer Dictionary
	var entries map[int64]xrefEntry
	candidate := int64(-1)
	attempts := 0
	for pos, b := range data {
		if pos > 0 && !isSpace(data[pos-1]) {
			continue
		}
		at := start + int64(pos)
		if at == offset || (b != 'x' && (b < '1' || b > '9')) {
			continue
		}
		p := objectParser{data: data[pos:min(len(data), pos+512)]}
		token := p.token()
		if token != "xref" {
			if _, err := strconv.ParseInt(token, 10, 64); err != nil {
				continue
			}
			if _, err := strconv.ParseInt(p.token(), 10, 64); err != nil || p.token() != "obj" {
				continue
			}
			object, err := p.object()
			dict, ok := object.(Dictionary)
			if err != nil || !ok || dict["Type"] != Name("XRef") {
				continue
			}
		}
		attempts++
		if attempts > 256 {
			return nil, nil, -1, nil
		}
		if claim {
			matches, err := r.xrefClaimsOffset(at, offset)
			if err != nil {
				return nil, nil, -1, err
			}
			if !matches {
				continue
			}
		}
		dict, found, err := r.readXrefAt(at)
		if err != nil {
			if !readerStructuralError(err) {
				return nil, nil, -1, err
			}
			continue
		}
		if candidate >= 0 {
			return nil, nil, -1, nil
		}
		candidate, trailer, entries = at, dict, found
	}
	return trailer, entries, candidate, nil
}

// xrefClaimsOffset 核对完整xref封装后紧接的startxref数值，不把旧版本区段误作当前尾部
// 入参: at 候选实际位置, offset 待恢复声明位置
// 返回: bool 声明是否相符, error 源读取错误
func (r *Reader) xrefClaimsOffset(at, offset int64) (bool, error) {
	var stream bool
	var start, end int64
	var ref Reference
	var length Object
	err := r.parseAt(at, func(p *objectParser) error {
		if p.token() == "xref" {
			p.pos = 0
			if _, _, err := r.classicXref(p); err != nil {
				return err
			}
			end = at + int64(p.pos)
			return nil
		}
		p.pos = 0
		number, e1 := strconv.ParseInt(p.token(), 10, 64)
		generation, e2 := strconv.ParseInt(p.token(), 10, 64)
		if e1 != nil || e2 != nil || p.token() != "obj" {
			return p.fail("invalid candidate xref header")
		}
		ref = Reference{number, generation}
		object, err := p.object()
		dict, ok := object.(Dictionary)
		if err != nil || !ok || dict["Type"] != Name("XRef") || p.token() != "stream" || !p.streamDataStart() {
			return p.fail("invalid candidate xref stream")
		}
		stream, start, length = true, at+int64(p.pos), dict["Length"]
		return nil
	})
	if err == nil && stream {
		value, e := r.Resolve(length)
		if e != nil {
			err = e
		} else if length, ok := value.(Integer); ok {
			_, end, err = r.streamExtent(start, int64(length), ref)
		} else {
			err = fmt.Errorf("invalid candidate xref stream length")
		}
	}
	matches := false
	if err == nil {
		err = r.parseAt(end, func(p *objectParser) error {
			if p.token() != "startxref" {
				return p.fail("missing candidate xref ownership declaration")
			}
			declared, err := strconv.ParseInt(p.token(), 10, 64)
			matches = err == nil && declared == offset
			return err
		})
	}
	if err != nil && !readerStructuralError(err) {
		return false, err
	}
	return err == nil && matches, nil
}

// readXrefAt 读取指定位置的传统或流式交叉引用，不重新定位
// 入参: offset 交叉引用区段的字节偏移
// 返回: Dictionary 尾字典, map[int64]xrefEntry 交叉引用条目, error 读取或解析错误
func (r *Reader) readXrefAt(offset int64) (Dictionary, map[int64]xrefEntry, error) {
	if offset < 0 || offset >= r.size {
		return nil, nil, fmt.Errorf("cross-reference offset outside file")
	}
	data, err := r.readRange(offset, min(16, r.size-offset))
	probe := objectParser{data: data}
	if err != nil {
		return nil, nil, err
	}
	if probe.token() != "xref" {
		return r.readXrefStream(offset)
	}
	var trailer Dictionary
	var result map[int64]xrefEntry
	err = r.parseAt(offset, func(p *objectParser) error {
		var err error
		trailer, result, err = r.classicXref(p)
		return err
	})
	return trailer, result, err
}

// classicXref 解析传统交叉引用及其尾字典
// 入参: p 位于xref关键字的解析器
// 返回: Dictionary 尾字典, map[int64]xrefEntry 交叉引用条目, error 语法错误
func (r *Reader) classicXref(p *objectParser) (Dictionary, map[int64]xrefEntry, error) {
	p.token()
	entries := map[int64]xrefEntry{}
	for {
		token := p.token()
		if token == "trailer" {
			obj, err := p.object()
			if err != nil {
				return nil, nil, err
			}
			dict, ok := obj.(Dictionary)
			if !ok {
				return nil, nil, p.fail("invalid trailer")
			}
			return dict, entries, nil
		}
		first, e1 := strconv.ParseInt(token, 10, 64)
		count, e2 := strconv.ParseInt(p.token(), 10, 64)
		if e1 != nil || e2 != nil || first < 0 || count < 0 || count > r.size/18 || first > (1<<63-1)-count {
			return nil, nil, p.fail("invalid cross-reference subsection")
		}
		for i := int64(0); i < count; i++ {
			position, e1 := strconv.ParseInt(p.token(), 10, 64)
			generation, e2 := strconv.ParseInt(p.token(), 10, 64)
			state := p.token()
			if first+i == 0 && state == "f" && generation == 65536 {
				generation = 65535
			}
			if e1 != nil || e2 != nil || position < 0 || generation < 0 || generation > 65535 || (state != "f" && state != "n") {
				return nil, nil, p.fail("invalid cross-reference entry")
			}
			kind := int64(0)
			if state == "n" {
				kind = 1
			}
			if _, exists := entries[first+i]; exists {
				return nil, nil, p.fail("overlapping cross-reference subsections")
			}
			entries[first+i] = xrefEntry{kind, position, generation}
		}
	}
}

// readXrefStream 读取交叉引用流并检查字段边界
// 入参: offset 交叉引用流对象的字节偏移
// 返回: Dictionary 交叉引用流字典, map[int64]xrefEntry 交叉引用条目, error 读取或解析错误
func (r *Reader) readXrefStream(offset int64) (Dictionary, map[int64]xrefEntry, error) {
	ref, object, err := r.indirect(offset)
	if err != nil {
		return nil, nil, err
	}
	stream, ok := object.(*Stream)
	if !ok {
		return nil, nil, &SyntaxError{Offset: offset, Message: fmt.Sprintf("invalid cross-reference stream: object %d %d is not a stream", ref.Number, ref.Generation)}
	}
	if stream.Dictionary["Type"] == nil {
		return nil, nil, &SyntaxError{Offset: offset, Message: fmt.Sprintf("invalid cross-reference stream: object %d %d is missing /Type /XRef", ref.Number, ref.Generation)}
	}
	if stream.Dictionary["Type"] != Name("XRef") {
		return nil, nil, &SyntaxError{Offset: offset, Message: fmt.Sprintf("invalid cross-reference stream: object %d %d requires /Type /XRef", ref.Number, ref.Generation)}
	}
	dict := stream.Dictionary
	widths, ok := dict["W"].(Array)
	if !ok || len(widths) != 3 {
		return nil, nil, fmt.Errorf("invalid cross-reference widths")
	}
	var w [3]int
	for i, object := range widths {
		n, ok := object.(Integer)
		if !ok || n < 0 || n > 8 {
			return nil, nil, fmt.Errorf("invalid cross-reference field width")
		}
		w[i] = int(n)
	}
	stride := w[0] + w[1] + w[2]
	if stride == 0 {
		return nil, nil, fmt.Errorf("empty cross-reference entry")
	}
	size, ok := dict["Size"].(Integer)
	if !ok || size <= 0 {
		return nil, nil, fmt.Errorf("invalid cross-reference size")
	}
	index := Array{Integer(0), size}
	if obj := dict["Index"]; obj != nil {
		var ok bool
		index, ok = obj.(Array)
		if !ok || len(index)%2 != 0 {
			return nil, nil, fmt.Errorf("invalid cross-reference index")
		}
	}
	direct := *stream
	direct.reader = nil
	data, err := direct.decodeContext(context.Background(), r.warning)
	if err != nil {
		return nil, nil, err
	}
	entries := map[int64]xrefEntry{}
	pos := 0
	for i := 0; i < len(index); i += 2 {
		first, ok1 := index[i].(Integer)
		count, ok2 := index[i+1].(Integer)
		if !ok1 || !ok2 || first < 0 || count < 0 || first > Integer(1<<63-1)-count || int64(count) > int64((len(data)-pos)/stride) {
			return nil, nil, fmt.Errorf("invalid cross-reference range")
		}
		if count > size || first > size-count {
			if r.warning == nil {
				return nil, nil, fmt.Errorf("invalid cross-reference range")
			}
			r.warning(Diagnostic{Offset: offset, Message: "cross-reference Index exceeds Size; complete entries retained"})
		}
		for j := Integer(0); j < count; j++ {
			fields := [3]int64{1, 0, 0}
			for k, width := range w {
				if width == 0 {
					continue
				}
				v := uint64(0)
				for range width {
					v = v<<8 | uint64(data[pos])
					pos++
				}
				if v > 1<<63-1 {
					return nil, nil, fmt.Errorf("cross-reference field overflow")
				}
				fields[k] = int64(v)
			}
			id := int64(first + j)
			if _, exists := entries[id]; exists {
				return nil, nil, fmt.Errorf("overlapping cross-reference index")
			}
			entry := xrefEntry{fields[0], fields[1], fields[2]}
			if entry.kind > 2 {
				entry.kind = 0
			}
			entries[id] = entry
		}
	}
	if pos != len(data) {
		return nil, nil, fmt.Errorf("excess cross-reference data")
	}
	return dict, entries, nil
}

// compressedObject 从对象流读取指定压缩对象
// 入参: ref 对象引用, entry 对应的交叉引用条目
// 返回: Object 解压后的对象, error 读取或解析错误
func (r *Reader) compressedObject(ref Reference, entry xrefEntry) (Object, error) {
	containerEntry, ok := r.xref[entry.position]
	if !ok || containerEntry.kind != 1 {
		return nil, fmt.Errorf("invalid object stream reference")
	}
	container := Reference{entry.position, containerEntry.generation}
	var stream *objectStream
	for i, cached := range r.objectStreams {
		if cached != nil && cached.reference == container {
			stream = cached
			copy(r.objectStreams[1:i+1], r.objectStreams[:i])
			r.objectStreams[0] = stream
			break
		}
	}
	if stream == nil {
		var err error
		stream, err = r.readObjectStream(container)
		if err != nil {
			return nil, err
		}
		copy(r.objectStreams[1:], r.objectStreams[:len(r.objectStreams)-1])
		r.objectStreams[0] = stream
		used := stream.bytes
		for i, cached := range r.objectStreams[1:] {
			if cached == nil {
				break
			}
			used += cached.bytes
			if used > objectStreamCacheLimit {
				clear(r.objectStreams[i+1:])
				break
			}
		}
	}
	index := entry.generation
	if index < 0 || index >= int64(len(stream.ids)) || stream.ids[index] != ref.Number {
		if r.warning == nil {
			return nil, fmt.Errorf("object stream index mismatch")
		}
		var found bool
		index, found = stream.indices[ref.Number]
		if !found {
			return nil, fmt.Errorf("object stream index mismatch")
		}
		r.warning(Diagnostic{Message: fmt.Sprintf("object stream index %d recovered as %d for object %d in stream %d", entry.generation, index, ref.Number, container.Number)})
	}
	p := objectParser{data: stream.data[stream.offsets[index]:stream.offsets[index+1]], final: true}
	if r.warning != nil {
		p.warning = func(d Diagnostic) {
			d.Message = fmt.Sprintf("compressed object %d %d: %s", ref.Number, ref.Generation, d.Message)
			d.Offset = 0
			r.warning(d)
		}
	}
	value, err := p.object()
	if err != nil {
		return nil, err
	}
	p.skipSpace()
	if p.pos != len(p.data) {
		if r.warning == nil || p.token() != "endobj" {
			return nil, fmt.Errorf("excess compressed object data")
		}
		p.skipSpace()
		if p.pos != len(p.data) {
			return nil, fmt.Errorf("excess compressed object data")
		}
		r.warning(Diagnostic{Message: fmt.Sprintf("redundant endobj in compressed object %d %d", ref.Number, ref.Generation)})
	}
	return value, nil
}

// readObjectStream 解码对象流并校验全部索引，内容仍按引用逐项解析
// 入参: ref 对象流引用
// 返回: *objectStream 对象流数据, error 错误信息
func (r *Reader) readObjectStream(ref Reference) (*objectStream, error) {
	return r.readObjectStreamLimit(ref, 0)
}

// readObjectStreamLimit 在重建时限制解码和索引内存，零上限沿用普通读取
// 入参: ref 对象流引用, limit 解码及索引总字节上限
// 返回: *objectStream 对象流数据, error 错误信息
func (r *Reader) readObjectStreamLimit(ref Reference, limit int64) (*objectStream, error) {
	object, err := r.Object(ref)
	if err != nil {
		return nil, err
	}
	stream, ok := object.(*Stream)
	if !ok || stream.Dictionary["Type"] != Name("ObjStm") {
		return nil, fmt.Errorf("missing object stream")
	}
	count, e1 := integerDefault(stream.Dictionary, "N", -1)
	first, e2 := integerDefault(stream.Dictionary, "First", -1)
	if e1 != nil || e2 != nil || count <= 0 || first < 0 {
		return nil, fmt.Errorf("invalid object stream header")
	}
	data, err := stream.decodeContextLimit(context.Background(), r.warning, limit)
	if err != nil {
		return nil, err
	}
	if first > int64(len(data)) || count > first/4 {
		return nil, fmt.Errorf("invalid object stream dimensions")
	}
	if limit > 0 && (int64(cap(data))+8 > limit || count > (limit-int64(cap(data))-8)/80) {
		return nil, fmt.Errorf("object stream recovery index limit exceeded")
	}
	p := objectParser{data: data[:first]}
	ids, offsets := make([]int64, count), make([]int64, count+1)
	indices := make(map[int64]int64, count)
	for i := int64(0); i < count; i++ {
		ids[i], e1 = strconv.ParseInt(p.token(), 10, 64)
		offsets[i], e2 = strconv.ParseInt(p.token(), 10, 64)
		if e1 != nil || e2 != nil || ids[i] <= 0 || offsets[i] < 0 || offsets[i] > int64(len(data))-first || (i > 0 && offsets[i] <= offsets[i-1]) {
			return nil, fmt.Errorf("invalid object stream index")
		}
		if _, exists := indices[ids[i]]; exists {
			return nil, fmt.Errorf("duplicate object stream number")
		}
		indices[ids[i]] = i
	}
	p.skipSpace()
	if p.pos != len(p.data) {
		return nil, fmt.Errorf("object stream index mismatch")
	}
	offsets[count] = int64(len(data)) - first
	return &objectStream{reference: ref, data: data[first:], ids: ids, indices: indices, offsets: offsets, bytes: int64(cap(data)) + 8*(count*2+1) + 64*count}, nil
}
