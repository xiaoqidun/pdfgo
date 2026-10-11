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
	"maps"
	"math"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"

	"golang.org/x/text/unicode/bidi"
)

// annotationShapeCacheLimit 按字符与字形总数限制临时缓存，单个超限结果仅保留最近一次
const annotationShapeCacheLimit = 1 << 16

// annotationShapedText 保存整形文字的外观范围、默认状态和对齐方式
type annotationShapedText struct {
	text      string
	matrix    Matrix
	font      Name
	defaults  string
	frame     Rectangle
	clip      Rectangle
	align     Integer
	multiline bool
	free      bool
	choice    *ChoiceField
}

// annotationShapeKey 按原文和段落方向区分整形结果
type annotationShapeKey struct {
	text string
	rtl  bool
}

// annotationShaper 在单次外观生成中保留段落整形，并限量缓存断行与字形编码
type annotationShaper struct {
	ctx         context.Context
	shape       TextShaper
	font        *Font
	cache       map[annotationShapeKey]*annotationShapedLine
	entries     map[uint16]fontEncodedGlyph
	cacheSize   int
	input       string
	paragraphs  []annotationShapedParagraph
	widths      map[uint16]float64
	resourceIDs []uint16
}

// annotationShapedParagraph 保存段落方向与完整整形结果，不随候选行缓存淘汰
type annotationShapedParagraph struct {
	line *annotationShapedLine
	rtl  bool
}

// annotationShapedLine 保存不可变的整行字形与逻辑簇边界
type annotationShapedLine struct {
	runes    []rune
	glyphs   []ShapedGlyph
	clusters []annotationShapedCluster
	advance  float64
	words    int
}

// annotationShapedCluster 保存原文范围、视觉顺序和推进量
type annotationShapedCluster struct {
	start, end int
	visual     int
	advance    float64
	words      int
}

// annotationNeedsShaping 判断文字是否需要字形组合、上下文替换或双向排列
// 入参: text 原文
// 返回: bool 是否需要整形
func annotationNeedsShaping(text string) bool {
	for _, c := range text {
		if unicode.IsMark(c) || unicode.Is(unicode.Cf, c) || unicode.IsLetter(c) && !unicode.In(c, unicode.Latin, unicode.Greek, unicode.Cyrillic, unicode.Han, unicode.Hiragana, unicode.Katakana, unicode.Hangul) {
			return true
		}
	}
	return false
}

// annotationParagraphRTL 按Unicode双向算法P2与P3确定段落方向，不计隔离区段内的强方向字符
// 入参: text 段落原文
// 返回: bool 是否从右向左
func annotationParagraphRTL(text string) bool {
	isolates := 0
	for _, c := range text {
		property, _ := bidi.LookupRune(c)
		switch property.Class() {
		case bidi.LRI, bidi.RLI, bidi.FSI:
			isolates++
			continue
		case bidi.PDI:
			isolates = max(0, isolates-1)
			continue
		}
		if isolates != 0 {
			continue
		}
		switch property.Class() {
		case bidi.L:
			return false
		case bidi.R, bidi.AL:
			return true
		}
	}
	return false
}

