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

// Package pdfgo 原生、全平台、纯 Go 语言高性能 PDF 处理引擎
package pdfgo

import "fmt"

// Diagnostic 记录可恢复的问题或未保留的语义，Page从1开始，未知页码为0
// Offset为解码后当前页面或表单内容流内的字节位置
type Diagnostic struct {
	Page    int
	Offset  int64
	Message string
}

// SyntaxError 表示PDF语法错误及其字节位置
type SyntaxError struct {
	Offset  int64
	Message string
}

// ReadError 表示输入源读取失败，Err保留原始错误
type ReadError struct {
	Offset int64
	Err    error
}

// UnsupportedError 表示尚未支持的PDF能力
type UnsupportedError struct {
	Feature string
}

// Error 返回错误信息
// 返回: string 错误描述
func (e *SyntaxError) Error() string {
	return fmt.Sprintf("offset %d: %s", e.Offset, e.Message)
}

// Error 返回读取位置及原始错误
// 返回: string 错误描述
func (e *ReadError) Error() string {
	return fmt.Sprintf("offset %d: %v", e.Offset, e.Err)
}

// Unwrap 返回原始读取错误
// 返回: error 原始错误
func (e *ReadError) Unwrap() error {
	return e.Err
}

// Error 返回错误信息
// 返回: string 错误描述
func (e *UnsupportedError) Error() string {
	return "unsupported " + e.Feature
}
