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
	"encoding/binary"
	"fmt"
	"math"
)

// movieActions 校验视频目标并准备独立的播放动作，不读取或播放媒体文件
// 入参: ctx 取消上下文, options 替换配置, added 新增视频注解引用
// 返回: map[*MovieAction]Dictionary 视频动作, error 目标、参数或取消错误
func (r *Reader) movieActions(ctx context.Context, options RewriteOptions, added map[*MovieAnnotation]Reference) (map[*MovieAction]Dictionary, error) {
	var result map[*MovieAction]Dictionary
	for actions := range options.actionSequences() {
		for _, action := range actions {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			if action.Movie == nil {
				continue
			}
			if result == nil {
				result = make(map[*MovieAction]Dictionary)
			}
			result[action.Movie] = nil
		}
	}
	if len(result) == 0 {
		return nil, ctx.Err()
	}
	refs := make(map[Reference]bool)
	titles := make(map[string]bool)
	for action := range result {
		if action.Target != nil {
			if action.Annotation != (Reference{}) || action.Title != "" || added[action.Target] == (Reference{}) {
				return nil, fmt.Errorf("invalid new movie annotation target")
			}
		} else if action.Annotation != (Reference{}) {
			if action.Annotation.Number <= 0 || action.Annotation.Generation < 0 || action.Annotation.Generation > 65535 || action.Title != "" {
				return nil, fmt.Errorf("invalid movie annotation reference")
			}
			refs[action.Annotation] = false
		} else {
			titles[action.Title] = false
		}
	}
	var err error
	if len(refs) != 0 || len(titles) != 0 {
		err = r.WalkPages(ctx, func(_ int, page *Page) error {
			value, err := r.Resolve(page.Dictionary["Annots"])
			if err != nil || value == nil {
				return err
			}
			annotations, ok := value.(Array)
			if !ok {
				return fmt.Errorf("invalid PDF page annotations")
			}
			for _, object := range annotations {
				if err := ctx.Err(); err != nil {
					return err
				}
				ref, indirect := object.(Reference)
				_, selected := refs[ref]
				if !selected && len(titles) == 0 {
					continue
				}
				value, err := r.Resolve(object)
				if err != nil {
					return err
				}
				dict, ok := value.(Dictionary)
				if !ok {
					continue
				}
				kind, err := r.Resolve(dict["Subtype"])
				if err != nil {
					return err
				}
				if kind != Name("Movie") {
					continue
				}
				if selected && indirect {
					refs[ref] = true
				}
				if len(titles) == 0 {
					continue
				}
				value, err = r.Resolve(dict["T"])
				if err != nil {
					return err
				}
				if text, ok := value.(String); ok {
					title, err := DecodeTextString(text)
					if err != nil {
						return err
					}
					if _, selected := titles[title]; selected {
						titles[title] = true
					}
				}
			}
			return nil
		})
	}
	if err != nil {
		return nil, err
	}
	for action := range result {
		if action.Target == nil && (action.Annotation != (Reference{}) && !refs[action.Annotation] || action.Annotation == (Reference{}) && !titles[action.Title]) {
			return nil, fmt.Errorf("movie target is not a page annotation")
		}
		dict, err := r.rewriteMovieActivation(ctx, action.Activation)
		if err != nil {
			return nil, err
		}
		dict["S"] = Name("Movie")
		operation := action.Operation
		if operation == "" {
			operation = "Play"
		}
		if operation != "Play" && operation != "Stop" && operation != "Pause" && operation != "Resume" {
			return nil, fmt.Errorf("invalid movie action operation")
		}
		dict["Operation"] = operation
		if action.Target != nil {
			dict["Annotation"] = added[action.Target]
		} else if action.Annotation != (Reference{}) {
			dict["Annotation"] = action.Annotation
		} else {
			dict["T"], err = EncodeTextString(action.Title)
			if err != nil {
				return nil, err
			}
		}
		result[action] = dict
	}
	return result, ctx.Err()
}

// rewriteMovieActivation 解析显式播放参数并解除间接引用，不覆盖未指定的注解默认值
// 入参: ctx 取消上下文, dict 播放参数，引用须属于当前文件
// 返回: Dictionary 独立播放参数, error 参数、引用或取消错误
func (r *Reader) rewriteMovieActivation(ctx context.Context, dict Dictionary) (Dictionary, error) {
	if len(dict) == 0 {
		return make(Dictionary), ctx.Err()
	}
	activation, err := r.ReadMovieActivation(ctx, dict)
	if err != nil {
		return nil, err
	}
	result := make(Dictionary, len(dict))
	for key, object := range dict {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		value, err := r.Resolve(object)
		if err != nil {
			return nil, err
		}
		if value == nil {
			continue
		}
		switch key {
		case "Start":
			result[key] = rewriteMovieTime(*activation.Start)
		case "Duration":
			result[key] = rewriteMovieTime(*activation.Duration)
		case "Rate":
			result[key] = Real(activation.Rate)
		case "Volume":
			result[key] = Real(activation.Volume)
		case "ShowControls":
			result[key] = Boolean(activation.ShowControls)
		case "Synchronous":
			result[key] = Boolean(activation.Synchronous)
		case "Mode":
			result[key] = activation.Mode
		case "FWScale":
			result[key] = Array{Integer(activation.FloatingScale[0]), Integer(activation.FloatingScale[1])}
		case "FWPosition":
			result[key] = Array{Real(activation.FloatingPosition[0]), Real(activation.FloatingPosition[1])}
		default:
			return nil, fmt.Errorf("unknown movie activation parameter: %s", key)
		}
	}
	return result, ctx.Err()
}

// rewriteMovieTime 写出非负时间，超出32位整数范围时采用大端64位字节串
// 入参: value 已校验的时间和时间单位
// 返回: Object 时间值或二元数组
func rewriteMovieTime(value MovieTime) Object {
	var time Object = Integer(value.Value)
	if value.Value > math.MaxInt32 {
		data := make(String, 8)
		binary.BigEndian.PutUint64(data, uint64(value.Value))
		time = data
	}
	if value.Scale != 0 {
		return Array{time, Integer(value.Scale)}
	}
	return time
}