// line 整形单行并验证簇与编码，不以缺省字形补齐缺字
// 入参: text 行原文, rtl 段落方向
// 返回: *annotationShapedLine 整行结果, error 字形、编码或取消错误
func (s *annotationShaper) line(text string, rtl bool) (*annotationShapedLine, error) {
	if err := s.ctx.Err(); err != nil {
		return nil, err
	}
	key := annotationShapeKey{text, rtl}
	if line := s.cache[key]; line != nil {
		return line, nil
	}
	line := &annotationShapedLine{runes: []rune(text)}
	if text == "" {
		s.cache[key] = line
		return line, nil
	}
	if !utf8.ValidString(text) {
		return nil, fmt.Errorf("invalid shaping text")
	}
	glyphs, err := s.shape(s.ctx, s.font, text, rtl)
	if err != nil {
		return nil, err
	}
	if err := s.ctx.Err(); err != nil {
		return nil, err
	}
	if len(glyphs) == 0 || glyphs[0].Cluster != 0 {
		return nil, fmt.Errorf("missing shaped text clusters")
	}
	line.glyphs = slices.Clone(glyphs)
	if s.widths == nil {
		s.widths = make(map[uint16]float64)
	}
	wanted := make(map[uint16]int)
	for _, glyph := range line.glyphs {
		if err := s.ctx.Err(); err != nil {
			return nil, err
		}
		if glyph.ID == 0 || glyph.Cluster < 0 || glyph.Cluster >= len(line.runes) || math.IsNaN(glyph.X) || math.IsInf(glyph.X, 0) || math.IsNaN(glyph.Y) || math.IsInf(glyph.Y, 0) || math.IsNaN(glyph.Advance) || math.IsInf(glyph.Advance, 0) {
			return nil, fmt.Errorf("invalid shaped glyph or position")
		}
		if previous, exists := s.widths[glyph.ID]; math.IsNaN(glyph.Width) || math.IsInf(glyph.Width, 0) || exists && previous != glyph.Width {
			return nil, fmt.Errorf("invalid or inconsistent shaped glyph width")
		}
		s.widths[glyph.ID] = glyph.Width
		if _, ok := s.entries[glyph.ID]; !ok {
			wanted[glyph.ID]++
		}
		if len(line.clusters) == 0 || glyph.Cluster != line.clusters[len(line.clusters)-1].start {
			if len(line.clusters) != 0 {
				previous := &line.clusters[len(line.clusters)-1]
				if glyph.Cluster < previous.start {
					return nil, fmt.Errorf("unordered shaped clusters")
				}
				previous.end = glyph.Cluster
			}
			line.clusters = append(line.clusters, annotationShapedCluster{start: glyph.Cluster, end: len(line.runes), visual: glyph.VisualOrder})
		}
		cluster := &line.clusters[len(line.clusters)-1]
		if cluster.visual != glyph.VisualOrder {
			return nil, fmt.Errorf("inconsistent shaped cluster order")
		}
		cluster.advance += glyph.Advance
	}
	if len(wanted) != 0 {
		entries, err := s.font.glyphLookup(s.ctx, wanted)
		if err != nil {
			return nil, err
		}
		for id := range wanted {
			entry, ok := entries[id]
			if !ok {
				if s.resourceIDs == nil {
					s.resourceIDs, _, _, err = s.font.glyphResourceMapping(s.ctx)
					if err != nil {
						return nil, err
					}
				}
				if int(id) >= len(s.resourceIDs) {
					return nil, fmt.Errorf("shaped glyph exceeds font program")
				}
			}
			s.entries[id] = entry
		}
	}
	seen := make([]bool, len(line.clusters))
	for _, cluster := range line.clusters {
		if cluster.visual < 0 || cluster.visual >= len(seen) || seen[cluster.visual] || math.IsNaN(cluster.advance) || math.IsInf(cluster.advance, 0) {
			return nil, fmt.Errorf("invalid shaped cluster order or advance")
		}
		seen[cluster.visual] = true
	}
	cluster := 0
	for _, glyph := range line.glyphs {
		if glyph.Cluster != line.clusters[cluster].start {
			cluster++
		}
		if s.entries[glyph.ID].glyph.WordSpace {
			line.clusters[cluster].words++
		}
	}
	for _, cluster := range line.clusters {
		line.advance += cluster.advance
		if cluster.visual+1 < len(line.clusters) {
			line.words += cluster.words
		}
	}
	if math.IsNaN(line.advance) || math.IsInf(line.advance, 0) {
		return nil, fmt.Errorf("shaped line advance exceeds coordinate range")
	}
	if err := s.ctx.Err(); err != nil {
		return nil, err
	}
	units := len(line.runes) + len(line.glyphs)
	if units > annotationShapeCacheLimit-s.cacheSize {
		clear(s.cache)
		s.cacheSize = 0
	}
	s.cacheSize += units
	s.cache[key] = line
	return line, nil
}

// fontResource 为源编码未覆盖的整形字形建立独立字体，保留原单词间距语义
// 返回: *Font 新字体，无需替换时为空, error 字体、编码或取消错误
func (s *annotationShaper) fontResource() (*Font, error) {
	if s.resourceIDs == nil {
		return nil, s.ctx.Err()
	}
	widths := make([]GlyphWidth, 0, len(s.entries))
	wanted := make(map[uint16]int, len(s.entries))
	for id := range s.entries {
		if err := s.ctx.Err(); err != nil {
			return nil, err
		}
		widths = append(widths, GlyphWidth{ID: id, Width: s.widths[id]})
		wanted[id] = 1
	}
	font, err := s.font.GlyphFontContext(s.ctx, widths)
	if err != nil {
		return nil, err
	}
	entries, err := font.glyphLookup(s.ctx, wanted)
	if err != nil {
		return nil, err
	}
	for id, previous := range s.entries {
		entry, exists := entries[id]
		if !exists {
			return nil, fmt.Errorf("missing generated glyph encoding")
		}
		entry.glyph.WordSpace = previous.glyph.WordSpace
		entries[id] = entry
	}
	s.entries = entries
	return font, s.ctx.Err()
}

