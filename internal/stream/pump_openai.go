package stream

import (
	"bufio"
	"encoding/json"
	"io"
	"strings"
)

// oaiChunk is the minimal slice of an OpenAI chat.completion.chunk we consume.
type oaiChunk struct {
	Choices []struct {
		Delta struct {
			Content          string `json:"content"`
			Reasoning        string `json:"reasoning"`
			ReasoningContent string `json:"reasoning_content"`
			ToolCalls        []struct {
				Index    int    `json:"index"`
				ID       string `json:"id"`
				Function struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
		} `json:"delta"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Usage *struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
		TotalTokens      int `json:"total_tokens"`
	} `json:"usage"`
}

// PumpOpenAI reads an upstream OpenAI-compatible SSE stream and drives the
// Encoder. Used by the plain-model (no-agent) proxy path so a direct model chat
// produces the same wire format as an agent run. It owns the full lifecycle
// (RunStarted → content → RunFinished).
func PumpOpenAI(body io.Reader, enc Encoder, model string) {
	enc.RunStarted()
	enc.Metadata(model, "", 0)

	type toolAcc struct {
		id, name, args string
	}
	tools := map[int]*toolAcc{}
	var usage Usage
	flushTools := func() {
		for i := 0; i < len(tools); i++ {
			tc := tools[i]
			if tc == nil {
				continue
			}
			enc.ToolCall(int64(i), tc.id, tc.name, tc.args)
		}
		tools = map[int]*toolAcc{}
	}

	sc := bufio.NewScanner(body)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(line[5:])
		if payload == "" || payload == "[DONE]" {
			continue
		}
		var c oaiChunk
		if err := json.Unmarshal([]byte(payload), &c); err != nil {
			continue
		}
		if c.Usage != nil && c.Usage.TotalTokens > 0 {
			usage.Prompt = c.Usage.PromptTokens
			usage.Completion = c.Usage.CompletionTokens
			usage.Total = c.Usage.TotalTokens
		}
		for _, ch := range c.Choices {
			d := ch.Delta
			if r := d.Reasoning; r != "" {
				enc.Reasoning(r)
			} else if r := d.ReasoningContent; r != "" {
				enc.Reasoning(r)
			}
			if d.Content != "" {
				enc.Text(d.Content)
			}
			for _, tc := range d.ToolCalls {
				acc := tools[tc.Index]
				if acc == nil {
					acc = &toolAcc{}
					tools[tc.Index] = acc
				}
				if tc.ID != "" {
					acc.id = tc.ID
				}
				if tc.Function.Name != "" {
					acc.name = tc.Function.Name
				}
				acc.args += tc.Function.Arguments
			}
			if ch.FinishReason == "tool_calls" {
				flushTools()
			}
		}
	}
	flushTools()
	enc.Usage(usage, []Bucket{
		{Label: "History", Tokens: usage.Prompt},
		{Label: "Completion", Tokens: usage.Completion},
	})
	enc.Metadata(model, "", 0)
	enc.RunFinished(usage)
}
