package admin

import (
	"context"
	"sort"
	"testing"

	"github.com/stretchr/testify/require"
)

// 导入筛选层的验收测试。
//
// 被测的是 selectMirasimImportCandidates 这一层运营规则,不是导入本身。143 个
// mirasim 账号最初由一段一次性 shell 脚本筛出,规则只活在那段脚本里;这些测试
// 存在的意义就是让「重跑一次选中的还是不是同一批」变成可判决的问题。
//
// 一条纪律贯穿全文件:每条正例都配一条只改一个开关的差分阴性。少了阴性,一个
// 「永远返回全选」或「永远返回全保留」的实现也能让正例全绿 —— 那样的门证明不
// 了自己有鉴别力,只能证明它被执行过。
//
// 夹具里的邮箱一律是 example.invalid。真实的点名清单是运营者在请求里递进来的
// 数据,不进仓库:把它写进测试等于把一份运营名单固化成代码,改名单要改代码。

// mirasimSelectionTestCutoff 是订阅到期上界:晚于 2026 年底到期的(即那批 2027
// 到期的一年期自用号)保留不导。
const mirasimSelectionTestCutoff = "2026-12-31T23:59:59Z"

// mirasimSelectionTestRecord 造一条来源记录。只带筛选层真正要读的字段:device
// seed / token 是导入层的事,筛选层在它们之前就得出结论。
func mirasimSelectionTestRecord(email string, overrides map[string]any) map[string]any {
	record := map[string]any{"email": email}
	for key, value := range overrides {
		record[key] = value
	}
	return record
}

// mirasimSelectionTestEntries 按 1-based 顺序编号,与 parseMirasimImportEntries
// 的编号方式一致 —— decisions 里的 Index 要能被运营者对回来源清单的行号。
func mirasimSelectionTestEntries(records ...map[string]any) []mirasimImportEntry {
	entries := make([]mirasimImportEntry, 0, len(records))
	for i, record := range records {
		entries = append(entries, mirasimImportEntry{Index: i + 1, Value: record})
	}
	return entries
}

// mirasimSelectionTestEmails 直接从原始 map 读邮箱,不经过 mirasimImportFactsOf。
// 读数必须与被测代码正交:用被测代码解析出的标识去断言被测代码的分组,标识解析
// 一旦坏掉两边会一起坏,测试照样绿。
func mirasimSelectionTestEmails(t *testing.T, entries []mirasimImportEntry) []string {
	t.Helper()
	out := make([]string, 0, len(entries))
	for _, entry := range entries {
		record, ok := entry.Value.(map[string]any)
		require.True(t, ok, "测试夹具第 %d 条不是 map,读数管道坏了", entry.Index)
		email, ok := record["email"].(string)
		require.True(t, ok, "测试夹具第 %d 条没有 email", entry.Index)
		out = append(out, email)
	}
	return out
}

// mirasimSelectionTestDecision 取某条来源记录的判定。找不到即失败:decisions 少
// 一条,就是有账号没有留下去向记录。
func mirasimSelectionTestDecision(t *testing.T, decisions []mirasimImportDecision, index int) mirasimImportDecision {
	t.Helper()
	for _, decision := range decisions {
		if decision.Index == index {
			return decision
		}
	}
	require.FailNowf(t, "缺少判定", "第 %d 条来源记录没有出现在 decisions 里", index)
	return mirasimImportDecision{}
}

