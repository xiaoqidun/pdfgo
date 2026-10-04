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
)

// ColorantSpace 保存不可变分色定义，Names与Process切片只读，不持有Reader
// NChannel保留独立过程空间和专色变换，普通DeviceN使用整体备用变换
type ColorantSpace struct {
	Names      []Name
	NChannel   bool
	Process    *ProcessColorants
	separation *separationSpace
	tint       *deviceNSpace
	spots      []*separationSpace
	lab        *labSpace
	none       bool
}

// ColorantPaint 保存只读源浓度快照，普通四分量内嵌保存，避免独立浓度缓冲分配
type ColorantPaint struct {
	Space  *ColorantSpace
	Tints  []float64
	values [4]float64
}

// ColorantDevice 保存输出设备的原生过程空间与额外色料，公开字段只读
// 原生过程空间仅接受设备色，额外色料仅用于减色设备，不代表屏幕备用颜色
type ColorantDevice struct {
	Space *ColorSpace
	Spots []Name
	index map[Name]int
}

// ColorantResult 分别保存设备过程色、设备专色与不可用色料的备用画刷
// ProcessMask与SpotMask区分未指定通道和显式零值，Spots按输出设备顺序保存
// Alternates不预先混合，调用方须按输出设备及透明组规则合成，不将近似RGB当作原生色料
type ColorantResult struct {
	Process     *Paint
	ProcessMask [4]bool
	Spots       []float64
	SpotMask    []bool
	Alternates  []Paint
}

// NewColorantDevice 建立只读输出设备定义，复制色料列表并拒绝保留名称和重复色料
// 入参: space 原生设备空间，nil使用设备RGB, spots 额外减色色料
// 返回: *ColorantDevice 输出设备, error 设备或色料错误
func NewColorantDevice(space *ColorSpace, spots []Name) (*ColorantDevice, error) {
	if space == nil {
		space = &ColorSpace{Model: "DeviceRGB"}
	}
	if !space.Device() || len(spots) != 0 && space.Model != "DeviceCMYK" {
		return nil, fmt.Errorf("invalid native colorant device")
	}
	device := &ColorantDevice{Space: space, Spots: append([]Name(nil), spots...), index: make(map[Name]int, len(spots))}
	for i, name := range spots {
		if _, exists := device.index[name]; exists || name == "" || name == "All" || name == "None" || nChannelCMYKComponent(name) >= 0 {
			return nil, fmt.Errorf("invalid device spot colorant %q", name)
		}
		device.index[name] = i
	}
	return device, nil
}

// CompositeOpaque 按不透明分色套印规则合成原生通道，显式零值仍标记命名通道
// 入参: out 输出缓冲，可与backdrop相同, backdrop 过程色及专色背景, source 已求值的原生色料, overprint 是否保留未标记通道
// 返回: error 分量错误或仍需合成备用色，错误时不修改输出
func (d *ColorantDevice) CompositeOpaque(out, backdrop []float64, source ColorantResult, overprint bool) error {
	components, marked, err := d.validateComposite(out, backdrop, source, 1, 1, false)
	if err != nil {
		return err
	}
	if !marked || overprint {
		copy(out, backdrop)
	} else {
		clear(out)
		if d.Space.Model != "DeviceCMYK" {
			for c := 0; c < components; c++ {
				out[c] = 1
			}
		}
	}
	for c := 0; c < components; c++ {
		if source.ProcessMask[c] {
			out[c] = source.Process.Values[c]
		}
	}
	for c, mask := range source.SpotMask {
		if mask {
			out[components+c] = source.Spots[c]
		}
	}
	return nil
}

