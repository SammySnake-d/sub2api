package service

// mirasim 请求形状的本地处理：剥掉必被拒的采样参数，拦掉必被拒的 body 形状。
//
// # 上游有两层校验，都只回通用文案
//
// mirasim 拒绝一个 /v1/messages 请求时从不指出是哪个字段错了。但两层校验的文案
// 大小写不同，这是可以用来分层的指纹（2026-09-18 逐字段单变量实测，法国生产实例
// 走真实账号，每组都带前后双对照，对照不绿的读数一律作废重跑）：
//
//	网关层  "the request was rejected as invalid"    小写、无句点
//	后端层  "The request was rejected as invalid."   大写、有句点
//
// ## 网关层：拒绝一切采样参数
//
//	只给 temperature      → 400        只给 top_p → 400
//	只给 top_k            → 400        temperature=1（默认值）→ 400
//	三个都不给            → 200
//
// 跨模型一致（fable 与 haiku 同样）。注意它拒的是**字段存在**，不是取值范围 ——
// 连官方默认值 1 都不接受。
//
// （同一层还会因为 system 缺少 Claude Agent SDK 身份行而拒，见 mirasim_body.go。
// 两者文案相同，属于同一层的两条规则。）
//
// ## 后端层：严格 schema，additionalProperties:false
//
//	message 对象多一个键        id / usage / stop_reason / type —— 各自单独就 400
//	text 块多一个键             index → 400（citations 与 cache_control 均 200）
//	tool_use / tool_result      未知键 → 400（is_error、cache_control 均 200）
//	工具名含 . : 空格 中文      → 400（长度不限，128 字符也 200）
//	两个工具重名                → 400
//	body 顶层未知字段           seed / betas / output_version / mcp_servers → 400
//	fable 模型的 assistant 预填 → 400（同一条请求换 haiku 则 200，是模型相关的）
//
// 合法字段全部放行，实测 200 的有：stop_sequences、metadata、service_tier、
// tool_choice、tools（含 $ref/$defs/anyOf/64 个工具）、system 字符串与数组、
// thinking（enabled 与 adaptive）、output_config、container、context_management、
// cache_control、citations、image 块、tool_use/tool_result 回传、?beta=true、
// Claude Code 那一整串 anthropic-beta 头。
//
// # 生产上是谁在踩
//
// 2026-09-16 起，66 次这类 400 分布在 4 个 key 上。最值得注意的一类是把**整条 API
// 响应原样塞回历史**的客户端：真实 Anthropic 响应的 message 对象带 id / usage /
// stop_reason，text 块带 citations，流式重组后还会残留 index。也就是说
// **mirasim 比真 Anthropic API 严格：把它自己的响应原样喂回去会被它自己拒。**
//
// # 为什么这次改写 body，而探活那次不改
//
// 探活那次（mirasim_availability_probe.go）我拒绝把 max_tokens 从 1 改成 2，
// 理由是 max_tokens 是调用方的输出契约，静默改它会让一个真的只要 1 个 token 的
// 调用方拿到 2 个。采样参数不一样：上游对它们的处理是**全部忽略**（三个都不给
// 与都给，在上游看来是同一个请求 —— 后者只是被拒了），所以剥掉它们不改变任何
// 一次成功调用的语义，只把「必然失败」变成「按默认采样成功」。
//
// 其余形状问题不剥：那些键属于**内容**（对话历史、工具定义），替客户端重写内容
// 就是在猜他想说什么。这些一律本地拒掉，但给出上游不肯给的那句话 —— 到底哪个
// 字段错了。

