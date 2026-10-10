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
)

// lifecycleReplacements 替换文档打开及页面打开、关闭动作，不执行事件
// 入参: ctx 取消上下文, options 替换配置, result 已有替换对象, resources 动作资源
// 返回: error 页面、动作或取消错误
func (r *Reader) lifecycleReplacements(ctx context.Context, options RewriteOptions, result map[Reference]Object, resources *rewriteResources) error {
	if options.OpenActions == nil && len(options.PageOpenActions) == 0 && len(options.PageCloseActions) == 0 {
		return nil
	}
	pages := make(map[Reference]bool)
	if err := r.WalkPages(ctx, func(_ int, page *Page) error {
		pages[page.Reference] = true
		return nil
	}); err != nil {
		return err
	}
	if options.OpenActions != nil {
		action, err := r.navigationAction(ctx, options.OpenActions, pages, resources)
		if err != nil {
			return err
		}
		ref, ok := r.Trailer["Root"].(Reference)
		if !ok {
			return fmt.Errorf("PDF catalog is not indirect")
		}
		catalog, err := r.rewriteDictionary(ref, result)
		if err != nil {
			return err
		}
		delete(catalog, "OpenAction")
		if action != nil {
			catalog["OpenAction"] = action
		}
		result[ref] = catalog
	}
	for _, event := range []struct {
		name  Name
		pages map[Reference][]NavigationAction
	}{{"O", options.PageOpenActions}, {"C", options.PageCloseActions}} {
		for ref, actions := range event.pages {
			if err := ctx.Err(); err != nil {
				return err
			}
			if !pages[ref] {
				return fmt.Errorf("PDF action page is not in the page tree")
			}
			action, err := r.navigationAction(ctx, actions, pages, resources)
			if err != nil {
				return err
			}
			page, err := r.rewriteDictionary(ref, result)
			if err != nil {
				return err
			}
			value, err := r.Resolve(page["AA"])
			if err != nil {
				return err
			}
			additional := make(Dictionary)
			if value != nil {
				original, ok := value.(Dictionary)
				if !ok {
					return fmt.Errorf("invalid PDF page additional actions")
				}
				additional = maps.Clone(original)
			}
			delete(additional, event.name)
			if action != nil {
				additional[event.name] = action
			}
			delete(page, "AA")
			if len(additional) != 0 {
				page["AA"] = additional
			}
			result[ref] = page
		}
	}
	return ctx.Err()
}

// pageActionVersion 计算页面打开、关闭动作所需的最低版本，不继承父级事件
// 入参: ctx 取消上下文, dict 待写字典
// 返回: string 最低版本, error 类型、动作或取消错误
func (r *Reader) pageActionVersion(ctx context.Context, dict Dictionary) (string, error) {
	version := "1.0"
	if dict["AA"] == nil {
		return version, nil
	}
	kind, err := r.Resolve(dict["Type"])
	if err != nil || kind != Name("Page") {
		return version, err
	}
	value, err := r.Resolve(dict["AA"])
	if err != nil || value == nil {
		return version, err
	}
	additional, ok := value.(Dictionary)
	if !ok {
		return "", fmt.Errorf("invalid PDF page additional actions")
	}
	for _, key := range []Name{"O", "C"} {
		if additional[key] == nil {
			continue
		}
		required, err := r.navigationVersion(ctx, additional[key])
		if err != nil {
			return "", err
		}
		version = max(version, "1.2", required)
	}
	return version, ctx.Err()
}

// rewriteDictionary 复制当前替换字典或源字典，隔离后续修改
// 入参: ref 对象引用, replacements 已有替换对象
// 返回: Dictionary 字典副本, error 引用或类型错误
func (r *Reader) rewriteDictionary(ref Reference, replacements map[Reference]Object) (Dictionary, error) {
	value, exists := replacements[ref]
	if !exists {
		var err error
		value, err = r.Object(ref)
		if err != nil {
			return nil, err
		}
	}
	dict, ok := value.(Dictionary)
	if !ok || dict == nil {
		return nil, fmt.Errorf("invalid PDF replacement dictionary")
	}
	return maps.Clone(dict), nil
}
