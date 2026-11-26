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
	"crypto/aes"
	"crypto/cipher"
	"crypto/md5"
	"crypto/rc4"
	"crypto/subtle"
	"encoding/binary"
	"errors"
	"fmt"
)

// ErrPassword 表示需要提供正确的PDF用户或所有者密码
var ErrPassword = errors.New("PDF password required or incorrect")

// passwordPadding 保存PDF标准安全处理器的密码填充字节
var passwordPadding = [32]byte{0x28, 0xbf, 0x4e, 0x5e, 0x4e, 0x75, 0x8a, 0x41, 0x64, 0x00, 0x4e, 0x56, 0xff, 0xfa, 0x01, 0x08, 0x2e, 0x2e, 0x00, 0xb6, 0xd0, 0x68, 0x3e, 0x80, 0x2f, 0x0c, 0xa9, 0xfe, 0x64, 0x53, 0x69, 0x7a}

// EncryptionInfo 保存标准安全处理器版本、用户权限位和所有者认证状态
// Permissions采用ISO32000的P字段位定义，Owner为true时不受用户权限限制
// 阅读器只解析数据，权限控制由调用方实施
type EncryptionInfo struct {
	Revision    int
	Permissions uint32
	Owner       bool
}

// standardSecurity 保存标准安全处理器的文件密钥及加密过滤器
type standardSecurity struct {
	info            EncryptionInfo
	key             []byte
	filters         Dictionary
	streamFilter    Name
	stringFilter    Name
	embeddedFilter  Name
	encryptMetadata bool
	unencrypted     map[Reference]bool
}

// collectUnencryptedRefs 递归收集Trailer中Encrypt与ID可达的全部非流间接引用
// 入参: trailer 文件尾字典, fetch 对象读取函数
// 返回: map[Reference]bool 豁免加密及对象流打包的引用集合, error 读取或嵌套错误
func collectUnencryptedRefs(trailer Dictionary, fetch func(Reference) (Object, error)) (map[Reference]bool, error) {
	if trailer == nil || (trailer["Encrypt"] == nil && trailer["ID"] == nil) {
		return nil, nil
	}
	unencrypted := make(map[Reference]bool)
	visited := make(map[Reference]bool)
	var walk func(Object, int) error
	walk = func(value Object, depth int) error {
		if depth > 256 {
			return fmt.Errorf("excessive PDF object nesting")
		}
		if value == nil {
			return nil
		}
		switch v := value.(type) {
		case Reference:
			if visited[v] {
				return nil
			}
			visited[v] = true
			obj, err := fetch(v)
			if err != nil {
				return err
			}
			if _, isStream := obj.(*Stream); isStream {
				return nil
			}
			unencrypted[v] = true
			return walk(obj, depth+1)
		case Array:
			for _, item := range v {
				if err := walk(item, depth+1); err != nil {
					return err
				}
			}
		case Dictionary:
			for _, item := range v {
				if err := walk(item, depth+1); err != nil {
					return err
				}
			}
		}
		return nil
	}
	if trailer["Encrypt"] != nil {
		if err := walk(trailer["Encrypt"], 0); err != nil {
			return nil, err
		}
	}
	if trailer["ID"] != nil {
		if err := walk(trailer["ID"], 0); err != nil {
			return nil, err
		}
	}
	if len(unencrypted) == 0 {
		return nil, nil
	}
	return unencrypted, nil
}

// padPassword 截断或填充密码为32字节
// 入参: password 密码字节
// 返回: []byte 填充后的密码
func padPassword(password []byte) []byte {
	data := make([]byte, 32)
	n := copy(data, password)
	copy(data[n:], passwordPadding[:])
	return data
}

// Encryption 返回加密信息副本，未加密文档返回nil
// 返回: *EncryptionInfo 加密信息
func (r *Reader) Encryption() *EncryptionInfo {
	if r == nil || r.closed || r.security == nil {
		return nil
	}
	info := r.security.info
	return &info
}

