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

// MovieTime 保存非负视频时间，Scale为0表示使用媒体自身的时间单位
type MovieTime struct {
	Value int64
	Scale int64
}

// MovieActivation 保存视频播放参数，不播放媒体或读取外部文件
// Start为空表示从头播放，Duration为空表示播放至结尾，FloatingScale为空表示嵌入显示
type MovieActivation struct {
	Start, Duration           *MovieTime
	Rate, Volume              float64
	ShowControls, Synchronous bool
	Mode                      Name
	FloatingScale             *[2]int64
	FloatingPosition          [2]float64
}

// MovieAction 保存视频目标和播放操作，Activation中的显式值覆盖目标注解的参数
// Annotation和Title指定原有目标，Target用于写入时引用本次新增的视频注解，不可同时指定
// 未声明参数需由调用方结合目标注解解析
type MovieAction struct {
	Annotation Reference
	Target     *MovieAnnotation
	Title      string
	Operation  Name
	Activation Dictionary
}

// ReadMovieAction 按PDF标准表209解析视频引用、标题及播放操作
// 入参: ctx 取消上下文, object 动作字典或引用
// 返回: MovieAction 播放动作, error 目标、操作、播放参数或取消错误
func (r *Reader) ReadMovieAction(ctx context.Context, object Object) (MovieAction, error) {
	result := MovieAction{Operation: "Play", Activation: make(Dictionary)}
	dict, err := r.actionDictionary(ctx, object, "Movie")
	if err != nil {
		return result, err
	}
	annotation, err := r.Resolve(dict["Annotation"])
	if err != nil {
		return result, err
	}
	title, err := r.Resolve(dict["T"])
	if err != nil {
		return result, err
	}
	if (annotation == nil) == (title == nil) {
		return result, fmt.Errorf("invalid movie action target")
	}
	if annotation != nil {
		ref, indirect := dict["Annotation"].(Reference)
		target, valid := annotation.(Dictionary)
		if !indirect || ref.Number <= 0 || !valid || target == nil {
			return result, fmt.Errorf("invalid movie annotation reference")
		}
		kind, err := r.Resolve(target["Subtype"])
		if err != nil {
			return result, err
		}
		if kind != Name("Movie") {
			return result, fmt.Errorf("invalid movie annotation type")
		}
		result.Annotation = ref
	} else {
		text, ok := title.(String)
		if !ok {
			return result, fmt.Errorf("invalid movie action title")
		}
		result.Title, err = DecodeTextString(text)
		if err != nil {
			return result, err
		}
	}
	value, err := r.Resolve(dict["Operation"])
	if err != nil {
		return result, err
	}
	if value != nil {
		operation, ok := value.(Name)
		if !ok || operation != "Play" && operation != "Stop" && operation != "Pause" && operation != "Resume" {
			return result, fmt.Errorf("invalid movie action operation")
		}
		result.Operation = operation
	}
	for _, key := range []Name{"Start", "Duration", "Rate", "Volume", "ShowControls", "Mode", "Synchronous", "FWScale", "FWPosition"} {
		value, err := r.Resolve(dict[key])
		if err != nil {
			return result, err
		}
		if value != nil {
			result.Activation[key] = value
		}
	}
	_, err = r.ReadMovieActivation(ctx, result.Activation)
	return result, err
}

