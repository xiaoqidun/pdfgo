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
	"encoding/binary"
	"fmt"
	"math"
	"sync"
)

// iccD50ToLinearSRGB 保存D50白点经Bradford适应到D65并转线性sRGB的合并矩阵
var iccD50ToLinearSRGB = [9]float64{
	3.133782735081906, -1.616878091008565, -0.490623328209115,
	-0.978761812801972, 1.916136685826815, 0.033441008074734,
	0.071966941276779, -0.229027051584764, 1.405790510443146,
}

// iccSRGBEncodeTable 延迟初始化线性sRGB到sRGB编码查找表，避免逐像素幂运算
var iccSRGBEncodeTable = sync.OnceValue(func() *[8193]float32 {
	table := new([8193]float32)
	for i := range table {
		v := float64(i) / 8192
		if v <= 0.0031308 {
			table[i] = float32(12.92 * v)
		} else {
			table[i] = float32(1.055*math.Pow(v, 1/2.4) - 0.055)
		}
	}
	return table
})

// iccLUT 保存ICC输入曲线、多维查找表及输出曲线
type iccLUT struct {
	input, output  [][]float64
	inputIdentity  bool
	outputIdentity bool
	grid           int
	values         []float64
	matrix         [9]float64
	precision      int
	pipeline       *iccPipeline
}

// iccLUTSpace 保存按渲染意图划分的设备与连接空间变换
type iccLUTSpace struct {
	components int
	pcs        string
	toPCS      [3]*iccLUT
	fromPCS    [3]*iccLUT
}

// maxICCLUTBytes 限制单个ICC配置展开多维查找表与管线浮点网格的累计内存上限
const maxICCLUTBytes int64 = 64 << 20

// iccReserveBytes 从ICC查找表预算中扣减指定字节数
// 入参: budget 剩余字节预算指针, size 新增字节数
// 返回: error 超出预算时返回包装ErrStreamLimitExceeded的错误
func iccReserveBytes(budget *int64, size int64) error {
	if size < 0 || size > *budget {
		return fmt.Errorf("%w: ICC lookup table memory limit exceeded", ErrStreamLimitExceeded)
	}
	*budget -= size
	return nil
}

// iccReserveLUTBytes 从ICC查找表预算中扣减指定数量float64元素的内存字节
// 入参: budget 剩余字节预算指针, elements 新增float64元素数
// 返回: error 超出预算时返回包装ErrStreamLimitExceeded的错误
func iccReserveLUTBytes(budget *int64, elements int64) error {
	if elements < 0 || elements > math.MaxInt64/8 {
		return fmt.Errorf("%w: ICC lookup table memory limit exceeded", ErrStreamLimitExceeded)
	}
	return iccReserveBytes(budget, elements*8)
}

// parseICCLUTSpace 读取ICC源空间的正向查找表，共享标签去重并受累计内存预算约束
// 入参: data 配置文件数据, tags 已校验的标签, budget 剩余查找表字节预算指针
// 返回: *iccLUTSpace 颜色变换, error 格式、能力或预算错误
func parseICCLUTSpace(data []byte, tags map[string][]byte, budget *int64) (*iccLUTSpace, error) {
	s := &iccLUTSpace{pcs: string(data[20:24])}
	switch string(data[16:20]) {
	case "GRAY":
		s.components = 1
	case "RGB ", "Lab ":
		s.components = 3
	case "CMYK":
		s.components = 4
	default:
		return nil, &UnsupportedError{Feature: "ICC lookup table color model"}
	}
	for i := range s.toPCS {
		name := fmt.Sprintf("A2B%d", i)
		if tag := tags[name]; tag != nil {
			for j := 0; j < i; j++ {
				if sameICCTag(tag, tags[fmt.Sprintf("A2B%d", j)]) {
					s.toPCS[i] = s.toPCS[j]
					break
				}
			}
			if s.toPCS[i] != nil {
				continue
			}
			lut, err := iccLookupTransform(tag, s.components, 3, s.pcs, false, budget)
			if err != nil {
				return nil, fmt.Errorf("ICC %s: %w", name, err)
			}
			s.toPCS[i] = lut
		}
	}
	if s.toPCS[0] == nil {
		return nil, fmt.Errorf("missing ICC A2B0 transform")
	}
	return s, nil
}

