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

// writeAnnotationCloudFrame 在内框内生成矩形或椭圆云线轮廓，不改变笔画和颜色
// 入参: content 外观内容, frame 内框, intensity 云线强度, ellipse 是否为椭圆
// 返回: error 非有限边界
func writeAnnotationCloudFrame(content *strings.Builder, frame Rectangle, intensity float64, ellipse bool) error {
	w, h := frame.XMax-frame.XMin, frame.YMax-frame.YMin
	for _, value := range []float64{frame.XMin, frame.YMin, frame.XMax, frame.YMax, w, h} {
		if math.IsNaN(value) || math.IsInf(value, 0) {
			return fmt.Errorf("invalid annotation cloud bounds")
		}
	}
	if w <= 0 || h <= 0 {
		writeAnnotationOperation(content, "re", frame.XMin, frame.YMin, w, h)
		return nil
	}
	radius := math.Min(3*intensity, math.Min(w, h)/4)
	if radius == 0 {
		writeAnnotationOperation(content, "re", frame.XMin, frame.YMin, w, h)
		return nil
	}
	x0, y0, x1, y1 := frame.XMin+radius, frame.YMin+radius, frame.XMax-radius, frame.YMax-radius
	if !ellipse {
		return writeAnnotationCloud(content, []float64{x0, y0, x1, y0, x1, y1, x0, y1}, radius)
	}
	cx, cy := frame.XMin+w/2, frame.YMin+h/2
	rx, ry := w/2-radius, h/2-radius
	count := int(math.Min(2048, math.Max(16, math.Ceil(math.Pi*math.Hypot(rx, ry)/radius))))
	points := make([]float64, 2*count)
	for i := range count {
		angle := 2 * math.Pi * float64(i) / float64(count)
		points[2*i], points[2*i+1] = cx+rx*math.Cos(angle), cy+ry*math.Sin(angle)
	}
	return writeAnnotationCloud(content, points, radius)
}

// writeAnnotationCloud 沿闭合顶点轮廓生成外凸云瓣，按环绕方向确定外侧
// 入参: content 外观内容, points 顶点坐标, radius 云瓣幅度
// 返回: error 非有限坐标或周长
func writeAnnotationCloud(content *strings.Builder, points []float64, radius float64) error {
	x0, y0, x1, y1 := points[0], points[1], points[0], points[1]
	perimeter := 0.0
	for i := 0; i < len(points); i += 2 {
		x, y := points[i], points[i+1]
		if math.IsNaN(x) || math.IsInf(x, 0) || math.IsNaN(y) || math.IsInf(y, 0) {
			return fmt.Errorf("invalid annotation cloud vertex")
		}
		x0, y0, x1, y1 = math.Min(x0, x), math.Min(y0, y), math.Max(x1, x), math.Max(y1, y)
		j := (i + 2) % len(points)
		perimeter += math.Hypot(points[j]-x, points[j+1]-y)
	}
	if math.IsInf(perimeter, 0) {
		return fmt.Errorf("invalid annotation cloud perimeter")
	}
	scale, area := math.Max(x1-x0, y1-y0), 0.0
	if scale > 0 {
		for i := 0; i < len(points); i += 2 {
			j := (i + 2) % len(points)
			area += (points[i]-x0)/scale*((points[j+1]-y0)/scale) - (points[j]-x0)/scale*((points[i+1]-y0)/scale)
		}
	}
	side := 1.0
	if area < 0 {
		side = -1
	}
	spacing := math.Max(2*radius, perimeter/2048)
	k := 4 * (math.Sqrt2 - 1) / 3
	writeAnnotationOperation(content, "m", points[0], points[1])
	for i := 0; i < len(points); i += 2 {
		j := (i + 2) % len(points)
		x, y, dx, dy := points[i], points[i+1], points[j]-points[i], points[j+1]-points[i+1]
		length := math.Hypot(dx, dy)
		if length == 0 {
			continue
		}
		count := max(1, int(math.Ceil(length/spacing)))
		half, height := length/float64(count)/2, math.Min(radius, length/float64(count)/2)
		ux, uy := dx/length, dy/length
		nx, ny := side*uy, -side*ux
		for n := range count {
			endX, endY := points[i]+dx*(float64(n+1)/float64(count)), points[i+1]+dy*(float64(n+1)/float64(count))
			mx, my := x+(endX-x)/2, y+(endY-y)/2
			writeAnnotationOperation(content, "c", x+nx*k*height, y+ny*k*height, mx-ux*k*half+nx*height, my-uy*k*half+ny*height, mx+nx*height, my+ny*height)
			writeAnnotationOperation(content, "c", mx+ux*k*half+nx*height, my+uy*k*half+ny*height, endX+nx*k*height, endY+ny*k*height, endX, endY)
			x, y = endX, endY
		}
	}
	content.WriteString("h\n")
	return nil
}
