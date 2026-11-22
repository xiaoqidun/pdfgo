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
	"reflect"
)

// WalkNameTree 按节点顺序访问名称树，名称保留原始字节，值不预先解析
// 配置Warning时诊断直接子字典、重复键及重复节点，重复节点仅访问一次
// 入参: ctx 取消上下文, root 名称树根, visit 名称和值的访问函数
// 返回: error 结构、重复名称、循环或访问错误
func (r *Reader) WalkNameTree(ctx context.Context, root Object, visit func(string, Object) error) error {
	if r == nil || ctx == nil {
		return fmt.Errorf("invalid name tree context")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	value, err := r.Resolve(root)
	if err != nil || value == nil {
		return err
	}
	seen := map[Reference]bool{}
	dictionaries := map[uintptr]Dictionary{}
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
				err := fmt.Errorf("repeated name tree node")
				if !r.recoverAuxiliary(err) {
					return err
				}
				continue
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
		identity := reflect.ValueOf(node).Pointer()
		if _, seen := dictionaries[identity]; seen {
			err := fmt.Errorf("repeated name tree node")
			if !r.recoverAuxiliary(err) {
				return err
			}
			continue
		}
		dictionaries[identity] = node
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
					err := fmt.Errorf("duplicate name tree key")
					if !r.recoverAuxiliary(err) {
						return err
					}
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
					err := fmt.Errorf("name tree child is not indirect")
					if _, direct := kid.(Dictionary); !direct || !r.recoverAuxiliary(err) {
						return err
					}
				}
			}
			stack = append(stack, kids)
		}
	}
	return ctx.Err()
}

