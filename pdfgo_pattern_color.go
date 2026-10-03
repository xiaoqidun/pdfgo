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

import "math"

// patternColorSpace 保存无色图案的底层分量数和颜色变换
type patternColorSpace struct {
	components int
	convert    func([]float64, Name) (Paint, error)
	invisible  bool
	blending   *ColorSpace
}

// readPatternBase 读取无色图案的设备、校准、索引或专色空间
// 入参: object 底层颜色空间
// 返回: *patternColorSpace 颜色变换, error 解析错误
func (r *Reader) readPatternBase(object Object) (*patternColorSpace, error) {
	return r.readPatternColorSpace(object, false)
}

// readPatternColorSpace 读取原始或已按资源校验的图案基础空间
// 入参: object 基础空间, effective 是否已按资源校验并替换
// 返回: *patternColorSpace 分量数及颜色变换, error 颜色空间错误
func (r *Reader) readPatternColorSpace(object Object, effective bool) (*patternColorSpace, error) {
	object, err := r.resolveColorSpace(object)
	if err != nil {
		return nil, err
	}
	if array, ok := object.(Array); ok && len(array) > 0 {
		switch array[0] {
		case Name("Separation"):
			space, err := r.readSeparationSpace(array, effective, 0)
			if err != nil {
				return nil, err
			}
			return &patternColorSpace{1, func(values []float64, intent Name) (Paint, error) {
				paint, err := space.paint(values[0], intent)
				paint.Alpha = 1
				return paint, err
			}, space.name == "None" || space.none, nil}, nil
		case Name("DeviceN"):
			space, err := r.readDeviceNSpace(array, effective, 0)
			if err != nil {
				return nil, err
			}
			return &patternColorSpace{space.components, func(values []float64, intent Name) (Paint, error) {
				paint, err := space.paint(values, intent)
				paint.Alpha = 1
				return paint, err
			}, space.none, nil}, nil
		case Name("Lab"):
			lab, err := r.readLab(array)
			if err != nil {
				return nil, err
			}
			space := &graphicsColorSpace{lab: lab, displaySpace: ColorSpace{Model: "DeviceRGB", mapped: true}}
			return &patternColorSpace{3, func(values []float64, _ Name) (Paint, error) {
				paint, err := space.paint(values)
				paint.Alpha = 1
				return paint, err
			}, false, nil}, nil
		case Name("Indexed"):
			image := &Image{reader: r, ColorSpace: array, Stream: &Stream{Dictionary: Dictionary{}}, effectiveColorSpace: effective}
			palette, err := image.palette()
			if err != nil {
				return nil, err
			}
			return &patternColorSpace{1, func(values []float64, intent Name) (Paint, error) {
				index := int(math.Round(math.Max(0, math.Min(float64(len(palette.colors)-1), values[0]))))
				color := palette.colors[index]
				paint := Paint{SourceSpace: "Indexed", RGB: [3]float64{float64(color.R) / 65535, float64(color.G) / 65535, float64(color.B) / 65535}, Alpha: float64(color.A) / 65535, None: color.A == 0, Space: palette.space, Values: palette.values[index], Process: palette.process}
				if palette.colorants != nil {
					paint.Colorant = newColorantPaint(palette.colorants, palette.tints[index])
				}
				if palette.space != nil {
					var err error
					paint.RGB, err = palette.space.RGB(paint.Values[:palette.space.Components()], intent)
					return paint, err
				}
				return paint, nil
			}, palette.colors[0].A == 0, nil}, nil
		}
	}
	space, err := r.readEffectiveColorSpace(object, effective)
	if err != nil {
		return nil, err
	}
	source := space.Model
	if array, ok := object.(Array); ok && len(array) != 0 {
		source, _ = array[0].(Name)
	}
	return &patternColorSpace{space.Components(), func(values []float64, intent Name) (Paint, error) {
		if space.profile != nil && space.profile.alternate != nil {
			paint, err := space.profile.paint(values, intent)
			paint.Alpha = 1
			return paint, err
		}
		paint := Paint{SourceSpace: source, Space: space, Alpha: 1}
		if space.profile != nil {
			paint.Values = space.profile.normalize(values)
		} else {
			for i, value := range values {
				paint.Values[i] = math.Max(0, math.Min(1, value))
			}
		}
		var err error
		paint.RGB, err = space.RGB(paint.Values[:space.Components()], intent)
		if space.Model == "DeviceCMYK" && !space.Calibrated() {
			values := paint.Values
			paint.CMYK = &values
		}
		return paint, err
	}, space.profile != nil && space.profile.alternate != nil && space.profile.alternate.invisible, space}, nil
}