// width 计算整行推进量，视觉末簇后不追加字符或单词间距
// 入参: state 文字状态, size 字号
// 返回: float64 行宽
func (l *annotationShapedLine) width(state graphicsState, size float64) float64 {
	return (l.advance*size/1000 + float64(max(0, len(l.clusters)-1))*state.spacing + float64(l.words)*state.wordSpacing) * state.hscale
}

// prepare 保留本次原文的段落整形，避免自动字号计算反复处理完整段落
// 入参: text 原文
// 返回: []annotationShapedParagraph 各段结果, error 整形或取消错误
func (s *annotationShaper) prepare(text string) ([]annotationShapedParagraph, error) {
	if err := s.ctx.Err(); err != nil {
		return nil, err
	}
	if s.paragraphs != nil && s.input == text {
		return s.paragraphs, nil
	}
	parts := strings.Split(text, "\n")
	paragraphs := make([]annotationShapedParagraph, len(parts))
	for i, part := range parts {
		rtl := annotationParagraphRTL(part)
		line, err := s.line(part, rtl)
		if err != nil {
			return nil, err
		}
		paragraphs[i] = annotationShapedParagraph{line: line, rtl: rtl}
	}
	if err := s.ctx.Err(); err != nil {
		return nil, err
	}
	s.input, s.paragraphs = text, paragraphs
	return paragraphs, nil
}

// lines 按逻辑簇和空白边界折行，断行后重新整形并保留段落方向
// 入参: text 原文, state 文字状态, size 字号, width 行宽, multiline 是否多行
// 返回: []*annotationShapedLine 各行结果, error 整形或取消错误
func (s *annotationShaper) lines(text string, state graphicsState, size, width float64, multiline bool) ([]*annotationShapedLine, error) {
	paragraphs, err := s.prepare(text)
	if err != nil {
		return nil, err
	}
	var lines []*annotationShapedLine
	for _, paragraph := range paragraphs {
		if err := s.ctx.Err(); err != nil {
			return nil, err
		}
		full, rtl := paragraph.line, paragraph.rtl
		if !multiline || len(full.clusters) == 0 || full.width(state, size) <= width {
			lines = append(lines, full)
			continue
		}
		for start := 0; start < len(full.clusters); {
			end, lastBreak := start, start
			advance := 0.0
			for end < len(full.clusters) {
				if err := s.ctx.Err(); err != nil {
					return nil, err
				}
				next := full.clusters[end].advance * size / 1000
				if end > start {
					next += state.spacing + float64(full.clusters[end-1].words)*state.wordSpacing
				}
				if (advance+next)*state.hscale > width && end > start {
					break
				}
				advance += next
				last := full.runes[full.clusters[end].end-1]
				end++
				if unicode.IsSpace(last) && last != '\u00a0' && last != '\u2007' && last != '\u202f' {
					lastBreak = end
				}
			}
			if end < len(full.clusters) && lastBreak > start {
				end = lastBreak
			}
			var line *annotationShapedLine
			for {
				line, err = s.line(string(full.runes[full.clusters[start].start:full.clusters[end-1].end]), rtl)
				if err != nil {
					return nil, err
				}
				if end == start+1 || line.width(state, size) <= width {
					break
				}
				end--
			}
			lines = append(lines, line)
			start = end
		}
	}
	if err := s.ctx.Err(); err != nil {
		return nil, err
	}
	return lines, nil
}

