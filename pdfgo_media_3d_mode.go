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

import "fmt"

// ThreeDRenderMode 保存三维绘制参数，FaceColor为空时沿用视图背景色
// AnnotationBackground为真时改用宿主注解背景色，对应未知的面色名称
// Opacity为附加不透明度，CreaseAngle为轮廓夹角，不适用的字段保持默认值
type ThreeDRenderMode struct {
	Subtype              Name
	AuxiliaryColor       [3]float64
	FaceColor            *[3]float64
	AnnotationBackground bool
	Opacity              float64
	CreaseAngle          float64
	Dictionary           Dictionary
}

// ReadThreeDRenderMode 按PDF标准表307及表308读取绘制模式，仅解析生效字段
// 入参: object 绘制字典或引用，空值及未知模式忽略
// 返回: *ThreeDRenderMode 绘制参数, error 类型、数值或引用错误
func (r *Reader) ReadThreeDRenderMode(object Object) (*ThreeDRenderMode, error) {
	dict, err := r.mediaDictionary(object, true)
	if err != nil || dict == nil {
		return nil, err
	}
	value, err := r.Resolve(dict["Subtype"])
	if err != nil {
		return nil, err
	}
	name, ok := value.(Name)
	if !ok {
		return nil, fmt.Errorf("invalid 3D render mode subtype")
	}
	auxiliary, face, transparent, crease := false, false, false, false
	switch name {
	case "Solid", "ShadedWireframe", "ShadedVertices":
	case "SolidWireframe", "BoundingBox", "Wireframe", "HiddenWireframe", "Vertices":
		auxiliary = true
	case "Transparent":
		transparent = true
	case "TransparentWireframe":
		auxiliary, transparent = true, true
	case "TransparentBoundingBox":
		face, transparent = true, true
	case "TransparentBoundingBoxOutline":
		auxiliary, face, transparent = true, true, true
	case "Illustration":
		auxiliary, face, crease = true, true, true
	case "SolidOutline", "ShadedIllustration":
		auxiliary, crease = true, true
	default:
		return nil, nil
	}
	typeName := Name("3DRenderMode")
	if err := r.threeDName(dict, "Type", &typeName, "3DRenderMode"); err != nil {
		return nil, err
	}
	result := &ThreeDRenderMode{Subtype: name, Opacity: .5, CreaseAngle: 45, Dictionary: dict}
	if auxiliary {
		if err := r.threeDColor(dict["AC"], &result.AuxiliaryColor); err != nil {
			return nil, err
		}
	}
	if face {
		value, err := r.Resolve(dict["FC"])
		if err != nil {
			return nil, err
		}
		if name, ok := value.(Name); ok {
			result.AnnotationBackground = name != "BG"
		}
		if _, named := value.(Name); value != nil && !named {
			array, ok := value.(Array)
			if !ok || len(array) == 0 {
				return nil, fmt.Errorf("invalid 3D face color")
			}
			space, err := r.Resolve(array[0])
			if err != nil {
				return nil, err
			}
			if name, ok := space.(Name); ok && name == "DeviceRGB" {
				color := [3]float64{}
				if err := r.threeDColor(array, &color); err != nil {
					return nil, err
				}
				result.FaceColor = &color
			}
		}
	}
	if transparent {
		if err := r.threeDNumber(dict, "O", &result.Opacity); err != nil {
			return nil, err
		}
		if result.Opacity < 0 || result.Opacity > 1 {
			return nil, fmt.Errorf("invalid 3D render opacity")
		}
	}
	if crease {
		if err := r.threeDNumber(dict, "CV", &result.CreaseAngle); err != nil {
			return nil, err
		}
	}
	return result, nil
}
