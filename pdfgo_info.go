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

// DocumentInfo 保存信息字典中的文本、原始日期与陷印声明，不以XMP覆盖信息字典
type DocumentInfo struct {
	Title        string
	Author       string
	Subject      string
	Keywords     string
	Creator      string
	Producer     string
	CreationDate string
	ModDate      string
	Trapped      Name
}

// Info 读取文档信息字典，解码PDF文本字符串，日期保留原文以免丢失精度
// 返回: DocumentInfo 文档信息, error 字典或字段类型错误
func (r *Reader) Info() (DocumentInfo, error) {
	result := DocumentInfo{Trapped: "Unknown"}
	value, err := r.Resolve(r.Trailer["Info"])
	if err != nil || value == nil {
		return result, err
	}
	dict, ok := value.(Dictionary)
	if !ok {
		return result, fmt.Errorf("invalid document information dictionary")
	}
	for _, field := range []struct {
		key    Name
		target *string
	}{
		{"Title", &result.Title}, {"Author", &result.Author}, {"Subject", &result.Subject},
		{"Keywords", &result.Keywords}, {"Creator", &result.Creator}, {"Producer", &result.Producer},
		{"CreationDate", &result.CreationDate}, {"ModDate", &result.ModDate},
	} {
		value, err := r.Resolve(dict[field.key])
		if err != nil {
			return result, err
		}
		if value == nil {
			continue
		}
		text, ok := value.(String)
		if !ok {
			return result, fmt.Errorf("invalid document information %s", field.key)
		}
		*field.target, err = DecodeTextString(text)
		if err != nil {
			return result, err
		}
	}
	value, err = r.Resolve(dict["Trapped"])
	if err != nil {
		return result, err
	}
	if value != nil {
		trapped, ok := value.(Name)
		if !ok || trapped != "True" && trapped != "False" && trapped != "Unknown" {
			return result, fmt.Errorf("invalid document trapping status")
		}
		result.Trapped = trapped
	}
	return result, nil
}
