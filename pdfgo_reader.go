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
	"sync"
	"sync/atomic"
)

// objectStreamCacheLimit 限制多个对象流的解码缓存，单个超限流仅保留最近一次
const objectStreamCacheLimit = 64 << 20

// streamCacheBudget 限制Reader缓存的流对象总字节上限
const streamCacheBudget = 32 << 20

// readerRecoveryLimit 限制损坏文档的顺序重建范围和单个解析窗口
const readerRecoveryLimit = 64 << 20

// readerTailLimit 限制文件尾部定位窗口
const readerTailLimit = 1 << 20

// maxXrefEntries 限制交叉引用表与交叉引用流允许累计加载的最大条目数
const maxXrefEntries = 10_000_000

// maxXrefSections 限制Prev交叉引用链允许遍历的最大区段数
const maxXrefSections = 4096

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
	cache              *objectCache
	loading            map[Reference]bool
	destinations       map[string]Object
	legacyDestinations Dictionary
	fonts              map[Reference]*Font
	fontCacheBytes     int64
	halftones          map[Reference]*Halftone
	halftoneCacheBytes int64
	colorProfiles      map[[32]byte]*iccColorSpace
	deviceProfile      *Stream
	colorProfileDepth  int
	objectStreams      [8]*objectStream
	security           *standardSecurity
	warning            func(Diagnostic)
	textShaper         TextShaper
	decodeWorkers      int
	maxStreamBytes     int64
	maxFilterChain     int
	jbig2Globals       *jbig2GlobalsCache
	owner              *Reader
	ownerHasWarning    bool
	closed             bool
	recoveryIndex      map[int64]xrefEntry
	recoveryTried      bool
	rewriteMaxID       *rewriteMaxIDCache
}

