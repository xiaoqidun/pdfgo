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
	"errors"
	"fmt"
	"maps"
	"math"
	"os"
)

// ErrDestinationNotFound 表示命名目标在文档目标表中不存在
var ErrDestinationNotFound = errors.New("destination not found")

// ErrInvalidDestination 表示目标结构或参数不符合标准，不包含读取和取消错误
var ErrInvalidDestination = errors.New("invalid destination")

// ErrInvalidAnnotationAppearance 表示所选注解外观结构无效，不包含读取和取消错误
var ErrInvalidAnnotationAppearance = errors.New("invalid annotation appearance stream")

// Annotation 保存注解类型、区域及原始字典，不执行动作
type Annotation struct {
	Reference  Reference
	Subtype    Name
	Rect       Rectangle
	Dictionary Dictionary
}

// PopupAnnotation 保存弹出批注及其父批注引用，Dictionary中的文字信息已按父批注覆盖
type PopupAnnotation struct {
	Annotation
	Parent Reference
	Open   bool
}

// Destination 保存文档内目标，空参数表示保持阅读器当前值
type Destination struct {
	Page       Reference
	Mode       Name
	Parameters Array
}

// IsStandard 判断注解类型是否由PDF标准定义，不表示交互能力已实现
// 返回: bool 标准类型
func (a Annotation) IsStandard() bool {
	switch a.Subtype {
	case "Text", "Link", "FreeText", "Line", "Square", "Circle", "Polygon", "PolyLine", "Highlight", "Underline", "Squiggly", "StrikeOut", "Caret", "Ink", "Stamp", "Popup", "FileAttachment", "Sound", "Movie", "Widget", "Screen", "PrinterMark", "TrapNet", "Watermark", "3D", "Redact", "RichMedia", "Projection":
		return true
	}
	return false
}

// ReadAnnotationFlags 读取非负注解标志，未提供或空值按零处理
// 入参: annotation 注解信息
// 返回: int64 标志位, error 类型或引用错误
func (r *Reader) ReadAnnotationFlags(annotation Annotation) (int64, error) {
	value, err := r.Resolve(annotation.Dictionary["F"])
	if err != nil || value == nil {
		return 0, err
	}
	flags, ok := value.(Integer)
	if !ok || flags < 0 {
		return 0, fmt.Errorf("invalid annotation flags")
	}
	return int64(flags), nil
}

// ReadPopupAnnotation 读取弹出批注，继承父批注的Contents、M、C和T，不执行界面操作
// 入参: object 弹出批注字典或间接引用
// 返回: PopupAnnotation 弹出批注信息, error 解析错误
func (r *Reader) ReadPopupAnnotation(object Object) (PopupAnnotation, error) {
	var popup PopupAnnotation
	value, err := r.Resolve(object)
	if err != nil {
		return popup, err
	}
	dict, ok := value.(Dictionary)
	if !ok {
		return popup, fmt.Errorf("invalid popup annotation")
	}
	subtype, err := r.Resolve(dict["Subtype"])
	if err != nil {
		return popup, err
	}
	if subtype != Name("Popup") {
		return popup, fmt.Errorf("invalid popup annotation")
	}
	popup.Reference, _ = object.(Reference)
	popup.Subtype = "Popup"
	popup.Rect, err = r.annotationRectangle(dict, "Popup")
	if err != nil {
		return popup, err
	}
	popup.Dictionary = maps.Clone(dict)
	if value, err = r.Resolve(dict["Open"]); err != nil {
		return popup, err
	} else if value != nil {
		open, ok := value.(Boolean)
		if !ok {
			return popup, fmt.Errorf("invalid popup open state")
		}
		popup.Open = bool(open)
	}
	if dict["Parent"] != nil {
		parent, ok := dict["Parent"].(Reference)
		if !ok {
			return popup, fmt.Errorf("invalid popup parent reference")
		}
		value, err = r.Resolve(parent)
		if err != nil {
			return popup, err
		}
		if value == nil {
			return popup, nil
		}
		parentDict, ok := value.(Dictionary)
		if !ok {
			return popup, fmt.Errorf("invalid popup parent annotation")
		}
		subtype, err := r.Resolve(parentDict["Subtype"])
		if err != nil {
			return popup, err
		}
		if subtype == Name("Popup") {
			return popup, fmt.Errorf("invalid popup parent annotation")
		}
		if _, ok := subtype.(Name); !ok {
			return popup, fmt.Errorf("invalid popup parent subtype")
		}
		popup.Parent = parent
		for _, key := range []Name{"Contents", "M", "C", "T"} {
			delete(popup.Dictionary, key)
			if value := parentDict[key]; value != nil {
				popup.Dictionary[key] = value
			}
		}
	}
	return popup, nil
}

