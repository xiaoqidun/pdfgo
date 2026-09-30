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
	"crypto/sha256"
	"crypto/sha512"
	"crypto/subtle"
	"encoding/binary"
	"fmt"
)

// openAES256Security 认证R5/R6密码并校验AES-256文件密钥和权限
// 入参: dict 加密字典, password 已编码密码, revision 安全处理器版本
// 返回: error 认证或参数错误
func (r *Reader) openAES256Security(dict Dictionary, password []byte, revision int) error {
	length, err := integerDefault(dict, "Length", 256)
	if err != nil || length != 256 {
		return fmt.Errorf("invalid AES-256 key length")
	}
	values := make(map[Name][]byte, 5)
	for name, size := range map[Name]int{"O": 48, "U": 48, "OE": 32, "UE": 32, "Perms": 16} {
		value, ok := dict[name].(String)
		if !ok || len(value) != size {
			return fmt.Errorf("invalid AES-256 %s entry", name)
		}
		values[name] = value
	}
	permissions, ok := dict["P"].(Integer)
	if !ok || permissions < -1<<31 || permissions > 1<<32-1 {
		return fmt.Errorf("invalid AES-256 permissions")
	}
	s := &standardSecurity{info: EncryptionInfo{Revision: revision, Permissions: uint32(permissions)}, encryptMetadata: true, streamFilter: "Identity", stringFilter: "Identity", embeddedFilter: "Identity"}
	if value := dict["EncryptMetadata"]; value != nil {
		flag, ok := value.(Boolean)
		if !ok {
			return fmt.Errorf("invalid EncryptMetadata")
		}
		s.encryptMetadata = bool(flag)
	}
	if value := dict["CF"]; value != nil {
		s.filters, ok = value.(Dictionary)
		if !ok {
			return fmt.Errorf("invalid crypt filter dictionary")
		}
	}
	for _, entry := range []struct {
		name   Name
		target *Name
	}{{"StmF", &s.streamFilter}, {"StrF", &s.stringFilter}} {
		if value := dict[entry.name]; value != nil {
			*entry.target, ok = value.(Name)
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
	password = password[:min(len(password), 127)]
	user, owner := values["U"], values["O"]
	validation := aes256PasswordHash(password, owner[32:40], user, revision)
	ownerValid := subtle.ConstantTimeCompare(validation, owner[:32]) == 1
	clear(validation)
	entry, encrypted, extra := owner, values["OE"], user
	if ownerValid {
		s.info.Owner = true
	} else {
		validation = aes256PasswordHash(password, user[32:40], nil, revision)
		userValid := subtle.ConstantTimeCompare(validation, user[:32]) == 1
		clear(validation)
		if !userValid {
			return ErrPassword
		}
		entry, encrypted, extra = user, values["UE"], nil
	}
	wrapping := aes256PasswordHash(password, entry[40:48], extra, revision)
	defer clear(wrapping)
	block, _ := aes.NewCipher(wrapping)
	s.key = make([]byte, 32)
	cipher.NewCBCDecrypter(block, make([]byte, aes.BlockSize)).CryptBlocks(s.key, encrypted)
	valid := false
	defer func() {
		if !valid {
			clear(s.key)
		}
	}()
	block, _ = aes.NewCipher(s.key)
	var plain [16]byte
	block.Decrypt(plain[:], values["Perms"])
	defer clear(plain[:])
	metadata := byte('F')
	if s.encryptMetadata {
		metadata = 'T'
	}
	if binary.LittleEndian.Uint32(plain[:4]) != s.info.Permissions || !bytes.Equal(plain[4:8], []byte{255, 255, 255, 255}) || plain[8] != metadata || string(plain[9:12]) != "adb" {
		return fmt.Errorf("invalid AES-256 permissions authentication")
	}
	for _, name := range []Name{s.streamFilter, s.stringFilter, s.embeddedFilter} {
		if _, err := s.cryptMethod(name); err != nil {
			return err
		}
	}
	r.security = s
	valid = true
	return nil
}

// aes256PasswordHash 按R5或算法2.B派生密码摘要
// 入参: password 截断后的密码, salt 盐, user 所有者认证使用的U条目, revision 版本
// 返回: []byte 32字节摘要
func aes256PasswordHash(password, salt, user []byte, revision int) []byte {
	h := sha256.New()
	h.Write(password)
	h.Write(salt)
	h.Write(user)
	key := h.Sum(nil)
	if revision == 5 {
		return key
	}
	buffer := make([]byte, 64*(len(password)+64+len(user)))
	defer clear(buffer)
	for round, last := 0, 0; round < 64 || last > round-32; round++ {
		size := len(password) + len(key) + len(user)
		copy(buffer, password)
		copy(buffer[len(password):], key)
		copy(buffer[len(password)+len(key):], user)
		for i := 1; i < 64; i++ {
			copy(buffer[i*size:], buffer[:size])
		}
		data := buffer[:size*64]
		block, _ := aes.NewCipher(key[:16])
		cipher.NewCBCEncrypter(block, key[16:32]).CryptBlocks(data, data)
		selector := 0
		for _, b := range data[:16] {
			selector += int(b)
		}
		switch selector % 3 {
		case 0:
			sum := sha256.Sum256(data)
			key = append(key[:0], sum[:]...)
		case 1:
			sum := sha512.Sum384(data)
			key = append(key[:0], sum[:]...)
		case 2:
			sum := sha512.Sum512(data)
			key = append(key[:0], sum[:]...)
		}
		last = int(data[len(data)-1])
	}
	result := bytes.Clone(key[:32])
	clear(key)
	return result
}
