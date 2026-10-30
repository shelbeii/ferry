package filetext

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"strings"
	"unicode/utf16"
	"unicode/utf8"
)

const Limit = 1 << 20

type Document struct {
	ReadOnly    string `json:"readOnly,omitempty"`
	Backup      bool   `json:"backup"`
	Path        string `json:"path"`
	Size        int64  `json:"size"`
	Content     string `json:"content"`
	Encoding    string `json:"encoding"`
	Truncated   bool   `json:"truncated"`
	Limit       int    `json:"limit"`
	Unsupported string `json:"unsupported,omitempty"`
	Revision    string `json:"revision,omitempty"` // 完整内容及目标元数据的指纹，保存前校验。
	BOM         bool   `json:"bom"`
	LineEnding  string `json:"lineEnding"`
}

func Decode(r io.Reader) (*Document, error) {
	b, err := io.ReadAll(io.LimitReader(r, Limit+1))
	if err != nil {
		return nil, err
	}
	out := &Document{Limit: Limit, Truncated: len(b) > Limit}
	digest := fmt.Sprintf("%x", sha256.Sum256(b))
	if out.Truncated {
		b = b[:Limit]
	}
	text, encoding, ok := Text(b, out.Truncated)
	if !ok {
		out.Unsupported = "此文件包含二进制数据或使用了暂不支持的编码，请下载后查看。支持 UTF-8 和带 BOM 的 UTF-16 文本。"
		return out, nil
	}
	out.Content, out.Encoding = text, encoding
	out.BOM = bytes.HasPrefix(b, []byte{0xef, 0xbb, 0xbf}) || encoding != "UTF-8"
	out.LineEnding = "LF"
	if strings.Contains(text, "\r\n") {
		out.LineEnding = "CRLF"
	} else if strings.Contains(text, "\r") {
		out.LineEnding = "CR"
	}
	if !out.Truncated {
		out.Revision = digest
	}
	return out, nil
}

func Text(b []byte, truncated bool) (string, string, bool) {
	encoding := "UTF-8"
	var text string
	switch {
	case bytes.HasPrefix(b, []byte{0xff, 0xfe, 0, 0}), bytes.HasPrefix(b, []byte{0, 0, 0xfe, 0xff}):
		return "", "", false // UTF-32 的 BOM 不能当成 UTF-16。
	case bytes.HasPrefix(b, []byte{0xff, 0xfe}), bytes.HasPrefix(b, []byte{0xfe, 0xff}):
		var order binary.ByteOrder = binary.LittleEndian
		encoding = "UTF-16 LE"
		if b[0] == 0xfe {
			order, encoding = binary.BigEndian, "UTF-16 BE"
		}
		b = b[2:]
		if len(b)%2 != 0 {
			if !truncated {
				return "", "", false
			}
			b = b[:len(b)-1]
		}
		units := make([]uint16, len(b)/2)
		for i := range units {
			units[i] = order.Uint16(b[2*i:])
		}
		if truncated && len(units) > 0 && units[len(units)-1] >= 0xd800 && units[len(units)-1] <= 0xdbff {
			units = units[:len(units)-1]
		}
		for i := 0; i < len(units); i++ {
			u := units[i]
			if u >= 0xd800 && u <= 0xdbff {
				if i+1 == len(units) || units[i+1] < 0xdc00 || units[i+1] > 0xdfff {
					return "", "", false
				}
				i++
			} else if u >= 0xdc00 && u <= 0xdfff {
				return "", "", false
			}
		}
		text = string(utf16.Decode(units))
	default:
		b = bytes.TrimPrefix(b, []byte{0xef, 0xbb, 0xbf})
		if truncated {
			// 预览边界可能正好切在一个 UTF-8 字符中间，只去掉不完整的尾部。
			for i := len(b) - 1; i >= 0 && i >= len(b)-utf8.UTFMax; i-- {
				if utf8.RuneStart(b[i]) {
					if !utf8.FullRune(b[i:]) {
						b = b[:i]
					}
					break
				}
			}
		}
		if !utf8.Valid(b) {
			return "", "", false
		}
		text = string(b)
	}
	if strings.ContainsFunc(text, func(r rune) bool { return r < 0x20 && r != '\t' && r != '\n' && r != '\r' && r != '\f' }) {
		return "", "", false
	}
	return text, encoding, true
}

func Encode(text string, original *Document) ([]byte, error) {
	if !utf8.ValidString(text) || len(text) > Limit*4 {
		return nil, errors.New("内容无效或过大，无法保存")
	}
	if _, _, ok := Text([]byte(text), false); !ok {
		return nil, errors.New("内容包含二进制控制字符，无法作为文本保存")
	}
	text = strings.ReplaceAll(strings.ReplaceAll(text, "\r\n", "\n"), "\r", "\n")
	switch original.LineEnding {
	case "CRLF":
		text = strings.ReplaceAll(text, "\n", "\r\n")
	case "CR":
		text = strings.ReplaceAll(text, "\n", "\r")
	}
	var data []byte
	switch original.Encoding {
	case "UTF-8":
		if original.BOM {
			data = append(data, 0xef, 0xbb, 0xbf)
		}
		data = append(data, text...)
	case "UTF-16 LE", "UTF-16 BE":
		var order binary.ByteOrder = binary.LittleEndian
		if original.Encoding == "UTF-16 BE" {
			order = binary.BigEndian
		}
		units := append([]uint16{0xfeff}, utf16.Encode([]rune(text))...)
		data = make([]byte, len(units)*2)
		for i, u := range units {
			order.PutUint16(data[2*i:], u)
		}
	default:
		return nil, errors.New("暂不支持保存此文件编码")
	}
	if len(data) > Limit {
		return nil, errors.New("编辑后的文件超过 1 MiB，请下载后编辑并上传")
	}
	return data, nil
}
