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
	"fmt"
	"math"
	"slices"
)

// ThreeDActivation 保存3D注解的激活、停用及界面初始状态，不执行脚本
type ThreeDActivation struct {
	Activation, ActivationState     Name
	Deactivation, DeactivationState Name
	Toolbar, Navigator              bool
}

// ThreeDAnimation 保存关键帧动画方式，负播放次数表示无限重复
type ThreeDAnimation struct {
	Style          Name
	PlayCount      int64
	TimeMultiplier float64
}

// ThreeDProjection 保存相机投影参数，空裁剪距离表示自动裁剪或未限定远平面
// PerspectiveBinding为空时使用PerspectiveDiameter，正交缩放不采用透视参数
type ThreeDProjection struct {
	Subtype, Clipping                       Name
	Near, Far                               *float64
	FieldOfView                             float64
	PerspectiveBinding, OrthographicBinding Name
	PerspectiveDiameter, OrthographicScale  float64
}

// ThreeDCamera 保存相机来源，Source为空时使用模型中的视图
// Matrix仅用于M来源，Path仅用于U3D来源；CenterOfOrbit为空时由阅读器确定
type ThreeDCamera struct {
	Source        Name
	Matrix        [12]float64
	Path          []string
	CenterOfOrbit *float64
}

// ThreeDBackground 保存不透明背景，未知颜色空间保留原值且RGB为空
type ThreeDBackground struct {
	ColorSpace       Object
	Color            Object
	RGB              *[3]float64
	EntireAnnotation bool
}

// ThreeDLight 保存无限远光源，Direction表示光线发射方向，CameraAttached表示随相机转动
type ThreeDLight struct {
	Color, Direction [3]float64
	CameraAttached   bool
}

// ThreeDLighting 保存标准灯光方案，Artwork沿用模型灯光，Ambient为材质漫反射增量
type ThreeDLighting struct {
	Subtype    Name
	Lights     []ThreeDLight
	Ambient    [3]float64
	Dictionary Dictionary
}

// ThreeDPresentation 保存当前视图的绘制模式、叠加外观及节点状态，未知绘制模式按默认处理
type ThreeDPresentation struct {
	RenderMode    Name
	Overlay       *Stream
	CrossSections []ThreeDCrossSection
	Nodes         []ThreeDNode
	ResetNodes    bool
}

// ReadThreeDPresentation 按PDF标准表304及表308读取生效的视图内容，不执行脚本
// 入参: object 视图字典或引用
// 返回: ThreeDPresentation 呈现参数, error 类型或引用错误
func (r *Reader) ReadThreeDPresentation(object Object) (ThreeDPresentation, error) {
	result := ThreeDPresentation{}
	dict, err := r.mediaDictionary(object, true)
	if err != nil {
		return result, err
	}
	mode, err := r.mediaDictionary(dict["RM"], true)
	if err != nil {
		return result, err
	}
	if mode != nil {
		value, err := r.Resolve(mode["Subtype"])
		if err != nil {
			return result, err
		}
		name, ok := value.(Name)
		if !ok {
			return result, fmt.Errorf("invalid 3D render mode")
		}
		switch name {
		case "Solid", "SolidWireframe", "Transparent", "TransparentWireframe", "BoundingBox", "TransparentBoundingBox", "TransparentBoundingBoxOutline", "Wireframe", "ShadedWireframe", "HiddenWireframe", "Vertices", "ShadedVertices", "Illustration", "SolidOutline", "ShadedIllustration":
			result.RenderMode = name
		}
	}
	sections, err := r.mediaDictionaries(dict["SA"])
	if err != nil {
		return result, err
	}
	if len(sections) != 0 {
		result.CrossSections = make([]ThreeDCrossSection, len(sections))
		for i, section := range sections {
			result.CrossSections[i], err = r.ReadThreeDCrossSection(section)
			if err != nil {
				return result, err
			}
		}
	}
	reset, err := r.Resolve(dict["NR"])
	if err != nil {
		return result, err
	}
	if reset != nil {
		flag, ok := reset.(Boolean)
		if !ok {
			return result, fmt.Errorf("invalid 3D node reset")
		}
		result.ResetNodes = bool(flag)
		nodes, err := r.mediaDictionaries(dict["NA"])
		if err != nil {
			return result, err
		}
		if len(nodes) != 0 {
			result.Nodes = make([]ThreeDNode, len(nodes))
			for i, node := range nodes {
				result.Nodes[i], err = r.ReadThreeDNode(node)
				if err != nil {
					return result, err
				}
			}
		}
	}
	source, err := r.Resolve(dict["MS"])
	if err != nil {
		return result, err
	}
	projection, err := r.Resolve(dict["P"])
	if err != nil {
		return result, err
	}
	if source != nil && projection != nil {
		value, err := r.Resolve(dict["O"])
		if err != nil {
			return result, err
		}
		if value != nil {
			stream, ok := value.(*Stream)
			if !ok || stream == nil {
				return result, fmt.Errorf("invalid 3D view overlay")
			}
			subtype, err := r.Resolve(stream.Dictionary["Subtype"])
			if err != nil {
				return result, err
			}
			if subtype != Name("Form") {
				return result, fmt.Errorf("invalid 3D view overlay subtype")
			}
			result.Overlay = stream
		}
	}
	return result, nil
}