// Annotations 按页面顺序读取注解，外观和动作由调用方解释
// 返回: []Annotation 注解信息, error 错误信息
func (p *Page) Annotations() ([]Annotation, error) {
	return p.AnnotationsContext(context.Background())
}

// AnnotationsContext 按页面顺序读取注解，取消或解析失败时不返回部分结果
// 入参: ctx 取消上下文
// 返回: []Annotation 注解信息, error 错误信息
func (p *Page) AnnotationsContext(ctx context.Context) ([]Annotation, error) {
	if p == nil || p.reader == nil || ctx == nil {
		return nil, fmt.Errorf("invalid annotation context")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if p.reader.isClosed() {
		return nil, os.ErrClosed
	}
	value, err := p.reader.Resolve(p.Dictionary["Annots"])
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if value == nil {
		return nil, nil
	}
	array, ok := value.(Array)
	if !ok {
		return nil, fmt.Errorf("invalid page annotations")
	}
	result := make([]Annotation, 0, len(array))
	for _, object := range array {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		value, err := p.reader.Resolve(object)
		if err != nil {
			return nil, err
		}
		dict, ok := value.(Dictionary)
		if !ok {
			return nil, fmt.Errorf("invalid annotation dictionary")
		}
		value, err = p.reader.Resolve(dict["Subtype"])
		if err != nil {
			return nil, err
		}
		subtype, ok := value.(Name)
		if !ok {
			return nil, fmt.Errorf("missing annotation subtype")
		}
		box, err := p.reader.annotationRectangle(dict, subtype)
		if err != nil {
			return nil, err
		}
		reference, _ := object.(Reference)
		result = append(result, Annotation{Reference: reference, Subtype: subtype, Rect: box, Dictionary: dict})
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return result, nil
}

// annotationRectangle 读取注解矩形，诊断模式保留缺少区域的非可视弹窗
// 入参: dict 原始注解字典, subtype 已解析的注解类型
// 返回: Rectangle 注解区域，缺失弹窗区域时为空, error 矩形或引用错误
func (r *Reader) annotationRectangle(dict Dictionary, subtype Name) (Rectangle, error) {
	value, err := r.Resolve(dict["Rect"])
	if err != nil {
		return Rectangle{}, err
	}
	if value == nil && subtype == "Popup" && r.warning != nil {
		r.warning(Diagnostic{Message: "PDF Popup annotation has no Rect; nonvisual popup data retained without a display region"})
		return Rectangle{}, nil
	}
	return r.rectangle(value)
}

// ReadAnnotationAppearance 读取已有的正常外观，按AS选择状态，不生成或解码外观
// 入参: annotation 注解
// 返回: *Stream 表单外观，未提供或状态不存在时为空, error 类型或引用错误
func (r *Reader) ReadAnnotationAppearance(annotation Annotation) (*Stream, error) {
	value, err := r.Resolve(annotation.Dictionary["AP"])
	if err != nil || value == nil {
		return nil, err
	}
	appearance, ok := value.(Dictionary)
	if !ok {
		return nil, fmt.Errorf("%w: dictionary", ErrInvalidAnnotationAppearance)
	}
	stream, err := r.annotationAppearanceStream(appearance, annotation.Dictionary["AS"])
	if err != nil || stream == nil {
		return nil, err
	}
	subtype, err := r.Resolve(stream.Dictionary["Subtype"])
	if err != nil {
		return nil, err
	}
	if subtype != Name("Form") {
		return nil, fmt.Errorf("%w: subtype", ErrInvalidAnnotationAppearance)
	}
	return stream, nil
}

// annotationAppearanceStream 从正常外观字典选择状态，不解码流内容
// 入参: appearance 外观字典, selected 当前状态
// 返回: *Stream 已选流，状态不存在时为空, error 类型或引用错误
func (r *Reader) annotationAppearanceStream(appearance Dictionary, selected Object) (*Stream, error) {
	value, err := r.Resolve(appearance["N"])
	if err != nil {
		return nil, err
	}
	if states, ok := value.(Dictionary); ok {
		selected, err := r.Resolve(selected)
		if err != nil {
			return nil, err
		}
		state, ok := selected.(Name)
		if !ok {
			return nil, fmt.Errorf("%w: missing state", ErrInvalidAnnotationAppearance)
		}
		value, err = r.Resolve(states[state])
		if err != nil || value == nil {
			return nil, err
		}
	}
	stream, ok := value.(*Stream)
	if !ok || stream == nil {
		return nil, ErrInvalidAnnotationAppearance
	}
	return stream, nil
}

// annotationAppearanceTransform 校验外观流边界与矩阵并计算映射到注解区域的变换与包围盒
// 入参: stream 外观流, rect 注解区域, noRotate 是否应用页面逆向旋转补偿, rotation 页面旋转角度
// 返回: Matrix 页面映射变换, Rectangle 变换后包围盒, error 读取或无效外观错误
func (r *Reader) annotationAppearanceTransform(stream *Stream, rect Rectangle, noRotate bool, rotation int) (Matrix, Rectangle, error) {
	if rect.XMin > rect.XMax {
		rect.XMin, rect.XMax = rect.XMax, rect.XMin
	}
	if rect.YMin > rect.YMax {
		rect.YMin, rect.YMax = rect.YMax, rect.YMin
	}
	bboxValue, err := r.Resolve(stream.Dictionary["BBox"])
	if err != nil {
		return Matrix{}, Rectangle{}, err
	}
	box, err := r.rectangle(bboxValue)
	if err != nil {
		if isFatalReaderError(err) {
			return Matrix{}, Rectangle{}, err
		}
		return Matrix{}, Rectangle{}, fmt.Errorf("%w: invalid appearance bounds", ErrInvalidAnnotationAppearance)
	}
	matrix := Identity()
	value, err := r.Resolve(stream.Dictionary["Matrix"])
	if err != nil {
		return Matrix{}, Rectangle{}, err
	}
	if value != nil {
		values, err := r.numberArray(value, 6)
		if err != nil {
			if isFatalReaderError(err) {
				return Matrix{}, Rectangle{}, err
			}
			return Matrix{}, Rectangle{}, fmt.Errorf("%w: invalid appearance matrix", ErrInvalidAnnotationAppearance)
		}
		matrix = Matrix(values)
	}
	minX, minY, maxX, maxY := math.Inf(1), math.Inf(1), math.Inf(-1), math.Inf(-1)
	for _, point := range []Point{{box.XMin, box.YMin}, {box.XMax, box.YMin}, {box.XMax, box.YMax}, {box.XMin, box.YMax}} {
		point = matrix.Apply(point)
		minX, minY = math.Min(minX, point.X), math.Min(minY, point.Y)
		maxX, maxY = math.Max(maxX, point.X), math.Max(maxY, point.Y)
	}
	if math.IsInf(minX, 0) || math.IsInf(minY, 0) || math.IsInf(maxX, 0) || math.IsInf(maxY, 0) || math.IsNaN(minX) || math.IsNaN(minY) || math.IsNaN(maxX) || math.IsNaN(maxY) || minX == maxX || minY == maxY {
		return Matrix{}, Rectangle{}, fmt.Errorf("%w: invalid appearance bounds", ErrInvalidAnnotationAppearance)
	}
	target := rect
	bounds := rect
	var compensate Matrix
	rotated := false
	if noRotate {
		w, h := rect.XMax-rect.XMin, rect.YMax-rect.YMin
		switch (rotation%360 + 360) % 360 {
		case 90:
			target = Rectangle{0, 0, w, h}
			compensate = Matrix{0, 1, -1, 0, rect.XMin + h, rect.YMax}
			bounds = Rectangle{rect.XMin, rect.YMax, rect.XMin + h, rect.YMax + w}
			rotated = true
		case 180:
			target = Rectangle{0, 0, w, h}
			compensate = Matrix{-1, 0, 0, -1, rect.XMin, rect.YMax + h}
			bounds = Rectangle{rect.XMin - w, rect.YMax, rect.XMin, rect.YMax + h}
			rotated = true
		case 270:
			target = Rectangle{0, 0, w, h}
			compensate = Matrix{0, -1, 1, 0, rect.XMin - h, rect.YMax}
			bounds = Rectangle{rect.XMin - h, rect.YMax - w, rect.XMin, rect.YMax}
			rotated = true
		}
	}
	scaleX, scaleY := (target.XMax-target.XMin)/(maxX-minX), (target.YMax-target.YMin)/(maxY-minY)
	tx, ty := target.XMin-minX*scaleX, target.YMin-minY*scaleY
	if math.IsInf(scaleX, 0) || math.IsNaN(scaleX) || scaleX == 0 || math.IsInf(scaleY, 0) || math.IsNaN(scaleY) || scaleY == 0 || math.IsInf(tx, 0) || math.IsNaN(tx) || math.IsInf(ty, 0) || math.IsNaN(ty) {
		return Matrix{}, Rectangle{}, fmt.Errorf("%w: invalid appearance transform", ErrInvalidAnnotationAppearance)
	}
	transform := Matrix{scaleX, 0, 0, scaleY, tx, ty}
	if rotated {
		transform = compensate.Mul(transform)
		if !matrixFinite(transform) || !rectFinite(bounds) {
			return Matrix{}, Rectangle{}, fmt.Errorf("%w: invalid appearance transform", ErrInvalidAnnotationAppearance)
		}
	}
	return transform, bounds, nil
}

// WalkAnnotationAppearance 解释注解当前外观并映射到页面用户空间
// Warning非空时按控件属性恢复结构无效的外观，保留原始字典并报告原因
// 入参: ctx 取消上下文, page 所在页面, annotation 注解, visitor 图元访问器
// 返回: error 外观缺失、解析或访问错误
func (r *Reader) WalkAnnotationAppearance(ctx context.Context, page *Page, annotation Annotation, visitor Visitor) error {
	return r.walkAnnotationAppearance(ctx, page, annotation, visitor, nil)
}

// walkAnnotationAppearance 在页面或引用表单坐标中解释注解外观
// 入参: ctx 取消上下文, page 所在页面, annotation 注解, visitor 图元访问器, parent 引用表单状态，可为空
// 返回: error 外观或访问错误
func (r *Reader) walkAnnotationAppearance(ctx context.Context, page *Page, annotation Annotation, visitor Visitor, parent *pageInterpreter) error {
	if r == nil || ctx == nil {
		return fmt.Errorf("invalid annotation context")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if r.isClosed() {
		return os.ErrClosed
	}
	if page == nil || page.reader != r {
		return fmt.Errorf("page belongs to another reader")
	}
	flags, err := r.ReadAnnotationFlags(annotation)
	if err != nil {
		return err
	}
	if !annotation.IsStandard() && flags&1 != 0 {
		return ctx.Err()
	}
	optional, err := r.Resolve(annotation.Dictionary["OC"])
	if err != nil {
		return err
	}
	if optional != nil {
		object := annotation.Dictionary["OC"]
		var show bool
		if visitor.OptionalContent != nil {
			show, err = visitor.OptionalContent(object)
		} else {
			show, err = r.optionalContentVisibleContext(ctx, object)
		}
		if err != nil || !show {
			return err
		}
	}
	if annotation.Rect.XMin == annotation.Rect.XMax || annotation.Rect.YMin == annotation.Rect.YMax {
		return ctx.Err()
	}
	if annotation.Subtype == "Popup" {
		_, err := r.ReadPopupAnnotation(annotation.Dictionary)
		return err
	}
	if annotation.Subtype == "Projection" {
		if _, err := r.ReadProjectionAnnotation(annotation); err != nil {
			return err
		}
		appearance, err := r.ReadAnnotationAppearance(annotation)
		if err != nil || appearance == nil {
			return err
		}
	}
	value, err := r.Resolve(annotation.Dictionary["AP"])
	if err != nil {
		return err
	}
	if value == nil && !annotation.IsStandard() {
		return ctx.Err()
	}
	if value == nil {
		var stream *Stream
		var synthErr error
		synthesized := true
		switch annotation.Subtype {
		case "Stamp":
			stream, synthErr = r.stampAppearance(annotation)
		case "Widget":
			stream, synthErr = r.widgetAppearance(ctx, page, annotation)
		case "FreeText":
			var text string
			text, synthErr = r.readAnnotationText(ctx, annotation, "Contents", visitor.Warning)
			if synthErr == nil {
				annotation.Dictionary = maps.Clone(annotation.Dictionary)
				annotation.Dictionary["Contents"] = String("\xef\xbb\xbf" + text)
				stream, synthErr = r.variableTextAppearance(ctx, page, annotation)
			}
		case "Link":
			stream, synthErr = r.linkAppearanceContext(ctx, annotation)
		case "Text", "FileAttachment", "Sound", "Caret":
			stream, synthErr = r.iconAppearance(annotation)
		case "Ink":
			stream, synthErr = r.inkAppearance(ctx, annotation)
		case "Line":
			stream, synthErr = r.lineAppearance(ctx, page, annotation, visitor.Warning)
		case "Redact":
			stream, synthErr = r.redactionAppearance(ctx, annotation)
		case "Highlight", "Underline", "StrikeOut", "Squiggly":
			stream, synthErr = r.textMarkupAppearance(ctx, annotation)
		case "Square", "Circle", "Polygon", "PolyLine":
			stream, synthErr = r.shapeAppearanceContext(ctx, annotation)
		case "Screen":
			return ctx.Err()
		case "Movie":
			var movie Movie
			movie, synthErr = r.ReadMovie(annotation.Dictionary["Movie"])
			if synthErr == nil {
				if movie.Poster == Boolean(true) {
					synthErr = &UnsupportedError{Feature: "poster extraction from movie"}
				} else {
					poster, ok := movie.Poster.(*Stream)
					if !ok || poster == nil {
						return ctx.Err()
					}
					stream = &Stream{Dictionary: Dictionary{"Subtype": Name("Form"), "BBox": Array{Integer(0), Integer(0), Integer(1), Integer(1)}, "Resources": Dictionary{"XObject": Dictionary{"Poster": poster}}}, Data: []byte("/Poster Do"), reader: r, cache: r.cache, hasWarning: r.warning != nil}
				}
			}
		default:
			synthesized = false
		}
		if synthErr != nil {
			if err := ctx.Err(); err != nil {
				return err
			}
			if visitor.Warning == nil || isFatalReaderError(synthErr) {
				return synthErr
			}
			visitor.Warning(Diagnostic{Message: fmt.Sprintf("PDF %s annotation appearance synthesis failed: %v; omitted", annotation.Subtype, synthErr)})
			return ctx.Err()
		}
		if synthesized {
			value = Dictionary{"N": stream}
		}
	}
	appearance, ok := value.(Dictionary)
	if !ok {
		if visitor.Warning != nil {
			visitor.Warning(Diagnostic{Message: "PDF annotation has no usable static appearance"})
			return ctx.Err()
		}
		return &UnsupportedError{Feature: "annotation appearance"}
	}
	noRotate := parent == nil && page.Rotation != 0 && (flags&16 != 0 || annotation.Subtype == "Text")
	var transform Matrix
	var bounds Rectangle
	stream, err := r.annotationAppearanceStream(appearance, annotation.Dictionary["AS"])
	if err == nil && stream != nil {
		transform, bounds, err = r.annotationAppearanceTransform(stream, annotation.Rect, noRotate, page.Rotation)
	}
	if errors.Is(err, ErrInvalidAnnotationAppearance) && annotation.Subtype == "Widget" && visitor.Warning != nil {
		recovered, synthErr := r.widgetAppearance(ctx, page, annotation)
		if synthErr != nil {
			if cancelErr := ctx.Err(); cancelErr != nil {
				return cancelErr
			}
			if isFatalReaderError(synthErr) {
				return synthErr
			}
			visitor.Warning(Diagnostic{Message: "PDF Widget annotation appearance synthesis failed: " + synthErr.Error() + "; omitted"})
			return ctx.Err()
		}
		if recovered != nil {
			transform, bounds, err = r.annotationAppearanceTransform(recovered, annotation.Rect, noRotate, page.Rotation)
			if err == nil {
				stream = recovered
				visitor.Warning(Diagnostic{Message: "invalid PDF widget appearance replaced from field properties"})
			}
		}
	}
	if errors.Is(err, ErrInvalidAnnotationAppearance) && visitor.Warning != nil {
		visitor.Warning(Diagnostic{Message: "PDF annotation has no usable static appearance: " + err.Error()})
		return ctx.Err()
	}
	if err != nil || stream == nil {
		return err
	}
	subtype, err := r.Resolve(stream.Dictionary["Subtype"])
	if err != nil {
		return err
	}
	if subtype != Name("Form") {
		if visitor.Warning == nil {
			return &UnsupportedError{Feature: "annotation appearance stream"}
		}
		visitor.Warning(Diagnostic{Message: "PDF annotation appearance has invalid Form subtype; form appearance retained"})
	}
	source := stream
	if visitor.Group != nil {
		group, err := r.Resolve(stream.Dictionary["Group"])
		if err != nil {
			return err
		}
		if group == nil {
			copy := *stream
			copy.Dictionary = maps.Clone(stream.Dictionary)
			copy.Dictionary["Group"] = Dictionary{"S": Name("Transparency")}
			stream = &copy
		}
	}
	interpreter := pageInterpreter{reader: r, page: page, resources: page.Resources, pageResources: page.Resources, visitor: visitor, ctx: ctx, textMatrix: Identity(), lineMatrix: Identity(), bounds: bounds}
	interpreter.patternMatrix = Identity()
	interpreter.state = graphicsState{matrix: transform, hscale: 1, fillSpace: "DeviceGray", strokeSpace: "DeviceGray", style: Style{Fill: Paint{SourceSpace: "DeviceGray", Alpha: 1}, Stroke: Paint{SourceSpace: "DeviceGray", Alpha: 1}, LineWidth: 1, MiterLimit: 10, BlendMode: "Normal"}}
	if err := interpreter.initFormDefaultColors(stream); err != nil {
		return err
	}
	if parent != nil {
		interpreter.sequence = parent.sequence
		interpreter.state.matrix = parent.state.matrix.Mul(interpreter.state.matrix)
		interpreter.state.style.Clips = append([]Path(nil), parent.state.style.Clips...)
		interpreter.bounds = parent.bounds
		interpreter.depth = parent.depth
		interpreter.patternMatrix = parent.patternMatrix
	}
	if annotation.Reference != (Reference{}) && annotation.Dictionary["StructParent"] != nil {
		interpreter.sequence = &ContentSequence{Reader: r, Page: page, Stream: source, Object: annotation.Reference, Parent: interpreter.sequence}
	}
	return interpreter.formContent(stream, source)
}

// ReadField 读取控件字段并补齐可继承属性，不修改原始注解字典
// 入参: annotation 表单控件
// 返回: Dictionary 字段属性, error 字段或继承链错误
func (r *Reader) ReadField(annotation Annotation) (Dictionary, error) {
	if r == nil {
		return nil, fmt.Errorf("invalid reader")
	}
	if r.isClosed() {
		return nil, os.ErrClosed
	}
	if annotation.Subtype != "Widget" {
		return nil, fmt.Errorf("annotation is not a widget")
	}
	result := make(Dictionary, len(annotation.Dictionary))
	for key, value := range annotation.Dictionary {
		result[key] = value
	}
	inherited := []Name{"FT", "Ff", "V", "DV", "DA", "Q", "MaxLen", "RV", "DS", "Opt", "I", "TI"}
	for _, key := range inherited {
		delete(result, key)
	}
	seen := map[Reference]bool{}
	current := annotation.Dictionary
	for current != nil {
		for _, key := range inherited {
			if result[key] == nil {
				value, err := r.Resolve(current[key])
				if err != nil {
					return nil, err
				}
				if value != nil {
					result[key] = value
				}
			}
		}
		parent := current["Parent"]
		if parent == nil {
			break
		}
		ref, ok := parent.(Reference)
		if !ok || seen[ref] {
			return nil, fmt.Errorf("invalid field parent chain")
		}
		seen[ref] = true
		value, err := r.Resolve(parent)
		if err != nil {
			return nil, err
		}
		if value == nil {
			break
		}
		current, ok = value.(Dictionary)
		if !ok {
			return nil, fmt.Errorf("invalid field parent")
		}
	}
	defaults, err := r.formDefaults()
	if err != nil {
		return nil, err
	}
	flags, _ := result["Ff"].(Integer)
	if result["FT"] != Name("Btn") || flags&(1<<16) != 0 {
		for _, key := range []Name{"DA", "Q"} {
			if result[key] == nil {
				result[key], err = r.Resolve(defaults[key])
				if err != nil {
					return nil, err
				}
			}
		}
	}
	return result, nil
}

// BaseURI 读取文档声明的链接基准地址，不访问外部资源
// 返回: string 基准地址，未声明时为空, error 错误信息
func (r *Reader) BaseURI() (string, error) {
	if r == nil {
		return "", fmt.Errorf("invalid reader")
	}
	if r.isClosed() {
		return "", os.ErrClosed
	}
	root, err := r.Resolve(r.Trailer["Root"])
	if err != nil {
		return "", err
	}
	catalog, ok := root.(Dictionary)
	if !ok {
		return "", fmt.Errorf("invalid catalog")
	}
	value, err := r.Resolve(catalog["URI"])
	if err != nil || value == nil {
		return "", err
	}
	dict, ok := value.(Dictionary)
	if !ok {
		return "", fmt.Errorf("invalid URI dictionary")
	}
	value, err = r.Resolve(dict["Base"])
	if err != nil || value == nil {
		return "", err
	}
	base, ok := value.(String)
	if !ok {
		return "", fmt.Errorf("invalid base URI")
	}
	return string(base), nil
}

// ReadDestination 解析直接目标或名称树中的命名目标，保留名称对象与名称树的区别，不跳转页面
// 入参: ctx 取消上下文, object 目标数组、名称、字符串或引用
// 返回: Destination 跳转目标, error 错误信息
func (r *Reader) ReadDestination(ctx context.Context, object Object) (Destination, error) {
	if ctx == nil {
		return Destination{}, fmt.Errorf("invalid destination context")
	}
	if err := ctx.Err(); err != nil {
		return Destination{}, err
	}
	value, err := r.Resolve(object)
	if err != nil {
		return Destination{}, err
	}
	var name string
	named, legacy := false, false
	switch v := value.(type) {
	case Name:
		name = string(v)
		named, legacy = true, true
	case String:
		name = string(v)
		named = true
	}
	if named {
		if err := r.readDestinations(ctx); err != nil {
			return Destination{}, err
		}
		target := r.destinations[name]
		if legacy {
			target = r.legacyDestinations[Name(name)]
		}
		if target == nil {
			return Destination{}, fmt.Errorf("%w: %q", ErrDestinationNotFound, name)
		}
		value, err = r.Resolve(target)
		if err != nil {
			return Destination{}, err
		}
	}
	if dict, ok := value.(Dictionary); ok {
		value, err = r.Resolve(dict["D"])
		if err != nil {
			return Destination{}, err
		}
	}
	array, ok := value.(Array)
	if !ok || len(array) < 2 {
		return Destination{}, fmt.Errorf("%w: missing array", ErrInvalidDestination)
	}
	if _, ok := array[0].(Reference); !ok {
		return Destination{}, fmt.Errorf("%w: page reference", ErrInvalidDestination)
	}
	page, err := r.resolveIndirectReference(array[0])
	if err != nil {
		return Destination{}, err
	}
	mode, parameters, err := r.destinationParameters(array)
	if err != nil {
		return Destination{}, err
	}
	return Destination{Page: page, Mode: mode, Parameters: parameters}, nil
}

// destinationParameters 解析显式目标的显示模式与可空参数，不解释首项页标识
// 入参: array 完整目标数组
// 返回: Name 显示模式, Array 已解析参数, error 模式或参数错误
func (r *Reader) destinationParameters(array Array) (Name, Array, error) {
	modeValue, err := r.Resolve(array[1])
	if err != nil {
		return "", nil, err
	}
	mode, ok := modeValue.(Name)
	count, known := destinationParameterCount(mode)
	if !ok || !known || len(array) != count+2 {
		return "", nil, fmt.Errorf("%w: mode or parameter count", ErrInvalidDestination)
	}
	parameters := make(Array, count)
	var coordinates [4]float64
	for n, v := range array[2:] {
		parameters[n], err = r.Resolve(v)
		if err != nil {
			return "", nil, err
		}
		if parameters[n] == nil && mode == "FitR" {
			return "", nil, fmt.Errorf("%w: null rectangle parameter", ErrInvalidDestination)
		}
		if parameters[n] != nil {
			var value float64
			switch number := parameters[n].(type) {
			case Integer:
				value = float64(number)
			case Real:
				value = float64(number)
			default:
				return "", nil, fmt.Errorf("%w: numeric parameter", ErrInvalidDestination)
			}
			if math.IsNaN(value) || math.IsInf(value, 0) || mode == "XYZ" && n == 2 && value < 0 {
				return "", nil, fmt.Errorf("%w: parameter range", ErrInvalidDestination)
			}
			coordinates[n] = value
		}
	}
	if mode == "FitR" && (coordinates[0] == coordinates[2] || coordinates[1] == coordinates[3]) {
		return "", nil, fmt.Errorf("%w: empty rectangle", ErrInvalidDestination)
	}
	return mode, parameters, nil
}

// readDestinations 缓存旧式目标字典和名称树，检测递归引用
// 入参: ctx 取消上下文
// 返回: error 错误信息
func (r *Reader) readDestinations(ctx context.Context) error {
	if r.destinations != nil {
		return nil
	}
	root, err := r.Resolve(r.Trailer["Root"])
	if err != nil {
		return err
	}
	catalog, ok := root.(Dictionary)
	if !ok {
		return fmt.Errorf("invalid catalog")
	}
	result := map[string]Object{}
	var legacy Dictionary
	value, err := r.Resolve(catalog["Dests"])
	if err != nil {
		return err
	}
	if value != nil {
		dict, ok := value.(Dictionary)
		if !ok {
			return fmt.Errorf("invalid destinations dictionary")
		}
		legacy = dict
	}
	names, err := r.Resolve(catalog["Names"])
	if err != nil {
		return err
	}
	if names != nil {
		dict, ok := names.(Dictionary)
		if !ok {
			return fmt.Errorf("invalid names dictionary")
		}
		if err := r.WalkNameTree(ctx, dict["Dests"], func(name string, value Object) error {
			result[name] = value
			return nil
		}); err != nil {
			return err
		}
	}
	r.destinations = result
	r.legacyDestinations = legacy
	return nil
}
