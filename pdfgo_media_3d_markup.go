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
	"crypto/md5"
	"fmt"
	"reflect"
)

// ThreeDMarkup 保存三维批注的宿主、视图及可选模型校验和，不决定激活状态
type ThreeDMarkup struct {
	Annotation Annotation
	View       Dictionary
	Checksum   *[16]byte
	Dictionary Dictionary
}

// MatchesView 比较同一Reader内的视图对象身份，不将参数相同的不同视图合并
// 入参: view 当前选定视图
// 返回: bool 是否为绑定视图
func (m ThreeDMarkup) MatchesView(view Dictionary) bool {
	return m.View != nil && view != nil && reflect.ValueOf(m.View).Pointer() == reflect.ValueOf(view).Pointer()
}

// ReadThreeDMarkups 批量解析同页批注关联，按输入顺序返回，未关联项为空
// 没有三维批注时返回空列表；名称引用仅在传入的同页注解中查找
// 入参: ctx 取消上下文, annotations 同页注解
// 返回: []*ThreeDMarkup 批注关联, error 类型、引用或取消错误
func (r *Reader) ReadThreeDMarkups(ctx context.Context, annotations []Annotation) ([]*ThreeDMarkup, error) {
	if ctx == nil || r == nil || r.closed {
		return nil, fmt.Errorf("invalid 3D markup reader or context")
	}
	var result []*ThreeDMarkup
	var names map[string][]Annotation
	for index, annotation := range annotations {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		dict, err := r.mediaDictionary(annotation.Dictionary["ExData"], true)
		if err != nil {
			return nil, err
		}
		if dict == nil {
			continue
		}
		subtype, err := r.Resolve(dict["Subtype"])
		if err != nil {
			return nil, err
		}
		if subtype != Name("Markup3D") {
			continue
		}
		kind, err := r.Resolve(dict["Type"])
		if err != nil {
			return nil, err
		}
		if kind != nil && kind != Name("ExData") {
			return nil, fmt.Errorf("invalid 3D markup external data type")
		}
		view, err := r.mediaDictionary(dict["3DV"], false)
		if err != nil || view == nil {
			if err == nil {
				err = fmt.Errorf("missing 3D markup view")
			}
			return nil, err
		}
		kind, err = r.Resolve(view["Type"])
		if err != nil {
			return nil, err
		}
		if kind != nil && kind != Name("3DView") {
			return nil, fmt.Errorf("invalid 3D markup view type")
		}
		value, err := r.Resolve(dict["3DA"])
		if err != nil {
			return nil, err
		}
		var host Annotation
		switch value := value.(type) {
		case String:
			name, err := DecodeTextString(value)
			if err != nil {
				return nil, err
			}
			if names == nil {
				names, err = r.threeDAnnotationNames(ctx, annotations)
				if err != nil {
					return nil, err
				}
			}
			matches := names[name]
			if len(matches) != 1 {
				return nil, fmt.Errorf("missing or ambiguous 3D markup annotation name")
			}
			host = matches[0]
		case Dictionary:
			subtype, err := r.Resolve(value["Subtype"])
			if err != nil {
				return nil, err
			}
			if subtype != Name("3D") {
				return nil, fmt.Errorf("invalid 3D markup annotation subtype")
			}
			box, err := r.rectangle(value["Rect"])
			if err != nil {
				return nil, err
			}
			host = Annotation{Subtype: "3D", Rect: box, Dictionary: value}
			host.Reference, _ = dict["3DA"].(Reference)
		default:
			return nil, fmt.Errorf("invalid 3D markup annotation")
		}
		markup := &ThreeDMarkup{Annotation: host, View: view, Dictionary: dict}
		checksum, err := r.Resolve(dict["MD5"])
		if err != nil {
			return nil, err
		}
		if checksum != nil {
			data, ok := checksum.(String)
			if !ok || len(data) != md5.Size {
				return nil, fmt.Errorf("invalid 3D markup checksum")
			}
			markup.Checksum = new([16]byte)
			copy(markup.Checksum[:], data)
		}
		if result == nil {
			result = make([]*ThreeDMarkup, len(annotations))
		}
		result[index] = markup
	}
	return result, ctx.Err()
}

// threeDAnnotationNames 为同页三维注解建立名称索引，保留重名供调用方检查
// 入参: ctx 取消上下文, annotations 同页注解
// 返回: map[string][]Annotation 名称索引, error 文字、引用或取消错误
func (r *Reader) threeDAnnotationNames(ctx context.Context, annotations []Annotation) (map[string][]Annotation, error) {
	names := make(map[string][]Annotation)
	for _, annotation := range annotations {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if annotation.Subtype != "3D" {
			continue
		}
		value, err := r.Resolve(annotation.Dictionary["NM"])
		if err != nil {
			return nil, err
		}
		if value == nil {
			continue
		}
		text, ok := value.(String)
		if !ok {
			return nil, fmt.Errorf("invalid 3D annotation name")
		}
		name, err := DecodeTextString(text)
		if err != nil {
			return nil, err
		}
		names[name] = append(names[name], annotation)
	}
	return names, nil
}

// ThreeDArtworkChecksum 计算三维模型数据的MD5，不将PDF流过滤器编码计入模型内容
// 校验仅用于检测模型变化，不用于安全认证
// 入参: ctx 取消上下文, stream 三维模型流
// 返回: [16]byte 校验和, error 解码或取消错误
func ThreeDArtworkChecksum(ctx context.Context, stream *Stream) ([16]byte, error) {
	var result [16]byte
	if ctx == nil || stream == nil {
		return result, fmt.Errorf("invalid 3D artwork checksum source")
	}
	data, err := stream.DecodeContext(ctx)
	if err != nil {
		return result, err
	}
	hash := md5.New()
	for len(data) != 0 {
		if err := ctx.Err(); err != nil {
			return result, err
		}
		n := min(len(data), 1<<20)
		if _, err := hash.Write(data[:n]); err != nil {
			return result, err
		}
		data = data[n:]
	}
	copy(result[:], hash.Sum(nil))
	return result, ctx.Err()
}
