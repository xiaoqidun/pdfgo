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
	"math"
)

// Rectangle 表示PDF用户空间中的矩形坐标
type Rectangle struct {
	XMin, YMin, XMax, YMax float64
}

// Page 保存页面字典及继承后生效的页面属性
type Page struct {
	Reference  Reference
	Dictionary Dictionary
	Resources  Dictionary
	MediaBox   Rectangle
	CropBox    Rectangle
	Rotation   int
	UserUnit   float64
	reader     *Reader
}

// pageAttributes 保存页面树可继承的四项属性
type pageAttributes struct {
	resources Object
	mediaBox  Object
	cropBox   Object
	rotation  Object
}

// WalkPages 按页面顺序访问页面，不执行动作或访问外部资源
// 入参: ctx 取消上下文, visit 页面访问函数
// 返回: error 错误信息
func (r *Reader) WalkPages(ctx context.Context, visit func(int, *Page) error) error {
	root, err := r.Resolve(r.Trailer["Root"])
	if err != nil {
		return err
	}
	catalog, ok := root.(Dictionary)
	if !ok {
		return fmt.Errorf("invalid catalog")
	}
	seen := map[Reference]bool{}
	index := 0
	var walk func(Object, pageAttributes, int) error
	walk = func(object Object, inherited pageAttributes, depth int) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if depth > 256 {
			return fmt.Errorf("page tree depth exceeded")
		}
		ref, ok := object.(Reference)
		if !ok {
			return fmt.Errorf("page tree node is not indirect")
		}
		if seen[ref] {
			return fmt.Errorf("repeated page tree node")
		}
		seen[ref] = true
		resolved, err := r.Resolve(ref)
		if err != nil {
			return err
		}
		dict, ok := resolved.(Dictionary)
		if !ok {
			return fmt.Errorf("invalid page tree node")
		}
		attrs := inherited
		for _, attr := range [...]struct {
			key   Name
			value *Object
		}{{"Resources", &attrs.resources}, {"MediaBox", &attrs.mediaBox}, {"CropBox", &attrs.cropBox}, {"Rotate", &attrs.rotation}} {
			value, err := r.Resolve(dict[attr.key])
			if err != nil {
				return err
			}
			if value != nil {
				*attr.value = value
			}
		}
		switch dict["Type"] {
		case Name("Pages"):
			kids, err := r.Resolve(dict["Kids"])
			if err != nil {
				return err
			}
			array, ok := kids.(Array)
			if !ok {
				return fmt.Errorf("invalid page tree kids")
			}
			for _, child := range array {
				if err := walk(child, attrs, depth+1); err != nil {
					return err
				}
			}
			return nil
		case Name("Page"):
			page, err := r.readPage(ref, dict, attrs)
			if err != nil {
				return err
			}
			if err := visit(index, page); err != nil {
				return err
			}
			index++
			return nil
		default:
			return fmt.Errorf("invalid page tree type")
		}
	}
	return walk(catalog["Pages"], pageAttributes{}, 0)
}

// readPage 解析继承属性并检查页面尺寸
func (r *Reader) readPage(ref Reference, dict Dictionary, attrs pageAttributes) (*Page, error) {
	media, err := r.rectangle(attrs.mediaBox)
	if err != nil {
		return nil, err
	}
	crop := media
	if attrs.cropBox != nil {
		crop, err = r.rectangle(attrs.cropBox)
		if err != nil {
			return nil, err
		}
		crop = Rectangle{math.Max(media.XMin, crop.XMin), math.Max(media.YMin, crop.YMin), math.Min(media.XMax, crop.XMax), math.Min(media.YMax, crop.YMax)}
		if crop.XMin >= crop.XMax || crop.YMin >= crop.YMax {
			return nil, fmt.Errorf("empty crop box")
		}
	}
	resourceDict, ok := attrs.resources.(Dictionary)
	if !ok && attrs.resources != nil {
		return nil, fmt.Errorf("invalid page resources")
	}
	rotation := Integer(0)
	if attrs.rotation != nil {
		rotation, ok = attrs.rotation.(Integer)
		if !ok || rotation%90 != 0 {
			return nil, fmt.Errorf("invalid page rotation")
		}
	}
	unit := 1.0
	if dict["UserUnit"] != nil {
		unit, err = r.number(dict["UserUnit"])
		if err != nil {
			return nil, err
		}
		if unit <= 0 || unit > 75000 {
			return nil, fmt.Errorf("invalid page user unit")
		}
	}
	return &Page{Reference: ref, Dictionary: dict, Resources: resourceDict, MediaBox: media, CropBox: crop, Rotation: int((rotation%360 + 360) % 360), UserUnit: unit, reader: r}, nil
}

// number 解析直接或间接数字对象
func (r *Reader) number(object Object) (float64, error) {
	value, err := r.Resolve(object)
	if err != nil {
		return 0, err
	}
	switch n := value.(type) {
	case Integer:
		return float64(n), nil
	case Real:
		return float64(n), nil
	default:
		return 0, fmt.Errorf("expected number")
	}
}

// rectangle 读取并规范化矩形顶点顺序
func (r *Reader) rectangle(object Object) (Rectangle, error) {
	value, err := r.Resolve(object)
	if err != nil {
		return Rectangle{}, err
	}
	array, ok := value.(Array)
	if !ok || len(array) != 4 {
		return Rectangle{}, fmt.Errorf("invalid rectangle")
	}
	var values [4]float64
	for i, v := range array {
		values[i], err = r.number(v)
		if err != nil {
			return Rectangle{}, err
		}
	}
	rect := Rectangle{math.Min(values[0], values[2]), math.Min(values[1], values[3]), math.Max(values[0], values[2]), math.Max(values[1], values[3])}
	if rect.XMin == rect.XMax || rect.YMin == rect.YMax {
		return Rectangle{}, fmt.Errorf("empty rectangle")
	}
	return rect, nil
}

// Content 读取并依次连接页面内容流，保留流间的图形状态语义
// 返回: []byte 内容操作符数据, error 错误信息
func (p *Page) Content() ([]byte, error) {
	value, err := p.reader.Resolve(p.Dictionary["Contents"])
	if err != nil {
		return nil, err
	}
	if value == nil {
		return nil, nil
	}
	objects := Array{value}
	if array, ok := value.(Array); ok {
		objects = array
	}
	var out []byte
	for _, object := range objects {
		resolved, err := p.reader.Resolve(object)
		if err != nil {
			return nil, err
		}
		stream, ok := resolved.(*Stream)
		if !ok {
			return nil, fmt.Errorf("page content is not a stream")
		}
		data, err := stream.Decode()
		if err != nil {
			return nil, err
		}
		out = append(out, data...)
		out = append(out, '\n')
	}
	return out, nil
}