// ReaderOptions 配置密码编码、结构诊断、文字整形、默认设备色ICC、解码并发及流解压预算
// Warning非空时诊断并恢复可确定的结构、字体及解码声明（不改源数据，IO/取消/认证错仍返回；绘制诊断由Visitor.Warning控制）
// ShapeText用于横排文本字段、选项及注释标题（nil不生成需整形的外观）；DeviceColorProfile仅作用于同通道未标定设备色，不覆盖显式或Default空间
// Workers限制图像解码并发（0自动，外层并发建议设为1）；MaxStreamBytes默认256MiB、MaxFilterChain默认16（负值不限）
type ReaderOptions struct {
	Password           []byte
	PasswordUTF8       bool
	Warning            func(Diagnostic)
	ShapeText          TextShaper
	DeviceColorProfile []byte
	Workers            int
	MaxStreamBytes     int64
	MaxFilterChain     int
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

// streamCacheKey 区分普通流与按内嵌文件过滤器解密的流缓存条目
type streamCacheKey struct {
	ref      Reference
	embedded bool
}

// objectCacheEntry 保存受字节预算控制的流缓存链表节点
type objectCacheEntry struct {
	key   streamCacheKey
	value *Stream
	size  int64
	prev  *objectCacheEntry
	next  *objectCacheEntry
}

// objectCache 管理PDF对象缓存，对流对象按字节预算执行LRU淘汰并保持并发安全
type objectCache struct {
	mu      sync.Mutex
	shared  atomic.Bool
	closed  atomic.Bool
	origin  *objectCache
	objects map[Reference]Object
	pinned  map[Reference]Object
	streams map[streamCacheKey]*objectCacheEntry
	head    *objectCacheEntry
	tail    *objectCacheEntry
	used    int64
	budget  int64
}

// newObjectCache 创建带流字节预算的对象缓存
// 入参: budget 流缓存字节上限
// 返回: *objectCache 对象缓存
func newObjectCache(budget int64) *objectCache {
	if budget <= 0 {
		budget = streamCacheBudget
	}
	cache := &objectCache{objects: make(map[Reference]Object), streams: make(map[streamCacheKey]*objectCacheEntry), budget: budget}
	cache.origin = cache
	return cache
}

// sameObjectCache 判断两个对象缓存是否同源，兼容共享视图与写回视图副本
// 入参: a 首个对象缓存, b 对比对象缓存
// 返回: bool 是否同源
func sameObjectCache(a, b *objectCache) bool {
	return a != nil && b != nil && a.origin == b.origin
}

// ownsObject 判断已归属阅读器的对象是否属于当前阅读器或同源视图，不访问对象原阅读器字段
// 入参: objReader 对象原阅读器, objCache 对象构造时缓存
// 返回: bool 是否属于当前阅读器
func (r *Reader) ownsObject(objReader *Reader, objCache *objectCache) bool {
	if objReader == nil {
		return true
	}
	if r == nil {
		return false
	}
	return objReader == r || sameObjectCache(objCache, r.cache)
}

// rootHasWarning 返回根阅读器是否配置诊断回调，子视图读取不可变副本避免并发竞态
// 返回: bool 根阅读器是否启用诊断
func (r *Reader) rootHasWarning() bool {
	if r == nil {
		return false
	}
	if r.owner != nil {
		return r.ownerHasWarning
	}
	return r.warning != nil
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
	return OpenWithOptions(path, ReaderOptions{Password: password})
}

// OpenWithUTF8Password 自动按安全处理器版本转换UTF-8密码并打开PDF文件
// 入参: path 文件路径, password 用户或所有者密码的UTF-8字节，nil表示空密码
// 返回: *Reader 阅读器, error 错误信息
func OpenWithUTF8Password(path string, password []byte) (*Reader, error) {
	return OpenWithOptions(path, ReaderOptions{Password: password, PasswordUTF8: true})
}

// OpenWithOptions 按读取选项打开PDF文件并读取交叉引用
// 入参: path 文件路径, options 读取及外观选项
// 返回: *Reader 阅读器, error 错误信息
func OpenWithOptions(path string, options ReaderOptions) (*Reader, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	info, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, err
	}
	r, err := NewReaderWithOptions(f, info.Size(), options)
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
	if source == nil {
		return nil, fmt.Errorf("nil source")
	}
	if size < 8 {
		return nil, fmt.Errorf("invalid file size")
	}
	var deviceProfile *Stream
	if options.DeviceColorProfile != nil {
		space, err := NewICCColorSpace(options.DeviceColorProfile)
		if err != nil {
			return nil, err
		}
		deviceProfile = &Stream{Dictionary: Dictionary{"N": Integer(space.Components())}, Data: bytes.Clone(options.DeviceColorProfile)}
	}
	data := make([]byte, min(size, 1024))
	if _, err := io.ReadFull(io.NewSectionReader(source, 0, size), data); err != nil {
		return nil, err
	}
	header := bytes.Index(data, []byte("%PDF-"))
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
	if !validPDFVersion(version) {
		if options.Warning == nil {
			return nil, &UnsupportedError{Feature: "PDF version " + version}
		}
		options.Warning(Diagnostic{Message: "invalid PDF version " + version + "; using 1.7"})
		version = "1.7"
	}
	r := &Reader{Version: version, source: io.NewSectionReader(source, int64(header), size-int64(header)), size: size - int64(header), xref: map[int64]xrefEntry{}, cache: newObjectCache(streamCacheBudget), loading: map[Reference]bool{}, deviceProfile: deviceProfile, jbig2Globals: newJBIG2GlobalsCache()}
	r.originalSource, r.originalSize = source, size
	r.warning = options.Warning
	r.textShaper = options.ShapeText
	if options.Workers > 0 {
		r.decodeWorkers = options.Workers
	}
	r.maxStreamBytes = normalizeMaxStreamBytes(options.MaxStreamBytes)
	r.maxFilterChain = normalizeMaxFilterChain(options.MaxFilterChain)
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
	if err := r.openSecurity(options.Password, options.PasswordUTF8); err != nil {
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
	if versionObj, err := r.Resolve(catalog["Version"]); err != nil {
		r.Close()
		return nil, err
	} else if versionObj != nil {
		v, ok := versionObj.(Name)
		if !ok || !validPDFVersion(string(v)) {
			if r.warning == nil {
				r.Close()
				return nil, fmt.Errorf("invalid catalog PDF version")
			}
			r.warning(Diagnostic{Message: "invalid catalog Version ignored"})
		} else if string(v) > r.Version {
			r.Version = string(v)
		}
	}
	return r, nil
}

// validPDFVersion 判断版本字符串是否为ISO 32000允许的PDF版本格式
// 入参: version 版本号文本
// 返回: bool 是否为合法版本
func validPDFVersion(version string) bool {
	return version == "2.0" || (len(version) == 3 && version[0] == '1' && version[1] == '.' && version[2] >= '0' && version[2] <= '7')
}

// readerStructuralError 排除源读取、取消、关闭和安全错误，限定结构恢复范围
// 入参: err 原始错误
// 返回: bool 是否可以尝试结构恢复
func readerStructuralError(err error) bool {
	var read *ReadError
	var unsupported *UnsupportedError
	var security *objectSecurityError
	return err != nil && !errors.As(err, &read) && !errors.As(err, &unsupported) && !errors.As(err, &security) && !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) && !errors.Is(err, os.ErrClosed) && !errors.Is(err, ErrPassword) && !errors.Is(err, ErrStreamLimitExceeded)
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

// isClosed 检查对象缓存或其原始缓存是否已随根阅读器关闭
// 返回: bool 是否已关闭
func (c *objectCache) isClosed() bool {
	if c == nil {
		return false
	}
	if c.closed.Load() {
		return true
	}
	return c.origin != nil && c.origin != c && c.origin.closed.Load()
}

// get 按流解密模式查询缓存对象，隔离普通流与内嵌文件流缓存
// 入参: ref 对象引用, embedded 是否按内嵌文件流模式查询
// 返回: Object 缓存对象, bool 是否命中
func (c *objectCache) get(ref Reference, embedded bool) (Object, bool) {
	if c == nil {
		return nil, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.isClosed() {
		return nil, false
	}
	if c.pinned != nil {
		if value, ok := c.pinned[ref]; ok {
			return value, true
		}
	}
	if value, ok := c.objects[ref]; ok {
		return value, true
	}
	if entry, ok := c.streams[streamCacheKey{ref: ref, embedded: embedded}]; ok {
		c.unlinkLocked(entry)
		c.pushFrontLocked(entry)
		return entry.value, true
	}
	return nil, false
}

// set 按流解密模式写入对象缓存，隔离普通流与内嵌文件流缓存
// 入参: ref 对象引用, value 对象值, embedded 是否为内嵌文件流解密结果
func (c *objectCache) set(ref Reference, value Object, embedded bool) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.isClosed() {
		return
	}
	if c.pinned != nil {
		if _, ok := c.pinned[ref]; ok {
			c.pinned[ref] = value
			return
		}
	}
	stream, isStream := value.(*Stream)
	if !isStream {
		for _, mode := range [2]bool{false, true} {
			key := streamCacheKey{ref: ref, embedded: mode}
			if entry, ok := c.streams[key]; ok {
				c.unlinkLocked(entry)
				delete(c.streams, key)
				c.used -= entry.size
			}
		}
		c.objects[ref] = value
		return
	}
	delete(c.objects, ref)
	key := streamCacheKey{ref: ref, embedded: embedded}
	size := int64(len(stream.Data)) + 256
	if entry, ok := c.streams[key]; ok {
		c.unlinkLocked(entry)
		delete(c.streams, key)
		c.used -= entry.size
	}
	if size > c.budget {
		return
	}
	entry := &objectCacheEntry{key: key, value: stream, size: size}
	c.streams[key] = entry
	c.pushFrontLocked(entry)
	c.used += size
	c.evictLocked()
}

// has 检查引用是否已在缓存中，不改变淘汰顺序
// 入参: ref 对象引用
// 返回: bool 是否已缓存
func (c *objectCache) has(ref Reference) bool {
	if c == nil {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.isClosed() {
		return false
	}
	if c.pinned != nil {
		if _, ok := c.pinned[ref]; ok {
			return true
		}
	}
	if _, ok := c.objects[ref]; ok {
		return true
	}
	if _, ok := c.streams[streamCacheKey{ref: ref, embedded: false}]; ok {
		return true
	}
	_, ok := c.streams[streamCacheKey{ref: ref, embedded: true}]
	return ok
}

// delete 移除非固定缓存条目
// 入参: ref 对象引用
func (c *objectCache) delete(ref Reference) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.objects, ref)
	for _, mode := range [2]bool{false, true} {
		key := streamCacheKey{ref: ref, embedded: mode}
		if entry, ok := c.streams[key]; ok {
			c.unlinkLocked(entry)
			delete(c.streams, key)
			c.used -= entry.size
		}
	}
}

