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
	"compress/zlib"
	"context"
	"fmt"
	"image"
	"io"
	"iter"
	"maps"
	"math"
	"os"
	"slices"
)

// RewriteOptions 批量修改文档信息、图像、文字、链接、大纲、附件及动作，保留原加密配置；图像与页面引用须指向原间接对象，空配置原样写出
// Information按键更新且nil删除，文本须由调用方预先用EncodeTextString编码为String（不同步XMP），Optimization.Creator非空时覆盖Creator与Producer
// OutlineActions与LinkActions替换动作序列（空序列清除）且不可与同项Destination并用；OpenActions为nil保留、空切片清除，页级事件按页替换
// Attachments按名称树原始键增删（nil删除）；MovieAnnotations与ScreenAnnotations按页追加且同指针仅限一次，大纲标题使用UTF-8
// TextReplacements按文字对象序号整段替换，TransformContent随后逐页变换内容（返回nil保留，输入可原地改），回调Page与资源只读且须使用已有资源
type RewriteOptions struct {
	Information         Dictionary
	Images              map[Reference]image.Image
	ImageOptions        ImageWriteOptions
	LinkRegions         map[AnnotationLocation]LinkRegion
	LinkDestinations    map[AnnotationLocation]Destination
	LinkActions         map[AnnotationLocation][]NavigationAction
	OutlineDestinations map[Reference]Destination
	OutlineTitles       map[Reference]string
	OutlineActions      map[Reference][]NavigationAction
	OpenActions         []NavigationAction
	PageOpenActions     map[Reference][]NavigationAction
	PageCloseActions    map[Reference][]NavigationAction
	Attachments         map[string]*EmbeddedFile
	MovieAnnotations    map[Reference][]*MovieAnnotation
	ScreenAnnotations   map[Reference][]*ScreenAnnotation
	TextReplacements    map[Reference][]TextReplacement
	TransformContent    func(context.Context, *Page, []byte) ([]byte, error)
	Optimization        OptimizeOptions
}

// rewriteResources 保存本次写出使用的动作资源
type rewriteResources struct {
	attachments map[string]Object
	sounds      map[*SoundAction]Reference
	movies      map[*MovieAction]Dictionary
	renditions  map[*RenditionAction]Dictionary
}

// rewriteMaxIDCache 缓存在单次RewriteTo周期内复用的原始可达图最大对象编号
type rewriteMaxIDCache struct {
	ready   bool
	maxID   int64
	xrefLen int
}

// AnnotationLocation 通过页面引用和从零开始的注解索引定位页面注解
type AnnotationLocation struct {
	Page  Reference
	Index int
}

// LinkRegion 描述默认用户空间中的链接矩形和点击区域
// Rect须为非空有限矩形；Quads为逆时针凸四边形，全部顶点须位于Rect内
// Quads为空时使用整个Rect，不同四边形的点击区域取并集
type LinkRegion struct {
	Rect  Rectangle
	Quads [][4]Point
}

// rewriteInformation 校验信息字典修改并隔离调用方的数据
// 入参: ctx 取消上下文, changes 待修改字段
// 返回: Dictionary 独立修改集, error 字段或取消错误
func rewriteInformation(ctx context.Context, changes Dictionary) (Dictionary, error) {
	result := make(Dictionary, len(changes))
	for key, value := range changes {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if value == nil {
			result[key] = nil
			continue
		}
		if key == "Trapped" {
			if value != Name("True") && value != Name("False") && value != Name("Unknown") {
				return nil, fmt.Errorf("invalid document trapping state")
			}
			result[key] = value
			continue
		}
		text, ok := value.(String)
		if !ok {
			return nil, fmt.Errorf("document information requires a text string")
		}
		decoded, err := DecodeTextString(text)
		if err != nil {
			return nil, err
		}
		if key == "CreationDate" || key == "ModDate" {
			if _, err := ParseDate(decoded); err != nil {
				return nil, err
			}
		}
		result[key] = String(slices.Clone(text))
	}
	return result, ctx.Err()
}

