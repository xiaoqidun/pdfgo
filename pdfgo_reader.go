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
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
)

// Reader 读取PDF对象，方法不可并发调用，返回对象不可并发修改
type Reader struct {
	Version            string
	Trailer            Dictionary
	source             io.ReaderAt
	size               int64
	closer             io.Closer
	xref               map[int64]xrefEntry
	cache              map[Reference]Object
	loading            map[Reference]bool
	destinations       map[string]Object
	legacyDestinations Dictionary
	fonts              map[Reference]*Font
	objectStream       *objectStream
}

// objectStream 保留最近访问的对象流索引和解码数据，避免逐对象重复解压
type objectStream struct {
	reference Reference
	data      []byte
	ids       []int64
	offsets   []int64
}

// xrefEntry 保存交叉引用类型、文件偏移或对象流索引
type xrefEntry struct {
	kind       int64
	position   int64
	generation int64
}

// Open 打开PDF文件并读取交叉引用
// 入参: path 文件路径
// 返回: *Reader 阅读器, error 错误信息
func Open(path string) (*Reader, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	info, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, err
	}
	r, err := NewReader(f, info.Size())
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
	if size < 8 {
		return nil, fmt.Errorf("invalid file size")
	}
	data := make([]byte, min(size, 1024))
	if _, err := io.ReadFull(io.NewSectionReader(source, 0, size), data); err != nil {
		return nil, err
	}
	header := bytes.Index(data[:min(len(data), 1024)], []byte("%PDF-"))
	if header < 0 {
		return nil, fmt.Errorf("missing PDF header")
	}
	data = data[header:]
	end := bytes.IndexAny(data[:min(len(data), 32)], "\r\n")
	if end < 0 {
		return nil, fmt.Errorf("invalid PDF header")
	}
	version := string(bytes.TrimSpace(data[5:end]))
	if version != "2.0" && (len(version) != 3 || version[0:2] != "1." || version[2] < '0' || version[2] > '7') {
		return nil, &UnsupportedError{Feature: "PDF version " + version}
	}
	r := &Reader{Version: version, source: io.NewSectionReader(source, int64(header), size-int64(header)), size: size - int64(header), xref: map[int64]xrefEntry{}, cache: map[Reference]Object{}, loading: map[Reference]bool{}}
	var err error
	for length := min(r.size, int64(4096)); ; length = min(r.size, length+min(length, r.size-length)) {
		data, err = r.readRange(r.size-length, length)
		if err != nil {
			return nil, err
		}
		if bytes.Contains(data, []byte("startxref")) || length == r.size {
			break
		}
	}
	start := bytes.LastIndex(data, []byte("startxref"))
	if start < 0 {
		return nil, fmt.Errorf("missing startxref")
	}
	p := objectParser{data: data, pos: start + 9}
	offset, err := strconv.ParseInt(p.token(), 10, 64)
	if err != nil {
		return nil, p.fail("invalid startxref offset")
	}
	if !bytes.Equal(bytes.TrimSpace(data[p.pos:]), []byte("%%EOF")) {
		return nil, p.fail("missing final EOF marker")
	}
	visited := map[int64]bool{}
	for {
		if visited[offset] {
			return nil, fmt.Errorf("cyclic cross-reference chain")
		}
		visited[offset] = true
		trailer, entries, err := r.readXref(offset)
		if err != nil {
			return nil, err
		}
		if hybrid, ok := trailer["XRefStm"].(Integer); ok {
			if visited[int64(hybrid)] {
				return nil, fmt.Errorf("cyclic hybrid cross-reference")
			}
			visited[int64(hybrid)] = true
			_, supplement, err := r.readXref(int64(hybrid))
			if err != nil {
				return nil, err
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
			return nil, fmt.Errorf("invalid previous cross-reference offset")
		}
		offset = int64(prev)
	}
	if r.Trailer["Encrypt"] != nil {
		return nil, &UnsupportedError{Feature: "encrypted PDF"}
	}
	sizeObject, ok := r.Trailer["Size"].(Integer)
	if !ok || sizeObject <= 0 {
		return nil, fmt.Errorf("invalid trailer size")
	}
	for id := range r.xref {
		if id >= int64(sizeObject) {
			delete(r.xref, id)
		}
	}
	root, err := r.Resolve(r.Trailer["Root"])
	if err != nil {
		return nil, err
	}
	catalog, ok := root.(Dictionary)
	if !ok || catalog["Type"] != Name("Catalog") {
		return nil, fmt.Errorf("missing document catalog")
	}
	if v, ok := catalog["Version"].(Name); ok && string(v) > r.Version {
		r.Version = string(v)
	}
	return r, nil
}