// snapshot 复制当前全部缓存引用集合
// 返回: map[Reference]bool 已缓存引用集合
func (c *objectCache) snapshot() map[Reference]bool {
	if c == nil {
		return map[Reference]bool{}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	result := make(map[Reference]bool, len(c.objects)+len(c.streams)+len(c.pinned))
	for ref := range c.pinned {
		result[ref] = true
	}
	for ref := range c.objects {
		result[ref] = true
	}
	for key := range c.streams {
		result[key.ref] = true
	}
	return result
}

// pruneObjectsIfOver 当非流对象缓存超过上限时清理不在保留集合中的条目
// 入参: keep 需要保留的引用集合, limit 非流对象条目上限
func (c *objectCache) pruneObjectsIfOver(keep map[Reference]bool, limit int) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.objects) <= limit {
		return
	}
	for ref := range c.objects {
		if !keep[ref] {
			delete(c.objects, ref)
		}
	}
}

// prune 删除不在保留集合中的非固定缓存条目
// 入参: keep 需要保留的引用集合
func (c *objectCache) prune(keep map[Reference]bool) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	for ref := range c.objects {
		if !keep[ref] {
			delete(c.objects, ref)
		}
	}
	for key, entry := range c.streams {
		if !keep[key.ref] {
			c.unlinkLocked(entry)
			delete(c.streams, key)
			c.used -= entry.size
		}
	}
}

// cloneWithPinned 复制缓存并附加不参与淘汰的固定替换对象
// 入参: pinned 固定替换对象映射
// 返回: *objectCache 独立缓存副本
func (c *objectCache) cloneWithPinned(pinned map[Reference]Object) *objectCache {
	budget := int64(streamCacheBudget)
	if c != nil && c.budget > 0 {
		budget = c.budget
	}
	clone := newObjectCache(budget)
	if len(pinned) > 0 {
		clone.pinned = make(map[Reference]Object, len(pinned))
		for ref, value := range pinned {
			clone.pinned[ref] = value
		}
	}
	if c == nil {
		return clone
	}
	clone.origin = c.origin
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.pinned) > 0 {
		if clone.pinned == nil {
			clone.pinned = make(map[Reference]Object, len(c.pinned))
		}
		for ref, value := range c.pinned {
			if _, overridden := clone.pinned[ref]; !overridden {
				clone.pinned[ref] = value
			}
		}
	}
	for ref, value := range c.objects {
		if _, overridden := clone.pinned[ref]; !overridden {
			clone.objects[ref] = value
		}
	}
	for entry := c.tail; entry != nil; entry = entry.prev {
		if _, overridden := clone.pinned[entry.key.ref]; overridden {
			continue
		}
		item := &objectCacheEntry{key: entry.key, value: entry.value, size: entry.size}
		clone.streams[item.key] = item
		clone.pushFrontLocked(item)
		clone.used += item.size
	}
	return clone
}