// RewriteTo 按配置一次写出全部文档修改并保留外观与未知属性，不修改签名文件；出错须丢弃输出，调用期间源数据与替换图像须可读
// 指定目标时替换主动作（已有GoTo保留后续动作，其余整体替换）；修改大纲按实际层级修正父引用且保持顺序与样式
// 独占链接保留原引用，共享链接按页复制，带结构标记的共享链接不可单独拆分
// 入参: ctx 取消上下文, writer 输出流, options 替换配置
// 返回: OptimizeReport 写出结果, error 参数、编码或写入错误
func (r *Reader) RewriteTo(ctx context.Context, writer io.Writer, options RewriteOptions) (OptimizeReport, error) {
	if r == nil || ctx == nil {
		return OptimizeReport{}, fmt.Errorf("invalid PDF rewrite context")
	}
	if r.closed {
		return OptimizeReport{}, os.ErrClosed
	}
	if err := ctx.Err(); err != nil {
		return OptimizeReport{}, err
	}
	if writer == nil {
		return OptimizeReport{}, fmt.Errorf("missing output writer")
	}
	if err := options.Optimization.Compression.Validate(); err != nil {
		return OptimizeReport{}, err
	}
	information, err := rewriteInformation(ctx, options.Information)
	if err != nil {
		return OptimizeReport{}, err
	}
	baseReader := r
	baseReader.rewriteMaxID = &rewriteMaxIDCache{}
	defer func() { baseReader.rewriteMaxID = nil }()
	lazyImages, err := r.prepareImageReplacements(ctx, options.Images, options.ImageOptions)
	if err != nil {
		return OptimizeReport{}, err
	}
	replacements := make(map[Reference]Object)
	attachments, err := r.attachmentReplacements(ctx, options, replacements)
	if err != nil {
		return OptimizeReport{}, err
	}
	sounds, err := r.soundReplacements(ctx, options, replacements)
	if err != nil {
		return OptimizeReport{}, err
	}
	resources := &rewriteResources{attachments: attachments, sounds: sounds}
	movies, err := r.movieAnnotationReplacements(ctx, options.MovieAnnotations, replacements)
	if err != nil {
		return OptimizeReport{}, err
	}
	resources.movies, err = r.movieActions(ctx, options, movies)
	if err != nil {
		return OptimizeReport{}, err
	}
	screens, err := r.screenAnnotationReplacements(ctx, options.ScreenAnnotations, replacements)
	if err != nil {
		return OptimizeReport{}, err
	}
	resources.renditions, err = r.renditionActions(ctx, options, screens, replacements)
	if err != nil {
		return OptimizeReport{}, err
	}
	if err := r.screenAnnotationActions(ctx, options, screens, replacements, resources); err != nil {
		return OptimizeReport{}, err
	}
	destinations, err := r.navigationReplacements(ctx, options, replacements, resources)
	if err != nil {
		return OptimizeReport{}, err
	}
	if err := r.linkReplacements(ctx, options.LinkRegions, destinations, replacements); err != nil {
		return OptimizeReport{}, err
	}
	if err := r.lifecycleReplacements(ctx, options, replacements, resources); err != nil {
		return OptimizeReport{}, err
	}
	compressLevel := zlib.BestCompression
	if options.Optimization.Compression.Mode == CompressionUnchanged {
		compressLevel = zlib.DefaultCompression
	}
	pendingContent, textChanged, err := r.textReplacements(ctx, options.TextReplacements, replacements, options.TransformContent != nil, compressLevel)
	if err != nil {
		return OptimizeReport{}, err
	}
	if options.TransformContent != nil {
		if err := r.transformPageContent(ctx, options.TransformContent, pendingContent, replacements, compressLevel); err != nil {
			return OptimizeReport{}, err
		}
	}
	if textChanged || options.TransformContent != nil || len(replacements) != 0 {
		r = r.rewriteReader(replacements)
	}
	if textChanged {
		r.Version = max(r.Version, "1.4")
	}
	return r.rewriteTo(ctx, writer, options.Optimization, replacements, lazyImages, information)
}

// compareReference 按对象编号和代号升序比较间接引用
// 入参: a 首个引用, b 第二个引用
// 返回: int 比较结果
func compareReference(a, b Reference) int {
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
}

// compareAnnotationLocation 按页面引用与注解索引升序比较注解位置
// 入参: a 首个位置, b 第二个位置
// 返回: int 比较结果
func compareAnnotationLocation(a, b AnnotationLocation) int {
	if cmp := compareReference(a.Page, b.Page); cmp != 0 {
		return cmp
	}
	if a.Index < b.Index {
		return -1
	}
	if a.Index > b.Index {
		return 1
	}
	return 0
}

