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
	"image"
)

// ImageComponents 保存应用Decode、索引映射及遮罩后的16位非预乘颜色分量
// Pix逐行交错保存Space.Components()个颜色分量和一个透明度分量
// 模板图像保留灰度样本，填充颜色及反向覆盖由调用方应用
// Colorants和Tints保留专色名称及逐像素浓度，供支持相应色料的输出设备使用
type ImageComponents struct {
	Space     *ColorSpace
	Rect      image.Rectangle
	Pix       []uint16
	Colorants []Name
	Tints     []uint16
}

// Colorants 读取图像或索引基础空间的专色名称，设备色返回空列表
// 返回: []Name 色料名称, error 颜色空间格式错误
func (i *Image) Colorants() ([]Name, error) {
	object := i.ColorSpace
	if a, ok := object.(Array); ok && len(a) == 4 && a[0] == Name("Indexed") {
		var err error
		object, err = i.reader.resolveColorSpace(a[1])
		if err != nil {
			return nil, err
		}
	}
	a, ok := object.(Array)
	if !ok || len(a) == 0 {
		return nil, nil
	}
	if a[0] == Name("Separation") && len(a) == 4 {
		value, err := i.reader.Resolve(a[1])
		if err != nil {
			return nil, err
		}
		name, ok := value.(Name)
		if !ok {
			return nil, fmt.Errorf("invalid Separation colorant")
		}
		return []Name{name}, nil
	}
	if a[0] != Name("DeviceN") {
		return nil, nil
	}
	if len(a) != 4 && len(a) != 5 {
		return nil, fmt.Errorf("invalid DeviceN color space")
	}
	object, err := i.reader.Resolve(a[1])
	if err != nil {
		return nil, err
	}
	names, ok := object.(Array)
	if !ok || len(names) == 0 {
		return nil, fmt.Errorf("invalid DeviceN colorants")
	}
	result := make([]Name, len(names))
	for n, v := range names {
		v, err = i.reader.Resolve(v)
		if err != nil {
			return nil, err
		}
		name, ok := v.(Name)
		if !ok {
			return nil, fmt.Errorf("invalid DeviceN colorant")
		}
		result[n] = name
	}
	return result, nil
}

// TintsAt 返回原始专色的已映射浓度，区域外或无专色时返回nil
// 入参: x 横坐标, y 纵坐标
// 返回: []uint16 非预乘浓度，返回切片只读
func (i *ImageComponents) TintsAt(x, y int) []uint16 {
	if len(i.Colorants) == 0 || !(image.Point{X: x, Y: y}).In(i.Rect) {
		return nil
	}
	offset := ((y-i.Rect.Min.Y)*i.Rect.Dx() + x - i.Rect.Min.X) * len(i.Colorants)
	return i.Tints[offset : offset+len(i.Colorants)]
}

// ValuesAt 返回像素的单位颜色分量及透明度，区域外返回透明值
// 入参: x 横坐标, y 纵坐标
// 返回: [4]float64 非预乘颜色, float64 透明度
func (i *ImageComponents) ValuesAt(x, y int) ([4]float64, float64) {
	var values [4]float64
	if !(image.Point{X: x, Y: y}).In(i.Rect) {
		return values, 0
	}
	channels := i.Space.Components()
	offset := ((y-i.Rect.Min.Y)*i.Rect.Dx() + x - i.Rect.Min.X) * (channels + 1)
	for c := 0; c < channels; c++ {
		values[c] = float64(i.Pix[offset+c]) / 65535
	}
	return values, float64(i.Pix[offset+channels]) / 65535
}

// DecodeComponents 读取原始颜色空间的已映射分量，不经sRGB往返转换
// 返回: *ImageComponents 颜色分量及透明度, error 解码或不支持的源空间错误
func (i *Image) DecodeComponents() (*ImageComponents, error) {
	result := &ImageComponents{}
	if _, err := i.decodeImage(result); err != nil {
		return nil, err
	}
	return result, nil
}