import (
	"fmt"
	"strings"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// mirasimStrippedSamplingParams 是上游必拒、且剥掉不改变语义的顶层采样参数。
//
// 顺序固定，让日志里的记录可比对。
var mirasimStrippedSamplingParams = []string{"temperature", "top_p", "top_k"}

// mirasimMessageKeys 是 message 对象上被上游接受的全部键。
//
// 这个集合是穷举的而不是取样的：id / usage / stop_reason / type 四个各自**单独**
// 加上去都被拒，而官方 Messages API 的 message 对象本来也只定义了这两个键。
var mirasimMessageKeys = map[string]bool{"role": true, "content": true}

// mirasimBlockKeys 给出每种 content 块被接受的键。
//
// 只收录**实测过**的三种块。image / document / thinking / server_tool_use 等
// 没有逐键测过，就不在这里校验 —— 漏拦一条的代价是维持现状（上游照样拒，和今天
// 一样），误拦一条的代价是打断一条本来能成功的请求。两者不对等。
var mirasimBlockKeys = map[string]map[string]bool{
	"text": {
		"type": true, "text": true, "cache_control": true, "citations": true,
	},
	"tool_use": {
		"type": true, "id": true, "name": true, "input": true, "cache_control": true,
	},
	"tool_result": {
		"type": true, "tool_use_id": true, "content": true, "is_error": true,
		"cache_control": true,
	},
}

// MirasimInvalidRequestError 表示这条请求的形状注定被上游拒，我们在本地提前回掉。
//
// 和 MirasimAvailabilityProbeError 同一个形状，理由也相同：Forward 的成功路径上
// handler 会无条件解引用 result（gateway_handler.go:1108 没有判空），所以 service
// 绝不能返回 (nil, nil)。走错误路径同时拿到「不触发 failover」——换个账号同样会被
// 同一条 schema 规则拒，每次重试都是一次白烧的上游往返。
type MirasimInvalidRequestError struct {
	Message string
}

func (e *MirasimInvalidRequestError) Error() string { return e.Message }

// sanitizeMirasimSamplingParams 剥掉上游必拒的采样参数。
//
// 返回处理后的 body 和被剥掉的参数名（供日志用；没剥任何东西时返回原 body 本身，
// 不做无谓的拷贝，也就不会扰动 prompt cache 前缀）。
func sanitizeMirasimSamplingParams(body []byte) ([]byte, []string) {
	if len(body) == 0 {
		return body, nil
	}
	var stripped []string
	out := body
	for _, key := range mirasimStrippedSamplingParams {
		if !gjson.GetBytes(out, key).Exists() {
			continue
		}
		next, err := sjson.DeleteBytes(out, key)
		if err != nil {
			// 删不掉就整条放弃剥离：让上游给出它自己的判定，而不是把一个
			// 半改过的 body 发出去。
			return body, nil
		}
		out = next
		stripped = append(stripped, key)
	}
	if len(stripped) == 0 {
		return body, nil
	}
	return out, stripped
}

// mirasimRequestShapeViolation 检查已实测过的那些必拒形状。
//
// 返回 nil 表示放行。判据刻意只覆盖实测过的规则：读不懂的 body（不是合法 JSON、
// messages 不是数组）一律放行，交给上游判。
func mirasimRequestShapeViolation(body []byte) *MirasimInvalidRequestError {
	if len(body) == 0 {
		return nil
	}
	if !gjson.ValidBytes(body) {
		return nil
	}
	if v := mirasimToolsViolation(body); v != nil {
		return v
	}
	if v := mirasimMessagesViolation(body); v != nil {
		return v
	}
	return mirasimPrefillViolation(body)
}

// mirasimToolsViolation 检查工具名的字符集与重名。
func mirasimToolsViolation(body []byte) *MirasimInvalidRequestError {
	tools := gjson.GetBytes(body, "tools")
	if !tools.IsArray() {
		return nil
	}
	seen := make(map[string]int)
	var bad *MirasimInvalidRequestError
	idx := -1
	tools.ForEach(func(_, tool gjson.Result) bool {
		idx++
		name := tool.Get("name").String()
		if name == "" {
			return true
		}
		if offending, ok := firstCharOutsideMirasimToolNameSet(name); ok {
			bad = &MirasimInvalidRequestError{Message: fmt.Sprintf(
				"mirasim rejected this request locally: tools[%d].name %q contains %q, "+
					"which is outside the accepted character set [a-zA-Z0-9_-]. "+
					"Rename the tool (the upstream accepts any length, only the character set is enforced).",
				idx, name, offending)}
			return false
		}
		if prev, dup := seen[name]; dup {
			bad = &MirasimInvalidRequestError{Message: fmt.Sprintf(
				"mirasim rejected this request locally: tools[%d].name %q duplicates tools[%d].name. "+
					"Tool names must be unique.", idx, name, prev)}
			return false
		}
		seen[name] = idx
		return true
	})
	return bad
}

// firstCharOutsideMirasimToolNameSet 返回第一个越界字符，方便错误文案直接指出来。
func firstCharOutsideMirasimToolNameSet(name string) (string, bool) {
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z',
			r >= 'A' && r <= 'Z',
			r >= '0' && r <= '9',
			r == '_', r == '-':
		default:
			return string(r), true
		}
	}
	return "", false
}