// isLiveXrefRef 判断间接引用是否对应交叉引用表中的有效对象
// 入参: ref 间接引用
// 返回: bool 是否为交叉引用表中的存活对象
func (r *Reader) isLiveXrefRef(ref Reference) bool {
	entry, ok := r.xref[ref.Number]
	if !ok || entry.kind == 0 {
		return false
	}
	if entry.kind == 2 {
		return ref.Generation == 0
	}
	return entry.generation == ref.Generation
}

// baseRewriteObjectID 统计交叉引用表与原始可达对象图悬空引用中的最大对象编号
// 返回: int64 原始文档最大对象编号
func (r *Reader) baseRewriteObjectID() int64 {
	maximum := int64(0)
	for id := range r.xref {
		maximum = max(maximum, id)
	}
	if r.Trailer == nil {
		return maximum
	}
	originalCache := r.cache.snapshot()
	defer r.cache.prune(originalCache)
	originalObjectStreams := r.objectStreams
	defer func() { r.objectStreams = originalObjectStreams }()
	seen := make(map[Reference]bool)
	var queue []Reference
	var dangling []int64
	var scan func(Object, int)
	scan = func(value Object, depth int) {
		if depth > 256 {
			return
		}
		switch v := value.(type) {
		case Reference:
			if v.Number <= 0 || v.Generation < 0 || v.Generation > 65535 {
				return
			}
			if !r.isLiveXrefRef(v) {
				dangling = append(dangling, v.Number)
				return
			}
			if !seen[v] {
				seen[v] = true
				queue = append(queue, v)
			}
		case Array:
			for _, item := range v {
				scan(item, depth+1)
			}
		case Dictionary:
			for _, item := range v {
				scan(item, depth+1)
			}
		case *Stream:
			scan(v.Dictionary, depth+1)
		}
	}
	scan(r.Trailer, 0)
	for i := 0; i < len(queue); i++ {
		ref := queue[i]
		value, err := r.Object(ref)
		if !originalCache[ref] {
			r.cache.delete(ref)
		}
		if err != nil {
			continue
		}
		scan(value, 0)
	}
	slices.Sort(dangling)
	for _, id := range dangling {
		if id > maximum && id <= maximum+1<<20 && id < math.MaxInt32-1024 {
			maximum = id
		}
	}
	return maximum
}

// maxRewriteObjectID 统计交叉引用表、替换对象表与原始可达对象图悬空引用中的最大对象编号，并复用单次写出周期的基准扫描结果
// 入参: replacements 已有替换对象
// 返回: int64 最大对象编号
func (r *Reader) maxRewriteObjectID(replacements map[Reference]Object) int64 {
	var maximum int64
	if cache := r.rewriteMaxID; cache != nil {
		if !cache.ready || cache.xrefLen != len(r.xref) {
			cache.maxID = r.baseRewriteObjectID()
			cache.xrefLen = len(r.xref)
			cache.ready = true
		}
		maximum = cache.maxID
	} else {
		maximum = r.baseRewriteObjectID()
	}
	for ref := range replacements {
		maximum = max(maximum, ref.Number)
	}
	return maximum
}

// actionSequences 按确定顺序遍历待写动作序列，不改变各序列内部顺序
// 返回: iter.Seq[[]NavigationAction] 动作序列迭代器
func (o RewriteOptions) actionSequences() iter.Seq[[]NavigationAction] {
	return func(yield func([]NavigationAction) bool) {
		if !yield(o.OpenActions) {
			return
		}
		for _, group := range []map[Reference][]NavigationAction{o.OutlineActions, o.PageOpenActions, o.PageCloseActions} {
			refs := slices.Collect(maps.Keys(group))
			slices.SortFunc(refs, compareReference)
			for _, ref := range refs {
				if !yield(group[ref]) {
					return
				}
			}
		}
		locations := slices.Collect(maps.Keys(o.LinkActions))
		slices.SortFunc(locations, compareAnnotationLocation)
		for _, location := range locations {
			if !yield(o.LinkActions[location]) {
				return
			}
		}
		screenPages := slices.Collect(maps.Keys(o.ScreenAnnotations))
		slices.SortFunc(screenPages, compareReference)
		for _, pageRef := range screenPages {
			for _, annotation := range o.ScreenAnnotations[pageRef] {
				if annotation != nil && !yield(annotation.Actions) {
					return
				}
			}
		}
	}
}

