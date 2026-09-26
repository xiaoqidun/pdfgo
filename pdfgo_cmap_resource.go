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
	"io/fs"
	"path"
	"strings"
	"sync"
)

// cmapResources 保存Adobe官方原始映射，按上游目录区分编码和Unicode资源
// https://github.com/adobe-type-tools/cmap-resources
// https://github.com/adobe-type-tools/mapping-resources-pdf
//
//go:embed assets/cmap/cmap-resources assets/cmap/mapping-resources-pdf
var cmapResources embed.FS

// cmapResourceIndex 延迟索引资源名称，不在启动时解析全部映射
var cmapResourceIndex = sync.OnceValues(func() (map[Name]string, error) {
	index := map[Name]string{}
	err := fs.WalkDir(cmapResources, "assets/cmap", func(name string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return err
		}
		if !strings.Contains(name, "/CMap/") && !strings.Contains(name, "/pdf2unicode/") && !strings.Contains(name, "/pdf2other/") {
			return nil
		}
		key := Name(path.Base(name))
		if _, exists := index[key]; exists {
			return fmt.Errorf("duplicate CMap resource %s", key)
		}
		index[key] = name
		return nil
	})
	return index, err
})

// predefinedCIDMaps 复用只读编码映射，竖排资源共享横排基础映射
var predefinedCIDMaps sync.Map

// predefinedUnicodeMaps 复用只读Unicode映射
var predefinedUnicodeMaps sync.Map

// cmapResource 读取命名资源，不将资源名解释为文件系统路径
// 入参: name 资源名称
// 返回: []byte 原始资源, string 上游相对路径, error 读取错误
func cmapResource(name Name) ([]byte, string, error) {
	index, err := cmapResourceIndex()
	if err != nil {
		return nil, "", err
	}
	file, ok := index[name]
	if !ok {
		return nil, "", &UnsupportedError{Feature: "predefined CMap " + string(name)}
	}
	data, err := cmapResources.ReadFile(file)
	return data, file, err
}

// loadCIDCMap 按需解析Adobe编码映射及其基础映射
// 入参: name 资源名称, active 当前继承链
// 返回: *cidCMap 只读映射, error 读取或继承错误
func loadCIDCMap(name Name, active map[Name]bool) (*cidCMap, error) {
	if active[name] {
		return nil, fmt.Errorf("cyclic CMap inheritance")
	}
	if cached, ok := predefinedCIDMaps.Load(name); ok {
		return cached.(*cidCMap), nil
	}
	data, file, err := cmapResource(name)
	if err != nil {
		return nil, err
	}
	if !strings.Contains(file, "/cmap-resources/") {
		return nil, fmt.Errorf("CMap %s is not a CID encoding", name)
	}
	if active == nil {
		active = map[Name]bool{}
	}
	active[name] = true
	defer delete(active, name)
	mapping, err := parseCIDCMap(data)
	if err != nil {
		return nil, err
	}
	if mapping.use != "" {
		mapping.base, err = loadCIDCMap(mapping.use, active)
		if err != nil {
			return nil, err
		}
	}
	mapping.inherit()
	if len(mapping.spaces) == 0 {
		return nil, fmt.Errorf("missing CMap codespace")
	}
	cached, _ := predefinedCIDMaps.LoadOrStore(name, mapping)
	return cached.(*cidCMap), nil
}

// loadUnicodeCMap 按需解析命名Unicode资源，不混用其他目标编码
// 入参: name 资源名称, active 当前继承链
// 返回: UnicodeMap 只读映射, error 读取或继承错误
func loadUnicodeCMap(name Name, active map[Name]bool) (UnicodeMap, error) {
	if active[name] {
		return nil, fmt.Errorf("cyclic ToUnicode CMap inheritance")
	}
	if cached, ok := predefinedUnicodeMaps.Load(name); ok {
		return cached.(UnicodeMap), nil
	}
	data, file, err := cmapResource(name)
	if err != nil {
		return nil, err
	}
	if !strings.Contains(file, "/pdf2unicode/") && !(strings.Contains(file, "/pdf2other/") && (strings.HasSuffix(string(name), "-UCS2") || strings.HasSuffix(string(name), "-UCS2C"))) {
		return nil, fmt.Errorf("CMap %s is not a Unicode mapping", name)
	}
	if active == nil {
		active = map[Name]bool{}
	}
	active[name] = true
	defer delete(active, name)
	mapping, err := parseUnicodeMap(data, active)
	if err != nil {
		return nil, err
	}
	cached, _ := predefinedUnicodeMaps.LoadOrStore(name, mapping)
	return cached.(UnicodeMap), nil
}