// iccLookupTransform 在给定字节预算内按变换方向校验查找表通道、PCS编码及矩阵
// 入参: data 标签数据, input 输入通道, output 输出通道, pcs 连接空间, inverse 是否逆向, budget 剩余字节预算指针
// 返回: *iccLUT 查找表, error 无效变换或预算错误
func iccLookupTransform(data []byte, input, output int, pcs string, inverse bool, budget *int64) (*iccLUT, error) {
	if len(data) >= 4 && (string(data[:4]) == "mAB " && inverse || string(data[:4]) == "mBA " && !inverse) {
		return nil, fmt.Errorf("invalid ICC lookup direction")
	}
	lut, err := parseICCLUT(data, budget)
	if err != nil {
		return nil, err
	}
	if len(lut.input) != input || len(lut.output) != output {
		return nil, fmt.Errorf("invalid ICC lookup channels")
	}
	if pcs == "XYZ " && lut.precision == 8 {
		return nil, &UnsupportedError{Feature: "8-bit ICC XYZ encoding"}
	}
	if lut.pipeline == nil && (!inverse || pcs != "XYZ ") && lut.matrix != [9]float64{1, 0, 0, 0, 1, 0, 0, 0, 1} {
		return nil, fmt.Errorf("invalid ICC lookup matrix")
	}
	return lut, nil
}

// parseICCLUT 解析ICC查找表及多阶段变换，按实际数据长度校验维数并约束展开内存
// 入参: data 标签数据, budget 剩余字节预算指针
// 返回: *iccLUT 查找表, error 格式或预算错误
func parseICCLUT(data []byte, budget *int64) (*iccLUT, error) {
	if len(data) >= 4 && (string(data[:4]) == "mAB " || string(data[:4]) == "mBA ") {
		return parseICCPipeline(data, budget)
	}
	if len(data) < 48 {
		return nil, fmt.Errorf("invalid ICC lookup table header")
	}
	s := &iccLUT{grid: int(data[10]), precision: 8}
	width, offset, input, output := 1, 48, 256, 256
	switch string(data[:4]) {
	case "mft1":
	case "mft2":
		if len(data) < 52 {
			return nil, fmt.Errorf("invalid ICC lookup table header")
		}
		width, offset, s.precision = 2, 52, 16
		input, output = int(binary.BigEndian.Uint16(data[48:])), int(binary.BigEndian.Uint16(data[50:]))
		if input < 2 || input > 4096 || output < 2 || output > 4096 {
			return nil, fmt.Errorf("invalid ICC lookup curve size")
		}
	default:
		return nil, &UnsupportedError{Feature: "ICC lookup table type " + string(data[:4])}
	}
	in, out := int(data[8]), int(data[9])
	if in < 1 || in > 4 || out < 1 || out > 4 || s.grid < 2 {
		return nil, fmt.Errorf("invalid ICC lookup dimensions")
	}
	count := out
	for i := 0; i < in; i++ {
		if count > (len(data)-offset)/width/s.grid {
			return nil, fmt.Errorf("truncated ICC lookup grid")
		}
		count *= s.grid
	}
	if input*in+count+output*out > (len(data)-offset)/width {
		return nil, fmt.Errorf("truncated ICC lookup table")
	}
	if err := iccReserveLUTBytes(budget, int64(input)*int64(in)+int64(count)+int64(output)*int64(out)); err != nil {
		return nil, err
	}
	for i := range s.matrix {
		s.matrix[i] = float64(int32(binary.BigEndian.Uint32(data[12+4*i:]))) / 65536
	}
	read := func(count int) []float64 {
		values := make([]float64, count)
		for i := range values {
			if width == 1 {
				values[i] = float64(data[offset]) / 255
			} else {
				values[i] = float64(binary.BigEndian.Uint16(data[offset:])) / 65535
			}
			offset += width
		}
		return values
	}
	s.input = make([][]float64, in)
	s.inputIdentity = true
	for i := range s.input {
		s.input[i] = read(input)
		if !iccIdentityCurve(s.input[i]) {
			s.inputIdentity = false
		}
	}
	s.values = read(count)
	s.output = make([][]float64, out)
	s.outputIdentity = true
	for i := range s.output {
		s.output[i] = read(output)
		if !iccIdentityCurve(s.output[i]) {
			s.outputIdentity = false
		}
	}
	return s, nil
}