// Composite 按PDF透明模型分别合成过程色和原生专色，未指定通道按无色料计算，不执行套印
// 入参: out 输出缓冲，可与backdrop相同, backdrop 非预乘过程色及专色背景, source 原生源色料, backdropAlpha 背景透明度, sourceAlpha 源透明度, mode 标准混合模式
// 返回: float64 结果透明度, error 定义错误或仍需合成备用色，错误时不修改输出
func (d *ColorantDevice) Composite(out, backdrop []float64, source ColorantResult, backdropAlpha, sourceAlpha float64, mode Name) (float64, error) {
	return d.composite(out, backdrop, source, backdropAlpha, sourceAlpha, mode, false)
}

// CompositeOverprint 按兼容套印和当前混合模式合成原生色料，源掩码须已按套印模式选择
// 入参: out 输出缓冲，可与backdrop相同, backdrop 非预乘背景, source 原生源色料, backdropAlpha 背景透明度, sourceAlpha 源透明度, mode 当前混合模式
// 返回: float64 结果透明度, error 定义错误或仍需合成备用色，错误时不修改输出
func (d *ColorantDevice) CompositeOverprint(out, backdrop []float64, source ColorantResult, backdropAlpha, sourceAlpha float64, mode Name) (float64, error) {
	return d.composite(out, backdrop, source, backdropAlpha, sourceAlpha, mode, true)
}

// composite 统一原生透明合成，兼容套印先计算未标记通道的隐式组贡献
// 入参: out 输出缓冲, backdrop 背景分量, source 原生源色料, backdropAlpha 背景透明度, sourceAlpha 源透明度, mode 混合模式, overprint 是否兼容套印
// 返回: float64 结果透明度, error 定义或数值错误
func (d *ColorantDevice) composite(out, backdrop []float64, source ColorantResult, backdropAlpha, sourceAlpha float64, mode Name, overprint bool) (float64, error) {
	if d == nil || d.Space == nil || !d.Space.Device() {
		return 0, fmt.Errorf("invalid native colorant device")
	}
	group := ColorantGroup{Space: d.Space, Device: d}
	return group.composite(out, backdrop, source, backdropAlpha, sourceAlpha, mode, overprint)
}

// validateComposite 校验输出缓冲和原生通道，零透明度不读取未定义颜色
// 入参: out 输出缓冲, backdrop 背景分量, source 源色料, backdropAlpha 背景透明度, sourceAlpha 源透明度, overprint 是否忽略未标记源分量
// 返回: int 过程分量数, bool 是否指定色料, error 定义或数值错误
func (d *ColorantDevice) validateComposite(out, backdrop []float64, source ColorantResult, backdropAlpha, sourceAlpha float64, overprint bool) (int, bool, error) {
	if d == nil || d.Space == nil || !d.Space.Device() {
		return 0, false, fmt.Errorf("invalid native colorant device")
	}
	group := ColorantGroup{Space: d.Space, Device: d}
	return group.validateComposite(out, backdrop, source, backdropAlpha, sourceAlpha, overprint)
}

// ReadColorantSpace 读取分色、多色或索引基础空间，解析资源别名及实际选用的默认备用色
// 入参: object 源颜色空间, resources 当前资源字典
// 返回: *ColorantSpace 只读色料定义，普通设备色为nil, error 定义错误
func (r *Reader) ReadColorantSpace(object Object, resources Dictionary) (*ColorantSpace, error) {
	object, err := r.resourceColorSpace(object, resources)
	if err != nil {
		return nil, err
	}
	return r.readColorantSpace(object, true)
}

// readColorantSpace 编译已解析的色料或索引基础空间，不重复替换默认色
// 入参: object 颜色空间, effective 是否已按资源校验并替换
// 返回: *ColorantSpace 色料定义，非色料空间为nil, error 定义错误
func (r *Reader) readColorantSpace(object Object, effective bool) (*ColorantSpace, error) {
	return r.readColorantDefinition(object, effective, 0)
}

