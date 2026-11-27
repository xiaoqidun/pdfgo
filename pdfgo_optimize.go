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
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"image"
	"io"
	"maps"
	"math"
	"os"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"unicode/utf16"
)

// OptimizeOptions 配置原生PDF重写，不经过页面渲染
// 原始模式或包含签名时保留源文件；加密文档保留原安全处理器、密码和权限
// Creator非空时设置重写结果的制作软件；OnProgress回报scan、compress、write阶段，支持取消
// Workers限制并行优化的工作协程数，零值自动按处理器核数选择，WASM环境恒为单线程
type OptimizeOptions struct {
	Compression CompressionOptions
	Creator     string
	Workers     int
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
	ctx           context.Context
	writer        io.Writer
	reader        *Reader
	security      *standardSecurity
	unencrypted   map[Reference]bool
	embeddedFiles map[Reference]bool
	aliases       map[Reference]Reference
	live          map[Reference]bool
	count         int64
}

// smaskOptInfo 缓存共享软遮罩的不透明与预混合检测结果
type smaskOptInfo struct {
	opaque   bool
	hasMatte bool
}

// streamOptJob 保存有界并发流优化任务的输入与输出状态
type streamOptJob struct {
	ref              Reference
	embedded         bool
	lazy             *lazyReplacementImage
	stream           *Stream
	ownedMaskRef     Reference
	ownedMaskObj     Object
	ownedHardMaskRef Reference
	compression      CompressionOptions
	mode             CompressionMode
	exactData        bool
	size             image.Point
	colorResources   Dictionary
	hasSMaskInfo     bool
	smaskInfo        smaskOptInfo
	done             chan struct{}
	outValue         Object
	outLazyMask      *Stream
	outOwnedMask     Object
	outOwnedHardMask Object
	droppedSMask     bool
	err              error
}

// optimizeWorkers 按配置与运行平台计算并行工作协程数，WASM环境恒返回1
// 入参: workers 用户指定的并发数，非正数时按处理器核数自动选择
// 返回: int 有效工作协程数
func optimizeWorkers(workers int) int {
	if runtime.GOOS == "js" || runtime.GOARCH == "wasm" {
		return 1
	}
	if workers > 0 {
		return workers
	}
	return max(1, runtime.GOMAXPROCS(0))
}

// syncWarning 包装可恢复诊断回调以支持多协程并发上报
// 入参: warning 原始警告回调
// 返回: func(Diagnostic) 并发安全的警告回调
func syncWarning(warning func(Diagnostic)) func(Diagnostic) {
	if warning == nil {
		return nil
	}
	var mu sync.Mutex
	return func(d Diagnostic) {
		mu.Lock()
		defer mu.Unlock()
		warning(d)
	}
}

// minUintBytes 计算无符号整数大端表示所需的最小字节数
// 入参: v 非负整数
// 返回: int 所需字节数
func minUintBytes(v uint64) int {
	switch {
	case v == 0:
		return 0
	case v <= 0xff:
		return 1
	case v <= 0xffff:
		return 2
	case v <= 0xffffff:
		return 3
	case v <= 0xffffffff:
		return 4
	case v <= 0xffffffffff:
		return 5
	case v <= 0xffffffffffff:
		return 6
	case v <= 0xffffffffffffff:
		return 7
	default:
		return 8
	}
}

// putBigEndianBytes 将无符号整数按大端序写入定长目标切片
// 入参: dst 目标切片, v 无符号整数
func putBigEndianBytes(dst []byte, v uint64) {
	for i := len(dst) - 1; i >= 0; i-- {
		dst[i] = byte(v)
		v >>= 8
	}
}

// streamHasFilter 判断流字典终端过滤器是否为指定名称
// 入参: dict 流字典, target 目标过滤器名称
// 返回: bool 是否匹配
func streamHasFilter(dict Dictionary, target Name) bool {
	switch f := dict["Filter"].(type) {
	case Name:
		return f == target
	case Array:
		return len(f) > 0 && f[len(f)-1] == target
	default:
		return false
	}
}

// compressPDFBytes 以最高无损等级编码单个流
// 入参: ctx 取消上下文, data 原始数据
// 返回: []byte 压缩数据, error 压缩错误
func compressPDFBytes(ctx context.Context, data []byte) ([]byte, error) {
	return compressPDFBytesLevel(ctx, data, zlib.BestCompression)
}

// compressPDFBytesLevel 按指定zlib级别编码单个流
// 入参: ctx 取消上下文, data 原始数据, level zlib压缩级别
// 返回: []byte 压缩数据, error 压缩错误
func compressPDFBytesLevel(ctx context.Context, data []byte, level int) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	var result bytes.Buffer
	writer := optimizationCompressorLevel(&result, level)
	defer releaseOptimizationCompressorLevel(writer, level)
	for len(data) > 0 {
		if err := ctx.Err(); err != nil {
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
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return result.Bytes(), nil
}

// recompressPDFBytes 流式重压缩已有Flate编码，不持有完整解码副本且只采用更小结果
// 入参: ctx 取消上下文, data 原始编码
// 返回: []byte 更小编码或原文, error 取消错误
func recompressPDFBytes(ctx context.Context, data []byte) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	input, err := acquireZlibReader(bytes.NewReader(data))
	if err != nil {
		return data, ctx.Err()
	}
	defer func() {
		input.Close()
		releaseZlibReader(input)
	}()
	var result bytes.Buffer
	output := &optimizationBuffer{buffer: &result, limit: min(len(data), optimizationBufferLimit)}
	encoder := optimizationCompressor(output)
	defer releaseOptimizationCompressor(encoder)
	n, err := io.Copy(encoder, io.LimitReader(&contextInput{ctx: ctx, reader: input}, optimizationBufferLimit+1))
	closeErr := encoder.Close()
	if err != nil || closeErr != nil || n > optimizationBufferLimit || result.Len() >= len(data) {
		return data, ctx.Err()
	}
	return result.Bytes(), ctx.Err()
}

// OptimizeTo 重写PDF对象及资源并保留未知字典、表单、附件、导航与加密配置，无损仅改表示压缩、有损可重编可识别RGB/灰度图
// 签名文件原样交付，出错须丢弃输出，调用期间Reader须保持源数据可读
// 入参: ctx 取消上下文, writer 输出流, options 优化配置
// 返回: OptimizeReport 优化结果, error 读写错误
func (r *Reader) OptimizeTo(ctx context.Context, writer io.Writer, options OptimizeOptions) (report OptimizeReport, err error) {
	return r.rewriteTo(ctx, writer, options, nil, nil, nil)
}