// rewriteReader 建立写出期间使用的替换后只读视图，不接管源文件与解密密钥，供压缩与去重读取一致对象
// 入参: replacements 替换对象
// 返回: *Reader 独立对象缓存的读取视图
func (r *Reader) rewriteReader(replacements map[Reference]Object) *Reader {
	owner := r.owner
	if owner == nil {
		owner = r
	}
	return &Reader{
		Version:         r.Version,
		Trailer:         r.Trailer,
		source:          r.source,
		size:            r.size,
		originalSource:  r.originalSource,
		originalSize:    r.originalSize,
		xref:            maps.Clone(r.xref),
		cache:           r.cache.cloneWithPinned(replacements),
		loading:         make(map[Reference]bool),
		deviceProfile:   r.deviceProfile,
		security:        r.security,
		warning:         r.warning,
		decodeWorkers:   r.decodeWorkers,
		maxStreamBytes:  r.maxStreamBytes,
		maxFilterChain:  r.maxFilterChain,
		jbig2Globals:    r.jbig2Globals,
		owner:           owner,
		ownerHasWarning: r.rootHasWarning(),
	}
}

// linkReplacements 按页面复制链接及注解数组，不影响其他页面共享的注解
// 入参: ctx 取消上下文, regions 注解位置与目标区域, destinations 链接导航修改, result 已有替换对象
// 返回: error 引用或区域错误
func (r *Reader) linkReplacements(ctx context.Context, regions map[AnnotationLocation]LinkRegion, destinations map[AnnotationLocation]linkNavigation, result map[Reference]Object) error {
	if len(regions) == 0 && len(destinations) == 0 {
		return nil
	}
	selected := make(map[AnnotationLocation]struct{}, len(regions)+len(destinations))
	for location := range regions {
		selected[location] = struct{}{}
	}
	for location := range destinations {
		selected[location] = struct{}{}
	}
	uses, err := r.linkReferenceUses(ctx, selected)
	if err != nil {
		return err
	}
	maximum := r.maxRewriteObjectID(result)
	if maximum > math.MaxInt32-int64(len(selected))-1024 {
		return fmt.Errorf("PDF object number exceeds output limit")
	}
	locations := slices.Collect(maps.Keys(selected))
	slices.SortFunc(locations, compareAnnotationLocation)
	for _, location := range locations {
		if err := ctx.Err(); err != nil {
			return err
		}
		ref := location.Page
		if ref.Number <= 0 || ref.Generation < 0 || ref.Generation > 65535 {
			return fmt.Errorf("invalid PDF page reference")
		}
		value, err := r.Object(ref)
		if err != nil {
			return err
		}
		page, ok := value.(Dictionary)
		if !ok {
			return fmt.Errorf("invalid PDF page dictionary")
		}
		kind, err := r.Resolve(page["Type"])
		if err != nil {
			return err
		}
		if kind != Name("Page") {
			return fmt.Errorf("replacement target is not a PDF page")
		}
		updated, ok := result[ref].(Dictionary)
		if !ok {
			updated = maps.Clone(page)
			value, err := r.Resolve(updated["Annots"])
			if err != nil {
				return err
			}
			array, ok := value.(Array)
			if !ok {
				return fmt.Errorf("invalid PDF page annotations")
			}
			updated["Annots"] = slices.Clone(array)
			result[ref] = updated
		}
		page = updated
		annotations, ok := page["Annots"].(Array)
		if !ok || location.Index < 0 || location.Index >= len(annotations) {
			return fmt.Errorf("PDF annotation index out of range")
		}
		annotationRef, indirect := annotations[location.Index].(Reference)
		value, err = r.Resolve(annotations[location.Index])
		if err != nil {
			return err
		}
		dict, ok := value.(Dictionary)
		if !ok {
			return fmt.Errorf("invalid PDF link dictionary")
		}
		subtype, err := r.Resolve(dict["Subtype"])
		if err != nil {
			return err
		}
		if subtype != Name("Link") {
			return fmt.Errorf("replacement object is not a PDF link")
		}
		updated = maps.Clone(dict)
		updated["Subtype"], updated["P"] = Name("Link"), ref
		if region, ok := regions[location]; ok {
			points, err := region.quadPoints(ctx)
			if err != nil {
				return err
			}
			updated["Rect"] = Array{Real(region.Rect.XMin), Real(region.Rect.YMin), Real(region.Rect.XMax), Real(region.Rect.YMax)}
			delete(updated, "QuadPoints")
			if len(points) != 0 {
				updated["QuadPoints"] = points
			}
		}
		if destination, ok := destinations[location]; ok {
			if destination.replaceActions {
				delete(updated, "Dest")
				delete(updated, "A")
				if destination.action != nil {
					updated["A"] = destination.action
				}
			} else {
				if err := r.replaceDestination(updated, destination.destination); err != nil {
					return err
				}
			}
		}
		if !indirect || uses[annotationRef] != 1 {
			if dict["StructParent"] != nil {
				return fmt.Errorf("cannot split a shared or direct tagged PDF link")
			}
			maximum++
			annotationRef = Reference{Number: maximum}
		}
		result[annotationRef] = updated
		annotations[location.Index] = annotationRef
	}
	return nil
}

