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
	"os"
)

// ThreeD 保存3D数据、视图和激活参数，不解码模型或执行脚本
// AnnotationBackground仅在面色要求宿主背景时读取，空值表示透明
// AnnotationBox使用页面用户坐标，ViewBox以注解中心为原点
type ThreeD struct {
	Stream                *Stream
	Format                Name
	Views                 []Dictionary
	ViewReferences        []Reference
	DefaultView           Object
	SelectedView          Dictionary
	SelectedViewReference Reference
	Activation            Dictionary
	ActivationPolicy      ThreeDActivation
	AnimationStyle        ThreeDAnimation
	Projection            *ThreeDProjection
	Camera                ThreeDCamera
	Background            ThreeDBackground
	AnnotationBackground  *[3]float64
	Lighting              ThreeDLighting
	Presentation          ThreeDPresentation
	Measurements          []ThreeDMeasurement
	AnnotationBox         Rectangle
	ViewBox               Rectangle
	Resources             Object
	OnInstantiate         *Stream
	Animation             Dictionary
	Interactive           bool
	Dictionary            Dictionary
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

// mediaDictionariesContext 按取消上下文读取媒体字典数组，不预先解析字典内的脚本或数据流
// 入参: ctx 取消上下文, object 数组或引用
// 返回: []Dictionary 字典列表, error 数组、字典、引用或取消错误
func (r *Reader) mediaDictionariesContext(ctx context.Context, object Object) ([]Dictionary, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
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
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		dict, err := r.mediaDictionary(object, false)
		if err != nil {
			return nil, err
		}
		result = append(result, dict)
	}
	return result, ctx.Err()
}

// resolveIndirect 解析间接引用链并返回最终间接引用与目标对象，直接对象返回零值引用与原对象
// 入参: object 原始对象或间接引用
// 返回: Reference 最终间接引用, Object 目标对象, error 引用或循环错误
func (r *Reader) resolveIndirect(object Object) (Reference, Object, error) {
	if r == nil {
		return Reference{}, nil, fmt.Errorf("invalid reader")
	}
	if r.closed {
		return Reference{}, nil, os.ErrClosed
	}
	ref, ok := object.(Reference)
	if !ok {
		return Reference{}, object, nil
	}
	resolved, err := r.Object(ref)
	if err != nil {
		return Reference{}, nil, err
	}
	if _, ok := resolved.(Reference); !ok {
		return ref, resolved, nil
	}
	seen := map[Reference]bool{ref: true}
	object = resolved
	for {
		next, ok := object.(Reference)
		if !ok {
			return ref, object, nil
		}
		if seen[next] {
			return Reference{}, nil, fmt.Errorf("cyclic indirect reference %d %d", next.Number, next.Generation)
		}
		seen[next] = true
		ref = next
		resolved, err = r.Object(next)
		if err != nil {
			return Reference{}, nil, err
		}
		object = resolved
	}
}

// resolveIndirectReference 解析对象对应的最终间接引用，直接对象返回零值引用
// 入参: object 原始对象或间接引用
// 返回: Reference 最终间接引用, error 引用或循环错误
func (r *Reader) resolveIndirectReference(object Object) (Reference, error) {
	ref, _, err := r.resolveIndirect(object)
	return ref, err
}

// mediaDictionariesWithRefs 按取消上下文读取媒体字典数组及其间接引用，不预先解析字典内的脚本或数据流
// 入参: ctx 取消上下文, object 数组或引用
// 返回: []Dictionary 字典列表, []Reference 对应间接引用列表, error 数组、字典、引用或取消错误
func (r *Reader) mediaDictionariesWithRefs(ctx context.Context, object Object) ([]Dictionary, []Reference, error) {
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	value, err := r.Resolve(object)
	if err != nil || value == nil {
		return nil, nil, err
	}
	array, ok := value.(Array)
	if !ok {
		return nil, nil, fmt.Errorf("invalid media dictionary array")
	}
	result := make([]Dictionary, 0, len(array))
	refs := make([]Reference, 0, len(array))
	for _, item := range array {
		if err := ctx.Err(); err != nil {
			return nil, nil, err
		}
		ref, value, err := r.resolveIndirect(item)
		if err != nil {
			return nil, nil, err
		}
		dict, ok := value.(Dictionary)
		if !ok {
			return nil, nil, fmt.Errorf("invalid media dictionary")
		}
		result = append(result, dict)
		refs = append(refs, ref)
	}
	return result, refs, ctx.Err()
}

// ReadThreeD 解析3D注解及共享数据引用，保留模型原始编码和视图选择
// 入参: annotation 3D注解
// 返回: ThreeD 模型信息, error 类型、引用或视图错误
func (r *Reader) ReadThreeD(annotation Annotation) (ThreeD, error) {
	return r.ReadThreeDContext(context.Background(), annotation)
}