// openSecurity 解析加密字典并认证用户或所有者密码
// 入参: password 用户或所有者密码
// 返回: error 认证或加密参数错误
func (r *Reader) openSecurity(password []byte) error {
	if r.Trailer["Encrypt"] == nil {
		return nil
	}
	cached := r.cache.snapshot()
	unencrypted, err := collectUnencryptedRefs(r.Trailer, r.Object)
	if err != nil {
		r.cache.prune(cached)
		return err
	}
	for ref := range unencrypted {
		cached[ref] = true
	}
	r.cache.prune(cached)
	object, err := r.Resolve(r.Trailer["Encrypt"])
	if err != nil || object == nil {
		return err
	}
	dict, ok := object.(Dictionary)
	if !ok {
		return fmt.Errorf("invalid encryption dictionary")
	}
	filterObj, err := r.Resolve(dict["Filter"])
	if err != nil {
		return err
	}
	if filterObj != Name("Standard") {
		return &UnsupportedError{Feature: "PDF security handler"}
	}
	vObj, err := r.Resolve(dict["V"])
	if err != nil {
		return err
	}
	rObj, err := r.Resolve(dict["R"])
	if err != nil {
		return err
	}
	v, vok := vObj.(Integer)
	revision, rok := rObj.(Integer)
	if vok && rok && v == 5 && (revision == 5 || revision == 6) {
		return r.openAES256Security(dict, password, int(revision), unencrypted)
	}
	if !vok || !rok || !(v == 1 && (revision == 2 || revision == 3) || v == 2 && revision == 3 || v == 4 && revision == 4) {
		return &UnsupportedError{Feature: "PDF encryption revision"}
	}
	s := &standardSecurity{info: EncryptionInfo{Revision: int(revision)}, encryptMetadata: true, streamFilter: "V2", stringFilter: "V2", embeddedFilter: "V2", unencrypted: unencrypted}
	if v == 4 {
		metaObj, err := r.Resolve(dict["EncryptMetadata"])
		if err != nil {
			return err
		}
		if metaObj != nil {
			b, ok := metaObj.(Boolean)
			if !ok {
				return fmt.Errorf("invalid EncryptMetadata")
			}
			s.encryptMetadata = bool(b)
		}
		filters, err := r.securityFilters(dict["CF"])
		if err != nil {
			return err
		}
		s.filters = filters
		for _, entry := range []struct {
			name  Name
			value *Name
		}{{"StmF", &s.streamFilter}, {"StrF", &s.stringFilter}} {
			*entry.value = "Identity"
			value, err := r.Resolve(dict[entry.name])
			if err != nil {
				return err
			}
			if value != nil {
				filterName, ok := value.(Name)
				if !ok {
					return fmt.Errorf("invalid %s crypt filter", entry.name)
				}
				*entry.value = filterName
			}
		}
		s.embeddedFilter = s.streamFilter
		effObj, err := r.Resolve(dict["EFF"])
		if err != nil {
			return err
		}
		if effObj != nil {
			effName, ok := effObj.(Name)
			if !ok {
				return fmt.Errorf("invalid embedded file crypt filter")
			}
			s.embeddedFilter = effName
		}
	}
	lengthObj, err := r.Resolve(dict["Length"])
	if err != nil {
		return err
	}
	length := int64(40)
	if lengthObj != nil {
		n, ok := lengthObj.(Integer)
		if !ok {
			return fmt.Errorf("Length is not an integer")
		}
		length = int64(n)
	} else if v == 4 {
		length = 128
		for _, name := range []Name{s.streamFilter, s.stringFilter, s.embeddedFilter} {
			if name == "Identity" {
				continue
			}
			cf, ok := s.filters[name].(Dictionary)
			if !ok {
				continue
			}
			method, _ := cf["CFM"].(Name)
			if cf["CFM"] == nil || method == "None" {
				continue
			}
			if cfLenObj := cf["Length"]; cfLenObj != nil {
				cfLen, ok := cfLenObj.(Integer)
				if !ok {
					return fmt.Errorf("invalid crypt filter key length")
				}
				if cfLen < 5 || cfLen > 16 {
					return fmt.Errorf("invalid encryption key length")
				}
				length = int64(cfLen) * 8
				break
			}
			if method == "AESV2" {
				break
			}
		}
	}
	if length < 40 || length > 128 || length%8 != 0 || v == 1 && length != 40 {
		return fmt.Errorf("invalid encryption key length")
	}
	ownerObj, err := r.Resolve(dict["O"])
	if err != nil {
		return err
	}
	userObj, err := r.Resolve(dict["U"])
	if err != nil {
		return err
	}
	permObj, err := r.Resolve(dict["P"])
	if err != nil {
		return err
	}
	owner, ook := ownerObj.(String)
	user, uok := userObj.(String)
	permissions, pok := permObj.(Integer)
	idObject, err := r.Resolve(r.Trailer["ID"])
	if err != nil {
		return err
	}
	ids, _ := idObject.(Array)
	if !ook || !uok || len(owner) != 32 || len(user) != 32 || !pok || permissions < -1<<31 || permissions > 1<<32-1 || len(ids) != 2 {
		return fmt.Errorf("invalid standard encryption parameters")
	}
	firstID, err := r.Resolve(ids[0])
	if err != nil {
		return err
	}
	id, ok := firstID.(String)
	if !ok {
		return fmt.Errorf("invalid encryption file identifier")
	}
	s.info.Permissions = uint32(permissions)
	n := int(length / 8)
	key := s.authenticateUser(password, owner, user, id, n)
	ownerKey := md5.Sum(padPassword(password))
	if revision >= 3 {
		for range 50 {
			ownerKey = md5.Sum(ownerKey[:])
		}
	}
	defer clear(ownerKey[:])
	decoded := bytes.Clone(owner)
	defer clear(decoded)
	for i := 0; i == 0 || revision >= 3 && i < 20; i++ {
		k := bytes.Clone(ownerKey[:n])
		if revision >= 3 {
			for j := range k {
				k[j] ^= byte(19 - i)
			}
		}
		c, _ := rc4.NewCipher(k)
		clear(k)
		c.XORKeyStream(decoded, decoded)
	}
	if ownerKey := s.authenticateUser(decoded, owner, user, id, n); ownerKey != nil {
		clear(key)
		key, s.info.Owner = ownerKey, true
	}
	if key == nil {
		return ErrPassword
	}
	s.key = key
	defer func() {
		if r.security != s {
			clear(s.key)
		}
	}()
	for _, name := range []Name{s.streamFilter, s.stringFilter, s.embeddedFilter} {
		if _, err := s.cryptMethod(name); err != nil {
			return err
		}
	}
	r.security = s
	return nil
}

