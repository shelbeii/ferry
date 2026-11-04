package proto

import "time"

// AppInfo 是按需采集的运行详情，不随应用列表的自动刷新重复执行。
type AppInfo struct {
	Name        string           `json:"name"`
	Type        string           `json:"type"`
	CollectedAt time.Time        `json:"collectedAt"`
	Status      AppStatus        `json:"status"`
	Sections    []AppInfoSection `json:"sections"`
	Notes       []string         `json:"notes,omitempty"`
}

type AppInfoSection struct {
	ID     string         `json:"id"`
	Title  string         `json:"title"`
	Fields []AppInfoField `json:"fields"`
}

type AppInfoField struct {
	Label string `json:"label"`
	Value string `json:"value"`
	Path  bool   `json:"path,omitempty"` // 可以在服务器文件浏览器中打开的位置。
}
