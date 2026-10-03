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
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
)

// ReferenceXObject 保存引用表单的目标文件、页面选择及文件标识
// PageLabel不为nil时按文本页标签选择，否则使用从0开始的PageIndex
type ReferenceXObject struct {
	File      FileSpecification
	PageIndex int64
	PageLabel *string
	ID        []String
}

// ReferenceResolver 由调用方提供目标页面，返回nil时使用代理内容
// 目标阅读器由调用方维护，须在本次遍历及访问器回调结束前保持可用
type ReferenceResolver func(context.Context, *Reader, ReferenceXObject) (*Page, error)

// PageLabelRange 保存页标签区间的起始索引、编号格式、前缀及起始编号
type PageLabelRange struct {
	Start  int64
	Style  Name
	Prefix string
	First  int64
}

// ReadReferenceXObject 读取引用表单的Ref字典，不访问目标文件
// 入参: object 引用字典或间接引用
// 返回: ReferenceXObject 目标说明, error 字典或选择参数错误
func (r *Reader) ReadReferenceXObject(object Object) (ReferenceXObject, error) {
	var result ReferenceXObject
	value, err := r.Resolve(object)
	if err != nil {
		return result, err
	}
	dict, ok := value.(Dictionary)
	if !ok {
		return result, fmt.Errorf("invalid reference XObject dictionary")
	}
	result.File, err = r.ReadFileSpecification(dict["F"])
	if err != nil {
		return result, err
	}
	value, err = r.Resolve(dict["Page"])
	if err != nil {
		return result, err
	}
	switch page := value.(type) {
	case Integer:
		if page < 0 {
			return result, fmt.Errorf("negative reference XObject page index")
		}
		result.PageIndex = int64(page)
	case String:
		label, err := DecodeTextString(page)
		if err != nil {
			return result, err
		}
		result.PageLabel = &label
	default:
		return result, fmt.Errorf("invalid reference XObject page selector")
	}
	value, err = r.Resolve(dict["ID"])
	if err != nil || value == nil {
		return result, err
	}
	ids, ok := value.(Array)
	if !ok || len(ids) != 2 {
		return result, fmt.Errorf("invalid reference XObject file identifier")
	}
	for _, object := range ids {
		value, err := r.Resolve(object)
		if err != nil {
			return result, err
		}
		id, ok := value.(String)
		if !ok {
			return result, fmt.Errorf("invalid reference XObject identifier string")
		}
		result.ID = append(result.ID, String(bytes.Clone(id)))
	}
	return result, nil
}

// WalkPageLabelRanges 按起始页面索引访问页标签定义，不生成整本标签
// 入参: ctx 取消上下文, visit 区间访问函数
// 返回: error 数字树、标签定义或访问错误
func (r *Reader) WalkPageLabelRanges(ctx context.Context, visit func(PageLabelRange) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	catalog, err := r.catalogDictionary()
	if err != nil {
		return err
	}
	root, err := r.Resolve(catalog["PageLabels"])
	if err != nil || root == nil {
		return err
	}
	first := true
	err = r.WalkNumberTree(ctx, root, func(index int64, object Object) error {
		if index < 0 || first && index != 0 {
			return fmt.Errorf("invalid page label range start")
		}
		first = false
		value, err := r.Resolve(object)
		if err != nil {
			return err
		}
		dict, ok := value.(Dictionary)
		if !ok {
			return fmt.Errorf("invalid page label dictionary")
		}
		result := PageLabelRange{Start: index, First: 1}
		kind, err := r.Resolve(dict["Type"])
		if err != nil {
			return err
		}
		if kind != nil && kind != Name("PageLabel") {
			return fmt.Errorf("invalid page label type")
		}
		value, err = r.Resolve(dict["S"])
		if err != nil {
			return err
		}
		if value != nil {
			result.Style, ok = value.(Name)
			if !ok || result.Style != "D" && result.Style != "R" && result.Style != "r" && result.Style != "A" && result.Style != "a" {
				return fmt.Errorf("invalid page label style")
			}
		}
		value, err = r.Resolve(dict["P"])
		if err != nil {
			return err
		}
		if value != nil {
			prefix, ok := value.(String)
			if !ok {
				return fmt.Errorf("invalid page label prefix")
			}
			result.Prefix, err = DecodeTextString(prefix)
			if err != nil {
				return err
			}
		}
		value, err = r.Resolve(dict["St"])
		if err != nil {
			return err
		}
		if value != nil {
			number, ok := value.(Integer)
			if !ok || number < 1 {
				return fmt.Errorf("invalid page label start number")
			}
			result.First = int64(number)
		}
		return visit(result)
	})
	if err != nil {
		return err
	}
	if first {
		return fmt.Errorf("missing page label range for page zero")
	}
	return ctx.Err()
}

