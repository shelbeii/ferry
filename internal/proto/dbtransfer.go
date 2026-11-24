package proto

// DBTransfer travels over the SSH command's standard input. Passwords never
// appear in command arguments, task history, progress events or results.
type DBTransfer struct {
	Plan              bool              `json:"plan"`
	Engine            string            `json:"engine"`
	Scope             string            `json:"scope"` // tables, database, instance
	Source            DBEndpoint        `json:"source"`
	Target            DBEndpoint        `json:"target"`
	Tables            []DBTransferTable `json:"tables,omitempty"`
	Threads           int               `json:"threads,omitempty"`
	Directory         string            `json:"directory,omitempty"`
	TempDir           string            `json:"tempDir,omitempty"`           // 逻辑迁移落盘临时目录（mydumper / pg_dump）；空 = 系统默认临时目录
	SourceSSH         string            `json:"sourceSsh,omitempty"`         // MariaDB physical backup, configured on the worker
	AllowLocalInfile  bool              `json:"allowLocalInfile,omitempty"`  // Temporarily enable on the MySQL target during the run.
	AllowDumpFallback bool              `json:"allowDumpFallback,omitempty"` // 缺少并行工具时允许降级为 mysqldump | mysql 流式迁移
	CreateTargetDB    bool              `json:"createTargetDb,omitempty"`    // 目标库不存在时自动创建
}

type DBEndpoint struct {
	Host     string `json:"host"`
	Port     int    `json:"port"`
	User     string `json:"user"`
	Password string `json:"password"`
	Database string `json:"database,omitempty"`
	TLS      string `json:"tls,omitempty"`
}

type DBTransferTable struct {
	Schema string `json:"schema,omitempty"`
	Name   string `json:"name"`
}

type DBTransferResult struct {
	Method        string   `json:"method"`
	SourceVersion string   `json:"sourceVersion"`
	TargetVersion string   `json:"targetVersion,omitempty"`
	Tables        int      `json:"tables"`
	Directory     string   `json:"directory,omitempty"`
	Warnings      []string `json:"warnings,omitempty"`
	ElapsedMs     int64    `json:"elapsedMs,omitempty"`
}

type DBTransferProgress struct {
	Label string `json:"label"`
	Done  int64  `json:"done"`
	Total int64  `json:"total"`
}

type DBTransferEvent struct {
	Kind     string              `json:"kind"`
	Text     string              `json:"text,omitempty"`
	Progress *DBTransferProgress `json:"progress,omitempty"`
	Result   *DBTransferResult   `json:"result,omitempty"`
	Problem  *DBTransferProblem  `json:"problem,omitempty"`
}

type DBTransferProblem struct {
	Message              string   `json:"-"`
	Code                 string   `json:"code"`
	Install              []string `json:"install,omitempty"`
	CanEnableLocalInfile bool     `json:"canEnableLocalInfile,omitempty"`
	CanUseDumpStream     bool     `json:"canUseDumpStream,omitempty"`
	CanCreateTargetDB    bool     `json:"canCreateTargetDb,omitempty"`
}

func (p *DBTransferProblem) Error() string { return p.Message }

func (p *DBTransferProblem) JSONDetail() map[string]any {
	return map[string]any{"kind": "db-native-problem", "nativeProblem": p}
}
