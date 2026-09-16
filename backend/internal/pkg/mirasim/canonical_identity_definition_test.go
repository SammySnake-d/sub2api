package mirasim

// canonical 身份**定义本身**的验收。
//
// 这个文件与 repository/mirasim_identity_chokepoint_test.go 分工不同，两者都需要：
//
//	chokepoint_test  —— 出站请求的画像是否等于 canonical（**行为**）
//	本文件           —— canonical 自己是否是一台真实存在的机器（**定义**）
//
// 缺了本文件的后果很具体：chokepoint 那一侧逐个头比对 canonical，所以
// 把 canonical 的定义改成一个从未出厂过的混搭（例如 claude-cli/2.1.272 配
// 0.94.0 的 stainless 版本），**那一整批断言照样全绿** —— 它们只保证「出站等于
// 我们声称的 canonical」，不保证「我们声称的 canonical 是真的」。
//
// 这不是假想的失效。线上就出现过这种混搭，只不过来源是两条路径各写一半：
//
//	客户流量      claude-cli/2.1.272 (external, sdk-cli)  MacOS  0.112.1  v26.3.0
//	账号健康检查   claude-cli/2.1.272 (external, cli)     Linux  0.94.0   v24.3.0
//
// 收口之后来源只剩一处，于是「那一处写的是否自洽」成了唯一的风险，也就是本文件。

import (
	"regexp"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/claude"
)

// knownClientSnapshot 是一份**真实观测过**的客户端版本组合。
//
// 只能由实测填充：抓一条真实 Claude Code 的出站请求，把那一组值原样抄进来，
// 并注明观测时间与来源。凭直觉拼出来的组合放进这张表，就等于把这道门关掉了。
type knownClientSnapshot struct {
	name             string
	cliVersion       string
	stainlessPackage string
	stainlessRuntime string
	os               string
}

// knownClientSnapshots 是白名单。新增一行**必须**附实测来源。
func knownClientSnapshots() []knownClientSnapshot {
	return []knownClientSnapshot{
		{
			// 2026-09-16 本地抓包：真实 Claude Code 2.1.272 在 macOS 上的出站组合。
			// x-stainless-package-version=0.112.1 与 runtime=v26.3.0 是同一条请求里读到的，
			// 不是从两条请求拼的。
			name:             "claude-code-2.1.272-macos",
			cliVersion:       "2.1.272",
			stainlessPackage: "0.112.1",
			stainlessRuntime: "v26.3.0",
			os:               "MacOS",
		},
	}
}

var canonicalUAVersionRe = regexp.MustCompile(`^claude-cli/([0-9]+\.[0-9]+\.[0-9]+)\s`)

// TestCanonicalIdentityIsARealSnapshotNotAMashup 是本文件的主门。
func TestCanonicalIdentityIsARealSnapshotNotAMashup(t *testing.T) {
	h := CanonicalIdentityHeaders()

	ua := h["User-Agent"]
	m := canonicalUAVersionRe.FindStringSubmatch(ua)
	if m == nil {
		t.Fatalf("canonical User-Agent 解不出版本号: %q —— 形态本身就不像真实客户端", ua)
	}
	gotCLI := m[1]
	gotPkg := h["X-Stainless-Package-Version"]
	gotRuntime := h["X-Stainless-Runtime-Version"]
	gotOS := h["X-Stainless-OS"]

	for _, snap := range knownClientSnapshots() {
		if gotCLI == snap.cliVersion &&
			gotPkg == snap.stainlessPackage &&
			gotRuntime == snap.stainlessRuntime &&
			gotOS == snap.os {
			return // 整组命中同一份实测快照
		}
	}

	// 逐项列出差异，而不是只说"不匹配"：混搭时通常只有一两个字段跑偏，
	// 直接指出是哪个能省掉一轮排查。
	var lines []string
	for _, snap := range knownClientSnapshots() {
		var diff []string
		if gotCLI != snap.cliVersion {
			diff = append(diff, "cli "+gotCLI+"≠"+snap.cliVersion)
		}
		if gotPkg != snap.stainlessPackage {
			diff = append(diff, "package "+gotPkg+"≠"+snap.stainlessPackage)
		}
		if gotRuntime != snap.stainlessRuntime {
			diff = append(diff, "runtime "+gotRuntime+"≠"+snap.stainlessRuntime)
		}
		if gotOS != snap.os {
			diff = append(diff, "os "+gotOS+"≠"+snap.os)
		}
		lines = append(lines, "  vs "+snap.name+": "+strings.Join(diff, ", "))
	}
	t.Fatalf("canonical 身份不是任何一份实测快照，是个混搭组合。\n"+
		"实际: cli=%s package=%s runtime=%s os=%s\n%s\n"+
		"上游侧看到的是一台不存在的机器。要么把 canonical 改回某份实测组合，"+
		"要么抓一条真实请求、把新组合连同观测时间一起加进 knownClientSnapshots()。",
		gotCLI, gotPkg, gotRuntime, gotOS, strings.Join(lines, "\n"))
}

// TestKnownSnapshotWhitelistIsNotEmpty 防止这道门被最省事的方式关掉。
//
// 把 knownClientSnapshots() 清空，上面那条会因为「没有任何快照可比」而永远失败
// —— 但反过来，如果有人为了让它通过而把当前值直接补成一行新快照，这道门就白设了。
// 后者机器判不了（一行实测值和一行编造值在代码里长得一样），所以它进验收收据的
// 未机检项，这里只守「白名单非空」这条底线。
func TestKnownSnapshotWhitelistIsNotEmpty(t *testing.T) {
	if len(knownClientSnapshots()) == 0 {
		t.Fatal("实测快照白名单为空 —— 主门失去判据")
	}
}

// TestCanonicalUAVersionTracksTheProcessPin 钉住 canonical UA 的版本来源。
//
// canonical UA 的版本段刻意取 claude.CLIVersion()（进程级 pin，只能向上覆盖、
// init 时解析一次），而不是再硬编码一份。硬编码第二份的后果是它会和 sub2api
// 其余部分漂开，而漂开是静默的。
func TestCanonicalUAVersionTracksTheProcessPin(t *testing.T) {
	ua := CanonicalIdentityHeaders()["User-Agent"]
	want := "claude-cli/" + claude.CLIVersion()
	if !strings.HasPrefix(ua, want+" ") {
		t.Fatalf("canonical UA %q 的版本段不是进程级 pin %q —— "+
			"硬编码第二份版本号会与 sub2api 其余部分静默漂开", ua, claude.CLIVersion())
	}
}