// ResolveReferencePage 在目标文档中按索引或页标签查找引用页面
// 入参: ctx 取消上下文, reference 引用目标
// 返回: *Page 目标页面, error 页面缺失或标签结构错误
func (r *Reader) ResolveReferencePage(ctx context.Context, reference ReferenceXObject) (*Page, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if reference.PageLabel == nil && reference.PageIndex < 0 {
		return nil, fmt.Errorf("negative reference XObject page index")
	}
	var ranges []PageLabelRange
	if reference.PageLabel != nil {
		if err := r.WalkPageLabelRanges(ctx, func(label PageLabelRange) error {
			ranges = append(ranges, label)
			return nil
		}); err != nil {
			return nil, err
		}
		if len(ranges) == 0 {
			return nil, fmt.Errorf("%w: reference XObject page label", ErrDestinationNotFound)
		}
	}
	var selected *Page
	stop := errors.New("reference page selected")
	labelIndex := 0
	err := r.WalkPages(ctx, func(index int, page *Page) error {
		match := int64(index) == reference.PageIndex
		if reference.PageLabel != nil {
			for labelIndex+1 < len(ranges) && ranges[labelIndex+1].Start <= int64(index) {
				labelIndex++
			}
			match = ranges[labelIndex].matches(int64(index), *reference.PageLabel)
		}
		if match {
			selected = page
			return stop
		}
		return nil
	})
	if err != nil && err != stop {
		return nil, err
	}
	if selected == nil {
		return nil, fmt.Errorf("%w: reference XObject page", ErrDestinationNotFound)
	}
	return selected, ctx.Err()
}

// ReferenceIDMatches 比较引用记录与目标文档的文件标识，不阻止读取已变化的文件
// 入参: reference 引用目标
// 返回: bool 文件标识相同或引用未声明标识, error 标识结构错误
func (r *Reader) ReferenceIDMatches(reference ReferenceXObject) (bool, error) {
	if len(reference.ID) == 0 {
		return true, nil
	}
	value, err := r.Resolve(r.Trailer["ID"])
	if err != nil {
		return false, err
	}
	ids, ok := value.(Array)
	if !ok || len(ids) != 2 || len(reference.ID) != 2 {
		return false, nil
	}
	for index, object := range ids {
		value, err := r.Resolve(object)
		if err != nil {
			return false, err
		}
		id, ok := value.(String)
		if !ok || !bytes.Equal(reference.ID[index], id) {
			return false, nil
		}
	}
	return true, nil
}

// referencePage 绘制目标页面及其可打印注解外观，保留引用表单的变换与裁剪
// 入参: page 目标页面
// 返回: error 内容、注解或访问错误
func (p *pageInterpreter) referencePage(page *Page) error {
	initial := *p
	var data bytes.Buffer
	if _, err := page.WriteContent(p.ctx, &data); err != nil {
		return err
	}
	if err := p.run(data.Bytes()); err != nil {
		return err
	}
	annotations, err := page.Annotations()
	if err != nil {
		return err
	}
	for _, annotation := range annotations {
		if err := p.ctx.Err(); err != nil {
			return err
		}
		value, err := p.reader.Resolve(annotation.Dictionary["F"])
		if err != nil {
			return err
		}
		flags := Integer(0)
		if value != nil {
			var ok bool
			flags, ok = value.(Integer)
			if !ok || flags < 0 {
				return fmt.Errorf("invalid reference page annotation flags")
			}
		}
		if flags&4 == 0 || flags&2 != 0 {
			continue
		}
		value, err = p.reader.Resolve(annotation.Dictionary["AP"])
		if err != nil {
			return err
		}
		if value == nil {
			continue
		}
		if flags&1 != 0 && !standardAnnotationSubtype(annotation.Subtype) {
			continue
		}
		if err := p.reader.walkAnnotationAppearance(p.ctx, page, annotation, p.visitor, &initial); err != nil {
			return err
		}
	}
	return p.ctx.Err()
}

// standardAnnotationSubtype 判断标准定义的注解类型，不执行交互内容
// 入参: subtype 注解类型
// 返回: bool 标准类型
func standardAnnotationSubtype(subtype Name) bool {
	switch subtype {
	case "Text", "Link", "FreeText", "Line", "Square", "Circle", "Polygon", "PolyLine", "Highlight", "Underline", "Squiggly", "StrikeOut", "Caret", "Ink", "Stamp", "Popup", "FileAttachment", "Sound", "Movie", "Widget", "Screen", "PrinterMark", "TrapNet", "Watermark", "3D", "Redact", "RichMedia", "Projection":
		return true
	}
	return false
}

// matches 按标准编号规则比较单页标签，不分配超出目标文本长度的字符串
// 入参: index 页面索引, label 目标标签
// 返回: bool 标签相同
func (r PageLabelRange) matches(index int64, label string) bool {
	suffix, ok := strings.CutPrefix(label, r.Prefix)
	if !ok || index < r.Start || index-r.Start > math.MaxInt64-r.First {
		return false
	}
	number := r.First + index - r.Start
	switch r.Style {
	case "":
		return suffix == ""
	case "D":
		return suffix == strconv.FormatInt(number, 10)
	case "A", "a":
		count := (number-1)/26 + 1
		if count != int64(len(suffix)) {
			return false
		}
		base := byte('A')
		if r.Style == "a" {
			base = 'a'
		}
		for index := range suffix {
			if suffix[index] != base+byte((number-1)%26) {
				return false
			}
		}
		return true
	case "R", "r":
		if number/1000 > int64(len(suffix)) {
			return false
		}
		var result strings.Builder
		for _, part := range []struct {
			value int64
			text  string
		}{{1000, "M"}, {900, "CM"}, {500, "D"}, {400, "CD"}, {100, "C"}, {90, "XC"}, {50, "L"}, {40, "XL"}, {10, "X"}, {9, "IX"}, {5, "V"}, {4, "IV"}, {1, "I"}} {
			count := number / part.value
			if count > int64((len(suffix)-result.Len())/len(part.text)) {
				return false
			}
			result.WriteString(strings.Repeat(part.text, int(count)))
			number %= part.value
		}
		text := result.String()
		if r.Style == "r" {
			text = strings.ToLower(text)
		}
		return suffix == text
	}
	return false
}
