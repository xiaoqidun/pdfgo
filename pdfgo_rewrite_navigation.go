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
	"maps"
	"net/url"
	"strings"
	"unicode/utf8"
)

// NavigationAction 描述文档内跳转或URI导航，Destination和URI须且仅须指定一项
// URI使用UTF-8文字或已转义地址，写出时转换为ASCII，不访问外部资源
type NavigationAction struct {
	Destination *Destination
	URI         string
}

// linkNavigation 保存链接目标替换或完整动作序列替换
type linkNavigation struct {
	destination    Array
	action         Dictionary
	replaceActions bool
}

// navigationReplacements 校验导航修改并按实际层级更新大纲节点
// 入参: ctx 取消上下文, options 替换配置, result 已有替换对象
// 返回: map[AnnotationLocation]linkNavigation 链接导航修改, error 页面、目标或大纲错误
func (r *Reader) navigationReplacements(ctx context.Context, options RewriteOptions, result map[Reference]Object) (map[AnnotationLocation]linkNavigation, error) {
	if len(options.LinkDestinations) == 0 && len(options.LinkActions) == 0 && len(options.OutlineDestinations) == 0 && len(options.OutlineTitles) == 0 && len(options.OutlineActions) == 0 {
		return nil, nil
	}
	pages := make(map[Reference]bool)
	needsPages := len(options.LinkDestinations) != 0 || len(options.LinkActions) != 0 || len(options.OutlineDestinations) != 0
	for location := range options.LinkActions {
		if _, exists := options.LinkDestinations[location]; exists {
			return nil, fmt.Errorf("conflicting PDF link actions and destination")
		}
	}
	for ref, actions := range options.OutlineActions {
		if _, exists := options.OutlineDestinations[ref]; exists {
			return nil, fmt.Errorf("conflicting PDF outline actions and destination")
		}
		for _, action := range actions {
			needsPages = needsPages || action.Destination != nil
		}
	}
	if needsPages {
		if err := r.WalkPages(ctx, func(_ int, page *Page) error {
			pages[page.Reference] = true
			return nil
		}); err != nil {
			return nil, err
		}
	}
	links := make(map[AnnotationLocation]linkNavigation, len(options.LinkDestinations)+len(options.LinkActions))
	for location, dest := range options.LinkDestinations {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if !pages[location.Page] {
			return nil, fmt.Errorf("PDF link page is not in the page tree")
		}
		array, err := r.rewriteDestination(dest, pages)
		if err != nil {
			return nil, err
		}
		links[location] = linkNavigation{destination: array}
	}
	for location, actions := range options.LinkActions {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if !pages[location.Page] {
			return nil, fmt.Errorf("PDF link page is not in the page tree")
		}
		action, err := r.navigationAction(ctx, actions, pages)
		if err != nil {
			return nil, err
		}
		links[location] = linkNavigation{action: action, replaceActions: true}
	}
	if len(options.OutlineDestinations) == 0 && len(options.OutlineTitles) == 0 && len(options.OutlineActions) == 0 {
		return links, nil
	}
	catalog, err := r.catalogDictionary()
	if err != nil {
		return nil, err
	}
	root, ok := catalog["Outlines"].(Reference)
	if !ok {
		return nil, fmt.Errorf("PDF outline root is not indirect")
	}
	parents := []Reference{root}
	destCount, titleCount, actionCount := 0, 0, 0
	err = r.WalkOutlines(ctx, func(level int, item OutlineItem) error {
		if level < 0 || level >= len(parents) {
			return fmt.Errorf("invalid PDF outline hierarchy")
		}
		parent := parents[level]
		parents = append(parents[:level+1], item.Reference)
		originalParent, _ := item.Dictionary["Parent"].(Reference)
		dest, changeDest := options.OutlineDestinations[item.Reference]
		title, changeTitle := options.OutlineTitles[item.Reference]
		actions, changeActions := options.OutlineActions[item.Reference]
		if !changeDest && !changeTitle && !changeActions && originalParent == parent {
			return nil
		}
		if _, exists := result[item.Reference]; exists {
			return fmt.Errorf("conflicting PDF outline replacement")
		}
		updated := maps.Clone(item.Dictionary)
		updated["Parent"] = parent
		if changeDest {
			array, err := r.rewriteDestination(dest, pages)
			if err != nil {
				return err
			}
			if err := r.replaceDestination(updated, array); err != nil {
				return err
			}
			destCount++
		}
		if changeTitle {
			encoded, err := EncodeTextString(title)
			if err != nil {
				return err
			}
			updated["Title"] = encoded
			titleCount++
		}
		if changeActions {
			action, err := r.navigationAction(ctx, actions, pages)
			if err != nil {
				return err
			}
			delete(updated, "Dest")
			delete(updated, "A")
			if action != nil {
				updated["A"] = action
			}
			actionCount++
		}
		result[item.Reference] = updated
		return nil
	})
	if err != nil {
		return nil, err
	}
	if destCount != len(options.OutlineDestinations) || titleCount != len(options.OutlineTitles) || actionCount != len(options.OutlineActions) {
		return nil, fmt.Errorf("PDF outline replacement is not in the outline tree")
	}
	return links, nil
}

