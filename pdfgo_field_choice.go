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
	"slices"
)

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
// 返回: ChoiceField 选择状态, error 字段或选项错误
func (r *Reader) ReadChoiceField(annotation Annotation) (ChoiceField, error) {
	field, err := r.ReadField(annotation)
	if err != nil {
		return ChoiceField{}, err
	}
	return r.readChoiceField(field)
}

// readChoiceField 解码已合并继承属性的选择字段
// 入参: field 字段属性
// 返回: ChoiceField 选择状态, error 数据不一致错误
func (r *Reader) readChoiceField(field Dictionary) (ChoiceField, error) {
	var result ChoiceField
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
	result.Options = make([]ChoiceOption, 0, len(options))
	byValue := make(map[string]int, len(options))
	for i, object := range options {
		value, err := r.Resolve(object)
		if err != nil {
			return result, err
		}
		pair, isPair := value.(Array)
		if isPair && len(pair) != 2 {
			return result, fmt.Errorf("invalid choice option pair")
		}
		a, b := value, value
		if isPair {
			a, b = pair[0], pair[1]
		}
		export, err := r.choiceString(a)
		if err != nil {
			return result, err
		}
		label := export
		if isPair {
			label, err = r.choiceString(b)
			if err != nil {
				return result, err
			}
		}
		result.Options = append(result.Options, ChoiceOption{export, label})
		if _, exists := byValue[export]; exists {
			byValue[export] = -1
		} else {
			byValue[export] = i
		}
	}
	value, err = r.Resolve(field["V"])
	if err != nil {
		return result, err
	}
	hasValue := value != nil
	if value != nil {
		values, array := value.(Array)
		if !array {
			values = Array{value}
		}
		if len(values) > 1 && !result.MultiSelect {
			return result, fmt.Errorf("multiple values in single choice field")
		}
		for _, object := range values {
			text, err := r.choiceString(object)
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
	if value != nil {
		indices, ok := value.(Array)
		if !ok {
			return result, fmt.Errorf("invalid choice selection indices")
		}
		if len(indices) > 1 && !result.MultiSelect {
			return result, fmt.Errorf("multiple indices in single choice field")
		}
		previous := Integer(-1)
		selectedValues := make([]string, 0, len(indices))
		for _, object := range indices {
			value, err := r.Resolve(object)
			if err != nil {
				return result, err
			}
			index, ok := value.(Integer)
			if !ok || index <= previous || index < 0 || index >= Integer(len(options)) {
				return result, fmt.Errorf("invalid choice selection index")
			}
			previous = index
			result.Selected = append(result.Selected, int(index))
			selectedValues = append(selectedValues, result.Options[index].Value)
		}
		if hasValue {
			values := slices.Clone(result.Values)
			expected := slices.Clone(selectedValues)
			slices.Sort(values)
			slices.Sort(expected)
			if !slices.Equal(values, expected) && !(len(values) == 1 && len(expected) == 0 && (values[0] == "" || result.Editable)) {
				return result, fmt.Errorf("choice values and indices differ")
			}
		} else {
			result.Values = selectedValues
		}
	} else {
		for _, text := range result.Values {
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
	return result, nil
}

// choiceString 解码选择字段中的文本字符串
// 入参: object 字符串或引用
// 返回: string Unicode文字, error 类型或编码错误
func (r *Reader) choiceString(object Object) (string, error) {
	value, err := r.Resolve(object)
	if err != nil {
		return "", err
	}
	text, ok := value.(String)
	if !ok {
		return "", fmt.Errorf("invalid choice text string")
	}
	return DecodeTextString(text)
}
