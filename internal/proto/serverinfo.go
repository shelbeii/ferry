package proto

// ServerInfo 服务器只读采样结果（常驻 agent 原生采集或纯 SSH 脚本采集共用同一结构）。
type ServerInfo struct {
	Hostname       string        `json:"hostname"`
	OS             string        `json:"os"`
	Kernel         string        `json:"kernel"`
	Arch           string        `json:"arch"`
	Virtualization string        `json:"virtualization"`
	Timezone       string        `json:"timezone"`
	Now            int64         `json:"now"`
	Uptime         float64       `json:"uptime"`
	CPU            ServerCPU     `json:"cpu"`
	Memory         ServerMemory  `json:"memory"`
	Swap           ServerSwap    `json:"swap"`
	Mounts         []ServerMount `json:"mounts"`
	Addresses      []ServerAddr  `json:"addresses"`
	Notes          []string      `json:"notes,omitempty"`
}

type ServerCPU struct {
	Model string `json:"model"`
	Cores int    `json:"cores"` // 逻辑 CPU 数
	// Usage 为两次 /proc/stat 的差值：第一次读时在服务器上间隔约一秒读两次；之后和上次读到的比（这段时间的平均）。nil 表示采样不可用。
	Usage *float64 `json:"usage"`
	// Window Usage 是多少秒内的平均
	Window float64   `json:"window,omitempty"`
	Load   []float64 `json:"load"` // 1 / 5 / 15 分钟平均负载，不是百分比
}

type ServerMemory struct {
	Total     uint64 `json:"total"`
	Used      uint64 `json:"used"` // total - available，缓存不算实际占用
	Available uint64 `json:"available"`
	Free      uint64 `json:"free"`
	Cached    uint64 `json:"cached"` // buffers + cached + SReclaimable - Shmem
}

type ServerSwap struct {
	Total uint64 `json:"total"`
	Used  uint64 `json:"used"`
	Free  uint64 `json:"free"`
}

type ServerMount struct {
	Device      string   `json:"device"`
	Path        string   `json:"path"`
	FSType      string   `json:"fsType"`
	Total       uint64   `json:"total"`
	Used        uint64   `json:"used"`
	Available   uint64   `json:"available"`
	Usage       *float64 `json:"usage"` // df 的使用率，包含文件系统保留空间的影响
	Inodes      uint64   `json:"inodes"`
	InodesUsed  uint64   `json:"inodesUsed"`
	InodesUsage *float64 `json:"inodesUsage"`
	Temporary   bool     `json:"temporary"`
}

type ServerAddr struct {
	Interface string `json:"interface"`
	Family    string `json:"family"`
	Address   string `json:"address"`
}
