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

// Movie 保存视频文件说明、显示参数和海报，不解码媒体或执行播放
type Movie struct {
	File          FileSpecification
	Width, Height float64
	Rotate        int
	Poster        Object
}

// Sound 保存音频采样参数和数据流，不执行播放或格式转换
type Sound struct {
	Stream      *Stream
	Rate        float64
	Channels    int
	Bits        int
	Encoding    Name
	Compression Name
	File        *FileSpecification
}

// FileSpecification 保存PDF文件说明，不读取外部文件或访问网络
type FileSpecification struct {
	Name        string
	Description string
	Embedded    *Stream
	FileSystem  Name
	Dictionary  Dictionary
}

// FileResolver 由调用方读取外部文件，库不自动访问网络或本地路径
type FileResolver func(context.Context, FileSpecification) ([]byte, error)

// ReadFileData 读取内嵌文件或交给调用方解析外部文件，不改变文件编码
// 入参: ctx 取消上下文, file 文件说明, resolver 外部文件读取器，可为空
// 返回: []byte 文件数据, error 解码、取消或外部文件不可用
func (r *Reader) ReadFileData(ctx context.Context, file FileSpecification, resolver FileResolver) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	var data []byte
	var err error
	if file.Embedded != nil {
		data, err = file.Embedded.Decode()
	} else if resolver != nil {
		data, err = resolver(ctx, file)
	} else {
		return nil, &UnsupportedError{Feature: "external file " + file.Name}
	}
	if err != nil {
		return nil, err
	}
	return data, ctx.Err()
}

// ReadMovie 读取视频字典，未声明尺寸时保留零值
// 入参: object 视频字典或间接引用
// 返回: Movie 视频信息, error 字典或显示参数错误
func (r *Reader) ReadMovie(object Object) (Movie, error) {
	value, err := r.Resolve(object)
	if err != nil {
		return Movie{}, err
	}
	dict, ok := value.(Dictionary)
	if !ok {
		return Movie{}, fmt.Errorf("invalid movie dictionary")
	}
	file, err := r.ReadFileSpecification(dict["F"])
	if err != nil {
		return Movie{}, err
	}
	movie := Movie{File: file}
	value, err = r.Resolve(dict["Aspect"])
	if err != nil {
		return movie, err
	}
	if value != nil {
		array, ok := value.(Array)
		if !ok || len(array) != 2 {
			return movie, fmt.Errorf("invalid movie aspect")
		}
		for index, target := range []*float64{&movie.Width, &movie.Height} {
			*target, err = r.number(array[index])
			if err != nil || *target <= 0 || math.IsInf(*target, 0) || math.IsNaN(*target) {
				return movie, fmt.Errorf("invalid movie dimension")
			}
		}
	}
	value, err = r.Resolve(dict["Rotate"])
	if err != nil {
		return movie, err
	}
	if value != nil {
		rotation, ok := value.(Integer)
		if !ok || rotation%90 != 0 {
			return movie, fmt.Errorf("invalid movie rotation")
		}
		movie.Rotate = int((rotation%360 + 360) % 360)
	}
	movie.Poster, err = r.Resolve(dict["Poster"])
	if err != nil {
		return movie, err
	}
	switch movie.Poster.(type) {
	case nil, Boolean:
	case *Stream:
		if movie.Poster.(*Stream).Dictionary["Subtype"] != Name("Image") {
			return movie, fmt.Errorf("invalid movie poster image")
		}
	default:
		return movie, fmt.Errorf("invalid movie poster")
	}
	return movie, nil
}