// readColorantDefinition 限制索引基础空间的递归深度，不重复展开源定义
// 入参: object 颜色空间, effective 是否已按资源校验并替换, depth 索引嵌套深度
// 返回: *ColorantSpace 色料定义，非色料空间为nil, error 定义错误
func (r *Reader) readColorantDefinition(object Object, effective bool, depth int) (*ColorantSpace, error) {
	if depth >= 64 {
		return nil, fmt.Errorf("color space recursion limit exceeded")
	}
	object, err := r.resolveColorSpace(object)
	if err != nil {
		return nil, err
	}
	array, ok := object.(Array)
	if !ok || len(array) == 0 {
		return nil, nil
	}
	if array[0] == Name("Indexed") && len(array) == 4 {
		return r.readColorantDefinition(array[1], effective, depth+1)
	}
	if array[0] == Name("Separation") {
		separation, err := r.readSeparationSpace(array, effective, depth)
		if err != nil {
			return nil, err
		}
		return separation.colorants, nil
	}
	if array[0] == Name("DeviceN") {
		tint, err := r.readDeviceNSpace(array, effective, depth)
		if err != nil {
			return nil, err
		}
		return tint.colorants, nil
	}
	return nil, nil
}

// Resolve 按设备能力选择原生色料或备用画刷，NChannel逐分量处理而DeviceN整体回退
// 入参: tints 源浓度, device 输出设备，nil使用设备RGB, intent 渲染意图, softMask 是否用于软蒙版组
// 返回: ColorantResult 独立过程色、专色和备用色, error 浓度或颜色变换错误
func (s *ColorantSpace) Resolve(tints []float64, device *ColorantDevice, intent Name, softMask bool) (ColorantResult, error) {
	return s.ResolveInGroup(tints, device, nil, intent, softMask)
}

