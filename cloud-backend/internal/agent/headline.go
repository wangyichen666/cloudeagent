package agent

// QwenPaw Scroll 上下文系统会在回复中插入"里程碑注释行"：
//   `<!-- ⟦ 一段思考/任务回声 ⟧ -->`（也接受 〚〛 变体，可省略 HTML 注释包裹）。
// QwenPaw 自己的前端通道会通过 strip_headline 剥离这些行，但 ACP 通道不会——
// 这里在 agent-runtime 中继层做同样的事，避免思考内容混入正文。

import (
	"regexp"
	"strings"
)

// headlineLineRE 匹配独占一行的里程碑注释（与 qwenpaw
// agents/context/scroll/serialize.py 的 _HEADLINE_RE 语义一致）。
var headlineLineRE = regexp.MustCompile(
	`(?m)^[ \t]*(?:<!--)?[ \t]*[⟦〚][ \t]*.*?[ \t]*[⟧〛][ \t]*(?:-->)?[ \t]*\n?$`,
)

// stripHeadlineText 从整段文本中移除所有里程碑注释行，并折叠多余空行。
func stripHeadlineText(s string) string {
	if s == "" {
		return s
	}
	cleaned := headlineLineRE.ReplaceAllString(s, "")
	cleaned = regexp.MustCompile(`\n{3,}`).ReplaceAllString(cleaned, "\n\n")
	return strings.TrimSpace(cleaned)
}

func isHeadlineLine(line string) bool {
	return headlineLineRE.MatchString(line)
}

// headlineLineFilter 是流式过滤器：按完整行判断，逐行放行非里程碑内容。
// 避免流式过程中思考注释先漏给用户、结束后才被清理。
type headlineLineFilter struct {
	emit   func(text string)
	buffer strings.Builder
}

func newHeadlineLineFilter(emit func(text string)) *headlineLineFilter {
	return &headlineLineFilter{emit: emit}
}

func (f *headlineLineFilter) Write(s string) {
	for {
		idx := strings.IndexByte(s, '\n')
		if idx < 0 {
			f.buffer.WriteString(s)
			return
		}
		line := s[:idx+1] // 保留换行符
		s = s[idx+1:]
		f.buffer.WriteString(line)
		complete := f.buffer.String()
		f.buffer.Reset()
		if isHeadlineLine(complete) {
			continue
		}
		if f.emit != nil {
			f.emit(complete)
		}
	}
}

// Flush 处理末尾未换行的残段（也是里程碑则丢弃）。
func (f *headlineLineFilter) Flush() {
	if f.buffer.Len() == 0 {
		return
	}
	line := f.buffer.String()
	f.buffer.Reset()
	if isHeadlineLine(line) {
		return
	}
	if f.emit != nil {
		f.emit(line)
	}
}