// ReadSound 读取音频对象，原始多字节样本采用大端顺序
// 入参: object 音频流或间接引用
// 返回: Sound 音频参数, error 无效对象或参数
func (r *Reader) ReadSound(object Object) (Sound, error) {
	value, err := r.Resolve(object)
	if err != nil {
		return Sound{}, err
	}
	stream, ok := value.(*Stream)
	if !ok {
		return Sound{}, fmt.Errorf("invalid sound stream")
	}
	result := Sound{Stream: stream, Channels: 1, Bits: 8, Encoding: "Raw"}
	if file, err := r.Resolve(stream.Dictionary["F"]); err != nil {
		return result, err
	} else if file != nil {
		specification, err := r.ReadFileSpecification(file)
		result.File = &specification
		return result, err
	}
	result.Rate, err = r.number(stream.Dictionary["R"])
	if err != nil || math.IsNaN(result.Rate) || math.IsInf(result.Rate, 0) || result.Rate <= 0 {
		return result, fmt.Errorf("invalid sound sampling rate")
	}
	for _, field := range []struct {
		key    Name
		target *int
	}{{"C", &result.Channels}, {"B", &result.Bits}} {
		value, err := r.Resolve(stream.Dictionary[field.key])
		if err != nil {
			return result, err
		}
		if value == nil {
			continue
		}
		n, ok := value.(Integer)
		if !ok || n <= 0 || uint64(n) > uint64(^uint(0)>>1) {
			return result, fmt.Errorf("invalid sound %s", field.key)
		}
		*field.target = int(n)
	}
	for _, field := range []struct {
		key    Name
		target *Name
	}{{"E", &result.Encoding}, {"CO", &result.Compression}} {
		value, err := r.Resolve(stream.Dictionary[field.key])
		if err != nil {
			return result, err
		}
		if value == nil {
			continue
		}
		name, ok := value.(Name)
		if !ok {
			return result, fmt.Errorf("invalid sound %s", field.key)
		}
		*field.target = name
	}
	if result.Encoding != "Raw" && result.Encoding != "Signed" && result.Encoding != "muLaw" && result.Encoding != "ALaw" {
		return result, fmt.Errorf("invalid sound encoding")
	}
	return result, nil
}

// ReadFileSpecification 解析文件名、说明和可选的内嵌数据流，优先使用Unicode文件名
// 入参: object 文件说明字典、字符串或间接引用
// 返回: FileSpecification 文件说明, error 结构或文本编码错误
func (r *Reader) ReadFileSpecification(object Object) (FileSpecification, error) {
	value, err := r.Resolve(object)
	if err != nil {
		return FileSpecification{}, err
	}
	result := FileSpecification{}
	if name, ok := value.(String); ok {
		result.Name, err = DecodeTextString(name)
		return result, err
	}
	dict, ok := value.(Dictionary)
	if !ok {
		return result, fmt.Errorf("invalid file specification")
	}
	result.Dictionary = dict
	if value, err := r.Resolve(dict["FS"]); err != nil {
		return result, err
	} else if value != nil {
		result.FileSystem, ok = value.(Name)
		if !ok {
			return result, fmt.Errorf("invalid file system")
		}
	}
	key := Name("UF")
	name, err := r.Resolve(dict[key])
	if err != nil {
		return result, err
	}
	if name == nil {
		key = "F"
		name, err = r.Resolve(dict[key])
		if err != nil {
			return result, err
		}
	}
	for _, field := range []struct {
		value  Object
		target *string
	}{{name, &result.Name}, {dict["Desc"], &result.Description}} {
		value, err := r.Resolve(field.value)
		if err != nil {
			return result, err
		}
		if value == nil {
			continue
		}
		encoded, ok := value.(String)
		if !ok {
			return result, fmt.Errorf("invalid file specification text")
		}
		*field.target, err = DecodeTextString(encoded)
		if err != nil {
			return result, err
		}
	}
	embedded, err := r.Resolve(dict["EF"])
	if err != nil {
		return result, err
	}
	if embedded == nil {
		return result, nil
	}
	files, ok := embedded.(Dictionary)
	if !ok {
		return result, fmt.Errorf("invalid embedded file dictionary")
	}
	stream, err := r.Resolve(files[key])
	if err != nil {
		return result, err
	}
	if stream == nil && key == "UF" {
		stream, err = r.Resolve(files["F"])
		if err != nil {
			return result, err
		}
	}
	if stream != nil {
		result.Embedded, ok = stream.(*Stream)
		if !ok {
			return result, fmt.Errorf("invalid embedded file stream")
		}
	}
	return result, nil
}
