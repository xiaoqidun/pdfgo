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

const (
	CompressionLight CompressionLevel = iota
	CompressionMedium
	CompressionStrong
)

// CompressionMode 选择默认、无损或有损输出策略
type CompressionMode uint8

// CompressionLevel 选择轻压、均衡或强压，仅用于有损模式
type CompressionLevel uint8

// CompressionOptions 配置输出压缩，默认模式沿用现有输出策略，不额外优化
// Quality为有损质量1至100，0按Level使用85、60或40
// MaxDPI为图片降采样目标1至9600，0按Level使用不限、150或96，不改变页面尺寸
type CompressionOptions struct {
	Mode    CompressionMode  `json:"mode"`
	Level   CompressionLevel `json:"level,omitempty"`
	Quality int              `json:"quality,omitempty"`
	MaxDPI  int              `json:"maxDPI,omitempty"`
}

// Validate 检查压缩档位、画质及精度范围
// 返回: error 参数错误
func (o CompressionOptions) Validate() error {
	if o.Mode > CompressionLossy || o.Level > CompressionStrong || o.Quality < 0 || o.Quality > 100 || o.MaxDPI < 0 || o.MaxDPI > 9600 {
		return fmt.Errorf("invalid compression options")
	}
	return nil
}

// ImageQuality 返回有损编码使用的画质
// 返回: int 编码画质
func (o CompressionOptions) ImageQuality() int {
	if o.Quality == 0 {
		switch o.Level {
		case CompressionMedium:
			return 60
		case CompressionStrong:
			return 40
		default:
			return 85
		}
	}
	return o.Quality
}

// ImageDPI 返回嵌入图片的降采样目标精度，0表示不降采样
// 返回: int 目标精度
func (o CompressionOptions) ImageDPI() int {
	if o.Mode != CompressionLossy {
		return 0
	}
	if o.MaxDPI != 0 {
		return o.MaxDPI
	}
	switch o.Level {
	case CompressionMedium:
		return 150
	case CompressionStrong:
		return 96
	default:
		return 0
	}
}