// ResolveInGroup 在指定组空间求值过程色，设备专色独立保留，软蒙版只使用备用色
// 入参: tints 源浓度, device 输出设备，nil使用设备RGB, group 组混合空间，nil继承设备, intent 渲染意图, softMask 是否用于软蒙版组
// 返回: ColorantResult 组过程色、设备专色与备用色, error 浓度或颜色变换错误
func (s *ColorantSpace) ResolveInGroup(tints []float64, device *ColorantDevice, group *ColorSpace, intent Name, softMask bool) (ColorantResult, error) {
	var result ColorantResult
	if s == nil || len(tints) != len(s.Names) || s.separation == nil && s.tint == nil {
		return result, fmt.Errorf("invalid colorant component count")
	}
	for _, value := range tints {
		if math.IsNaN(value) || math.IsInf(value, 0) {
			return result, fmt.Errorf("invalid colorant tint")
		}
	}
	if device == nil {
		device = &ColorantDevice{Space: &ColorSpace{Model: "DeviceRGB"}}
	}
	if device.Space == nil || !device.Space.Device() {
		return result, fmt.Errorf("invalid native colorant device")
	}
	output := device.Space
	if group != nil {
		if group.Components() == 0 {
			return result, fmt.Errorf("invalid colorant blending space")
		}
		output = group
	}
	process := output.Device() && output.Equal(device.Space)
	if s.separation != nil {
		name := s.Names[0]
		if name == "None" {
			return result, nil
		}
		if name == "All" {
			value := math.Max(0, math.Min(1, tints[0]))
			values := [4]float64{}
			for c := 0; c < output.Components(); c++ {
				values[c], result.ProcessMask[c] = value, true
				if output.Model != "DeviceCMYK" {
					values[c] = 1 - value
				}
			}
			if !process {
				var err error
				values, err = output.Convert([]float64{1 - value}, &ColorSpace{Model: "DeviceGray"}, intent)
				if err != nil {
					return ColorantResult{}, err
				}
			}
			paint, err := colorantProcessPaint(values, output, intent)
			if err != nil {
				return ColorantResult{}, err
			}
			result.Process = &paint
			if !softMask {
				result.Spots, result.SpotMask = make([]float64, len(device.Spots)), make([]bool, len(device.Spots))
				for i := range result.Spots {
					result.Spots[i], result.SpotMask[i] = value, true
				}
			}
			return result, nil
		}
		if !softMask && device.available(name, process) {
			return device.native(s.Names, tints, intent)
		}
		paint, err := s.separation.alternatePaint(tints[0], intent)
		if err != nil {
			return result, err
		}
		if !paint.None {
			paint.Alpha = 1
			result.Alternates = []Paint{paint}
		}
		return result, nil
	}
	if !s.NChannel {
		available, any := !softMask, false
		for _, name := range s.Names {
			if name != "None" {
				available, any = available && device.available(name, process), true
			}
		}
		if !any {
			return result, nil
		}
		if available {
			return device.native(s.Names, tints, intent)
		}
		paint, err := s.tint.alternatePaint(tints, intent)
		if err != nil {
			return result, err
		}
		if !paint.None {
			paint.Alpha = 1
			result.Alternates = []Paint{paint}
		}
		return result, nil
	}
	if s.Process != nil {
		values, mask := [4]float64{}, [4]bool{}
		for i, channel := range s.Process.Channels {
			if channel >= 0 {
				values[channel], mask[channel] = math.Max(0, math.Min(1, tints[i])), true
			}
		}
		if mask != ([4]bool{}) {
			space := s.Process.Space
			if s.lab != nil {
				color := s.lab.color(values[0], values[1], values[2])
				values = [4]float64{float64(color.R) / 65535, float64(color.G) / 65535, float64(color.B) / 65535}
			} else if space.profile != nil && space.profile.ranges != nil {
				values = space.profile.normalize(values[:space.Components()])
			}
			converted, err := output.Convert(values[:space.Components()], space, intent)
			if err != nil {
				return ColorantResult{}, err
			}
			if !output.Equal(space) || softMask {
				mask = [4]bool{}
				for c := 0; c < output.Components(); c++ {
					mask[c] = true
				}
			}
			paint, err := colorantProcessPaint(converted, output, intent)
			if err != nil {
				return ColorantResult{}, err
			}
			result.Process, result.ProcessMask = &paint, mask
		}
	}
	for i, name := range s.Names {
		if s.Process != nil && s.Process.Channels[i] >= 0 {
			continue
		}
		if !softMask && device.available(name, process) {
			if result.Spots == nil {
				result.Spots, result.SpotMask = make([]float64, len(device.Spots)), make([]bool, len(device.Spots))
			}
			index := device.index[name]
			result.Spots[index], result.SpotMask[index] = math.Max(0, math.Min(1, tints[i])), true
			continue
		}
		paint, err := s.spots[i].alternatePaint(tints[i], intent)
		if err != nil {
			return ColorantResult{}, err
		}
		if !paint.None {
			paint.Alpha = 1
			result.Alternates = append(result.Alternates, paint)
		}
	}
	return result, nil
}

// available 判断色料是否可在当前组使用，非原生组不直接访问设备四色
// 入参: name 源色料名, process 是否可访问原生过程通道
// 返回: bool 是否可直接绘制
func (d *ColorantDevice) available(name Name, process bool) bool {
	if nChannelCMYKComponent(name) >= 0 {
		return process && d.Space.Model == "DeviceCMYK"
	}
	_, ok := d.index[name]
	return ok
}

// nativeAvailable 判断完全不可见的备用色是否仍有可用的设备色料，不求值备用函数
// 入参: device 输出设备, group 组混合空间
// 返回: bool 是否仍有原生输出
func (s *ColorantSpace) nativeAvailable(device *ColorantDevice, group *ColorSpace) bool {
	if s == nil || s.none || device == nil || device.Space == nil || !device.Space.Device() {
		return false
	}
	process := group == nil || group.Device() && group.Equal(device.Space)
	if s.separation != nil {
		return s.Names[0] == "All" || device.available(s.Names[0], process)
	}
	if s.NChannel {
		if s.Process != nil {
			for _, channel := range s.Process.Channels {
				if channel >= 0 {
					return true
				}
			}
		}
		for _, name := range s.Names {
			if device.available(name, process) {
				return true
			}
		}
		return false
	}
	any := false
	for _, name := range s.Names {
		if name != "None" {
			if !device.available(name, process) {
				return false
			}
			any = true
		}
	}
	return any
}

