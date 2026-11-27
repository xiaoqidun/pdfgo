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
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
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

// OutputIntent 保存目标输出条件和ICC配置快照，不替代对象颜色空间或启用屏幕打样
type OutputIntent struct {
	Subtype                   Name
	OutputCondition           string
	OutputConditionIdentifier string
	RegistryName              string
	Info                      string
	Profile                   []byte
	Space                     *ColorSpace
}

// outputIntentProfile 保存已解码输出意图ICC配置及其校验结果
type outputIntentProfile struct {
	profile []byte
	space   *ColorSpace
	err     error
}

// outputIntentReadState 跟踪单次输出意图读取的文本预算、配置预算与共享ICC缓存
type outputIntentReadState struct {
	textBudget   textStringBudget
	profileLimit int64
	profileUsed  int64
	profiles     map[Reference]outputIntentProfile
}

// consumeProfile 累计输出意图ICC配置逻辑字节数，超过上限时返回超限错误
// 入参: size 新增配置字节数
// 返回: error 超限错误
func (s *outputIntentReadState) consumeProfile(size int64) error {
	if s.profileLimit <= 0 {
		return nil
	}
	if size > s.profileLimit-s.profileUsed {
		return fmt.Errorf("%w: output intent profile size limit exceeded", ErrStreamLimitExceeded)
	}
	s.profileUsed += size
	return nil
}

