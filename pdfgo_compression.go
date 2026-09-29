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

import "fmt"

const (
	CompressionUnchanged CompressionMode = iota
	CompressionLossless
	CompressionLossy
)

// CompressionMode 指定保持现有编码、无损优化或允许图像有损优化
type CompressionMode uint8

// CompressionOptions 配置输出压缩，不改变页面尺寸和渲染分辨率
// Quality仅用于有损图像编码，范围1至100，零值使用85；Mode零值保持现有处理策略
type CompressionOptions struct {
	Mode    CompressionMode `json:"mode"`
	Quality int             `json:"quality,omitempty"`
}

// Validate 检查压缩模式及画质范围
// 返回: error 参数错误
func (o CompressionOptions) Validate() error {
	if o.Mode > CompressionLossy || o.Quality < 0 || o.Quality > 100 {
		return fmt.Errorf("invalid compression options")
	}
	return nil
}

// ImageQuality 返回有损编码使用的画质
// 返回: int 编码画质
func (o CompressionOptions) ImageQuality() int {
	if o.Quality == 0 {
		return 85
	}
	return o.Quality
}