// mirasimMessagesViolation 检查 message 对象与 content 块上的多余键。
//
// 这是生产上最常踩的一条：客户端把整条 API 响应存下来、原样当历史回传。
func mirasimMessagesViolation(body []byte) *MirasimInvalidRequestError {
	messages := gjson.GetBytes(body, "messages")
	if !messages.IsArray() {
		return nil
	}
	var bad *MirasimInvalidRequestError
	mi := -1
	messages.ForEach(func(_, msg gjson.Result) bool {
		mi++
		if !msg.IsObject() {
			return true
		}
		msg.ForEach(func(key, _ gjson.Result) bool {
			k := key.String()
			if mirasimMessageKeys[k] {
				return true
			}
			bad = &MirasimInvalidRequestError{Message: fmt.Sprintf(
				"mirasim rejected this request locally: messages[%d] carries the key %q, "+
					"but the upstream accepts only \"role\" and \"content\" on a message object. "+
					"This is usually a client that stores the whole API response and replays it as "+
					"conversation history — drop everything except role and content.", mi, k)}
			return false
		})
		if bad != nil {
			return false
		}
		bad = mirasimContentViolation(mi, msg.Get("content"))
		return bad == nil
	})
	return bad
}

// mirasimContentViolation 检查一条消息里各 content 块的键。
func mirasimContentViolation(mi int, content gjson.Result) *MirasimInvalidRequestError {
	if !content.IsArray() {
		return nil
	}
	var bad *MirasimInvalidRequestError
	bi := -1
	content.ForEach(func(_, block gjson.Result) bool {
		bi++
		if !block.IsObject() {
			return true
		}
		blockType := block.Get("type").String()
		allowed, measured := mirasimBlockKeys[blockType]
		if !measured {
			// 没逐键实测过的块类型不校验，见 mirasimBlockKeys 的说明。
			return true
		}
		block.ForEach(func(key, _ gjson.Result) bool {
			k := key.String()
			if allowed[k] {
				return true
			}
			hint := ""
			if k == "index" {
				hint = " \"index\" is a streaming-event artifact: a client that reassembles SSE " +
					"deltas into a block must drop it before replaying the block as history."
			}
			bad = &MirasimInvalidRequestError{Message: fmt.Sprintf(
				"mirasim rejected this request locally: messages[%d].content[%d] (type %q) carries "+
					"the key %q, which the upstream does not accept on that block type.%s",
				mi, bi, blockType, k, hint)}
			return false
		})
		return bad == nil
	})
	return bad
}

// mirasimPrefillViolation 拦掉 fable 模型上的 assistant 预填。
//
// 作用域刻意只到 fable：同一条请求发给 haiku 是 200，所以这条规则是模型相关的，
// 而 opus / sonnet 没有实测过。把没测过的模型也拦下来就是把实测结论外推，
// 代价是打断本来能成功的请求。
func mirasimPrefillViolation(body []byte) *MirasimInvalidRequestError {
	if mirasimModelFamily(gjson.GetBytes(body, "model").String()) != mirasimFamilyFable {
		return nil
	}
	messages := gjson.GetBytes(body, "messages")
	if !messages.IsArray() {
		return nil
	}
	arr := messages.Array()
	if len(arr) == 0 {
		return nil
	}
	if !strings.EqualFold(arr[len(arr)-1].Get("role").String(), "assistant") {
		return nil
	}
	return &MirasimInvalidRequestError{Message: fmt.Sprintf(
		"mirasim rejected this request locally: the conversation ends with an assistant turn "+
			"(messages[%d]), i.e. a prefill, which the fable models do not accept. "+
			"Append a user turn, or send the same conversation to a claude model.",
		len(arr)-1)}
}