// ReadMovieActivation 按PDF标准表296解析播放区间、时间单位及窗口参数
// 入参: ctx 取消上下文, object 播放字典或引用，空值采用标准默认值
// 返回: MovieActivation 播放参数, error 数值、窗口、引用或取消错误
func (r *Reader) ReadMovieActivation(ctx context.Context, object Object) (MovieActivation, error) {
	result := MovieActivation{Rate: 1, Volume: 1, Mode: "Once", FloatingPosition: [2]float64{0.5, 0.5}}
	if ctx == nil {
		return result, fmt.Errorf("invalid movie activation context")
	}
	if err := ctx.Err(); err != nil {
		return result, err
	}
	value, err := r.Resolve(object)
	if err != nil {
		return result, err
	}
	if value == nil {
		return result, ctx.Err()
	}
	dict, ok := value.(Dictionary)
	if !ok || dict == nil {
		return result, fmt.Errorf("invalid movie activation dictionary")
	}
	for _, field := range []struct {
		key    Name
		target **MovieTime
	}{{"Start", &result.Start}, {"Duration", &result.Duration}} {
		value, err := r.Resolve(dict[field.key])
		if err != nil {
			return result, err
		}
		if value != nil {
			time, err := r.movieTime(value)
			if err != nil {
				return result, err
			}
			*field.target = &time
		}
	}
	for _, field := range []struct {
		key    Name
		target *float64
	}{{"Rate", &result.Rate}, {"Volume", &result.Volume}} {
		value, err := r.Resolve(dict[field.key])
		if err != nil {
			return result, err
		}
		if value != nil {
			number, err := r.number(value)
			if err != nil {
				return result, err
			}
			if math.IsNaN(number) || math.IsInf(number, 0) || field.key == "Volume" && (number < -1 || number > 1) {
				return result, fmt.Errorf("invalid movie activation %s", field.key)
			}
			*field.target = number
		}
	}
	for _, field := range []struct {
		key    Name
		target *bool
	}{{"ShowControls", &result.ShowControls}, {"Synchronous", &result.Synchronous}} {
		value, err := r.Resolve(dict[field.key])
		if err != nil {
			return result, err
		}
		if value != nil {
			flag, ok := value.(Boolean)
			if !ok {
				return result, fmt.Errorf("invalid movie activation %s", field.key)
			}
			*field.target = bool(flag)
		}
	}
	value, err = r.Resolve(dict["Mode"])
	if err != nil {
		return result, err
	}
	if value != nil {
		mode, ok := value.(Name)
		if !ok || mode != "Once" && mode != "Open" && mode != "Repeat" && mode != "Palindrome" {
			return result, fmt.Errorf("invalid movie activation mode")
		}
		result.Mode = mode
	}
	for _, key := range []Name{"FWScale", "FWPosition"} {
		value, err := r.Resolve(dict[key])
		if err != nil {
			return result, err
		}
		if value == nil {
			continue
		}
		array, ok := value.(Array)
		if !ok || len(array) != 2 {
			return result, fmt.Errorf("invalid movie activation %s", key)
		}
		if key == "FWScale" {
			scale := [2]int64{}
			for index, object := range array {
				value, err := r.Resolve(object)
				if err != nil {
					return result, err
				}
				number, ok := value.(Integer)
				if !ok || number <= 0 {
					return result, fmt.Errorf("invalid movie floating scale")
				}
				scale[index] = int64(number)
			}
			result.FloatingScale = &scale
		} else {
			for index, object := range array {
				number, err := r.number(object)
				if err != nil {
					return result, err
				}
				if math.IsNaN(number) || number < 0 || number > 1 {
					return result, fmt.Errorf("invalid movie floating position")
				}
				result.FloatingPosition[index] = number
			}
		}
	}
	return result, ctx.Err()
}

// movieTime 读取非负整数或大端64位字节串及可选时间单位，不使用浮点数
// 入参: object 时间值或二元时间数组
// 返回: MovieTime 时间和单位, error 数值、长度或引用错误
func (r *Reader) movieTime(object Object) (MovieTime, error) {
	var result MovieTime
	value, err := r.Resolve(object)
	if err != nil {
		return result, err
	}
	if array, ok := value.(Array); ok {
		if len(array) != 2 {
			return result, fmt.Errorf("invalid movie time scale array")
		}
		value, err = r.Resolve(array[1])
		if err != nil {
			return result, err
		}
		scale, ok := value.(Integer)
		if !ok || scale <= 0 {
			return result, fmt.Errorf("invalid movie time scale")
		}
		result.Scale = int64(scale)
		value, err = r.Resolve(array[0])
		if err != nil {
			return result, err
		}
	}
	switch value := value.(type) {
	case Integer:
		result.Value = int64(value)
	case String:
		if len(value) != 8 {
			return result, fmt.Errorf("invalid movie time byte string")
		}
		result.Value = int64(binary.BigEndian.Uint64(value))
	default:
		return result, fmt.Errorf("invalid movie time")
	}
	if result.Value < 0 {
		return result, fmt.Errorf("negative movie time")
	}
	return result, nil
}
