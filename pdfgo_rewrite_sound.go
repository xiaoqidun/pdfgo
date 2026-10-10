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
	"reflect"
)

// soundReplacements 为动作中的音频分配输出资源，同一动作实例只写一次
// 入参: ctx 取消上下文, options 替换配置, result 已有替换对象
// 返回: map[*SoundAction]Reference 音频引用, error 参数、复制或取消错误
func (r *Reader) soundReplacements(ctx context.Context, options RewriteOptions, result map[Reference]Object) (map[*SoundAction]Reference, error) {
	var sounds map[*SoundAction]Reference
	var streams map[Sound]Reference
	number := int64(0)
	for actions := range options.actionSequences() {
		for _, action := range actions {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			if action.Sound == nil {
				continue
			}
			if _, exists := sounds[action.Sound]; exists {
				continue
			}
			if sounds == nil {
				sounds = make(map[*SoundAction]Reference)
				streams = make(map[Sound]Reference)
				for id := range r.xref {
					number = max(number, id)
				}
				for ref := range result {
					number = max(number, ref.Number)
				}
			}
			volume := action.Sound.Volume
			if math.IsNaN(volume) || math.IsInf(volume, 0) || volume < -1 || volume > 1 {
				return nil, fmt.Errorf("invalid sound action volume")
			}
			if ref, exists := streams[action.Sound.Sound]; exists {
				sounds[action.Sound] = ref
				continue
			}
			ref, err := appendRewriteObject(result, &number, nil)
			if err != nil {
				return nil, err
			}
			stream, err := rewriteSound(ctx, action.Sound.Sound, result, &number, ref)
			if err != nil {
				return nil, err
			}
			result[ref] = stream
			sounds[action.Sound] = ref
			streams[action.Sound.Sound] = ref
		}
	}
	return sounds, ctx.Err()
}

// rewriteSound 写出采样流或自描述音频文件，不访问外部资源或执行音频编码
// 入参: ctx 取消上下文, sound 音频信息, result 输出对象, number 最大对象编号, ref 音频输出引用
// 返回: *Stream 独立音频流, error 参数、复制或取消错误
func rewriteSound(ctx context.Context, sound Sound, result map[Reference]Object, number *int64, ref Reference) (*Stream, error) {
	dict := Dictionary{"Type": Name("Sound")}
	if sound.File != nil {
		ref, err := rewriteFile(ctx, *sound.File, result, number)
		if err != nil {
			return nil, err
		}
		dict["F"] = ref
		return &Stream{Dictionary: dict}, ctx.Err()
	}
	if sound.Stream == nil {
		return nil, fmt.Errorf("missing sound samples")
	}
	if math.IsNaN(sound.Rate) || math.IsInf(sound.Rate, 0) || sound.Rate <= 0 {
		return nil, fmt.Errorf("invalid sound sampling rate")
	}
	channels, bits, encoding := sound.Channels, sound.Bits, sound.Encoding
	if channels == 0 {
		channels = 1
	}
	if bits == 0 {
		bits = 8
	}
	if encoding == "" {
		encoding = "Raw"
	}
	if channels < 0 || bits < 0 {
		return nil, fmt.Errorf("invalid sound sample dimensions")
	}
	if encoding != "Raw" && encoding != "Signed" && encoding != "muLaw" && encoding != "ALaw" {
		return nil, fmt.Errorf("invalid sound encoding")
	}
	dict["R"], dict["C"], dict["B"], dict["E"] = Real(sound.Rate), Integer(channels), Integer(bits), encoding
	dict["Filter"], dict["DecodeParms"] = sound.Stream.Dictionary["Filter"], sound.Stream.Dictionary["DecodeParms"]
	if sound.Compression != "" {
		dict["CO"] = sound.Compression
		if parameter := sound.Stream.Dictionary["CP"]; parameter != nil {
			dict["CP"] = parameter
		}
	}
	source := *sound.Stream
	source.Dictionary = dict
	copier := rewriteObjectCopy{ctx: ctx, source: source.reader, objects: result, number: number, copied: make(map[rewriteObjectIdentity]Reference)}
	copier.copied[rewriteObjectIdentity{kind: 's', pointer: reflect.ValueOf(sound.Stream).Pointer()}] = ref
	return copier.copyStream(&source, 0)
}