// WalkNumberTree 按节点顺序访问数字树并验证键递增，值不预先解析
// 配置Warning时诊断直接子字典、键顺序和Limits错误，保留访问顺序及所有键值
// 入参: ctx 取消上下文, root 数字树根, visit 数字键和值的访问函数
// 返回: error 结构、重复键、循环或访问错误
func (r *Reader) WalkNumberTree(ctx context.Context, root Object, visit func(int64, Object) error) error {
	if r == nil || ctx == nil {
		return fmt.Errorf("invalid number tree context")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	value, err := r.Resolve(root)
	if err != nil || value == nil {
		return err
	}
	type frame struct {
		object      Object
		kids        Array
		next        int
		entered     bool
		first       int64
		last        int64
		hasKeys     bool
		limits      [2]int64
		validLimits bool
	}
	stack := []frame{{object: root}}
	seen := make(map[Reference]bool)
	dictionaries := make(map[uintptr]Dictionary)
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
					err := fmt.Errorf("number tree child is not indirect")
					if _, direct := object.(Dictionary); !direct || !r.recoverAuxiliary(err) {
						return err
					}
				}
				stack = append(stack, frame{object: object})
				continue
			}
			if len(stack) > 1 {
				if !current.hasKeys || current.validLimits && current.limits != [2]int64{current.first, current.last} {
					err := fmt.Errorf("number tree limits do not match keys")
					if !r.recoverAuxiliary(err) {
						return err
					}
				}
				parent := &stack[len(stack)-2]
				if current.hasKeys {
					if !parent.hasKeys {
						parent.first, parent.hasKeys = current.first, true
					}
					parent.last = current.last
				}
			}
			stack = stack[:len(stack)-1]
			continue
		}
		object := current.object
		if ref, ok := object.(Reference); ok {
			if seen[ref] {
				err := fmt.Errorf("repeated number tree node")
				if !r.recoverAuxiliary(err) {
					return err
				}
				stack = stack[:len(stack)-1]
				continue
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
		identity := reflect.ValueOf(node).Pointer()
		if _, seen := dictionaries[identity]; seen {
			err := fmt.Errorf("repeated number tree node")
			if !r.recoverAuxiliary(err) {
				return err
			}
			stack = stack[:len(stack)-1]
			continue
		}
		dictionaries[identity] = node
		value, err = r.Resolve(node["Limits"])
		if err != nil {
			return err
		}
		if len(stack) == 1 {
			if value != nil {
				err := fmt.Errorf("number tree root contains limits")
				if !r.recoverAuxiliary(err) {
					return err
				}
			}
		} else {
			limits, ok := value.(Array)
			if !ok || len(limits) != 2 {
				err := fmt.Errorf("invalid number tree limits")
				if !r.recoverAuxiliary(err) {
					return err
				}
				limits = nil
			}
			current.validLimits = len(limits) == 2
			for index, object := range limits {
				value, err := r.Resolve(object)
				if err != nil {
					return err
				}
				key, ok := value.(Integer)
				if !ok {
					err := fmt.Errorf("invalid number tree limit key")
					if !r.recoverAuxiliary(err) {
						return err
					}
					current.validLimits = false
					continue
				}
				current.limits[index] = int64(key)
			}
			if current.validLimits && current.limits[0] > current.limits[1] {
				err := fmt.Errorf("invalid number tree limits order")
				if !r.recoverAuxiliary(err) {
					return err
				}
				current.validLimits = false
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
				if !ok {
					return fmt.Errorf("invalid number tree key")
				}
				if hasPrevious && int64(key) <= previous {
					err := fmt.Errorf("invalid number tree key order")
					if !r.recoverAuxiliary(err) {
						return err
					}
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

// LookupNumberTree 按整数键在数字树中二分查找对应条目，值不预先解析
// 遵循ISO 32000升序Nums与Limits剪枝，兼容邻接重叠Limits及命中无Limits子节点回退
// 配置Warning时跳过重复节点，并在探测到键序或Limits违序、无效键或损坏子树时记录诊断并回退顺序查找
// 入参: ctx 取消上下文, root 数字树根, key 目标整数键
// 返回: Object 命中值, bool 是否命中, error 结构、循环或取消错误
func (r *Reader) LookupNumberTree(ctx context.Context, root Object, key int64) (Object, bool, error) {
	if r == nil || ctx == nil {
		return nil, false, fmt.Errorf("invalid number tree context")
	}
	if err := ctx.Err(); err != nil {
		return nil, false, err
	}
	seenRefs := make(map[Reference]bool)
	seenDicts := make(map[uintptr]Dictionary)
	return r.lookupNumberTreeNode(ctx, root, key, 0, seenRefs, seenDicts)
}

// lookupNumberTreeNode 递归查找数字树节点，优先按Limits剪枝与二分定位
// 入参: ctx 取消上下文, node 当前节点对象, key 目标整数键, depth 递归深度, seenRefs 已访问引用, seenDicts 已访问字典
// 返回: Object 命中值, bool 是否命中, error 结构、循环或取消错误
func (r *Reader) lookupNumberTreeNode(ctx context.Context, node Object, key int64, depth int, seenRefs map[Reference]bool, seenDicts map[uintptr]Dictionary) (Object, bool, error) {
	if err := ctx.Err(); err != nil {
		return nil, false, err
	}
	if depth > 64 {
		return nil, false, fmt.Errorf("number tree depth exceeded")
	}
	if ref, ok := node.(Reference); ok {
		if seenRefs[ref] {
			err := fmt.Errorf("repeated number tree node")
			if !r.recoverAuxiliary(err) {
				return nil, false, err
			}
			return nil, false, nil
		}
		seenRefs[ref] = true
	}
	resolved, err := r.Resolve(node)
	if err != nil {
		return nil, false, err
	}
	if resolved == nil {
		return nil, false, nil
	}
	dict, ok := resolved.(Dictionary)
	if !ok {
		return nil, false, fmt.Errorf("invalid number tree node")
	}
	identity := reflect.ValueOf(dict).Pointer()
	if _, exists := seenDicts[identity]; exists {
		err := fmt.Errorf("repeated number tree node")
		if !r.recoverAuxiliary(err) {
			return nil, false, err
		}
		return nil, false, nil
	}
	seenDicts[identity] = dict
	var nodeLo, nodeHi int64
	nodeHasLimits := false
	if depth > 0 {
		var err error
		nodeLo, nodeHi, nodeHasLimits, err = r.numberTreeLimits(dict["Limits"])
		if err != nil {
			return nil, false, err
		}
		if nodeHasLimits && (key < nodeLo || key > nodeHi) {
			return nil, false, nil
		}
	}
	if numsObj := dict["Nums"]; numsObj != nil {
		numsVal, err := r.Resolve(numsObj)
		if err != nil {
			return nil, false, err
		}
		if numsVal != nil {
			nums, ok := numsVal.(Array)
			if !ok || len(nums)%2 != 0 {
				return nil, false, fmt.Errorf("invalid number tree pairs")
			}
			if pairs := len(nums) / 2; pairs > 0 {
				low, high := 0, pairs-1
				var lowerKey, upperKey int64
				hasLower, hasUpper := false, false
				ordered, orderViolation := true, false
				for low <= high {
					if err := ctx.Err(); err != nil {
						return nil, false, err
					}
					mid := (low + high) / 2
					keyVal, err := r.Resolve(nums[mid*2])
					if err != nil {
						if !r.recoverAuxiliary(err) {
							return nil, false, err
						}
						ordered = false
						break
					}
					keyInt, ok := keyVal.(Integer)
					if !ok {
						if err := fmt.Errorf("invalid number tree key"); !r.recoverAuxiliary(err) {
							return nil, false, err
						}
						ordered = false
						break
					}
					current := int64(keyInt)
					if r.warning != nil && (hasLower && current <= lowerKey || hasUpper && current >= upperKey || nodeHasLimits && (current < nodeLo || current > nodeHi)) {
						ordered, orderViolation = false, true
						break
					}
					if current == key {
						return nums[mid*2+1], true, nil
					}
					if current < key {
						lowerKey, hasLower = current, true
						low = mid + 1
					} else {
						upperKey, hasUpper = current, true
						high = mid - 1
					}
				}
				if ordered && r.warning != nil && pairs > 1 {
					probes := [8]int{0, 1, high - 1, high, low, low + 1, pairs - 2, pairs - 1}
					var prev int64
					hasPrev, lastIdx := false, -1
					for _, idx := range probes {
						if idx < 0 || idx >= pairs || idx <= lastIdx {
							continue
						}
						lastIdx = idx
						if err := ctx.Err(); err != nil {
							return nil, false, err
						}
						keyVal, err := r.Resolve(nums[idx*2])
						if err != nil {
							if isFatalReaderError(err) {
								return nil, false, err
							}
							continue
						}
						keyInt, ok := keyVal.(Integer)
						if !ok {
							r.recoverAuxiliary(fmt.Errorf("invalid number tree key"))
							ordered = false
							break
						}
						current := int64(keyInt)
						if hasPrev && current <= prev || nodeHasLimits && (current < nodeLo || current > nodeHi) {
							ordered, orderViolation = false, true
							break
						}
						prev, hasPrev = current, true
						if current == key {
							r.recoverAuxiliary(fmt.Errorf("invalid number tree key order"))
							return nums[idx*2+1], true, nil
						}
					}
				}
				if !ordered {
					var previous int64
					hasPrevious, reportedOrder := false, orderViolation
					if reportedOrder {
						r.recoverAuxiliary(fmt.Errorf("invalid number tree key order"))
					}
					for i := 0; i+1 < len(nums); i += 2 {
						if err := ctx.Err(); err != nil {
							return nil, false, err
						}
						keyVal, err := r.Resolve(nums[i])
						if err != nil {
							if isFatalReaderError(err) {
								return nil, false, err
							}
							continue
						}
						keyInt, ok := keyVal.(Integer)
						if !ok {
							continue
						}
						current := int64(keyInt)
						if hasPrevious && current <= previous && !reportedOrder {
							r.recoverAuxiliary(fmt.Errorf("invalid number tree key order"))
							reportedOrder = true
						}
						previous, hasPrevious = current, true
						if current == key {
							return nums[i+1], true, nil
						}
					}
				}
			}
		}
	}
	if kidsObj := dict["Kids"]; kidsObj != nil {
		kidsVal, err := r.Resolve(kidsObj)
		if err != nil {
			return nil, false, err
		}
		if kidsVal != nil {
			kids, ok := kidsVal.(Array)
			if !ok {
				return nil, false, fmt.Errorf("invalid number tree children")
			}
			if len(kids) != 0 {
				low, high := 0, len(kids)-1
				var lowerLo, lowerHi, upperLo, upperHi, probedLo, probedHi int64
				hasLower, hasUpper := false, false
				ordered, orderViolation := true, false
				probed := -1
				var tried map[int]bool
				markTried := func(idx int) {
					if tried == nil {
						tried = make(map[int]bool)
					}
					tried[idx] = true
				}
				for low <= high {
					if err := ctx.Err(); err != nil {
						return nil, false, err
					}
					mid := (low + high) / 2
					kidVal, err := r.Resolve(kids[mid])
					if err != nil {
						if !r.recoverAuxiliary(err) {
							return nil, false, err
						}
						ordered = false
						break
					}
					kidDict, ok := kidVal.(Dictionary)
					if !ok {
						ordered = false
						break
					}
					lo, hi, hasLimits, err := r.numberTreeLimits(kidDict["Limits"])
					if err != nil {
						if !r.recoverAuxiliary(err) {
							return nil, false, err
						}
						ordered = false
						break
					}
					if !hasLimits {
						ordered = false
						break
					}
					if r.warning != nil && (hasLower && (lo < lowerLo || hi < lowerHi) || hasUpper && (lo > upperLo || hi > upperHi) || nodeHasLimits && (lo < nodeLo || hi > nodeHi)) {
						ordered, orderViolation = false, true
						break
					}
					if key < lo {
						upperLo, upperHi, hasUpper = lo, hi, true
						high = mid - 1
					} else if key > hi {
						lowerLo, lowerHi, hasLower = lo, hi, true
						low = mid + 1
					} else {
						probed, probedLo, probedHi = mid, lo, hi
						markTried(mid)
						val, found, err := r.lookupNumberTreeNode(ctx, kids[mid], key, depth+1, seenRefs, seenDicts)
						if found {
							return val, true, nil
						}
						if err != nil {
							if !r.recoverAuxiliary(err) {
								return nil, false, err
							}
							ordered = false
						}
						break
					}
				}
				if ordered {
					leftEnd := high
					refLo, refHi, hasRef := upperLo, upperHi, hasUpper
					if probed >= 0 {
						leftEnd = probed - 1
						refLo, refHi, hasRef = probedLo, probedHi, true
					}
					for i := leftEnd; i >= 0; i-- {
						if err := ctx.Err(); err != nil {
							return nil, false, err
						}
						sibVal, err := r.Resolve(kids[i])
						if err != nil {
							if !r.recoverAuxiliary(err) {
								return nil, false, err
							}
							ordered = false
							break
						}
						sibDict, ok := sibVal.(Dictionary)
						if !ok {
							ordered = false
							break
						}
						slo, shi, has, err := r.numberTreeLimits(sibDict["Limits"])
						if err != nil {
							if !r.recoverAuxiliary(err) {
								return nil, false, err
							}
							ordered = false
							break
						}
						if !has {
							ordered = false
							break
						}
						if r.warning != nil && (hasRef && (slo > refLo || shi > refHi) || nodeHasLimits && (slo < nodeLo || shi > nodeHi)) {
							ordered, orderViolation = false, true
							break
						}
						refLo, refHi, hasRef = slo, shi, true
						if key > shi {
							break
						}
						if key >= slo {
							markTried(i)
							val, found, err := r.lookupNumberTreeNode(ctx, kids[i], key, depth+1, seenRefs, seenDicts)
							if found {
								return val, true, nil
							}
							if err != nil {
								if !r.recoverAuxiliary(err) {
									return nil, false, err
								}
								ordered = false
								break
							}
						}
					}
				}
				if ordered && probed >= 0 {
					refLo, refHi := probedLo, probedHi
					for i := probed + 1; i < len(kids); i++ {
						if err := ctx.Err(); err != nil {
							return nil, false, err
						}
						sibVal, err := r.Resolve(kids[i])
						if err != nil {
							if !r.recoverAuxiliary(err) {
								return nil, false, err
							}
							ordered = false
							break
						}
						sibDict, ok := sibVal.(Dictionary)
						if !ok {
							ordered = false
							break
						}
						slo, shi, has, err := r.numberTreeLimits(sibDict["Limits"])
						if err != nil {
							if !r.recoverAuxiliary(err) {
								return nil, false, err
							}
							ordered = false
							break
						}
						if !has {
							ordered = false
							break
						}
						if r.warning != nil && (slo < refLo || shi < refHi || nodeHasLimits && (slo < nodeLo || shi > nodeHi)) {
							ordered, orderViolation = false, true
							break
						}
						refLo, refHi = slo, shi
						if key < slo {
							break
						}
						if key <= shi {
							markTried(i)
							val, found, err := r.lookupNumberTreeNode(ctx, kids[i], key, depth+1, seenRefs, seenDicts)
							if found {
								return val, true, nil
							}
							if err != nil {
								if !r.recoverAuxiliary(err) {
									return nil, false, err
								}
								ordered = false
								break
							}
						}
					}
				}
				if ordered && r.warning != nil && len(kids) > 1 {
					anchor := high
					if probed >= 0 {
						anchor = probed
					}
					probes := [8]int{0, 1, anchor - 1, anchor, anchor + 1, anchor + 2, len(kids) - 2, len(kids) - 1}
					var prevLo, prevHi int64
					hasPrev, lastIdx := false, -1
					for _, idx := range probes {
						if idx < 0 || idx >= len(kids) || idx <= lastIdx {
							continue
						}
						lastIdx = idx
						if err := ctx.Err(); err != nil {
							return nil, false, err
						}
						kidVal, err := r.Resolve(kids[idx])
						if err != nil {
							if isFatalReaderError(err) {
								return nil, false, err
							}
							continue
						}
						kidDict, ok := kidVal.(Dictionary)
						if !ok {
							continue
						}
						lo, hi, has, err := r.numberTreeLimits(kidDict["Limits"])
						if err != nil {
							if isFatalReaderError(err) {
								return nil, false, err
							}
							continue
						}
						if !has {
							ordered = false
							break
						}
						if hasPrev && (lo < prevLo || hi < prevHi) || nodeHasLimits && (lo < nodeLo || hi > nodeHi) || !tried[idx] && key >= lo && key <= hi {
							ordered, orderViolation = false, true
							break
						}
						prevLo, prevHi, hasPrev = lo, hi, true
					}
				}
				if !ordered {
					if r.warning == nil {
						for i, kid := range kids {
							if tried[i] {
								continue
							}
							val, found, err := r.lookupNumberTreeNode(ctx, kid, key, depth+1, seenRefs, seenDicts)
							if err != nil || found {
								return val, found, err
							}
						}
					} else {
						var prevLo, prevHi int64
						hasPrev, reportedOrder := false, orderViolation
						if reportedOrder {
							r.recoverAuxiliary(fmt.Errorf("invalid number tree key order"))
						}
						for i, kid := range kids {
							if err := ctx.Err(); err != nil {
								return nil, false, err
							}
							kidVal, err := r.Resolve(kid)
							if err != nil {
								if isFatalReaderError(err) {
									return nil, false, err
								}
								continue
							}
							kidDict, ok := kidVal.(Dictionary)
							if !ok {
								continue
							}
							lo, hi, has, err := r.numberTreeLimits(kidDict["Limits"])
							if err != nil {
								if isFatalReaderError(err) {
									return nil, false, err
								}
								continue
							}
							if has {
								if hasPrev && (lo < prevLo || hi < prevHi) && !reportedOrder {
									r.recoverAuxiliary(fmt.Errorf("invalid number tree key order"))
									reportedOrder = true
								}
								prevLo, prevHi, hasPrev = lo, hi, true
								if key < lo || key > hi {
									continue
								}
							}
							if tried[i] {
								continue
							}
							val, found, err := r.lookupNumberTreeNode(ctx, kid, key, depth+1, seenRefs, seenDicts)
							if err != nil {
								if isFatalReaderError(err) {
									return nil, false, err
								}
								continue
							}
							if found {
								return val, true, nil
							}
						}
					}
				}
			}
		}
	}
	return nil, false, nil
}

// numberTreeLimits 解析数字树节点的Limits上下界
// 入参: obj Limits对象
// 返回: int64 下界, int64 上界, bool 是否有效, error 解析错误
func (r *Reader) numberTreeLimits(obj Object) (int64, int64, bool, error) {
	if obj == nil {
		return 0, 0, false, nil
	}
	val, err := r.Resolve(obj)
	if err != nil {
		return 0, 0, false, err
	}
	arr, ok := val.(Array)
	if !ok || len(arr) != 2 {
		return 0, 0, false, nil
	}
	loVal, err := r.Resolve(arr[0])
	if err != nil {
		return 0, 0, false, err
	}
	hiVal, err := r.Resolve(arr[1])
	if err != nil {
		return 0, 0, false, err
	}
	lo, okLo := loVal.(Integer)
	hi, okHi := hiVal.(Integer)
	if !okLo || !okHi || lo > hi {
		return 0, 0, false, nil
	}
	return int64(lo), int64(hi), true, nil
}

// WalkEmbeddedFiles 枚举文档名称树中的附件，不解码数据、不读取外部文件
// 入参: ctx 取消上下文, visit 名称树原始键和文件说明的访问函数
// 返回: error 名称树、文件说明或访问错误
func (r *Reader) WalkEmbeddedFiles(ctx context.Context, visit func(string, FileSpecification) error) error {
	if r == nil || ctx == nil {
		return fmt.Errorf("invalid embedded files context")
	}
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
