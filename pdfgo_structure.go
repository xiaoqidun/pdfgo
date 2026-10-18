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
)

// StructureTree 保存逻辑结构根、角色映射和属性类，树索引及扩展信息保留在原始字典中
type StructureTree struct {
	Reference  Reference
	RoleMap    Dictionary
	ClassMap   Dictionary
	Dictionary Dictionary
}

// StructureItem 保存结构元素或内容引用，Type为StructElem、MCR或OBJR
// Tag和Revision仅用于结构元素，MCID仅用于MCR，Target仅用于OBJR
// Page对内容引用采用其自身或直属结构元素的Pg，不继承更上层元素的Pg
// Stream为空时MCR引用页面内容流，整数形式的MCR没有Dictionary
type StructureItem struct {
	Reference   Reference
	Parent      Reference
	Page        Reference
	Type        Name
	Tag         Name
	Revision    int64
	MCID        int64
	Stream      Reference
	StreamOwner Reference
	Target      Reference
	Dictionary  Dictionary
}

// StructureAttribute 保存结构属性的所属方、类名和修订号，不合并同名属性
// Class为空指针表示元素直接声明的属性，Stream非空时属性内容由调用方按需解码
type StructureAttribute struct {
	Reference  Reference
	Owner      Name
	Class      *Name
	Revision   int64
	Dictionary Dictionary
	Stream     *Stream
}

// StructureContent 保存内容引用的容器及父树索引，Stream非空时容器为数据流
// Reference对MCR为页面或独立内容流，对OBJR为目标对象
type StructureContent struct {
	Reference  Reference
	ParentKey  int64
	Dictionary Dictionary
	Stream     *Stream
}

// StructureParent 保存父树键及结构元素引用，MCID为-1表示整对象，非负值表示内容流中的标记
type StructureParent struct {
	Element Reference
	Key     int64
	MCID    int64
}

// StructureContent 解析MCR或OBJR的内容容器及父树键，不展开父树或解码流
// 入参: item 内容引用
// 返回: StructureContent 内容容器, error 引用或父树键错误
func (r *Reader) StructureContent(item StructureItem) (StructureContent, error) {
	var result StructureContent
	ref, keyName, otherName := item.Target, Name("StructParent"), Name("StructParents")
	switch item.Type {
	case "MCR":
		if item.MCID < 0 {
			return result, fmt.Errorf("invalid structure marked-content identifier")
		}
		if item.Page == (Reference{}) {
			return result, fmt.Errorf("missing structure content page")
		}
		ref, keyName, otherName = item.Page, otherName, keyName
		if item.Stream != (Reference{}) {
			ref = item.Stream
		}
	case "OBJR":
	default:
		return result, fmt.Errorf("invalid structure content reference")
	}
	if ref == (Reference{}) {
		return result, fmt.Errorf("missing structure content reference")
	}
	value, err := r.Resolve(ref)
	if err != nil {
		return result, err
	}
	switch value := value.(type) {
	case Dictionary:
		result.Dictionary = value
	case *Stream:
		if value != nil {
			result.Stream, result.Dictionary = value, value.Dictionary
		}
	}
	if result.Dictionary == nil {
		return StructureContent{}, fmt.Errorf("invalid structure content target")
	}
	if item.Type == "MCR" {
		if item.Stream != (Reference{}) {
			if result.Stream == nil {
				return StructureContent{}, fmt.Errorf("invalid structure content stream")
			}
		} else {
			kind, err := r.Resolve(result.Dictionary["Type"])
			if err != nil {
				return StructureContent{}, err
			}
			if result.Stream != nil || kind != Name("Page") {
				return StructureContent{}, fmt.Errorf("invalid structure content page")
			}
		}
	}
	key, err := r.Resolve(result.Dictionary[keyName])
	if err != nil {
		return StructureContent{}, err
	}
	parent, ok := key.(Integer)
	if !ok || parent < 0 {
		return StructureContent{}, fmt.Errorf("invalid structure content parent key")
	}
	parents, err := r.Resolve(result.Dictionary[otherName])
	if err != nil {
		return StructureContent{}, err
	}
	if parents != nil {
		return StructureContent{}, fmt.Errorf("conflicting structure parent entries")
	}
	result.Reference, result.ParentKey = ref, int64(parent)
	return result, nil
}

