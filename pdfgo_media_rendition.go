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
)

// Rendition 保存媒体呈现及有序候选树，不自行选择播放器或执行脚本
type Rendition struct {
	Subtype      Name
	Name         string
	Clip         *MediaClip
	Alternatives []*Rendition
	Play         Dictionary
	Screen       Dictionary
	MustHonor    Dictionary
	BestEffort   Dictionary
	Dictionary   Dictionary
}

// MediaClip 保存媒体数据、嵌套片段及必须遵守的播放约束
type MediaClip struct {
	Subtype     Name
	Name        string
	ContentType string
	File        *FileSpecification
	Form        *Stream
	Section     *MediaClip
	Permissions Dictionary
	Players     Dictionary
	MustHonor   Dictionary
	BestEffort  Dictionary
	Dictionary  Dictionary
}

// ReadRendition 读取媒体呈现及选择器候选树，保留必须遵守与尽力处理的约束
// 入参: ctx 取消上下文, object 呈现字典或间接引用
// 返回: *Rendition 媒体呈现, error 定义或循环引用错误
func (r *Reader) ReadRendition(ctx context.Context, object Object) (*Rendition, error) {
	return r.readRendition(ctx, object, 0)
}

// readRendition 递归读取呈现树，不将未知子类型误判为可播放媒体
// 入参: ctx 取消上下文, object 呈现对象, depth 嵌套深度
// 返回: *Rendition 呈现对象, error 引用或定义错误
func (r *Reader) readRendition(ctx context.Context, object Object, depth int) (*Rendition, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if depth >= 32 {
		return nil, fmt.Errorf("rendition recursion limit exceeded")
	}
	dict, err := r.mediaDictionary(object, false)
	if err != nil {
		return nil, err
	}
	result := &Rendition{Dictionary: dict}
	result.Subtype, result.Name, err = r.mediaHeader(dict, "Rendition")
	if err != nil {
		return nil, err
	}
	result.MustHonor, err = r.mediaDictionary(dict["MH"], true)
	if err != nil {
		return nil, err
	}
	result.BestEffort, err = r.mediaDictionary(dict["BE"], true)
	if err != nil {
		return nil, err
	}
	if result.Subtype == "SR" {
		value, err := r.Resolve(dict["R"])
		if err != nil {
			return nil, err
		}
		array, ok := value.(Array)
		if !ok {
			return nil, fmt.Errorf("invalid selector rendition array")
		}
		for _, child := range array {
			candidate, err := r.readRendition(ctx, child, depth+1)
			if err != nil {
				return nil, err
			}
			result.Alternatives = append(result.Alternatives, candidate)
		}
		return result, nil
	}
	if result.Subtype != "MR" {
		return result, nil
	}
	if dict["C"] != nil {
		result.Clip, err = r.readMediaClip(ctx, dict["C"], 0)
		if err != nil {
			return nil, err
		}
	}
	result.Play, err = r.mediaParameters(dict["P"], "MediaPlayParams")
	if err != nil {
		return nil, err
	}
	result.Screen, err = r.mediaParameters(dict["SP"], "MediaScreenParams")
	if err != nil {
		return nil, err
	}
	if result.Clip == nil {
		players, err := r.mediaDictionary(result.Play["PL"], true)
		if err != nil {
			return nil, err
		}
		available := false
		for _, key := range []Name{"MU", "A"} {
			value, err := r.Resolve(players[key])
			if err != nil {
				return nil, err
			}
			if value != nil {
				array, ok := value.(Array)
				if !ok {
					return nil, fmt.Errorf("invalid media player array")
				}
				available = available || len(array) != 0
			}
		}
		if !available {
			return nil, fmt.Errorf("media rendition has no input or player")
		}
	}
	return result, nil
}

// ReadMediaClip 读取媒体数据或连续片段，不访问外部文件或网络
// 入参: ctx 取消上下文, object 媒体片段字典或间接引用
// 返回: *MediaClip 媒体片段, error 定义或循环引用错误
func (r *Reader) ReadMediaClip(ctx context.Context, object Object) (*MediaClip, error) {
	return r.readMediaClip(ctx, object, 0)
}

