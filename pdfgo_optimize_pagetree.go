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
	"maps"
)

// pageTreeDegree 定义平衡页面树各内部节点的最大分支度
const pageTreeDegree = 64

// balancedPageTree 保存平衡页面树重建产生的覆盖字典、移除节点与新增中间节点
type balancedPageTree struct {
	overrides map[Reference]Object
	removed   map[Reference]bool
	added     []Reference
}

// buildBalancedPageTree 在页面数超阈值且原树过宽或过深时重建度为64的平衡Pages树
// 复用原根Pages引用并下推被移除中间节点的可继承属性，存在外部引用或未知扩展键时保守跳过
// 入参: ctx 取消上下文, trailer 尾字典, refs 当前可达对象, isStream 流对象标记, metadata 对象属性映射, maxID 当前最大对象号指针
// 返回: *balancedPageTree 重建结果，无需或不宜重建时为nil, error 取消错误
func buildBalancedPageTree(ctx context.Context, trailer Dictionary, refs []Reference, isStream map[Reference]bool, metadata map[Reference]Object, maxID *int64) (*balancedPageTree, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	rootRef, ok := trailer["Root"].(Reference)
	if !ok {
		return nil, nil
	}
	catalog, ok := metadata[rootRef].(Dictionary)
	if !ok || isStream[rootRef] {
		return nil, nil
	}
	rootPagesRef, ok := catalog["Pages"].(Reference)
	if !ok || isStream[rootPagesRef] {
		return nil, nil
	}
	rootPagesDict, ok := metadata[rootPagesRef].(Dictionary)
	if !ok || rootPagesDict["Type"] != Name("Pages") {
		return nil, nil
	}
	type inheritedAttrs struct {
		resources Object
		mediaBox  Object
		cropBox   Object
		rotation  Object
	}
	type pageEntry struct {
		ref   Reference
		dict  Dictionary
		attrs inheritedAttrs
	}
	var pages []pageEntry
	oldInternal := make(map[Reference]bool)
	seenNodes := make(map[Reference]bool)
	wide := false
	maxDepth := 0
	var walk func(ref Reference, depth int, attrs inheritedAttrs, isRoot bool) bool
	walk = func(ref Reference, depth int, attrs inheritedAttrs, isRoot bool) bool {
		if ctx.Err() != nil || depth > 256 || seenNodes[ref] || isStream[ref] {
			return false
		}
		dict, ok := metadata[ref].(Dictionary)
		if !ok {
			return false
		}
		seenNodes[ref] = true
		if depth > maxDepth {
			maxDepth = depth
		}
		switch dict["Type"] {
		case Name("Pages"):
			if !isRoot {
				for key := range dict {
					switch key {
					case "Type", "Parent", "Kids", "Count", "Resources", "MediaBox", "CropBox", "Rotate":
					default:
						return false
					}
				}
				oldInternal[ref] = true
				if dict["Resources"] != nil {
					attrs.resources = dict["Resources"]
				}
				if dict["MediaBox"] != nil {
					attrs.mediaBox = dict["MediaBox"]
				}
				if dict["CropBox"] != nil {
					attrs.cropBox = dict["CropBox"]
				}
				if dict["Rotate"] != nil {
					attrs.rotation = dict["Rotate"]
				}
			}
			kids, ok := dict["Kids"].(Array)
			if !ok {
				return false
			}
			if len(kids) > pageTreeDegree {
				wide = true
			}
			for _, kid := range kids {
				kidRef, ok := kid.(Reference)
				if !ok {
					return false
				}
				if !walk(kidRef, depth+1, attrs, false) {
					return false
				}
			}
			return true
		case Name("Page"):
			pages = append(pages, pageEntry{ref: ref, dict: dict, attrs: attrs})
			return true
		default:
			return false
		}
	}
	if !walk(rootPagesRef, 0, inheritedAttrs{}, true) {
		return nil, ctx.Err()
	}
	if len(pages) <= pageTreeDegree {
		return nil, ctx.Err()
	}
	idealDepth := 1
	for capacity := pageTreeDegree; capacity < len(pages); capacity *= pageTreeDegree {
		idealDepth++
	}
	if !wide && maxDepth <= idealDepth+2 {
		return nil, ctx.Err()
	}
	if len(oldInternal) > 0 {
		pageSet := make(map[Reference]bool, len(pages))
		for _, page := range pages {
			pageSet[page.ref] = true
		}
		var hasExternalRef func(val Object, depth int) bool
		hasExternalRef = func(val Object, depth int) bool {
			if depth > 256 {
				return true
			}
			switch v := val.(type) {
			case Reference:
				return oldInternal[v]
			case Array:
				for _, item := range v {
					if hasExternalRef(item, depth+1) {
						return true
					}
				}
			case Dictionary:
				for _, item := range v {
					if hasExternalRef(item, depth+1) {
						return true
					}
				}
			case *Stream:
				return hasExternalRef(v.Dictionary, depth+1)
			}
			return false
		}
		if hasExternalRef(trailer, 0) {
			return nil, ctx.Err()
		}
		for _, ref := range refs {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			if oldInternal[ref] {
				continue
			}
			if ref == rootPagesRef {
				for key, item := range rootPagesDict {
					if key == "Kids" {
						continue
					}
					if hasExternalRef(item, 0) {
						return nil, ctx.Err()
					}
				}
				continue
			}
			if pageSet[ref] {
				if dict, ok := metadata[ref].(Dictionary); ok {
					for key, item := range dict {
						if key == "Parent" {
							continue
						}
						if hasExternalRef(item, 0) {
							return nil, ctx.Err()
						}
					}
				}
				continue
			}
			if hasExternalRef(metadata[ref], 0) {
				return nil, ctx.Err()
			}
		}
	}
	type treeNode struct {
		ref   Reference
		count int
		dict  Dictionary
	}
	overrides := make(map[Reference]Object, len(pages)+len(pages)/pageTreeDegree+2)
	current := make([]treeNode, len(pages))
	for i, page := range pages {
		cloned := maps.Clone(page.dict)
		if cloned["Resources"] == nil && page.attrs.resources != nil {
			cloned["Resources"] = page.attrs.resources
		}
		if cloned["MediaBox"] == nil && page.attrs.mediaBox != nil {
			cloned["MediaBox"] = page.attrs.mediaBox
		}
		if cloned["CropBox"] == nil && page.attrs.cropBox != nil {
			cloned["CropBox"] = page.attrs.cropBox
		}
		if cloned["Rotate"] == nil && page.attrs.rotation != nil {
			cloned["Rotate"] = page.attrs.rotation
		}
		overrides[page.ref] = cloned
		current[i] = treeNode{ref: page.ref, count: 1, dict: cloned}
	}
	var added []Reference
	for len(current) > pageTreeDegree {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		next := make([]treeNode, 0, (len(current)+pageTreeDegree-1)/pageTreeDegree)
		for start := 0; start < len(current); start += pageTreeDegree {
			end := min(start+pageTreeDegree, len(current))
			*maxID++
			parentRef := Reference{Number: *maxID}
			kids := make(Array, 0, end-start)
			count := 0
			for _, child := range current[start:end] {
				child.dict["Parent"] = parentRef
				kids = append(kids, child.ref)
				count += child.count
			}
			nodeDict := Dictionary{"Type": Name("Pages"), "Kids": kids, "Count": Integer(count)}
			overrides[parentRef] = nodeDict
			added = append(added, parentRef)
			next = append(next, treeNode{ref: parentRef, count: count, dict: nodeDict})
		}
		current = next
	}
	rootDict := maps.Clone(rootPagesDict)
	delete(rootDict, "Parent")
	rootKids := make(Array, 0, len(current))
	totalCount := 0
	for _, child := range current {
		child.dict["Parent"] = rootPagesRef
		rootKids = append(rootKids, child.ref)
		totalCount += child.count
	}
	rootDict["Type"] = Name("Pages")
	rootDict["Kids"] = rootKids
	rootDict["Count"] = Integer(totalCount)
	overrides[rootPagesRef] = rootDict
	return &balancedPageTree{overrides: overrides, removed: oldInternal, added: added}, ctx.Err()
}