// StructureTree 读取逻辑结构入口，不展开属性类或改写角色映射
// 返回: *StructureTree 结构入口，未声明时为空, error 解析或类型错误
func (r *Reader) StructureTree() (*StructureTree, error) {
	catalog, err := r.catalogDictionary()
	if err != nil {
		return nil, err
	}
	value, err := r.Resolve(catalog["StructTreeRoot"])
	if err != nil || value == nil {
		return nil, err
	}
	dict, ok := value.(Dictionary)
	if !ok {
		return nil, fmt.Errorf("invalid structure tree root")
	}
	kind, err := r.Resolve(dict["Type"])
	if err != nil {
		return nil, err
	}
	if kind != Name("StructTreeRoot") {
		return nil, fmt.Errorf("invalid structure tree type")
	}
	tree := &StructureTree{Dictionary: dict}
	tree.Reference, _ = catalog["StructTreeRoot"].(Reference)
	for _, field := range []struct {
		name Name
		dest *Dictionary
	}{{"RoleMap", &tree.RoleMap}, {"ClassMap", &tree.ClassMap}} {
		value, err := r.Resolve(dict[field.name])
		if err != nil {
			return nil, err
		}
		if value != nil {
			mapping, ok := value.(Dictionary)
			if !ok {
				return nil, fmt.Errorf("invalid structure %s", field.name)
			}
			*field.dest = mapping
		}
	}
	return tree, nil
}

// ResolveStructureRole 沿角色映射查找调用方支持的角色，循环映射在重复名称处结束
// 已声明映射时先映射再识别，未匹配时仍可从结构元素保留原始标签
// 入参: ctx 取消上下文, tag 原始角色, accepts 支持的角色判断函数
// 返回: Name 匹配角色, bool 是否匹配, error 解析、类型或取消错误
func (r *Reader) ResolveStructureRole(ctx context.Context, tag Name, accepts func(Name) bool) (Name, bool, error) {
	if err := ctx.Err(); err != nil {
		return "", false, err
	}
	tree, err := r.StructureTree()
	if err != nil {
		return "", false, err
	}
	var roles Dictionary
	if tree != nil {
		roles = tree.RoleMap
	}
	seen := make(map[Name]bool)
	for {
		if err := ctx.Err(); err != nil {
			return "", false, err
		}
		value, err := r.Resolve(roles[tag])
		if err != nil {
			return "", false, err
		}
		mapped := value != nil
		if mapped {
			var ok bool
			tag, ok = value.(Name)
			if !ok {
				return "", false, fmt.Errorf("invalid structure role mapping")
			}
		}
		if seen[tag] {
			return "", false, nil
		}
		seen[tag] = true
		accepted := accepts(tag)
		if err := ctx.Err(); err != nil {
			return "", false, err
		}
		if accepted {
			return tag, true, nil
		}
		if !mapped {
			return "", false, nil
		}
	}
}

// WalkStructure 按K声明顺序先序访问结构元素和内容引用，根的子项从零层开始
// 原始属性和引用由调用方按需解析，不读取页面内容流
// 入参: ctx 取消上下文, visit 零基层级和结构项的访问函数
// 返回: error 解析、结构、循环、取消或访问错误
func (r *Reader) WalkStructure(ctx context.Context, visit func(int, StructureItem) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	tree, err := r.StructureTree()
	if err != nil || tree == nil {
		return err
	}
	type frame struct {
		children Array
		parent   Reference
		page     Reference
	}
	children, err := r.structureChildren(tree.Dictionary["K"])
	if err != nil {
		return err
	}
	stack := []frame{{children: children, parent: tree.Reference}}
	seen := make(map[Reference]bool)
	for len(stack) > 0 {
		if err := ctx.Err(); err != nil {
			return err
		}
		current := &stack[len(stack)-1]
		if len(current.children) == 0 {
			stack = stack[:len(stack)-1]
			continue
		}
		object := current.children[0]
		current.children = current.children[1:]
		item, err := r.structureItem(object, current.parent, current.page)
		if err != nil {
			return err
		}
		if len(stack) == 1 && item.Type != "StructElem" {
			return fmt.Errorf("structure root child is not an element")
		}
		if item.Type == "StructElem" && item.Reference != (Reference{}) {
			if seen[item.Reference] {
				return fmt.Errorf("repeated structure element")
			}
			seen[item.Reference] = true
		}
		if err := visit(len(stack)-1, item); err != nil {
			return err
		}
		if item.Type == "StructElem" {
			children, err := r.structureChildren(item.Dictionary["K"])
			if err != nil {
				return err
			}
			if len(children) > 0 {
				stack = append(stack, frame{children: children, parent: item.Reference, page: item.Page})
			}
		}
	}
	return ctx.Err()
}