// ReadThreeDLighting 按PDF标准表309及表310解析灯光，未知方案忽略并沿用模型灯光
// 入参: object 灯光字典或引用
// 返回: ThreeDLighting 灯光参数, error 类型或引用错误
func (r *Reader) ReadThreeDLighting(object Object) (ThreeDLighting, error) {
	result := ThreeDLighting{Subtype: "Artwork"}
	dict, err := r.mediaDictionary(object, true)
	if err != nil || dict == nil {
		return result, err
	}
	result.Dictionary = dict
	typeName := Name("3DLightingScheme")
	if err := r.threeDName(dict, "Type", &typeName, "3DLightingScheme"); err != nil {
		return result, err
	}
	value, err := r.Resolve(dict["Subtype"])
	if err != nil {
		return result, err
	}
	subtype, ok := value.(Name)
	if !ok {
		return result, fmt.Errorf("invalid 3D lighting subtype")
	}
	var lights [][2][3]float64
	switch subtype {
	case "Artwork", "None":
	case "White":
		lights = [][2][3]float64{{{.38, .38, .45}, {-2, -1.5, -.5}}, {{.6, .6, .67}, {2, 1.1, -2.5}}, {{.5, .5, .57}, {-.5, 0, 2}}}
	case "Day":
		lights = [][2][3]float64{{{.5, .5, .5}, {-2, -1.5, -.5}}, {{.8, .8, .9}, {2, 1.1, -2.5}}, {{.9, .9, .9}, {.02, .01, 2}}}
	case "Night":
		lights = [][2][3]float64{{{1, .75, .39}, {-2, -1.5, -.5}}, {{.31, .47, .55}, {2, 1.1, -2.5}}, {{.5, .5, 1}, {0, 0, 2}}}
	case "Hard":
		lights = [][2][3]float64{{{.5, .5, .5}, {-1.5, -1.5, -1.5}}, {{.8, .8, .9}, {1.5, 1.5, -1.5}}, {{.9, .9, .9}, {-.5, 0, 2}}}
		result.Ambient = [3]float64{.5, .5, .5}
	case "Primary":
		lights = [][2][3]float64{{{1, .2, .5}, {-2, -1.5, -.5}}, {{.2, 1, .5}, {2, 1.1, -2.5}}, {{0, 0, 1}, {0, 0, 2}}}
	case "Blue":
		lights = [][2][3]float64{{{.4, .4, .7}, {-2, -1.5, -.5}}, {{.75, .75, .95}, {2, 1.1, -2.5}}, {{.7, .7, .95}, {0, 0, 2}}}
	case "Red":
		lights = [][2][3]float64{{{.8, .3, .4}, {-2, -1.5, -.5}}, {{.95, .5, .7}, {2, 1.1, -2.5}}, {{.95, .4, .5}, {0, 0, 2}}}
	case "Cube":
		lights = [][2][3]float64{{{.4, .4, .4}, {1, .01, .01}}, {{.4, .4, .4}, {.01, 1, .01}}, {{.4, .4, .4}, {.01, .01, 1}}, {{.4, .4, .4}, {-1, .01, .01}}, {{.4, .4, .4}, {.01, -1, .01}}, {{.4, .4, .4}, {.01, .01, -1}}}
	case "CAD":
		lights = [][2][3]float64{{{.72, .72, .81}, {}}, {{.2, .2, .2}, {-2, -1.5, -.5}}, {{.32, .32, .32}, {2, 1.1, -2.5}}, {{.36, .36, .36}, {.04, .01, 2}}}
	case "Headlamp":
		lights = [][2][3]float64{{{.8, .8, .9}, {}}}
		result.Ambient = [3]float64{.1, .1, .1}
	default:
		return result, nil
	}
	result.Subtype = subtype
	if len(lights) != 0 {
		result.Lights = make([]ThreeDLight, len(lights))
		for i, light := range lights {
			result.Lights[i] = ThreeDLight{Color: light[0], Direction: light[1], CameraAttached: light[1] == ([3]float64{})}
		}
	}
	return result, nil
}