// TestSelectMirasimImportCandidatesReservesDisabledSource:来源里标记为 disabled
// 的账号不导入。
func TestSelectMirasimImportCandidatesReservesDisabledSource(t *testing.T) {
	// [[cov:IMP:skip-disabled]]
	entries := mirasimSelectionTestEntries(
		mirasimSelectionTestRecord("healthy@example.invalid", nil),
		mirasimSelectionTestRecord("flagged@example.invalid", map[string]any{"disabled": true}),
		// 第二种写法:来源不一定用 disabled 布尔,状态字段同样是肯定式标记。
		mirasimSelectionTestRecord("suspended@example.invalid", map[string]any{"status": "disabled"}),
		// disabled:false 是明确的「没被禁用」。它必须被导入 —— 若判据松成「出现
		// 过 disabled 这个键就算禁用」,这一条会被静默吞掉,而少导的号不报错。
		mirasimSelectionTestRecord("explicitly-enabled@example.invalid", map[string]any{"disabled": false}),
	)

	selected, reserved, decisions := selectMirasimImportCandidates(entries, MirasimImportSelectionPolicy{SkipDisabled: true})

	require.Equal(t,
		[]string{"healthy@example.invalid", "explicitly-enabled@example.invalid"},
		mirasimSelectionTestEmails(t, selected),
		"只有未被标记禁用的账号可以进入导入集,且必须保持来源顺序")
	require.Equal(t,
		[]string{"flagged@example.invalid", "suspended@example.invalid"},
		mirasimSelectionTestEmails(t, reserved),
		"两种禁用写法都要被 mirasimImportDisabled 认出来")

	// 计数相等还不够:两个禁用账号可能一个被正确保留、另一个因为别的原因被保留,
	// 总数照样对得上。理由必须逐条钉死在 mirasimImportReasonDisabled 上。
	require.Equal(t, mirasimImportReasonDisabled, mirasimSelectionTestDecision(t, decisions, 2).Reason)
	require.Equal(t, mirasimImportReasonDisabled, mirasimSelectionTestDecision(t, decisions, 3).Reason)
	require.Equal(t, mirasimImportReasonDefault, mirasimSelectionTestDecision(t, decisions, 1).Reason)
	require.Equal(t, mirasimImportReasonDefault, mirasimSelectionTestDecision(t, decisions, 4).Reason)

	// ── 差分阴性:同一批来源,只把 SkipDisabled 关掉 ──
	// 结论必须反转。不反转说明这两个号是被别的东西挡住的,上面那组断言就没有在
	// 证明 SkipDisabled 有效。
	relaxedSelected, relaxedReserved, relaxedDecisions := selectMirasimImportCandidates(entries, MirasimImportSelectionPolicy{SkipDisabled: false})
	require.Len(t, relaxedSelected, len(entries), "关掉 SkipDisabled 后禁用账号也应进入导入集")
	require.Len(t, relaxedReserved, 0)
	require.NotEqual(t, mirasimImportReasonDisabled, mirasimSelectionTestDecision(t, relaxedDecisions, 2).Reason)
	require.Equal(t, mirasimImportReasonDefault, mirasimSelectionTestDecision(t, relaxedDecisions, 2).Reason)
}

