package mirasim

import (
	"testing"
	"time"
)

// 出口代理（resin）单次请求内的换节点预算：最多 3 个节点、每次 8 秒。
// 这两个数字住在另一个仓库（Resin/internal/proxy/tunnel.go 的
// connectDialMaxAttempts 与 connectDialAttemptTimeout），本仓没法 import，
// 所以在这里显式抄一份并由下面的断言看住。
//
// 抄常量本身有漂移风险，这一点如实承认：对面改了这里不会自动红。
// 但**不抄**的后果更糟 —— 那就是现在这个缺陷的成因：两个预算没有任何地方
// 写明它们互相约束，于是一个被照抄成 10s、另一个被设成 24s，谁也没发现。
const (
	proxyDialMaxAttempts   = 3
	proxyDialAttemptBudget = 8 * time.Second
	proxyWorstCaseConnect  = proxyDialMaxAttempts * proxyDialAttemptBudget // 24s
)

// TestProbeTimeoutsExceedProxyRotationBudget 钉住探测预算与代理换节点预算的关系。
//
// 缺陷形态（2026-09-16 线上实录）：limitsTimeout 照抄 ma-relay 的 10s，而 ma-relay
// 与它的出口池同机、走干净网络、且当时代理层没有换节点重试。本仓的拓扑多了跨境一跳
// 和最坏 24s 的代理建连预算，于是 143 个账号里 43 个（30%）报 timeout —— 那不是上游慢，
// 是**我们在代理还没换完节点的时候就放弃了**。
//
// 这条门判的不是"超时值够不够大"这种主观量，而是一个**可核的关系**：
// 探测预算必须严格大于代理最坏建连预算，否则一次节点轮换就等于一次失败。
func TestProbeTimeoutsExceedProxyRotationBudget(t *testing.T) {
	cases := map[string]time.Duration{
		"limitsTimeout（/v1/limits 额度探测）":       limitsTimeout,
		"referralTimeout（/auth/referral 计划探测）": referralTimeout,
	}
	for name, budget := range cases {
		if budget <= proxyWorstCaseConnect {
			t.Errorf("%s = %v，不大于代理最坏建连预算 %v —— "+
				"代理每换一次节点都会被误判成上游超时", name, budget, proxyWorstCaseConnect)
		}
		// 光"大于"不够：还要留出上游真实处理时间，否则代理刚建完连就没预算了。
		if margin := budget - proxyWorstCaseConnect; margin < 10*time.Second {
			t.Errorf("%s = %v，扣掉代理最坏建连 %v 后只剩 %v 给上游处理 —— 余量太薄",
				name, budget, proxyWorstCaseConnect, margin)
		}
	}
}

// 差分阴性：这条门必须对"配小了"真的会红。
// 没有它，把上面的断言写反（<= 写成 >=）也能让正例过。
func TestProbeTimeoutGateRejectsATooSmallBudget(t *testing.T) {
	tooSmall := 10 * time.Second // 就是出事时的那个值
	if tooSmall > proxyWorstCaseConnect {
		t.Fatalf("仪器失效：%v 本应不大于代理最坏建连预算 %v，判据失去鉴别力",
			tooSmall, proxyWorstCaseConnect)
	}
	margin := 40*time.Second - proxyWorstCaseConnect
	if margin < 10*time.Second {
		t.Fatalf("仪器失效：当前采用的 40s 自己就过不了余量判据（余量 %v）", margin)
	}
}