// iccIdentityCurve 判断查找表曲线是否为恒等映射
// 入参: curve 查找表曲线
// 返回: bool 是否恒等
func iccIdentityCurve(curve []float64) bool {
	return len(curve) == 2 && curve[0] == 0 && curve[1] == 1
}

// iccLookupCurve 线性插值输入或输出表，允许非单调曲线
// 入参: curve 查找表, x 单位输入
// 返回: float64 插值结果
func iccLookupCurve(curve []float64, x float64) float64 {
	if !(x > 0) {
		return curve[0]
	}
	if x >= 1 {
		return curve[len(curve)-1]
	}
	x *= float64(len(curve) - 1)
	i := min(int(x), len(curve)-2)
	return curve[i] + (x-float64(i))*(curve[i+1]-curve[i])
}

// iccIntentIndex 选择ICC查找表的渲染意图
// 入参: intent PDF渲染意图
// 返回: int 标签序号, error 未支持的意图
func iccIntentIndex(intent Name) (int, error) {
	switch intent {
	case "", "RelativeColorimetric":
		return 1, nil
	case "Perceptual":
		return 0, nil
	case "Saturation":
		return 2, nil
	default:
		return 0, &UnsupportedError{Feature: "ICC rendering intent " + string(intent)}
	}
}

// iccXYZRGB 将D50色度适应到D65并编码为sRGB
// 入参: xyz D50色度
// 返回: [3]float64 单位sRGB分量
func iccXYZRGB(xyz [3]float64) [3]float64 {
	m := iccD50ToLinearSRGB
	table := iccSRGBEncodeTable()
	return [3]float64{
		iccSRGBEncode(table, m[0]*xyz[0]+m[1]*xyz[1]+m[2]*xyz[2]),
		iccSRGBEncode(table, m[3]*xyz[0]+m[4]*xyz[1]+m[5]*xyz[2]),
		iccSRGBEncode(table, m[6]*xyz[0]+m[7]*xyz[1]+m[8]*xyz[2]),
	}
}

// iccSRGBEncode 通过预计算表线性插值将线性分量编码为sRGB
// 入参: table 编码查找表, v 线性分量
// 返回: float64 单位sRGB分量
func iccSRGBEncode(table *[8193]float32, v float64) float64 {
	if !(v > 0) {
		return 0
	}
	if v >= 1 {
		return 1
	}
	if v <= 0.0031308 {
		return 12.92 * v
	}
	x := v * 8192
	i := int(x)
	a := float64(table[i])
	return a + (x-float64(i))*(float64(table[i+1])-a)
}

// evaluate 按矩阵、输入表、多维线性插值和输出表求值
// 入参: values 单位输入分量
// 返回: [4]float64 单位输出分量
func (s *iccLUT) evaluate(values []float64) [4]float64 {
	if s.pipeline != nil {
		return s.pipeline.evaluate(values)
	}
	var input [4]float64
	copy(input[:], values)
	if len(s.input) == 3 {
		v := input
		for i := 0; i < 3; i++ {
			input[i] = s.matrix[3*i]*v[0] + s.matrix[3*i+1]*v[1] + s.matrix[3*i+2]*v[2]
		}
	}
	var index [4]int
	var fraction [4]float64
	scale := float64(s.grid - 1)
	limit := s.grid - 2
	if s.inputIdentity {
		for i := range s.input {
			v := input[i]
			if !(v > 0) {
				v = 0
			} else if v > 1 {
				v = 1
			}
			x := v * scale
			index[i] = min(int(x), limit)
			fraction[i] = x - float64(index[i])
		}
	} else {
		for i, curve := range s.input {
			x := iccLookupCurve(curve, input[i]) * scale
			index[i] = min(int(x), limit)
			fraction[i] = x - float64(index[i])
		}
	}
	var result [4]float64
	if len(s.input) == 4 && len(s.output) == 3 {
		result = s.interpolate4x3(index, fraction)
	} else if len(s.input) == 3 && len(s.output) == 3 {
		result = s.interpolate3x3(index, fraction)
	} else {
		for corner := 0; corner < 1<<len(s.input); corner++ {
			weight, offset := 1.0, 0
			for i := range s.input {
				bit := corner >> i & 1
				offset = offset*s.grid + index[i] + bit
				if bit == 0 {
					weight *= 1 - fraction[i]
				} else {
					weight *= fraction[i]
				}
			}
			for i := range s.output {
				result[i] += weight * s.values[offset*len(s.output)+i]
			}
		}
	}
	if !s.outputIdentity {
		for i, curve := range s.output {
			result[i] = iccLookupCurve(curve, result[i])
		}
	}
	return result
}

