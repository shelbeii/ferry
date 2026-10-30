package ai

import (
	"regexp"
	"strings"
)

// 发给模型服务商、写进改动记录和服务器日志之前，遮住常见的密码、密钥。
// 只是尽力而为：认不出来的格式照样会发出去（设置里有提醒）。界面上给用户自己看的输出不遮。

const mask = "******"

var (
	rePEM = regexp.MustCompile(`-----BEGIN ([A-Z ]*)PRIVATE KEY-----[\s\S]*?-----END ([A-Z ]*)PRIVATE KEY-----`)
	// key = value、key: value、"key": "value"（名字里带 password、secret、token、apikey……）
	reKV = regexp.MustCompile(`(?i)([\w.-]*(?:password|passwd|pwd|secret|token|api[_-]?key|access[_-]?key|private[_-]?key|credential|requirepass|masterauth)[\w.-]*["']?\s*[:=]\s*)(["']?)([^\s"',;}]+)`)
	// 配置文件里「名字 值」一行（如 redis 的 requirepass xxx）
	reLineKV = regexp.MustCompile(`(?im)^(\s*[\w.-]*(?:password|passwd|secret|token|requirepass|masterauth)[\w.-]*[ \t]+)(\S+)[ \t]*$`)
	reURL    = regexp.MustCompile(`([a-zA-Z][a-zA-Z0-9+.-]*://[^\s:/@]+):([^\s@/]+)@`)
	reBearer = regexp.MustCompile(`(?i)(authorization:\s*(?:bearer|basic)\s+)\S+`)
	reSK     = regexp.MustCompile(`\b(sk-[A-Za-z0-9_-]{4})[A-Za-z0-9_-]{12,}`)
	reAKIA   = regexp.MustCompile(`\b(AKIA)[0-9A-Z]{16}\b`)
	reJWT    = regexp.MustCompile(`\beyJ[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}`)
	// 命令行里的密码：mysql -p密码、--password=xxx、redis-cli -a xxx
	reMySQLP = regexp.MustCompile(`(\b(?:mysql|mysqldump|mysqladmin|mariadb|mariadb-dump)\b[^\n|;&]*?\s-p)([^\s'"]+)`)
	rePassEq = regexp.MustCompile(`(?i)(--(?:password|pass|passwd|token|secret)[= ])(\S+)`)
	reRedisA = regexp.MustCompile(`(\bredis-cli\b[^\n|;&]*?\s-a\s+)(\S+)`)
)

// 这些值不是密码（布尔、空值、占位符），不用遮
var notSecret = map[string]bool{"yes": true, "no": true, "true": true, "false": true, "on": true, "off": true, "none": true,
	"null": true, "nil": true, "required": true, "optional": true, "": true, "******": true, "<hidden>": true, "xxx": true}

// Redact 遮住文本里常见的密码、密钥、令牌。
func Redact(s string) string {
	if s == "" {
		return s
	}
	s = rePEM.ReplaceAllString(s, "-----BEGIN ${1}PRIVATE KEY-----（已隐藏）-----END ${2}PRIVATE KEY-----")
	s = reKV.ReplaceAllStringFunc(s, func(m string) string {
		sub := reKV.FindStringSubmatch(m)
		if notSecret[strings.ToLower(sub[3])] || strings.HasPrefix(sub[3], "$") {
			return m // 布尔、变量引用（$PASSWORD）不遮
		}
		return sub[1] + sub[2] + mask
	})
	s = reLineKV.ReplaceAllStringFunc(s, func(m string) string {
		sub := reLineKV.FindStringSubmatch(m)
		if notSecret[strings.ToLower(sub[2])] {
			return m
		}
		return sub[1] + mask
	})
	s = reURL.ReplaceAllString(s, "${1}:"+mask+"@")
	s = reBearer.ReplaceAllString(s, "${1}"+mask)
	s = reSK.ReplaceAllString(s, "${1}"+mask)
	s = reAKIA.ReplaceAllString(s, "${1}"+mask)
	s = reJWT.ReplaceAllString(s, "（令牌已隐藏）")
	s = reMySQLP.ReplaceAllString(s, "${1}"+mask)
	s = rePassEq.ReplaceAllString(s, "${1}"+mask)
	s = reRedisA.ReplaceAllString(s, "${1}"+mask)
	return s
}
