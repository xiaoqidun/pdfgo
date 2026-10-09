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
	"maps"
	"math"
	"os"
	"slices"
)

// RewriteOptions 批量替换图像和链接区域，不改变其他对象或原加密配置
// 图像及页面引用须指向原文件的间接对象；空配置原样写出，不修改源对象
type RewriteOptions struct {
	Images       map[Reference]image.Image
	ImageOptions ImageWriteOptions
	LinkRegions  map[AnnotationLocation]LinkRegion
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

// RewriteTo 一次写出图像及链接区域替换，保留链接动作、外观和未知属性
// 独占链接保留原引用；共享链接按页面复制，带结构标记的共享链接不能单独拆分
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
	replacements, err := r.imageReplacements(ctx, options.Images, options.ImageOptions)
	if err != nil {
		return OptimizeReport{}, err
	}
	if err := r.linkReplacements(ctx, options.LinkRegions, replacements); err != nil {
		return OptimizeReport{}, err
	}
	return r.rewriteTo(ctx, writer, OptimizeOptions{}, replacements)
}

// linkReplacements 按页面复制链接及注解数组，不影响其他页面共享的注解
// 入参: ctx 取消上下文, regions 注解位置与目标区域, result 已有替换对象
// 返回: error 引用或区域错误
func (r *Reader) linkReplacements(ctx context.Context, regions map[AnnotationLocation]LinkRegion, result map[Reference]Object) error {
	if len(regions) == 0 {
		return nil
	}
	uses, err := r.linkReferenceUses(ctx, regions)
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
	if maximum > math.MaxInt32-int64(len(regions))-1024 {
		return fmt.Errorf("PDF object number exceeds output limit")
	}
	locations := slices.Collect(maps.Keys(regions))
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
		ref, region := location.Page, regions[location]
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
		points, err := region.quadPoints(ctx)
		if err != nil {
			return err
		}
		updated := maps.Clone(dict)
		updated["Subtype"], updated["P"] = Name("Link"), ref
		updated["Rect"] = Array{Real(region.Rect.XMin), Real(region.Rect.YMin), Real(region.Rect.XMax), Real(region.Rect.YMax)}
		delete(updated, "QuadPoints")
		if len(points) != 0 {
			updated["QuadPoints"] = points
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
// 入参: ctx 取消上下文, regions 待修改的注解位置
// 返回: map[Reference]int 引用次数, error 页面或引用错误
func (r *Reader) linkReferenceUses(ctx context.Context, regions map[AnnotationLocation]LinkRegion) (map[Reference]int, error) {
	uses := make(map[Reference]int)
	for location := range regions {
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