// unlinkLocked 将节点从流缓存双向链表摘除，调用方须已持有锁
// 入参: entry 待摘除节点
func (c *objectCache) unlinkLocked(entry *objectCacheEntry) {
	if entry.prev != nil {
		entry.prev.next = entry.next
	} else if c.head == entry {
		c.head = entry.next
	}
	if entry.next != nil {
		entry.next.prev = entry.prev
	} else if c.tail == entry {
		c.tail = entry.prev
	}
	entry.prev = nil
	entry.next = nil
}

// pushFrontLocked 将节点插入流缓存双向链表头部，调用方须已持有锁
// 入参: entry 待插入节点
func (c *objectCache) pushFrontLocked(entry *objectCacheEntry) {
	entry.prev = nil
	entry.next = c.head
	if c.head != nil {
		c.head.prev = entry
	} else {
		c.tail = entry
	}
	c.head = entry
}

// evictLocked 淘汰最久未使用的流缓存直至总字节不超过预算，调用方须已持有锁
func (c *objectCache) evictLocked() {
	for c.used > c.budget && c.tail != nil {
		victim := c.tail
		c.unlinkLocked(victim)
		delete(c.streams, victim.key)
		c.used -= victim.size
	}
}

// xrefEntry 并发安全读取单个交叉引用条目
// 入参: number 对象编号
// 返回: xrefEntry 交叉引用条目, bool 是否存在
func (r *Reader) xrefEntry(number int64) (xrefEntry, bool) {
	if r.cache != nil {
		r.cache.mu.Lock()
		entry, ok := r.xref[number]
		r.cache.mu.Unlock()
		return entry, ok
	}
	entry, ok := r.xref[number]
	return entry, ok
}

// setXrefEntry 并发安全更新单个交叉引用条目，避免与并发Fork克隆冲突
// 入参: number 对象编号, entry 交叉引用条目
func (r *Reader) setXrefEntry(number int64, entry xrefEntry) {
	if r.cache != nil {
		r.cache.mu.Lock()
		r.xref[number] = entry
		r.cache.mu.Unlock()
		return
	}
	r.xref[number] = entry
}

// Close 释放源数据、缓存与解密密钥并关闭Open持有的文件（不关闭NewReader外部源，可重复调用），关闭后读取方法及关联流均返回os.ErrClosed，共享底层os.File的Fork视图随之失效
// 返回: error 关闭错误
func (r *Reader) Close() error {
	if r == nil {
		return nil
	}
	cache := r.cache
	if cache != nil {
		cache.mu.Lock()
	}
	if r.closed {
		if cache != nil {
			cache.mu.Unlock()
		}
		return nil
	}
	closer := r.closer
	if r.owner == nil && cache != nil {
		cache.closed.Store(true)
		clear(cache.objects)
		clear(cache.pinned)
		clear(cache.streams)
		cache.head = nil
		cache.tail = nil
		cache.used = 0
	}
	if r.security != nil {
		clear(r.security.key)
	}
	r.Version = ""
	r.Trailer = nil
	r.source = nil
	r.size = 0
	r.originalSource = nil
	r.originalSize = 0
	r.closer = nil
	r.xref = nil
	r.loading = nil
	r.destinations = nil
	r.legacyDestinations = nil
	r.fonts = nil
	r.fontCacheBytes = 0
	r.halftones = nil
	r.halftoneCacheBytes = 0
	r.colorProfiles = nil
	r.deviceProfile = nil
	r.colorProfileDepth = 0
	r.objectStreams = [8]*objectStream{}
	r.security = nil
	r.warning = nil
	r.textShaper = nil
	r.decodeWorkers = 0
	r.maxStreamBytes = 0
	r.maxFilterChain = 0
	r.jbig2Globals = nil
	r.owner = nil
	r.ownerHasWarning = false
	r.closed = true
	r.recoveryIndex = nil
	r.recoveryTried = false
	r.rewriteMaxID = nil
	if cache != nil {
		cache.mu.Unlock()
	}
	if closer != nil {
		return closer.Close()
	}
	return nil
}

// Resolve 解析间接引用，直接对象原样返回
// 入参: object 待解析对象
// 返回: Object 解析后的对象, error 错误信息
func (r *Reader) Resolve(object Object) (Object, error) {
	return r.resolve(object, false)
}

// resolveEmbeddedFile 解析文件说明中的内嵌文件流引用，在EFF与StmF不同时按内嵌文件过滤器解密
// 入参: object 内嵌文件对象或间接引用
// 返回: Object 解析后的对象, error 错误信息
func (r *Reader) resolveEmbeddedFile(object Object) (Object, error) {
	return r.resolve(object, true)
}

