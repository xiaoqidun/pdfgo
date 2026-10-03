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
	"bytes"
	"errors"
	"fmt"
	"sync"
)

// iccInverseSpace 按需读取共享逆向标签，不影响源颜色或配置身份
type iccInverseSpace struct {
	once    sync.Once
	tags    map[string][]byte
	lut     *iccLUTSpace
	process [4]*iccProcessElements
	err     error
}

// newICCInverse 保存逆向标签快照，不解码普通绘制用不到的查找表
// 入参: tags 已校验标签, components 设备分量数, pcs 连接空间
// 返回: *iccInverseSpace 延迟变换，无逆向标签时为空
func newICCInverse(tags map[string][]byte, components int, pcs string) *iccInverseSpace {
	var inverse *iccInverseSpace
	for name, data := range tags {
		if name != "B2A0" && name != "B2A1" && name != "B2A2" && name != "B2D0" && name != "B2D1" && name != "B2D2" && name != "B2D3" {
			continue
		}
		if inverse == nil {
			inverse = &iccInverseSpace{tags: make(map[string][]byte), lut: &iccLUTSpace{components: components, pcs: pcs}}
		}
		for previous, copy := range inverse.tags {
			if sameICCTag(data, tags[previous]) {
				inverse.tags[name] = copy
				break
			}
		}
		if inverse.tags[name] == nil {
			inverse.tags[name] = bytes.Clone(data)
		}
	}
	return inverse
}

// sameICCTag 判断标签是否引用同一段配置数据，不比较或修改内容
// 入参: first 首个标签, second 另一个标签
// 返回: bool 是否共享数据
func sameICCTag(first, second []byte) bool {
	return len(first) > 0 && len(first) == len(second) && &first[0] == &second[0]
}

// validateBlending 按PDF第11.3.4节排除明度色度空间，并检查全部基础意图的逆向变换
// 返回: error 无效混合空间或不可逆的配置
func (s *iccColorSpace) validateBlending() error {
	if s.lab {
		return fmt.Errorf("invalid ICC lightness-chromaticity blending space")
	}
	lut, floating, err := s.inverseTransforms()
	if err != nil {
		return err
	}
	for i := range 3 {
		if floating[i] != nil || lut != nil && (lut.fromPCS[i] != nil || lut.fromPCS[0] != nil) {
			continue
		}
		if s.lut != nil {
			return fmt.Errorf("missing ICC blending inverse for intent %d", i)
		}
		if s.gray != nil {
			m := s.gray.matrix.matrix
			if m[1]+m[4]+m[7] == 0 {
				return fmt.Errorf("singular ICC gray matrix")
			}
			_, err = s.gray.curve.inverse(.5)
		} else {
			if s.rgb.inverse == ([9]float64{}) && iccInverseMatrix(s.rgb.matrix.matrix) == ([9]float64{}) {
				return fmt.Errorf("singular ICC colorant matrix")
			}
			for _, curve := range s.rgb.curves {
				if _, err = curve.inverse(.5); err != nil {
					break
				}
			}
		}
		return err
	}
	return nil
}

// inverseTransforms 取得目标颜色需要的逆向变换，不修改共享正向配置
// 返回: *iccLUTSpace 传统变换, [4]*iccProcessElements 浮点变换, error 无效逆向信息
func (s *iccColorSpace) inverseTransforms() (*iccLUTSpace, [4]*iccProcessElements, error) {
	if s.inverse == nil {
		return s.lut, s.fromFloat, nil
	}
	i := s.inverse
	i.once.Do(i.initialize)
	return i.lut, i.process, i.err
}

// initialize 校验实际使用的逆向标签并释放原始快照，失败结果同样只计算一次
func (s *iccInverseSpace) initialize() {
	defer func() {
		s.tags = nil
		if s.err != nil {
			s.lut.fromPCS = [3]*iccLUT{}
			s.process = [4]*iccProcessElements{}
		}
	}()
	for i := range s.lut.fromPCS {
		name := fmt.Sprintf("B2A%d", i)
		if data := s.tags[name]; data != nil {
			for j := 0; j < i; j++ {
				if sameICCTag(data, s.tags[fmt.Sprintf("B2A%d", j)]) {
					s.lut.fromPCS[i] = s.lut.fromPCS[j]
					break
				}
			}
			if s.lut.fromPCS[i] != nil {
				continue
			}
			s.lut.fromPCS[i], s.err = iccLookupTransform(data, 3, s.lut.components, s.lut.pcs, true)
			if s.err != nil {
				s.err = fmt.Errorf("ICC %s: %w", name, s.err)
				return
			}
		}
	}
	for i := range s.process {
		name := fmt.Sprintf("B2D%d", i)
		if data := s.tags[name]; data != nil {
			for j := 0; j < i; j++ {
				if sameICCTag(data, s.tags[fmt.Sprintf("B2D%d", j)]) {
					s.process[i] = s.process[j]
					break
				}
			}
			if s.process[i] != nil {
				continue
			}
			transform, err := parseICCProcessElements(data)
			var unsupported *UnsupportedError
			if errors.As(err, &unsupported) {
				continue
			}
			if err != nil {
				s.err = fmt.Errorf("ICC %s: %w", name, err)
				return
			}
			if transform.input != 3 || transform.output != s.lut.components {
				s.err = fmt.Errorf("invalid ICC %s channels", name)
				return
			}
			s.process[i] = transform
		}
	}
}