// OutputIntents 读取并验证文档输出意图，不将目标配置应用到普通屏显
// 配置Warning时诊断无效辅助字段并返回可读取的部分，无效配置的Space为空，原始对象保持不变
// 入参: ctx 取消上下文
// 返回: []OutputIntent 按文档顺序排列的输出条件, error 字典、字段、ICC、超限或取消错误
func (r *Reader) OutputIntents(ctx context.Context) ([]OutputIntent, error) {
	if ctx == nil {
		return nil, fmt.Errorf("invalid output intents context")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	catalog, err := r.catalogDictionary()
	if err != nil {
		return nil, err
	}
	value, err := r.Resolve(catalog["OutputIntents"])
	if err != nil || value == nil {
		if r.recoverAuxiliary(err) {
			return nil, nil
		}
		return nil, err
	}
	array, ok := value.(Array)
	if !ok {
		err := fmt.Errorf("invalid OutputIntents array")
		if r.recoverAuxiliary(err) {
			return nil, nil
		}
		return nil, err
	}
	profileLimit, _ := effectiveStreamLimits(r, nil)
	state := outputIntentReadState{textBudget: newTextStringBudget(r), profileLimit: profileLimit}
	result := make([]OutputIntent, 0, min(len(array), 64))
	for n, object := range array {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if n >= 4096 {
			return nil, fmt.Errorf("%w: output intents count limit exceeded", ErrStreamLimitExceeded)
		}
		intent, err := r.readOutputIntent(ctx, object, &state)
		if err != nil {
			err = fmt.Errorf("output intent %d: %w", n+1, err)
			if !r.recoverAuxiliary(err) {
				return nil, err
			}
			intent.Space = nil
		}
		result = append(result, intent)
	}
	return result, nil
}

// readOutputIntent 解析输出条件并验证嵌入配置，不使用ICCBased的备用空间掩盖无效目标配置
// 入参: ctx 取消上下文, object 输出意图字典或引用, state 共享预算与配置缓存状态
// 返回: OutputIntent 输出条件, error 定义、配置、超限或取消错误
func (r *Reader) readOutputIntent(ctx context.Context, object Object, state *outputIntentReadState) (OutputIntent, error) {
	var result OutputIntent
	if err := ctx.Err(); err != nil {
		return result, err
	}
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
		ref, value, err := r.resolveIndirect(dict[field.key])
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
		*field.target, err = state.textBudget.decodeString(ref, text, "output intent")
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
	profileRef, value, err := r.resolveIndirect(dict["DestOutputProfile"])
	if err != nil || value == nil {
		return result, err
	}
	stream, ok := value.(*Stream)
	if !ok {
		return result, fmt.Errorf("invalid DestOutputProfile stream")
	}
	if profileRef != (Reference{}) {
		if cached, exists := state.profiles[profileRef]; exists {
			if err := state.consumeProfile(int64(len(cached.profile))); err != nil {
				return result, err
			}
			result.Profile = cached.profile
			result.Space = cached.space
			return result, cached.err
		}
	}
	data, err := r.DecodeStreamContext(ctx, stream)
	if err != nil {
		return result, fmt.Errorf("DestOutputProfile: %w", err)
	}
	if err := state.consumeProfile(int64(len(data))); err != nil {
		return result, err
	}
	result.Profile = data
	result.Space, err = NewICCColorSpace(data)
	if err != nil {
		err = fmt.Errorf("DestOutputProfile: %w", err)
	} else {
		value, err = r.Resolve(stream.Dictionary["N"])
		if err == nil && value != nil && value != Integer(result.Space.Components()) {
			err = fmt.Errorf("DestOutputProfile component count mismatch")
		}
	}
	if profileRef != (Reference{}) && !isFatalReaderError(err) {
		if state.profiles == nil {
			state.profiles = make(map[Reference]outputIntentProfile)
		}
		state.profiles[profileRef] = outputIntentProfile{profile: data, space: result.Space, err: err}
	}
	if err != nil {
		return result, err
	}
	return result, nil
}

// Info 读取文档信息字典，解码PDF文本字符串，日期保留原文以免丢失精度
// 配置Warning时诊断无效辅助字段并读取其他文本，原始对象保持不变
// 返回: DocumentInfo 文档信息, error 字典、字段类型或超限错误
func (r *Reader) Info() (DocumentInfo, error) {
	result := DocumentInfo{Trapped: "Unknown"}
	if r == nil {
		return result, fmt.Errorf("invalid reader")
	}
	if r.isClosed() {
		return result, os.ErrClosed
	}
	value, err := r.Resolve(r.Trailer["Info"])
	if err != nil || value == nil {
		if r.recoverAuxiliary(err) {
			return result, nil
		}
		return result, err
	}
	dict, ok := value.(Dictionary)
	if !ok {
		err := fmt.Errorf("invalid document information dictionary")
		if r.recoverAuxiliary(err) {
			return result, nil
		}
		return result, err
	}
	budget := newTextStringBudget(r)
	for _, key := range slices.Sorted(maps.Keys(dict)) {
		ref, value, err := r.resolveIndirect(dict[key])
		if err != nil {
			if r.recoverAuxiliary(err) {
				continue
			}
			return result, err
		}
		if value == nil {
			continue
		}
		if key == "Trapped" {
			trapped, ok := value.(Name)
			if !ok || trapped != "True" && trapped != "False" && trapped != "Unknown" {
				err := fmt.Errorf("invalid document trapping status")
				if r.recoverAuxiliary(err) {
					continue
				}
				return result, err
			}
			result.Trapped = trapped
			continue
		}
		text, ok := value.(String)
		if !ok {
			err := fmt.Errorf("invalid document information %s", key)
			if r.recoverAuxiliary(err) {
				continue
			}
			return result, err
		}
		decoded, err := budget.decodeString(ref, text, "document info")
		if err != nil {
			if r.recoverAuxiliary(err) {
				continue
			}
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

// isFatalReaderError 判断错误是否为不可恢复的读取、解密、认证、关闭或取消错误
// 入参: err 待判定错误
// 返回: bool 是否必须向上传递
func isFatalReaderError(err error) bool {
	var read *ReadError
	var security *objectSecurityError
	return errors.As(err, &read) || errors.As(err, &security) || errors.Is(err, ErrPassword) || errors.Is(err, ErrStreamLimitExceeded) || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) || errors.Is(err, os.ErrClosed)
}

// recoverAuxiliary 报告辅助信息错误，输入读取、解密、认证、关闭及取消错误继续传递
// 入参: err 辅助信息错误
// 返回: bool 是否已记录并允许继续
func (r *Reader) recoverAuxiliary(err error) bool {
	if err == nil || r.warning == nil || isFatalReaderError(err) {
		return false
	}
	r.warning(Diagnostic{Message: err.Error()})
	return true
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
