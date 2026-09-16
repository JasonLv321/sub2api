//go:build unit

package service

import (
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func thinkTagTestSplitter() *thinkTagSplitter {
	return newThinkTagSplitter(&Account{Platform: PlatformDeepseek})
}

// runThinkTagPieces 逐片喂 content，最后一片带 finish，返回拼接后的 reasoning / content。
func runThinkTagPieces(s *thinkTagSplitter, pieces []string) (string, string) {
	var reasoning, content strings.Builder
	for i, p := range pieces {
		p := p
		r, c, changed := s.split(0, &p, false, i == len(pieces)-1)
		if !changed {
			c = p
		}
		reasoning.WriteString(r)
		content.WriteString(c)
	}
	return reasoning.String(), content.String()
}

func TestThinkTagSplitter(t *testing.T) {
	t.Parallel()

	full := "<think>\n想一想 a<b 的情况。\n</think>\n最终答案。"

	t.Run("whole string", func(t *testing.T) {
		t.Parallel()
		r, c := runThinkTagPieces(thinkTagTestSplitter(), []string{full})
		require.Equal(t, "想一想 a<b 的情况。\n", r)
		require.Equal(t, "最终答案。", c)
	})

	t.Run("split at every byte", func(t *testing.T) {
		t.Parallel()
		var pieces []string
		for i := 0; i < len(full); i++ {
			pieces = append(pieces, full[i:i+1])
		}
		r, c := runThinkTagPieces(thinkTagTestSplitter(), pieces)
		require.Equal(t, "想一想 a<b 的情况。\n", r)
		require.Equal(t, "最终答案。", c)
	})

	t.Run("no think prefix passes through", func(t *testing.T) {
		t.Parallel()
		r, c := runThinkTagPieces(thinkTagTestSplitter(), []string{"答案里提到 ", "<think>", " 标签"})
		require.Empty(t, r)
		require.Equal(t, "答案里提到 <think> 标签", c)
	})

	t.Run("partial open tag that is not a tag", func(t *testing.T) {
		t.Parallel()
		r, c := runThinkTagPieces(thinkTagTestSplitter(), []string{"<th", "ing>"})
		require.Empty(t, r)
		require.Equal(t, "<thing>", c)
	})

	t.Run("unterminated think flushes as reasoning", func(t *testing.T) {
		t.Parallel()
		r, c := runThinkTagPieces(thinkTagTestSplitter(), []string{"<think>还没想完</thi"})
		require.Equal(t, "还没想完</thi", r)
		require.Empty(t, c)
	})

	t.Run("upstream reasoning_content disables splitting", func(t *testing.T) {
		t.Parallel()
		s := thinkTagTestSplitter()
		_, _, changed := s.split(0, nil, true, false)
		require.False(t, changed)
		content := "<think>x</think>y"
		_, _, changed = s.split(0, &content, false, true)
		require.False(t, changed)
	})

	t.Run("choices are tracked independently", func(t *testing.T) {
		t.Parallel()
		s := thinkTagTestSplitter()
		a, b := "<think>r0</think>c0", "plain"
		r, c, changed := s.split(0, &a, false, false)
		require.True(t, changed)
		require.Equal(t, "r0", r)
		require.Equal(t, "c0", c)
		_, _, changed = s.split(1, &b, false, false)
		require.False(t, changed)
	})

	t.Run("non deepseek account is a no-op", func(t *testing.T) {
		t.Parallel()
		s := newThinkTagSplitter(&Account{Platform: PlatformOpenAI})
		require.Nil(t, s)
		line := `data: {"choices":[{"index":0,"delta":{"content":"<think>x"}}]}`
		require.Equal(t, line, s.applyToSSELine(line))
		chunk := &apicompat.ChatCompletionsChunk{}
		s.applyToChunk(chunk)
	})
}

func TestThinkTagSplitterSSELine(t *testing.T) {
	t.Parallel()

	s := thinkTagTestSplitter()
	lines := []string{
		`data: {"id":"1","choices":[{"index":0,"delta":{"role":"assistant","content":"<think>\n想"}}]}`,
		`data: {"id":"1","choices":[{"index":0,"delta":{"content":"一想</th"}}]}`,
		`data: {"id":"1","choices":[{"index":0,"delta":{"content":"ink>\n答案"}}]}`,
		`data: {"id":"1","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
		`data: {"id":"1","choices":[],"usage":{"prompt_tokens":3}}`,
		`data: [DONE]`,
		``,
	}
	var reasoning, content strings.Builder
	var out []string
	for _, l := range lines {
		o := s.applyToSSELine(l)
		out = append(out, o)
		payload, ok := extractOpenAISSEDataLine(o)
		if !ok || strings.TrimSpace(payload) == "[DONE]" {
			continue
		}
		reasoning.WriteString(gjson.Get(payload, "choices.0.delta.reasoning_content").String())
		content.WriteString(gjson.Get(payload, "choices.0.delta.content").String())
		require.NotContains(t, gjson.Get(payload, "choices.0.delta.content").String(), "think>")
	}
	require.Equal(t, "想一想", reasoning.String())
	require.Equal(t, "答案", content.String())
	require.Equal(t, "assistant", gjson.Get(strings.TrimPrefix(out[0], "data: "), "choices.0.delta.role").String())
	require.Equal(t, lines[3], out[3])
	require.Equal(t, lines[4], out[4])
	require.Equal(t, lines[5], out[5])
	require.Equal(t, lines[6], out[6])
}

func TestThinkTagSplitterChunk(t *testing.T) {
	t.Parallel()

	s := thinkTagTestSplitter()
	stop := "stop"
	p1, p2 := "<think>推理", "</think>结论"
	c1 := &apicompat.ChatCompletionsChunk{Choices: []apicompat.ChatChunkChoice{{Delta: apicompat.ChatDelta{Content: &p1}}}}
	c2 := &apicompat.ChatCompletionsChunk{Choices: []apicompat.ChatChunkChoice{{Delta: apicompat.ChatDelta{Content: &p2}, FinishReason: &stop}}}
	s.applyToChunk(c1)
	s.applyToChunk(c2)

	require.Nil(t, c1.Choices[0].Delta.Content)
	require.Equal(t, "推理", *c1.Choices[0].Delta.ReasoningContent)
	require.Nil(t, c2.Choices[0].Delta.ReasoningContent)
	require.Equal(t, "结论", *c2.Choices[0].Delta.Content)
}
