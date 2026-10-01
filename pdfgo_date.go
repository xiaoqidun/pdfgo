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
	"fmt"
	"strings"
	"time"
)

// ParseDate 解析PDF日期，省略字段使用标准默认值，接受Adobe规范的结尾时区引号
// 入参: value 已解码的PDF日期字符串
// 返回: time.Time 保留当地时间和时区的日期, error 日期语法或取值错误
func ParseDate(value string) (time.Time, error) {
	invalid := func() (time.Time, error) { return time.Time{}, fmt.Errorf("invalid PDF date") }
	if !strings.HasPrefix(value, "D:") {
		return invalid()
	}
	value = value[2:]
	parts := [6]int{0, 1, 1, 0, 0, 0}
	for i, length := range [6]int{4, 2, 2, 2, 2, 2} {
		if i > 0 && (len(value) == 0 || value[0] == '+' || value[0] == '-' || value[0] == 'Z') {
			break
		}
		if len(value) < length {
			return invalid()
		}
		n := 0
		for j := 0; j < length; j++ {
			if value[j] < '0' || value[j] > '9' {
				return invalid()
			}
			n = n*10 + int(value[j]-'0')
		}
		parts[i], value = n, value[length:]
	}
	location := time.UTC
	if value != "" {
		sign := value[0]
		if sign != '+' && sign != '-' && sign != 'Z' {
			return invalid()
		}
		value = value[1:]
		hour, minute := 0, 0
		if value != "" {
			if len(value) < 2 || value[0] < '0' || value[0] > '9' || value[1] < '0' || value[1] > '9' {
				return invalid()
			}
			hour, value = int(value[0]-'0')*10+int(value[1]-'0'), value[2:]
			if value != "" {
				if value[0] != '\'' {
					return invalid()
				}
				value = value[1:]
				if value != "" {
					if len(value) == 3 && value[2] == '\'' {
						value = value[:2]
					}
					if len(value) != 2 || value[0] < '0' || value[0] > '9' || value[1] < '0' || value[1] > '9' {
						return invalid()
					}
					minute = int(value[0]-'0')*10 + int(value[1]-'0')
				}
			}
		}
		if hour > 23 || minute > 59 || sign == 'Z' && (hour != 0 || minute != 0) {
			return invalid()
		}
		offset := hour*3600 + minute*60
		if sign == '-' {
			offset = -offset
		}
		if offset != 0 {
			location = time.FixedZone("", offset)
		}
	}
	date := time.Date(parts[0], time.Month(parts[1]), parts[2], parts[3], parts[4], parts[5], 0, location)
	if date.Year() != parts[0] || int(date.Month()) != parts[1] || date.Day() != parts[2] || date.Hour() != parts[3] || date.Minute() != parts[4] || date.Second() != parts[5] {
		return invalid()
	}
	return date, nil
}
