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
	"hash/maphash"
	"image/color"
)

// imageColorEntry 保存完全相同输入分量和背景还原透明度的颜色，包含色键透明度但不叠加遮罩
type imageColorEntry struct {
	key   [5]uint16
	pixel color.NRGBA64
	valid bool
}

// imageColorCache 在单次图像解码内固定保存4096项，不近似输入或保留颜色配置文件
type imageColorCache struct {
	seed    maphash.Seed
	entries [4096]imageColorEntry
}

// entry 定位颜色缓存，完整分量用于核对散列碰撞
// 入参: values 原始16位分量, alpha 背景还原透明度，无背景还原时为零
// 返回: *imageColorEntry 缓存位置或空值, [5]uint16 精确分量键
func (c *imageColorCache) entry(values [4]uint16, alpha uint16) (*imageColorEntry, [5]uint16) {
	if c == nil {
		return nil, [5]uint16{}
	}
	key := [5]uint16{values[0], values[1], values[2], values[3], alpha}
	packed := struct {
		components uint64
		alpha      uint16
	}{uint64(values[0]) | uint64(values[1])<<16 | uint64(values[2])<<32 | uint64(values[3])<<48, alpha}
	index := maphash.Comparable(c.seed, packed) % uint64(len(c.entries))
	return &c.entries[index], key
}
