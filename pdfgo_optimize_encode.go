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

// optimizationWriters 复用最高无损级别的流编码缓冲，不保留输出流
var optimizationWriters sync.Pool

// defaultOptimizationWriters 复用默认级别的流编码缓冲，用于原始写出模式的新增或重写流
var defaultOptimizationWriters sync.Pool

// optimizationCompressor 取得独占的高压缩级别编码器，每次使用后须归还
// 入参: writer 压缩数据输出
// 返回: *zlib.Writer 高压缩级别编码器
func optimizationCompressor(writer io.Writer) *zlib.Writer {
	return optimizationCompressorLevel(writer, zlib.BestCompression)
}

// releaseOptimizationCompressor 解除输出引用并归还高压缩级别编码缓冲，允许错误或取消后复用
// 入参: writer 本次独占的压缩器
func releaseOptimizationCompressor(writer *zlib.Writer) {
	releaseOptimizationCompressorLevel(writer, zlib.BestCompression)
}

// optimizationCompressorLevel 按压缩级别取得独占的zlib编码器，每次使用后须按同级别归还
// 入参: writer 压缩数据输出, level zlib压缩级别
// 返回: *zlib.Writer 对应级别的编码器
func optimizationCompressorLevel(writer io.Writer, level int) *zlib.Writer {
	pool := &optimizationWriters
	if level == zlib.DefaultCompression {
		pool = &defaultOptimizationWriters
	} else {
		level = zlib.BestCompression
	}
	if cached, ok := pool.Get().(*zlib.Writer); ok {
		cached.Reset(writer)
		return cached
	}
	compressor, _ := zlib.NewWriterLevel(writer, level)
	return compressor
}

// releaseOptimizationCompressorLevel 解除输出引用并按级别归还zlib编码缓冲
// 入参: writer 本次独占的压缩器, level 获取时指定的压缩级别
func releaseOptimizationCompressorLevel(writer *zlib.Writer, level int) {
	writer.Reset(io.Discard)
	if level == zlib.DefaultCompression {
		defaultOptimizationWriters.Put(writer)
		return
	}
	optimizationWriters.Put(writer)
}