// StructureAttributes 展开结构元素的属性类和直接属性，类属性在前，直接属性在后
// 同名属性以直接声明为准，修订号不匹配的属性仍保留，不修改共享属性对象
// 入参: ctx 取消上下文, item 结构元素
// 返回: []StructureAttribute 属性列表, error 解析、结构或取消错误
func (r *Reader) StructureAttributes(ctx context.Context, item StructureItem) ([]StructureAttribute, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if item.Type != "StructElem" || item.Dictionary == nil {
		return nil, fmt.Errorf("structure attributes require an element")
	}
	var result []StructureAttribute
	appendAttribute := func(object Object, class *Name, revision int64) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		value, err := r.Resolve(object)
		if err != nil {
			return err
		}
		attribute := StructureAttribute{Class: class, Revision: revision}
		attribute.Reference, _ = object.(Reference)
		switch value := value.(type) {
		case Dictionary:
			attribute.Dictionary = value
		case *Stream:
			attribute.Stream, attribute.Dictionary = value, value.Dictionary
		default:
			return fmt.Errorf("invalid structure attribute object")
		}
		owner, err := r.Resolve(attribute.Dictionary["O"])
		if err != nil {
			return err
		}
		var ok bool
		attribute.Owner, ok = owner.(Name)
		if !ok {
			return fmt.Errorf("invalid structure attribute owner")
		}
		result = append(result, attribute)
		return nil
	}
	classes, err := r.Resolve(item.Dictionary["C"])
	if err != nil {
		return nil, err
	}
	if classes != nil {
		tree, err := r.StructureTree()
		if err != nil {
			return nil, err
		}
		if tree == nil {
			return nil, fmt.Errorf("missing structure class map")
		}
		err = r.walkStructureRevisions(ctx, classes, func(object Object, revision int64) error {
			value, err := r.Resolve(object)
			if err != nil {
				return err
			}
			class, ok := value.(Name)
			if !ok {
				return fmt.Errorf("invalid structure attribute class")
			}
			value, err = r.Resolve(tree.ClassMap[class])
			if err != nil {
				return err
			}
			if objects, ok := value.(Array); ok {
				for _, object := range objects {
					if err := appendAttribute(object, &class, revision); err != nil {
						return err
					}
				}
				return nil
			}
			return appendAttribute(tree.ClassMap[class], &class, revision)
		})
		if err != nil {
			return nil, err
		}
	}
	if err := r.walkStructureRevisions(ctx, item.Dictionary["A"], func(object Object, revision int64) error {
		return appendAttribute(object, nil, revision)
	}); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return result, nil
}

// WalkStructureIDs 按名称树顺序访问元素标识和对应结构元素，标识保留原始字节
// 校验索引名称与元素ID一致，不扫描结构层级或检查元素归属
// 入参: ctx 取消上下文, visit 标识及结构元素的访问函数
// 返回: error 名称树、元素标识、取消或访问错误
func (r *Reader) WalkStructureIDs(ctx context.Context, visit func(string, StructureItem) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	tree, err := r.StructureTree()
	if err != nil || tree == nil {
		return err
	}
	return r.WalkNameTree(ctx, tree.Dictionary["IDTree"], func(id string, object Object) error {
		item, err := r.structureItem(object, Reference{}, Reference{})
		if err != nil {
			return err
		}
		if item.Type != "StructElem" {
			return fmt.Errorf("structure identifier target is not an element")
		}
		value, err := r.Resolve(item.Dictionary["ID"])
		if err != nil {
			return err
		}
		text, ok := value.(String)
		if !ok || string(text) != id {
			return fmt.Errorf("structure identifier does not match element")
		}
		return visit(id, item)
	})
}

// WalkStructureParents 按父树键及MCID递增访问父元素引用，跳过标记数组中的空槽
// 复用数字树校验，不扫描页面内容或改变结构树
// 入参: ctx 取消上下文, visit 父元素引用访问函数
// 返回: error 数字树、元素引用、取消或访问错误
func (r *Reader) WalkStructureParents(ctx context.Context, visit func(StructureParent) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	tree, err := r.StructureTree()
	if err != nil || tree == nil {
		return err
	}
	root, err := r.Resolve(tree.Dictionary["ParentTree"])
	if err != nil || root == nil {
		return err
	}
	parent := func(key, mcid int64, object Object) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		ref, ok := object.(Reference)
		if !ok {
			return fmt.Errorf("structure parent is not indirect")
		}
		item, err := r.structureItem(ref, Reference{}, Reference{})
		if err != nil {
			return fmt.Errorf("structure parent %d %d: %w", ref.Number, ref.Generation, err)
		}
		if item.Type != "StructElem" {
			return fmt.Errorf("structure parent is not an element")
		}
		if err := visit(StructureParent{Element: ref, Key: key, MCID: mcid}); err != nil {
			return err
		}
		return ctx.Err()
	}
	return r.WalkNumberTree(ctx, tree.Dictionary["ParentTree"], func(key int64, object Object) error {
		if key < 0 {
			return fmt.Errorf("invalid structure parent key")
		}
		value, err := r.Resolve(object)
		if err != nil {
			return err
		}
		if array, ok := value.(Array); ok {
			for index, element := range array {
				if err := ctx.Err(); err != nil {
					return err
				}
				value, err := r.Resolve(element)
				if err != nil {
					return err
				}
				if value == nil {
					continue
				}
				if err := parent(key, int64(index), element); err != nil {
					return err
				}
			}
			return ctx.Err()
		}
		return parent(key, -1, object)
	})
}

