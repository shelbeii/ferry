// Package settings ferry 的应用设置（数据目录下的 settings.json），命令行与桌面版共用。
package settings

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/captain/ferry/internal/appdir"
)

// Settings 全部设置。字段为零值时用默认值（见 Defaults）。
type Settings struct {
	Connect Connect `json:"connect"`
	Build   Build   `json:"build"`
	Logs    Logs    `json:"logs"`
	Deploy  Deploy  `json:"deploy"`
	Runtime Runtime `json:"runtime"`
	UI      UI      `json:"ui"`
	AI      AI      `json:"ai"`
	MCP     MCP     `json:"mcp"`
	Notify  Notify  `json:"notify"`
	Desktop Desktop `json:"desktop"`
	Team    Team    `json:"team"`
}

// Team 团队协作。
type Team struct {
	// Name 你的名字（如 张三）：部署记录、「正在部署」、通知里显示，部署名单（deployers）按它认人；空 = 只显示 用户@电脑名
	Name string `json:"name"`
}

// Notify 部署通知：部署、回滚结束后发到企业微信、钉钉、飞书的群机器人，或自己的接口。
// 机器人地址（含令牌）和加签密钥存在系统凭据库（ferry notify secret），不在这里。
type Notify struct {
	Channels []NotifyChannel `json:"channels"`
}

// NotifyChannel 一个发通知的地方。
type NotifyChannel struct {
	ID   string `json:"id"`   // 小写字母、数字、横线；凭据账户按它区分
	Name string `json:"name"` // 显示名，如「后端群」
	Kind string `json:"kind"` // wecom | dingtalk | feishu | webhook
	// On 什么时候发：success（成功）、failure（失败）；空 = 都发
	On []string `json:"on"`
	// Scope 只发这些项目的（ferry.yaml 的 project，或「项目/目标」）；空 = 全部
	Scope []string `json:"scope"`
	// ProductionOnly 只发生产服务器（production: true）的
	ProductionOnly bool `json:"productionOnly"`
	Off            bool `json:"off"` // 暂停
}

// NotifyKinds 支持的通知方式。
var NotifyKinds = map[string]string{"wecom": "企业微信", "dingtalk": "钉钉", "feishu": "飞书", "webhook": "自定义接口"}

// MCP 编程软件通过 ferry mcp 使用 Ferry。
type MCP struct {
	// AllowChanges 允许编程软件请求部署、启停、回滚。每次都要在 Ferry 桌面版里确认；
	// 生产服务器（production: true）和 aiChanges: false 的服务器始终不允许
	AllowChanges bool `json:"allowChanges"`
	// Targets 只允许这些目标（「项目/目标」，项目是 ferry.yaml 里的 project）；为空 = 全部非生产目标
	Targets []string `json:"targets"`
	// 调用记录（数据目录的 mcp/calls.jsonl）：保留多少天、最多占用多少 MB（超出时从最旧的删起）
	LogKeepDays int `json:"logKeepDays"` // 默认 30
	LogMaxMB    int `json:"logMaxMB"`    // 默认 20
}

// AI 服务器助手。可以配置多个模型，对话时选用；各模型的 API Key 存在系统凭据库（ferry ai-key set --model <ID>），不在这里。
type AI struct {
	Models  []AIModel `json:"models"`
	Default string    `json:"default"` // 默认用哪个模型（ID）
	// 旧版本只有一个模型：读到时转成 Models 里的一个（ID 为 LegacyModelID），保存时不再写
	Provider string `json:"provider,omitempty"`
	BaseURL  string `json:"baseUrl,omitempty"`
	Model    string `json:"model,omitempty"`
	// 对话记录：保留多少天（按最后一次对话算）、最多占用多少 MB（超出时从最旧的删起）
	KeepDays int `json:"keepDays"` // 默认 30
	MaxMB    int `json:"maxMB"`    // 默认 200
}