// resolve 解析间接引用链并检测循环引用，支持按内嵌文件流过滤器解密
// 入参: object 待解析对象, embedded 是否按内嵌文件流过滤器解密
// 返回: Object 解析后的对象, error 错误信息
func (r *Reader) resolve(object Object, embedded bool) (Object, error) {
	if r == nil {
		return nil, fmt.Errorf("invalid reader")
	}
	if r.closed {
		return nil, os.ErrClosed
	}
	ref, ok := object.(Reference)
	if !ok {
		return object, nil
	}
	object, err := r.object(ref, embedded)
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
		object, err = r.object(ref, embedded)
		if err != nil {
			return nil, err
		}
	}
}

// resolveOptional 在绑定阅读器时解析对象，未绑定阅读器时原样返回直接对象
// 入参: object 待解析对象
// 返回: Object 解析后的对象, error 阅读器状态或引用错误
func (r *Reader) resolveOptional(object Object) (Object, error) {
	if r != nil {
		return r.Resolve(object)
	}
	if _, ok := object.(Reference); ok {
		return nil, fmt.Errorf("invalid reader")
	}
	return object, nil
}

// Object 按编号与代数读取间接对象
// 不存在或已释放的引用按PDF规则返回空对象
// 入参: ref 间接引用
// 返回: Object 对象, error 错误信息
func (r *Reader) Object(ref Reference) (Object, error) {
	return r.object(ref, false)
}

