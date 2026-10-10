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
	"encoding/hex"
	"fmt"
	"slices"
)

// TextReplacement 为连续文字对象指定整段原文，不改变绘制操作及字体编码
// First为页面合并内容流中从零开始的BT序号，Count须大于零，不包含表单内部文字对象
// Text使用UTF-8，允许为空；区段不得重叠、跨越已有内容标记边界或位于已有ActualText内部
type TextReplacement struct {
	First int
	Count int
	Text  string
}

// textReplacements 按页写入标准替换文本，复制内容流以隔离共享资源
// 入参: ctx 取消上下文, changes 页面替换区段, result 已有替换对象
// 返回: bool 是否修改内容, error 参数、内容或取消错误
func (r *Reader) textReplacements(ctx context.Context, changes map[Reference][]TextReplacement, result map[Reference]Object) (bool, error) {
	if len(changes) == 0 {
		return false, nil
	}
	number := int64(0)
	for id := range r.xref {
		number = max(number, id)
	}
	for ref := range result {
		number = max(number, ref.Number)
	}
	visited, changed := 0, false
	err := r.WalkPages(ctx, func(_ int, source *Page) error {
		spans, exists := changes[source.Reference]
		if !exists {
			return nil
		}
		visited++
		if len(spans) == 0 {
			return nil
		}
		var content bytes.Buffer
		if _, err := source.WriteContent(ctx, &content); err != nil {
			return err
		}
		data, err := r.replaceContentText(ctx, content.Bytes(), source.Resources, spans)
		if err != nil {
			return err
		}
		encoded, err := compressPDFBytes(ctx, data)
		if err != nil {
			return err
		}
		ref, err := appendRewriteObject(result, &number, &Stream{Dictionary: Dictionary{"Filter": Name("FlateDecode")}, Data: encoded})
		if err != nil {
			return err
		}
		page, err := r.rewriteDictionary(source.Reference, result)
		if err != nil {
			return err
		}
		page["Contents"] = ref
		result[source.Reference] = page
		changed = true
		return nil
	})
	if err != nil {
		return false, err
	}
	if visited != len(changes) {
		return false, fmt.Errorf("PDF text replacement page is not in the page tree")
	}
	return changed, ctx.Err()
}

// replaceContentText 在完整文字区段外写入ActualText，保留原始操作字节
// 入参: ctx 取消上下文, data 解码内容, resources 页面资源, changes 替换区段
// 返回: []byte 更新内容, error 范围、标记或取消错误
func (r *Reader) replaceContentText(ctx context.Context, data []byte, resources Dictionary, changes []TextReplacement) ([]byte, error) {
	spans := slices.Clone(changes)
	slices.SortFunc(spans, func(a, b TextReplacement) int {
		if a.First < b.First {
			return -1
		}
		if a.First > b.First {
			return 1
		}
		return 0
	})
	texts := make([]String, len(spans))
	for i, span := range spans {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if span.First < 0 || span.Count <= 0 || span.First+span.Count <= span.First || i > 0 && span.First < spans[i-1].First+spans[i-1].Count {
			return nil, fmt.Errorf("invalid or overlapping PDF text replacement range")
		}
		var err error
		texts[i], err = EncodeTextString(span.Text)
		if err != nil {
			return nil, err
		}
	}
	p := pageInterpreter{reader: r, ctx: ctx, resources: resources, visitor: Visitor{MarkedContent: func(MarkedContentMark) error { return nil }}}
	var result bytes.Buffer
	var frames []int64
	position, object, current := 0, 0, 0
	inside, replacing := false, false
	var frame int64 = -1
	depth := 0
	err := walkOperations(ctx, data, func(op Operation) error {
		switch op.Operator {
		case "BMC", "BDC", "EMC", "MP", "DP":
			if err := p.markedContent(op); err != nil {
				return err
			}
			if op.Operator == "BMC" || op.Operator == "BDC" {
				frames = append(frames, op.Offset)
			} else if op.Operator == "EMC" {
				frames = frames[:len(frames)-1]
			}
		case "BT":
			if inside || len(op.Operands) != 0 {
				return fmt.Errorf("invalid PDF text object begin")
			}
			inside = true
			if current < len(spans) && object == spans[current].First {
				if p.replacement != nil {
					return fmt.Errorf("PDF text replacement is enclosed by ActualText")
				}
				depth, frame = len(frames), -1
				if depth > 0 {
					frame = frames[depth-1]
				}
				result.Write(data[position:int(op.Offset)])
				result.WriteString("\n/Span << /ActualText <")
				result.WriteString(hex.EncodeToString(texts[current]))
				result.WriteString("> >> BDC\n")
				position, replacing = int(op.Offset), true
			}
		case "ET":
			if !inside || len(op.Operands) != 0 {
				return fmt.Errorf("invalid PDF text object end")
			}
			inside = false
			object++
			if replacing && object == spans[current].First+spans[current].Count {
				if len(frames) != depth || depth > 0 && frames[depth-1] != frame {
					return fmt.Errorf("PDF text replacement crosses marked content boundary")
				}
				end := int(op.Offset) + len(op.Operator)
				result.Write(data[position:end])
				result.WriteString("\nEMC\n")
				position, replacing = end, false
				current++
			}
		}
		return nil
	}, func(value Object) (Object, error) { return r.resourceColorSpace(value, resources) })
	if err != nil {
		return nil, err
	}
	if inside || len(frames) != 0 || current != len(spans) {
		return nil, fmt.Errorf("incomplete PDF text replacement range")
	}
	result.Write(data[position:])
	return result.Bytes(), ctx.Err()
}
