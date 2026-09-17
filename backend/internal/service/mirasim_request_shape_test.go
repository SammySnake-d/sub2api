package service

import (
	"strings"
	"testing"

	"github.com/tidwall/gjson"
)

// 本文件的每条用例都对应 2026-09-18 在法国生产实例上跑过的一次真实探针：
// 标注「实测 400」的形状上游确实拒了，标注「实测 200」的上游确实收了。
// 没有实测支撑的形状不写进来当断言——那会把推测固化成契约。

func TestSanitizeMirasimSamplingParams(t *testing.T) {
	t.Run("三个参数各自单独都会被剥掉", func(t *testing.T) {
		for _, key := range []string{"temperature", "top_p", "top_k"} {
			body := []byte(`{"model":"claude-fable-5-1","` + key + `":0.7,"max_tokens":8}`)

			// 前提断言：剥之前这个字段确实在，否则下面的"剥掉了"是空转。
			if !gjson.GetBytes(body, key).Exists() {
				t.Fatalf("前提不成立：构造的 body 里没有 %s", key)
			}

			out, stripped := sanitizeMirasimSamplingParams(body)
			if len(stripped) != 1 || stripped[0] != key {
				t.Fatalf("%s: 期望剥掉 [%s]，实际 %v", key, key, stripped)
			}
			if gjson.GetBytes(out, key).Exists() {
				t.Errorf("%s: 剥完之后字段还在：%s", key, out)
			}
			if gjson.GetBytes(out, "model").String() != "claude-fable-5-1" {
				t.Errorf("%s: 剥掉采样参数时动到了 model：%s", key, out)
			}
			if gjson.GetBytes(out, "max_tokens").Int() != 8 {
				t.Errorf("%s: 剥掉采样参数时动到了 max_tokens：%s", key, out)
			}
		}
	})

	t.Run("三个一起给时一次剥干净", func(t *testing.T) {
		body := []byte(`{"model":"m","temperature":1,"top_p":0.9,"top_k":40,"max_tokens":8}`)
		out, stripped := sanitizeMirasimSamplingParams(body)
		if len(stripped) != 3 {
			t.Fatalf("期望剥掉 3 个，实际 %v", stripped)
		}
		for _, key := range []string{"temperature", "top_p", "top_k"} {
			if gjson.GetBytes(out, key).Exists() {
				t.Errorf("%s 还在：%s", key, out)
			}
		}
	})

	t.Run("temperature=1 也要剥——上游拒的是字段存在不是取值", func(t *testing.T) {
		body := []byte(`{"model":"m","temperature":1}`)
		_, stripped := sanitizeMirasimSamplingParams(body)
		if len(stripped) != 1 {
			t.Fatalf("默认值 1 也必须剥，实际 %v", stripped)
		}
	})

	t.Run("没有采样参数时原样返回、不动一个字节", func(t *testing.T) {
		body := []byte(`{"model":"m","max_tokens":8,"messages":[{"role":"user","content":"hi"}]}`)
		out, stripped := sanitizeMirasimSamplingParams(body)
		if stripped != nil {
			t.Fatalf("不该剥任何东西，实际 %v", stripped)
		}
		if string(out) != string(body) {
			t.Errorf("body 被改动了：\n原 %s\n新 %s", body, out)
		}
	})

	t.Run("空 body 不 panic", func(t *testing.T) {
		if out, stripped := sanitizeMirasimSamplingParams(nil); out != nil || stripped != nil {
			t.Errorf("空输入应原样返回，实际 out=%v stripped=%v", out, stripped)
		}
	})
}