// Close 关闭Open持有的文件，不关闭NewReader的外部数据源
// 返回: error 关闭错误
func (r *Reader) Close() error {
	if r.closer == nil {
		return nil
	}
	err := r.closer.Close()
	r.closer = nil
	return err
}

// readRange 按需读取对象片段并检查文件边界和平台整数溢出
func (r *Reader) readRange(offset, length int64) ([]byte, error) {
	if offset < 0 || length < 0 || offset > r.size || length > r.size-offset || uint64(length) > uint64(^uint(0)>>1) {
		return nil, fmt.Errorf("invalid file range")
	}
	data := make([]byte, int(length))
	_, err := io.ReadFull(io.NewSectionReader(r.source, offset, length), data)
	return data, err
}

// parseAt 按需扩展单个对象的解析窗口，不复制整个源文件
func (r *Reader) parseAt(offset int64, parse func(*objectParser) error) error {
	if offset < 0 || offset >= r.size {
		return fmt.Errorf("object offset outside file")
	}
	remaining := r.size - offset
	for length := min(remaining, int64(4096)); ; length = min(remaining, length+min(length, remaining-length)) {
		data, err := r.readRange(offset, length)
		if err != nil {
			return err
		}
		p := objectParser{data: data}
		err = parse(&p)
		if int64(p.pos) >= length-1 && length < remaining {
			continue
		}
		var syntax *SyntaxError
		if errors.As(err, &syntax) {
			return &SyntaxError{Offset: offset + syntax.Offset, Message: syntax.Message}
		}
		return err
	}
}

// Resolve 解析间接引用，直接对象原样返回
// 入参: object 待解析对象
// 返回: Object 解析后的对象, error 错误信息
func (r *Reader) Resolve(object Object) (Object, error) {
	seen := map[Reference]bool{}
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
	if value, ok := r.cache[ref]; ok {
		return value, nil
	}
	entry, ok := r.xref[ref.Number]
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
	}
	if err != nil {
		return nil, err
	}
	r.cache[ref] = value
	return value, nil
}

// indirect 从文件偏移读取完整间接对象
func (r *Reader) indirect(offset int64) (Reference, Object, error) {
	var p objectParser
	err := r.parseAt(offset, func(input *objectParser) error {
		p = *input
		defer func() { input.pos = p.pos }()
		for range 3 {
			p.token()
		}
		if _, err := p.object(); err != nil {
			return err
		}
		p.token()
		if p.pos < len(p.data) && p.data[p.pos] == '\r' {
			p.pos++
		}
		return nil
	})
	if err != nil {
		return Reference{}, nil, err
	}
	p.pos = 0
	number, e1 := strconv.ParseInt(p.token(), 10, 64)
	generation, e2 := strconv.ParseInt(p.token(), 10, 64)
	if e1 != nil || e2 != nil || number <= 0 || generation < 0 || generation > 65535 || p.token() != "obj" {
		return Reference{}, nil, p.fail("invalid indirect object header")
	}
	ref := Reference{number, generation}
	value, err := p.object()
	if err != nil {
		return ref, nil, err
	}
	keyword := p.token()
	if keyword == "stream" {
		dict, ok := value.(Dictionary)
		if !ok {
			return ref, nil, p.fail("stream requires dictionary")
		}
		if p.pos < len(p.data) && p.data[p.pos] == '\r' {
			p.pos++
		}
		if p.pos >= len(p.data) || p.data[p.pos] != '\n' {
			return ref, nil, p.fail("missing stream line ending")
		}
		p.pos++
		lengthObject, err := r.Resolve(dict["Length"])
		if err != nil {
			return ref, nil, err
		}
		length, ok := lengthObject.(Integer)
		start := offset + int64(p.pos)
		if !ok || length < 0 || int64(length) > r.size-start {
			return ref, nil, p.fail("invalid stream length")
		}
		data, err := r.readRange(start, int64(length))
		if err != nil {
			return ref, nil, err
		}
		value = &Stream{Dictionary: dict, Data: data}
		err = r.parseAt(start+int64(length), func(tail *objectParser) error {
			if tail.token() != "endstream" {
				return tail.fail("missing endstream")
			}
			if tail.token() != "endobj" {
				return tail.fail("missing endobj")
			}
			return nil
		})
		return ref, value, err
	}
	if keyword != "endobj" {
		return ref, nil, p.fail("missing endobj")
	}
	return ref, value, nil
}

