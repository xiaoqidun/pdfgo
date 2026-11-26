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
	"reflect"
)

// maxActualTextCacheEntries 限制单次遍历驻留的ActualText解码去重缓存条目数
const maxActualTextCacheEntries = 4096

// replacementText 读取Span的替换文本，区分空字符串与未声明属性，按间接引用与指针去重并受字节预算约束
// 入参: properties 区段属性
// 返回: *ReplacementText 替换区段，未声明时为空, error 类型、编码、超限或取消错误
func (p *pageInterpreter) replacementText(properties Dictionary) (*ReplacementText, error) {
	if err := p.ctx.Err(); err != nil {
		return nil, err
	}
	ref, object, err := p.reader.resolveIndirect(properties["ActualText"])
	if err != nil || object == nil {
		return nil, err
	}
	value, ok := object.(String)
	if !ok {
		return nil, fmt.Errorf("ActualText is not a text string")
	}
	if len(value) == 0 {
		return &ReplacementText{Text: "", Parent: p.replacement}, p.ctx.Err()
	}
	walk := p.ensureWalkState()
	if walk.textBudgetReader == nil {
		walk.textBudget = newTextStringBudget(p.reader)
		walk.textBudgetReader = p.reader
	}
	var text string
	if ref != (Reference{}) && walk.textBudgetReader == p.reader {
		if cached, ok := walk.textBudget.cache[ref]; ok {
			if err := walk.textBudget.consume(int64(len(cached)), "ActualText"); err != nil {
				return nil, err
			}
			text = cached
		} else {
			cacheRef := ref
			if len(walk.textBudget.cache) >= maxActualTextCacheEntries {
				cacheRef = Reference{}
			}
			text, err = walk.textBudget.decodeString(cacheRef, value, "ActualText")
			if err != nil {
				return nil, err
			}
		}
	} else {
		hit := false
		key := actualTextPointerKey{ptr: reflect.ValueOf(value).Pointer(), length: len(value)}
		if key.ptr != 0 {
			if cached, ok := walk.textPtrCache[key]; ok {
				if err := walk.textBudget.consume(int64(len(cached.decoded)), "ActualText"); err != nil {
					return nil, err
				}
				text, hit = cached.decoded, true
			}
		}
		if !hit {
			text, err = walk.textBudget.decodeString(Reference{}, value, "ActualText")
			if err != nil {
				return nil, err
			}
			if key.ptr != 0 && len(walk.textPtrCache) < maxActualTextCacheEntries {
				if walk.textPtrCache == nil {
					walk.textPtrCache = make(map[actualTextPointerKey]cachedActualText)
				}
				walk.textPtrCache[key] = cachedActualText{raw: value, decoded: text}
			}
		}
	}
	if err := p.ctx.Err(); err != nil {
		return nil, err
	}
	return &ReplacementText{Text: text, Parent: p.replacement}, nil
}