// TestSelectMirasimImportCandidatesReservesPlansExpiringAfterCutoff:2027 年到期
// 的一年期账号是运营者自用的,不导入。
func TestSelectMirasimImportCandidatesReservesPlansExpiringAfterCutoff(t *testing.T) {
	// [[cov:IMP:skip-annual-reserved]]
	entries := mirasimSelectionTestEntries(
		mirasimSelectionTestRecord("monthly@example.invalid", map[string]any{"plan_expires_at": "2026-03-01T00:00:00Z"}),
		// 来源里真实的形态带小数秒,解析必须吃得下,否则这个号会掉进「读不出」
		// 分支,理由错了(结论碰巧一样),运营者复核时看到的是另一回事。
		mirasimSelectionTestRecord("annual@example.invalid", map[string]any{"plan_expires_at": "2027-09-10T19:20:40.460323Z"}),
		// 这条是判据的防混淆桩:token 到期在 2027,订阅到期字段缺席。规则读的若
		// 是 credentials.expires_at,这个号会被误留;而真正该留的自用号因为
		// token 几小时后就过期(永远早于 cutoff),反倒会被放进生产池 —— 两个错
		// 误方向相反,计数上互相抵消,只有逐条断言看得见。
		mirasimSelectionTestRecord("token-expiry-2027@example.invalid", map[string]any{"expires_at": "2027-12-31T00:00:00Z"}),
	)
	policy := MirasimImportSelectionPolicy{SkipExpiringAfter: mirasimSelectionTestCutoff}

	selected, reserved, decisions := selectMirasimImportCandidates(entries, policy)

	require.Equal(t,
		[]string{"monthly@example.invalid", "token-expiry-2027@example.invalid"},
		mirasimSelectionTestEmails(t, selected),
		"只有订阅到期晚于 cutoff 的才保留;token 到期与这条规则无关")
	require.Equal(t, []string{"annual@example.invalid"}, mirasimSelectionTestEmails(t, reserved))
	require.Equal(t, mirasimImportReasonExpiryReserved, mirasimSelectionTestDecision(t, decisions, 2).Reason)
	// Detail 要带上被判据读到的那个时间:运营者复核「哪几个被规则挡了」时,没有
	// 这个值就只能回来源清单里重新对一遍。
	require.Contains(t, mirasimSelectionTestDecision(t, decisions, 2).Detail, "2027-09-10")
	require.Equal(t, mirasimImportReasonDefault, mirasimSelectionTestDecision(t, decisions, 3).Reason)

	// ── 差分阴性 A:只把 cutoff 去掉 ──
	noCutoffSelected, noCutoffReserved, noCutoffDecisions := selectMirasimImportCandidates(entries, MirasimImportSelectionPolicy{})
	require.Len(t, noCutoffSelected, len(entries))
	require.Len(t, noCutoffReserved, 0)
	require.NotEqual(t, mirasimImportReasonExpiryReserved, mirasimSelectionTestDecision(t, noCutoffDecisions, 2).Reason)

	// ── 差分阴性 B:只把 cutoff 往后挪过 2027 ──
	// 证明判据是「跟 cutoff 比」,不是把 2027 写死。写死的实现在阴性 A 下也会反转
	// (规则没开),唯有这一条能把它揪出来。
	lateSelected, lateReserved, lateDecisions := selectMirasimImportCandidates(entries, MirasimImportSelectionPolicy{SkipExpiringAfter: "2028-01-01T00:00:00Z"})
	require.Len(t, lateSelected, len(entries))
	require.Len(t, lateReserved, 0)
	require.Equal(t, mirasimImportReasonDefault, mirasimSelectionTestDecision(t, lateDecisions, 2).Reason)
}

// TestSelectMirasimImportCandidatesHonorsExplicitKeep:运营者点名要导的账号,即使
// 命中 disabled / 2027 到期也必须导入。
func TestSelectMirasimImportCandidatesHonorsExplicitKeep(t *testing.T) {
	// [[cov:IMP:honor-explicit-keep]]
	entries := mirasimSelectionTestEntries(
		// 同时命中两条默认规则:只压过其中一条不算数。
		// 邮箱刻意用混合大小写:来源导出保留用户输入的大小写,而点名清单是人手
		// 敲的。两边必须折叠到同一形态 —— 大小写不该决定一个号的去向。
		mirasimSelectionTestRecord("Named@Example.Invalid", map[string]any{
			"disabled":        true,
			"plan_expires_at": "2027-09-10T19:20:40.460323Z",
		}),
		// 同批对照:一样被标记 disabled,但没被点名。它必须留在 reserved ——
		// 否则「点名」可能是把 SkipDisabled 整条规则关掉了,那是完全不同的行为。
		mirasimSelectionTestRecord("collateral@example.invalid", map[string]any{"disabled": true}),
	)
	policy := MirasimImportSelectionPolicy{
		SkipDisabled:      true,
		SkipExpiringAfter: mirasimSelectionTestCutoff,
		ForceKeep:         []string{"named@example.invalid"},
	}

	selected, reserved, decisions := selectMirasimImportCandidates(entries, policy)

	require.Equal(t, []string{"Named@Example.Invalid"}, mirasimSelectionTestEmails(t, selected))
	require.Equal(t, []string{"collateral@example.invalid"}, mirasimSelectionTestEmails(t, reserved))
	// 理由必须是「被点名」而不是「规则没命中」:两者结论相同,但只有前者说明运营
	// 者的决定压过了规则。若哪天规则判定坏了(比如 disabled 读不出来),理由会退
	// 回 default,这条断言会红,而只看 selected 的断言不会。
	require.Equal(t, mirasimImportReasonForceKeep, mirasimSelectionTestDecision(t, decisions, 1).Reason)
	// 命中的是哪个标识要报出来,且报来源里的原样写法(不是折叠过的小写):运营者
	// 核对名单时要能把它直接搜回来源清单。
	require.Equal(t, "Named@Example.Invalid", mirasimSelectionTestDecision(t, decisions, 1).Detail)
	require.Equal(t, mirasimImportReasonDisabled, mirasimSelectionTestDecision(t, decisions, 2).Reason)

	// ── 差分阴性:同一批来源、同一份策略,只把 ForceKeep 去掉 ──
	withoutKeep := policy
	withoutKeep.ForceKeep = nil
	plainSelected, plainReserved, plainDecisions := selectMirasimImportCandidates(entries, withoutKeep)
	require.Len(t, plainSelected, 0, "没有点名时这个号本来就该被规则挡住")
	require.Len(t, plainReserved, len(entries))
	require.NotEqual(t, mirasimImportReasonForceKeep, mirasimSelectionTestDecision(t, plainDecisions, 1).Reason)
	require.Equal(t, mirasimImportReasonDisabled, mirasimSelectionTestDecision(t, plainDecisions, 1).Reason)
}

