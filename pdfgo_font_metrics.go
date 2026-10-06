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
	"slices"
	"sort"
)

// cidMetricRange 保存连续CID共用的度量，不展开逐字缓存
type cidMetricRange[T comparable] struct {
	first, last uint32
	value       T
}

// cidMetrics 按CID排列互不相交的度量范围，后声明的范围覆盖前值
type cidMetrics[T comparable] struct {
	ranges []cidMetricRange[T]
}

// set 覆盖指定范围，保留两端旧值并合并相邻同值范围
// 入参: first 起始CID, last 结束CID, value 度量
func (m *cidMetrics[T]) set(first, last uint32, value T) {
	i := sort.Search(len(m.ranges), func(i int) bool { return m.ranges[i].last >= first })
	j := sort.Search(len(m.ranges), func(i int) bool { return m.ranges[i].first > last })
	var replacement [3]cidMetricRange[T]
	n := 0
	if i < j && m.ranges[i].first < first {
		replacement[n] = m.ranges[i]
		replacement[n].last = first - 1
		n++
	}
	replacement[n] = cidMetricRange[T]{first: first, last: last, value: value}
	n++
	if i < j && m.ranges[j-1].last > last {
		replacement[n] = m.ranges[j-1]
		replacement[n].first = last + 1
		n++
	}
	m.ranges = slices.Replace(m.ranges, i, j, replacement[:n]...)
	for k := max(i-1, 0); k+1 < len(m.ranges) && k <= i+n; {
		if m.ranges[k].last+1 == m.ranges[k+1].first && m.ranges[k].value == m.ranges[k+1].value {
			m.ranges[k].last = m.ranges[k+1].last
			m.ranges = slices.Delete(m.ranges, k+1, k+2)
		} else {
			k++
		}
	}
}

// get 查找CID的显式度量，未声明时交由字体使用默认值
// 入参: cid 字符标识
// 返回: T 度量, bool 是否存在
func (m *cidMetrics[T]) get(cid uint32) (T, bool) {
	i := sort.Search(len(m.ranges), func(i int) bool { return m.ranges[i].last >= cid })
	if i < len(m.ranges) && m.ranges[i].first <= cid {
		return m.ranges[i].value, true
	}
	var zero T
	return zero, false
}
