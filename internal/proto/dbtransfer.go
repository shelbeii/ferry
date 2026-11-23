package proto

// DBTransfer travels over the SSH command's standard input. Passwords never
// appear in command arguments, task history, progress events or results.
type DBTransfer struct {
	Plan             bool              `json:"plan"`
	Engine           string            `json:"engine"`
	Scope            string            `json:"scope"` // tables, database, instance
	Source           DBEndpoint        `json:"source"`
	Target           DBEndpoint        `json:"target"`
	Tables           []DBTransferTable `json:"tables,omitempty"`
	Threads          int               `json:"threads,omitempty"`
	Directory        string            `json:"directory,omitempty"`
	SourceSSH        string            `json:"sourceSsh,omitempty"`        // MariaDB physical backup, configured on the worker
	AllowLocalInfile bool              `json:"allowLocalInfile,omitempty"` // Temporarily enable on the MySQL target during the run.
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

type DBTransferEvent struct {
	Kind    string             `json:"kind"`
	Text    string             `json:"text,omitempty"`
	Result  *DBTransferResult  `json:"result,omitempty"`
	Problem *DBTransferProblem `json:"problem,omitempty"`
}

type DBTransferProblem struct {
	Message              string   `json:"-"`
	Code                 string   `json:"code"`
	Install              []string `json:"install,omitempty"`
	CanEnableLocalInfile bool     `json:"canEnableLocalInfile,omitempty"`
}

func (p *DBTransferProblem) Error() string { return p.Message }

func (p *DBTransferProblem) JSONDetail() map[string]any {
	return map[string]any{"kind": "db-native-problem", "nativeProblem": p}
}
