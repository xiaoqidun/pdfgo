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
	"math"
	"strings"
)

// writeAnnotationRelief 在内框中生成凸起或凹陷边框，明暗比例属于外观实现选择
// 入参: content 外观内容, border 已校验笔画, frame 边框范围
// 返回: error 颜色分量错误
func (r *Reader) writeAnnotationRelief(content *strings.Builder, border *annotationBorder, frame Rectangle) error {
	width := math.Min(border.width, math.Min(frame.XMax-frame.XMin, frame.YMax-frame.YMin)/2)
	if width <= 0 {
		return nil
	}
	var values [4]float64
	for i, component := range border.color {
		value, err := r.number(component)
		if err != nil {
			return err
		}
		values[i] = value
	}
	x0, y0, x1, y1 := frame.XMin, frame.YMin, frame.XMax, frame.YMax
	paths := [2][6]Point{
		{{x0, y0}, {x0, y1}, {x1, y1}, {x1 - width, y1 - width}, {x0 + width, y1 - width}, {x0 + width, y0 + width}},
		{{x0, y0}, {x1, y0}, {x1, y1}, {x1 - width, y1 - width}, {x1 - width, y0 + width}, {x0 + width, y0 + width}},
	}
	content.WriteString("q\n")
	for side, path := range paths {
		light := (side == 0) == (border.style == "B")
		for i, value := range values[:len(border.color)] {
			if len(border.color) == 4 {
				if light {
					value *= .25
				} else if i == 3 {
					value = .5 + value*.5
				}
			} else if light {
				value = .75 + value*.25
			} else {
				value *= .5
			}
			fmt.Fprintf(content, "%g ", value)
		}
		switch len(border.color) {
		case 1:
			content.WriteString("g\n")
		case 3:
			content.WriteString("rg\n")
		case 4:
			content.WriteString("k\n")
		}
		for i, point := range path {
			op := "l"
			if i == 0 {
				op = "m"
			}
			fmt.Fprintf(content, "%g %g %s\n", point.X, point.Y, op)
		}
		content.WriteString("h f\n")
	}
	content.WriteString("Q\n")
	return nil
}
