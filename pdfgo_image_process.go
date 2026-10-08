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
	"image"
)

// ImageProcess 保存已校验的原生图像采样，源分量和色料定义在使用期间须保持只读
// 不复制像素缓冲，可并发采样，不执行备用变换或图像插值
type ImageProcess struct {
	source  *ImageComponents
	process *ColorantProcess
}

// DecodeProcess 只解码设备可直接使用的原生过程通道，不生成备用颜色缓冲
// 入参: device 输出设备, group 组混合空间，nil继承设备, softMask 是否用于软蒙版
// 返回: *ImageProcess 原生采样，需要备用色时为nil, error 解码或布局错误
func (i *Image) DecodeProcess(device *ColorantDevice, group *ColorSpace, softMask bool) (*ImageProcess, error) {
	return i.DecodeProcessContext(context.Background(), device, group, softMask)
}

// DecodeProcessContext 共用源样本和遮罩解码，保留16位浓度及源透明度
// 入参: ctx 解码取消上下文, device 输出设备, group 组混合空间，nil继承设备, softMask 是否用于软蒙版
// 返回: *ImageProcess 独立只读采样，需要备用色时为nil, error 解码或取消错误
func (i *Image) DecodeProcessContext(ctx context.Context, device *ColorantDevice, group *ColorSpace, softMask bool) (*ImageProcess, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	colorants, err := i.ColorantSpace()
	if err != nil {
		return nil, err
	}
	process := colorants.PrepareProcess(device, group, softMask)
	if process == nil || process.Mask() == ([4]bool{}) {
		return nil, nil
	}
	source := &ImageComponents{processOnly: true}
	if _, err := i.decodeImage(ctx, source, false); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if _, err := source.colorantSampleOffset(source.Rect.Min.X, source.Rect.Min.Y); err != nil {
		return nil, err
	}
	return &ImageProcess{source: source, process: process}, nil
}

// PrepareProcess 校验图像布局并编译原生过程通道，原始浓度与源透明度独立于备用色
// 入参: device 输出设备, group 组混合空间，nil继承设备, softMask 是否用于软蒙版
// 返回: *ImageProcess 原生采样，需备用色或独立专色时为nil, error 像素布局错误
func (i *ImageComponents) PrepareProcess(device *ColorantDevice, group *ColorSpace, softMask bool) (*ImageProcess, error) {
	if i == nil || i.ColorantSpace == nil {
		return nil, nil
	}
	process := i.ColorantSpace.PrepareProcess(device, group, softMask)
	if process == nil || process.Mask() == ([4]bool{}) {
		return nil, nil
	}
	if _, err := i.colorantSampleOffset(i.Rect.Min.X, i.Rect.Min.Y); err != nil {
		return nil, err
	}
	return &ImageProcess{source: i, process: process}, nil
}

// Mask 返回显式指定的过程通道，零浓度仍标记对应通道
// 返回: [4]bool 原生过程通道
func (p *ImageProcess) Mask() [4]bool {
	if p == nil {
		return [4]bool{}
	}
	return p.process.Mask()
}

// Bounds 返回原始采样边界，不将非零原点归零
// 返回: image.Rectangle 源边界，未初始化时为空
func (p *ImageProcess) Bounds() image.Rectangle {
	if p == nil || p.source == nil {
		return image.Rectangle{}
	}
	return p.source.Rect
}

// ValuesAt 从原始16位浓度直接取样，不分配浓度快照或逐像素解析颜色定义
// 入参: x 横坐标, y 纵坐标
// 返回: [4]float64 原生过程分量, float64 源透明度，无效采样或区域外返回透明
func (p *ImageProcess) ValuesAt(x, y int) ([4]float64, float64) {
	var values [4]float64
	if p == nil || p.source == nil || p.process == nil {
		return values, 0
	}
	i := p.source
	if x < i.Rect.Min.X || x >= i.Rect.Max.X || y < i.Rect.Min.Y || y >= i.Rect.Max.Y {
		return values, 0
	}
	offset := (y-i.Rect.Min.Y)*i.Rect.Dx() + x - i.Rect.Min.X
	for c, input := range p.process.inputs {
		if input >= 0 {
			values[c] = float64(i.Tints[offset*p.process.components+input]) / 65535
			if p.process.complement {
				values[c] = 1 - values[c]
			}
		}
	}
	if i.ColorantAlpha != nil {
		return values, float64(i.ColorantAlpha[offset]) / 65535
	}
	if i.processOnly {
		return values, 1
	}
	channels := i.Space.Components() + 1
	return values, float64(i.Pix[offset*channels+channels-1]) / 65535
}
