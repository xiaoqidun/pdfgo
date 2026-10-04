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

// PostScriptMark 交付仅用于PostScript输出的原始程序，不执行程序
// Data及Level1为独立解码数据，Level1为nil表示未提供低版本程序
// Resources属于阅读器，字体按BaseFont名称由输出方处理
type PostScriptMark struct {
	Data      []byte
	Level1    []byte
	Matrix    Matrix
	Style     Style
	Resources Dictionary
}

// postscript 仅向显式输出访问器交付原始程序，其他设备不绘制
// 入参: stream PostScript数据流
// 返回: error 解码、访问或取消错误
func (p *pageInterpreter) postscript(stream *Stream) error {
	if err := p.ctx.Err(); err != nil {
		return err
	}
	visible, err := p.optionalVisible(stream.Dictionary["OC"])
	if err != nil || !visible {
		return err
	}
	if p.visitor.PostScript == nil {
		return nil
	}
	data, err := stream.DecodeContext(p.ctx)
	if err != nil {
		return err
	}
	mark := PostScriptMark{Data: data, Matrix: p.state.matrix, Style: p.state.style, Resources: p.resources}
	value, err := p.reader.Resolve(stream.Dictionary["Level1"])
	if err != nil {
		return err
	}
	if value != nil {
		level1, ok := value.(*Stream)
		if !ok || level1 == nil {
			return fmt.Errorf("invalid PostScript Level1 stream")
		}
		mark.Level1, err = level1.DecodeContext(p.ctx)
		if err != nil {
			return err
		}
		if mark.Level1 == nil {
			mark.Level1 = []byte{}
		}
	}
	if err := p.visitor.PostScript(mark); err != nil {
		return err
	}
	return p.ctx.Err()
}
