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
	"maps"
)

// MoviePlayback 保存动作目标、视频资源及合并后的播放参数，不读取文件或执行播放
type MoviePlayback struct {
	Annotation Annotation
	Movie      Movie
	Operation  Name
	Activation MovieActivation
}

// ReadMoviePlayback 在指定页面内解析视频动作目标，以显式动作参数覆盖注解播放参数
// page应为执行动作时的页面；库不执行前序跳转，也不在其他页面猜测同名目标
// 入参: ctx 取消上下文, page 当前页面, object 视频动作字典或引用
// 返回: MoviePlayback 视频播放信息, error 目标、参数、引用或取消错误
func (r *Reader) ReadMoviePlayback(ctx context.Context, page *Page, object Object) (MoviePlayback, error) {
	var result MoviePlayback
	if r == nil || ctx == nil || page == nil || page.reader != r {
		return result, fmt.Errorf("invalid movie playback context")
	}
	action, err := r.ReadMovieAction(ctx, object)
	if err != nil {
		return result, err
	}
	annotation, err := r.movieTarget(ctx, page, action)
	if err != nil {
		return result, err
	}
	value, err := r.Resolve(annotation.Dictionary["A"])
	if err != nil {
		return result, err
	}
	parameters := make(Dictionary)
	switch value := value.(type) {
	case nil, Boolean:
	case Dictionary:
		maps.Copy(parameters, value)
	default:
		return result, fmt.Errorf("invalid movie annotation activation")
	}
	maps.Copy(parameters, action.Activation)
	result.Activation, err = r.ReadMovieActivation(ctx, parameters)
	if err != nil {
		return result, err
	}
	result.Movie, err = r.ReadMovie(annotation.Dictionary["Movie"])
	if err != nil {
		return result, err
	}
	result.Annotation, result.Operation = annotation, action.Operation
	return result, ctx.Err()
}

// movieTarget 按引用或标题查找当前页视频注解，不解析无关注解的外观和内容
// 入参: ctx 取消上下文, page 当前页面, action 已解析的视频动作
// 返回: Annotation 唯一目标, error 页面注解、目标歧义或取消错误
func (r *Reader) movieTarget(ctx context.Context, page *Page, action MovieAction) (Annotation, error) {
	var result Annotation
	value, err := r.Resolve(page.Dictionary["Annots"])
	if err != nil {
		return result, err
	}
	array, ok := value.(Array)
	if value != nil && !ok {
		return result, fmt.Errorf("invalid page annotations")
	}
	found := false
	for _, object := range array {
		if err := ctx.Err(); err != nil {
			return Annotation{}, err
		}
		ref, indirect := object.(Reference)
		if action.Annotation != (Reference{}) && (!indirect || ref != action.Annotation) {
			continue
		}
		value, err := r.Resolve(object)
		if err != nil {
			return Annotation{}, err
		}
		dict, ok := value.(Dictionary)
		if !ok {
			continue
		}
		kind, err := r.Resolve(dict["Subtype"])
		if err != nil {
			return Annotation{}, err
		}
		if kind != Name("Movie") {
			continue
		}
		if action.Annotation == (Reference{}) {
			value, err := r.Resolve(dict["T"])
			if err != nil {
				return Annotation{}, err
			}
			text, ok := value.(String)
			if !ok {
				continue
			}
			title, err := DecodeTextString(text)
			if err != nil {
				return Annotation{}, err
			}
			if title != action.Title {
				continue
			}
		}
		if found {
			if ref != (Reference{}) && ref == result.Reference {
				continue
			}
			return Annotation{}, fmt.Errorf("%w: ambiguous movie target", ErrInvalidAction)
		}
		box, err := r.rectangle(dict["Rect"])
		if err != nil {
			return Annotation{}, err
		}
		result = Annotation{Reference: ref, Subtype: "Movie", Rect: box, Dictionary: dict}
		found = true
	}
	if !found {
		return Annotation{}, fmt.Errorf("%w: movie target is not on the current page", ErrInvalidAction)
	}
	return result, ctx.Err()
}