// navigationAction 按给定顺序生成导航动作链，空序列不生成动作
// 入参: ctx 取消上下文, actions 导航序列, pages 文档页面引用
// 返回: Dictionary 首个动作及后续动作, error 参数或取消错误
func (r *Reader) navigationAction(ctx context.Context, actions []NavigationAction, pages map[Reference]bool) (Dictionary, error) {
	if len(actions) == 0 {
		return nil, nil
	}
	list := make(Array, 0, len(actions))
	for _, action := range actions {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if (action.Destination == nil) == (action.URI == "") {
			return nil, fmt.Errorf("invalid PDF navigation action")
		}
		if action.Destination != nil {
			dest, err := r.rewriteDestination(*action.Destination, pages)
			if err != nil {
				return nil, err
			}
			list = append(list, Dictionary{"S": Name("GoTo"), "D": dest})
		} else {
			uri, err := navigationURI(action.URI)
			if err != nil {
				return nil, err
			}
			list = append(list, Dictionary{"S": Name("URI"), "URI": uri})
		}
	}
	first := list[0].(Dictionary)
	if len(list) > 1 {
		first["Next"] = list[1:]
	}
	return first, nil
}

// navigationURI 校验URI并将非ASCII字节及非法URI字符转换为百分号编码
// 入参: value UTF-8地址
// 返回: String ASCII地址, error 编码或地址错误
func navigationURI(value string) (String, error) {
	if !utf8.ValidString(value) {
		return nil, fmt.Errorf("invalid PDF URI encoding")
	}
	if _, err := url.PathUnescape(value); err != nil {
		return nil, fmt.Errorf("invalid PDF URI: %w", err)
	}
	parsed, err := url.Parse(value)
	if err != nil {
		return nil, fmt.Errorf("invalid PDF URI: %w", err)
	}
	value = parsed.String()
	var result strings.Builder
	result.Grow(len(value))
	const hex = "0123456789ABCDEF"
	for i := range len(value) {
		c := value[i]
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.ContainsRune("-._~:/?#[]@!$&'()*+,;=%", rune(c)) {
			result.WriteByte(c)
		} else {
			result.WriteByte('%')
			result.WriteByte(hex[c>>4])
			result.WriteByte(hex[c&15])
		}
	}
	return String(result.String()), nil
}

// rewriteDestination 校验目标页和显示参数，保留空坐标及零缩放的原有语义
// 入参: dest 跳转目标, pages 文档页面引用
// 返回: Array 显式目标, error 页面或参数错误
func (r *Reader) rewriteDestination(dest Destination, pages map[Reference]bool) (Array, error) {
	if !pages[dest.Page] {
		return nil, fmt.Errorf("%w: page is not in the page tree", ErrInvalidDestination)
	}
	count, known := destinationParameterCount(dest.Mode)
	if !known || len(dest.Parameters) != count {
		return nil, fmt.Errorf("%w: mode or parameter count", ErrInvalidDestination)
	}
	array := append(Array{dest.Page, dest.Mode}, dest.Parameters...)
	mode, parameters, err := r.destinationParameters(array)
	if err != nil {
		return nil, err
	}
	if mode == "FitR" {
		values := [4]float64{}
		for i, value := range parameters {
			switch number := value.(type) {
			case Integer:
				values[i] = float64(number)
			case Real:
				values[i] = float64(number)
			}
		}
		if values[0] >= values[2] || values[1] >= values[3] {
			return nil, fmt.Errorf("%w: empty rectangle", ErrInvalidDestination)
		}
	}
	return append(Array{dest.Page, mode}, parameters...), nil
}

// replaceDestination 更新独立目标或GoTo动作，不修改共享动作及其后续动作
// 入参: dict 已复制的链接或大纲字典, destination 显式目标
// 返回: error 动作解析错误
func (r *Reader) replaceDestination(dict Dictionary, destination Array) error {
	value, err := r.Resolve(dict["A"])
	if err != nil {
		return err
	}
	if action, ok := value.(Dictionary); ok {
		kind, err := r.Resolve(action["S"])
		if err != nil {
			return err
		}
		if kind == Name("GoTo") {
			updated := maps.Clone(action)
			updated["D"] = destination
			dict["A"] = updated
			delete(dict, "Dest")
			return nil
		}
	}
	delete(dict, "A")
	dict["Dest"] = destination
	return nil
}