// object 按编号与代数读取间接对象，支持为内嵌文件流指定EFF解密及独立流缓存
// 入参: ref 间接引用, embedded 是否按内嵌文件流过滤器解密
// 返回: Object 对象, error 错误信息
func (r *Reader) object(ref Reference, embedded bool) (Object, error) {
	if r == nil {
		return nil, fmt.Errorf("invalid reader")
	}
	if r.closed {
		return nil, os.ErrClosed
	}
	useEmbedded := embedded && r.security != nil && r.security.embeddedFilter != r.security.streamFilter
	if value, ok := r.cache.get(ref, useEmbedded); ok {
		return value, nil
	}
	entry, ok := r.xrefEntry(ref.Number)
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
			r.setXrefEntry(ref.Number, entry)
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
					r.setXrefEntry(ref.Number, xrefEntry{kind: 1, position: position, generation: ref.Generation})
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
					r.setXrefEntry(ref.Number, recovered)
					r.warning(Diagnostic{Offset: recovered.position, Message: fmt.Sprintf("recovered cross-reference offset for object %d %d", ref.Number, ref.Generation)})
				}
			}
		}
		if err == nil && r.security != nil && !r.security.unencrypted[ref] {
			value, err = r.security.decryptObject(r, value, ref, useEmbedded)
			if err != nil {
				err = &objectSecurityError{err: err}
			}
		}
	}
	if err != nil {
		return nil, err
	}
	if stream, ok := value.(*Stream); ok {
		stream.maxStreamBytes = r.maxStreamBytes
		stream.maxFilterChain = r.maxFilterChain
		stream.cacheFilterRefs(r)
		if r.owner != nil {
			stream.reader = r.owner
			stream.hasWarning = r.ownerHasWarning
		}
	}
	r.cache.set(ref, value, useEmbedded)
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
	if r.cache != nil {
		r.cache.mu.Lock()
	}
	for number, entry := range r.xref {
		if number != target.Number && entry.kind == 1 && entry.position >= 0 && entry.position < offset {
			previous = max(previous, entry.position)
		}
	}
	if r.cache != nil {
		r.cache.mu.Unlock()
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

// Is 使对象安全错误匹配导出哨兵错误ErrDecrypt
// 入参: target 目标错误
// 返回: bool 是否匹配ErrDecrypt
func (e *objectSecurityError) Is(target error) bool {
	return target == ErrDecrypt
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
	sections := 0
	var totalEntries int64
	var rebaseBase int64
	for {
		sections++
		if sections > maxXrefSections {
			return fmt.Errorf("%w: cross-reference section limit exceeded", ErrStreamLimitExceeded)
		}
		if visited[offset] {
			if r.warning != nil && r.Trailer != nil {
				r.warning(Diagnostic{Offset: offset, Message: "cyclic previous cross-reference; latest entries retained"})
				break
			}
			return fmt.Errorf("cyclic cross-reference chain")
		}
		visited[offset] = true
		trailer, entries, actual, rebased, err := r.readXref(offset, maxXrefEntries-totalEntries, rebaseBase == 0)
		if err != nil {
			return err
		}
		if rebased > 0 {
			rebaseBase = rebased
		} else if rebaseBase > 0 {
			if err := r.applyXrefBase(rebaseBase, trailer, entries); err != nil {
				return err
			}
		}
		totalEntries += int64(len(entries))
		if actual != offset {
			if visited[actual] {
				if r.warning != nil && r.Trailer != nil {
					r.warning(Diagnostic{Offset: actual, Message: "cyclic previous cross-reference; latest entries retained"})
					break
				}
				return fmt.Errorf("cyclic cross-reference chain")
			}
			visited[actual] = true
		}
		if hybridObj := trailer["XRefStm"]; hybridObj != nil {
			hybrid, ok := hybridObj.(Integer)
			if !ok || hybrid < 0 {
				return fmt.Errorf("invalid hybrid cross-reference offset")
			}
			if visited[int64(hybrid)] {
				return fmt.Errorf("cyclic hybrid cross-reference")
			}
			visited[int64(hybrid)] = true
			_, supplement, err := r.readXrefStream(int64(hybrid), maxXrefEntries-totalEntries)
			if err != nil {
				return err
			}
			if rebaseBase > 0 {
				if err := r.applyXrefBase(rebaseBase, nil, supplement); err != nil {
					return err
				}
			}
			totalEntries += int64(len(supplement))
			for id, entry := range supplement {
				if existing, exists := entries[id]; !exists || existing.kind == 0 && entry.kind != 0 {
					entries[id] = entry
				}
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
	var offset, xrefEntries int64
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
			xrefTrailer, entries, xrefErr := r.readXrefAt(start, maxXrefEntries-xrefEntries)
			if xrefErr != nil && !readerStructuralError(xrefErr) {
				return nil, nil, xrefErr
			}
			if xrefErr == nil {
				xrefEntries += int64(len(entries))
				if xrefTrailer["XRefStm"] == nil {
					for id, entry := range entries {
						if entry.kind == 0 {
							index[id] = entry
						}
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
					_, entries, err := r.readXrefStream(start, maxXrefEntries-xrefEntries)
					if err != nil {
						if !readerStructuralError(err) {
							return nil, nil, err
						}
						damagedStreamIndex = true
						r.warning(Diagnostic{Offset: start, Message: "invalid xref stream index; validated uncompressed object boundaries retained"})
					} else {
						xrefEntries += int64(len(entries))
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
	if r.cache != nil {
		r.cache.mu.Lock()
	}
	for id, entry := range r.xref {
		if entry.kind == 0 {
			index[id] = entry
		}
	}
	if r.cache != nil {
		r.cache.mu.Unlock()
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
	local.cache, local.loading = newObjectCache(streamCacheBudget), map[Reference]bool{}
	local.recoveryIndex, local.recoveryTried = index, true
	clear(local.objectStreams[:])
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
			return fmt.Errorf("object stream recovery %w", ErrStreamLimitExceeded)
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
		local.cache.delete(ref)
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
			return ref, nil, &SyntaxError{Offset: offset + int64(p.pos), Message: "stream requires dictionary"}
		}
		if !streamStart {
			return ref, nil, &SyntaxError{Offset: offset + int64(p.pos), Message: "missing stream line ending"}
		}
		lengthObject, err := r.Resolve(dict["Length"])
		if err != nil {
			return ref, nil, err
		}
		length, ok := lengthObject.(Integer)
		start := offset + int64(p.pos)
		if (!ok || length < 0 || int64(length) > r.size-start) && r.warning == nil {
			return ref, nil, &SyntaxError{Offset: offset + int64(p.pos), Message: "invalid stream length"}
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
		value = &Stream{Dictionary: dict, Data: data, Reference: ref, reader: r, cache: r.cache, maxStreamBytes: r.maxStreamBytes, maxFilterChain: r.maxFilterChain, hasWarning: r.warning != nil}
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
// 入参: start 流数据起点, length 声明长度，负值表示缺失, ref 对象引用
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
	var expected Reference
	nextPosition := int64(1<<63 - 1)
	if r.cache != nil {
		r.cache.mu.Lock()
	}
	if entry, ok := r.xref[ref.Number]; ok && entry.kind == 1 && entry.generation == ref.Generation {
		base = entry.position
	}
	for number, entry := range r.xref {
		if entry.kind == 1 && entry.position > base && entry.position < nextPosition {
			nextPosition = entry.position
			expected = Reference{number, entry.generation}
		}
	}
	if r.cache != nil {
		r.cache.mu.Unlock()
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
		if data[len(data)-1] == '\n' {
			actual--
			if len(data) > 1 && data[len(data)-2] == '\r' {
				actual--
			}
		} else if data[len(data)-1] == '\r' {
			actual--
		}
	}
	r.warning(Diagnostic{Offset: start, Message: fmt.Sprintf("stream Length %d replaced by validated boundary length %d", length, actual)})
	return actual, candidateEnd, nil
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

// readXref 读取传统或流式交叉引用区段，并返回实际定位的区段偏移与嵌入基址
// 入参: offset 交叉引用区段的字节偏移, limit 剩余允许加载的最大条目数, allowRebase 是否允许探测嵌入PDF基址
// 返回: Dictionary 尾字典或交叉引用流字典, map[int64]xrefEntry 交叉引用条目, int64 实际区段偏移, int64 嵌入PDF基址, error 读取或解析错误
func (r *Reader) readXref(offset int64, limit int64, allowRebase bool) (Dictionary, map[int64]xrefEntry, int64, int64, error) {
	trailer, entries, original := r.readXrefAt(offset, limit)
	if original == nil || r.warning == nil || !readerStructuralError(original) {
		return trailer, entries, offset, 0, original
	}
	start, stop := max(int64(0), offset-1024), min(r.size, offset+1024)
	if offset < 0 || offset >= r.size {
		start, stop = max(int64(0), r.size-readerTailLimit), r.size
	}
	for _, region := range []struct {
		start, stop int64
		claim       bool
	}{{start, stop, false}, {max(int64(0), r.size-readerTailLimit), r.size, true}} {
		trailer, entries, candidate, err := r.findXrefOffset(offset, region.start, region.stop, region.claim, limit)
		if err != nil {
			return nil, nil, -1, 0, err
		}
		if candidate >= 0 {
			var base int64
			if allowRebase {
				base, err = r.rebaseXrefEntries(offset, candidate, trailer, entries)
				if err != nil {
					if !readerStructuralError(err) {
						return nil, nil, -1, 0, err
					}
					continue
				}
			}
			r.warning(Diagnostic{Offset: candidate, Message: fmt.Sprintf("cross-reference offset %d recovered at %d", offset, candidate)})
			return trailer, entries, candidate, base, nil
		}
	}
	return nil, nil, -1, 0, original
}

// applyXrefBase 校验并平移相对嵌入PDF文件头的xref条目及链式偏移
// 入参: base 嵌入PDF文件头偏移, trailer 尾字典，可为空, entries 在用索引
// 返回: error 源错误或不能一致验证的偏移
func (r *Reader) applyXrefBase(base int64, trailer Dictionary, entries map[int64]xrefEntry) error {
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
	for _, key := range []Name{"Prev", "XRefStm"} {
		if value, ok := trailer[key].(Integer); ok && value > 0 {
			if int64(value) >= r.size-base {
				return fmt.Errorf("relative cross-reference chain outside file")
			}
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
			trailer[key] = value + Integer(base)
		}
	}
	return nil
}

// rebaseXrefEntries 验证嵌入PDF文件头及全部在用对象头后修正相对该文件头的xref偏移
// 入参: declared 声明xref偏移, actual 实际xref偏移, trailer 尾字典, entries 在用索引
// 返回: int64 已应用的嵌入基址, error 源错误或不能一致验证的偏移
func (r *Reader) rebaseXrefEntries(declared, actual int64, trailer Dictionary, entries map[int64]xrefEntry) (int64, error) {
	if declared < 0 || actual <= declared || actual-declared > r.size-8 {
		return 0, nil
	}
	base := actual - declared
	header, err := r.readRange(base, 8)
	if err != nil {
		return 0, err
	}
	if !bytes.HasPrefix(header, []byte("%PDF-")) {
		return 0, nil
	}
	if err := r.applyXrefBase(base, trailer, entries); err != nil {
		return 0, err
	}
	r.warning(Diagnostic{Offset: base, Message: "xref offsets relative to embedded PDF header validated and rebased"})
	return base, nil
}

// findXrefOffset 在限定范围校验唯一xref候选，远距离修复须与该区段的startxref声明相互印证
// 入参: offset 错误声明偏移, start 起点, stop 终点, claim 是否要求尾部声明相符, limit 剩余允许加载的最大条目数
// 返回: Dictionary 尾字典, map[int64]xrefEntry 索引, int64 唯一位置或负值, error 源错误
func (r *Reader) findXrefOffset(offset, start, stop int64, claim bool, limit int64) (Dictionary, map[int64]xrefEntry, int64, error) {
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
			matches, err := r.xrefClaimsOffset(at, offset, limit)
			if err != nil {
				return nil, nil, -1, err
			}
			if !matches {
				continue
			}
		}
		dict, found, err := r.readXrefAt(at, limit)
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
// 入参: at 候选实际位置, offset 待恢复声明位置, limit 剩余允许加载的最大条目数
// 返回: bool 声明是否相符, error 源读取错误
func (r *Reader) xrefClaimsOffset(at, offset, limit int64) (bool, error) {
	var stream bool
	var start, end int64
	var ref Reference
	var length Object
	err := r.parseAt(at, func(p *objectParser) error {
		if p.token() == "xref" {
			p.pos = 0
			if _, _, err := r.classicXref(p, limit); err != nil {
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
// 入参: offset 交叉引用区段的字节偏移, limit 剩余允许加载的最大条目数
// 返回: Dictionary 尾字典, map[int64]xrefEntry 交叉引用条目, error 读取或解析错误
func (r *Reader) readXrefAt(offset int64, limit int64) (Dictionary, map[int64]xrefEntry, error) {
	if offset < 0 || offset >= r.size {
		return nil, nil, fmt.Errorf("cross-reference offset outside file")
	}
	data, err := r.readRange(offset, min(16, r.size-offset))
	probe := objectParser{data: data}
	if err != nil {
		return nil, nil, err
	}
	if probe.token() != "xref" {
		return r.readXrefStream(offset, limit)
	}
	var trailer Dictionary
	var result map[int64]xrefEntry
	err = r.parseAt(offset, func(p *objectParser) error {
		var err error
		trailer, result, err = r.classicXref(p, limit)
		return err
	})
	return trailer, result, err
}

// classicXref 解析传统交叉引用及其尾字典
// 入参: p 位于xref关键字的解析器, limit 剩余允许加载的最大条目数
// 返回: Dictionary 尾字典, map[int64]xrefEntry 交叉引用条目, error 语法错误
func (r *Reader) classicXref(p *objectParser, limit int64) (Dictionary, map[int64]xrefEntry, error) {
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
		if limit < 0 || count > limit-int64(len(entries)) {
			return nil, nil, fmt.Errorf("%w: cross-reference entry limit exceeded", ErrStreamLimitExceeded)
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
// 入参: offset 交叉引用流对象的字节偏移, limit 剩余允许加载的最大条目数
// 返回: Dictionary 交叉引用流字典, map[int64]xrefEntry 交叉引用条目, error 读取或解析错误
func (r *Reader) readXrefStream(offset int64, limit int64) (Dictionary, map[int64]xrefEntry, error) {
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
	var total int64
	for i := 0; i < len(index); i += 2 {
		first, ok1 := index[i].(Integer)
		count, ok2 := index[i+1].(Integer)
		if !ok1 || !ok2 || first < 0 || count < 0 || first > Integer(1<<63-1)-count {
			return nil, nil, fmt.Errorf("invalid cross-reference range")
		}
		if limit < 0 || int64(count) > limit-total {
			return nil, nil, fmt.Errorf("%w: cross-reference entry limit exceeded", ErrStreamLimitExceeded)
		}
		total += int64(count)
	}
	data, err := stream.decodeWith(context.Background(), nil, r.warning, 0)
	if err != nil {
		return nil, nil, err
	}
	entries := map[int64]xrefEntry{}
	pos := 0
	for i := 0; i < len(index); i += 2 {
		first := index[i].(Integer)
		count := index[i+1].(Integer)
		if int64(count) > int64((len(data)-pos)/stride) {
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
	containerEntry, ok := r.xrefEntry(entry.position)
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
		kept := 1
		for _, cached := range r.objectStreams[1:] {
			if cached == nil {
				break
			}
			if used+cached.bytes <= objectStreamCacheLimit {
				r.objectStreams[kept] = cached
				kept++
				used += cached.bytes
			}
		}
		clear(r.objectStreams[kept:])
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
	nObj, err := r.Resolve(stream.Dictionary["N"])
	if err != nil {
		return nil, err
	}
	firstObj, err := r.Resolve(stream.Dictionary["First"])
	if err != nil {
		return nil, err
	}
	nVal, ok1 := nObj.(Integer)
	firstVal, ok2 := firstObj.(Integer)
	count, first := int64(nVal), int64(firstVal)
	if !ok1 || !ok2 || count <= 0 || first < 0 {
		return nil, fmt.Errorf("invalid object stream header")
	}
	data, err := stream.decodeWith(context.Background(), r, r.warning, limit)
	if err != nil {
		return nil, err
	}
	if first > int64(len(data)) || count > (first+1)/4 {
		return nil, fmt.Errorf("invalid object stream dimensions")
	}
	if limit > 0 {
		if int64(cap(data))+8 > limit || count > (limit-int64(cap(data))-8)/80 {
			return nil, fmt.Errorf("object stream recovery index %w", ErrStreamLimitExceeded)
		}
	} else if maxBytes, _ := effectiveStreamLimits(r, stream); maxBytes > 0 && count > max(maxBytes, int64(objectStreamCacheLimit))/64 {
		return nil, fmt.Errorf("%w: object stream index limit exceeded", ErrStreamLimitExceeded)
	}
	p := objectParser{data: data[:first]}
	initial := int(min(count, int64(4096)))
	ids := make([]int64, 0, initial)
	offsets := make([]int64, 0, initial+1)
	indices := make(map[int64]int64, initial)
	for i := int64(0); i < count; i++ {
		id, e1 := strconv.ParseInt(p.token(), 10, 64)
		off, e2 := strconv.ParseInt(p.token(), 10, 64)
		if e1 != nil || e2 != nil || id <= 0 || off < 0 || off > int64(len(data))-first || (i > 0 && off <= offsets[i-1]) {
			return nil, fmt.Errorf("invalid object stream index")
		}
		if _, exists := indices[id]; exists {
			return nil, fmt.Errorf("duplicate object stream number")
		}
		ids = append(ids, id)
		offsets = append(offsets, off)
		indices[id] = i
	}
	p.skipSpace()
	if p.pos != len(p.data) {
		return nil, fmt.Errorf("object stream index mismatch")
	}
	offsets = append(offsets, int64(len(data))-first)
	return &objectStream{reference: ref, data: data[first:], ids: ids, indices: indices, offsets: offsets, bytes: int64(cap(data)) + 8*(count*2+1) + 64*count}, nil
}
