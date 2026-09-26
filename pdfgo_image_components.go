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

import "image"

// ImageComponents 保存应用Decode、索引映射及遮罩后的16位非预乘颜色分量
// Pix逐行交错保存Space.Components()个颜色分量和一个透明度分量
// 模板图像保留灰度样本，填充颜色及反向覆盖由调用方应用
type ImageComponents struct {
	Space *ColorSpace
	Rect  image.Rectangle
	Pix   []uint16
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
