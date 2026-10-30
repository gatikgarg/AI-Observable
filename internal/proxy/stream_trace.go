package proxy

import (
	"bytes"
	"encoding/json"
	"time"
)

// streamTrace observes SSE incrementally. It never changes the bytes sent to
// the agent; it only finds terminal events and the first generated-content
// event for TTFT and success accounting.
type streamTrace struct {
	line  []byte
	data  []byte
	sse   bool
	seen  bool
	first time.Time
	end   bool
	fail  bool
}

func (t *streamTrace) Feed(chunk []byte, now time.Time) {
	for len(chunk) > 0 {
		i := bytes.IndexByte(chunk, '\n')
		if i < 0 {
			t.line = append(t.line, chunk...)
			return
		}
		t.line = append(t.line, chunk[:i]...)
		t.chunkLine(now)
		chunk = chunk[i+1:]
	}
}

func (t *streamTrace) Finish(now time.Time) {
	if len(t.line) > 0 {
		t.chunkLine(now)
	}
	t.flush(now)
}

func (t *streamTrace) chunkLine(now time.Time) {
	line := bytes.TrimSuffix(t.line, []byte("\r"))
	t.line = t.line[:0]
	if bytes.HasPrefix(line, []byte("data:")) {
		t.sse = true
		part := bytes.TrimSpace(bytes.TrimPrefix(line, []byte("data:")))
		if len(part) > 0 {
			if len(t.data) > 0 {
				t.data = append(t.data, '\n')
			}
			t.data = append(t.data, part...)
		}
		return
	}
	if len(bytes.TrimSpace(line)) == 0 {
		t.flush(now)
	}
}

func (t *streamTrace) flush(now time.Time) {
	if len(t.data) == 0 {
		return
	}
	payload := bytes.TrimSpace(t.data)
	t.data = t.data[:0]
	if bytes.Equal(payload, []byte("[DONE]")) {
		t.end = true
		return
	}
	var ev struct {
		Type    string `json:"type"`
		Choices []struct {
			Delta struct {
				Content          any               `json:"content"`
				ReasoningContent string            `json:"reasoning_content"`
				ToolCalls        []json.RawMessage `json:"tool_calls"`
				FunctionCall     json.RawMessage   `json:"function_call"`
			} `json:"delta"`
			FinishReason any `json:"finish_reason"`
		} `json:"choices"`
	}
	if json.Unmarshal(payload, &ev) != nil {
		return
	}
	for _, choice := range ev.Choices {
		if !t.seen && (generated(choice.Delta.Content) || choice.Delta.ReasoningContent != "" ||
			len(choice.Delta.ToolCalls) > 0 || meaningfulJSON(choice.Delta.FunctionCall)) {
			t.seen, t.first = true, now
		}
		if choice.FinishReason != nil && choice.FinishReason != "" {
			t.end = true
		}
	}
	switch ev.Type {
	case "response.output_text.delta", "response.output_audio.delta", "response.reasoning_text.delta",
		"response.reasoning_summary_text.delta", "response.function_call_arguments.delta",
		"response.output_item.added":
		if !t.seen {
			t.seen, t.first = true, now
		}
	case "response.completed":
		t.end = true
	case "response.failed", "response.incomplete", "error":
		t.end, t.fail = true, true
	}
}

func generated(v any) bool {
	switch x := v.(type) {
	case string:
		return x != ""
	case []any:
		return len(x) > 0
	case map[string]any:
		return len(x) > 0
	}
	return false
}

func meaningfulJSON(v json.RawMessage) bool {
	x := bytes.TrimSpace(v)
	return len(x) > 0 && !bytes.Equal(x, []byte("null")) && !bytes.Equal(x, []byte(`""`))
}