// TestSelectMirasimImportCandidatesHonorsExplicitSkipOverKeep:运营者点名不导的
// 账号一定不导入,且压过「点名要导」。
func TestSelectMirasimImportCandidatesHonorsExplicitSkipOverKeep(t *testing.T) {
	// [[cov:IMP:honor-explicit-skip]]
	entries := mirasimSelectionTestEntries(
		// 健康号:两条默认规则都不命中,所以它的去向完全由点名决定 —— 这样才能把
		// ForceSkip 的效果与规则的效果分开。
		mirasimSelectionTestRecord("contested@example.invalid", nil),
		mirasimSelectionTestRecord("plain@example.invalid", nil),
	)
	policy := MirasimImportSelectionPolicy{
		SkipDisabled:      true,
		SkipExpiringAfter: mirasimSelectionTestCutoff,
		ForceKeep:         []string{"contested@example.invalid"},
		ForceSkip:         []string{"CONTESTED@example.invalid"},
	}

	selected, reserved, decisions := selectMirasimImportCandidates(entries, policy)

	// 两份名单同时命中时必须判「不导」。反过来解释会造成不可回退的后果:号被投进
	// 生产池、被客户用掉;判成保留最坏只是少导一个,运营者在 decisions 里看得到。
	require.Equal(t, []string{"plain@example.invalid"}, mirasimSelectionTestEmails(t, selected))
	require.Equal(t, []string{"contested@example.invalid"}, mirasimSelectionTestEmails(t, reserved))
	require.Equal(t, mirasimImportReasonForceSkip, mirasimSelectionTestDecision(t, decisions, 1).Reason)
	require.NotEqual(t, mirasimImportReasonForceKeep, mirasimSelectionTestDecision(t, decisions, 1).Reason)
	require.Equal(t, mirasimImportReasonDefault, mirasimSelectionTestDecision(t, decisions, 2).Reason)

	// ── 差分阴性:同一批来源、同一份策略,只把 ForceSkip 去掉 ──
	// 结论反转成 force_keep。不反转就说明这个号是被别的原因挡住的,上面的断言并没
	// 有证明 ForceSkip 的优先级。
	withoutSkip := policy
	withoutSkip.ForceSkip = nil
	keptSelected, keptReserved, keptDecisions := selectMirasimImportCandidates(entries, withoutSkip)
	require.Equal(t,
		[]string{"contested@example.invalid", "plain@example.invalid"},
		mirasimSelectionTestEmails(t, keptSelected))
	require.Len(t, keptReserved, 0)
	require.Equal(t, mirasimImportReasonForceKeep, mirasimSelectionTestDecision(t, keptDecisions, 1).Reason)
}

