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

// WalkNameTree 按节点顺序访问名称树，名称保留原始字节，值不预先解析
// 入参: ctx 取消上下文, root 名称树根, visit 名称和值的访问函数
// 返回: error 结构、重复名称、循环或访问错误
func (r *Reader) WalkNameTree(ctx context.Context, root Object, visit func(string, Object) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	value, err := r.Resolve(root)
	if err != nil || value == nil {
		return err
	}
	seen := map[Reference]bool{}
	names := map[string]bool{}
	stack := []Array{{root}}
	for len(stack) > 0 {
		if err := ctx.Err(); err != nil {
			return err
		}
		top := &stack[len(stack)-1]
		if len(*top) == 0 {
			stack = stack[:len(stack)-1]
			continue
		}
		object := (*top)[0]
		*top = (*top)[1:]
		if ref, ok := object.(Reference); ok {
			if seen[ref] {
				return fmt.Errorf("repeated name tree node")
			}
			seen[ref] = true
		}
		value, err := r.Resolve(object)
		if err != nil {
			return err
		}
		node, ok := value.(Dictionary)
		if !ok {
			return fmt.Errorf("invalid name tree node")
		}
		value, err = r.Resolve(node["Names"])
		if err != nil {
			return err
		}
		if value != nil {
			pairs, ok := value.(Array)
			if !ok || len(pairs)%2 != 0 {
				return fmt.Errorf("invalid name tree pairs")
			}
			for i := 0; i < len(pairs); i += 2 {
				if err := ctx.Err(); err != nil {
					return err
				}
				value, err := r.Resolve(pairs[i])
				if err != nil {
					return err
				}
				name, ok := value.(String)
				if !ok {
					return fmt.Errorf("invalid name tree key")
				}
				key := string(name)
				if names[key] {
					return fmt.Errorf("duplicate name tree key")
				}
				names[key] = true
				if err := visit(key, pairs[i+1]); err != nil {
					return err
				}
			}
		}
		value, err = r.Resolve(node["Kids"])
		if err != nil {
			return err
		}
		if value != nil {
			kids, ok := value.(Array)
			if !ok {
				return fmt.Errorf("invalid name tree children")
			}
			for _, kid := range kids {
				if _, ok := kid.(Reference); !ok {
					return fmt.Errorf("name tree child is not indirect")
				}
			}
			stack = append(stack, kids)
		}
	}
	return ctx.Err()
}

// WalkNumberTree 按键递增访问数字树，值不预先解析
// 入参: ctx 取消上下文, root 数字树根, visit 数字键和值的访问函数
// 返回: error 结构、重复键、循环或访问错误
func (r *Reader) WalkNumberTree(ctx context.Context, root Object, visit func(int64, Object) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	value, err := r.Resolve(root)
	if err != nil || value == nil {
		return err
	}
	type frame struct {
		object  Object
		kids    Array
		next    int
		entered bool
		first   int64
		last    int64
		hasKeys bool
		limits  [2]int64
	}
	stack := []frame{{object: root}}
	seen := make(map[Reference]bool)
	var previous int64
	hasPrevious := false
	for len(stack) != 0 {
		if err := ctx.Err(); err != nil {
			return err
		}
		current := &stack[len(stack)-1]
		if current.entered {
			if current.next < len(current.kids) {
				object := current.kids[current.next]
				current.next++
				if _, ok := object.(Reference); !ok {
					return fmt.Errorf("number tree child is not indirect")
				}
				stack = append(stack, frame{object: object})
				continue
			}
			if len(stack) > 1 {
				if !current.hasKeys || current.limits != [2]int64{current.first, current.last} {
					return fmt.Errorf("number tree limits do not match keys")
				}
				parent := &stack[len(stack)-2]
				if !parent.hasKeys {
					parent.first, parent.hasKeys = current.first, true
				}
				parent.last = current.last
			}
			stack = stack[:len(stack)-1]
			continue
		}
		object := current.object
		if ref, ok := object.(Reference); ok {
			if seen[ref] {
				return fmt.Errorf("repeated number tree node")
			}
			seen[ref] = true
		}
		value, err := r.Resolve(object)
		if err != nil {
			return err
		}
		node, ok := value.(Dictionary)
		if !ok {
			return fmt.Errorf("invalid number tree node")
		}
		value, err = r.Resolve(node["Limits"])
		if err != nil {
			return err
		}
		if len(stack) == 1 {
			if value != nil {
				return fmt.Errorf("number tree root contains limits")
			}
		} else {
			limits, ok := value.(Array)
			if !ok || len(limits) != 2 {
				return fmt.Errorf("invalid number tree limits")
			}
			for index, object := range limits {
				value, err := r.Resolve(object)
				if err != nil {
					return err
				}
				key, ok := value.(Integer)
				if !ok {
					return fmt.Errorf("invalid number tree limit key")
				}
				current.limits[index] = int64(key)
			}
			if current.limits[0] > current.limits[1] {
				return fmt.Errorf("invalid number tree limits order")
			}
		}
		pairs, err := r.Resolve(node["Nums"])
		if err != nil {
			return err
		}
		kids, err := r.Resolve(node["Kids"])
		if err != nil {
			return err
		}
		if (pairs == nil) == (kids == nil) {
			return fmt.Errorf("number tree node requires pairs or children")
		}
		if pairs != nil {
			array, ok := pairs.(Array)
			if !ok || len(array)%2 != 0 {
				return fmt.Errorf("invalid number tree pairs")
			}
			for i := 0; i < len(array); i += 2 {
				if err := ctx.Err(); err != nil {
					return err
				}
				value, err := r.Resolve(array[i])
				if err != nil {
					return err
				}
				key, ok := value.(Integer)
				if !ok || hasPrevious && int64(key) <= previous {
					return fmt.Errorf("invalid number tree key order")
				}
				previous, hasPrevious = int64(key), true
				if !current.hasKeys {
					current.first, current.hasKeys = int64(key), true
				}
				current.last = int64(key)
				if err := visit(int64(key), array[i+1]); err != nil {
					return err
				}
			}
		}
		if kids != nil {
			array, ok := kids.(Array)
			if !ok {
				return fmt.Errorf("invalid number tree children")
			}
			current.kids = array
		}
		current.entered = true
	}
	return ctx.Err()
}

// WalkEmbeddedFiles 枚举文档名称树中的附件，不解码数据、不读取外部文件
// 入参: ctx 取消上下文, visit 名称树原始键和文件说明的访问函数
// 返回: error 名称树、文件说明或访问错误
func (r *Reader) WalkEmbeddedFiles(ctx context.Context, visit func(string, FileSpecification) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	catalog, err := r.catalogDictionary()
	if err != nil {
		return err
	}
	value, err := r.Resolve(catalog["Names"])
	if err != nil || value == nil {
		return err
	}
	names, ok := value.(Dictionary)
	if !ok {
		return fmt.Errorf("invalid names dictionary")
	}
	return r.WalkNameTree(ctx, names["EmbeddedFiles"], func(name string, object Object) error {
		file, err := r.ReadFileSpecification(object)
		if err != nil {
			return err
		}
		return visit(name, file)
	})
}

// catalogDictionary 读取目录字典，不执行目录中的动作
// 返回: Dictionary 目录, error 结构错误
func (r *Reader) catalogDictionary() (Dictionary, error) {
	value, err := r.Resolve(r.Trailer["Root"])
	if err != nil {
		return nil, err
	}
	catalog, ok := value.(Dictionary)
	if !ok {
		return nil, fmt.Errorf("invalid catalog")
	}
	return catalog, nil
}