// readXref 读取传统或流式交叉引用区段
func (r *Reader) readXref(offset int64) (Dictionary, map[int64]xrefEntry, error) {
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
func (r *Reader) readXrefStream(offset int64) (Dictionary, map[int64]xrefEntry, error) {
	_, object, err := r.indirect(offset)
	if err != nil {
		return nil, nil, err
	}
	stream, ok := object.(*Stream)
	if !ok || stream.Dictionary["Type"] != Name("XRef") {
		return nil, nil, fmt.Errorf("invalid cross-reference stream")
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
	data, err := stream.Decode()
	if err != nil {
		return nil, nil, err
	}
	entries := map[int64]xrefEntry{}
	pos := 0
	for i := 0; i < len(index); i += 2 {
		first, ok1 := index[i].(Integer)
		count, ok2 := index[i+1].(Integer)
		if !ok1 || !ok2 || first < 0 || count < 0 || count > size || first > size-count || int64(count) > int64((len(data)-pos)/stride) {
			return nil, nil, fmt.Errorf("invalid cross-reference range")
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
func (r *Reader) compressedObject(ref Reference, entry xrefEntry) (Object, error) {
	containerEntry, ok := r.xref[entry.position]
	if !ok || containerEntry.kind != 1 {
		return nil, fmt.Errorf("invalid object stream reference")
	}
	container := Reference{entry.position, containerEntry.generation}
	stream := r.objectStream
	if stream == nil || stream.reference != container {
		var err error
		stream, err = r.readObjectStream(container)
		if err != nil {
			return nil, err
		}
		r.objectStream = stream
	}
	index := entry.generation
	if index < 0 || index >= int64(len(stream.ids)) || stream.ids[index] != ref.Number {
		return nil, fmt.Errorf("object stream index mismatch")
	}
	p := objectParser{data: stream.data[stream.offsets[index]:stream.offsets[index+1]]}
	value, err := p.object()
	if err != nil {
		return nil, err
	}
	p.skipSpace()
	if p.pos != len(p.data) {
		return nil, fmt.Errorf("excess compressed object data")
	}
	return value, nil
}

// readObjectStream 解码对象流并校验全部索引，内容仍按引用逐项解析
// 入参: ref 对象流引用
// 返回: *objectStream 对象流数据, error 错误信息
func (r *Reader) readObjectStream(ref Reference) (*objectStream, error) {
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
	data, err := stream.Decode()
	if err != nil {
		return nil, err
	}
	if first > int64(len(data)) || count > first/4 {
		return nil, fmt.Errorf("invalid object stream dimensions")
	}
	p := objectParser{data: data[:first]}
	ids, offsets := make([]int64, count), make([]int64, count+1)
	for i := int64(0); i < count; i++ {
		ids[i], e1 = strconv.ParseInt(p.token(), 10, 64)
		offsets[i], e2 = strconv.ParseInt(p.token(), 10, 64)
		if e1 != nil || e2 != nil || ids[i] <= 0 || offsets[i] < 0 || offsets[i] > int64(len(data))-first || (i > 0 && offsets[i] <= offsets[i-1]) {
			return nil, fmt.Errorf("invalid object stream index")
		}
	}
	p.skipSpace()
	if p.pos != len(p.data) {
		return nil, fmt.Errorf("object stream index mismatch")
	}
	offsets[count] = int64(len(data)) - first
	return &objectStream{reference: ref, data: data[first:], ids: ids, offsets: offsets}, nil
}