// TestSelectMirasimImportCandidatesConservesTheLedger:账目守恒 ——
// 导入数 + 保留数 == 来源总数,且两个集合无交集。
func TestSelectMirasimImportCandidatesConservesTheLedger(t *testing.T) {
	// [[cov:IMP:import-count-matches]]
	// 混合样本,四类情形各有代表:默认导入 / disabled / 2027 到期 / 被点名(两个
	// 方向)。守恒式必须在规则互相交叉时也成立,只用同质样本测等于没测。
	entries := mirasimSelectionTestEntries(
		mirasimSelectionTestRecord("plain-a@example.invalid", nil),
		mirasimSelectionTestRecord("disabled-a@example.invalid", map[string]any{"disabled": true}),
		mirasimSelectionTestRecord("annual-a@example.invalid", map[string]any{"plan_expires_at": "2027-09-10T19:20:40.460323Z"}),
		mirasimSelectionTestRecord("keep-disabled@example.invalid", map[string]any{"disabled": true}),
		mirasimSelectionTestRecord("keep-annual@example.invalid", map[string]any{"plan_expires_at": "2027-04-01T00:00:00Z"}),
		mirasimSelectionTestRecord("skip-healthy@example.invalid", nil),
		mirasimSelectionTestRecord("contested@example.invalid", map[string]any{"plan_expires_at": "2026-02-01T00:00:00Z"}),
		mirasimSelectionTestRecord("plain-b@example.invalid", map[string]any{"status": "active"}),
	)
	policy := MirasimImportSelectionPolicy{
		SkipDisabled:      true,
		SkipExpiringAfter: mirasimSelectionTestCutoff,
		ForceKeep:         []string{"keep-disabled@example.invalid", "keep-annual@example.invalid", "contested@example.invalid"},
		ForceSkip:         []string{"skip-healthy@example.invalid", "contested@example.invalid"},
	}

	selected, reserved, decisions := selectMirasimImportCandidates(entries, policy)

	// 守恒式本体。
	require.Equal(t, len(entries), len(selected)+len(reserved),
		"selectMirasimImportCandidates 的两个出口之和必须等于来源总数")
	require.Len(t, decisions, len(entries), "每条来源记录都要留下一条判定")

	// 光有数量相等挡不住「一条被复制、另一条被丢掉」:总数照样对。把索引并起来去
	// 重后必须正好是 1..N,这一条同时否证了重复与蒸发两种情形。
	indices := make([]int, 0, len(entries))
	for _, entry := range append(append([]mirasimImportEntry(nil), selected...), reserved...) {
		indices = append(indices, entry.Index)
	}
	sort.Ints(indices)
	require.Equal(t, []int{1, 2, 3, 4, 5, 6, 7, 8}, indices,
		"selected ∪ reserved 必须恰好是来源的全部索引,无重复无遗漏")

	// 逐条核对落点与判定一致:decision 说 selected,人就必须在 selected 里。否则
	// 计数与理由是两套账,运营者复核的那一套可能根本不是真正执行的那一套。
	inSelected := map[int]bool{}
	for _, entry := range selected {
		inSelected[entry.Index] = true
	}
	for _, decision := range decisions {
		require.Equal(t, decision.Selected, inSelected[decision.Index],
			"第 %d 条的判定与实际落点不一致", decision.Index)
	}

	require.Equal(t,
		[]string{"plain-a@example.invalid", "keep-disabled@example.invalid", "keep-annual@example.invalid", "plain-b@example.invalid"},
		mirasimSelectionTestEmails(t, selected))
	require.Equal(t,
		[]string{"disabled-a@example.invalid", "annual-a@example.invalid", "skip-healthy@example.invalid", "contested@example.invalid"},
		mirasimSelectionTestEmails(t, reserved))

	// ── 差分阴性:同一批来源,只把 SkipDisabled 关掉 ──
	// 守恒式是不变量,它在任何策略下都成立;所以守恒断言本身无法区分「实现正确」
	// 与「实现永远全选」。切分必须随开关移动,这一条才是鉴别力的来源。
	relaxed := policy
	relaxed.SkipDisabled = false
	relaxedSelected, relaxedReserved, relaxedDecisions := selectMirasimImportCandidates(entries, relaxed)
	require.Equal(t, len(entries), len(relaxedSelected)+len(relaxedReserved), "守恒式与策略无关")
	require.Len(t, relaxedDecisions, len(entries))
	require.NotEqual(t, len(selected), len(relaxedSelected), "关掉一条规则后导入集必须变大")
	require.Equal(t, len(selected)+1, len(relaxedSelected), "本样本里只有 disabled-a 一个号是靠 SkipDisabled 挡住的")
}