// interpolate4x3 展开四进三出查找表的层级线性插值
// 入参: index 各轴网格索引, fraction 各轴插值权重
// 返回: [4]float64 插值输出
func (s *iccLUT) interpolate4x3(index [4]int, fraction [4]float64) [4]float64 {
	v := s.values
	n := s.grid
	sY := n * 3
	sM := n * sY
	sC := n * sM
	dc, dm, dy, dk := fraction[0], fraction[1], fraction[2], fraction[3]
	p := index[0]*sC + index[1]*sM + index[2]*sY + index[3]*3
	p1 := p + sY
	p2 := p + sM
	p3 := p2 + sY
	pC := p + sC
	pC1 := pC + sY
	pC2 := pC + sM
	pC3 := pC2 + sY
	l000, a000, b000 := v[p]+dk*(v[p+3]-v[p]), v[p+1]+dk*(v[p+4]-v[p+1]), v[p+2]+dk*(v[p+5]-v[p+2])
	l001, a001, b001 := v[p1]+dk*(v[p1+3]-v[p1]), v[p1+1]+dk*(v[p1+4]-v[p1+1]), v[p1+2]+dk*(v[p1+5]-v[p1+2])
	l010, a010, b010 := v[p2]+dk*(v[p2+3]-v[p2]), v[p2+1]+dk*(v[p2+4]-v[p2+1]), v[p2+2]+dk*(v[p2+5]-v[p2+2])
	l011, a011, b011 := v[p3]+dk*(v[p3+3]-v[p3]), v[p3+1]+dk*(v[p3+4]-v[p3+1]), v[p3+2]+dk*(v[p3+5]-v[p3+2])
	l100, a100, b100 := v[pC]+dk*(v[pC+3]-v[pC]), v[pC+1]+dk*(v[pC+4]-v[pC+1]), v[pC+2]+dk*(v[pC+5]-v[pC+2])
	l101, a101, b101 := v[pC1]+dk*(v[pC1+3]-v[pC1]), v[pC1+1]+dk*(v[pC1+4]-v[pC1+1]), v[pC1+2]+dk*(v[pC1+5]-v[pC1+2])
	l110, a110, b110 := v[pC2]+dk*(v[pC2+3]-v[pC2]), v[pC2+1]+dk*(v[pC2+4]-v[pC2+1]), v[pC2+2]+dk*(v[pC2+5]-v[pC2+2])
	l111, a111, b111 := v[pC3]+dk*(v[pC3+3]-v[pC3]), v[pC3+1]+dk*(v[pC3+4]-v[pC3+1]), v[pC3+2]+dk*(v[pC3+5]-v[pC3+2])
	l00, a00, b00 := l000+dy*(l001-l000), a000+dy*(a001-a000), b000+dy*(b001-b000)
	l01, a01, b01 := l010+dy*(l011-l010), a010+dy*(a011-a010), b010+dy*(b011-b010)
	l10, a10, b10 := l100+dy*(l101-l100), a100+dy*(a101-a100), b100+dy*(b101-b100)
	l11, a11, b11 := l110+dy*(l111-l110), a110+dy*(a111-a110), b110+dy*(b111-b110)
	l0, a0, b0 := l00+dm*(l01-l00), a00+dm*(a01-a00), b00+dm*(b01-b00)
	l1, a1, b1 := l10+dm*(l11-l10), a10+dm*(a11-a10), b10+dm*(b11-b10)
	return [4]float64{l0 + dc*(l1-l0), a0 + dc*(a1-a0), b0 + dc*(b1-b0)}
}

