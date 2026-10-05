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

import "math"

// functionPosition 归一化函数区间位置，单点区间固定到编码起点
// 入参: value 已限制的输入值, lower 区间起点, upper 区间终点
// 返回: float64 区间位置
func functionPosition(value, lower, upper float64) float64 {
	if lower == upper || value == lower {
		return 0
	}
	if value == upper {
		return 1
	}
	if math.IsInf(upper-lower, 0) {
		return (value/2 - lower/2) / (upper/2 - lower/2)
	}
	return (value - lower) / (upper - lower)
}

// functionValue 映射区间位置，保留端点并避免差值溢出和次正规数中间舍入
// 入参: position 区间位置, lower 区间起点, upper 区间终点
// 返回: float64 映射值
func functionValue(position, lower, upper float64) float64 {
	if position == 0 || lower == upper {
		return lower
	}
	if position == 1 {
		return upper
	}
	span := upper - lower
	if math.IsInf(span, 0) {
		return (1-position)*lower + position*upper
	}
	if math.Abs(span) < 0x1p-1022 {
		return math.FMA(position, span, lower)
	}
	return lower + position*span
}
