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
	"compress/zlib"
	"io"
	"sync"
)

// imageCompressionWriters 复用默认级别的图像压缩缓冲，不保留图像或输出流
var imageCompressionWriters sync.Pool

// imageCompressor 取得独立的图像压缩器，每次使用后须归还
// 入参: writer 压缩数据输出
// 返回: *zlib.Writer 默认级别压缩器
func imageCompressor(writer io.Writer) *zlib.Writer {
	if cached, ok := imageCompressionWriters.Get().(*zlib.Writer); ok {
		cached.Reset(writer)
		return cached
	}
	return zlib.NewWriter(writer)
}

// releaseImageCompressor 解除输出引用并归还压缩缓冲，允许编码错误后复用
// 入参: writer 本次独占的压缩器
func releaseImageCompressor(writer *zlib.Writer) {
	writer.Reset(io.Discard)
	imageCompressionWriters.Put(writer)
}