// ReadThreeDCamera 按PDF标准表304解析相机来源，仅读取该来源生效的参数
// 入参: object 视图字典或引用
// 返回: ThreeDCamera 相机参数, error 类型、数值、路径或引用错误
func (r *Reader) ReadThreeDCamera(object Object) (ThreeDCamera, error) {
	result := ThreeDCamera{}
	dict, err := r.mediaDictionary(object, true)
	if err != nil {
		return result, err
	}
	if err := r.threeDName(dict, "MS", &result.Source, "M", "U3D"); err != nil {
		return result, err
	}
	if result.Source == "" {
		return result, nil
	}
	if result.Source == "M" {
		values, err := r.numberArray(dict["C2W"], 12)
		if err != nil {
			return result, err
		}
		for i, value := range values {
			if math.IsNaN(value) || math.IsInf(value, 0) {
				return result, fmt.Errorf("invalid 3D camera matrix")
			}
			result.Matrix[i] = value
		}
	} else {
		value, err := r.Resolve(dict["U3DPath"])
		if err != nil {
			return result, err
		}
		path, ok := value.(Array)
		if _, single := value.(String); single {
			path, ok = Array{value}, true
		}
		if !ok || len(path) == 0 {
			return result, fmt.Errorf("invalid 3D camera path")
		}
		result.Path = make([]string, len(path))
		for i, item := range path {
			value, err := r.Resolve(item)
			if err != nil {
				return result, err
			}
			text, ok := value.(String)
			if !ok {
				return result, fmt.Errorf("invalid 3D camera path component")
			}
			result.Path[i], err = DecodeTextString(text)
			if err != nil {
				return result, err
			}
		}
	}
	value, err := r.Resolve(dict["CO"])
	if err != nil || value == nil {
		return result, err
	}
	number := 0.0
	if err := r.threeDNumber(dict, "CO", &number); err != nil {
		return result, err
	}
	if number < 0 {
		return result, fmt.Errorf("invalid 3D center of orbit")
	}
	result.CenterOfOrbit = &number
	return result, nil
}

// ReadThreeDBackground 按PDF标准表306解析背景，缺省使用白色且仅覆盖3D视口
// 入参: object 背景字典或引用
// 返回: ThreeDBackground 背景参数, error 类型、数值或引用错误
func (r *Reader) ReadThreeDBackground(object Object) (ThreeDBackground, error) {
	result := ThreeDBackground{ColorSpace: Name("DeviceRGB")}
	dict, err := r.mediaDictionary(object, true)
	if err != nil {
		return result, err
	}
	typeName, subtype := Name("3DBG"), Name("SC")
	if err := r.threeDName(dict, "Type", &typeName, "3DBG"); err != nil {
		return result, err
	}
	if err := r.threeDName(dict, "Subtype", &subtype, "SC"); err != nil {
		return result, err
	}
	value, err := r.Resolve(dict["CS"])
	if err != nil {
		return result, err
	}
	if value != nil {
		switch value.(type) {
		case Name, Array:
			result.ColorSpace = value
		default:
			return result, fmt.Errorf("invalid 3D background color space")
		}
	}
	result.Color, err = r.Resolve(dict["C"])
	if err != nil {
		return result, err
	}
	if space, ok := result.ColorSpace.(Name); ok && space == "DeviceRGB" {
		color := [3]float64{1, 1, 1}
		if result.Color != nil {
			values, err := r.numberArray(result.Color, 3)
			if err != nil {
				return result, err
			}
			for i, v := range values {
				if math.IsNaN(v) || math.IsInf(v, 0) {
					return result, fmt.Errorf("invalid 3D background color")
				}
				color[i] = min(1, max(0, v))
			}
		}
		result.RGB = &color
	}
	value, err = r.Resolve(dict["EA"])
	if err != nil {
		return result, err
	}
	if value != nil {
		flag, ok := value.(Boolean)
		if !ok {
			return result, fmt.Errorf("invalid 3D background extent")
		}
		result.EntireAnnotation = bool(flag)
	}
	return result, nil
}

