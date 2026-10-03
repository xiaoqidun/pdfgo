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
	"encoding/binary"
	"fmt"
	"math"
	"os"
)

// Halftone 保存设备空间半色调定义，连续色调显示不需要网屏化
// Thresholds保持标准中的数组顺序，类型16使用高位优先的16位阈值
type Halftone struct {
	Type                           int
	Name                           []byte
	Dictionary                     Dictionary
	Frequency, Angle               float64
	SpotFunction                   Object
	TransferFunction               Object
	AccurateScreens                bool
	Width, Height, Width2, Height2 int
	Xsquare, Ysquare               int
	Thresholds                     []uint16
	Components                     map[Name]*Halftone
	reader                         *Reader
}

// ReadHalftone 读取类型1、5、6、10和16的网屏定义，Default返回空值
// 入参: object 半色调字典、数据流、Default或间接引用
// 返回: *Halftone 网屏参数, error 类型、尺寸或阈值数据错误
func (r *Reader) ReadHalftone(object Object) (*Halftone, error) {
	ref, indirect := object.(Reference)
	if indirect && !r.closed {
		if cached, ok := r.halftones[ref]; ok {
			return cached, nil
		}
	}
	h, err := r.readHalftone(object, false, nil, nil)
	if err == nil && indirect {
		if r.halftones == nil {
			r.halftones = make(map[Reference]*Halftone)
		}
		r.halftones[ref] = h
	}
	return h, err
}

// ReadHalftoneForDevice 优先选择设备命名网屏，未匹配时解析文件中的备用定义
// 入参: object 网屏对象, named 只读设备网屏，匹配后不解析其他字段或阈值流
// 返回: *Halftone 有效网屏, error 名称或备用定义错误
func (r *Reader) ReadHalftoneForDevice(object Object, named map[string]*Halftone) (*Halftone, error) {
	if len(named) == 0 {
		return r.ReadHalftone(object)
	}
	ref, indirect := object.(Reference)
	if indirect && !r.closed {
		if cached, ok := r.halftones[ref]; ok && !cached.namedOverride(named) {
			return cached, nil
		}
	}
	used := false
	h, err := r.readHalftone(object, false, named, &used)
	if err == nil && indirect && !used {
		if r.halftones == nil {
			r.halftones = make(map[Reference]*Halftone)
		}
		r.halftones[ref] = h
	}
	return h, err
}

// namedOverride 检查缓存定义及其单层分量是否被设备名称覆盖
// 入参: named 只读设备命名网屏
// 返回: bool 是否存在名称匹配
func (h *Halftone) namedOverride(named map[string]*Halftone) bool {
	if h == nil {
		return false
	}
	if h.Name != nil && named[string(h.Name)] != nil {
		return true
	}
	for _, part := range h.Components {
		if part != nil && part.Name != nil && named[string(part.Name)] != nil {
			return true
		}
	}
	return false
}

