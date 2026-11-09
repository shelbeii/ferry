package proto

// 常驻的 agent（ferry-agent serve）：每条连接上开一个，只读的查看都交给它，不用每次新开会话、启动 agent（经 sudo 时还要再走一次 sudo）。
//
// 本机发：一行一个请求 {"id":"7","cmd":"status","req":{…}}（ServeRequest）。
// agent 回：每行前面加请求号和一个空格：「7 <原来一次性运行时输出的那一行>」；这个请求做完时「7 !end <退出码>」。
// 几个请求同时做，输出按行交错。

// ServeCmds 交给常驻 agent 的命令：只读、很快结束的查看。
// 改动服务器的（部署、启停……）仍然每次单独运行：本机取消或连接断开后它们要在服务器上照常做完，
// 不能跟着常驻的 agent 一起结束。
var ServeCmds = map[string]bool{
	"status": true, "appinfo": true, "appruntimes": true, "releases": true, "history": true,
	"logruns": true, "logread": true, "loggrep": true, "software": true, "ports": true,
	"portcheck": true, "keepscan": true, "swinfo": true,
}

// ServeRequest 发给常驻 agent 的一个请求。
type ServeRequest struct {
	ID  string   `json:"id"`
	Cmd string   `json:"cmd"`
	Req *Request `json:"req"`
}

// ServeEnd 请求做完的那一行里，请求号后面的标记。
const ServeEnd = "!end "