// linkReferenceUses 统计待修改链接的页面引用次数，独占链接保持原结构树引用
// 入参: ctx 取消上下文, locations 待修改的注解位置
// 返回: map[Reference]int 引用次数, error 页面或引用错误
func (r *Reader) linkReferenceUses(ctx context.Context, locations map[AnnotationLocation]struct{}) (map[Reference]int, error) {
	uses := make(map[Reference]int)
	for location := range locations {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		value, err := r.Object(location.Page)
		if err != nil {
			return nil, err
		}
		page, ok := value.(Dictionary)
		if !ok {
			return nil, fmt.Errorf("invalid PDF page dictionary")
		}
		value, err = r.Resolve(page["Annots"])
		if err != nil {
			return nil, err
		}
		array, ok := value.(Array)
		if !ok || location.Index < 0 || location.Index >= len(array) {
			return nil, fmt.Errorf("PDF annotation index out of range")
		}
		if ref, ok := array[location.Index].(Reference); ok {
			uses[ref] = 0
		}
	}
	if len(uses) == 0 {
		return uses, nil
	}
	err := r.WalkPages(ctx, func(_ int, page *Page) error {
		value, err := r.Resolve(page.Dictionary["Annots"])
		if err != nil {
			return err
		}
		array, _ := value.(Array)
		for _, value := range array {
			if ref, ok := value.(Reference); ok {
				if _, wanted := uses[ref]; wanted {
					uses[ref]++
				}
			}
		}
		return nil
	})
	return uses, err
}

// quadPoints 校验链接矩形与凸四边形，生成PDF1.6链接坐标数组
// 入参: ctx 取消上下文
// 返回: Array 顶点坐标, error 非有限值、越界或方向错误
func (r LinkRegion) quadPoints(ctx context.Context) (Array, error) {
	box := r.Rect
	for _, value := range [...]float64{box.XMin, box.YMin, box.XMax, box.YMax} {
		if math.IsNaN(value) || math.IsInf(value, 0) {
			return nil, fmt.Errorf("invalid PDF link rectangle")
		}
	}
	if box.XMin >= box.XMax || box.YMin >= box.YMax {
		return nil, fmt.Errorf("empty PDF link rectangle")
	}
	var result Array
	for _, quad := range r.Quads {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		for i, point := range quad {
			if math.IsNaN(point.X) || math.IsNaN(point.Y) || point.X < box.XMin || point.X > box.XMax || point.Y < box.YMin || point.Y > box.YMax {
				return nil, fmt.Errorf("PDF link vertex lies outside its rectangle")
			}
			a, b := quad[(i+1)%4], quad[(i+2)%4]
			cross := (a.X-point.X)*(b.Y-a.Y) - (a.Y-point.Y)*(b.X-a.X)
			if cross <= 0 || math.IsNaN(cross) || math.IsInf(cross, 0) {
				return nil, fmt.Errorf("PDF link quadrilateral must be convex and counterclockwise")
			}
			result = append(result, Real(point.X), Real(point.Y))
		}
	}
	return result, nil
}