// AIModel 一个模型的配置。
type AIModel struct {
	ID       string `json:"id"`       // 小写字母、数字、横线；API Key 的凭据账户按它区分
	Name     string `json:"name"`     // 显示名，如「DeepSeek」
	Provider string `json:"provider"` // anthropic | openai（OpenAI 兼容接口）
	BaseURL  string `json:"baseUrl"`  // 空 = 服务商默认地址（OpenAI 兼容接口必填）
	Model    string `json:"model"`    // 模型名；Anthropic 空 = claude-opus-5-5
	// Effort 默认的思考强度：空 = 不设置（用模型自己的默认）| off | on | low | medium | high | xhigh | max。对话时可以另选
	Effort string `json:"effort"`
	// Thinking OpenAI 兼容接口怎么传思考设置：空 = 按接口地址自动识别 | effort（reasoning_effort）| deepseek（thinking）
	// | qwen（enable_thinking）| none（不传）
	Thinking string `json:"thinking"`
	// Images 能不能看图片：空 = 按服务商和模型名识别 | on | off
	Images string `json:"images"`
	// 单价（每百万 tokens，可以不填）：输入、命中缓存的输入、输出；Currency 为 ¥ 或 $
	PriceIn    float64 `json:"priceIn"`
	PriceCache float64 `json:"priceCache"`
	PriceOut   float64 `json:"priceOut"`
	Currency   string  `json:"currency"`
}

// LegacyModelID 从旧版本单个模型配置转过来的模型 ID（它的 API Key 在旧的凭据账户里）。
const LegacyModelID = "default"