// interpolate3x3 展开三进三出查找表的层级线性插值
// 入参: index 各轴网格索引, fraction 各轴插值权重
// 返回: [4]float64 插值输出
func (s *iccLUT) interpolate3x3(index [4]int, fraction [4]float64) [4]float64 {
	v := s.values
	n := s.grid
	sY := n * 3
	sX := n * sY
	dx, dy, dz := fraction[0], fraction[1], fraction[2]
	p := index[0]*sX + index[1]*sY + index[2]*3
	p1 := p + sY
	p2 := p + sX
	p3 := p2 + sY
	l00, a00, b00 := v[p]+dz*(v[p+3]-v[p]), v[p+1]+dz*(v[p+4]-v[p+1]), v[p+2]+dz*(v[p+5]-v[p+2])
	l01, a01, b01 := v[p1]+dz*(v[p1+3]-v[p1]), v[p1+1]+dz*(v[p1+4]-v[p1+1]), v[p1+2]+dz*(v[p1+5]-v[p1+2])
	l10, a10, b10 := v[p2]+dz*(v[p2+3]-v[p2]), v[p2+1]+dz*(v[p2+4]-v[p2+1]), v[p2+2]+dz*(v[p2+5]-v[p2+2])
	l11, a11, b11 := v[p3]+dz*(v[p3+3]-v[p3]), v[p3+1]+dz*(v[p3+4]-v[p3+1]), v[p3+2]+dz*(v[p3+5]-v[p3+2])
	l0, a0, b0 := l00+dy*(l01-l00), a00+dy*(a01-a00), b00+dy*(b01-b00)
	l1, a1, b1 := l10+dy*(l11-l10), a10+dy*(a11-a10), b10+dy*(b11-b10)
	return [4]float64{l0 + dx*(l1-l0), a0 + dx*(a1-a0), b0 + dx*(b1-b0)}
}

// xyz 将设备分量变换到D50连接空间，保持色度值而非显示亮度
// 入参: values 设备分量, intent 渲染意图
// 返回: [3]float64 D50色度, error 意图错误
func (s *iccLUTSpace) xyz(values []float64, intent Name) ([3]float64, error) {
	i, err := iccIntentIndex(intent)
	if err != nil {
		return [3]float64{}, err
	}
	lut := s.toPCS[i]
	if lut == nil {
		lut = s.toPCS[0]
	}
	v := lut.evaluate(values)
	if s.pcs == "XYZ " {
		return [3]float64{v[0] * 65535 / 32768, v[1] * 65535 / 32768, v[2] * 65535 / 32768}, nil
	}
	scale := 1.0
	if lut.precision == 16 && lut.pipeline == nil {
		scale = 65535.0 / 65280
	}
	l, a, b := math.Min(100, v[0]*100*scale), v[1]*255*scale-128, v[2]*255*scale-128
	y := (l + 16) / 116
	inverse := func(v float64) float64 {
		if v > 6.0/29 {
			return v * v * v
		}
		return 3 * (6.0 / 29) * (6.0 / 29) * (v - 4.0/29)
	}
	return [3]float64{0.9642 * inverse(y+a/500), inverse(y), 0.8249 * inverse(y-b/200)}, nil
}

// fromXYZ 按目标渲染意图将D50色度编码为PCS并执行逆向查找表
// 入参: xyz D50色度, intent 渲染意图
// 返回: [4]float64 设备分量, error 缺失或未支持的变换
func (s *iccLUTSpace) fromXYZ(xyz [3]float64, intent Name) ([4]float64, error) {
	i, err := iccIntentIndex(intent)
	if err != nil {
		return [4]float64{}, err
	}
	lut := s.fromPCS[i]
	if lut == nil {
		lut = s.fromPCS[0]
	}
	if lut == nil {
		return [4]float64{}, fmt.Errorf("missing ICC B2A0 transform")
	}
	var pcs [3]float64
	if s.pcs == "XYZ " {
		for i := range pcs {
			pcs[i] = xyz[i] * 32768 / 65535
		}
	} else {
		f := func(v float64) float64 {
			if v > (6.0/29)*(6.0/29)*(6.0/29) {
				return math.Cbrt(v)
			}
			return v/(3*(6.0/29)*(6.0/29)) + 4.0/29
		}
		x, y, z := f(xyz[0]/.9642), f(xyz[1]), f(xyz[2]/.8249)
		pcs = [3]float64{(116*y - 16) / 100, (500*(x-y) + 128) / 255, (200*(y-z) + 128) / 255}
		if lut.precision == 16 && lut.pipeline == nil {
			for i := range pcs {
				pcs[i] *= 65280.0 / 65535
			}
		}
	}
	return lut.evaluate(pcs[:]), nil
}
