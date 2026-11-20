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
	_ "embed"
	"math"
	"sync"
)

// defaultCMYKProfile 保存CC0公有领域CGATS TR 001印刷配置文件，用于未标定DeviceCMYK屏显
// https://github.com/saucecontrol/Compact-ICC-Profiles
//
//go:embed assets/color/CGATS001Compat-v2-micro.icc
var defaultCMYKProfile []byte

// defaultCMYKSpace 延迟解析内置印刷CMYK色彩空间，未处理CMYK文档时不占用内存
var defaultCMYKSpace = sync.OnceValue(func() *iccColorSpace {
	space, err := NewICCColorSpace(defaultCMYKProfile)
	if err != nil {
		return nil
	}
	return space.profile
})

// DeviceCMYKToSRGB 按内置印刷配置文件将未标定CMYK分量转换为sRGB显示颜色
// 入参: c 青色分量, m 品红分量, y 黄色分量, k 黑色分量，范围均为0至1
// 返回: [3]float64 sRGB单位分量
func DeviceCMYKToSRGB(c, m, y, k float64) [3]float64 {
	values := [4]float64{
		math.Max(0, math.Min(1, c)),
		math.Max(0, math.Min(1, m)),
		math.Max(0, math.Min(1, y)),
		math.Max(0, math.Min(1, k)),
	}
	if profile := defaultCMYKSpace(); profile != nil {
		if rgb, err := profile.color(values[:], "Perceptual"); err == nil {
			return rgb
		}
	}
	return [3]float64{1 - math.Min(1, values[0]+values[3]), 1 - math.Min(1, values[1]+values[3]), 1 - math.Min(1, values[2]+values[3])}
}