// securityFilters 在认证前解析加密过滤器的间接字典，不改变原始加密参数
// 入参: object CF字典或间接引用
// 返回: Dictionary 已解析的字典, error 引用或类型错误
func (r *Reader) securityFilters(object Object) (Dictionary, error) {
	if object == nil {
		return nil, nil
	}
	value, err := r.Resolve(object)
	if err != nil {
		return nil, err
	}
	dict, ok := value.(Dictionary)
	if !ok {
		return nil, fmt.Errorf("invalid crypt filter dictionary")
	}
	result := make(Dictionary, len(dict))
	for name, entry := range dict {
		value, err := r.Resolve(entry)
		if err != nil {
			return nil, err
		}
		filter, ok := value.(Dictionary)
		if !ok {
			return nil, fmt.Errorf("invalid crypt filter %s dictionary", name)
		}
		copied := make(Dictionary, len(filter))
		for key, item := range filter {
			resolved, err := r.Resolve(item)
			if err != nil {
				return nil, err
			}
			copied[key] = resolved
		}
		if r.warning != nil {
			if length, ok := copied["Length"].(Integer); ok && ((copied["CFM"] == Name("AESV2") && length == 128) || (copied["CFM"] == Name("AESV3") && length == 256)) {
				copied["Length"] = length / 8
				r.warning(Diagnostic{Message: fmt.Sprintf("crypt filter %s Length is in bits; equivalent byte length retained", name)})
			}
		}
		result[name] = copied
	}
	return result, nil
}

// authenticateUser 按标准安全处理器算法2、4、5验证用户密码并派生文件密钥
// 入参: password 密码, owner 所有者条目, user 用户条目, id 文件标识, n 密钥字节数
// 返回: []byte 文件密钥，认证失败时为nil
func (s *standardSecurity) authenticateUser(password, owner, user, id []byte, n int) []byte {
	h := md5.New()
	h.Write(padPassword(password))
	h.Write(owner)
	var p [4]byte
	binary.LittleEndian.PutUint32(p[:], s.info.Permissions)
	h.Write(p[:])
	h.Write(id)
	if s.info.Revision >= 4 && !s.encryptMetadata {
		h.Write([]byte{255, 255, 255, 255})
	}
	digest := h.Sum(nil)
	if s.info.Revision >= 3 {
		for range 50 {
			sum := md5.Sum(digest[:n])
			digest = sum[:]
		}
	}
	key := bytes.Clone(digest[:n])
	check := bytes.Clone(passwordPadding[:])
	if s.info.Revision >= 3 {
		h.Reset()
		h.Write(check)
		h.Write(id)
		check = h.Sum(nil)
	}
	for i := 0; i == 0 || s.info.Revision >= 3 && i < 20; i++ {
		k := bytes.Clone(key)
		for j := range k {
			k[j] ^= byte(i)
		}
		c, _ := rc4.NewCipher(k)
		clear(k)
		c.XORKeyStream(check, check)
	}
	if subtle.ConstantTimeCompare(check, user[:len(check)]) != 1 {
		clear(key)
		return nil
	}
	return key
}

