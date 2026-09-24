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

// Package pdfgo 原生、全平台、纯 Go 语言高性能 PDF 解析引擎
package pdfgo

import "fmt"

// SyntaxError 表示PDF语法错误及其字节位置
type SyntaxError struct {
	Offset  int64
	Message string
}

// Error 返回错误信息
func (e *SyntaxError) Error() string {
	return fmt.Sprintf("offset %d: %s", e.Offset, e.Message)
}

// UnsupportedError 表示尚未支持的PDF能力
type UnsupportedError struct {
	Feature string
}

// Error 返回错误信息
func (e *UnsupportedError) Error() string {
	return "unsupported " + e.Feature
}
