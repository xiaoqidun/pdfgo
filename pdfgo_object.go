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

func (Boolean) pdfObject()    {}
func (Integer) pdfObject()    {}
func (Real) pdfObject()       {}
func (Name) pdfObject()       {}
func (String) pdfObject()     {}
func (Array) pdfObject()      {}
func (Dictionary) pdfObject() {}
func (Reference) pdfObject()  {}
