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
	"os"
)

// OutlineItem 保存目录标题、显示样式及原始跳转声明，不执行动作
type OutlineItem struct {
	Reference   Reference
	Title       string
	Expanded    bool
	Bold        bool
	Italic      bool
	Color       [3]float64
	Destination Object
	Action      Object
	Dictionary  Dictionary
}

// WalkOutlines 按先序访问文档目录，包括折叠节点的子项，不限制层级
// 配置Warning时跳过损坏、循环或格式异常的目录根及目录项，保留可访问条目并记录诊断
// 入参: ctx 取消上下文, visit 零基层级和目录项的访问函数
// 返回: error 目录结构、文本、循环、读取、解密、认证或访问错误
func (r *Reader) WalkOutlines(ctx context.Context, visit func(int, OutlineItem) error) error {
	if r == nil || ctx == nil {
		return fmt.Errorf("invalid outline context")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if r.closed {
		return os.ErrClosed
	}
	if visit == nil {
		return fmt.Errorf("missing outline visitor")
	}
	catalog, err := r.catalogDictionary()
	if err != nil {
		return err
	}
	value, err := r.Resolve(catalog["Outlines"])
	if err != nil {
		if !r.recoverAuxiliary(err) {
			return err
		}
		return ctx.Err()
	}
	if value == nil {
		return nil
	}
	outlines, ok := value.(Dictionary)
	if !ok {
		err := fmt.Errorf("invalid outlines dictionary")
		if !r.recoverAuxiliary(err) {
			return err
		}
		return ctx.Err()
	}
	type entry struct {
		object Object
		level  int
	}
	stack := []entry{{outlines["First"], 0}}
	seen := map[Reference]bool{}
	for len(stack) > 0 {
		if err := ctx.Err(); err != nil {
			return err
		}
		next := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if next.object == nil {
			continue
		}
		ref, ok := next.object.(Reference)
		if !ok {
			err := fmt.Errorf("outline item is not indirect")
			if !r.recoverAuxiliary(err) {
				return err
			}
			continue
		}
		if seen[ref] {
			err := fmt.Errorf("repeated outline item")
			if !r.recoverAuxiliary(err) {
				return err
			}
			continue
		}
		seen[ref] = true
		value, err := r.Resolve(ref)
		if err != nil {
			if !r.recoverAuxiliary(err) {
				return err
			}
			continue
		}
		if value == nil {
			continue
		}
		dict, ok := value.(Dictionary)
		if !ok {
			err := fmt.Errorf("invalid outline item")
			if !r.recoverAuxiliary(err) {
				return err
			}
			continue
		}
		title, err := r.Resolve(dict["Title"])
		if err != nil {
			if !r.recoverAuxiliary(err) {
				return err
			}
			stack = append(stack, entry{dict["Next"], next.level})
			continue
		}
		text, ok := title.(String)
		if !ok {
			err := fmt.Errorf("invalid outline title")
			if !r.recoverAuxiliary(err) {
				return err
			}
			stack = append(stack, entry{dict["Next"], next.level})
			continue
		}
		item := OutlineItem{Reference: ref, Dictionary: dict, Destination: dict["Dest"], Action: dict["A"]}
		item.Title, err = DecodeTextString(text)
		if err != nil {
			if !r.recoverAuxiliary(err) {
				return err
			}
			stack = append(stack, entry{dict["Next"], next.level})
			continue
		}
		count, err := r.Resolve(dict["Count"])
		if err != nil {
			if !r.recoverAuxiliary(err) {
				return err
			}
		}
		if count != nil {
			n, ok := count.(Integer)
			if !ok {
				err := fmt.Errorf("invalid outline count")
				if !r.recoverAuxiliary(err) {
					return err
				}
			} else {
				item.Expanded = n > 0
			}
		}
		flags, err := r.Resolve(dict["F"])
		if err != nil {
			if !r.recoverAuxiliary(err) {
				return err
			}
		}
		if flags != nil {
			n, ok := flags.(Integer)
			if !ok || n < 0 {
				err := fmt.Errorf("invalid outline flags")
				if !r.recoverAuxiliary(err) {
					return err
				}
			} else {
				item.Italic, item.Bold = n&1 != 0, n&2 != 0
			}
		}
		color, err := r.Resolve(dict["C"])
		if err != nil {
			if !r.recoverAuxiliary(err) {
				return err
			}
		}
		if color != nil {
			values, err := r.numberArray(color, 3)
			if err != nil {
				if !r.recoverAuxiliary(err) {
					return err
				}
			} else {
				var rgb [3]float64
				valid := true
				for i, v := range values {
					if math.IsNaN(v) || math.IsInf(v, 0) || v < 0 || v > 1 {
						err := fmt.Errorf("invalid outline color")
						if !r.recoverAuxiliary(err) {
							return err
						}
						valid = false
						break
					}
					rgb[i] = v
				}
				if valid {
					item.Color = rgb
				}
			}
		}
		if err := visit(next.level, item); err != nil {
			return err
		}
		stack = append(stack, entry{dict["Next"], next.level}, entry{dict["First"], next.level + 1})
	}
	return ctx.Err()
}