// ReadThreeDActivation 按PDF标准表299解析3D激活参数，空值采用标准默认值
// 入参: object 激活字典或引用
// 返回: ThreeDActivation 激活参数, error 字段类型、枚举或引用错误
func (r *Reader) ReadThreeDActivation(object Object) (ThreeDActivation, error) {
	result := ThreeDActivation{Activation: "XA", ActivationState: "L", Deactivation: "PI", DeactivationState: "U", Toolbar: true}
	dict, err := r.mediaDictionary(object, true)
	if err != nil {
		return result, err
	}
	for _, field := range []struct {
		key    Name
		target *Name
		values []Name
	}{
		{"A", &result.Activation, []Name{"PO", "PV", "XA"}},
		{"AIS", &result.ActivationState, []Name{"I", "L"}},
		{"D", &result.Deactivation, []Name{"PC", "PI", "XD"}},
		{"DIS", &result.DeactivationState, []Name{"U", "I", "L"}},
	} {
		if err := r.threeDName(dict, field.key, field.target, field.values...); err != nil {
			return result, err
		}
	}
	for _, field := range []struct {
		key    Name
		target *bool
	}{{"TB", &result.Toolbar}, {"NP", &result.Navigator}} {
		value, err := r.Resolve(dict[field.key])
		if err != nil {
			return result, err
		}
		if value != nil {
			flag, ok := value.(Boolean)
			if !ok {
				return result, fmt.Errorf("invalid 3D activation %s", field.key)
			}
			*field.target = bool(flag)
		}
	}
	return result, nil
}

// ReadThreeDAnimation 按PDF标准表301解析动画方式，未知方式按None处理并忽略播放参数
// 入参: object 动画字典或引用
// 返回: ThreeDAnimation 动画参数, error 字典、数值或引用错误
func (r *Reader) ReadThreeDAnimation(object Object) (ThreeDAnimation, error) {
	result := ThreeDAnimation{Style: "None", TimeMultiplier: 1}
	dict, err := r.mediaDictionary(object, true)
	if err != nil {
		return result, err
	}
	typeName := Name("3DAnimationStyle")
	if err := r.threeDName(dict, "Type", &typeName, "3DAnimationStyle"); err != nil {
		return result, err
	}
	value, err := r.Resolve(dict["Subtype"])
	if err != nil {
		return result, err
	}
	if value != nil {
		name, ok := value.(Name)
		if !ok {
			return result, fmt.Errorf("invalid 3D animation subtype")
		}
		if name == "Linear" || name == "Oscillating" {
			result.Style = name
		}
	}
	if result.Style == "None" {
		return result, nil
	}
	value, err = r.Resolve(dict["PC"])
	if err != nil {
		return result, err
	}
	if value != nil {
		count, ok := value.(Integer)
		if !ok {
			return result, fmt.Errorf("invalid 3D animation play count")
		}
		result.PlayCount = int64(count)
	}
	if err := r.threeDNumber(dict, "TM", &result.TimeMultiplier); err != nil {
		return result, err
	}
	if result.TimeMultiplier <= 0 {
		return result, fmt.Errorf("invalid 3D animation time multiplier")
	}
	return result, nil
}

