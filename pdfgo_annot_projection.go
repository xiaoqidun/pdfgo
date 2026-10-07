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

// ProjectionAnnotation 保存依赖运行环境的批注及其可选三维测量关联
// Measurement为空不代表无效，其他运行环境可不提供ExData或使用其他子类型
type ProjectionAnnotation struct {
	Measurement  *ThreeDMeasurement
	ExternalData Dictionary
	Dictionary   Dictionary
}

// ReadProjectionAnnotation 解析投影批注，不要求静态外观或递归展开测量的回指
// 入参: annotation 投影批注
// 返回: ProjectionAnnotation 关联数据, error 类型、参数或引用错误
func (r *Reader) ReadProjectionAnnotation(annotation Annotation) (ProjectionAnnotation, error) {
	result := ProjectionAnnotation{Dictionary: annotation.Dictionary}
	if r == nil || r.closed || annotation.Subtype != "Projection" {
		return result, fmt.Errorf("invalid projection annotation")
	}
	dict, err := r.mediaDictionary(annotation.Dictionary["ExData"], true)
	if err != nil || dict == nil {
		return result, err
	}
	result.ExternalData = dict
	kind, err := r.Resolve(dict["Type"])
	if err != nil {
		return result, err
	}
	if kind != nil && kind != Name("ExData") {
		return result, fmt.Errorf("invalid projection external data type")
	}
	value, err := r.Resolve(dict["Subtype"])
	if err != nil {
		return result, err
	}
	subtype, ok := value.(Name)
	if !ok || subtype == "" {
		return result, fmt.Errorf("invalid projection external data subtype")
	}
	if subtype != "3DM" {
		return result, nil
	}
	ref, ok := dict["M3DREF"].(Reference)
	if !ok {
		return result, fmt.Errorf("invalid projection measurement reference")
	}
	measurement, err := r.ReadThreeDMeasurement(ref)
	if err != nil {
		return result, err
	}
	result.Measurement = &measurement
	return result, nil
}
