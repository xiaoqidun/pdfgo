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
	"embed"
	"fmt"
	"math"
	"strconv"
	"strings"
	"sync"
)

// coreFontResources 保存标准14种字体的原始度量资源及授权文件
// https://github.com/apache/pdfbox/tree/trunk/pdfbox/src/main/resources/org/apache/pdfbox/resources/afm
//
//go:embed assets/afm
var coreFontResources embed.FS

// coreFonts 按需加载标准字体度量，后续解码共享只读数据
var coreFonts = sync.OnceValues(func() (map[string]*coreFontMetrics, error) {
	fonts := make(map[string]*coreFontMetrics, 14)
	for _, name := range []string{"Courier", "Courier-Bold", "Courier-Oblique", "Courier-BoldOblique", "Helvetica", "Helvetica-Bold", "Helvetica-Oblique", "Helvetica-BoldOblique", "Times-Roman", "Times-Bold", "Times-Italic", "Times-BoldItalic", "Symbol", "ZapfDingbats"} {
		data, err := coreFontResources.ReadFile("assets/afm/" + name + ".afm")
		if err != nil {
			return nil, err
		}
		metrics, err := parseCoreFontMetrics(data)
		if err != nil {
			return nil, fmt.Errorf("font %s: %w", name, err)
		}
		fonts[name] = metrics
	}
	return fonts, nil
})

// coreFontMetrics 保存标准字体的缺省编码及千分之一字宽
type coreFontMetrics struct {
	names  [256]string
	widths map[string]float64
}

// parseCoreFontMetrics 读取内置AFM资源中的字符编码、名称和横向字宽
// 入参: data 原始AFM资源
// 返回: *coreFontMetrics 标准字体度量, error 无效字符度量
func parseCoreFontMetrics(data []byte) (*coreFontMetrics, error) {
	metrics := &coreFontMetrics{widths: make(map[string]float64)}
	remaining := -1
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		if fields[0] == "StartCharMetrics" {
			if len(fields) != 2 || remaining != -1 {
				return nil, fmt.Errorf("invalid AFM character count")
			}
			var err error
			remaining, err = strconv.Atoi(fields[1])
			if err != nil || remaining <= 0 {
				return nil, fmt.Errorf("invalid AFM character count")
			}
			continue
		}
		if fields[0] == "EndCharMetrics" {
			if remaining != 0 {
				return nil, fmt.Errorf("AFM character count mismatch")
			}
			return metrics, nil
		}
		if remaining < 0 || fields[0] == "Comment" {
			continue
		}
		code, name, width, hasWidth := -2, "", float64(0), false
		for _, field := range strings.Split(line, ";") {
			values := strings.Fields(field)
			if len(values) == 0 {
				continue
			}
			switch values[0] {
			case "C":
				if len(values) != 2 {
					return nil, fmt.Errorf("invalid AFM character code")
				}
				var err error
				code, err = strconv.Atoi(values[1])
				if err != nil || code < -1 || code > 255 {
					return nil, fmt.Errorf("invalid AFM character code")
				}
			case "N":
				if len(values) != 2 {
					return nil, fmt.Errorf("invalid AFM glyph name")
				}
				name = values[1]
			case "WX":
				if len(values) != 2 {
					return nil, fmt.Errorf("invalid AFM glyph width")
				}
				var err error
				width, err = strconv.ParseFloat(values[1], 64)
				if err != nil || math.IsNaN(width) || math.IsInf(width, 0) {
					return nil, fmt.Errorf("invalid AFM glyph width")
				}
				hasWidth = true
			}
		}
		if remaining == 0 || code == -2 || name == "" || !hasWidth {
			return nil, fmt.Errorf("invalid AFM character metrics")
		}
		if _, exists := metrics.widths[name]; exists || code >= 0 && metrics.names[code] != "" {
			return nil, fmt.Errorf("duplicate AFM character metrics")
		}
		metrics.widths[name] = width
		if code >= 0 {
			metrics.names[code] = name
		}
		remaining--
	}
	return nil, fmt.Errorf("missing AFM character metrics terminator")
}
