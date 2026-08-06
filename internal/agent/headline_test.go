package agent

import "testing"

func TestIsHeadlineLine(t *testing.T) {
	yes := []string{
		"<!-- ⟦ 用一句话介绍自己 ⟧ -->\n",
		"<!-- ⟦ 用一句话介绍自己 ⟧ -->",
		"⟦ milestone ⟧\n",
		"〚 变体 〛",
		"\t<!-- ⟦ 带缩进 ⟧ -->\n",
	}
	for _, s := range yes {
		if !isHeadlineLine(s) {
			t.Errorf("应识别为里程碑行: %q", s)
		}
	}
	no := []string{
		"普通文本\n",
		"代码里的 ⟦ 不在行首 ⟧ 中间",
		"<!-- 普通注释 -->\n",
		"",
	}
	for _, s := range no {
		if isHeadlineLine(s) {
			t.Errorf("不应识别为里程碑行: %q", s)
		}
	}
}

func TestStripHeadlineText(t *testing.T) {
	in := "你好！这是回复。\n<!-- ⟦ 用一句话介绍自己 ⟧ -->\n继续。"
	out := stripHeadlineText(in)
	if out != "你好！这是回复。\n\n继续。" {
		t.Fatalf("剥离结果 = %q", out)
	}
}

func TestHeadlineLineFilterStreaming(t *testing.T) {
	var got string
	f := newHeadlineLineFilter(func(text string) { got += text })
	// 里程碑注释跨多个 chunk 到达。
	f.Write("<!-- ⟦ 用一句话")
	f.Write("介绍自己 ⟧ -->\n")
	f.Write("正常回复第一行。\n")
	f.Write("末尾未换行")
	f.Flush()
	if got != "正常回复第一行。\n末尾未换行" {
		t.Fatalf("过滤结果 = %q", got)
	}
}
