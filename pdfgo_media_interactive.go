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

// ThreeD 保存3D数据、视图和激活参数，不解码模型或执行脚本
type ThreeD struct {
	Stream        *Stream
	Format        Name
	Views         []Dictionary
	DefaultView   Object
	SelectedView  Dictionary
	Activation    Dictionary
	Resources     Object
	OnInstantiate *Stream
	Animation     Dictionary
	Interactive   bool
	Dictionary    Dictionary
}

// RichMedia 保存富媒体资源、配置、视图和播放设置，不执行脚本
type RichMedia struct {
	Assets              []MediaAsset
	Configurations      []MediaConfiguration
	Views               []Dictionary
	Settings            Dictionary
	Dictionary          Dictionary
	Activation          Dictionary
	Deactivation        Dictionary
	Presentation        Dictionary
	ActiveConfiguration int
}

// MediaAsset 保存名称树中的媒体文件及其原始文件说明
type MediaAsset struct {
	Name string
	File FileSpecification
}

// MediaConfiguration 保存媒体实例组合及其原始配置
type MediaConfiguration struct {
	Reference  Reference
	Subtype    Name
	Instances  []MediaInstance
	Dictionary Dictionary
}

// MediaInstance 保存实例类型、媒体文件和运行参数
type MediaInstance struct {
	Subtype    Name
	Asset      FileSpecification
	Parameters Dictionary
	Dictionary Dictionary
}

// mediaDictionary 读取媒体字典，保留未知扩展字段
// 入参: object 字典或引用, optional 是否允许空值
// 返回: Dictionary 字典, error 对象类型或引用错误
func (r *Reader) mediaDictionary(object Object, optional bool) (Dictionary, error) {
	value, err := r.Resolve(object)
	if err != nil || value == nil && optional {
		return nil, err
	}
	dict, ok := value.(Dictionary)
	if !ok {
		return nil, fmt.Errorf("invalid media dictionary")
	}
	return dict, nil
}

// mediaDictionaries 读取媒体字典数组，不预先解析字典内的脚本或数据流
// 入参: object 数组或引用
// 返回: []Dictionary 字典列表, error 数组、字典或引用错误
func (r *Reader) mediaDictionaries(object Object) ([]Dictionary, error) {
	value, err := r.Resolve(object)
	if err != nil || value == nil {
		return nil, err
	}
	array, ok := value.(Array)
	if !ok {
		return nil, fmt.Errorf("invalid media dictionary array")
	}
	result := make([]Dictionary, 0, len(array))
	for _, object := range array {
		dict, err := r.mediaDictionary(object, false)
		if err != nil {
			return nil, err
		}
		result = append(result, dict)
	}
	return result, nil
}

// ReadThreeD 解析3D注解及共享数据引用，保留模型原始编码和视图选择
// 入参: annotation 3D注解
// 返回: ThreeD 模型信息, error 类型、引用或视图错误
func (r *Reader) ReadThreeD(annotation Annotation) (ThreeD, error) {
	result := ThreeD{Interactive: true, Dictionary: annotation.Dictionary}
	if annotation.Subtype != "3D" {
		return result, fmt.Errorf("not a 3D annotation")
	}
	value, err := r.Resolve(annotation.Dictionary["3DD"])
	if err != nil {
		return result, err
	}
	if dict, ok := value.(Dictionary); ok {
		typeValue, typeErr := r.Resolve(dict["Type"])
		if typeErr != nil {
			return result, typeErr
		}
		if typeValue != nil && typeValue != Name("3DRef") {
			return result, fmt.Errorf("invalid 3D reference")
		}
		value, err = r.Resolve(dict["3DD"])
		if err != nil {
			return result, err
		}
	}
	stream, ok := value.(*Stream)
	if !ok {
		return result, fmt.Errorf("invalid 3D stream")
	}
	result.Stream = stream
	value, err = r.Resolve(stream.Dictionary["Type"])
	if err != nil {
		return result, err
	}
	if value != nil && value != Name("3D") {
		return result, fmt.Errorf("invalid 3D stream type")
	}
	value, err = r.Resolve(stream.Dictionary["Subtype"])
	if err != nil {
		return result, err
	}
	result.Format, ok = value.(Name)
	if !ok || result.Format == "" {
		return result, fmt.Errorf("invalid 3D format")
	}
	result.Views, err = r.mediaDictionaries(stream.Dictionary["VA"])
	if err != nil {
		return result, err
	}
	result.DefaultView, err = r.Resolve(annotation.Dictionary["3DV"])
	if err != nil {
		return result, err
	}
	if result.DefaultView == nil {
		result.DefaultView, err = r.Resolve(stream.Dictionary["DV"])
		if err != nil {
			return result, err
		}
		if result.DefaultView == nil && len(result.Views) != 0 {
			result.DefaultView = Integer(0)
		}
	}
	selector := result.DefaultView
	if selector == Name("D") {
		selector, err = r.Resolve(stream.Dictionary["DV"])
		if err != nil {
			return result, err
		}
		if selector == nil && len(result.Views) != 0 {
			selector = Integer(0)
		}
	}
	switch v := selector.(type) {
	case nil:
	case Dictionary:
		result.SelectedView = v
	case String:
		name, err := DecodeTextString(v)
		if err != nil {
			return result, err
		}
		for _, view := range result.Views {
			value, err := r.Resolve(view["IN"])
			if err != nil {
				return result, err
			}
			if value, ok := value.(String); ok {
				internal, err := DecodeTextString(value)
				if err != nil {
					return result, err
				}
				if internal == name {
					result.SelectedView = view
					break
				}
			}
		}
		if result.SelectedView == nil {
			return result, fmt.Errorf("3D view name not found")
		}
	case Integer:
		if v < 0 || uint64(v) >= uint64(len(result.Views)) {
			return result, fmt.Errorf("invalid 3D view index")
		}
		result.SelectedView = result.Views[v]
	case Name:
		if (v != "F" && v != "L") || len(result.Views) == 0 {
			return result, fmt.Errorf("invalid 3D view selector")
		}
		index := 0
		if v == "L" {
			index = len(result.Views) - 1
		}
		result.SelectedView = result.Views[index]
	default:
		return result, fmt.Errorf("invalid 3D default view")
	}
	result.Resources, err = r.Resolve(stream.Dictionary["Resources"])
	if err != nil {
		return result, err
	}
	value, err = r.Resolve(stream.Dictionary["OnInstantiate"])
	if err != nil {
		return result, err
	}
	if value != nil {
		result.OnInstantiate, ok = value.(*Stream)
		if !ok {
			return result, fmt.Errorf("invalid 3D initialization script")
		}
	}
	result.Animation, err = r.mediaDictionary(stream.Dictionary["AN"], true)
	if err != nil {
		return result, err
	}
	result.Activation, err = r.mediaDictionary(annotation.Dictionary["3DA"], true)
	if err != nil {
		return result, err
	}
	value, err = r.Resolve(annotation.Dictionary["3DI"])
	if err != nil {
		return result, err
	}
	if value != nil {
		flag, ok := value.(Boolean)
		if !ok {
			return result, fmt.Errorf("invalid 3D interactive flag")
		}
		result.Interactive = bool(flag)
	}
	return result, nil
}