// TestSelectMirasimImportCandidatesReservesUnreadablePlanExpiry 钉住一个刻意的
// fail-closed:来源确实声明了订阅到期、但这一份读不出时间时,保留而不是导入。
//
// 「没声明」和「声明了读不懂」必须分开:大量来源记录本来就不带 plan_expires_at,
// 把它们一起按不可读保留,这条规则就从「挡住 2027 的自用号」变成「挡住绝大多数
// 号」。反过来把读不懂当没声明,则那个 2027 的号可能正好就是格式怪的那条。
func TestSelectMirasimImportCandidatesReservesUnreadablePlanExpiry(t *testing.T) {
	entries := mirasimSelectionTestEntries(
		mirasimSelectionTestRecord("unreadable@example.invalid", map[string]any{"plan_expires_at": "一年后"}),
		mirasimSelectionTestRecord("absent@example.invalid", nil),
	)

	selected, reserved, decisions := selectMirasimImportCandidates(entries, MirasimImportSelectionPolicy{SkipExpiringAfter: mirasimSelectionTestCutoff})

	require.Equal(t, []string{"absent@example.invalid"}, mirasimSelectionTestEmails(t, selected))
	require.Equal(t, []string{"unreadable@example.invalid"}, mirasimSelectionTestEmails(t, reserved))
	require.Equal(t, mirasimImportReasonExpiryUnreadable, mirasimSelectionTestDecision(t, decisions, 1).Reason)
	require.Equal(t, mirasimImportReasonDefault, mirasimSelectionTestDecision(t, decisions, 2).Reason)

	// 差分阴性:不设 cutoff 时这条规则整个不参与,读不懂的字段不该自己变成保留理由。
	noCutoffSelected, _, noCutoffDecisions := selectMirasimImportCandidates(entries, MirasimImportSelectionPolicy{})
	require.Len(t, noCutoffSelected, len(entries))
	require.NotEqual(t, mirasimImportReasonExpiryUnreadable, mirasimSelectionTestDecision(t, noCutoffDecisions, 1).Reason)
}

// TestMirasimImportSelectionPolicyValidateRejectsUnparsableCutoff:写坏的 cutoff
// 必须在 HTTP 边界上被拒,而不是变成一条静默失效的规则。
func TestMirasimImportSelectionPolicyValidateRejectsUnparsableCutoff(t *testing.T) {
	require.NoError(t, MirasimImportSelectionPolicy{SkipDisabled: true}.validate(),
		"不设 cutoff 是合法的:那是「我要筛,但这批不设到期上界」")
	require.NoError(t, MirasimImportSelectionPolicy{SkipExpiringAfter: mirasimSelectionTestCutoff}.validate())

	err := MirasimImportSelectionPolicy{SkipExpiringAfter: "2026-12-31"}.validate()
	require.Error(t, err)
	// 报错要点名是哪个字段:运营者手上是一份策略 JSON,"invalid time" 不告诉他改哪。
	require.Contains(t, err.Error(), "skip_expiring_after")

	// 纯函数侧的兜底:validate 让这条在 HTTP 路径上不可达,但别的调用方仍可能递进
	// 来。此时整批保留(点名的除外)—— 「一个号都没导」运营者立刻会发现,而「规则
	// 悄悄失效、自用号进了生产池」没有任何可见信号。
	entries := mirasimSelectionTestEntries(
		mirasimSelectionTestRecord("ordinary@example.invalid", nil),
		mirasimSelectionTestRecord("named@example.invalid", nil),
	)
	selected, reserved, decisions := selectMirasimImportCandidates(entries, MirasimImportSelectionPolicy{
		SkipExpiringAfter: "2026-12-31",
		ForceKeep:         []string{"named@example.invalid"},
	})
	require.Equal(t, []string{"named@example.invalid"}, mirasimSelectionTestEmails(t, selected))
	require.Equal(t, []string{"ordinary@example.invalid"}, mirasimSelectionTestEmails(t, reserved))
	require.Equal(t, mirasimImportReasonPolicyUnreadable, mirasimSelectionTestDecision(t, decisions, 1).Reason)
}