// write 生成带字形定位和ActualText的横排外观，不改变源字体编码
// 入参: ctx 取消上下文, shape 整形后端, p 默认文字状态, resources 外观资源, content 外观内容
// 返回: error 整形、布局或取消错误
func (v annotationShapedText) write(ctx context.Context, shape TextShaper, p *pageInterpreter, resources Dictionary, content *strings.Builder) error {
	s := annotationShaper{ctx: ctx, shape: shape, font: p.state.font, cache: make(map[annotationShapeKey]*annotationShapedLine), entries: make(map[uint16]fontEncodedGlyph)}
	state := p.state
	width, height := v.frame.XMax-v.frame.XMin, v.frame.YMax-v.frame.YMin
	size := state.fontSize
	list := v.choice != nil && !v.choice.Combo
	if list && size == 0 {
		size = min(12, height)
	}
	if size == 0 {
		low, high := 0.0, height
		for range 32 {
			candidate := (low + high) / 2
			lines, err := s.lines(v.text, state, candidate, width, v.multiline)
			if err != nil {
				return err
			}
			leading := state.leading
			if leading <= 0 {
				leading = candidate * 1.2
			}
			fits := float64(len(lines)-1)*leading+candidate <= height
			for _, line := range lines {
				fits = fits && line.width(state, candidate) <= width
			}
			if fits {
				low = candidate
			} else {
				high = candidate
			}
		}
		size = low
	}
	if size <= 0 {
		return fmt.Errorf("invalid shaped font size")
	}
	leading := state.leading
	if leading <= 0 {
		leading = size * 1.2
	}
	text := v.text
	if list {
		var err error
		text, err = annotationChoiceVisibleText(ctx, text, state, height)
		if err != nil {
			return err
		}
	}
	lines, err := s.lines(text, state, size, width, v.multiline && !list)
	if err != nil {
		return err
	}
	actual, err := EncodeTextString(text)
	if err != nil {
		return err
	}
	font, err := s.fontResource()
	if err != nil {
		return err
	}
	if font != nil {
		value, err := s.font.reader.Resolve(resources["Font"])
		if err != nil {
			return err
		}
		fonts, ok := value.(Dictionary)
		if value != nil && !ok {
			return fmt.Errorf("invalid shaped font resources")
		}
		fonts = maps.Clone(fonts)
		if fonts == nil {
			fonts = Dictionary{}
		}
		v.font = "Shaped"
		for i := 1; fonts[v.font] != nil; i++ {
			v.font = Name(fmt.Sprintf("Shaped%d", i))
		}
		fonts[v.font] = font.Dictionary
		resources["Font"] = fonts
	}
	content.WriteString("q\n")
	writeAnnotationOperation(content, "re W n", v.clip.XMin, v.clip.YMin, v.clip.XMax-v.clip.XMin, v.clip.YMax-v.clip.YMin)
	if list {
		content.WriteString("q\n")
		writeAnnotationOperation(content, "cm", v.matrix[:]...)
		for _, selected := range v.choice.Selected {
			if err := ctx.Err(); err != nil {
				return err
			}
			row := selected - v.choice.TopIndex
			if row >= 0 && row < len(lines) {
				content.WriteString("q 0.153 0.392 0.714 rg\n")
				writeAnnotationOperation(content, "re f Q", v.frame.XMin, v.frame.YMax-float64(row+1)*leading, width, leading)
			}
		}
		content.WriteString("Q\n")
	}
	if !v.free {
		content.WriteString("/Tx BMC\n")
	}
	fmt.Fprintf(content, "/Span << /ActualText <%x> >> BDC\nBT\n%s\n/%s ", []byte(actual), v.defaults, escapeAnnotationName(v.font))
	writeAnnotationOperation(content, "Tf", size)
	baseline := v.frame.YMin + (height-size)/2 + size*.2 - state.rise
	if v.multiline {
		baseline = v.frame.YMax - size - state.rise
	}
	for i, line := range lines {
		if list {
			content.WriteString("0 g\n" + v.defaults)
			fmt.Fprintf(content, "/%s ", escapeAnnotationName(v.font))
			writeAnnotationOperation(content, "Tf", size)
			if _, selected := slices.BinarySearch(v.choice.Selected, v.choice.TopIndex+i); selected {
				content.WriteString("1 g\n")
			}
		}
		left := v.frame.XMin + float64(v.align)*(width-line.width(state, size))/2
		if err := s.writeLine(content, line, state, size, v.matrix, left, baseline-float64(i)*leading); err != nil {
			return err
		}
	}
	content.WriteString("ET\nEMC\n")
	if !v.free {
		content.WriteString("EMC\n")
	}
	content.WriteString("Q\n")
	return ctx.Err()
}

// writeLine 按视觉位置写入整形字形，保留间距、偏移与原始文字矩阵
// 入参: content 外观内容, line 整形行, state 文字状态, size 字号, matrix 定位矩阵, left 左边界, baseline 基线
// 返回: error 定位或取消错误
func (s *annotationShaper) writeLine(content *strings.Builder, line *annotationShapedLine, state graphicsState, size float64, matrix Matrix, left, baseline float64) error {
	extra := make([]float64, len(line.clusters))
	for _, cluster := range line.clusters {
		extra[cluster.visual] = state.spacing + float64(cluster.words)*state.wordSpacing
	}
	shift := 0.0
	for order, spacing := range extra {
		extra[order] = shift
		shift += spacing
	}
	for _, glyph := range line.glyphs {
		if err := s.ctx.Err(); err != nil {
			return err
		}
		x := left + (glyph.X*size/1000+extra[glyph.VisualOrder])*state.hscale
		y := baseline + glyph.Y*size/1000
		point := matrix.Apply(Point{x, y})
		if math.IsNaN(point.X) || math.IsInf(point.X, 0) || math.IsNaN(point.Y) || math.IsInf(point.Y, 0) {
			return fmt.Errorf("shaped text position exceeds coordinate range")
		}
		writeAnnotationTextMatrix(content, matrix, x, y)
		fmt.Fprintf(content, "<%x> Tj\n", s.entries[glyph.ID].code)
	}
	return s.ctx.Err()
}
