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

// NavigationAction 描述导航或媒体播放动作，须且仅须指定一项，库不访问外部资源、不执行脚本或播放媒体
// URI使用UTF-8或已转义地址并转为ASCII写出；External按结构化字段写出（忽略File.Dictionary），File.Name使用路径或URL
// Sound音量零值为静音，写出期间源流及文件数据须保持可读；Rendition引用原有或新增屏幕注解
// Movie指向原有或新增视频注解（未指定Annotation和Target时按Title查找，跨页播放须先跳转），Activation仅写出显式参数且引用须属当前文件
type NavigationAction struct {
	Destination *Destination
	External    *ExternalGoToAction
	Attachment  *AttachmentAction
	Sound       *SoundAction
	Movie       *MovieAction
	Rendition   *RenditionAction
	URI         string
}

// linkNavigation 保存链接目标替换或完整动作序列替换
type linkNavigation struct {
	destination    Array
	action         Dictionary
	replaceActions bool
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

// normalizeOutlineRefs 将大纲修改映射中的间接引用链规范化为最终目录项对象引用
// 入参: r 关联阅读器, source 原始大纲修改映射
// 返回: map[Reference]T 规范化后的映射, error 引用循环或冲突错误
func normalizeOutlineRefs[T any](r *Reader, source map[Reference]T) (map[Reference]T, error) {
	if len(source) == 0 {
		return nil, nil
	}
	normalized := make(map[Reference]T, len(source))
	for ref, value := range source {
		target := ref
		if ref.Number > 0 && ref.Generation >= 0 && ref.Generation <= 65535 {
			resolved, err := r.resolveIndirectReference(ref)
			if err != nil {
				return nil, err
			}
			if resolved != (Reference{}) {
				target = resolved
			}
		}
		if _, exists := normalized[target]; exists {
			return nil, fmt.Errorf("conflicting PDF outline replacement")
		}
		normalized[target] = value
	}
	return normalized, nil
}

// navigationReplacements 校验导航修改并按实际层级更新大纲节点
// 入参: ctx 取消上下文, options 替换配置, result 已有替换对象, resources 动作资源
// 返回: map[AnnotationLocation]linkNavigation 链接导航修改, error 页面、目标或大纲错误
func (r *Reader) navigationReplacements(ctx context.Context, options RewriteOptions, result map[Reference]Object, resources *rewriteResources) (map[AnnotationLocation]linkNavigation, error) {
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
	outlineDestinations, err := normalizeOutlineRefs(r, options.OutlineDestinations)
	if err != nil {
		return nil, err
	}
	outlineTitles, err := normalizeOutlineRefs(r, options.OutlineTitles)
	if err != nil {
		return nil, err
	}
	outlineActions, err := normalizeOutlineRefs(r, options.OutlineActions)
	if err != nil {
		return nil, err
	}
	for ref, actions := range outlineActions {
		if _, exists := outlineDestinations[ref]; exists {
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
		action, err := r.navigationAction(ctx, actions, pages, resources)
		if err != nil {
			return nil, err
		}
		links[location] = linkNavigation{action: action, replaceActions: true}
	}
	if len(outlineDestinations) == 0 && len(outlineTitles) == 0 && len(outlineActions) == 0 {
		return links, nil
	}
	catalog, err := r.catalogDictionary()
	if err != nil {
		return nil, err
	}
	root, err := r.resolveIndirectReference(catalog["Outlines"])
	if err != nil {
		return nil, err
	}
	if root == (Reference{}) {
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
		dest, changeDest := outlineDestinations[item.Reference]
		title, changeTitle := outlineTitles[item.Reference]
		actions, changeActions := outlineActions[item.Reference]
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
			action, err := r.navigationAction(ctx, actions, pages, resources)
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
	if destCount != len(outlineDestinations) || titleCount != len(outlineTitles) || actionCount != len(outlineActions) {
		return nil, fmt.Errorf("PDF outline replacement is not in the outline tree")
	}
	return links, nil
}

// navigationAction 按给定顺序生成导航动作链，空序列不生成动作
// 入参: ctx 取消上下文, actions 动作序列, pages 文档页面引用, resources 动作资源
// 返回: Dictionary 首个动作及后续动作, error 参数或取消错误
func (r *Reader) navigationAction(ctx context.Context, actions []NavigationAction, pages map[Reference]bool, resources *rewriteResources) (Dictionary, error) {
	if len(actions) == 0 {
		return nil, nil
	}
	list := make(Array, 0, len(actions))
	for _, action := range actions {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		count := 0
		if action.Destination != nil {
			count++
		}
		if action.External != nil {
			count++
		}
		if action.Attachment != nil {
			count++
		}
		if action.Sound != nil {
			count++
		}
		if action.Movie != nil {
			count++
		}
		if action.Rendition != nil {
			count++
		}
		if action.URI != "" {
			count++
		}
		if count != 1 {
			return nil, fmt.Errorf("invalid PDF navigation action")
		}
		if action.Destination != nil {
			dest, err := r.rewriteDestination(*action.Destination, pages)
			if err != nil {
				return nil, err
			}
			list = append(list, Dictionary{"S": Name("GoTo"), "D": dest})
		} else if action.External != nil {
			external, err := r.rewriteExternalGoTo(ctx, *action.External)
			if err != nil {
				return nil, err
			}
			list = append(list, external)
		} else if action.Attachment != nil {
			if resources == nil {
				return nil, fmt.Errorf("missing PDF action resources")
			}
			file, ok := resources.attachments[action.Attachment.Key]
			if !ok {
				return nil, fmt.Errorf("PDF attachment target not found")
			}
			dict := Dictionary{"S": Name("Launch"), "F": file}
			if action.Attachment.NewWindow != nil {
				dict["NewWindow"] = Boolean(*action.Attachment.NewWindow)
			}
			list = append(list, dict)
		} else if action.Sound != nil {
			if resources == nil {
				return nil, fmt.Errorf("missing PDF action resources")
			}
			ref, ok := resources.sounds[action.Sound]
			if !ok {
				return nil, fmt.Errorf("missing PDF sound resource")
			}
			dict := Dictionary{"S": Name("Sound"), "Sound": ref, "Volume": Real(action.Sound.Volume), "Mix": Boolean(action.Sound.Mix)}
			if action.Sound.Repeat {
				dict["Repeat"] = Boolean(true)
			} else if action.Sound.Synchronous {
				dict["Synchronous"] = Boolean(true)
			}
			list = append(list, dict)
		} else if action.Movie != nil {
			if resources == nil || resources.movies[action.Movie] == nil {
				return nil, fmt.Errorf("missing PDF movie action")
			}
			list = append(list, maps.Clone(resources.movies[action.Movie]))
		} else if action.Rendition != nil {
			if resources == nil || resources.renditions[action.Rendition] == nil {
				return nil, fmt.Errorf("missing PDF rendition action")
			}
			list = append(list, maps.Clone(resources.renditions[action.Rendition]))
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

// rewriteDestination 校验目标页和显示参数，保留空坐标及零缩放的原有语义
// 入参: dest 跳转目标, pages 文档页面引用
// 返回: Array 显式目标, error 页面或参数错误
func (r *Reader) rewriteDestination(dest Destination, pages map[Reference]bool) (Array, error) {
	if !pages[dest.Page] {
		return nil, fmt.Errorf("%w: page is not in the page tree", ErrInvalidDestination)
	}
	return r.rewriteDestinationArray(dest.Page, dest.Mode, dest.Parameters)
}

// rewriteDestinationArray 校验显示参数，分别保留本地页引用和远程页码
// 入参: page 页引用或零基页码, mode 显示模式, values 显示参数
// 返回: Array 显式目标, error 参数错误
func (r *Reader) rewriteDestinationArray(page Object, mode Name, values Array) (Array, error) {
	count, known := destinationParameterCount(mode)
	if !known || len(values) != count {
		return nil, fmt.Errorf("%w: mode or parameter count", ErrInvalidDestination)
	}
	array := append(Array{page, mode}, values...)
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
	return append(Array{page, mode}, parameters...), nil
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
