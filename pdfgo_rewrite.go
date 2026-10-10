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

// RewriteOptions 批量修改图像、文字、链接、附件和动作，保留原加密配置
// 图像及页面引用须指向原文件的间接对象；空配置原样写出，不修改源对象
// 大纲通过原节点引用定位，标题使用UTF-8文字
// OutlineActions替换整个动作序列，空序列移除动作及目标，不可与同节点的OutlineDestinations同时指定
// LinkActions按相同规则替换链接动作，不可与同位置的LinkDestinations同时指定
// OpenActions为nil时保留文档打开行为，非nil空列表清除，非空列表替换为指定动作序列
// PageOpenActions和PageCloseActions按页面引用替换相应事件，空列表清除，不影响其他事件
// Attachments按名称树原始键添加或替换附件，nil值删除该键，其他位置的文件引用不变
// MovieAnnotations按页面追加视频注解，同一注解指针只能添加一次，可由MovieAction.Target引用
// ScreenAnnotations按页面追加屏幕注解，可由RenditionAction.Target引用
// TextReplacements按页面内容流中的文字对象序号写入整段替换文本，不改动字形及定位
// Optimization在同次写出中处理压缩及制作软件，默认不优化
type RewriteOptions struct {
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
	Optimization        OptimizeOptions
}

// rewriteResources 保存本次写出使用的动作资源
type rewriteResources struct {
	attachments map[string]Object
	sounds      map[*SoundAction]Reference
	movies      map[*MovieAction]Dictionary
	renditions  map[*RenditionAction]Dictionary
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

// RewriteTo 一次写出图像、文字、链接、附件及事件修改，保留外观和未知属性
// 指定目标时替换原主动作；已有GoTo动作保留后续动作，其他动作整体替换
// 独占链接保留原引用；共享链接按页面复制，带结构标记的共享链接不能单独拆分
// 修改大纲时按实际层级修正父节点引用，不改变节点顺序及显示样式
// 不修改签名文件；出错时应丢弃本次输出，调用期间源数据和替换图像须保持可读
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
	replacements, err := r.imageReplacements(ctx, options.Images, options.ImageOptions)
	if err != nil {
		return OptimizeReport{}, err
	}
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
	textChanged, err := r.textReplacements(ctx, options.TextReplacements, replacements)
	if err != nil {
		return OptimizeReport{}, err
	}
	if textChanged || len(replacements) != 0 && options.Optimization.Compression.Mode != CompressionUnchanged {
		r = r.rewriteReader(replacements)
	}
	if textChanged {
		r.Version = max(r.Version, "1.4")
	}
	return r.rewriteTo(ctx, writer, options.Optimization, replacements)
}

// actionSequences 遍历待写动作序列，不改变各序列内部顺序
// 返回: iter.Seq[[]NavigationAction] 动作序列迭代器
func (o RewriteOptions) actionSequences() iter.Seq[[]NavigationAction] {
	return func(yield func([]NavigationAction) bool) {
		if !yield(o.OpenActions) {
			return
		}
		for _, group := range []map[Reference][]NavigationAction{o.OutlineActions, o.PageOpenActions, o.PageCloseActions} {
			for _, actions := range group {
				if !yield(actions) {
					return
				}
			}
		}
		for _, actions := range o.LinkActions {
			if !yield(actions) {
				return
			}
		}
		for _, annotations := range o.ScreenAnnotations {
			for _, annotation := range annotations {
				if annotation != nil && !yield(annotation.Actions) {
					return
				}
			}
		}
	}
}

// rewriteReader 建立替换后的独立读取视图，供压缩与资源去重读取一致的对象
// 视图不接管源文件和加密密钥，仅在本次写出期间使用
// 入参: replacements 替换对象
// 返回: *Reader 独立对象缓存的读取视图
func (r *Reader) rewriteReader(replacements map[Reference]Object) *Reader {
	cache := maps.Clone(r.cache)
	maps.Copy(cache, replacements)
	return &Reader{
		Version:        r.Version,
		Trailer:        r.Trailer,
		source:         r.source,
		size:           r.size,
		originalSource: r.originalSource,
		originalSize:   r.originalSize,
		xref:           r.xref,
		cache:          cache,
		loading:        make(map[Reference]bool),
		security:       r.security,
		warning:        r.warning,
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
	maximum := int64(0)
	for number := range r.xref {
		maximum = max(maximum, number)
	}
	for ref := range result {
		maximum = max(maximum, ref.Number)
	}
	if maximum > math.MaxInt32-int64(len(selected))-1024 {
		return fmt.Errorf("PDF object number exceeds output limit")
	}
	locations := slices.Collect(maps.Keys(selected))
	slices.SortFunc(locations, func(a, b AnnotationLocation) int {
		if a.Page.Number != b.Page.Number {
			if a.Page.Number < b.Page.Number {
				return -1
			}
			return 1
		}
		if a.Page.Generation != b.Page.Generation {
			if a.Page.Generation < b.Page.Generation {
				return -1
			}
			return 1
		}
		if a.Index < b.Index {
			return -1
		}
		if a.Index > b.Index {
			return 1
		}
		return 0
	})
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
		if updated, ok := result[ref].(Dictionary); ok {
			page = updated
		} else if result[ref] != nil {
			return fmt.Errorf("conflicting PDF replacement reference")
		} else {
			page = maps.Clone(page)
			value, err := r.Resolve(page["Annots"])
			if err != nil {
				return err
			}
			array, ok := value.(Array)
			if !ok {
				return fmt.Errorf("invalid PDF page annotations")
			}
			page["Annots"] = slices.Clone(array)
			result[ref] = page
		}
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
		updated := maps.Clone(dict)
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
