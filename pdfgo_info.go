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
	"maps"
	"slices"
)

// DocumentInfo 保存信息字典中的文本、原始日期与陷印声明，不以XMP覆盖信息字典
// Custom保存其他文本字段，每次读取返回独立映射
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
	Custom       map[Name]string
}

// OutputIntent 保存目标输出条件和独立ICC快照，不替代对象颜色空间或启用屏幕打样
type OutputIntent struct {
	Subtype                   Name
	OutputCondition           string
	OutputConditionIdentifier string
	RegistryName              string
	Info                      string
	Profile                   []byte
	Space                     *ColorSpace
}

// OutputIntents 读取并验证文档输出意图，不将目标配置应用到普通屏显
// 返回: []OutputIntent 按文档顺序排列的输出条件, error 字典、字段或ICC错误
func (r *Reader) OutputIntents() ([]OutputIntent, error) {
	catalog, err := r.catalogDictionary()
	if err != nil {
		return nil, err
	}
	value, err := r.Resolve(catalog["OutputIntents"])
	if err != nil || value == nil {
		return nil, err
	}
	array, ok := value.(Array)
	if !ok {
		return nil, fmt.Errorf("invalid OutputIntents array")
	}
	result := make([]OutputIntent, len(array))
	for n, object := range array {
		result[n], err = r.readOutputIntent(object)
		if err != nil {
			return nil, fmt.Errorf("output intent %d: %w", n+1, err)
		}
	}
	return result, nil
}

// readOutputIntent 解析输出条件并验证嵌入配置，不使用ICCBased的备用空间掩盖无效目标配置
// 入参: object 输出意图字典或引用
// 返回: OutputIntent 输出条件, error 定义或配置错误
func (r *Reader) readOutputIntent(object Object) (OutputIntent, error) {
	var result OutputIntent
	value, err := r.Resolve(object)
	if err != nil {
		return result, err
	}
	dict, ok := value.(Dictionary)
	if !ok {
		return result, fmt.Errorf("invalid output intent dictionary")
	}
	value, err = r.Resolve(dict["Type"])
	if err != nil {
		return result, err
	}
	if value != nil && value != Name("OutputIntent") {
		return result, fmt.Errorf("invalid output intent Type")
	}
	value, err = r.Resolve(dict["S"])
	if err != nil {
		return result, err
	}
	result.Subtype, ok = value.(Name)
	if !ok || result.Subtype == "" {
		return result, fmt.Errorf("invalid output intent S")
	}
	for _, field := range []struct {
		key    Name
		target *string
	}{
		{"OutputCondition", &result.OutputCondition},
		{"OutputConditionIdentifier", &result.OutputConditionIdentifier},
		{"RegistryName", &result.RegistryName},
		{"Info", &result.Info},
	} {
		value, err := r.Resolve(dict[field.key])
		if err != nil {
			return result, err
		}
		if value == nil && field.key != "OutputConditionIdentifier" {
			continue
		}
		text, ok := value.(String)
		if !ok {
			return result, fmt.Errorf("invalid output intent %s", field.key)
		}
		*field.target, err = DecodeTextString(text)
		if err != nil {
			return result, err
		}
	}
	value, err = r.Resolve(dict["DestOutputProfileRef"])
	if err != nil {
		return result, err
	}
	if value != nil {
		return result, &UnsupportedError{Feature: "external output intent profile"}
	}
	value, err = r.Resolve(dict["DestOutputProfile"])
	if err != nil || value == nil {
		return result, err
	}
	stream, ok := value.(*Stream)
	if !ok {
		return result, fmt.Errorf("invalid DestOutputProfile stream")
	}
	data, err := stream.Decode()
	if err != nil {
		return result, fmt.Errorf("DestOutputProfile: %w", err)
	}
	result.Space, err = NewICCColorSpace(data)
	if err != nil {
		return result, fmt.Errorf("DestOutputProfile: %w", err)
	}
	value, err = r.Resolve(stream.Dictionary["N"])
	if err != nil {
		return result, err
	}
	if value != nil && value != Integer(result.Space.Components()) {
		return result, fmt.Errorf("DestOutputProfile component count mismatch")
	}
	result.Profile = append([]byte(nil), data...)
	return result, nil
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
	for _, key := range slices.Sorted(maps.Keys(dict)) {
		value, err := r.Resolve(dict[key])
		if err != nil {
			return result, err
		}
		if value == nil {
			continue
		}
		if key == "Trapped" {
			trapped, ok := value.(Name)
			if !ok || trapped != "True" && trapped != "False" && trapped != "Unknown" {
				return result, fmt.Errorf("invalid document trapping status")
			}
			result.Trapped = trapped
			continue
		}
		text, ok := value.(String)
		if !ok {
			return result, fmt.Errorf("invalid document information %s", key)
		}
		decoded, err := DecodeTextString(text)
		if err != nil {
			return result, err
		}
		switch key {
		case "Title":
			result.Title = decoded
		case "Author":
			result.Author = decoded
		case "Subject":
			result.Subject = decoded
		case "Keywords":
			result.Keywords = decoded
		case "Creator":
			result.Creator = decoded
		case "Producer":
			result.Producer = decoded
		case "CreationDate":
			result.CreationDate = decoded
		case "ModDate":
			result.ModDate = decoded
		default:
			if result.Custom == nil {
				result.Custom = make(map[Name]string)
			}
			result.Custom[key] = decoded
		}
	}
	return result, nil
}

// Metadata 读取文档目录声明的元数据流，不解码XML或与信息字典合并
// 返回: *Stream 元数据流，未声明时为空, error 引用或定义错误
func (r *Reader) Metadata() (*Stream, error) {
	catalog, err := r.catalogDictionary()
	if err != nil {
		return nil, err
	}
	return r.ReadMetadata(catalog["Metadata"])
}

// ReadMetadata 读取元数据流或其引用，可用于组件及标记内容的Metadata字段
// 返回流沿用所属阅读器的生命周期，通过DecodeContext按需解码，不修改原始XML
// 入参: object 元数据流或引用，空值表示未声明
// 返回: *Stream 元数据流, error 引用、类型或子类型错误
func (r *Reader) ReadMetadata(object Object) (*Stream, error) {
	value, err := r.Resolve(object)
	if err != nil || value == nil {
		return nil, err
	}
	stream, ok := value.(*Stream)
	if !ok || stream == nil {
		return nil, fmt.Errorf("invalid metadata stream")
	}
	for _, field := range []struct{ key, want Name }{{"Type", "Metadata"}, {"Subtype", "XML"}} {
		value, err := r.Resolve(stream.Dictionary[field.key])
		if err != nil {
			return nil, err
		}
		if value != field.want {
			return nil, fmt.Errorf("invalid metadata %s", field.key)
		}
	}
	return stream, nil
}