// rewriteTo 写出资源替换与可选优化，保持原阅读器的对象和安全配置
// 入参: ctx 取消上下文, writer 输出流, options 优化配置, replacements 替换对象, lazyImages 延迟编码替换图像, information 信息字典修改
// 返回: OptimizeReport 写出结果, error 读写错误
func (r *Reader) rewriteTo(ctx context.Context, writer io.Writer, options OptimizeOptions, replacements map[Reference]Object, lazyImages map[Reference]*lazyReplacementImage, information Dictionary) (report OptimizeReport, err error) {
	if r == nil || ctx == nil {
		return report, fmt.Errorf("invalid PDF optimize context")
	}
	if r.isClosed() {
		return report, os.ErrClosed
	}
	if err := options.Compression.Validate(); err != nil {
		return report, err
	}
	if writer == nil {
		return report, fmt.Errorf("missing output writer")
	}
	embeddedFiles := make(map[Reference]bool)
	trackEmbedded := r.security != nil && r.security.embeddedFilter != r.security.streamFilter
	out := &pdfOutput{ctx: ctx, writer: writer, reader: r, security: r.security, embeddedFiles: embeddedFiles}
	defer func() { report.Bytes = out.count }()
	progress := func(stage string, done, total int) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if options.OnProgress != nil {
			if err := options.OnProgress(stage, done, total); err != nil {
				return err
			}
		}
		return ctx.Err()
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
	if options.Compression.Mode == CompressionUnchanged && len(replacements) == 0 && len(lazyImages) == 0 && len(information) == 0 {
		return report, copySource()
	}
	object := func(ref Reference) (Object, error) {
		if value, ok := replacements[ref]; ok {
			return value, nil
		}
		return r.object(ref, embeddedFiles[ref])
	}
	trailer := maps.Clone(r.Trailer)
	for _, name := range []Name{"Prev", "XRefStm", "Type", "W", "Index", "Length", "Filter", "DecodeParms"} {
		delete(trailer, name)
	}
	if len(information) != 0 {
		value, err := r.Resolve(trailer["Info"])
		if err != nil {
			return report, err
		}
		info := Dictionary{}
		if value != nil {
			dict, ok := value.(Dictionary)
			if !ok {
				return report, fmt.Errorf("invalid document information dictionary")
			}
			info = maps.Clone(dict)
		}
		for key, value := range information {
			if value == nil {
				delete(info, key)
			} else {
				info[key] = value
			}
		}
		trailer["Info"] = info
	}
	seen := make(map[Reference]bool)
	isStream := make(map[Reference]bool)
	metadata := make(map[Reference]Object)
	streamHashes := make(map[Reference][32]byte)
	var refs []Reference
	var danglingIDs []int64
	var maskRoots []Object
	var maskRefs []Reference
	var noDownsampleRoots []Object
	var noDownsampleRefs []Reference
	var dataRoots []Object
	var dataRefs []Reference
	var efRoots []Dictionary
	lossless := make(map[Reference]bool)
	noDownsample := make(map[Reference]bool)
	exactData := make(map[Reference]bool)
	protecting := false
	protectingScale := false
	preservingData := false
	signed := false
	isLiveRef := func(ref Reference) bool {
		if _, ok := replacements[ref]; ok || lazyImages[ref] != nil {
			return true
		}
		return r.isLiveXrefRef(ref)
	}
	originalCache := r.cache.snapshot()
	originalObjectStreams := r.objectStreams
	defer func() { r.objectStreams = originalObjectStreams }()
	defer r.cache.prune(originalCache)
	resolveOptValue := func(val Object) (Object, error) {
		cur := val
		var chain map[Reference]bool
		for {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			ref, isRef := cur.(Reference)
			if !isRef {
				return cur, nil
			}
			if chain[ref] || !isLiveRef(ref) {
				return nil, nil
			}
			if chain == nil {
				chain = make(map[Reference]bool)
			}
			chain[ref] = true
			if rep, ok := replacements[ref]; ok {
				cur = rep
				continue
			}
			if lazyImages[ref] != nil {
				return nil, nil
			}
			resolved, readErr := r.Object(ref)
			if !originalCache[ref] {
				r.cache.delete(ref)
			}
			if readErr != nil {
				return nil, readErr
			}
			cur = resolved
		}
	}
	var collect func(Object, int) error
	collect = func(value Object, depth int) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if depth > 256 {
			return fmt.Errorf("excessive PDF object nesting")
		}
		switch v := value.(type) {
		case Reference:
			if v.Number <= 0 || v.Generation < 0 || v.Generation > 65535 {
				return fmt.Errorf("invalid PDF reference")
			}
			if !isLiveRef(v) {
				danglingIDs = append(danglingIDs, v.Number)
				return nil
			}
			if !seen[v] {
				seen[v] = true
				refs = append(refs, v)
			}
			if protecting && !lossless[v] {
				lossless[v] = true
				maskRefs = append(maskRefs, v)
			}
			if protectingScale && !noDownsample[v] {
				noDownsample[v] = true
				noDownsampleRefs = append(noDownsampleRefs, v)
			}
			if preservingData && !exactData[v] {
				exactData[v] = true
				dataRefs = append(dataRefs, v)
			}
		case Array:
			for _, item := range v {
				if err := collect(item, depth+1); err != nil {
					return err
				}
			}
		case Dictionary:
			if ref, ok := v["SMask"].(Reference); ok && (replacements[ref] != nil || lazyImages[ref] != nil) {
				if _, original := r.xref[ref.Number]; original {
					return fmt.Errorf("cannot replace a soft-mask image")
				}
			}
			if !protecting && !protectingScale && !preservingData {
				for _, key := range []Name{"JS", "EF", "Sound"} {
					if v[key] != nil {
						dataRoots = append(dataRoots, v[key])
					}
				}
				if trackEmbedded && v["EF"] != nil {
					if t, direct := v["Type"].(Name); !direct || t == Name("Filespec") {
						efRoots = append(efRoots, v)
					}
				}
			}
			if !protecting && !protectingScale && !preservingData && options.Compression.Mode == CompressionLossy {
				for _, key := range []Name{"SMask", "Mask"} {
					if v[key] != nil {
						maskRoots = append(maskRoots, v[key])
					}
				}
				if v["Alternates"] != nil {
					if v["ImageMask"] == Boolean(true) {
						maskRoots = append(maskRoots, v["Alternates"])
					} else if _, indirect := v["ImageMask"].(Reference); indirect {
						maskVal, maskErr := resolveOptValue(v["ImageMask"])
						if maskErr != nil {
							return maskErr
						}
						if maskVal == Boolean(true) {
							maskRoots = append(maskRoots, v["Alternates"])
						}
					}
				}
				if options.Compression.ImageDPI() > 0 && v["CharProcs"] != nil {
					noDownsampleRoots = append(noDownsampleRoots, v["CharProcs"])
				}
			}
			if !signed && !protecting && !protectingScale && !preservingData {
				if v["Type"] == Name("Sig") || v["Type"] == Name("DocTimeStamp") || v["ByteRange"] != nil && v["Contents"] != nil {
					signed = true
				} else if _, indirect := v["Type"].(Reference); indirect {
					dictType, typeErr := resolveOptValue(v["Type"])
					if typeErr != nil {
						return typeErr
					}
					if dictType == Name("Sig") || dictType == Name("DocTimeStamp") {
						signed = true
					}
				}
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
	scanned := make(map[Reference]bool)
	scannedEmbedded := make(map[Reference]bool)
	deferredSecurity := make(map[Reference]error)
	var deferredOrder []Reference
	var reprocess []Reference
	queuedReprocess := make(map[Reference]bool)
	finishSigned := func() error {
		if len(replacements) != 0 || len(lazyImages) != 0 || len(information) != 0 {
			return ErrSignedDocument
		}
		return copySource()
	}
	markEmbeddedFromEF := func(host Dictionary) error {
		isSpec, specErr := isFileSpecDictionary(ctx, host, resolveOptValue)
		if specErr != nil || !isSpec {
			return specErr
		}
		cur, efErr := resolveOptValue(host["EF"])
		if efErr != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			var secErr *objectSecurityError
			if errors.As(efErr, &secErr) {
				return nil
			}
			return efErr
		}
		efDict, ok := cur.(Dictionary)
		if !ok {
			return nil
		}
		for _, key := range embeddedFileSpecKeys {
			if err := ctx.Err(); err != nil {
				return err
			}
			entryRef, isRef := efDict[key].(Reference)
			if !isRef {
				continue
			}
			chain := make(map[Reference]bool)
			for {
				if err := ctx.Err(); err != nil {
					return err
				}
				if chain[entryRef] || embeddedFiles[entryRef] || !isLiveRef(entryRef) {
					break
				}
				chain[entryRef] = true
				var resolved Object
				if rep, ok := replacements[entryRef]; ok {
					resolved = rep
				} else if lazyImages[entryRef] != nil {
					break
				} else {
					var readErr error
					resolved, readErr = r.object(entryRef, true)
					if !originalCache[entryRef] {
						r.cache.delete(entryRef)
					}
					if readErr != nil {
						var secErr *objectSecurityError
						if errors.As(readErr, &secErr) {
							alt, altErr := r.object(entryRef, false)
							if !originalCache[entryRef] {
								r.cache.delete(entryRef)
							}
							if altErr == nil {
								if altStream, ok := alt.(*Stream); ok {
									isEF, efCheckErr := isEmbeddedFileStream(ctx, altStream, resolveOptValue)
									if efCheckErr != nil {
										return efCheckErr
									}
									if !isEF {
										break
									}
								}
							}
							if _, exists := deferredSecurity[entryRef]; !exists {
								deferredOrder = append(deferredOrder, entryRef)
							}
							deferredSecurity[entryRef] = readErr
							break
						}
						return readErr
					}
				}
				if next, ok := resolved.(Reference); ok {
					entryRef = next
					continue
				}
				if stream, isStreamObj := resolved.(*Stream); isStreamObj {
					isEF, efCheckErr := isEmbeddedFileStream(ctx, stream, resolveOptValue)
					if efCheckErr != nil {
						return efCheckErr
					}
					if !isEF {
						break
					}
					embeddedFiles[entryRef] = true
					if _, deferred := deferredSecurity[entryRef]; deferred {
						delete(deferredSecurity, entryRef)
						if !queuedReprocess[entryRef] {
							queuedReprocess[entryRef] = true
							reprocess = append(reprocess, entryRef)
						}
					} else if scanned[entryRef] && !scannedEmbedded[entryRef] && !queuedReprocess[entryRef] {
						queuedReprocess[entryRef] = true
						reprocess = append(reprocess, entryRef)
					}
				}
				break
			}
		}
		return nil
	}
	drainEF := func() error {
		for len(efRoots) > 0 {
			if err := ctx.Err(); err != nil {
				return err
			}
			root := efRoots[0]
			efRoots = efRoots[1:]
			if err := markEmbeddedFromEF(root); err != nil {
				return err
			}
		}
		return nil
	}
	if signed {
		return report, finishSigned()
	}
	if err := drainEF(); err != nil {
		return report, err
	}
	for i := 0; i < len(refs) || len(reprocess) > 0; {
		var ref Reference
		allowDefer := true
		if len(reprocess) > 0 {
			ref = reprocess[0]
			reprocess = reprocess[1:]
			delete(queuedReprocess, ref)
			allowDefer = false
		} else {
			if err := progress("scan", i, len(refs)); err != nil {
				return report, err
			}
			ref = refs[i]
			i++
		}
		var value Object
		if lazy := lazyImages[ref]; lazy != nil {
			value = lazy.baseDict
		} else {
			value, err = object(ref)
			if err != nil {
				var secErr *objectSecurityError
				if allowDefer && trackEmbedded && !embeddedFiles[ref] && errors.As(err, &secErr) {
					if _, exists := deferredSecurity[ref]; !exists {
						deferredOrder = append(deferredOrder, ref)
					}
					deferredSecurity[ref] = err
					continue
				}
				return report, err
			}
		}
		scanned[ref] = true
		if embeddedFiles[ref] {
			scannedEmbedded[ref] = true
		}
		if err = collect(value, 0); err != nil {
			return report, err
		}
		if signed {
			if !originalCache[ref] {
				r.cache.delete(ref)
			}
			return report, finishSigned()
		}
		if err = drainEF(); err != nil {
			return report, err
		}
		if stream, ok := value.(*Stream); ok {
			isStream[ref] = true
			metadata[ref] = stream.Dictionary
			streamHashes[ref] = sha256.Sum256(stream.Data)
			streamType, typeErr := resolveObjectType(r, stream.Dictionary["Type"], ref)
			if typeErr != nil {
				return report, typeErr
			}
			if streamType == Name("EmbeddedFile") {
				dataRoots = append(dataRoots, ref)
			}
		} else {
			metadata[ref] = value
		}
		if !originalCache[ref] {
			r.cache.delete(ref)
		}
	}
	for _, ref := range deferredOrder {
		if err, exists := deferredSecurity[ref]; exists {
			return report, err
		}
	}
	for ref := range replacements {
		if !seen[ref] {
			return report, fmt.Errorf("replacement object %d is not reachable", ref.Number)
		}
	}
	for ref := range lazyImages {
		if !seen[ref] {
			return report, fmt.Errorf("replacement object %d is not reachable", ref.Number)
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
		var value Object
		if lazy := lazyImages[maskRefs[i]]; lazy != nil {
			value = lazy.baseDict
		} else {
			value, err = object(maskRefs[i])
			if err != nil {
				return report, err
			}
		}
		if err := collect(value, 0); err != nil {
			return report, err
		}
		if !originalCache[maskRefs[i]] {
			r.cache.delete(maskRefs[i])
		}
	}
	protecting = false
	protectingScale = true
	for _, root := range noDownsampleRoots {
		if err := collect(root, 0); err != nil {
			return report, err
		}
	}
	for i := 0; i < len(noDownsampleRefs); i++ {
		if err := progress("scan", i, len(noDownsampleRefs)); err != nil {
			return report, err
		}
		ref := noDownsampleRefs[i]
		if value, found := metadata[ref]; found {
			if err := collect(value, 0); err != nil {
				return report, err
			}
		}
	}
	protectingScale = false
	preservingData = true
	for _, root := range dataRoots {
		if err := collect(root, 0); err != nil {
			return report, err
		}
	}
	for i := 0; i < len(dataRefs); i++ {
		if err := progress("scan", i, len(dataRefs)); err != nil {
			return report, err
		}
		ref := dataRefs[i]
		if replacements[ref] != nil || lazyImages[ref] != nil {
			if _, original := r.xref[ref.Number]; original {
				return report, fmt.Errorf("cannot replace a stream shared with original file data")
			}
		}
		value, found := metadata[ref]
		if !found {
			return report, fmt.Errorf("missing PDF optimization object metadata")
		}
		if err := collect(value, 0); err != nil {
			return report, err
		}
	}
	var imageSizes map[Reference]image.Point
	var fontGlyphs map[Reference][]uint16
	var imageColorResources map[Reference]Dictionary
	var colorConflicts map[Reference]bool
	workers := optimizeWorkers(options.Workers)
	protectedAliases := make(map[Reference]bool)
	for _, key := range []Name{"Root", "Encrypt", "Info"} {
		if ref, ok := trailer[key].(Reference); ok {
			protectedAliases[ref] = true
		}
	}
	if r.security != nil || trailer["Encrypt"] != nil {
		out.unencrypted, err = collectUnencryptedRefs(trailer, func(ref Reference) (Object, error) {
			if value, ok := metadata[ref]; ok {
				if isStream[ref] {
					return &Stream{Dictionary: value.(Dictionary)}, nil
				}
				return value, nil
			}
			return object(ref)
		})
		if err != nil {
			return report, err
		}
		for ref := range out.unencrypted {
			protectedAliases[ref] = true
		}
	}
	for ref := range embeddedFiles {
		protectedAliases[ref] = true
	}
	if options.Compression.Mode != CompressionUnchanged {
		if err := progress("compress", 0, 0); err != nil {
			return report, err
		}
		imageSizes, err = r.imageOutputSizes(ctx, options.Compression.ImageDPI())
		if err != nil {
			return report, err
		}
		fontGlyphs, err = r.optimizationFontGlyphs(ctx, metadata, originalCache)
		if err != nil {
			return report, err
		}
		out.aliases, err = r.optimizationAliases(ctx, refs, isStream, metadata, streamHashes, lazyImages, protectedAliases, false)
		if err != nil {
			return report, err
		}
	} else {
		out.aliases, err = r.optimizationAliases(ctx, refs, isStream, metadata, streamHashes, lazyImages, protectedAliases, true)
		if err != nil {
			return report, err
		}
	}
	for ref, canonical := range out.aliases {
		if len(fontGlyphs[canonical]) > 0 && len(fontGlyphs[ref]) > 0 {
			fontGlyphs[canonical] = append(fontGlyphs[canonical], fontGlyphs[ref]...)
			slices.Sort(fontGlyphs[canonical])
			fontGlyphs[canonical] = slices.Compact(fontGlyphs[canonical])
		} else {
			delete(fontGlyphs, canonical)
		}
		delete(fontGlyphs, ref)
		lossless[canonical] = lossless[canonical] || lossless[ref]
		noDownsample[canonical] = noDownsample[canonical] || noDownsample[ref]
		exactData[canonical] = exactData[canonical] || exactData[ref]
		a, b := imageSizes[canonical], imageSizes[ref]
		if imageSizes != nil {
			if a.X == 0 || a.Y == 0 || b.X == 0 || b.Y == 0 {
				imageSizes[canonical] = image.Point{}
			} else {
				imageSizes[canonical] = image.Pt(max(a.X, b.X), max(a.Y, b.Y))
			}
		}
	}
	for ref := range noDownsample {
		if noDownsample[ref] {
			delete(imageSizes, ref)
		}
	}
	for ref := range exactData {
		if exactData[ref] {
			delete(fontGlyphs, ref)
		}
	}
	refs = slices.DeleteFunc(refs, func(ref Reference) bool { return out.aliases[ref].Number != 0 })
	var fontUpdates map[Reference]*Stream
	var fontTags map[Reference]string
	if options.Compression.Mode != CompressionUnchanged {
		fontUpdates, fontTags, err = r.optimizationFontUpdates(ctx, fontGlyphs, originalCache, workers)
		if err != nil {
			return report, err
		}
	}
	ownedSMask := make(map[Reference]Reference)
	ownedHardMask := make(map[Reference]Reference)
	deferredSMask := make(map[Reference]bool)
	smaskCache := make(map[Reference]smaskOptInfo)
	if options.Compression.Mode == CompressionLossy {
		imageColorResources, colorConflicts, err = r.imageDefaultColorResources(ctx, refs, metadata, out.aliases)
		if err != nil {
			return report, err
		}
		refCounts := make(map[Reference]int)
		var countRefs func(Object, int)
		countRefs = func(val Object, depth int) {
			if depth > 256 {
				return
			}
			switch v := val.(type) {
			case Reference:
				if canonical, ok := out.aliases[v]; ok {
					v = canonical
				}
				refCounts[v]++
			case Array:
				for _, item := range v {
					countRefs(item, depth+1)
				}
			case Dictionary:
				for _, item := range v {
					countRefs(item, depth+1)
				}
			}
		}
		countRefs(trailer, 0)
		for _, ref := range refs {
			countRefs(metadata[ref], 0)
		}
		for _, ref := range refs {
			if !isStream[ref] || lossless[ref] || colorConflicts[ref] || exactData[ref] || protectedAliases[ref] || lazyImages[ref] != nil || replacements[ref] != nil || fontUpdates[ref] != nil {
				continue
			}
			dict, ok := metadata[ref].(Dictionary)
			if !ok || dict["Subtype"] != Name("Image") || dict["SMask"] == nil {
				continue
			}
			var ownedSMaskRef Reference
			if smaskRef, ok := dict["SMask"].(Reference); ok {
				if canonical, aliased := out.aliases[smaskRef]; aliased {
					smaskRef = canonical
				}
				if smaskRef != ref && isStream[smaskRef] && refCounts[smaskRef] == 1 && !exactData[smaskRef] && lazyImages[smaskRef] == nil && replacements[smaskRef] == nil && fontUpdates[smaskRef] == nil {
					if smaskDict, ok := metadata[smaskRef].(Dictionary); ok && smaskDict["Subtype"] == Name("Image") && smaskDict["SMask"] == nil {
						ownedSMask[ref] = smaskRef
						deferredSMask[smaskRef] = true
						ownedSMaskRef = smaskRef
					}
				}
			}
			if maskRef, ok := dict["Mask"].(Reference); ok {
				if canonical, aliased := out.aliases[maskRef]; aliased {
					maskRef = canonical
				}
				if maskRef != ref && maskRef != ownedSMaskRef && !deferredSMask[maskRef] && isLiveRef(maskRef) && refCounts[maskRef] == 1 && !protectedAliases[maskRef] && !exactData[maskRef] && lazyImages[maskRef] == nil && replacements[maskRef] == nil && fontUpdates[maskRef] == nil {
					ownedHardMask[ref] = maskRef
					deferredSMask[maskRef] = true
				}
			}
		}
	}
	slices.SortFunc(refs, compareReference)
	filteredRefs := refs[:0]
	for i := 0; i < len(refs); {
		j := i + 1
		for j < len(refs) && refs[j].Number == refs[i].Number {
			j++
		}
		liveCount := 0
		var liveRef Reference
		for _, candidate := range refs[i:j] {
			if isLiveRef(candidate) {
				liveCount++
				liveRef = candidate
			}
		}
		if liveCount > 1 {
			return report, fmt.Errorf("conflicting PDF object generations")
		}
		if liveCount == 1 {
			filteredRefs = append(filteredRefs, liveRef)
		}
		i = j
	}
	refs = filteredRefs
	maxID := int64(0)
	for _, ref := range refs {
		maxID = max(maxID, ref.Number)
	}
	limit := int64(math.MaxInt32) - int64(len(lazyImages)) - int64(len(refs)/32) - 1024
	if maxID > limit {
		return report, fmt.Errorf("PDF object number exceeds output limit")
	}
	for id := range r.xref {
		if id > maxID && id <= limit {
			maxID = id
		}
	}
	slices.Sort(danglingIDs)
	for _, id := range danglingIDs {
		if id > maxID && id <= maxID+1<<20 && id <= limit {
			maxID = id
		}
	}
	var pageTreeOverrides map[Reference]Object
	if balanced, treeErr := buildBalancedPageTree(ctx, trailer, refs, isStream, metadata, out.aliases, &maxID); treeErr != nil {
		return report, treeErr
	} else if balanced != nil {
		pageTreeOverrides = balanced.overrides
		if len(balanced.removed) != 0 {
			refs = slices.DeleteFunc(refs, func(ref Reference) bool { return balanced.removed[ref] })
		}
		refs = append(refs, balanced.added...)
	}
	report.Objects = len(refs)
	infoRef, _ := trailer["Info"].(Reference)
	var newInfo Dictionary
	if options.Creator != "" || len(information) != 0 {
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
		if options.Creator != "" {
			newInfo["Creator"], newInfo["Producer"] = String(encoded), String(encoded)
		}
		if infoRef.Number == 0 || !isLiveRef(infoRef) {
			maxID++
			infoRef = Reference{Number: maxID}
			refs = append(refs, infoRef)
			report.Objects++
			trailer["Info"] = infoRef
		}
	}
	out.live = make(map[Reference]bool, len(refs)+len(lazyImages))
	for _, ref := range refs {
		out.live[ref] = true
	}
	version := r.Version
	if newInfo != nil && version < "1.2" {
		version = "1.2"
	}
	for _, value := range replacements {
		if dict, ok := value.(Dictionary); ok {
			if dict["Subtype"] == Name("Movie") {
				version = max(version, "1.2")
			}
			if dict["Subtype"] == Name("Screen") || dict["Type"] == Name("Rendition") {
				version = max(version, "1.5")
			}
			if dict["Type"] == Name("Filespec") && dict["UF"] != nil {
				version = max(version, "1.7")
			}
			if title, ok := dict["Title"].(String); ok && bytes.HasPrefix(title, []byte{0xfe, 0xff}) && version < "1.2" {
				version = "1.2"
			}
			dest, _ := dict["Dest"].(Array)
			for _, key := range []Name{"A", "OpenAction"} {
				if key == "A" && dict["Subtype"] == Name("Movie") {
					continue
				}
				if _, ok := dict[key].(Dictionary); !ok {
					continue
				}
				required, err := r.navigationVersion(ctx, dict[key])
				if err != nil {
					return report, err
				}
				version = max(version, required)
			}
			pageVersion, err := r.pageActionVersion(ctx, dict)
			if err != nil {
				return report, err
			}
			version = max(version, pageVersion)
			if action, ok := dict["A"].(Dictionary); ok && action["S"] == Name("GoTo") {
				dest, _ = action["D"].(Array)
			}
			if len(dest) >= 2 && version < "1.1" && (dest[1] == Name("FitB") || dest[1] == Name("FitBH") || dest[1] == Name("FitBV")) {
				version = "1.1"
			}
		}
		if dict, ok := value.(Dictionary); ok && dict["Subtype"] == Name("Link") && dict["QuadPoints"] != nil && version < "1.6" {
			version = "1.6"
		}
		if stream, ok := value.(*Stream); ok {
			if stream.Dictionary["SMask"] != nil && version < "1.4" {
				version = "1.4"
			}
			if stream.Dictionary["BitsPerComponent"] == Integer(16) && version < "1.5" {
				version = "1.5"
			}
			version = max(version, r.streamFilterVersion(stream.Dictionary))
		}
	}
	for ref := range isStream {
		if dict, ok := metadata[ref].(Dictionary); ok {
			version = max(version, r.streamFilterVersion(dict))
		}
	}
	for _, stream := range fontUpdates {
		if stream != nil {
			version = max(version, r.streamFilterVersion(stream.Dictionary))
		}
	}
	if len(lazyImages) > 0 && version < "1.2" {
		version = "1.2"
	}
	if options.Compression.Mode != CompressionUnchanged && len(isStream) > 0 && version < "1.2" {
		version = "1.2"
	}
	version, err = replacementImageVersion(ctx, version, lazyImages)
	if err != nil {
		return report, err
	}
	if !validPDFVersion(version) {
		return report, fmt.Errorf("invalid PDF output version %q", version)
	}
	if _, err := fmt.Fprintf(out, "%%PDF-%s\n%%\xE2\xE3\xCF\xD3\n", version); err != nil {
		return report, err
	}
	streamCompressLevel := zlib.BestCompression
	if options.Compression.Mode == CompressionUnchanged {
		streamCompressLevel = zlib.DefaultCompression
	}
	type entry struct {
		kind       byte
		position   int64
		generation int64
	}
	entries := map[int64]entry{0: {0, 0, 65535}}
	packed := version >= "1.5" && (r.security == nil || r.security.stringFilter == r.security.streamFilter)
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
		data, err := compressPDFBytesLevel(ctx, header.Bytes(), streamCompressLevel)
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
	lookupSMaskInfo := func(smaskVal Object) (smaskOptInfo, error) {
		var info smaskOptInfo
		var seenMask map[Reference]bool
		var chainRefs []Reference
		cur := smaskVal
		for {
			if err := ctx.Err(); err != nil {
				return info, err
			}
			sRef, isRef := cur.(Reference)
			if !isRef {
				break
			}
			if canonical, aliased := out.aliases[sRef]; aliased {
				sRef = canonical
			}
			if cachedInfo, ok := smaskCache[sRef]; ok {
				for _, rRef := range chainRefs {
					smaskCache[rRef] = cachedInfo
				}
				return cachedInfo, nil
			}
			if seenMask[sRef] {
				info.hasMatte = true
				return info, nil
			}
			if seenMask == nil {
				seenMask = make(map[Reference]bool)
			}
			seenMask[sRef] = true
			chainRefs = append(chainRefs, sRef)
			sObj, sErr := object(sRef)
			if !originalCache[sRef] {
				r.cache.delete(sRef)
			}
			if sErr != nil {
				if isFatalOptimizationReadError(ctx, sErr) {
					return info, sErr
				}
				info.hasMatte = true
				return info, nil
			}
			cur = sObj
		}
		sStream, ok := cur.(*Stream)
		if !ok || sStream == nil {
			info.hasMatte = true
			return info, nil
		}
		opaque, sCheckErr := r.isSMaskOpaque(ctx, sStream)
		if sCheckErr != nil {
			return info, sCheckErr
		}
		info.opaque = opaque
		if !info.opaque {
			matteVal, matteErr := r.optimizationValue(sStream.Dictionary["Matte"])
			if matteErr != nil {
				if isFatalOptimizationReadError(ctx, matteErr) {
					return info, matteErr
				}
				info.hasMatte = true
			} else if matteVal != nil {
				info.hasMatte = true
			}
		}
		for _, rRef := range chainRefs {
			smaskCache[rRef] = info
		}
		return info, nil
	}
	buildStreamJob := func(ref Reference) (*streamOptJob, error) {
		if deferredSMask[ref] {
			return nil, nil
		}
		lazy := lazyImages[ref]
		if lazy == nil && (!isStream[ref] || options.Compression.Mode == CompressionUnchanged || fontUpdates[ref] != nil) {
			return nil, nil
		}
		compression := options.Compression
		if lossless[ref] || colorConflicts[ref] {
			compression.Mode = CompressionLossless
		}
		job := &streamOptJob{
			ref:            ref,
			embedded:       embeddedFiles[ref],
			lazy:           lazy,
			compression:    compression,
			mode:           options.Compression.Mode,
			exactData:      exactData[ref],
			size:           imageSizes[ref],
			colorResources: imageColorResources[ref],
		}
		if lazy == nil {
			if rep, ok := replacements[ref]; ok {
				if s, ok := rep.(*Stream); ok {
					job.stream = s
				} else {
					return nil, nil
				}
			}
			if hardMaskRef, hasHard := ownedHardMask[ref]; hasHard && compression.Mode == CompressionLossy {
				job.ownedHardMaskRef = hardMaskRef
			}
			if smaskRef, hasOwned := ownedSMask[ref]; hasOwned && compression.Mode == CompressionLossy {
				job.ownedMaskRef = smaskRef
				if rep, ok := replacements[smaskRef]; ok {
					job.ownedMaskObj = rep
				}
			} else if compression.Mode == CompressionLossy && !exactData[ref] {
				if dict, ok := metadata[ref].(Dictionary); ok && dict["SMask"] != nil && dict["Subtype"] == Name("Image") {
					info, infoErr := lookupSMaskInfo(dict["SMask"])
					if infoErr != nil {
						return nil, infoErr
					}
					job.hasSMaskInfo = true
					job.smaskInfo = info
				}
			}
		}
		return job, nil
	}
	useParallel := workers > 1 && (options.Compression.Mode != CompressionUnchanged || len(lazyImages) > 1)
	var workCtx context.Context
	var workCancel context.CancelFunc
	var taskCh chan *streamOptJob
	var wg sync.WaitGroup
	var pendingJobs map[int]*streamOptJob
	var firstErrOnce sync.Once
	var firstErr atomic.Pointer[error]
	prefetchCursor := 0
	windowLimit := workers * 2
	if useParallel {
		origWarning := r.warning
		sharedWarning := syncWarning(origWarning)
		r.warning = sharedWarning
		wasShared := r.cache != nil && r.cache.shared.Swap(true)
		workCtx, workCancel = context.WithCancel(ctx)
		taskCh = make(chan *streamOptJob, windowLimit)
		pendingJobs = make(map[int]*streamOptJob, windowLimit)
		wg.Add(workers)
		for range workers {
			wr := r.workerReader(sharedWarning)
			go func() {
				defer wg.Done()
				for job := range taskCh {
					wr.executeStreamJob(workCtx, job)
					if job.err != nil {
						if ctx.Err() == nil && !errors.Is(job.err, context.Canceled) {
							errCopy := job.err
							firstErrOnce.Do(func() { firstErr.Store(&errCopy) })
						}
						workCancel()
					}
					close(job.done)
				}
			}()
		}
		defer func() {
			workCancel()
			close(taskCh)
			wg.Wait()
			if r.cache != nil && !wasShared {
				r.cache.shared.Store(false)
			}
			r.warning = origWarning
			r.cache.pruneObjectsIfOver(originalCache, 0)
		}()
	}
	fillPrefetch := func(current int) error {
		if !useParallel {
			return nil
		}
		if prefetchCursor < current {
			prefetchCursor = current
		}
		scanLimit := min(len(refs), current+windowLimit*8)
		for len(pendingJobs) < windowLimit && prefetchCursor < scanLimit {
			idx := prefetchCursor
			prefetchCursor++
			job, jobErr := buildStreamJob(refs[idx])
			if jobErr != nil {
				return jobErr
			}
			if job == nil {
				continue
			}
			job.done = make(chan struct{})
			pendingJobs[idx] = job
			taskCh <- job
		}
		return nil
	}
	for i, ref := range refs {
		if err := progress("write", i, len(refs)); err != nil {
			return report, err
		}
		if deferredSMask[ref] {
			continue
		}
		if err := fillPrefetch(i); err != nil {
			return report, err
		}
		var value Object
		var maskStream *Stream
		var maskRef Reference
		var ownedMaskOut Object
		var ownedMaskRef Reference
		var ownedHardMaskOut Object
		var ownedHardMaskRef Reference
		var job *streamOptJob
		resolved := false
		if pending, ok := pendingJobs[i]; ok {
			delete(pendingJobs, i)
			if err := fillPrefetch(i + 1); err != nil {
				return report, err
			}
			<-pending.done
			if pending.err != nil {
				err := pending.err
				if errors.Is(err, context.Canceled) && ctx.Err() == nil {
					if root := firstErr.Load(); root != nil {
						err = *root
					}
				}
				return report, err
			}
			job = pending
		} else if ref == infoRef && newInfo != nil {
			value = newInfo
			resolved = true
		} else if override, ok := pageTreeOverrides[ref]; ok {
			value = override
			resolved = true
		} else if !useParallel {
			var jobErr error
			job, jobErr = buildStreamJob(ref)
			if jobErr != nil {
				return report, jobErr
			}
			if job != nil {
				if options.Compression.Mode != CompressionUnchanged {
					if err := progress("compress", report.Streams, 0); err != nil {
						return report, err
					}
				}
				r.executeStreamJob(ctx, job)
				if job.err != nil {
					return report, job.err
				}
			}
		}
		if job != nil {
			value = job.outValue
			maskStream = job.outLazyMask
			ownedMaskOut = job.outOwnedMask
			if job.ownedMaskRef.Number > 0 {
				if job.droppedSMask {
					delete(out.live, job.ownedMaskRef)
					report.Objects--
				} else {
					ownedMaskRef = job.ownedMaskRef
				}
				if !originalCache[job.ownedMaskRef] {
					r.cache.delete(job.ownedMaskRef)
				}
			}
			if job.ownedHardMaskRef.Number > 0 {
				if job.droppedSMask {
					delete(out.live, job.ownedHardMaskRef)
					report.Objects--
				} else {
					ownedHardMaskRef = job.ownedHardMaskRef
					ownedHardMaskOut = job.outOwnedHardMask
				}
				if !originalCache[job.ownedHardMaskRef] {
					r.cache.delete(job.ownedHardMaskRef)
				}
			}
			if maskStream != nil {
				maxID++
				maskRef = Reference{Number: maxID}
				out.live[maskRef] = true
				if mainStream, ok := value.(*Stream); ok {
					updated := *mainStream
					updated.Dictionary = maps.Clone(mainStream.Dictionary)
					updated.Dictionary["SMask"] = maskRef
					value = &updated
				}
			}
			resolved = true
		} else if !resolved {
			if optimized := fontUpdates[ref]; optimized != nil {
				value = optimized
			} else {
				value, err = object(ref)
				if err != nil {
					return report, err
				}
			}
		}
		if _, isStreamVal := value.(*Stream); isStreamVal && (job != nil || !resolved) {
			report.Streams++
			if options.Compression.Mode != CompressionUnchanged {
				if err := progress("compress", report.Streams, 0); err != nil {
					return report, err
				}
			}
		}
		value = r.optimizedFontNames(value, fontTags, out.aliases, 0)
		if _, stream := value.(*Stream); packed && !stream && ref.Generation == 0 && !out.unencrypted[ref] {
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
		if ownedMaskRef.Number > 0 {
			if err := writeObject(ownedMaskRef, ownedMaskOut); err != nil {
				return report, err
			}
			if _, isMaskStream := ownedMaskOut.(*Stream); isMaskStream {
				report.Streams++
				if options.Compression.Mode != CompressionUnchanged {
					if err := progress("compress", report.Streams, 0); err != nil {
						return report, err
					}
				}
			}
		}
		if ownedHardMaskRef.Number > 0 {
			if err := writeObject(ownedHardMaskRef, ownedHardMaskOut); err != nil {
				return report, err
			}
			if _, isMaskStream := ownedHardMaskOut.(*Stream); isMaskStream {
				report.Streams++
				if options.Compression.Mode != CompressionUnchanged {
					if err := progress("compress", report.Streams, 0); err != nil {
						return report, err
					}
				}
			}
		}
		if maskStream != nil {
			if err := writeObject(maskRef, maskStream); err != nil {
				return report, err
			}
			report.Objects++
			report.Streams++
			if options.Compression.Mode != CompressionUnchanged {
				if err := progress("compress", report.Streams, 0); err != nil {
					return report, err
				}
			}
		}
		if !originalCache[ref] && (!useParallel || !isStream[ref]) {
			r.cache.delete(ref)
		}
		if useParallel {
			r.cache.pruneObjectsIfOver(originalCache, 1024)
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
		maxPos, maxGen := uint64(0), uint64(0)
		for _, id := range ids {
			item := entries[id]
			if uint64(item.position) > maxPos {
				maxPos = uint64(item.position)
			}
			if uint64(item.generation) > maxGen {
				maxGen = uint64(item.generation)
			}
		}
		offsetBytes := max(1, minUintBytes(maxPos))
		genBytes := max(1, minUintBytes(maxGen))
		stride := 1 + offsetBytes + genBytes
		index := Array{}
		var data bytes.Buffer
		data.Grow(len(ids) * (stride + 1))
		curr := make([]byte, stride)
		prev := make([]byte, stride)
		row := make([]byte, stride+1)
		row[0] = 2
		for i := 0; i < len(ids); {
			j := i + 1
			for j < len(ids) && ids[j] == ids[j-1]+1 {
				j++
			}
			index = append(index, Integer(ids[i]), Integer(j-i))
			for _, id := range ids[i:j] {
				item := entries[id]
				curr[0] = item.kind
				putBigEndianBytes(curr[1:1+offsetBytes], uint64(item.position))
				putBigEndianBytes(curr[1+offsetBytes:stride], uint64(item.generation))
				for k := range stride {
					row[k+1] = curr[k] - prev[k]
				}
				copy(prev, curr)
				data.Write(row)
			}
			i = j
		}
		trailer["Type"], trailer["W"], trailer["Index"], trailer["Size"] = Name("XRef"), Array{Integer(1), Integer(offsetBytes), Integer(genBytes)}, index, Integer(maxID+1)
		encoded, err := compressPDFBytesLevel(ctx, data.Bytes(), streamCompressLevel)
		if err != nil {
			return report, err
		}
		trailer["Filter"] = Name("FlateDecode")
		trailer["DecodeParms"] = Dictionary{"Predictor": Integer(12), "Columns": Integer(stride)}
		if err := writeObject(Reference{Number: maxID}, &Stream{Dictionary: trailer, Data: encoded}); err != nil {
			return report, err
		}
	} else {
		ids := slices.Sorted(maps.Keys(entries))
		var xrefBuf bytes.Buffer
		xrefBuf.Grow(4096)
		flushXref := func() error {
			if xrefBuf.Len() == 0 {
				return nil
			}
			_, err := out.Write(xrefBuf.Bytes())
			xrefBuf.Reset()
			return err
		}
		if maxID+1 > max(int64(len(entries))*4+1024, 65536) || maxID >= maxXrefEntries {
			if _, err := io.WriteString(out, "xref\n0 1\n0000000000 65535 f \n"); err != nil {
				return report, err
			}
			for i := 1; i < len(ids); {
				j := i + 1
				for j < len(ids) && ids[j] == ids[j-1]+1 {
					j++
				}
				fmt.Fprintf(&xrefBuf, "%d %d\n", ids[i], j-i)
				for k := i; k < j; k++ {
					if k&4095 == 0 {
						if err := ctx.Err(); err != nil {
							return report, err
						}
					}
					item := entries[ids[k]]
					if item.position > 9999999999 {
						return report, fmt.Errorf("PDF cross-reference offset exceeds classic limit")
					}
					fmt.Fprintf(&xrefBuf, "%010d %05d n \n", item.position, item.generation)
					if xrefBuf.Len() >= 4096 {
						if err := flushXref(); err != nil {
							return report, err
						}
					}
				}
				i = j
			}
		} else {
			if _, err := fmt.Fprintf(out, "xref\n0 %d\n", maxID+1); err != nil {
				return report, err
			}
			freeNext := func(after int64, idx int) int64 {
				cand := after + 1
				for idx < len(ids) && ids[idx] < cand {
					idx++
				}
				for idx < len(ids) && ids[idx] == cand {
					cand++
					idx++
				}
				if cand > maxID {
					return 0
				}
				return cand
			}
			idx := 1
			for id := int64(0); id <= maxID; id++ {
				if id&4095 == 0 {
					if err := ctx.Err(); err != nil {
						return report, err
					}
				}
				if id == 0 {
					fmt.Fprintf(&xrefBuf, "%010d 65535 f \n", freeNext(0, 1))
				} else if idx < len(ids) && ids[idx] == id {
					item := entries[id]
					idx++
					if item.position > 9999999999 {
						return report, fmt.Errorf("PDF cross-reference offset exceeds classic limit")
					}
					fmt.Fprintf(&xrefBuf, "%010d %05d n \n", item.position, item.generation)
				} else {
					fmt.Fprintf(&xrefBuf, "%010d 65535 f \n", freeNext(id, idx))
				}
				if xrefBuf.Len() >= 4096 {
					if err := flushXref(); err != nil {
						return report, err
					}
				}
			}
		}
		if err := flushXref(); err != nil {
			return report, err
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

// Write 写入前后检查取消，保留写入及短写错误
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
	if err == nil {
		err = w.ctx.Err()
	}
	return n, err
}

// validPDFName 检查PDF名称对象是否不含标准禁止的空字节
// 入参: name PDF名称对象
// 返回: bool 是否合法
func validPDFName(name Name) bool {
	return strings.IndexByte(string(name), 0) < 0
}

// object 编码PDF对象，字符串使用十六进制保持原始字节
// 入参: value 对象, ref 所属间接对象, depth 嵌套深度, encrypt 是否加密
// 返回: error 编码或写入错误
func (w *pdfOutput) object(value Object, ref Reference, depth int, encrypt bool) error {
	if depth > 256 {
		return fmt.Errorf("excessive PDF object nesting")
	}
	if w.unencrypted[ref] {
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
		if !validPDFName(v) {
			return fmt.Errorf("null byte in PDF name")
		}
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
		if canonical, ok := w.aliases[v]; ok {
			v = canonical
		}
		if w.live != nil && !w.live[v] {
			text = "null"
			break
		}
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
		var streamType Name
		if w.reader != nil {
			var err error
			streamType, err = resolveObjectType(w.reader, dict["Type"], ref)
			if err != nil {
				return err
			}
		} else {
			streamType, _ = dict["Type"].(Name)
		}
		if streamType == Name("XRef") {
			encrypt = false
		}
		if encrypt && w.security != nil {
			name, err := w.security.outputStreamFilter(v, w.reader, streamType, w.embeddedFiles[ref])
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

// workerReader 创建共享底层数据与安全缓存的工作阅读器副本，隔离xref并固定解码并发为1以防竞态与过度订阅
// 入参: warning 并发安全的警告回调
// 返回: *Reader 独立工作阅读器
func (r *Reader) workerReader(warning func(Diagnostic)) *Reader {
	owner := r.owner
	if owner == nil {
		owner = r
	}
	var security *standardSecurity
	if r.security != nil {
		copied := *r.security
		copied.key = bytes.Clone(r.security.key)
		security = &copied
	}
	return &Reader{
		Version:         r.Version,
		Trailer:         r.Trailer,
		source:          r.source,
		size:            r.size,
		originalSource:  r.originalSource,
		originalSize:    r.originalSize,
		xref:            maps.Clone(r.xref),
		cache:           r.cache,
		loading:         make(map[Reference]bool),
		deviceProfile:   r.deviceProfile,
		security:        security,
		warning:         warning,
		decodeWorkers:   1,
		maxStreamBytes:  r.maxStreamBytes,
		maxFilterChain:  r.maxFilterChain,
		jbig2Globals:    r.jbig2Globals,
		owner:           owner,
		ownerHasWarning: r.rootHasWarning(),
	}
}

// executeStreamJob 在独占阅读器上下文中执行单个流或延迟图像的编码与压缩优化
// 入参: ctx 取消上下文, job 待执行任务
func (r *Reader) executeStreamJob(ctx context.Context, job *streamOptJob) {
	if err := ctx.Err(); err != nil {
		job.err = err
		return
	}
	defer func() {
		if job.ownedHardMaskRef.Number == 0 || job.droppedSMask || job.err != nil {
			return
		}
		if err := ctx.Err(); err != nil {
			job.err = err
			return
		}
		hardObj, hardErr := r.Object(job.ownedHardMaskRef)
		if hardErr != nil {
			job.err = hardErr
			return
		}
		if hardStream, ok := hardObj.(*Stream); ok {
			hardComp := job.compression
			hardComp.Mode = CompressionLossless
			optHard, optErr := r.optimizeStreamSize(ctx, hardStream, hardComp, image.Point{})
			if optErr != nil {
				job.err = optErr
				return
			}
			job.outOwnedHardMask = optHard
		} else {
			job.outOwnedHardMask = hardObj
		}
	}()
	var stream *Stream
	var maskStream *Stream
	if job.lazy != nil {
		var err error
		stream, maskStream, err = encodeReplacementImage(ctx, job.lazy)
		if err != nil {
			job.err = err
			return
		}
		if maskStream != nil {
			stream.Dictionary["SMask"] = maskStream
		}
	} else if job.stream != nil {
		stream = job.stream
	} else {
		val, err := r.object(job.ref, job.embedded)
		if err != nil {
			job.err = err
			return
		}
		var ok bool
		stream, ok = val.(*Stream)
		if !ok {
			job.outValue = val
			return
		}
	}
	if job.mode == CompressionUnchanged {
		job.outValue = stream
		job.outLazyMask = maskStream
		return
	}
	var value Object = stream
	var err error
	if job.exactData {
		value, err = r.optimizeDataStream(ctx, stream)
	} else if maskStream != nil && job.compression.Mode == CompressionLossy {
		optMain, optSMask, dropped, optErr := r.optimizeStreamWithOwnedSMask(ctx, stream, maskStream, job.compression, job.size, job.colorResources)
		if optErr != nil {
			job.err = optErr
			return
		}
		value = optMain
		if dropped {
			maskStream = nil
		} else {
			maskStream = optSMask
		}
		job.outValue = value
		job.outLazyMask = maskStream
		return
	} else if job.ownedMaskRef.Number > 0 && job.compression.Mode == CompressionLossy {
		smaskObj := job.ownedMaskObj
		if smaskObj == nil {
			smaskObj, err = r.Object(job.ownedMaskRef)
			if err != nil {
				job.err = err
				return
			}
		}
		if smaskStream, ok := smaskObj.(*Stream); ok {
			optMain, optSMask, dropped, optErr := r.optimizeStreamWithOwnedSMask(ctx, stream, smaskStream, job.compression, job.size, job.colorResources)
			if optErr != nil {
				job.err = optErr
				return
			}
			job.outValue = optMain
			job.droppedSMask = dropped
			if !dropped {
				job.outOwnedMask = optSMask
			}
			return
		}
		job.outValue = stream
		job.outOwnedMask = smaskObj
		return
	} else if job.hasSMaskInfo {
		if job.smaskInfo.opaque {
			hasEmbeddedMask, maskErr := r.hasNonZeroSMaskInData(ctx, stream.Dictionary)
			if maskErr != nil {
				job.err = maskErr
				return
			}
			if hasEmbeddedMask {
				losslessComp := job.compression
				losslessComp.Mode = CompressionLossless
				value, err = r.optimizeStreamSizeWithContext(ctx, stream, losslessComp, image.Point{}, job.colorResources, false)
			} else {
				stripped := *stream
				stripped.Dictionary = maps.Clone(stream.Dictionary)
				delete(stripped.Dictionary, "SMask")
				delete(stripped.Dictionary, "Mask")
				cand, optErr := r.optimizeStreamSizeWithContext(ctx, &stripped, job.compression, job.size, job.colorResources, false)
				if optErr != nil {
					job.err = optErr
					return
				}
				if pdfStreamByteSize(ctx, cand) < pdfStreamByteSize(ctx, stream) {
					value = cand
					job.droppedSMask = true
				} else {
					value, err = r.optimizeStreamSizeWithContext(ctx, stream, job.compression, image.Point{}, job.colorResources, false)
				}
			}
		} else if job.smaskInfo.hasMatte {
			losslessComp := job.compression
			losslessComp.Mode = CompressionLossless
			value, err = r.optimizeStreamSizeWithContext(ctx, stream, losslessComp, image.Point{}, job.colorResources, false)
		} else {
			value, err = r.optimizeStreamSizeWithContext(ctx, stream, job.compression, image.Point{}, job.colorResources, true)
		}
	} else {
		value, err = r.optimizeStreamSizeWithContext(ctx, stream, job.compression, job.size, job.colorResources, false)
	}
	if err != nil {
		job.err = err
		return
	}
	if maskStream != nil {
		maskCompression := job.compression
		maskCompression.Mode = CompressionLossless
		optMask, maskErr := r.optimizeStreamSize(ctx, maskStream, maskCompression, image.Point{})
		if maskErr != nil {
			job.err = maskErr
			return
		}
		maskStream = optMask
	}
	job.outValue = value
	job.outLazyMask = maskStream
}

// optimizeStreamSize 优化流编码并按已知显示尺寸缩小图片
// 入参: ctx 取消上下文, s 原始流, options 压缩配置, size 像素上限
// 返回: *Stream 输出流, error 优化错误
func (r *Reader) optimizeStreamSize(ctx context.Context, s *Stream, options CompressionOptions, size image.Point) (*Stream, error) {
	return r.optimizeStreamSizeWithContext(ctx, s, options, size, nil, false)
}

// optimizeStreamSizeWithContext 结合默认颜色空间与软遮罩策略优化流编码并剪枝冗余的无损像素重压
// 入参: ctx 取消上下文, s 原始流, options 压缩配置, size 像素上限, colorResources 可选默认颜色空间资源, ignoreSMask 是否忽略外部软遮罩
// 返回: *Stream 输出流, error 优化错误
func (r *Reader) optimizeStreamSizeWithContext(ctx context.Context, s *Stream, options CompressionOptions, size image.Point, colorResources Dictionary, ignoreSMask bool) (*Stream, error) {
	encoded, err := r.optimizeStreamEncoding(ctx, s, options, size, colorResources, ignoreSMask)
	if err != nil {
		return nil, err
	}
	if streamHasFilter(encoded.Dictionary, Name("DCTDecode")) {
		return encoded, nil
	}
	if ew, ok1 := encoded.Dictionary["Width"].(Integer); ok1 {
		if sw, ok2 := s.Dictionary["Width"].(Integer); ok2 && ew < sw {
			return encoded, nil
		}
	}
	if eh, ok1 := encoded.Dictionary["Height"].(Integer); ok1 {
		if sh, ok2 := s.Dictionary["Height"].(Integer); ok2 && eh < sh {
			return encoded, nil
		}
	}
	if options.Mode == CompressionLossy && encoded != s && len(encoded.Data)*4 <= len(s.Data)*3 {
		return encoded, nil
	}
	compacted, err := r.optimizeImagePixels(ctx, s)
	if err != nil {
		return nil, err
	}
	if compacted != s && pdfStreamByteSize(ctx, compacted) < pdfStreamByteSize(ctx, encoded) {
		return compacted, nil
	}
	return encoded, nil
}

// optimizeStreamEncoding 优化通用流和有损图片编码，未变小时返回原始流以保留间接引用
// 入参: ctx 取消上下文, s 原始流, options 压缩配置, size 像素需求, colorResources 可选默认颜色空间资源, ignoreSMask 是否忽略外部软遮罩
// 返回: *Stream 输出流, error 编码错误
func (r *Reader) optimizeStreamEncoding(ctx context.Context, s *Stream, options CompressionOptions, size image.Point, colorResources Dictionary, ignoreSMask bool) (*Stream, error) {
	if len(s.Data) > optimizationBufferLimit {
		return s, ctx.Err()
	}
	dict, err := r.optimizationImageDictionary(s)
	if err != nil {
		return s, ctx.Err()
	}
	if dict["F"] != nil {
		return s, nil
	}
	filters, params, err := s.filterChain(r)
	if err != nil {
		return s, nil
	}
	if len(filters) > 1 && (filters[len(filters)-1] == Name("DCTDecode") || filters[len(filters)-1] == Name("JPXDecode")) {
		inner, err := r.unwrapImageStream(ctx, s, filters, params)
		if err != nil {
			return nil, err
		}
		if inner != nil {
			optimized, err := r.optimizeStreamSizeWithContext(ctx, inner, options, size, colorResources, ignoreSMask)
			if err != nil {
				return nil, err
			}
			if pdfStreamByteSize(ctx, optimized) < pdfStreamByteSize(ctx, s) {
				return optimized, nil
			}
		}
	}
	if len(filters) == 1 && filters[0] == Name("JPXDecode") && dict["Subtype"] == Name("Image") {
		return r.optimizeJPXImage(ctx, s, options, size, colorResources, ignoreSMask)
	}
	if len(filters) == 1 && filters[0] == Name("DCTDecode") {
		dctParams, _ := params[0].(Dictionary)
		return r.optimizePDFDCTImage(ctx, s, dict, dctParams, options, size, colorResources, ignoreSMask)
	}
	if options.Mode == CompressionLossy && dict["Subtype"] == Name("Image") {
		cand, err := r.optimizePDFPixelImage(ctx, s, dict, filters, params, options, size, colorResources, ignoreSMask)
		if err != nil {
			return nil, err
		}
		if cand != nil {
			return cand, nil
		}
	}
	return r.optimizeDataStream(ctx, s)
}

// optimizeDataStream 优化通用流编码，支持剥离ASCII包装的Flate流并保留原始未优化流
// 入参: ctx 取消上下文, s 原始流
// 返回: *Stream 输出流, error 取消或编码错误
func (r *Reader) optimizeDataStream(ctx context.Context, s *Stream) (*Stream, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if len(s.Data) > optimizationBufferLimit || s.Dictionary["F"] != nil {
		return s, nil
	}
	filters, params, err := s.filterChain(r)
	if err != nil {
		return s, ctx.Err()
	}
	if len(filters) > 0 && filters[0] == Name("FlateDecode") {
		if s.compressed {
			return s, nil
		}
		encoded, e := recompressPDFBytes(ctx, s.Data)
		if e != nil {
			return nil, e
		}
		if len(encoded) < len(s.Data) {
			result := *s
			result.Dictionary = maps.Clone(s.Dictionary)
			result.Data = encoded
			return &result, nil
		}
		return s, nil
	}
	if len(filters) == 0 {
		data, e := compressPDFBytes(ctx, s.Data)
		if e != nil {
			return nil, e
		}
		if len(data)+40 < len(s.Data) {
			return r.reencodedStream(s, data, Name("FlateDecode"))
		}
		return s, nil
	}
	if len(filters) > 1 && filters[len(filters)-1] == Name("FlateDecode") {
		asciiPrefix := true
		for _, filter := range filters[:len(filters)-1] {
			if filter != Name("ASCII85Decode") && filter != Name("ASCIIHexDecode") {
				asciiPrefix = false
				break
			}
		}
		if asciiPrefix {
			data := s.Data
			decodeOK := true
			for _, filter := range filters[:len(filters)-1] {
				if err := ctx.Err(); err != nil {
					return nil, err
				}
				var decodeErr error
				if filter == Name("ASCII85Decode") {
					_, size, e := ascii85Size(ctx, data, nil)
					if e != nil || size > optimizationBufferLimit {
						decodeOK = false
						break
					}
					data, decodeErr = decodeASCII85Context(ctx, data, nil)
				} else {
					_, size, e := asciiHexSize(ctx, data, nil)
					if e != nil || size > optimizationBufferLimit {
						decodeOK = false
						break
					}
					data, decodeErr = decodeASCIIHexContext(ctx, data, nil)
				}
				if decodeErr != nil || len(data) > optimizationBufferLimit {
					decodeOK = false
					break
				}
			}
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			if decodeOK {
				encoded, e := recompressPDFBytes(ctx, data)
				if e != nil {
					return nil, e
				}
				if len(encoded) < len(data) {
					data = encoded
				}
				cand := *s
				cand.Dictionary = maps.Clone(s.Dictionary)
				cand.Data = data
				cand.Dictionary["Filter"] = Name("FlateDecode")
				lastParam := params[len(params)-1]
				if lastParam != nil {
					cand.Dictionary["DecodeParms"] = lastParam
				} else {
					delete(cand.Dictionary, "DecodeParms")
				}
				if err := r.applyExplicitCryptFilter(s, &cand); err == nil && pdfStreamByteSize(ctx, &cand) < pdfStreamByteSize(ctx, s) {
					return &cand, nil
				}
			}
			return s, ctx.Err()
		}
	}
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
	data, e := r.DecodeStreamContext(ctx, s)
	if e != nil {
		return s, ctx.Err()
	}
	encoded, e := compressPDFBytes(ctx, data)
	if e != nil {
		return nil, e
	}
	if len(encoded)+40 < len(s.Data) {
		return r.reencodedStream(s, encoded, Name("FlateDecode"))
	}
	return s, nil
}

// streamFilterVersion 根据流类型与过滤器链返回所需的最低PDF版本
// 入参: dict 流字典
// 返回: string 最低PDF版本号，无额外要求时为空字符串
func (r *Reader) streamFilterVersion(dict Dictionary) string {
	if dict == nil {
		return ""
	}
	required := ""
	if kind, err := r.Resolve(dict["Type"]); err == nil && (kind == Name("ObjStm") || kind == Name("XRef")) {
		required = max(required, "1.5")
	}
	filterObj, err := r.Resolve(dict["Filter"])
	if err != nil || filterObj == nil {
		return required
	}
	check := func(item Object) {
		resolved, err := r.Resolve(item)
		if err != nil {
			return
		}
		name, ok := resolved.(Name)
		if !ok {
			return
		}
		switch name {
		case "FlateDecode":
			required = max(required, "1.2")
		case "JBIG2Decode":
			required = max(required, "1.4")
		case "JPXDecode":
			required = max(required, "1.5")
		case "BrotliDecode":
			required = max(required, "2.0")
		}
	}
	if array, ok := filterObj.(Array); ok {
		for _, item := range array {
			check(item)
		}
	} else {
		check(filterObj)
	}
	return required
}

// applyExplicitCryptFilter 在V4及以上且原流显式声明Crypt时为目标流前置加密过滤器，保持数组扁平
// 入参: source 原始流, dst 目标流
// 返回: error 加密过滤器解析错误
func (r *Reader) applyExplicitCryptFilter(source, dst *Stream) error {
	if !source.decrypted || r.security == nil {
		return nil
	}
	name, explicit, err := r.security.explicitStreamCryptFilter(source, r)
	if err != nil {
		return err
	}
	if explicit {
		cryptParms := Dictionary{"Name": name}
		switch existing := dst.Dictionary["Filter"].(type) {
		case nil:
			dst.Dictionary["Filter"] = Name("Crypt")
			dst.Dictionary["DecodeParms"] = cryptParms
		case Array:
			filters := make(Array, 0, len(existing)+1)
			filters = append(filters, Name("Crypt"))
			filters = append(filters, existing...)
			dst.Dictionary["Filter"] = filters
			parms := make(Array, len(filters))
			parms[0] = cryptParms
			switch existingParms := dst.Dictionary["DecodeParms"].(type) {
			case Array:
				copy(parms[1:], existingParms)
			case Dictionary:
				if len(existing) == 1 {
					parms[1] = existingParms
				}
			}
			dst.Dictionary["DecodeParms"] = parms
		default:
			dst.Dictionary["Filter"] = Array{Name("Crypt"), existing}
			dst.Dictionary["DecodeParms"] = Array{cryptParms, dst.Dictionary["DecodeParms"]}
		}
	}
	return nil
}

// reencodedStream 替换流编码，仅在V4及以上且原流显式声明Crypt时保留加密过滤器
// 入参: source 原始流, data 输出编码, filter 输出过滤器
// 返回: *Stream 输出流, error 加密过滤器解析错误
func (r *Reader) reencodedStream(source *Stream, data []byte, filter Name) (*Stream, error) {
	result := *source
	result.Dictionary = maps.Clone(source.Dictionary)
	result.Data = data
	result.Dictionary["Filter"] = filter
	delete(result.Dictionary, "DecodeParms")
	if err := r.applyExplicitCryptFilter(source, &result); err != nil {
		return nil, err
	}
	return &result, nil
}

// explicitStreamCryptFilter 提取V4及以上原流显式声明的Crypt过滤器名称
// 入参: stream 原始流, r 阅读器
// 返回: Name 过滤器名称, bool 是否显式声明, error 解析错误
func (s *standardSecurity) explicitStreamCryptFilter(stream *Stream, r *Reader) (Name, bool, error) {
	if s == nil || s.info.Revision < 4 {
		return "", false, nil
	}
	copy := *stream
	copy.decrypted = false
	filters, params, err := copy.filterChain(r)
	if err != nil {
		return "", false, err
	}
	if len(filters) == 0 || filters[0] != Name("Crypt") {
		return "", false, nil
	}
	name := Name("Identity")
	if dict, ok := params[0].(Dictionary); ok && dict["Name"] != nil {
		var valid bool
		name, valid = dict["Name"].(Name)
		if !valid {
			return "", false, fmt.Errorf("invalid stream crypt filter")
		}
	}
	return name, true, nil
}

// outputStreamFilter 获取原流的加密过滤器，保留显式Identity、流类型及内嵌文件策略
// 入参: stream 原始流, r 阅读器, streamType 已解析的流类型, embedded 是否属于内嵌文件流
// 返回: Name 过滤器名称, error 解析错误
func (s *standardSecurity) outputStreamFilter(stream *Stream, r *Reader, streamType Name, embedded bool) (Name, error) {
	if stream.Dictionary["F"] != nil {
		return "Identity", nil
	}
	name := s.streamFilter
	if embedded || streamType == Name("EmbeddedFile") {
		name = s.embeddedFilter
	}
	if streamType == Name("Metadata") && !s.encryptMetadata {
		name = "Identity"
	}
	if explicitName, explicit, err := s.explicitStreamCryptFilter(stream, r); err != nil {
		return "", err
	} else if explicit {
		return explicitName, nil
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

// embeddedFileSpecKeys 定义文件说明及EF字典中的规范文件键顺序
var embeddedFileSpecKeys = [5]Name{"UF", "F", "DOS", "Mac", "Unix"}

// fatalFilespecResolveError 过滤文件说明探测中可容忍的安全与非致命解析错误，保留致命读取与取消错误
// 入参: ctx 取消上下文, err 非空解析错误
// 返回: error 应向上传播的致命读取或取消错误，可容忍时返回空值
func fatalFilespecResolveError(ctx context.Context, err error) error {
	if cErr := ctx.Err(); cErr != nil {
		return cErr
	}
	var secErr *objectSecurityError
	if errors.As(err, &secErr) {
		return nil
	}
	if isFatalOptimizationReadError(ctx, err) {
		return err
	}
	return nil
}

// isFileSpecDictionary 判断宿主字典是否为合法文件说明字典，排除明确非Filespec类型及其他命名空间字典
// 入参: ctx 取消上下文, dict 候选字典, resolve 安全对象解析函数
// 返回: bool 是否为文件说明字典, error 致命读取或取消错误
func isFileSpecDictionary(ctx context.Context, dict Dictionary, resolve func(Object) (Object, error)) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if dict["EF"] == nil {
		return false, nil
	}
	if dict["Type"] != nil {
		typeVal, err := resolve(dict["Type"])
		if err != nil {
			return false, fatalFilespecResolveError(ctx, err)
		}
		if typeVal != nil {
			return typeVal == Name("Filespec"), nil
		}
	}
	for _, key := range embeddedFileSpecKeys {
		if err := ctx.Err(); err != nil {
			return false, err
		}
		if dict[key] == nil {
			continue
		}
		val, err := resolve(dict[key])
		if err != nil {
			if fatalErr := fatalFilespecResolveError(ctx, err); fatalErr != nil {
				return false, fatalErr
			}
			continue
		}
		if _, ok := val.(String); ok {
			return true, nil
		}
	}
	return false, nil
}

// isEmbeddedFileStream 判断候选流是否满足内嵌文件流特征，排除明确非附件的类型与子类型
// 入参: ctx 取消上下文, stream 候选流对象, resolve 安全对象解析函数
// 返回: bool 是否可按内嵌文件流处理, error 致命读取或取消错误
func isEmbeddedFileStream(ctx context.Context, stream *Stream, resolve func(Object) (Object, error)) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if stream.Dictionary["Type"] != nil {
		typeVal, err := resolve(stream.Dictionary["Type"])
		if err != nil {
			return false, fatalFilespecResolveError(ctx, err)
		}
		if typeName, ok := typeVal.(Name); ok && typeName != Name("EmbeddedFile") {
			return false, nil
		}
	}
	if stream.Dictionary["Subtype"] != nil {
		subVal, err := resolve(stream.Dictionary["Subtype"])
		if err != nil {
			return false, fatalFilespecResolveError(ctx, err)
		}
		if subName, ok := subVal.(Name); ok {
			switch subName {
			case Name("Image"), Name("Form"), Name("PS"), Name("Type1C"), Name("CIDFontType0C"), Name("OpenType"), Name("XML"):
				return false, nil
			}
		}
	}
	return true, nil
}