// ReadThreeDProjection 按PDF标准表305解析投影，仅校验当前投影与裁剪方式生效的字段
// 入参: object 投影字典或引用，空值使用90度透视投影
// 返回: ThreeDProjection 投影参数, error 类型、数值或引用错误
func (r *Reader) ReadThreeDProjection(object Object) (ThreeDProjection, error) {
	result := ThreeDProjection{Subtype: "P", Clipping: "ANF", FieldOfView: 90, PerspectiveBinding: "W", OrthographicBinding: "Absolute", OrthographicScale: 1}
	value, err := r.Resolve(object)
	if err != nil || value == nil {
		return result, err
	}
	dict, err := r.mediaDictionary(value, false)
	if err != nil {
		return result, err
	}
	result.Subtype = ""
	if err := r.threeDName(dict, "Subtype", &result.Subtype, "P", "O"); err != nil {
		return result, err
	}
	if result.Subtype == "" {
		return result, fmt.Errorf("missing 3D projection subtype")
	}
	if err := r.threeDName(dict, "CS", &result.Clipping, "ANF", "XNF"); err != nil {
		return result, err
	}
	if result.Clipping == "XNF" {
		near, err := r.Resolve(dict["N"])
		if err != nil {
			return result, err
		}
		if near == nil && result.Subtype == "P" {
			return result, fmt.Errorf("missing 3D perspective near distance")
		}
		number := 0.0
		if err := r.threeDNumber(dict, "N", &number); err != nil {
			return result, err
		}
		if number < 0 || number == 0 && result.Subtype == "P" {
			return result, fmt.Errorf("invalid 3D projection near distance")
		}
		result.Near = &number
		far, err := r.Resolve(dict["F"])
		if err != nil {
			return result, err
		}
		if far != nil {
			number := 0.0
			if err := r.threeDNumber(dict, "F", &number); err != nil {
				return result, err
			}
			result.Far = &number
		}
	}
	if result.Subtype == "O" {
		if err := r.threeDNumber(dict, "OS", &result.OrthographicScale); err != nil {
			return result, err
		}
		if result.OrthographicScale <= 0 {
			return result, fmt.Errorf("invalid 3D orthographic scale")
		}
		if err := r.threeDName(dict, "OB", &result.OrthographicBinding, "W", "H", "Min", "Max", "Absolute"); err != nil {
			return result, err
		}
		return result, nil
	}
	fieldOfView, err := r.Resolve(dict["FOV"])
	if err != nil {
		return result, err
	}
	if fieldOfView == nil {
		return result, fmt.Errorf("missing 3D perspective field of view")
	}
	if err := r.threeDNumber(dict, "FOV", &result.FieldOfView); err != nil {
		return result, err
	}
	if result.FieldOfView < 0 || result.FieldOfView > 180 {
		return result, fmt.Errorf("invalid 3D perspective field of view")
	}
	scale, err := r.Resolve(dict["PS"])
	if err != nil || scale == nil {
		return result, err
	}
	if _, ok := scale.(Name); ok {
		if err := r.threeDName(dict, "PS", &result.PerspectiveBinding, "W", "H", "Min", "Max"); err != nil {
			return result, err
		}
	} else {
		if err := r.threeDNumber(dict, "PS", &result.PerspectiveDiameter); err != nil {
			return result, err
		}
		if result.PerspectiveDiameter <= 0 {
			return result, fmt.Errorf("invalid 3D perspective diameter")
		}
		result.PerspectiveBinding = ""
	}
	return result, nil
}

// ReadThreeDViewBox 按PDF标准表298解析以注解中心为原点的显示区域，不使用页面坐标
// 入参: annotation 3D注解
// 返回: Rectangle 显示区域, error 类型、范围或引用错误
func (r *Reader) ReadThreeDViewBox(annotation Annotation) (Rectangle, error) {
	if annotation.Subtype != "3D" {
		return Rectangle{}, fmt.Errorf("not a 3D annotation")
	}
	width := math.Abs(annotation.Rect.XMax - annotation.Rect.XMin)
	height := math.Abs(annotation.Rect.YMax - annotation.Rect.YMin)
	if math.IsNaN(width) || math.IsInf(width, 0) || math.IsNaN(height) || math.IsInf(height, 0) {
		return Rectangle{}, fmt.Errorf("invalid 3D annotation rectangle")
	}
	bounds := Rectangle{-width / 2, -height / 2, width / 2, height / 2}
	value, err := r.Resolve(annotation.Dictionary["3DB"])
	if err != nil || value == nil {
		return bounds, err
	}
	box, err := r.rectangle(value)
	if err != nil {
		return Rectangle{}, err
	}
	if box.XMin < bounds.XMin || box.YMin < bounds.YMin || box.XMax > bounds.XMax || box.YMax > bounds.YMax {
		return Rectangle{}, fmt.Errorf("3D view box outside annotation")
	}
	return box, nil
}

// threeDName 读取可选的3D枚举字段，空值保留调用方默认值
// 入参: dict 参数字典, key 字段名, target 目标值, allowed 允许的枚举
// 返回: error 类型、枚举或引用错误
func (r *Reader) threeDName(dict Dictionary, key Name, target *Name, allowed ...Name) error {
	value, err := r.Resolve(dict[key])
	if err != nil || value == nil {
		return err
	}
	name, ok := value.(Name)
	if !ok || !slices.Contains(allowed, name) {
		return fmt.Errorf("invalid 3D %s", key)
	}
	*target = name
	return nil
}

// threeDNumber 读取可选的3D有限数值字段，空值保留调用方默认值
// 入参: dict 参数字典, key 字段名, target 目标值
// 返回: error 数值或引用错误
func (r *Reader) threeDNumber(dict Dictionary, key Name, target *float64) error {
	value, err := r.Resolve(dict[key])
	if err != nil || value == nil {
		return err
	}
	number, err := r.number(value)
	if err != nil {
		return err
	}
	if math.IsNaN(number) || math.IsInf(number, 0) {
		return fmt.Errorf("invalid 3D %s", key)
	}
	*target = number
	return nil
}
