package claude

import (
	"fmt"
	"strconv"
	"strings"
	"testing"
)

// bumpPatch / bumpMinor 从 CLICurrentVersion **推导**出"比基线高一点"的版本号。
//
// 这两个辅助存在的唯一理由是：这组用例曾经把 "2.1.259" 硬编码成"高于基线"，
// 在基线还是 2.1.258 时成立；基线提到 2.1.272 之后，同一个字面量变成了
// **低于**基线，两条测试一起变红 —— 红的不是被测行为，是用例自己过期了。
// 从常量推导之后，下一次 bump 基线不会再牵连这里。
func bumpPatch(v string) string {
	parts := strings.Split(v, ".")
	n, err := strconv.Atoi(parts[2])
	if err != nil {
		panic("CLICurrentVersion 的补丁位不是数字: " + v)
	}
	return fmt.Sprintf("%s.%s.%d", parts[0], parts[1], n+1)
}

func bumpMinor(v string) string {
	parts := strings.Split(v, ".")
	n, err := strconv.Atoi(parts[1])
	if err != nil {
		panic("CLICurrentVersion 的次版本位不是数字: " + v)
	}
	return fmt.Sprintf("%s.%d.0", parts[0], n+1)
}

func TestIsSupportedCLIVersion(t *testing.T) {
	cases := []struct {
		name    string
		version string
		want    bool
	}{
		{"内置基线本身", CLICurrentVersion, true},
		{"高于基线的补丁位", bumpPatch(CLICurrentVersion), true},
		{"高于基线的次版本", bumpMinor(CLICurrentVersion), true},
		{"高于基线的主版本", "3.0.0", true},
		{"低于基线", "2.1.219", false},
		{"远低于基线", "1.0.0", false},
		{"空值", "", false},
		{"只有两段", "2.2", false},
		{"带 v 前缀", "v2.2.0", false},
		{"预发布后缀", "2.2.0-local", false},
		{"构建元数据", "2.2.0+build1", false},
		{"哨兵版本带后缀", "999.0.0-local", false},
		{"纯数字哨兵仍然合法（形态没问题，运维自负）", "999.0.0", true},
		{"非数字", "abc", false},
		{"多余的段", "2.2.0.1", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsSupportedCLIVersion(tc.version); got != tc.want {
				t.Fatalf("IsSupportedCLIVersion(%q) = %v, want %v", tc.version, got, tc.want)
			}
		})
	}
}

func TestResolveCLIVersion(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want string
	}{
		{"未配置时用内置基线", "", CLICurrentVersion},
		{"只有空白等同未配置", "   ", CLICurrentVersion},
		{"合法覆盖生效", bumpPatch(CLICurrentVersion), bumpPatch(CLICurrentVersion)},
		{"两侧空白被裁掉", "  " + bumpPatch(CLICurrentVersion) + "  ", bumpPatch(CLICurrentVersion)},
		{"非法值回落基线", "not-a-version", CLICurrentVersion},
		{"向下覆盖被拒", "2.0.0", CLICurrentVersion},
		{"预发布后缀被拒（会毒化账号指纹）", "2.2.0-local", CLICurrentVersion},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := resolveCLIVersion(tc.raw); got != tc.want {
				t.Fatalf("resolveCLIVersion(%q) = %q, want %q", tc.raw, got, tc.want)
			}
		})
	}
}

// 伪装身份的自洽性：User-Agent 头里的版本号必须与 CLIVersion() 一致。
// 两者由不同代码路径写入同一个请求，不一致会被上游判为非正版客户端。
func TestDefaultHeadersUserAgentMatchesCLIVersion(t *testing.T) {
	want := "claude-cli/" + CLIVersion() + " (external, cli)"
	if got := DefaultHeaders["User-Agent"]; got != want {
		t.Fatalf("DefaultHeaders[User-Agent] = %q, want %q", got, want)
	}
}

// 没有配置覆盖时，CLIVersion() 必须等于内置基线——保证本 PR 对既有部署零行为变化。
func TestCLIVersionDefaultsToBuiltinPin(t *testing.T) {
	if got := CLIVersion(); got != CLICurrentVersion {
		t.Fatalf("CLIVersion() = %q, want built-in pin %q (测试进程未设置 %s)", got, CLICurrentVersion, CLIVersionEnv)
	}
}
