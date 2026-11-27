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
	"slices"
)

// textStringBudget 在单次批量读取中缓存共享间接文本字符串并限制累计解码字节数
type textStringBudget struct {
	limit int64
	used  int64
	cache map[Reference]string
}

// newTextStringBudget 创建受流解压上限和十六兆字节双重约束的文本解码预算
// 入参: reader 关联阅读器
// 返回: textStringBudget 文本解码预算
func newTextStringBudget(reader *Reader) textStringBudget {
	return textStringBudget{limit: int64(annotationRichContentLimit(reader))}
}

// consume 累计逻辑文本字节数，超过上限时返回超限错误
// 入参: size 新增字节数, label 场景描述
// 返回: error 超限错误
func (b *textStringBudget) consume(size int64, label string) error {
	if size < 0 || size > b.limit-b.used {
		return fmt.Errorf("%w: %s text size limit exceeded", ErrStreamLimitExceeded, label)
	}
	b.used += size
	return nil
}

// decodeString 按间接引用复用已解码文本字符串并执行字节预算控制
// 入参: ref 文本对象最终间接引用, data 原始PDF字符串, label 场景描述
// 返回: string 解码文本, error 解码或超限错误
func (b *textStringBudget) decodeString(ref Reference, data String, label string) (string, error) {
	if ref != (Reference{}) {
		if cached, ok := b.cache[ref]; ok {
			if err := b.consume(int64(len(cached)), label); err != nil {
				return "", err
			}
			return cached, nil
		}
	}
	if len(data) > 3 && int64((len(data)-3)/2) > b.limit-b.used {
		return "", fmt.Errorf("%w: %s text size limit exceeded", ErrStreamLimitExceeded, label)
	}
	text, err := DecodeTextString(data)
	if err != nil {
		return "", err
	}
	if err := b.consume(int64(len(text)), label); err != nil {
		return "", err
	}
	if ref != (Reference{}) {
		if b.cache == nil {
			b.cache = make(map[Reference]string)
		}
		b.cache[ref] = text
	}
	return text, nil
}

// ChoiceOption 保存选择字段的导出值和显示文字
type ChoiceOption struct {
	Value string
	Label string
}

// ChoiceField 保存选择字段当前状态，Options保持文档顺序，Selected为零基索引
type ChoiceField struct {
	Options     []ChoiceOption
	Values      []string
	Selected    []int
	TopIndex    int
	Combo       bool
	Editable    bool
	MultiSelect bool
}

// ReadChoiceField 读取选择字段，区分显示文字和导出值，不执行交互动作
// 入参: annotation 字段控件
// 返回: ChoiceField 选择状态, error 字段、选项或超限错误
func (r *Reader) ReadChoiceField(annotation Annotation) (ChoiceField, error) {
	field, err := r.ReadField(annotation)
	if err != nil {
		return ChoiceField{}, err
	}
	return r.readChoiceFieldContext(context.Background(), field)
}