// TestImportMirasimAccountsAppliesSelectionBeforeWriting 是上面那组纯函数测试的
// 激活证据,不承担某条义务。
//
// 它存在的理由:selectMirasimImportCandidates 如果压根没被 importMirasimAccounts
// 调用,前面五个测试依然全绿,而线上一个号都不会被筛掉。「写了」不等于「跑到」,
// 只有从导入入口进去、看真实的 CreateAccount 调用,才能把这两件事分开。
func TestImportMirasimAccountsAppliesSelectionBeforeWriting(t *testing.T) {
	seeds := []string{
		mirasimImportTestSeed(t, "test-seed-sel01"),
		mirasimImportTestSeed(t, "test-seed-sel02"),
	}
	buildEntries := func() []mirasimImportEntry {
		return []mirasimImportEntry{
			{Index: 1, Value: map[string]any{
				"name":                "mirasim-keep",
				"email":               "keep@example.invalid",
				"mirasim_device_seed": seeds[0],
				"access_token":        "test-access-token-keep",
				"refresh_token":       "test-refresh-token-keep",
			}},
			{Index: 2, Value: map[string]any{
				"name":                "mirasim-retired",
				"email":               "retired@example.invalid",
				"mirasim_device_seed": seeds[1],
				"access_token":        "test-access-token-retired",
				"refresh_token":       "test-refresh-token-retired",
				"disabled":            true,
			}},
		}
	}

	svc := newCodexImportMemoryAdminService(nil)
	handler := mirasimImportTestHandler(svc)
	req := mirasimImportTestRequest()
	req.Selection = &MirasimImportSelectionPolicy{SkipDisabled: true}

	result, err := handler.importMirasimAccounts(context.Background(), req, buildEntries())
	require.NoError(t, err)

	// Total 是来源总数,不是尝试导入数:被保留的号必须仍然出现在账上。
	require.Equal(t, 2, result.Total)
	require.Equal(t, 1, result.Selected)
	require.Equal(t, 1, result.Reserved)
	require.Equal(t, 1, result.Created)
	require.Equal(t, result.Total, result.Selected+result.Reserved)
	require.Equal(t, result.Total, result.Created+result.Updated+result.Skipped+result.Failed+result.Reserved)
	// 真正的判据:被保留的账号没有落到写入侧。计数对但账号照样被建出来,是最坏的
	// 一种绿 —— 报表说没导,库里有。
	require.Len(t, svc.createdAccounts, 1)
	require.Equal(t, "mirasim-keep", svc.createdAccounts[0].Name)
	require.Len(t, result.Decisions, 2)
	require.Equal(t, mirasimImportReasonDisabled, mirasimSelectionTestDecision(t, result.Decisions, 2).Reason)

	// ── 差分阴性:同一批来源,只把 selection 去掉 ──
	// 不传 selection 必须与筛选层加入之前完全一致:两个号都导入,一条判定都不产生。
	plainSvc := newCodexImportMemoryAdminService(nil)
	plainHandler := mirasimImportTestHandler(plainSvc)
	plainReq := mirasimImportTestRequest()
	plainReq.Selection = nil

	plainResult, err := plainHandler.importMirasimAccounts(context.Background(), plainReq, buildEntries())
	require.NoError(t, err)
	require.Equal(t, 2, plainResult.Created)
	require.Equal(t, 0, plainResult.Reserved)
	require.Equal(t, 2, plainResult.Selected, "没传策略时 Selected 如实等于来源总数,不是 0")
	require.Len(t, plainSvc.createdAccounts, 2)
	require.Len(t, plainResult.Decisions, 0, "没筛选就没有判定,空判定不该被伪造出来")
	require.NotEqual(t, len(svc.createdAccounts), len(plainSvc.createdAccounts))
}
