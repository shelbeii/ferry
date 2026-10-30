package ai

import (
	"strings"
	"testing"
)

func TestRedact(t *testing.T) {
	in := strings.Join([]string{
		"spring.datasource.password=S3cretPass",
		`"apiKey": "abcd1234efgh"`,
		"requirepass myRedisPass",
		"jdbc:mysql://app:dbpass123@10.0.0.5:3306/app",
		"Authorization: Bearer abc.def.ghi",
		"mysql -uroot -pRootPass123 -e 'show databases'",
		"redis-cli -a redispass ping",
		"--password=xyz789",
		"-----BEGIN RSA PRIVATE KEY-----\nMIIEow\nAAAA\n-----END RSA PRIVATE KEY-----",
		"PasswordAuthentication no",
		"password=$DB_PASSWORD",
	}, "\n")
	out := Redact(in)
	for _, leaked := range []string{"S3cretPass", "abcd1234efgh", "myRedisPass", "dbpass123", "abc.def.ghi", "RootPass123", "redispass", "xyz789", "MIIEow"} {
		if strings.Contains(out, leaked) {
			t.Errorf("没遮住 %q：\n%s", leaked, out)
		}
	}
	for _, kept := range []string{"spring.datasource.password=", "PasswordAuthentication no", "$DB_PASSWORD", "mysql -uroot -p", "@10.0.0.5:3306/app"} {
		if !strings.Contains(out, kept) {
			t.Errorf("应保留 %q：\n%s", kept, out)
		}
	}
	if Redact("Filesystem  Size  Used\n/dev/sda1  50G  45G") != "Filesystem  Size  Used\n/dev/sda1  50G  45G" {
		t.Error("普通输出不应改动")
	}
}

// 用通配符、{a,b} 写敏感文件也不能自动读
func TestSensitivePatterns(t *testing.T) {
	if ReadOnly("cat /root/.ss?/id_*") == nil || ReadOnly("cat ~/.ssh/{id_rsa,x}") == nil {
		t.Error("应拒绝")
	}
	if err := ReadOnly("ls /var/log/*.log"); err != nil {
		t.Errorf("普通通配符应放行：%v", err)
	}
}
