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
	"bytes"
	"context"
	"fmt"
	"io"
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
	if err := ctx.Err(); err != nil {
		return err
	}
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
	type frame struct {
		kids  Array
		attrs pageAttributes
	}
	stack := []frame{{kids: Array{catalog["Pages"]}}}
	for len(stack) != 0 {
		if err := ctx.Err(); err != nil {
			return err
		}
		current := &stack[len(stack)-1]
		if len(current.kids) == 0 {
			stack = stack[:len(stack)-1]
			continue
		}
		object, inherited := current.kids[0], current.attrs
		current.kids = current.kids[1:]
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
		kind, err := r.Resolve(dict["Type"])
		if err != nil {
			return err
		}
		switch kind {
		case Name("Pages"):
			kids, err := r.Resolve(dict["Kids"])
			if err != nil {
				return err
			}
			array, ok := kids.(Array)
			if !ok {
				return fmt.Errorf("invalid page tree kids")
			}
			stack = append(stack, frame{kids: array, attrs: attrs})
		case Name("Page"):
			page, err := r.readPage(ref, dict, attrs)
			if err != nil {
				return err
			}
			if err := visit(index, page); err != nil {
				return err
			}
			index++
		default:
			return fmt.Errorf("invalid page tree type")
		}
	}
	return nil
}

// Content 读取并依次连接页面内容流，保留流间的图形状态语义
// 返回: []byte 内容操作符数据, error 错误信息
func (p *Page) Content() ([]byte, error) {
	var out bytes.Buffer
	if _, err := p.WriteContent(context.Background(), &out); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

// WriteContent 逐个解码并输出页面内容流，以换行分隔，不拼接整页内容；单个流仍完整解码
// 入参: ctx 取消上下文，在流解码及写出间检查, writer 输出流
// 返回: int64 已写字节数, error 解码或写入错误，出错时可能已有部分输出
func (p *Page) WriteContent(ctx context.Context, writer io.Writer) (int64, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	value, err := p.reader.Resolve(p.Dictionary["Contents"])
	if err != nil {
		return 0, err
	}
	if value == nil {
		return 0, nil
	}
	objects := Array{value}
	if array, ok := value.(Array); ok {
		objects = array
	}
	var written int64
	for _, object := range objects {
		if err := ctx.Err(); err != nil {
			return written, err
		}
		resolved, err := p.reader.Resolve(object)
		if err != nil {
			return written, err
		}
		stream, ok := resolved.(*Stream)
		if !ok {
			return written, fmt.Errorf("page content is not a stream")
		}
		data, err := stream.DecodeContext(ctx)
		if err != nil {
			return written, err
		}
		if err := ctx.Err(); err != nil {
			return written, err
		}
		n, err := writer.Write(data)
		written += int64(n)
		if err != nil {
			return written, err
		}
		if n != len(data) {
			return written, io.ErrShortWrite
		}
		n, err = io.WriteString(writer, "\n")
		written += int64(n)
		if err != nil {
			return written, err
		}
		if n != 1 {
			return written, io.ErrShortWrite
		}
	}
	return written, nil
}

// readPage 解析继承属性并检查页面尺寸
// 入参: ref 页面引用, dict 页面字典, attrs 已继承的页面属性
// 返回: *Page 页面, error 页面属性错误
func (r *Reader) readPage(ref Reference, dict Dictionary, attrs pageAttributes) (*Page, error) {
	media, err := r.rectangle(attrs.mediaBox)
	if err != nil {
		return nil, err
	}
	if media.XMin == media.XMax || media.YMin == media.YMax {
		return nil, fmt.Errorf("empty media box")
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
// 入参: object 数字对象或间接引用
// 返回: float64 数值, error 引用或类型错误
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
// 入参: object 矩形数组或间接引用
// 返回: Rectangle 矩形, error 引用、类型或坐标错误
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
		if math.IsNaN(values[i]) || math.IsInf(values[i], 0) {
			return Rectangle{}, fmt.Errorf("invalid rectangle coordinate")
		}
	}
	rect := Rectangle{math.Min(values[0], values[2]), math.Min(values[1], values[3]), math.Max(values[0], values[2]), math.Max(values[1], values[3])}
	return rect, nil
}
