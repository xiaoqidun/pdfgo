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

// annotationTextLayout 在内框中安排文字空间，保留默认外观矩阵的线性变换
// 入参: matrix 默认文字矩阵, x 内框横坐标, y 内框纵坐标, width 内框宽度, height 内框高度
// 返回: Matrix 定位矩阵, float64 排版宽度, float64 排版高度, error 坐标溢出错误
func annotationTextLayout(matrix Matrix, x, y, width, height float64) (Matrix, float64, float64, error) {
	a, b, c, d := math.Abs(matrix[0]), math.Abs(matrix[1]), math.Abs(matrix[2]), math.Abs(matrix[3])
	scale := max(a, b, c, d)
	w, h := width, height
	if scale > 0 {
		a, b, c, d = a/scale, b/scale, c/scale, d/scale
		if matrix[0]/scale*(matrix[3]/scale) != matrix[1]/scale*(matrix[2]/scale) {
			w, h = d*width+c*height, b*width+a*height
		}
		fit := max((a*w+c*h)/width, (b*w+d*h)/height)
		w, h = w/fit/scale, h/fit/scale
	}
	matrix[4] = x + width/2 - matrix[0]*(x+w/2) - matrix[2]*(y+h/2)
	matrix[5] = y + height/2 - matrix[1]*(x+w/2) - matrix[3]*(y+h/2)
	for _, value := range []float64{w, h, matrix[4], matrix[5]} {
		if math.IsNaN(value) || math.IsInf(value, 0) {
			return Matrix{}, 0, 0, fmt.Errorf("invalid annotation text layout")
		}
	}
	if w <= 0 || h <= 0 {
		return Matrix{}, 0, 0, fmt.Errorf("invalid annotation text layout")
	}
	return matrix, w, h, nil
}

// writeAnnotationTextMatrix 将排版位置映射到注解坐标，写入文字矩阵
// 入参: content 内容, matrix 定位矩阵, x 排版横坐标, y 排版基线
func writeAnnotationTextMatrix(content *strings.Builder, matrix Matrix, x, y float64) {
	point := matrix.Apply(Point{X: x, Y: y})
	writeAnnotationOperation(content, "Tm", matrix[0], matrix[1], matrix[2], matrix[3], point.X, point.Y)
}
