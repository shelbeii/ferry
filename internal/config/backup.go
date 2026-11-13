package config

import (
	"regexp"
	"strings"
)

// Backup 一项备份（顶层 backups）：数据库（MySQL、PostgreSQL）、目录，或自己写的命令。
// 写在配置里不会自动开始：在服务器上打开（ferry backup <目标> on，或桌面版的「数据备份」）后才按时备份，随时可以关掉。
type Backup struct {
	Name     string    `yaml:"name"`
	MySQL    *DBBackup `yaml:"mysql"`
	Postgres *DBBackup `yaml:"postgres"`
	// Paths 要打包的目录或文件（服务器上的绝对路径，可以用 ${变量}）
	Paths []string `yaml:"paths"`
	// Command 输出就是备份内容的命令（sh -c，以 root 执行），如 redis-cli --rdb -
	Command string `yaml:"command"`
	// Restore 恢复的命令：备份的内容从标准输入进来。mysql、postgres、paths 不用写
	Restore string `yaml:"restore"`
	// EnvFile 执行时读的环境变量文件（服务器上）：数据库密码放这里（MYSQL_PWD=…、PGPASSWORD=…），不写进配置
	EnvFile StringList `yaml:"envFile"`
	// Schedule daily（默认，每天 3 点）| hourly | weekly | systemd 的 OnCalendar（如 *-*-* 02:30:00）
	Schedule string `yaml:"schedule"`
	Keep     int    `yaml:"keep"` // 保留最近几份，默认 7
	// BeforeDeploy 部署前先备份一次（失败就不部署）
	BeforeDeploy bool `yaml:"beforeDeploy"`
	// Targets 只在这些目标上；空 = 全部
	Targets []string `yaml:"targets"`
	// Upload 每次定时、手动备份完，再传一份到别处（服务器坏了、被删、被勒索加密时数据还在）
	Upload *BackupUpload `yaml:"upload"`
}

// BackupUpload 异地备份：传到 S3 兼容的对象存储（阿里云 OSS、腾讯云 COS、MinIO、AWS S3……）。
// 访问密钥写在 envFile 里（S3_ACCESS_KEY_ID、S3_SECRET_ACCESS_KEY）；加密的密码也写在那里（FERRY_BACKUP_PASSWORD）。
type BackupUpload struct {
	S3 *S3Upload `yaml:"s3"`
	// Encrypt 上传前加密（age 格式，没有 Ferry 也能用 age -d 解开）；默认 true
	Encrypt *bool `yaml:"encrypt"`
	// Keep 那边留几份；默认和 keep 一样
	Keep int `yaml:"keep"`
}

// S3Upload 对象存储的桶。
type S3Upload struct {
	// Endpoint 地址：https://oss-cn-hangzhou.aliyuncs.com、https://cos.ap-guangzhou.myqcloud.com、http://10.0.0.5:9000
	Endpoint string `yaml:"endpoint"`
	Bucket   string `yaml:"bucket"`
	// Region 签名用的区域；不写按地址猜（OSS、COS、AWS 猜得出来），猜不到用 us-east-1
	Region string `yaml:"region"`
	// Prefix 放在桶里的哪个目录；默认 ferry/<项目>/<目标>/
	Prefix string `yaml:"prefix"`
	// PathStyle 用 http://地址/桶/文件 的形式（MinIO 常用）；默认 http://桶.地址/文件。地址是 IP 或带端口时自动用路径形式
	PathStyle bool `yaml:"pathStyle"`
}

// EncryptOn 上传前要不要加密（默认要）。
func (u *BackupUpload) EncryptOn() bool { return u.Encrypt == nil || *u.Encrypt }

// DBBackup 数据库：连哪个库。密码放在 envFile 里（MYSQL_PWD / PGPASSWORD）。
type DBBackup struct {
	Database string `yaml:"database"`
	Host     string `yaml:"host"` // 默认 127.0.0.1（容器里默认 localhost）
	Port     int    `yaml:"port"` // 默认 3306 / 5432
	User     string `yaml:"user"` // 默认 root / postgres
	// Container 数据库在 Docker 容器里（如 1Panel 装的 1Panel-mysql）：在容器里执行 mysqldump / pg_dump
	Container string `yaml:"container"`
	// Bin mysqldump / pg_dump 等程序所在的目录（数据库的客户端不在 PATH 里时，如 Ferry 用官方安装包装的 MySQL）
	Bin string `yaml:"bin"`
}

// Kind 备份的种类：mysql | postgres | paths | command。
func (b *Backup) Kind() string {
	switch {
	case b.MySQL != nil:
		return "mysql"
	case b.Postgres != nil:
		return "postgres"
	case len(b.Paths) > 0:
		return "paths"
	}
	return "command"
}

// KeepN 保留几份。
func (b *Backup) KeepN() int {
	if b.Keep <= 0 {
		return 7
	}
	return b.Keep
}

// OnCalendar systemd 定时器的 OnCalendar。
func (b *Backup) OnCalendar() string {
	switch b.Schedule {
	case "", "daily":
		return "*-*-* 03:00:00"
	case "hourly":
		return "hourly"
	case "weekly":
		return "Sun *-*-* 03:00:00"
	}
	return b.Schedule
}

// AppliesTo 这项备份在目标 t 上有没有。
func (b *Backup) AppliesTo(t string) bool {
	if len(b.Targets) == 0 {
		return true
	}
	for _, x := range b.Targets {
		if x == t {
			return true
		}
	}
	return false
}

