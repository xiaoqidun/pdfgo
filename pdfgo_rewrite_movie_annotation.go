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
	"math"
	"slices"
)

// MovieAnnotation 描述新增视频注解，Rect使用默认用户空间，Title使用UTF-8文字
// Movie指针相同的注解复用视频资源，源数据在写出期间须保持可读
// Activation接受nil、Boolean或Dictionary；nil采用标准默认播放，false关闭注解自身的点击播放
// 播放参数中的引用须属于当前文件；海报接受Boolean或图像流，流引用随所属Reader解析
// 文件说明按结构化字段写出，Dictionary不参与写出，不自动添加时间或作者信息
type MovieAnnotation struct {
	Rect       Rectangle
	Title      string
	Movie      *Movie
	Activation Object
}

// movieAnnotationReplacements 按页追加视频注解，复用视频数据并隔离原注解数组
// 入参: ctx 取消上下文, additions 页面及新增注解, result 已有替换对象
// 返回: map[*MovieAnnotation]Reference 新增注解引用, error 页面、资源或取消错误
func (r *Reader) movieAnnotationReplacements(ctx context.Context, additions map[Reference][]*MovieAnnotation, result map[Reference]Object) (map[*MovieAnnotation]Reference, error) {
	if len(additions) == 0 {
		return nil, nil
	}
	number := int64(0)
	for id := range r.xref {
		number = max(number, id)
	}
	for ref := range result {
		number = max(number, ref.Number)
	}
	refs := make(map[*MovieAnnotation]Reference)
	media := make(map[*Movie]Reference)
	visited := make(map[Reference]bool)
	err := r.WalkPages(ctx, func(_ int, source *Page) error {
		annotations, exists := additions[source.Reference]
		if !exists {
			return nil
		}
		visited[source.Reference] = true
		if len(annotations) == 0 {
			return nil
		}
		page, err := r.rewriteDictionary(source.Reference, result)
		if err != nil {
			return err
		}
		value, err := r.Resolve(page["Annots"])
		if err != nil {
			return err
		}
		var array Array
		if value != nil {
			var ok bool
			array, ok = value.(Array)
			if !ok {
				return fmt.Errorf("invalid PDF page annotations")
			}
			array = slices.Clone(array)
		}
		for _, annotation := range annotations {
			if err := ctx.Err(); err != nil {
				return err
			}
			if annotation == nil || annotation.Movie == nil || refs[annotation] != (Reference{}) {
				return fmt.Errorf("missing or repeated movie annotation")
			}
			box := annotation.Rect
			for _, value := range []float64{box.XMin, box.YMin, box.XMax, box.YMax} {
				if math.IsNaN(value) || math.IsInf(value, 0) {
					return fmt.Errorf("invalid movie annotation rectangle")
				}
			}
			if box.XMin > box.XMax || box.YMin > box.YMax {
				return fmt.Errorf("inverted movie annotation rectangle")
			}
			movie, exists := media[annotation.Movie]
			if !exists {
				movie, err = r.rewriteMovie(ctx, *annotation.Movie, result, &number)
				if err != nil {
					return err
				}
				media[annotation.Movie] = movie
			}
			title, err := EncodeTextString(annotation.Title)
			if err != nil {
				return err
			}
			dict := Dictionary{"Type": Name("Annot"), "Subtype": Name("Movie"), "P": source.Reference, "Rect": Array{Real(box.XMin), Real(box.YMin), Real(box.XMax), Real(box.YMax)}, "Movie": movie, "T": title}
			switch activation := annotation.Activation.(type) {
			case nil:
			case Boolean:
				dict["A"] = activation
			case Dictionary:
				dict["A"], err = r.rewriteMovieActivation(ctx, activation)
				if err != nil {
					return err
				}
			default:
				return fmt.Errorf("invalid movie annotation activation")
			}
			ref, err := appendRewriteObject(result, &number, dict)
			if err != nil {
				return err
			}
			refs[annotation] = ref
			array = append(array, ref)
		}
		page["Annots"] = array
		result[source.Reference] = page
		return nil
	})
	if err != nil {
		return nil, err
	}
	if len(visited) != len(additions) {
		return nil, fmt.Errorf("movie annotation page is not in the page tree")
	}
	return refs, ctx.Err()
}

// rewriteMovie 写出视频文件、显示参数及海报，海报保留编码和关联资源
// 入参: ctx 取消上下文, movie 视频信息, result 输出对象, number 最大对象编号
// 返回: Reference 视频字典引用, error 参数、资源或取消错误
func (r *Reader) rewriteMovie(ctx context.Context, movie Movie, result map[Reference]Object, number *int64) (Reference, error) {
	dict := make(Dictionary)
	if movie.Width != 0 || movie.Height != 0 {
		if movie.Width <= 0 || movie.Height <= 0 || math.IsNaN(movie.Width) || math.IsNaN(movie.Height) || math.IsInf(movie.Width, 0) || math.IsInf(movie.Height, 0) {
			return Reference{}, fmt.Errorf("invalid movie dimensions")
		}
		dict["Aspect"] = Array{Real(movie.Width), Real(movie.Height)}
	}
	if movie.Rotate%90 != 0 {
		return Reference{}, fmt.Errorf("invalid movie rotation")
	}
	if movie.Rotate != 0 {
		dict["Rotate"] = Integer((movie.Rotate%360 + 360) % 360)
	}
	switch poster := movie.Poster.(type) {
	case nil:
	case Boolean:
		dict["Poster"] = poster
	case *Stream:
		if poster == nil {
			return Reference{}, fmt.Errorf("missing movie poster")
		}
		source := poster.reader
		if source == nil {
			source = r
		}
		if _, err := source.ReadImage(poster); err != nil {
			return Reference{}, err
		}
		copier := rewriteObjectCopy{ctx: ctx, source: source, objects: result, number: number, copied: make(map[rewriteObjectIdentity]Reference)}
		value, err := copier.copy(poster, 0)
		if err != nil {
			return Reference{}, err
		}
		dict["Poster"] = value
	default:
		return Reference{}, fmt.Errorf("invalid movie poster")
	}
	file, err := rewriteFile(ctx, movie.File, result, number)
	if err != nil {
		return Reference{}, err
	}
	dict["F"] = file
	return appendRewriteObject(result, number, dict)
}
