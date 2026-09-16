package service

import (
	"strconv"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// 部分 DeepSeek 系上游（如 apiko 的 deepseek-v4.1-flash）流式时把思维以
// <think>…</think> 前缀塞进 delta.content，reasoning_content 为空（非流式则拆分正确），
// 客户端于是把思考过程当正文显示。这里在流式路径上按 choice 维持状态，把前缀思维段
// 挪到 reasoning_content。只处理「正文以 <think> 开头」的情况；上游已给
// reasoning_content 时原样透传。

const (
	thinkOpenTag  = "<think>"
	thinkCloseTag = "</think>"
)

type thinkTagPhase int

const (
	thinkTagUndecided   thinkTagPhase = iota // 还没见到非空白正文
	thinkTagInThink                          // 已见 <think>，等 </think>
	thinkTagAfterThink                       // 刚过 </think>，吞掉正文前导空白
	thinkTagPassthrough                      // 不再介入
)

type thinkTagChoiceState struct {
	phase        thinkTagPhase
	buf          string // 暂扣的字节（可能是半截标签或前导空白）
	thinkStarted bool   // 思维段已出现非换行字符
}

type thinkTagSplitter struct {
	choices map[int]*thinkTagChoiceState
}

// newThinkTagSplitter 对非 deepseek 平台返回 nil（nil splitter 的方法都是空操作）。
func newThinkTagSplitter(account *Account) *thinkTagSplitter {
	if account == nil || account.Platform != PlatformDeepseek {
		return nil
	}
	return &thinkTagSplitter{choices: map[int]*thinkTagChoiceState{}}
}

// feed 吃进一段 delta.content，返回应输出的 reasoning 与 content。
func (st *thinkTagChoiceState) feed(in string) (reasoning, content string) {
	st.buf += in
	for {
		switch st.phase {
		case thinkTagUndecided:
			trimmed := strings.TrimLeft(st.buf, " \t\r\n")
			if trimmed == "" || (len(trimmed) < len(thinkOpenTag) && strings.HasPrefix(thinkOpenTag, trimmed)) {
				return reasoning, content
			}
			if !strings.HasPrefix(trimmed, thinkOpenTag) {
				st.phase = thinkTagPassthrough
				continue
			}
			st.phase = thinkTagInThink
			st.buf = trimmed[len(thinkOpenTag):]
		case thinkTagInThink:
			if !st.thinkStarted {
				st.buf = strings.TrimLeft(st.buf, "\r\n")
				if st.buf == "" {
					return reasoning, content
				}
				st.thinkStarted = true
			}
			if i := strings.Index(st.buf, thinkCloseTag); i >= 0 {
				reasoning += st.buf[:i]
				st.buf = st.buf[i+len(thinkCloseTag):]
				st.phase = thinkTagAfterThink
				continue
			}
			keep := partialTagSuffixLen(st.buf, thinkCloseTag)
			reasoning += st.buf[:len(st.buf)-keep]
			st.buf = st.buf[len(st.buf)-keep:]
			return reasoning, content
		case thinkTagAfterThink:
			trimmed := strings.TrimLeft(st.buf, " \t\r\n")
			st.buf = ""
			if trimmed == "" {
				return reasoning, content
			}
			st.phase = thinkTagPassthrough
			return reasoning, content + trimmed
		default:
			content += st.buf
			st.buf = ""
			return reasoning, content
		}
	}
}

// flush 在 choice 结束时吐出暂扣的字节。
func (st *thinkTagChoiceState) flush() (reasoning, content string) {
	buf := st.buf
	st.buf = ""
	switch st.phase {
	case thinkTagInThink:
		return buf, ""
	case thinkTagAfterThink:
		return "", ""
	default:
		return "", buf
	}
}

// partialTagSuffixLen 返回 s 末尾与 tag 前缀重合的最大长度（< len(tag)）。
func partialTagSuffixLen(s, tag string) int {
	for k := len(tag) - 1; k > 0; k-- {
		if strings.HasSuffix(s, tag[:k]) {
			return k
		}
	}
	return 0
}

// split 处理一个 choice 的 delta。changed=false 表示应原样透传。
func (s *thinkTagSplitter) split(index int, content *string, hasReasoning, finished bool) (reasoning, newContent string, changed bool) {
	if s == nil {
		return "", "", false
	}
	st := s.choices[index]
	if st == nil {
		st = &thinkTagChoiceState{}
		s.choices[index] = st
	}
	if hasReasoning && st.phase == thinkTagUndecided {
		st.phase = thinkTagPassthrough
	}
	if st.phase == thinkTagPassthrough && st.buf == "" {
		return "", "", false
	}
	if content != nil {
		reasoning, newContent = st.feed(*content)
	}
	if finished {
		r, c := st.flush()
		reasoning += r
		newContent += c
	}
	if content != nil && reasoning == "" && newContent == *content {
		return "", "", false
	}
	if content == nil && reasoning == "" && newContent == "" {
		return "", "", false
	}
	return reasoning, newContent, true
}

// applyToChunk 改写 CC→Responses 桥接路径上已解析的 chunk。
func (s *thinkTagSplitter) applyToChunk(chunk *apicompat.ChatCompletionsChunk) {
	if s == nil || chunk == nil {
		return
	}
	for i := range chunk.Choices {
		delta := &chunk.Choices[i].Delta
		hasReasoning := (delta.ReasoningContent != nil && *delta.ReasoningContent != "") ||
			(delta.Reasoning != nil && *delta.Reasoning != "")
		reasoning, content, changed := s.split(chunk.Choices[i].Index, delta.Content, hasReasoning, chunk.Choices[i].FinishReason != nil)
		if !changed {
			continue
		}
		if content == "" {
			delta.Content = nil
		} else {
			delta.Content = &content
		}
		if reasoning != "" {
			delta.ReasoningContent = &reasoning
		}
	}
}

// applyToSSELine 改写 raw CC 直转路径上的一行 SSE。
func (s *thinkTagSplitter) applyToSSELine(line string) string {
	if s == nil {
		return line
	}
	payload, ok := extractOpenAISSEDataLine(line)
	if !ok {
		return line
	}
	trimmed := strings.TrimSpace(payload)
	if trimmed == "" || trimmed == "[DONE]" || !gjson.Valid(trimmed) {
		return line
	}
	updated := trimmed
	changed := false
	for i, choice := range gjson.Get(trimmed, "choices").Array() {
		delta := choice.Get("delta")
		var content *string
		if c := delta.Get("content"); c.Type == gjson.String {
			str := c.Str
			content = &str
		}
		_, hasRC := jsonNonEmptyString(delta.Get("reasoning_content"))
		_, hasR := jsonNonEmptyString(delta.Get("reasoning"))
		finishReason := choice.Get("finish_reason")
		finished := finishReason.Exists() && finishReason.Type != gjson.Null
		index := i
		if idx := choice.Get("index"); idx.Exists() {
			index = int(idx.Int())
		}
		reasoning, newContent, ok := s.split(index, content, hasRC || hasR, finished)
		if !ok {
			continue
		}
		prefix := "choices." + strconv.Itoa(i) + ".delta."
		var err error
		if newContent == "" {
			updated, err = sjson.SetRaw(updated, prefix+"content", "null")
		} else {
			updated, err = sjson.Set(updated, prefix+"content", newContent)
		}
		if err != nil {
			return line
		}
		if reasoning != "" {
			if updated, err = sjson.Set(updated, prefix+"reasoning_content", reasoning); err != nil {
				return line
			}
		}
		changed = true
	}
	if !changed {
		return line
	}
	return "data: " + updated
}