// readMediaClip 读取片段链，保留时间、帧和标记偏移的原始字典
// 入参: ctx 取消上下文, object 片段对象, depth 嵌套深度
// 返回: *MediaClip 片段信息, error 引用或定义错误
func (r *Reader) readMediaClip(ctx context.Context, object Object, depth int) (*MediaClip, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if depth >= 32 {
		return nil, fmt.Errorf("media clip recursion limit exceeded")
	}
	dict, err := r.mediaDictionary(object, false)
	if err != nil {
		return nil, err
	}
	result := &MediaClip{Dictionary: dict}
	result.Subtype, result.Name, err = r.mediaHeader(dict, "MediaClip")
	if err != nil {
		return nil, err
	}
	result.MustHonor, err = r.mediaDictionary(dict["MH"], true)
	if err != nil {
		return nil, err
	}
	result.BestEffort, err = r.mediaDictionary(dict["BE"], true)
	if err != nil {
		return nil, err
	}
	if result.Subtype == "MCS" {
		result.Section, err = r.readMediaClip(ctx, dict["D"], depth+1)
		return result, err
	}
	if result.Subtype != "MCD" {
		return result, nil
	}
	value, err := r.Resolve(dict["D"])
	if err != nil {
		return nil, err
	}
	if stream, ok := value.(*Stream); ok {
		typeValue, err := r.Resolve(stream.Dictionary["Type"])
		if err != nil {
			return nil, err
		}
		kind, err := r.Resolve(stream.Dictionary["Subtype"])
		if err != nil {
			return nil, err
		}
		if typeValue != Name("XObject") || kind != Name("Form") || dict["CT"] != nil {
			return nil, fmt.Errorf("invalid form media clip")
		}
		result.Form = stream
	} else {
		fileDict, err := r.mediaDictionary(value, false)
		if err != nil {
			return nil, err
		}
		kind, err := r.Resolve(fileDict["Type"])
		if err != nil {
			return nil, err
		}
		if kind != Name("Filespec") {
			return nil, fmt.Errorf("invalid media clip file type")
		}
		file, err := r.ReadFileSpecification(fileDict)
		if err != nil {
			return nil, err
		}
		result.File = &file
		value, err := r.Resolve(dict["CT"])
		if err != nil {
			return nil, err
		}
		if value != nil {
			text, ok := value.(String)
			if !ok {
				return nil, fmt.Errorf("invalid media content type")
			}
			for _, b := range text {
				if b > 127 {
					return nil, fmt.Errorf("non-ASCII media content type")
				}
			}
			result.ContentType = string(text)
		}
	}
	result.Permissions, err = r.mediaParameters(dict["P"], "MediaPermissions")
	if err != nil {
		return nil, err
	}
	result.Players, err = r.mediaParameters(dict["PL"], "MediaPlayers")
	return result, err
}

// mediaParameters 验证媒体参数对象类型及约束字典，不评估设备可用性
// 入参: object 参数字典或间接引用, kind 预期对象类型
// 返回: Dictionary 参数字典, error 字段或引用错误
func (r *Reader) mediaParameters(object Object, kind Name) (Dictionary, error) {
	dict, err := r.mediaDictionary(object, true)
	if err != nil || dict == nil {
		return dict, err
	}
	value, err := r.Resolve(dict["Type"])
	if err != nil {
		return nil, err
	}
	if value != nil && value != kind {
		return nil, fmt.Errorf("invalid %s parameter type", kind)
	}
	for _, key := range []Name{"MH", "BE"} {
		if _, err := r.mediaDictionary(dict[key], true); err != nil {
			return nil, err
		}
	}
	return dict, nil
}

// mediaHeader 读取媒体对象类型、子类型及可选文本名称
// 入参: dict 媒体字典, kind 对象类型
// 返回: Name 子类型, string 名称, error 字段错误
func (r *Reader) mediaHeader(dict Dictionary, kind Name) (Name, string, error) {
	value, err := r.Resolve(dict["Type"])
	if err != nil {
		return "", "", err
	}
	if value != nil && value != kind {
		return "", "", fmt.Errorf("invalid %s object type", kind)
	}
	value, err = r.Resolve(dict["S"])
	if err != nil {
		return "", "", err
	}
	subtype, ok := value.(Name)
	if !ok {
		return "", "", fmt.Errorf("invalid %s subtype", kind)
	}
	value, err = r.Resolve(dict["N"])
	if err != nil || value == nil {
		return subtype, "", err
	}
	name, ok := value.(String)
	if !ok {
		return "", "", fmt.Errorf("invalid media name")
	}
	text, err := DecodeTextString(name)
	return subtype, text, err
}

// Select 按深度优先顺序选择首个可用呈现，回调也检查选择器自身的约束
// 入参: ctx 取消上下文, viable 调用方对设备、播放器和必须遵守约束的检查
// 返回: *Rendition 可用媒体呈现，无匹配返回空值, error 取消或检查错误
func (m *Rendition) Select(ctx context.Context, viable func(*Rendition) (bool, error)) (*Rendition, error) {
	if viable == nil {
		return nil, fmt.Errorf("missing rendition viability check")
	}
	return m.selectRendition(ctx, viable, 0)
}

// selectRendition 遍历候选树，跳过不可用选择器的整个分支
// 入参: ctx 取消上下文, viable 可用性检查, depth 嵌套深度
// 返回: *Rendition 可用媒体呈现, error 循环、取消或检查错误
func (m *Rendition) selectRendition(ctx context.Context, viable func(*Rendition) (bool, error), depth int) (*Rendition, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if depth >= 32 {
		return nil, fmt.Errorf("rendition selection recursion limit exceeded")
	}
	if m == nil || m.Subtype != "MR" && m.Subtype != "SR" {
		return nil, nil
	}
	ok, err := viable(m)
	if err != nil || !ok {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if m.Subtype == "MR" {
		return m, nil
	}
	for _, child := range m.Alternatives {
		selected, err := child.selectRendition(ctx, viable, depth+1)
		if err != nil || selected != nil {
			return selected, err
		}
	}
	return nil, nil
}
