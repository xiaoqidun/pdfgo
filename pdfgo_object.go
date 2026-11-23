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
	"bytes"
	"encoding/binary"
	"hash"
	"maps"
	"math"
	"slices"
)

// Object 表示PDF对象，空对象使用nil表示
type Object interface{ pdfObject() }

// Boolean 表示布尔对象
type Boolean bool

// Integer 表示整数对象
type Integer int64

// Real 表示实数对象
type Real float64

// Name 表示已解除转义的名称，不包含起始斜线
type Name string

// String 保留字符串原始字节，不预设文本编码
type String []byte

// Array 按原始顺序保存数组元素
type Array []Object

// Dictionary 保存字典条目，包括未知扩展字段
type Dictionary map[Name]Object

// Reference 标识间接对象编号及代数
type Reference struct {
	Number     int64
	Generation int64
}

// pdfObject 将布尔值标记为PDF对象
func (Boolean) pdfObject() {}

// pdfObject 将整数标记为PDF对象
func (Integer) pdfObject() {}

// pdfObject 将实数标记为PDF对象
func (Real) pdfObject() {}

// pdfObject 将名称标记为PDF对象
func (Name) pdfObject() {}

// pdfObject 将字符串标记为PDF对象
func (String) pdfObject() {}

// pdfObject 将数组标记为PDF对象
func (Array) pdfObject() {}

// pdfObject 将字典标记为PDF对象
func (Dictionary) pdfObject() {}

// pdfObject 将间接引用标记为PDF对象
func (Reference) pdfObject() {}

// EqualObject 按PDF值语义深度比较两个对象，流对象仅比较引用、字典与字节内容而不访问内部阅读器状态
// 入参: a 左侧对象, b 右侧对象
// 返回: bool 是否值等价
func EqualObject(a, b Object) bool {
	return equalObject(a, b, 0)
}

// equalObject 递归比较PDF对象值，限制嵌套深度以防循环结构栈溢出
// 入参: a 左侧对象, b 右侧对象, depth 当前递归深度
// 返回: bool 是否值等价
func equalObject(a, b Object, depth int) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	if depth > 64 {
		return false
	}
	switch x := a.(type) {
	case Boolean:
		y, ok := b.(Boolean)
		return ok && x == y
	case Integer:
		y, ok := b.(Integer)
		return ok && x == y
	case Real:
		y, ok := b.(Real)
		return ok && (x == y || math.IsNaN(float64(x)) && math.IsNaN(float64(y)))
	case Name:
		y, ok := b.(Name)
		return ok && x == y
	case String:
		y, ok := b.(String)
		return ok && bytes.Equal(x, y)
	case Reference:
		y, ok := b.(Reference)
		return ok && x == y
	case Array:
		y, ok := b.(Array)
		if !ok || len(x) != len(y) {
			return false
		}
		for i := range x {
			if !equalObject(x[i], y[i], depth+1) {
				return false
			}
		}
		return true
	case Dictionary:
		y, ok := b.(Dictionary)
		if !ok || len(x) != len(y) {
			return false
		}
		for k, xv := range x {
			yv, exists := y[k]
			if !exists || !equalObject(xv, yv, depth+1) {
				return false
			}
		}
		return true
	case *Stream:
		y, ok := b.(*Stream)
		if !ok {
			return false
		}
		if x == y {
			return true
		}
		if x == nil || y == nil {
			return false
		}
		return x.Reference == y.Reference && bytes.Equal(x.Data, y.Data) && equalObject(x.Dictionary, y.Dictionary, depth+1)
	default:
		return false
	}
}

// HashObject 按PDF值语义将对象写入哈希器，流对象仅写入引用、字典与字节内容而不访问内部阅读器状态
// 入参: h 目标哈希器, value 待摘要对象
// 返回: bool 是否完成确定性摘要
func HashObject(h hash.Hash, value Object) bool {
	return hashObject(h, value, 0)
}

// hashObject 递归按规范顺序写入对象内容，限制嵌套深度防止循环结构溢出
// 入参: h 目标哈希器, value 待摘要对象, depth 当前递归深度
// 返回: bool 是否完成确定性摘要
func hashObject(h hash.Hash, value Object, depth int) bool {
	if depth > 64 {
		return false
	}
	var buf [8]byte
	writeU64 := func(v uint64) {
		binary.LittleEndian.PutUint64(buf[:], v)
		h.Write(buf[:])
	}
	if value == nil {
		h.Write([]byte{'0'})
		return true
	}
	switch v := value.(type) {
	case Boolean:
		if v {
			h.Write([]byte{'B', 1})
		} else {
			h.Write([]byte{'B', 0})
		}
		return true
	case Integer:
		h.Write([]byte{'I'})
		writeU64(uint64(v))
		return true
	case Real:
		h.Write([]byte{'R'})
		f := float64(v)
		if math.IsNaN(f) {
			writeU64(0x7ff8000000000001)
		} else {
			if f == 0 {
				f = 0
			}
			writeU64(math.Float64bits(f))
		}
		return true
	case Name:
		h.Write([]byte{'N'})
		writeU64(uint64(len(v)))
		h.Write([]byte(v))
		return true
	case String:
		h.Write([]byte{'S'})
		writeU64(uint64(len(v)))
		h.Write(v)
		return true
	case Reference:
		h.Write([]byte{'X'})
		writeU64(uint64(v.Number))
		writeU64(uint64(v.Generation))
		return true
	case Array:
		h.Write([]byte{'A'})
		writeU64(uint64(len(v)))
		for _, item := range v {
			if !hashObject(h, item, depth+1) {
				return false
			}
		}
		return true
	case Dictionary:
		h.Write([]byte{'D'})
		writeU64(uint64(len(v)))
		for _, key := range slices.Sorted(maps.Keys(v)) {
			writeU64(uint64(len(key)))
			h.Write([]byte(key))
			if !hashObject(h, v[key], depth+1) {
				return false
			}
		}
		return true
	case *Stream:
		if v == nil {
			h.Write([]byte{'m'})
			return true
		}
		h.Write([]byte{'M'})
		writeU64(uint64(v.Reference.Number))
		writeU64(uint64(v.Reference.Generation))
		writeU64(uint64(len(v.Data)))
		h.Write(v.Data)
		return hashObject(h, v.Dictionary, depth+1)
	default:
		return false
	}
}