// ReadRichMedia 解析资源名称树和实例配置，允许实例资源不出现在名称树中
// 入参: ctx 取消上下文, annotation 富媒体注解
// 返回: RichMedia 媒体信息, error 资源、配置或引用错误
func (r *Reader) ReadRichMedia(ctx context.Context, annotation Annotation) (RichMedia, error) {
	result := RichMedia{}
	if err := ctx.Err(); err != nil {
		return result, err
	}
	if annotation.Subtype != "RichMedia" {
		return result, fmt.Errorf("not a rich media annotation")
	}
	dict, err := r.mediaDictionary(annotation.Dictionary["RichMediaContent"], false)
	if err != nil {
		return result, err
	}
	result.Dictionary = dict
	err = r.WalkNameTree(ctx, dict["Assets"], func(name string, object Object) error {
		file, err := r.ReadFileSpecification(object)
		if err != nil {
			return err
		}
		name, err = DecodeTextString(String(name))
		if err != nil {
			return err
		}
		result.Assets = append(result.Assets, MediaAsset{Name: name, File: file})
		return nil
	})
	if err != nil {
		return result, err
	}
	objects, err := r.Resolve(dict["Configurations"])
	if err != nil {
		return result, err
	}
	configs, err := r.mediaDictionaries(objects)
	if err != nil {
		return result, err
	}
	if len(configs) == 0 {
		return result, fmt.Errorf("missing rich media configurations")
	}
	for index, config := range configs {
		if err := ctx.Err(); err != nil {
			return result, err
		}
		value, err := r.Resolve(config["Subtype"])
		if err != nil {
			return result, err
		}
		kind, ok := value.(Name)
		if value != nil && (!ok || kind == "") {
			return result, fmt.Errorf("invalid rich media configuration subtype")
		}
		converted := MediaConfiguration{Subtype: kind, Dictionary: config}
		converted.Reference, _ = objects.(Array)[index].(Reference)
		instances, err := r.mediaDictionaries(config["Instances"])
		if err != nil {
			return result, err
		}
		for _, instance := range instances {
			if err := ctx.Err(); err != nil {
				return result, err
			}
			value, err := r.Resolve(instance["Subtype"])
			if err != nil {
				return result, err
			}
			subtype, ok := value.(Name)
			if !ok || subtype == "" {
				return result, fmt.Errorf("invalid rich media instance subtype")
			}
			file, err := r.ReadFileSpecification(instance["Asset"])
			if err != nil {
				return result, err
			}
			params, err := r.mediaDictionary(instance["Params"], true)
			if err != nil {
				return result, err
			}
			converted.Instances = append(converted.Instances, MediaInstance{Subtype: subtype, Asset: file, Parameters: params, Dictionary: instance})
		}
		if converted.Subtype == "" && len(converted.Instances) != 0 {
			converted.Subtype = converted.Instances[0].Subtype
		}
		result.Configurations = append(result.Configurations, converted)
	}
	result.Views, err = r.mediaDictionaries(dict["Views"])
	if err != nil {
		return result, err
	}
	result.Settings, err = r.mediaDictionary(annotation.Dictionary["RichMediaSettings"], true)
	if err != nil {
		return result, err
	}
	result.Activation, err = r.mediaDictionary(result.Settings["Activation"], true)
	if err != nil {
		return result, err
	}
	result.Deactivation, err = r.mediaDictionary(result.Settings["Deactivation"], true)
	if err != nil {
		return result, err
	}
	result.Presentation, err = r.mediaDictionary(result.Activation["Presentation"], true)
	if err != nil {
		return result, err
	}
	selected, err := r.Resolve(result.Activation["Configuration"])
	if err != nil {
		return result, err
	}
	if selected != nil {
		ref, ok := result.Activation["Configuration"].(Reference)
		if !ok {
			return result, fmt.Errorf("rich media activation configuration requires an indirect reference")
		}
		result.ActiveConfiguration = -1
		for index, config := range result.Configurations {
			if ref == config.Reference {
				result.ActiveConfiguration = index
				break
			}
		}
		if result.ActiveConfiguration < 0 {
			return result, fmt.Errorf("rich media activation configuration is not in content")
		}
	}
	return result, nil
}
