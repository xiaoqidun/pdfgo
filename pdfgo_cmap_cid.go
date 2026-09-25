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
	"strconv"
	"sync"
)

// CIDMap保存字符码到CID的映射
type CIDMap map[string]uint16

//go:embed assets/cmap/UniGB-UCS2-H
var uniGBUCS2H []byte

var loadUniGBUCS2H = sync.OnceValues(func() (CIDMap, error) {
	return ParseCIDMap(uniGBUCS2H)
})

// ParseCIDMap读取CMap中的字符码到CID的直接映射及连续范围
// 入参: data CMap数据
// 返回: CIDMap字符映射, error错误信息
func ParseCIDMap(data []byte) (CIDMap, error) {
	p := objectParser{data: data}
	result := CIDMap{}
	previous := ""
	for {
		p.skipSpace()
		if p.pos == len(data) {
			return result, nil
		}
		if b := p.data[p.pos]; b == '/' || b == '(' || b == '<' || b == '[' {
			if _, err := p.object(); err != nil {
				return nil, err
			}
			previous = ""
			continue
		}
		token := p.token()
		if token == "" {
			p.pos++
			previous = ""
			continue
		}
		if token == "usecmap" {
			return nil, &UnsupportedError{Feature: "inherited CID CMap"}
		}
		if token != "begincidchar" && token != "begincidrange" {
			previous = token
			continue
		}
		count, err := strconv.Atoi(previous)
		if err != nil || count < 0 || count > 65536 {
			return nil, p.fail("invalid CMap mapping count")
		}
		for range count {
			from, err := p.object()
			if err != nil {
				return nil, err
			}
			first, ok := from.(String)
			if !ok || len(first) == 0 || len(first) > 4 {
				return nil, p.fail("invalid CMap source code")
			}
			last := first
			if token == "begincidrange" {
				to, err := p.object()
				if err != nil {
					return nil, err
				}
				last, ok = to.(String)
				if !ok || len(last) != len(first) {
					return nil, p.fail("invalid CMap source range")
				}
			}
			value, err := p.object()
			if err != nil {
				return nil, err
			}
			cid, ok := value.(Integer)
			start, end := codeNumber(first), codeNumber(last)
			if !ok || cid < 0 || cid > 65535 || end < start || end-start > 65535 || uint64(cid)+end-start > 65535 {
				return nil, p.fail("invalid CMap CID range")
			}
			for code := start; code <= end; code++ {
				key := make([]byte, len(first))
				v := code
				for n := len(key) - 1; n >= 0; n-- {
					key[n] = byte(v)
					v >>= 8
				}
				result[string(key)] = uint16(uint64(cid) + code - start)
			}
		}
		end := "endcidchar"
		if token == "begincidrange" {
			end = "endcidrange"
		}
		if p.token() != end {
			return nil, p.fail("missing CMap block terminator")
		}
		previous = ""
	}
}