// ReadThreeDContext 解析三维注解及默认视图，支持批量测量读取取消
// 入参: ctx 取消上下文, annotation 三维注解
// 返回: ThreeD 模型信息, error 类型、引用、视图、关闭或取消错误
func (r *Reader) ReadThreeDContext(ctx context.Context, annotation Annotation) (ThreeD, error) {
	result := ThreeD{Interactive: true, Dictionary: annotation.Dictionary}
	if ctx == nil || r == nil {
		return result, fmt.Errorf("invalid 3D reader or context")
	}
	if err := ctx.Err(); err != nil {
		return result, err
	}
	if r.closed {
		return result, os.ErrClosed
	}
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
	if !ok || stream == nil {
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
	result.Views, result.ViewReferences, err = r.mediaDictionariesWithRefs(ctx, stream.Dictionary["VA"])
	if err != nil {
		return result, err
	}
	selectorRaw := annotation.Dictionary["3DV"]
	result.DefaultView, err = r.Resolve(selectorRaw)
	if err != nil {
		return result, err
	}
	if result.DefaultView == nil {
		selectorRaw = stream.Dictionary["DV"]
		result.DefaultView, err = r.Resolve(selectorRaw)
		if err != nil {
			return result, err
		}
		if result.DefaultView == nil && len(result.Views) != 0 {
			result.DefaultView = Integer(0)
			selectorRaw = Integer(0)
		}
	}
	selector := result.DefaultView
	if selector == Name("D") {
		selectorRaw = stream.Dictionary["DV"]
		selector, err = r.Resolve(selectorRaw)
		if err != nil {
			return result, err
		}
		if selector == nil && len(result.Views) != 0 {
			selector = Integer(0)
			selectorRaw = Integer(0)
		}
	}
	switch v := selector.(type) {
	case nil:
	case Dictionary:
		result.SelectedView = v
		result.SelectedViewReference, err = r.resolveIndirectReference(selectorRaw)
		if err != nil {
			return result, err
		}
	case String:
		name, err := DecodeTextString(v)
		if err != nil {
			return result, err
		}
		for i, view := range result.Views {
			if err := ctx.Err(); err != nil {
				return result, err
			}
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
					result.SelectedViewReference = result.ViewReferences[i]
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
		result.SelectedViewReference = result.ViewReferences[v]
	case Name:
		if (v != "F" && v != "L") || len(result.Views) == 0 {
			return result, fmt.Errorf("invalid 3D view selector")
		}
		index := 0
		if v == "L" {
			index = len(result.Views) - 1
		}
		result.SelectedView = result.Views[index]
		result.SelectedViewReference = result.ViewReferences[index]
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
		if !ok || result.OnInstantiate == nil {
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
	result.ActivationPolicy, err = r.ReadThreeDActivation(result.Activation)
	if err != nil {
		return result, err
	}
	result.AnimationStyle, err = r.ReadThreeDAnimation(result.Animation)
	if err != nil {
		return result, err
	}
	result.ViewBox, err = r.ReadThreeDViewBox(annotation)
	if err != nil {
		return result, err
	}
	box := annotation.Rect
	result.AnnotationBox = Rectangle{min(box.XMin, box.XMax), min(box.YMin, box.YMax), max(box.XMin, box.XMax), max(box.YMin, box.YMax)}
	if result.SelectedView != nil {
		projection, err := r.ReadThreeDProjection(result.SelectedView["P"])
		if err != nil {
			return result, err
		}
		result.Projection = &projection
	}
	result.Camera, err = r.readThreeDCamera(ctx, result.SelectedView)
	if err != nil {
		return result, err
	}
	result.Background, err = r.ReadThreeDBackground(result.SelectedView["BG"])
	if err != nil {
		return result, err
	}
	result.Lighting, err = r.ReadThreeDLighting(result.SelectedView["LS"])
	if err != nil {
		return result, err
	}
	result.Presentation, err = r.readThreeDPresentation(ctx, result.SelectedView)
	if err != nil {
		return result, err
	}
	result.Measurements, err = r.ReadThreeDMeasurements(ctx, result.SelectedView["MA"])
	if err != nil {
		return result, err
	}
	if mode := result.Presentation.RenderMode; mode != nil && mode.AnnotationBackground {
		result.AnnotationBackground, err = r.ReadAnnotationBackground(annotation)
		if err != nil {
			return result, err
		}
	}
	return result, ctx.Err()
}

// ReadRichMedia 解析资源名称树和实例配置，允许实例资源不出现在名称树中
// 入参: ctx 取消上下文, annotation 富媒体注解
// 返回: RichMedia 媒体信息, error 资源、配置、引用、关闭或取消错误
func (r *Reader) ReadRichMedia(ctx context.Context, annotation Annotation) (RichMedia, error) {
	result := RichMedia{}
	if ctx == nil || r == nil {
		return result, fmt.Errorf("invalid rich media reader or context")
	}
	if err := ctx.Err(); err != nil {
		return result, err
	}
	if r.closed {
		return result, os.ErrClosed
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
	configs, configRefs, err := r.mediaDictionariesWithRefs(ctx, dict["Configurations"])
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
		converted := MediaConfiguration{Reference: configRefs[index], Subtype: kind, Dictionary: config}
		instances, err := r.mediaDictionariesContext(ctx, config["Instances"])
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
	result.Views, err = r.mediaDictionariesContext(ctx, dict["Views"])
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
		ref, err := r.resolveIndirectReference(result.Activation["Configuration"])
		if err != nil {
			return result, err
		}
		if ref == (Reference{}) {
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
	return result, ctx.Err()
}