var (
	reModelID = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,31}$`)
	efforts   = map[string]bool{"": true, "off": true, "on": true, "low": true, "medium": true, "high": true, "xhigh": true, "max": true}
	thinkings = map[string]bool{"": true, "effort": true, "deepseek": true, "qwen": true, "none": true}
)

// FindModel 按 ID 找模型；id 为空时用默认模型（没有默认时用第一个）。
func (a AI) FindModel(id string) (AIModel, bool) {
	if id == "" {
		id = a.Default
	}
	for _, m := range a.Models {
		if m.ID == id {
			return m, true
		}
	}
	if id == a.Default && len(a.Models) > 0 {
		return a.Models[0], true
	}
	return AIModel{}, false
}

// Connect 连接。
type Connect struct {
	TimeoutSec       int    `json:"timeoutSec"`       // SSH 连接超时，默认 15
	AcceptNewHostKey bool   `json:"acceptNewHostKey"` // 首次连接自动记录主机公钥（不推荐）
	KeyDir           string `json:"keyDir"`           // keygen 新密钥的默认目录，空 = 数据目录/keys
	UploadStreams    int    `json:"uploadStreams"`    // 上传并行连接数，默认 6（目标里的 uploadStreams 优先）
	IdleMinutes      int    `json:"idleMinutes"`      // 桌面版保持的连接空闲多久后断开（分钟），默认 15，最多 1440
	// StatusMinutes 桌面版连着服务器时自动读取应用状态的间隔（分钟），默认 1，最多 60；StatusOff（-1）= 不自动读取，只手动刷新
	StatusMinutes int `json:"statusMinutes"`
}

// StatusOff Connect.StatusMinutes 取这个值时关闭自动读取应用状态。
const StatusOff = -1

// Build 构建环境。桌面版从开始菜单启动时拿不到终端里配置的环境变量，在这里补上。
type Build struct {
	Env     map[string]string `json:"env"`     // 额外的环境变量（JAVA_HOME、MAVEN_HOME……）
	Path    []string          `json:"path"`    // 加到 PATH 最前面的目录
	Verbose bool              `json:"verbose"` // 实时显示构建输出
}

// Logs 日志。
type Logs struct {
	BuildLogDays int `json:"buildLogDays"` // 构建日志保留天数，默认 30；0 以下 = 不清理
}

// Deploy 桌面版部署对话框的默认选项。
type Deploy struct {
	SkipBuild bool `json:"skipBuild"`
	Full      bool `json:"full"`
	// KeepPerms 部署时不自动修正权限（默认修正：运行用户访问不了的目录 / 文件、不安全的部署目录）
	KeepPerms bool `json:"keepPerms"`
}

// Runtime Ferry 提供的运行环境（JDK）。
type Runtime struct {
	// Source 下载源：Adoptium 兼容的 API 地址（默认 https://api.adoptium.net），
	// 或含 {java}、{arch} 的镜像地址（以 / 结尾为目录列表，否则为压缩包本身）
	Source string `json:"source"`
	// CacheDir 本机存放下载的 JDK 的目录；空 = 数据目录下的 runtimes
	CacheDir string `json:"cacheDir"`
	// NginxSource nginx 的下载源：nginx-binaries 格式（目录下有 index.json）；空 = https://jirutka.github.io/nginx-binaries
	NginxSource string `json:"nginxSource"`
	// GitHubMirror 从 GitHub 下载软件（Nacos、Sentinel）时加在地址前面的加速前缀，如 https://ghfast.top/；空 = 直接下载
	GitHubMirror string `json:"githubMirror"`
	// MySQLSource MySQL 的下载源（目录结构同 https://cdn.mysql.com）；空 = 官方
	MySQLSource string `json:"mysqlSource"`
}

// UI 桌面版界面。
type UI struct {
	Theme    string `json:"theme"`    // light | dark | system（默认）
	TaskKeep int    `json:"taskKeep"` // 任务面板最多保留的已完成任务，默认 50
	// 任务记录（数据目录的 tasks/，重启桌面版后还在）：保留多少天、最多占用多少 MB（超出时从最旧的删起）
	TaskKeepDays int  `json:"taskKeepDays"` // 默认 30
	TaskMaxMB    int  `json:"taskMaxMB"`    // 默认 100
	ConfirmClose bool `json:"confirmClose"` // 有任务进行中时关闭窗口要确认（默认 true，见 Defaults）
	// Notify 窗口不在前台时发系统通知：任务结束（部署、启停等）、编程软件请求改动服务器（默认 true）
	Notify bool `json:"notify"`
	// Glass 液态玻璃效果：项目页顶部按钮、功能切换、AI 助手的标题栏和输入框用半透明玻璃材质和动效（默认 true）；关掉是普通的实心样式
	Glass bool `json:"glass"`
	// GlassTransparency 玻璃的透明度 0～100（默认 90，见 DefaultGlassTransparency）：越大越透明（列表从工具栏下面滑过时看得越清楚），越小磨砂越重、越不透
	GlassTransparency int `json:"glassTransparency"`
}

// Desktop 桌面版的项目列表。
type Desktop struct {
	Projects []Project `json:"projects"`
	Current  string    `json:"current"`
}

// Project 桌面版里添加的一个项目。
type Project struct {
	Path string `json:"path"`
	Name string `json:"name"`
}

// DefaultGlassTransparency 新装时玻璃的透明度：比较通透，列表从工具栏下面滑过时看得见后面的字（用户在设置里拖到别的值后按用户的）
const DefaultGlassTransparency = 90

// Defaults 默认设置。
func Defaults() Settings {
	return Settings{
		Connect: Connect{TimeoutSec: 15, UploadStreams: 6, IdleMinutes: 15, StatusMinutes: 1},
		Logs:    Logs{BuildLogDays: 30},
		UI:      UI{Theme: "system", TaskKeep: 50, TaskKeepDays: 30, TaskMaxMB: 100, ConfirmClose: true, Notify: true, Glass: true, GlassTransparency: DefaultGlassTransparency},
	}
}

// Load 读取设置；文件不存在时返回默认值。
func Load() (Settings, error) {
	s := Defaults()
	b, err := os.ReadFile(appdir.SettingsFile())
	if errors.Is(err, os.ErrNotExist) {
		s.normalize() // 补上 AI 对话、任务记录的保留设置等默认值
		return s, nil
	}
	if err != nil {
		return s, err
	}
	if err := json.Unmarshal(b, &s); err != nil {
		return Defaults(), errors.New("设置文件 " + appdir.SettingsFile() + " 格式错误：" + err.Error())
	}
	s.normalize()
	return s, nil
}

func (s *Settings) normalize() {
	if s.Connect.TimeoutSec <= 0 {
		s.Connect.TimeoutSec = 15
	}
	if s.Connect.UploadStreams <= 0 {
		s.Connect.UploadStreams = 6
	}
	if s.Connect.UploadStreams > 32 {
		s.Connect.UploadStreams = 32
	}
	if s.Connect.IdleMinutes <= 0 {
		s.Connect.IdleMinutes = 15
	}
	if s.Connect.IdleMinutes > 1440 {
		s.Connect.IdleMinutes = 1440
	}
	switch {
	case s.Connect.StatusMinutes == 0: // 旧的设置文件没有这一项
		s.Connect.StatusMinutes = 1
	case s.Connect.StatusMinutes < 0:
		s.Connect.StatusMinutes = StatusOff
	case s.Connect.StatusMinutes > 60:
		s.Connect.StatusMinutes = 60
	}
	if s.UI.TaskKeep <= 0 {
		s.UI.TaskKeep = 50
	}
	// 0 是合法值（最不透明）：旧的设置文件没有这一项时，Load 先填的默认值会留着
	s.UI.GlassTransparency = max(0, min(s.UI.GlassTransparency, 100))
	s.UI.TaskKeep = min(s.UI.TaskKeep, 500)
	if s.UI.TaskKeepDays <= 0 {
		s.UI.TaskKeepDays = 30
	}
	s.UI.TaskKeepDays = min(s.UI.TaskKeepDays, 3650)
	if s.UI.TaskMaxMB <= 0 {
		s.UI.TaskMaxMB = 100
	}
	s.UI.TaskMaxMB = min(max(s.UI.TaskMaxMB, 10), 100*1024)
	switch s.UI.Theme {
	case "light", "dark", "system":
	default:
		s.UI.Theme = "system"
	}
	if len(s.AI.Models) == 0 && (s.AI.Provider != "" || s.AI.BaseURL != "" || s.AI.Model != "") {
		// 旧版本的单个模型
		name := strings.TrimSpace(s.AI.Model)
		if name == "" {
			name = map[bool]string{true: "OpenAI 兼容接口", false: "Claude"}[s.AI.Provider == "openai"]
		}
		s.AI.Models = []AIModel{{ID: LegacyModelID, Name: name, Provider: s.AI.Provider, BaseURL: s.AI.BaseURL, Model: s.AI.Model}}
		s.AI.Default = LegacyModelID
	}
	s.AI.Provider, s.AI.BaseURL, s.AI.Model = "", "", ""
	seen := map[string]bool{}
	models := []AIModel{} // 没有模型时也是空列表（不是 null）：设置界面直接读 models.length
	for i, m := range s.AI.Models {
		m.ID = strings.ToLower(strings.TrimSpace(m.ID))
		if !reModelID.MatchString(m.ID) || seen[m.ID] {
			m.ID = fmt.Sprintf("m%d", i+1)
			for seen[m.ID] {
				m.ID += "x"
			}
		}
		seen[m.ID] = true
		if m.Provider != "openai" {
			m.Provider = "anthropic"
		}
		m.Name, m.BaseURL, m.Model = strings.TrimSpace(m.Name), strings.TrimSpace(m.BaseURL), strings.TrimSpace(m.Model)
		if m.Name == "" {
			m.Name = m.Model
		}
		if m.Name == "" {
			m.Name = m.ID
		}
		if !efforts[m.Effort] {
			m.Effort = ""
		}
		if !thinkings[m.Thinking] || m.Provider != "openai" {
			m.Thinking = ""
		}
		if m.Images != "on" && m.Images != "off" {
			m.Images = ""
		}
		m.PriceIn, m.PriceCache, m.PriceOut = max(m.PriceIn, 0), max(m.PriceCache, 0), max(m.PriceOut, 0)
		if m.Currency != "$" {
			m.Currency = "¥"
		}
		models = append(models, m)
	}
	s.AI.Models = models
	if _, ok := s.AI.FindModel(s.AI.Default); !ok || !seen[s.AI.Default] {
		s.AI.Default = ""
		if len(models) > 0 {
			s.AI.Default = models[0].ID
		}
	}
	if s.AI.KeepDays <= 0 {
		s.AI.KeepDays = 30
	}
	s.AI.KeepDays = min(s.AI.KeepDays, 3650)
	if s.AI.MaxMB <= 0 {
		s.AI.MaxMB = 200
	}
	s.AI.MaxMB = min(max(s.AI.MaxMB, 10), 100*1024)
	if s.MCP.LogKeepDays <= 0 {
		s.MCP.LogKeepDays = 30
	}
	s.MCP.LogKeepDays = min(s.MCP.LogKeepDays, 3650)
	if s.MCP.LogMaxMB <= 0 {
		s.MCP.LogMaxMB = 20
	}
	s.MCP.LogMaxMB = min(s.MCP.LogMaxMB, 1024)
	chans := []NotifyChannel{} // 没有时也是空列表（不是 null）
	ids := map[string]bool{}
	for i, c := range s.Notify.Channels {
		c.ID = strings.ToLower(strings.TrimSpace(c.ID))
		if !reModelID.MatchString(c.ID) || ids[c.ID] {
			c.ID = fmt.Sprintf("n%d", i+1)
			for ids[c.ID] {
				c.ID += "x"
			}
		}
		ids[c.ID] = true
		if _, ok := NotifyKinds[c.Kind]; !ok {
			c.Kind = "webhook"
		}
		c.Name = strings.TrimSpace(c.Name)
		if c.Name == "" {
			c.Name = NotifyKinds[c.Kind]
		}
		var on []string
		for _, o := range c.On {
			if (o == "success" || o == "failure") && !slices.Contains(on, o) {
				on = append(on, o)
			}
		}
		c.On = on
		var scope []string
		for _, p := range c.Scope {
			if p = strings.TrimSpace(p); p != "" && !slices.Contains(scope, p) {
				scope = append(scope, p)
			}
		}
		c.Scope = scope
		chans = append(chans, c)
	}
	s.Notify.Channels = chans
}

// Save 保存设置。
func Save(s Settings) error {
	s.normalize()
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	return appdir.WriteFileAtomic(appdir.SettingsFile(), append(b, '\n'), 0o600)
}

// Timeout 连接超时。
func (s Settings) Timeout() time.Duration { return time.Duration(s.Connect.TimeoutSec) * time.Second }

// IdleTimeout 常驻连接的空闲断开时间。环境变量 FERRY_POOL_IDLE_SECONDS 可以覆盖（测试用）。
func (s Settings) IdleTimeout() time.Duration {
	if v, err := strconv.Atoi(os.Getenv("FERRY_POOL_IDLE_SECONDS")); err == nil && v > 0 {
		return time.Duration(v) * time.Second
	}
	return time.Duration(s.Connect.IdleMinutes) * time.Minute
}

// KeyDir keygen 新密钥的默认目录。
// RuntimeDir 本机存放 JDK 的目录。
func (s Settings) RuntimeDir() string {
	if s.Runtime.CacheDir != "" {
		os.MkdirAll(s.Runtime.CacheDir, 0o755)
		return s.Runtime.CacheDir
	}
	return appdir.SubDir("runtimes")
}

// NginxDir 本机存放 nginx 的目录（JDK 目录下的 nginx）。
// TemplateDir 自己的软件模板（数据目录下的 software-templates）。
func (s Settings) TemplateDir() string { return appdir.SubDir("software-templates") }

// SoftwareDir 本机存放下载的软件安装包（Nacos、Sentinel、MySQL）的目录。
func (s Settings) SoftwareDir() string {
	d := filepath.Join(s.RuntimeDir(), "software")
	os.MkdirAll(d, 0o755)
	return d
}

func (s Settings) NginxDir() string {
	d := filepath.Join(s.RuntimeDir(), "nginx")
	os.MkdirAll(d, 0o755)
	return d
}

func (s Settings) KeyDir() string {
	if s.Connect.KeyDir != "" {
		return s.Connect.KeyDir
	}
	return appdir.KeysDir()
}

// BuildEnv 构建时追加的环境变量（KEY=VALUE）。PATH 合并到现有 PATH 前面。
func (s Settings) BuildEnv() []string {
	var out []string
	keys := make([]string, 0, len(s.Build.Env))
	for k := range s.Build.Env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if strings.EqualFold(k, "PATH") {
			continue
		}
		out = append(out, k+"="+s.Build.Env[k])
	}
	if len(s.Build.Path) > 0 {
		out = append(out, "PATH="+strings.Join(append(append([]string{}, s.Build.Path...), os.Getenv("PATH")), string(os.PathListSeparator)))
	}
	return out
}

// CleanBuildLogs 删除 dir 下超过保留天数的构建日志。
func (s Settings) CleanBuildLogs(dir string) {
	days := s.Logs.BuildLogDays
	if days <= 0 {
		return
	}
	cut := time.Now().AddDate(0, 0, -days)
	ents, _ := os.ReadDir(dir)
	for _, e := range ents {
		if !strings.HasPrefix(e.Name(), "build-") || !strings.HasSuffix(e.Name(), ".log") {
			continue
		}
		if fi, err := e.Info(); err == nil && fi.ModTime().Before(cut) {
			os.Remove(filepath.Join(dir, e.Name()))
		}
	}
}