// walkStructureRevisions 读取属性或类名后的可选修订号，未指定时采用零
// 入参: ctx 取消上下文, object 属性或类声明, visit 属性及修订号访问函数
// 返回: error 解析、修订号或访问错误
func (r *Reader) walkStructureRevisions(ctx context.Context, object Object, visit func(Object, int64) error) error {
	value, err := r.Resolve(object)
	if err != nil || value == nil {
		return err
	}
	objects, array := value.(Array)
	if !array {
		objects = Array{object}
	}
	for i := 0; i < len(objects); i++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		object := objects[i]
		var revision int64
		if array && i+1 < len(objects) {
			value, err := r.Resolve(objects[i+1])
			if err != nil {
				return err
			}
			if number, ok := value.(Integer); ok {
				if number < 0 {
					return fmt.Errorf("invalid structure attribute revision")
				}
				revision = int64(number)
				i++
			}
		}
		if err := visit(object, revision); err != nil {
			return err
		}
	}
	return ctx.Err()
}

// structureChildren 读取K的单个子项或有序数组，保留间接子项的引用
// 入参: object 子项声明
// 返回: Array 子项, error 解析错误
func (r *Reader) structureChildren(object Object) (Array, error) {
	value, err := r.Resolve(object)
	if err != nil || value == nil {
		return nil, err
	}
	if children, ok := value.(Array); ok {
		return children, nil
	}
	return Array{object}, nil
}

// structureItem 解析结构元素、标记内容引用或对象引用
// 入参: object 原始子项, parent 直属父元素, page 父元素声明的页面
// 返回: StructureItem 结构项, error 解析或结构错误
func (r *Reader) structureItem(object Object, parent, page Reference) (StructureItem, error) {
	item := StructureItem{Parent: parent, Page: page}
	item.Reference, _ = object.(Reference)
	value, err := r.Resolve(object)
	if err != nil {
		return item, err
	}
	if id, ok := value.(Integer); ok {
		if id < 0 || page == (Reference{}) {
			return item, fmt.Errorf("invalid structure marked-content reference")
		}
		item.Type, item.MCID = "MCR", int64(id)
		return item, nil
	}
	dict, ok := value.(Dictionary)
	if !ok {
		return item, fmt.Errorf("invalid structure child")
	}
	item.Dictionary = dict
	kind, err := r.Resolve(dict["Type"])
	if err != nil {
		return item, err
	}
	if kind == nil {
		kind = Name("StructElem")
	}
	item.Type, ok = kind.(Name)
	if !ok {
		return item, fmt.Errorf("invalid structure child type")
	}
	if item.Type == "StructElem" {
		item.Page = Reference{}
		tag, err := r.Resolve(dict["S"])
		if err != nil {
			return item, err
		}
		item.Tag, ok = tag.(Name)
		if !ok {
			return item, fmt.Errorf("invalid structure element tag")
		}
		item.Parent, ok = dict["P"].(Reference)
		if !ok || parent != (Reference{}) && item.Parent != parent {
			return item, fmt.Errorf("invalid structure element parent")
		}
		revision, err := r.Resolve(dict["R"])
		if err != nil {
			return item, err
		}
		if revision != nil {
			n, ok := revision.(Integer)
			if !ok || n < 0 {
				return item, fmt.Errorf("invalid structure element revision")
			}
			item.Revision = int64(n)
		}
	} else if item.Type == "MCR" {
		id, err := r.Resolve(dict["MCID"])
		if err != nil {
			return item, err
		}
		n, ok := id.(Integer)
		if !ok || n < 0 {
			return item, fmt.Errorf("invalid structure marked-content identifier")
		}
		item.MCID = int64(n)
	} else if item.Type != "OBJR" {
		return item, fmt.Errorf("invalid structure child type")
	}
	for _, field := range []struct {
		name Name
		dest *Reference
		kind Name
	}{{"Pg", &item.Page, ""}, {"Stm", &item.Stream, "MCR"}, {"StmOwn", &item.StreamOwner, "MCR"}, {"Obj", &item.Target, "OBJR"}} {
		if field.kind != "" && field.kind != item.Type {
			continue
		}
		value := dict[field.name]
		if value == nil {
			continue
		}
		ref, ok := value.(Reference)
		if !ok {
			return item, fmt.Errorf("invalid structure %s reference", field.name)
		}
		*field.dest = ref
	}
	if item.Type != "StructElem" && item.Page == (Reference{}) {
		return item, fmt.Errorf("missing structure content page")
	}
	if item.Type == "OBJR" && item.Target == (Reference{}) {
		return item, fmt.Errorf("missing structure object reference")
	}
	return item, nil
}
