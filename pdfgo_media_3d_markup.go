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
	"os"
	"reflect"
)

// ThreeDMarkup 保存三维批注的宿主、视图及可选模型校验和，不决定激活状态
type ThreeDMarkup struct {
	Annotation    Annotation
	View          Dictionary
	ViewReference Reference
	Checksum      *[16]byte
	Dictionary    Dictionary
	reader        *Reader
}

// ThreeDArtworkChecksum 在当前阅读器视图下计算三维模型数据的MD5，保留诊断回调与间接过滤器解析能力
// 校验仅用于检测模型变化，不用于安全认证
// 入参: ctx 取消上下文, stream 三维模型流
// 返回: [16]byte 校验和, error 解码、关闭或取消错误
func (r *Reader) ThreeDArtworkChecksum(ctx context.Context, stream *Stream) ([16]byte, error) {
	var result [16]byte
	if r == nil || ctx == nil {
		return result, fmt.Errorf("invalid 3D artwork checksum source")
	}
	if err := ctx.Err(); err != nil {
		return result, err
	}
	if r.isClosed() {
		return result, os.ErrClosed
	}
	if stream == nil {
		return result, fmt.Errorf("invalid 3D artwork checksum source")
	}
	data, err := r.DecodeStreamContext(ctx, stream)
	if err != nil {
		return result, err
	}
	return threeDArtworkMD5(ctx, data)
}

// threeDArtworkMD5 分块计算三维模型解码数据的MD5摘要并响应取消
// 入参: ctx 取消上下文, data 解码后的模型字节
// 返回: [16]byte 校验和, error 哈希或取消错误
func threeDArtworkMD5(ctx context.Context, data []byte) ([16]byte, error) {
	var result [16]byte
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

// MatchesView 比较同一Reader内的视图对象身份，优先按引用核对缓存字典且不合并参数相同的不同视图
// 入参: view 当前选定视图
// 返回: bool 是否为绑定视图
func (m ThreeDMarkup) MatchesView(view Dictionary) bool {
	if m.View == nil || view == nil {
		return false
	}
	target := reflect.ValueOf(view).Pointer()
	if reflect.ValueOf(m.View).Pointer() == target {
		return true
	}
	if m.ViewReference != (Reference{}) && m.reader != nil && !m.reader.isClosed() {
		if resolved, err := m.reader.Object(m.ViewReference); err == nil {
			if dict, ok := resolved.(Dictionary); ok && dict != nil {
				return reflect.ValueOf(dict).Pointer() == target
			}
		}
	}
	return false
}

// MatchesViewReference 按间接对象引用比较三维批注是否绑定到指定视图，直接字典无引用时返回false
// 入参: ref 目标视图间接引用
// 返回: bool 是否为绑定视图引用
func (m ThreeDMarkup) MatchesViewReference(ref Reference) bool {
	return m.ViewReference != (Reference{}) && m.ViewReference == ref
}

// ReadThreeDMarkups 批量解析同页批注关联，按输入顺序返回，未关联项为空
// 没有三维批注时返回空列表；名称引用仅在传入的同页注解中查找
// 入参: ctx 取消上下文, annotations 同页注解
// 返回: []*ThreeDMarkup 批注关联, error 类型、引用、超限或取消错误
func (r *Reader) ReadThreeDMarkups(ctx context.Context, annotations []Annotation) ([]*ThreeDMarkup, error) {
	if ctx == nil || r == nil {
		return nil, fmt.Errorf("invalid 3D markup reader or context")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if r.isClosed() {
		return nil, os.ErrClosed
	}
	var result []*ThreeDMarkup
	var names map[string][]Annotation
	budget := newTextStringBudget(r)
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
		viewRef, err := r.resolveIndirectReference(dict["3DV"])
		if err != nil {
			return nil, err
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
		hostRef, value, err := r.resolveIndirect(dict["3DA"])
		if err != nil {
			return nil, err
		}
		var host Annotation
		switch value := value.(type) {
		case String:
			name, err := budget.decodeString(hostRef, value, "3D markup")
			if err != nil {
				return nil, err
			}
			if names == nil {
				names, err = r.threeDAnnotationNames(ctx, annotations, &budget)
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
			host = Annotation{Reference: hostRef, Subtype: "3D", Rect: box, Dictionary: value}
		default:
			return nil, fmt.Errorf("invalid 3D markup annotation")
		}
		markup := &ThreeDMarkup{Annotation: host, View: view, ViewReference: viewRef, Dictionary: dict, reader: r}
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
// 入参: ctx 取消上下文, annotations 同页注解, budget 文本解码预算
// 返回: map[string][]Annotation 名称索引, error 文字、引用、超限或取消错误
func (r *Reader) threeDAnnotationNames(ctx context.Context, annotations []Annotation, budget *textStringBudget) (map[string][]Annotation, error) {
	names := make(map[string][]Annotation)
	for _, annotation := range annotations {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if annotation.Subtype != "3D" {
			continue
		}
		ref, value, err := r.resolveIndirect(annotation.Dictionary["NM"])
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
		name, err := budget.decodeString(ref, text, "3D markup")
		if err != nil {
			return nil, err
		}
		names[name] = append(names[name], annotation)
	}
	return names, nil
}
