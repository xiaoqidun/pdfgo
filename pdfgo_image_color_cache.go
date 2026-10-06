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
	"math"
)

// imageColorEntry 保存完全相同输入分量的颜色，不缓存逐像素遮罩或色键透明度
type imageColorEntry struct {
	key   [4]uint64
	pixel color.NRGBA64
	valid bool
}

// imageColorCache 在单次图像解码内固定保存4096项，不近似输入或保留颜色配置文件
type imageColorCache struct {
	seed    maphash.Seed
	entries [4096]imageColorEntry
}

// entry 定位颜色缓存，完整分量用于核对散列碰撞
// 入参: values 已应用Decode、Matte及范围映射的分量
// 返回: *imageColorEntry 缓存位置或空值, [4]uint64 精确分量键
func (c *imageColorCache) entry(values [4]float64) (*imageColorEntry, [4]uint64) {
	if c == nil {
		return nil, [4]uint64{}
	}
	key := [4]uint64{math.Float64bits(values[0]), math.Float64bits(values[1]), math.Float64bits(values[2]), math.Float64bits(values[3])}
	index := maphash.Comparable(c.seed, key) % uint64(len(c.entries))
	return &c.entries[index], key
}