// cryptMethod 解析命名加密过滤器并检查密钥长度
// 入参: name 过滤器名称
// 返回: Name 加密方法, error 过滤器错误
func (s *standardSecurity) cryptMethod(name Name) (Name, error) {
	if name == "Identity" {
		return name, nil
	}
	if s.info.Revision < 4 {
		if name != "V2" {
			return "", fmt.Errorf("unsupported crypt filter %s for encryption revision %d", name, s.info.Revision)
		}
		return "V2", nil
	}
	dict, ok := s.filters[name].(Dictionary)
	if !ok {
		return "", fmt.Errorf("missing crypt filter %s", name)
	}
	method, ok := dict["CFM"].(Name)
	if dict["CFM"] == nil || method == "None" {
		return "Identity", nil
	}
	if !ok || method != "V2" && method != "AESV2" && method != "AESV3" {
		return "", &UnsupportedError{Feature: "crypt filter method"}
	}
	if (s.info.Revision >= 5) != (method == "AESV3") {
		return "", fmt.Errorf("crypt filter does not match encryption revision")
	}
	length, err := integerDefault(dict, "Length", int64(len(s.key)))
	if err != nil || length != int64(len(s.key)) || method == "AESV2" && length != 16 || method == "AESV3" && length != 32 {
		return "", fmt.Errorf("invalid crypt filter key length")
	}
	return method, nil
}

// decryptBytes 按对象编号和代数解密RC4或AES数据并验证填充
// 入参: data 加密数据, ref 所属对象, name 过滤器名称
// 返回: []byte 解密数据, error 解密错误
func (s *standardSecurity) decryptBytes(data []byte, ref Reference, name Name) ([]byte, error) {
	method, err := s.cryptMethod(name)
	if err != nil || method == "Identity" {
		return data, err
	}
	key := s.key
	if method != "AESV3" {
		seed := append(bytes.Clone(s.key), byte(ref.Number), byte(ref.Number>>8), byte(ref.Number>>16), byte(ref.Generation), byte(ref.Generation>>8))
		if method == "AESV2" {
			seed = append(seed, 's', 'A', 'l', 'T')
		}
		digest := md5.Sum(seed)
		clear(seed)
		key = digest[:min(len(s.key)+5, 16)]
	}
	if method == "V2" {
		c, err := rc4.NewCipher(key)
		if err != nil {
			return nil, err
		}
		out := make([]byte, len(data))
		c.XORKeyStream(out, data)
		return out, nil
	}
	if len(data) == 0 {
		return nil, nil
	}
	if len(data) < 2*aes.BlockSize || len(data)%aes.BlockSize != 0 {
		return nil, fmt.Errorf("invalid AES encrypted data length")
	}
	c, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	out := make([]byte, len(data)-aes.BlockSize)
	cipher.NewCBCDecrypter(c, data[:aes.BlockSize]).CryptBlocks(out, data[aes.BlockSize:])
	padding := int(out[len(out)-1])
	if padding == 0 || padding > aes.BlockSize {
		return nil, fmt.Errorf("invalid AES padding")
	}
	for _, b := range out[len(out)-padding:] {
		if int(b) != padding {
			return nil, fmt.Errorf("invalid AES padding")
		}
	}
	return out[:len(out)-padding], nil
}