var (
	reBackupName = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,31}$`)
	reDBName     = regexp.MustCompile(`^[A-Za-z0-9_$-]+$`)
	reDBHost     = regexp.MustCompile(`^[A-Za-z0-9._:-]+$`)
	reCalendar   = regexp.MustCompile(`^[A-Za-z0-9*:/,. -]+$`)
	reEndpoint   = regexp.MustCompile(`^https?://[A-Za-z0-9.-]+(:\d+)?/?$`)
	reBucket     = regexp.MustCompile(`^[a-z0-9][a-z0-9.-]{1,61}[a-z0-9]$`)
	reS3Prefix   = regexp.MustCompile(`^[A-Za-z0-9._/${}-]*$`)
	reRegion     = regexp.MustCompile(`^[a-z0-9-]*$`)
)

func checkBackups(f *File, bad func(string, ...any)) {
	seen := map[string]bool{}
	for i, b := range f.Backups {
		at := "backups[" + itoa(i) + "]"
		if b == nil {
			bad("%s 是空的", at)
			continue
		}
		if b.Name != "" {
			at = "backups." + b.Name
		}
		if !reBackupName.MatchString(b.Name) {
			bad("%s：name 只能是小写字母、数字、横线（当前 %q）", at, b.Name)
		}
		if seen[b.Name] {
			bad("%s：name 重复", at)
		}
		seen[b.Name] = true
		n := 0
		for _, set := range []bool{b.MySQL != nil, b.Postgres != nil, len(b.Paths) > 0, b.Command != ""} {
			if set {
				n++
			}
		}
		if n != 1 {
			bad("%s：mysql、postgres、paths、command 要写一个，且只写一个", at)
		}
		for _, db := range []*DBBackup{b.MySQL, b.Postgres} {
			if db == nil {
				continue
			}
			if db.Database != "*" && !reDBName.MatchString(db.Database) {
				bad("%s：database 要写库名，* 为全部库（当前 %q）", at, db.Database)
			}
			if db.Bin != "" && (!strings.HasPrefix(db.Bin, "/") || strings.ContainsAny(db.Bin, " '\"$`;&|")) {
				bad("%s：bin 要写服务器上的绝对路径（当前 %q）", at, db.Bin)
			}
			if db.Host != "" && !reDBHost.MatchString(db.Host) {
				bad("%s：host 无效 %q", at, db.Host)
			}
			if db.User != "" && !reDBName.MatchString(db.User) {
				bad("%s：user 无效 %q", at, db.User)
			}
			if db.Container != "" && !reDBName.MatchString(strings.ReplaceAll(db.Container, ".", "_")) {
				bad("%s：container 无效 %q", at, db.Container)
			}
			if db.Port < 0 || db.Port > 65535 {
				bad("%s：port 不对", at)
			}
		}
		for _, p := range b.Paths {
			if !strings.HasPrefix(p, "/") && !strings.HasPrefix(p, "${") {
				bad("%s：paths 要写服务器上的绝对路径（当前 %q）", at, p)
			}
		}
		if b.Restore != "" && b.Command == "" {
			bad("%s：restore 只在 command 时用（mysql、postgres、paths 由 Ferry 恢复）", at)
		}
		if b.Schedule != "" && !reCalendar.MatchString(b.Schedule) {
			bad("%s：schedule 写 daily、hourly、weekly 或 systemd 的 OnCalendar（当前 %q）", at, b.Schedule)
		}
		if b.Keep < 0 || b.Keep > 1000 {
			bad("%s：keep 最多 1000", at)
		}
		for _, t := range b.Targets {
			if _, ok := f.Targets[t]; !ok {
				bad("%s.targets：没有目标 %q", at, t)
			}
		}
		if u := b.Upload; u != nil {
			switch {
			case u.S3 == nil:
				bad("%s.upload：要写 s3（传到哪个对象存储）", at)
			case !reEndpoint.MatchString(u.S3.Endpoint) && !strings.Contains(u.S3.Endpoint, "${"):
				bad("%s.upload.s3.endpoint：写成 https://域名 或 http://地址:端口（当前 %q）", at, u.S3.Endpoint)
			case !reBucket.MatchString(u.S3.Bucket) && !strings.Contains(u.S3.Bucket, "${"):
				bad("%s.upload.s3.bucket：3～63 个小写字母、数字、点、横线（当前 %q）", at, u.S3.Bucket)
			case !reS3Prefix.MatchString(u.S3.Prefix) || strings.Contains(u.S3.Prefix, "..") || strings.HasPrefix(u.S3.Prefix, "/"):
				bad("%s.upload.s3.prefix：只能有字母、数字、. _ - /，不以 / 开头（当前 %q）", at, u.S3.Prefix)
			case !reRegion.MatchString(u.S3.Region):
				bad("%s.upload.s3.region 无效 %q", at, u.S3.Region)
			}
			if u.Keep < 0 || u.Keep > 1000 {
				bad("%s.upload.keep：最多 1000", at)
			}
			if len(b.EnvFile) == 0 {
				bad("%s.upload：要写 envFile（服务器上的文件，放对象存储的访问密钥 S3_ACCESS_KEY_ID、S3_SECRET_ACCESS_KEY%s）", at,
					map[bool]string{true: "，和加密的密码 FERRY_BACKUP_PASSWORD", false: ""}[u.EncryptOn()])
			}
		}
	}
}
