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
}

// passwordPadding 保存PDF标准安全处理器的密码填充字节
var passwordPadding = [32]byte{0x28, 0xbf, 0x4e, 0x5e, 0x4e, 0x75, 0x8a, 0x41, 0x64, 0x00, 0x4e, 0x56, 0xff, 0xfa, 0x01, 0x08, 0x2e, 0x2e, 0x00, 0xb6, 0xd0, 0x68, 0x3e, 0x80, 0x2f, 0x0c, 0xa9, 0xfe, 0x64, 0x53, 0x69, 0x7a}

// Encryption 返回加密信息副本，未加密文档返回nil
// 返回: *EncryptionInfo 加密信息
func (r *Reader) Encryption() *EncryptionInfo {
	if r.security == nil {
		return nil
	}
	info := r.security.info
	return &info
}

// openSecurity 解析加密字典并认证用户或所有者密码
// 入参: password 用户或所有者密码
// 返回: error 认证或加密参数错误
func (r *Reader) openSecurity(password []byte) error {
	object, err := r.Resolve(r.Trailer["Encrypt"])
	if err != nil || object == nil {
		return err
	}
	dict, ok := object.(Dictionary)
	if !ok {
		return fmt.Errorf("invalid encryption dictionary")
	}
	if dict["Filter"] != Name("Standard") {
		return &UnsupportedError{Feature: "PDF security handler"}
	}
	v, vok := dict["V"].(Integer)
	revision, rok := dict["R"].(Integer)
	if !vok || !rok || !(v == 1 && (revision == 2 || revision == 3) || v == 2 && revision == 3 || v == 4 && revision == 4) {
		return &UnsupportedError{Feature: "PDF encryption revision"}
	}
	length, err := integerDefault(dict, "Length", 40)
	if err != nil || length < 40 || length > 128 || length%8 != 0 || v == 1 && length != 40 {
		return fmt.Errorf("invalid encryption key length")
	}
	owner, ook := dict["O"].(String)
	user, uok := dict["U"].(String)
	permissions, pok := dict["P"].(Integer)
	ids, _ := r.Trailer["ID"].(Array)
	if !ook || !uok || len(owner) != 32 || len(user) != 32 || !pok || permissions < -1<<31 || permissions > 1<<32-1 || len(ids) != 2 {
		return fmt.Errorf("invalid standard encryption parameters")
	}
	id, ok := ids[0].(String)
	if !ok {
		return fmt.Errorf("invalid encryption file identifier")
	}
	s := &standardSecurity{info: EncryptionInfo{Revision: int(revision), Permissions: uint32(permissions)}, encryptMetadata: true, streamFilter: "V2", stringFilter: "V2", embeddedFilter: "V2"}
	if v == 4 {
		if value := dict["EncryptMetadata"]; value != nil {
			b, ok := value.(Boolean)
			if !ok {
				return fmt.Errorf("invalid EncryptMetadata")
			}
			s.encryptMetadata = bool(b)
		}
		if value := dict["CF"]; value != nil {
			s.filters, ok = value.(Dictionary)
			if !ok {
				return fmt.Errorf("invalid crypt filter dictionary")
			}
		}
		for _, entry := range []struct {
			name  Name
			value *Name
		}{{"StmF", &s.streamFilter}, {"StrF", &s.stringFilter}} {
			*entry.value = "Identity"
			if value := dict[entry.name]; value != nil {
				*entry.value, ok = value.(Name)
				if !ok {
					return fmt.Errorf("invalid %s crypt filter", entry.name)
				}
			}
		}
		s.embeddedFilter = s.streamFilter
		if value := dict["EFF"]; value != nil {
			s.embeddedFilter, ok = value.(Name)
			if !ok {
				return fmt.Errorf("invalid embedded file crypt filter")
			}
		}
	}
	n := int(length / 8)
	key := s.authenticateUser(password, owner, user, id, n)
	ownerKey := md5.Sum(padPassword(password))
	if revision >= 3 {
		for range 50 {
			ownerKey = md5.Sum(ownerKey[:])
		}
	}
	decoded := bytes.Clone(owner)
	for i := 0; i == 0 || revision >= 3 && i < 20; i++ {
		k := bytes.Clone(ownerKey[:n])
		if revision >= 3 {
			for j := range k {
				k[j] ^= byte(19 - i)
			}
		}
		c, _ := rc4.NewCipher(k)
		c.XORKeyStream(decoded, decoded)
	}
	if ownerKey := s.authenticateUser(decoded, owner, user, id, n); ownerKey != nil {
		key, s.info.Owner = ownerKey, true
	}
	if key == nil {
		return ErrPassword
	}
	s.key = key
	for _, name := range []Name{s.streamFilter, s.stringFilter, s.embeddedFilter} {
		if _, err := s.cryptMethod(name); err != nil {
			return err
		}
	}
	r.security = s
	return nil
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
		c.XORKeyStream(check, check)
	}
	if subtle.ConstantTimeCompare(check, user[:len(check)]) != 1 {
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
	if !ok || method != "V2" && method != "AESV2" {
		return "", &UnsupportedError{Feature: "crypt filter method"}
	}
	length, err := integerDefault(dict, "Length", int64(len(s.key)))
	if err != nil || length != int64(len(s.key)) || method == "AESV2" && length != 16 {
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
	seed := append(bytes.Clone(s.key), byte(ref.Number), byte(ref.Number>>8), byte(ref.Number>>16), byte(ref.Generation), byte(ref.Generation>>8))
	if method == "AESV2" {
		seed = append(seed, 's', 'A', 'l', 'T')
	}
	digest := md5.Sum(seed)
	key := digest[:min(len(s.key)+5, 16)]
	if method == "V2" {
		c, err := rc4.NewCipher(key)
		if err != nil {
			return nil, err
		}
		out := make([]byte, len(data))
		c.XORKeyStream(out, data)
		return out, nil
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

// decryptObject 解密直接字符串与流，保留签名值和交叉引用流
// 入参: value 对象, ref 所属间接引用
// 返回: Object 解密对象, error 解密错误
func (s *standardSecurity) decryptObject(value Object, ref Reference) (Object, error) {
	switch v := value.(type) {
	case String:
		data, err := s.decryptBytes(v, ref, s.stringFilter)
		return String(data), err
	case Array:
		for i, item := range v {
			decoded, err := s.decryptObject(item, ref)
			if err != nil {
				return nil, err
			}
			v[i] = decoded
		}
	case Dictionary:
		for name, item := range v {
			if name == "Contents" && (v["Type"] == Name("Sig") || v["Type"] == Name("DocTimeStamp") || v["ByteRange"] != nil && v["Filter"] != nil) {
				continue
			}
			decoded, err := s.decryptObject(item, ref)
			if err != nil {
				return nil, fmt.Errorf("decrypt object %d %d /%s: %w", ref.Number, ref.Generation, name, err)
			}
			v[name] = decoded
		}
	case *Stream:
		if v.Dictionary["Type"] == Name("XRef") {
			return v, nil
		}
		if _, err := s.decryptObject(v.Dictionary, ref); err != nil {
			return nil, err
		}
		name := s.streamFilter
		if v.Dictionary["Type"] == Name("EmbeddedFile") {
			name = s.embeddedFilter
		}
		if v.Dictionary["Type"] == Name("Metadata") && !s.encryptMetadata {
			name = "Identity"
		}
		if v.Dictionary["F"] != nil {
			name = "Identity"
		}
		filters, params, err := v.filterChain(v.reader)
		if err != nil {
			return nil, err
		}
		for i, filter := range filters {
			if filter != Name("Crypt") {
				continue
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
		v.Data, err = s.decryptBytes(v.Data, ref, name)
		if err != nil {
			return nil, fmt.Errorf("decrypt stream %d %d: %w", ref.Number, ref.Generation, err)
		}
		v.decrypted = true
	}
	return value, nil
}
