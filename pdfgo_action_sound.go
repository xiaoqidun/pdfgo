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
	"fmt"
	"math"
)

// SoundAction 保存音频及有效播放参数，不解码采样或执行播放
// Volume取值范围[-1,1]，Repeat出现时Synchronous无效
type SoundAction struct {
	Sound       Sound
	Volume      float64
	Synchronous bool
	Repeat      bool
	Mix         bool
}

// ReadSoundAction 解析标准音频动作，保留原始音频对象及外部文件说明
// 入参: ctx 取消上下文, object 动作字典或间接引用
// 返回: SoundAction 音频动作, error 类型、必填字段或播放参数错误
func (r *Reader) ReadSoundAction(ctx context.Context, object Object) (SoundAction, error) {
	if err := ctx.Err(); err != nil {
		return SoundAction{}, err
	}
	value, err := r.Resolve(object)
	if err != nil {
		return SoundAction{}, err
	}
	dict, ok := value.(Dictionary)
	if !ok {
		return SoundAction{}, fmt.Errorf("invalid sound action")
	}
	value, err = r.Resolve(dict["S"])
	if err != nil {
		return SoundAction{}, err
	}
	if value != Name("Sound") {
		return SoundAction{}, fmt.Errorf("invalid sound action type")
	}
	result := SoundAction{Volume: 1}
	result.Sound, err = r.ReadSound(dict["Sound"])
	if err != nil {
		return result, err
	}
	value, err = r.Resolve(dict["Volume"])
	if err != nil {
		return result, err
	}
	if value != nil {
		result.Volume, err = r.number(value)
		if err != nil || math.IsNaN(result.Volume) || math.IsInf(result.Volume, 0) || result.Volume < -1 || result.Volume > 1 {
			return result, fmt.Errorf("invalid sound action volume")
		}
	}
	repeatPresent := false
	for _, field := range []struct {
		key    Name
		target *bool
	}{{"Repeat", &result.Repeat}, {"Mix", &result.Mix}, {"Synchronous", &result.Synchronous}} {
		if field.key == "Synchronous" && repeatPresent {
			continue
		}
		value, err := r.Resolve(dict[field.key])
		if err != nil {
			return result, err
		}
		if value == nil {
			continue
		}
		flag, ok := value.(Boolean)
		if !ok {
			return result, fmt.Errorf("invalid sound action %s", field.key)
		}
		*field.target = bool(flag)
		repeatPresent = repeatPresent || field.key == "Repeat"
	}
	return result, ctx.Err()
}