// readChoiceFieldContext 在取消上下文和文本预算内解码已合并继承属性的选择字段
// 入参: ctx 取消上下文, field 字段属性
// 返回: ChoiceField 选择状态, error 数据不一致、超限或取消错误
func (r *Reader) readChoiceFieldContext(ctx context.Context, field Dictionary) (ChoiceField, error) {
	var result ChoiceField
	if err := ctx.Err(); err != nil {
		return result, err
	}
	if field["FT"] != Name("Ch") {
		return result, fmt.Errorf("not a choice field")
	}
	flags := Integer(0)
	if value := field["Ff"]; value != nil {
		var ok bool
		flags, ok = value.(Integer)
		if !ok || flags < 0 {
			return result, fmt.Errorf("invalid choice field flags")
		}
	}
	result.Combo = flags&(1<<17) != 0
	result.Editable = result.Combo && flags&(1<<18) != 0
	result.MultiSelect = !result.Combo && flags&(1<<21) != 0
	value, err := r.Resolve(field["Opt"])
	if err != nil {
		return result, err
	}
	options, ok := value.(Array)
	if value != nil && !ok {
		return result, fmt.Errorf("invalid choice options")
	}
	budget := newTextStringBudget(r)
	var pairCache map[Reference]ChoiceOption
	result.Options = make([]ChoiceOption, 0, len(options))
	byValue := make(map[string]int, len(options))
	for i, object := range options {
		if err := ctx.Err(); err != nil {
			return result, err
		}
		ref, value, err := r.resolveIndirect(object)
		if err != nil {
			return result, err
		}
		if ref != (Reference{}) {
			if cached, exists := pairCache[ref]; exists {
				if err := budget.consume(int64(len(cached.Value))+int64(len(cached.Label)), "choice field"); err != nil {
					return result, err
				}
				result.Options = append(result.Options, cached)
				if _, seen := byValue[cached.Value]; seen {
					byValue[cached.Value] = -1
				} else {
					byValue[cached.Value] = i
				}
				continue
			}
		}
		var export, label string
		pair, isPair := value.(Array)
		if isPair {
			if len(pair) != 2 {
				return result, fmt.Errorf("invalid choice option pair")
			}
			export, err = r.choiceString(ctx, pair[0], &budget)
			if err != nil {
				return result, err
			}
			label, err = r.choiceString(ctx, pair[1], &budget)
			if err != nil {
				return result, err
			}
			if ref != (Reference{}) {
				if pairCache == nil {
					pairCache = make(map[Reference]ChoiceOption)
				}
				pairCache[ref] = ChoiceOption{export, label}
			}
		} else {
			text, ok := value.(String)
			if !ok {
				return result, fmt.Errorf("invalid choice text string")
			}
			export, err = budget.decodeString(ref, text, "choice field")
			if err != nil {
				return result, err
			}
			label = export
		}
		result.Options = append(result.Options, ChoiceOption{export, label})
		if _, exists := byValue[export]; exists {
			byValue[export] = -1
		} else {
			byValue[export] = i
		}
	}
	rawValue := field["V"]
	value, err = r.Resolve(rawValue)
	if err != nil {
		return result, err
	}
	hasValue := value != nil
	if value != nil {
		values, array := value.(Array)
		if !array {
			values = Array{rawValue}
		}
		if len(values) > 1 && !result.MultiSelect {
			return result, fmt.Errorf("multiple values in single choice field")
		}
		for _, object := range values {
			if err := ctx.Err(); err != nil {
				return result, err
			}
			text, err := r.choiceString(ctx, object, &budget)
			if err != nil {
				return result, err
			}
			result.Values = append(result.Values, text)
		}
	}
	value, err = r.Resolve(field["I"])
	if err != nil {
		return result, err
	}
	resolveFromValues := value == nil
	if value != nil {
		var selected []int
		var selectedValues []string
		valid := false
		if indices, ok := value.(Array); ok && (len(indices) <= 1 || result.MultiSelect) {
			previous := Integer(-1)
			selected = make([]int, 0, len(indices))
			selectedValues = make([]string, 0, len(indices))
			valid = true
			for _, object := range indices {
				if err := ctx.Err(); err != nil {
					return result, err
				}
				item, err := r.Resolve(object)
				if err != nil {
					if !hasValue || isFatalReaderError(err) {
						return result, err
					}
					valid = false
					break
				}
				index, ok := item.(Integer)
				if !ok || index <= previous || index < 0 || index >= Integer(len(options)) {
					valid = false
					break
				}
				previous = index
				selected = append(selected, int(index))
				selectedValues = append(selectedValues, result.Options[index].Value)
			}
		} else if !hasValue {
			if _, ok := value.(Array); !ok {
				return result, fmt.Errorf("invalid choice selection indices")
			}
			return result, fmt.Errorf("multiple indices in single choice field")
		}
		if !valid {
			if !hasValue {
				return result, fmt.Errorf("invalid choice selection index")
			}
			resolveFromValues = true
		} else if hasValue {
			values := slices.Clone(result.Values)
			expected := slices.Clone(selectedValues)
			slices.Sort(values)
			slices.Sort(expected)
			if slices.Equal(values, expected) {
				result.Selected = selected
			} else {
				resolveFromValues = true
			}
		} else {
			result.Selected = selected
			result.Values = selectedValues
		}
	}
	if resolveFromValues {
		for _, text := range result.Values {
			if err := ctx.Err(); err != nil {
				return result, err
			}
			index, found := byValue[text]
			if !found {
				if text == "" || result.Editable {
					continue
				}
				return result, fmt.Errorf("choice value not found in options")
			}
			if index < 0 {
				return result, fmt.Errorf("ambiguous choice value without selection index")
			}
			result.Selected = append(result.Selected, index)
		}
		slices.Sort(result.Selected)
		for i := 1; i < len(result.Selected); i++ {
			if result.Selected[i] == result.Selected[i-1] {
				return result, fmt.Errorf("duplicate choice selection")
			}
		}
	}
	value, err = r.Resolve(field["TI"])
	if err != nil {
		return result, err
	}
	if value != nil {
		top, ok := value.(Integer)
		if !ok || top < 0 || top >= Integer(max(1, len(options))) {
			return result, fmt.Errorf("invalid choice top index")
		}
		result.TopIndex = int(top)
	}
	return result, ctx.Err()
}

// choiceString 在文本预算内解码选择字段中的文本字符串并复用共享引用
// 入参: ctx 取消上下文, object 字符串或引用, budget 文本解码预算
// 返回: string Unicode文字, error 类型、编码、超限或取消错误
func (r *Reader) choiceString(ctx context.Context, object Object, budget *textStringBudget) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	ref, value, err := r.resolveIndirect(object)
	if err != nil {
		return "", err
	}
	text, ok := value.(String)
	if !ok {
		return "", fmt.Errorf("invalid choice text string")
	}
	return budget.decodeString(ref, text, "choice field")
}