// colorSpaceVisible 区分屏幕备用色不可见与设备原生色料仍可输出，不解码被丢弃的图像
// 入参: object 已按资源映射的颜色空间
// 返回: bool 是否有输出, error 空间定义错误
func (p *pageInterpreter) colorSpaceVisible(object Object) (bool, error) {
	none, err := p.reader.colorSpaceNone(object, true)
	if err != nil || !none {
		return !none, err
	}
	if p.visitor.ColorantDevice == nil || p.maskGroup != nil {
		return false, nil
	}
	space, err := p.reader.readColorantSpace(object, true)
	return space.nativeAvailable(p.visitor.ColorantDevice, p.blendingSpace), err
}

// paintVisible 保留原生色料可用的画刷，None源色料仍不参与裁剪外的着色
// 入参: paint 当前画刷
// 返回: bool 是否有备用色或原生色料输出
func (p *pageInterpreter) paintVisible(paint Paint) bool {
	return !paint.None || p.maskGroup == nil && paint.Colorant != nil && paint.Colorant.Space.nativeAvailable(p.visitor.ColorantDevice, p.blendingSpace)
}

// native 映射已确认可用的源色料，None不标记设备通道
// 入参: names 源色料名, tints 源浓度, intent 渲染意图
// 返回: ColorantResult 原生通道, error 颜色变换错误
func (d *ColorantDevice) native(names []Name, tints []float64, intent Name) (ColorantResult, error) {
	result, values := ColorantResult{}, [4]float64{}
	for i, name := range names {
		if name == "None" {
			continue
		}
		value := math.Max(0, math.Min(1, tints[i]))
		if channel := nChannelCMYKComponent(name); channel >= 0 {
			values[channel], result.ProcessMask[channel] = value, true
		} else {
			if result.Spots == nil {
				result.Spots, result.SpotMask = make([]float64, len(d.Spots)), make([]bool, len(d.Spots))
			}
			index := d.index[name]
			result.Spots[index], result.SpotMask[index] = value, true
		}
	}
	if result.ProcessMask != ([4]bool{}) {
		paint, err := colorantProcessPaint(values, d.Space, intent)
		if err != nil {
			return ColorantResult{}, err
		}
		result.Process = &paint
	}
	return result, nil
}

// colorantProcessPaint 建立原生过程画刷，保留单位分量与设备四色
// 入参: values 过程分量, space 过程空间, intent 渲染意图
// 返回: Paint 原生画刷, error 颜色变换错误
func colorantProcessPaint(values [4]float64, space *ColorSpace, intent Name) (Paint, error) {
	rgb, err := space.RGB(values[:space.Components()], intent)
	if err != nil {
		return Paint{}, err
	}
	paint := Paint{SourceSpace: space.Model, Space: space, Values: values, RGB: rgb, Alpha: 1}
	if space.Device() && space.Model == "DeviceCMYK" {
		paint.CMYK = &values
	}
	return paint, nil
}

// newColorantPaint 复制已裁切的源浓度，不使用备用函数区间替代源浓度
// 入参: space 色料定义, tints 源浓度
// 返回: *ColorantPaint 独立浓度快照
func newColorantPaint(space *ColorantSpace, tints []float64) *ColorantPaint {
	if space == nil {
		return nil
	}
	result := &ColorantPaint{Space: space}
	if len(tints) <= len(result.values) {
		result.Tints = result.values[:len(tints)]
	} else {
		result.Tints = make([]float64, len(tints))
	}
	for i, value := range tints {
		result.Tints[i] = math.Max(0, math.Min(1, value))
	}
	return result
}
