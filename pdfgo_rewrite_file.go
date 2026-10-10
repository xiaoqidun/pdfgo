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

import "context"

// rewriteFile 写出结构化文件说明及可选内嵌内容，不访问外部文件或添加时间信息
// 入参: ctx 取消上下文, file 文件说明, result 输出对象, number 最大对象编号
// 返回: Reference 文件说明引用, error 参数、资源复制或取消错误
func rewriteFile(ctx context.Context, file FileSpecification, result map[Reference]Object, number *int64) (Reference, error) {
	embedded := file.Embedded
	file.Embedded = nil
	spec, err := externalFileSpecification(file)
	if err != nil {
		return Reference{}, err
	}
	if embedded != nil {
		copier := rewriteObjectCopy{ctx: ctx, source: embedded.reader, objects: result, number: number, copied: make(map[rewriteObjectIdentity]Reference)}
		value, err := copier.copy(embedded, 0)
		if err != nil {
			return Reference{}, err
		}
		ref := value.(Reference)
		result[ref].(*Stream).Dictionary["Type"] = Name("EmbeddedFile")
		spec["EF"] = Dictionary{"F": ref, "UF": ref}
	}
	if err := ctx.Err(); err != nil {
		return Reference{}, err
	}
	return appendRewriteObject(result, number, spec)
}