// resolveObjectType 安全解析字典Type字段的直接或间接名称，避免自引用循环
// 入参: r 阅读器, value Type字段值, ref 当前对象引用
// 返回: Name 对象类型名称, error 引用读取错误
func resolveObjectType(r *Reader, value Object, ref Reference) (Name, error) {
	if name, ok := value.(Name); ok {
		return name, nil
	}
	target, ok := value.(Reference)
	if !ok {
		return "", nil
	}
	seen := map[Reference]bool{ref: true}
	for {
		if seen[target] || r.loading[target] {
			return "", nil
		}
		seen[target] = true
		resolved, err := r.Object(target)
		if err != nil {
			return "", err
		}
		if name, ok := resolved.(Name); ok {
			return name, nil
		}
		target, ok = resolved.(Reference)
		if !ok {
			return "", nil
		}
	}
}

// decryptObject 解密直接字符串与流，保留签名值和交叉引用流
// 入参: r 阅读器, value 对象, ref 所属间接引用, embedded 是否按内嵌文件流过滤器解密
// 返回: Object 解密对象, error 解密错误
func (s *standardSecurity) decryptObject(r *Reader, value Object, ref Reference, embedded bool) (Object, error) {
	switch v := value.(type) {
	case String:
		data, err := s.decryptBytes(v, ref, s.stringFilter)
		return String(data), err
	case Array:
		for i, item := range v {
			decoded, err := s.decryptObject(r, item, ref, false)
			if err != nil {
				return nil, err
			}
			v[i] = decoded
		}
	case Dictionary:
		skipContents := false
		if v["Contents"] != nil {
			if v["ByteRange"] != nil && v["Filter"] != nil {
				skipContents = true
			} else if v["Type"] != nil {
				dictType, err := resolveObjectType(r, v["Type"], ref)
				if err != nil {
					return nil, fmt.Errorf("decrypt object %d %d /Type: %w", ref.Number, ref.Generation, err)
				}
				skipContents = dictType == Name("Sig") || dictType == Name("DocTimeStamp")
			}
		}
		for name, item := range v {
			if name == "Contents" && skipContents {
				continue
			}
			decoded, err := s.decryptObject(r, item, ref, false)
			if err != nil {
				return nil, fmt.Errorf("decrypt object %d %d /%s: %w", ref.Number, ref.Generation, name, err)
			}
			v[name] = decoded
		}
	case *Stream:
		streamType, err := resolveObjectType(r, v.Dictionary["Type"], ref)
		if err != nil {
			return nil, fmt.Errorf("decrypt stream %d %d /Type: %w", ref.Number, ref.Generation, err)
		}
		if streamType == Name("XRef") {
			return v, nil
		}
		if _, err := s.decryptObject(r, v.Dictionary, ref, false); err != nil {
			return nil, err
		}
		name := s.streamFilter
		if embedded || streamType == Name("EmbeddedFile") {
			name = s.embeddedFilter
		}
		if streamType == Name("Metadata") && !s.encryptMetadata {
			name = "Identity"
		}
		filters, params, err := v.filterChain(r)
		if err != nil {
			return nil, err
		}
		for i, filter := range filters {
			if filter != Name("Crypt") {
				continue
			}
			if s.info.Revision < 4 {
				return nil, fmt.Errorf("Crypt stream filter requires encryption revision 4 or higher")
			}
			if i != 0 {
				return nil, fmt.Errorf("Crypt must be the first stream filter")
			}
			name = "Identity"
			if dict, ok := params[i].(Dictionary); ok && dict["Name"] != nil {
				name, ok = dict["Name"].(Name)
				if !ok {
					return nil, fmt.Errorf("invalid Crypt filter name")
				}
			}
		}
		if v.Dictionary["F"] != nil {
			name = "Identity"
		}
		if len(v.Data) == aes.BlockSize && r.warning != nil {
			method, err := s.cryptMethod(name)
			if err != nil {
				return nil, fmt.Errorf("decrypt stream %d %d: %w", ref.Number, ref.Generation, err)
			}
			if method == "AESV2" || method == "AESV3" {
				r.warning(Diagnostic{Message: fmt.Sprintf("authenticated IV-only AES stream %d %d decoded as empty without changing source bytes", ref.Number, ref.Generation)})
				v.Data = nil
				v.decrypted = true
				return v, nil
			}
		}
		v.Data, err = s.decryptBytes(v.Data, ref, name)
		if err != nil {
			return nil, fmt.Errorf("decrypt stream %d %d: %w", ref.Number, ref.Generation, err)
		}
		v.decrypted = true
	}
	return value, nil
}
