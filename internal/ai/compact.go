package ai

import (
	"fmt"
	"strings"
	"unicode/utf8"
)

// 对话越来越长：每次请求都会把前面的内容全发过去，费用变高，超过模型的上限还会报错。
// 发给模型之前做精简（存下来的对话和界面上的不变）：
//   - 最近两轮（两句用户的话以来）的内容原样保留；
//   - 更早的工具输出只留开头一段；更早的回复不再带回思考内容（模型只需要当前这轮的）；更早的图片换成一句说明；
//   - 还是太长就省略最早的几轮。

const (
	keepTurns    = 2          // 最近几轮原样保留
	oldResultMax = 600        // 更早的工具输出保留多少字节
	contextMax   = 240 * 1024 // 发给模型的内容大约不超过这么多字节
)

// compactForModel 返回精简后的副本。
func compactForModel(msgs []Message) []Message {
	// 第 keepTurns 句（从后往前数）用户的话从哪开始
	cut, n := 0, 0
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role == "user" {
			n++
			if n == keepTurns {
				cut = i
				break
			}
		}
	}
	out := make([]Message, len(msgs))
	copy(out, msgs)
	for i := 0; i < cut; i++ {
		m := out[i]
		switch m.Role {
		case "tool":
			if len(m.Text) > oldResultMax {
				m.Text = cutUTF8(m.Text, oldResultMax) + "\n…（早先的输出太长，已省略；需要的话重新查看）"
			}
		case "assistant":
			m.Raw = nil // 不再带回思考内容，用文字和工具调用重建
		case "user":
			if n := len(m.Images); n > 0 {
				m.Images = nil
				m.Text = strings.TrimSpace(fmt.Sprintf("（这里附过 %d 张图片，为了节省已经省略）\n%s", n, m.Text))
			}
		}
		out[i] = m
	}
	// 还是太长：从最早的一轮开始整轮省略（一轮 = 一句用户的话到下一句之前），至少留最近两轮
	for size(out) > contextMax {
		// 最早一轮从第一句（不是省略说明的）用户的话开始，到下一句用户的话之前
		first, next := -1, -1
		for i := 0; i < len(out); i++ {
			if out[i].Role != "user" || out[i].Text == omittedNote {
				continue
			}
			if first < 0 {
				first = i
			} else {
				next = i
				break
			}
		}
		if next < 0 || next >= cutIndex(out) {
			break
		}
		out = append([]Message{{Role: "user", Text: omittedNote}}, out[next:]...)
	}
	return out
}

const omittedNote = "（更早的对话太长，已经省略。需要的信息请重新查看。）"

func cutIndex(msgs []Message) int {
	n := 0
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role == "user" {
			n++
			if n == keepTurns {
				return i
			}
		}
	}
	return 0
}

func size(msgs []Message) int {
	n := 0
	for _, m := range msgs {
		n += len(m.Text) + len(m.Raw)
		for _, c := range m.Calls {
			n += len(c.Input)
		}
	}
	return n
}

func cutUTF8(s string, n int) string {
	if len(s) <= n {
		return s
	}
	s = s[:n]
	for !utf8.ValidString(s) && len(s) > 0 {
		s = s[:len(s)-1]
	}
	return s
}