func TestMirasimRequestShapeViolation_实测会被拒的形状(t *testing.T) {
	cases := []struct {
		name string
		body string
		// want 是错误文案里必须出现的片段：不只断言"拒了"，还断言"指对了字段"。
		// 上游从不指字段，本地拒的全部价值就在这句话上。
		want []string
	}{
		{
			name: "message 对象多 id（客户端回塞整条响应）",
			body: `{"model":"claude-fable-5-1","messages":[
				{"role":"user","content":"hi"},
				{"role":"assistant","content":"yo","id":"msg_01A"},
				{"role":"user","content":"go"}]}`,
			want: []string{"messages[1]", `"id"`, "role", "content"},
		},
		{
			name: "message 对象多 usage",
			body: `{"model":"claude-fable-5-1","messages":[
				{"role":"user","content":"hi"},
				{"role":"assistant","content":"yo","usage":{"input_tokens":1}},
				{"role":"user","content":"go"}]}`,
			want: []string{"messages[1]", `"usage"`},
		},
		{
			name: "message 对象多 stop_reason",
			body: `{"model":"claude-fable-5-1","messages":[
				{"role":"assistant","content":"yo","stop_reason":"end_turn"},
				{"role":"user","content":"go"}]}`,
			want: []string{"messages[0]", `"stop_reason"`},
		},
		{
			name: "text 块残留 index（SSE 重组产物）",
			body: `{"model":"claude-fable-5-1","messages":[
				{"role":"assistant","content":[{"type":"text","text":"yo","index":0}]},
				{"role":"user","content":"go"}]}`,
			want: []string{"messages[0].content[0]", `"text"`, `"index"`, "streaming-event artifact"},
		},
		{
			name: "tool_use 块未知键",
			body: `{"model":"claude-fable-5-1","messages":[
				{"role":"assistant","content":[
					{"type":"tool_use","id":"toolu_1","name":"t","input":{},"zzz":1}]},
				{"role":"user","content":"go"}]}`,
			want: []string{"messages[0].content[0]", `"tool_use"`, `"zzz"`},
		},
		{
			name: "tool_result 块未知键",
			body: `{"model":"claude-fable-5-1","messages":[
				{"role":"user","content":[
					{"type":"tool_result","tool_use_id":"toolu_1","content":"ok","zzz":1}]},
				{"role":"user","content":"go"}]}`,
			want: []string{`"tool_result"`, `"zzz"`},
		},
		{
			name: "工具名带点",
			body: `{"model":"claude-fable-5-1","tools":[{"name":"a.b"}],
				"messages":[{"role":"user","content":"hi"}]}`,
			want: []string{"tools[0].name", `"a.b"`, `"."`, "[a-zA-Z0-9_-]"},
		},
		{
			name: "工具名带冒号",
			body: `{"model":"claude-fable-5-1","tools":[{"name":"ok"},{"name":"a:b"}],
				"messages":[{"role":"user","content":"hi"}]}`,
			want: []string{"tools[1].name", `":"`},
		},
		{
			name: "工具名带空格",
			body: `{"model":"claude-fable-5-1","tools":[{"name":"a b"}],
				"messages":[{"role":"user","content":"hi"}]}`,
			want: []string{"tools[0].name", `" "`},
		},
		{
			name: "工具名带中文",
			body: `{"model":"claude-fable-5-1","tools":[{"name":"查天气"}],
				"messages":[{"role":"user","content":"hi"}]}`,
			want: []string{"tools[0].name", "查"},
		},
		{
			name: "两个工具重名",
			body: `{"model":"claude-fable-5-1","tools":[
				{"name":"dup"},{"name":"other"},{"name":"dup"}],
				"messages":[{"role":"user","content":"hi"}]}`,
			want: []string{"tools[2].name", `"dup"`, "tools[0].name", "unique"},
		},
		{
			name: "fable 的 assistant 预填",
			body: `{"model":"claude-fable-5-1","messages":[
				{"role":"user","content":"hi"},
				{"role":"assistant","content":"pre"}]}`,
			want: []string{"messages[1]", "prefill", "fable"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// 前提断言：用例 body 必须是合法 JSON，否则 mirasimRequestShapeViolation
			// 会在 ValidBytes 那一步直接放行，用例就变成了空转的伪绿。
			if !gjson.Valid(tc.body) {
				t.Fatalf("用例自身的 body 不是合法 JSON，这条断言没有承重")
			}
			err := mirasimRequestShapeViolation([]byte(tc.body))
			if err == nil {
				t.Fatalf("上游实测会拒这个形状，本地却放行了")
			}
			for _, frag := range tc.want {
				if !strings.Contains(err.Message, frag) {
					t.Errorf("错误文案没指出 %q：\n%s", frag, err.Message)
				}
			}
		})
	}
}