// readHalftone 读取单色或分量网屏，禁止嵌套类型5
// 入参: object 网屏对象, component 是否为分量网屏, named 只读设备命名网屏, used 记录名称匹配
// 返回: *Halftone 网屏参数, error 定义错误
func (r *Reader) readHalftone(object Object, component bool, named map[string]*Halftone, used *bool) (*Halftone, error) {
	value, err := r.Resolve(object)
	if err != nil {
		return nil, err
	}
	if value == Name("Default") && !component {
		return nil, nil
	}
	dict, ok := value.(Dictionary)
	stream, isStream := value.(*Stream)
	if isStream && stream != nil {
		dict, ok = stream.Dictionary, true
	}
	if !ok {
		return nil, fmt.Errorf("invalid halftone dictionary")
	}
	value, err = r.Resolve(dict["HalftoneName"])
	if err != nil {
		return nil, err
	}
	var name []byte
	if value != nil {
		raw, ok := value.(String)
		if !ok {
			return nil, fmt.Errorf("invalid halftone name")
		}
		if device := named[string(raw)]; device != nil {
			if component && device.Type == 5 {
				return nil, fmt.Errorf("invalid component halftone type")
			}
			*used = true
			return device, nil
		}
		name = append([]byte{}, raw...)
	}
	h := &Halftone{Dictionary: dict, Name: name, reader: r}
	value, err = r.Resolve(dict["HalftoneType"])
	if err != nil {
		return nil, err
	}
	kind, ok := value.(Integer)
	if !ok && !(value == nil && h.Name != nil) {
		return nil, fmt.Errorf("invalid halftone type")
	}
	h.Type = int(kind)
	value, err = r.Resolve(dict["Type"])
	if err != nil {
		return nil, err
	}
	if value != nil && value != Name("Halftone") {
		return nil, fmt.Errorf("invalid halftone object type")
	}
	if !ok {
		return h, nil
	}
	if kind == 5 {
		if component || isStream {
			return nil, fmt.Errorf("invalid component halftone type")
		}
		h.Components = make(map[Name]*Halftone)
		for key, object := range dict {
			if key == "Type" || key == "HalftoneType" || key == "HalftoneName" {
				continue
			}
			part, err := r.readHalftone(object, true, named, used)
			if err != nil {
				return nil, err
			}
			h.Components[key] = part
		}
		if h.Components["Default"] == nil {
			return nil, fmt.Errorf("missing default component halftone")
		}
		return h, nil
	}
	h.TransferFunction, err = r.Resolve(dict["TransferFunction"])
	if err != nil {
		return nil, err
	}
	if kind == 1 {
		if isStream {
			return nil, fmt.Errorf("invalid spot halftone stream")
		}
		h.Frequency, err = r.number(dict["Frequency"])
		if err != nil || h.Frequency <= 0 || math.IsInf(h.Frequency, 0) || math.IsNaN(h.Frequency) {
			return nil, fmt.Errorf("invalid screen frequency")
		}
		h.Angle, err = r.number(dict["Angle"])
		if err != nil || math.IsInf(h.Angle, 0) || math.IsNaN(h.Angle) {
			return nil, fmt.Errorf("invalid screen angle")
		}
		h.SpotFunction, err = r.Resolve(dict["SpotFunction"])
		if err != nil {
			return nil, err
		}
		switch h.SpotFunction.(type) {
		case Name, Dictionary, *Stream:
		default:
			return nil, fmt.Errorf("invalid spot function")
		}
		value, err = r.Resolve(dict["AccurateScreens"])
		if err != nil {
			return nil, err
		}
		if value != nil {
			flag, ok := value.(Boolean)
			if !ok {
				return nil, fmt.Errorf("invalid accurate screens flag")
			}
			h.AccurateScreens = bool(flag)
		}
		return h, nil
	}
	if kind != 6 && kind != 10 && kind != 16 {
		return nil, &UnsupportedError{Feature: fmt.Sprintf("halftone type %d", kind)}
	}
	if !isStream {
		return nil, fmt.Errorf("missing halftone threshold stream")
	}
	fields := []struct {
		key    Name
		target *int
	}{{"Width", &h.Width}, {"Height", &h.Height}}
	if kind == 10 {
		fields = []struct {
			key    Name
			target *int
		}{{"Xsquare", &h.Xsquare}, {"Ysquare", &h.Ysquare}}
	}
	if kind == 16 && (dict["Width2"] != nil || dict["Height2"] != nil) {
		fields = append(fields, struct {
			key    Name
			target *int
		}{"Width2", &h.Width2}, struct {
			key    Name
			target *int
		}{"Height2", &h.Height2})
	}
	for _, field := range fields {
		value, err := r.Resolve(dict[field.key])
		if err != nil {
			return nil, err
		}
		n, ok := value.(Integer)
		if !ok || n <= 0 || uint64(n) > uint64(^uint(0)>>1) {
			return nil, fmt.Errorf("invalid halftone %s", field.key)
		}
		*field.target = int(n)
	}
	data, err := stream.Decode()
	if err != nil {
		return nil, err
	}
	width := uint64(1)
	if kind == 16 {
		width = 2
	}
	count := uint64(h.Width)*uint64(h.Height) + uint64(h.Width2)*uint64(h.Height2)
	if kind == 10 {
		count = uint64(h.Xsquare)*uint64(h.Xsquare) + uint64(h.Ysquare)*uint64(h.Ysquare)
	}
	limit := uint64(len(data)) / width
	for _, pair := range [][2]int{{h.Width, h.Height}, {h.Width2, h.Height2}, {h.Xsquare, h.Xsquare}, {h.Ysquare, h.Ysquare}} {
		if pair[0] != 0 && uint64(pair[1]) > limit/uint64(pair[0]) {
			return nil, fmt.Errorf("truncated halftone thresholds")
		}
	}
	if count != limit || uint64(len(data))%width != 0 {
		return nil, fmt.Errorf("invalid halftone threshold length")
	}
	h.Thresholds = make([]uint16, int(count))
	for i := range h.Thresholds {
		if kind == 16 {
			h.Thresholds[i] = binary.BigEndian.Uint16(data[2*i:])
		} else {
			h.Thresholds[i] = uint16(data[i]) * 257
		}
	}
	return h, nil
}

// IdentityTransfer 判断网屏中是否仅使用恒等传递函数，分量网屏逐项检查
// 入参: reader 无所属阅读器时使用的PDF阅读器
// 返回: bool 是否恒等, error 函数解析错误
func (h *Halftone) IdentityTransfer(reader *Reader) (bool, error) {
	return h.identityTransfer(reader, 0)
}

// identityTransfer 按所属阅读器检查网屏函数，限制手工定义的递归深度
// 入参: reader 备用阅读器, depth 嵌套深度
// 返回: bool 是否恒等, error 引用或递归错误
func (h *Halftone) identityTransfer(reader *Reader, depth int) (bool, error) {
	if h == nil {
		return true, nil
	}
	if depth >= 32 {
		return false, fmt.Errorf("halftone recursion limit exceeded")
	}
	if h.reader != nil {
		reader = h.reader
	}
	if reader == nil {
		return false, fmt.Errorf("missing halftone reader")
	}
	if reader.closed {
		return false, os.ErrClosed
	}
	if h.TransferFunction != nil {
		function, _, err := reader.halftoneTransfer(h)
		if err != nil {
			return false, err
		}
		if function != nil {
			return false, nil
		}
	}
	for _, part := range h.Components {
		identity, err := part.identityTransfer(reader, depth+1)
		if err != nil || !identity {
			return identity, err
		}
	}
	return true, nil
}

// component 选择原生分量网屏，缺省使用Default且禁止嵌套类型5
// 入参: colorant 原生分量名称
// 返回: *Halftone 分量网屏, error 定义错误
func (h *Halftone) component(colorant Name) (*Halftone, error) {
	if h == nil || h.Type != 5 {
		return h, nil
	}
	if h.Components["Default"] == nil || h.Components["Default"].Type == 5 {
		return nil, fmt.Errorf("invalid default halftone component")
	}
	part := h.Components[colorant]
	if part == nil {
		part = h.Components["Default"]
	}
	if part == nil || part.Type == 5 {
		return nil, fmt.Errorf("invalid halftone component")
	}
	return part, nil
}
