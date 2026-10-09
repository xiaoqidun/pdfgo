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
	"image"
	"io"
)

// EncodePNGContext 按原始尺寸无损导出图像，颜色、遮罩及精度与DecodeImageContext一致
// JPEG2000按行带解码，校准色逐行转换，不分配整幅显示缓冲；取消或失败时输出可能不完整
// 入参: ctx 取消上下文, writer PNG输出流
// 返回: error 参数、解码、颜色或写入错误
func (i *Image) EncodePNGContext(ctx context.Context, writer io.Writer) error {
	if ctx == nil || writer == nil || i == nil || i.Stream == nil || i.reader == nil {
		return fmt.Errorf("missing image export context, writer or source")
	}
	_, err := i.decodeImageOutput(ctx, nil, false, image.Point{}, writer)
	return err
}
