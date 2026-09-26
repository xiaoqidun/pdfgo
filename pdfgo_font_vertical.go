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

// VerticalMetrics 保存千分之一字形单位的竖排纵向位移及相对于横排原点的位置
type VerticalMetrics struct {
	Advance float64
	Origin  Point
}

// readVerticalMetrics 读取CID字体的默认竖排度量及逐字覆盖值
// 入参: font 字体资源, metrics CID字体字典
// 返回: error 错误信息
func (r *Reader) readVerticalMetrics(font *Font, metrics Dictionary) error {
	font.defaultVertical = [2]float64{880, -1000}
	value, err := r.Resolve(metrics["DW2"])
	if err != nil {
		return err
	}
	if value != nil {
		array, ok := value.(Array)
		if !ok || len(array) != 2 {
			return fmt.Errorf("invalid default vertical metrics")
		}
		for i, value := range array {
			font.defaultVertical[i], err = r.number(value)
			if err != nil {
				return err
			}
		}
	}
	value, err = r.Resolve(metrics["W2"])
	if err != nil || value == nil {
		return err
	}
	array, ok := value.(Array)
	if !ok {
		return fmt.Errorf("invalid CID vertical metrics")
	}
	font.verticals = make(map[uint32]VerticalMetrics)
	for n := 0; n < len(array); {
		start, ok := array[n].(Integer)
		if !ok || start < 0 || start > 65535 || n+1 >= len(array) {
			return fmt.Errorf("invalid CID vertical range")
		}
		value, err := r.Resolve(array[n+1])
		if err != nil {
			return err
		}
		n += 2
		if values, ok := value.(Array); ok {
			if len(values)%3 != 0 || int64(len(values)/3)+int64(start) > 65536 {
				return fmt.Errorf("invalid CID vertical range")
			}
			for i := 0; i < len(values); i += 3 {
				metric, err := r.verticalMetric(values[i : i+3])
				if err != nil {
					return err
				}
				font.verticals[uint32(start)+uint32(i/3)] = metric
			}
		} else {
			end, ok := value.(Integer)
			if !ok || end < start || end > 65535 || len(array)-n < 3 {
				return fmt.Errorf("invalid CID vertical range")
			}
			metric, err := r.verticalMetric(array[n : n+3])
			if err != nil {
				return err
			}
			n += 3
			for cid := start; cid <= end; cid++ {
				font.verticals[uint32(cid)] = metric
			}
		}
	}
	return nil
}

// verticalMetric 读取竖排位移及原点向量
// 入参: values 三个竖排度量值
// 返回: VerticalMetrics 字形竖排度量, error 错误信息
func (r *Reader) verticalMetric(values Array) (VerticalMetrics, error) {
	var metric [3]float64
	for i, value := range values {
		number, err := r.number(value)
		if err != nil {
			return VerticalMetrics{}, err
		}
		metric[i] = number
	}
	return VerticalMetrics{Advance: metric[0], Origin: Point{metric[1], metric[2]}}, nil
}