func TestMirasimRequestShapeViolation_实测会被接受的形状(t *testing.T) {
	// 这一组是防误伤的。每一条都在生产上实测返回 200——本地拦下来就是把一条
	// 本来能成功的请求打断，比漏拦严重得多。
	cases := []struct {
		name string
		body string
	}{
		{"裸对话", `{"model":"claude-fable-5-1","messages":[
			{"role":"user","content":"hi"},{"role":"assistant","content":"yo"},
			{"role":"user","content":"go"}]}`},
		{"text 块带 citations=null", `{"model":"claude-fable-5-1","messages":[
			{"role":"assistant","content":[{"type":"text","text":"yo","citations":null}]},
			{"role":"user","content":"go"}]}`},
		{"text 块带 citations=[]", `{"model":"claude-fable-5-1","messages":[
			{"role":"assistant","content":[{"type":"text","text":"yo","citations":[]}]},
			{"role":"user","content":"go"}]}`},
		{"text 块带 cache_control", `{"model":"claude-fable-5-1","messages":[
			{"role":"assistant","content":[
				{"type":"text","text":"yo","cache_control":{"type":"ephemeral"}}]},
			{"role":"user","content":"go"}]}`},
		{"tool_result 带 is_error", `{"model":"claude-fable-5-1","messages":[
			{"role":"user","content":[
				{"type":"tool_result","tool_use_id":"t1","content":"x","is_error":false}]}]}`},
		{"tool_use 带 cache_control", `{"model":"claude-fable-5-1","messages":[
			{"role":"assistant","content":[{"type":"tool_use","id":"t1","name":"n",
				"input":{},"cache_control":{"type":"ephemeral"}}]},
			{"role":"user","content":"go"}]}`},
		{"128 字符的工具名（长度不设限）", `{"model":"claude-fable-5-1","tools":[{"name":"` +
			strings.Repeat("a", 128) + `"}],"messages":[{"role":"user","content":"hi"}]}`},
		{"带连字符和下划线的真实 MCP 工具名", `{"model":"claude-fable-5-1","tools":[
			{"name":"mcp__chrome-devtools-mcp__take_snapshot"}],
			"messages":[{"role":"user","content":"hi"}]}`},
		{"image 块（键集未实测，一律放行）", `{"model":"claude-fable-5-1","messages":[
			{"role":"user","content":[{"type":"image","source":{"type":"base64",
				"media_type":"image/png","data":"x"},"whatever":1}]}]}`},
		{"content 是字符串不是数组", `{"model":"claude-fable-5-1","messages":[
			{"role":"user","content":"hi"}]}`},
		{"haiku 的 assistant 预填（实测 200，规则只到 fable）", `{"model":"claude-haiku-4-5",
			"messages":[{"role":"user","content":"hi"},{"role":"assistant","content":"pre"}]}`},
		{"opus 的 assistant 预填（未实测，不外推）", `{"model":"claude-opus-4-8",
			"messages":[{"role":"user","content":"hi"},{"role":"assistant","content":"pre"}]}`},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if !gjson.Valid(tc.body) {
				t.Fatalf("用例自身的 body 不是合法 JSON")
			}
			if err := mirasimRequestShapeViolation([]byte(tc.body)); err != nil {
				t.Errorf("误伤：上游实测接受这个形状，本地却拒了\n%s", err.Message)
			}
		})
	}
}

func TestMirasimRequestShapeViolation_读不懂就放行(t *testing.T) {
	// 判据刻意保守：解析不了的东西交给上游判，不要自己发明一个拒绝理由。
	for _, body := range []string{"", "not json at all", `{"messages":"not an array"}`,
		`{"model":"claude-fable-5-1","tools":"not an array","messages":[]}`,
		`{"model":"claude-fable-5-1","messages":[]}`} {
		if err := mirasimRequestShapeViolation([]byte(body)); err != nil {
			t.Errorf("读不懂的 body 应放行，实际拒了：%q -> %s", body, err.Message)
		}
	}
}

func TestFirstCharOutsideMirasimToolNameSet(t *testing.T) {
	ok := []string{"a", "Z", "0", "_", "-", "mcp__srv__tool", "get-weather-2",
		strings.Repeat("x", 200)}
	for _, name := range ok {
		if c, bad := firstCharOutsideMirasimToolNameSet(name); bad {
			t.Errorf("%q 应合法，却报了越界字符 %q", name, c)
		}
	}
	bad := map[string]string{"a.b": ".", "a:b": ":", "a b": " ", "查": "查",
		"a/b": "/", "a+b": "+", "ok.": "."}
	for name, want := range bad {
		c, isBad := firstCharOutsideMirasimToolNameSet(name)
		if !isBad {
			t.Errorf("%q 应越界，却判为合法", name)
			continue
		}
		if c != want {
			t.Errorf("%q 期望首个越界字符 %q，实际 %q", name, want, c)
		}
	}
}
