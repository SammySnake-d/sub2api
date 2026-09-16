//go:build unit

package service

import (
	"context"
	"strconv"
	"testing"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// 仪器：一个按 numeric(20,8) 语义工作的 userRepo stub
//
// 既有的 balanceUserRepoStub 全程 float64，和被测系统同构，所以它永远复现不了
// 2026-09-16 那次生产失败：真正的判据在 Postgres 里用精确十进制算
// （user_repo.go AdjustBalance 的 `WHERE ... AND balance + $1 >= 0`），
// 而 Go 侧是二进制浮点。这里把两件事分开还原：
//
//   - 余额本体：decimal.Decimal（列是 numeric(20,8)，见 ent/schema/user.go）
//   - 绑定参数：float64 → decimal.NewFromFloat，它内部就是 strconv.AppendFloat('f',-1,64)，
//     与 lib/pq v1.10.9 encode.go 对 float64 的编码逐字符相同，也就是 Postgres
//     实际解析成 numeric 的那串文本
//
// 拒绝分支刻意保留仓储层现在的行为（另起一条 SELECT 读 current，再在 Go 里用 float64
// 算 New），因为本次修复要证明的正是"即使拿到的是这种已经失真的 change，服务层也不再
// 输出编造的数值、并且分类正确"。
// ---------------------------------------------------------------------------

type decimalBalanceRepoStub struct {
	*userRepoStub
	balance decimal.Decimal
	// changes 记录每次真正落库的变更（拒绝的不进）。
	changes []BalanceChange
}

func newDecimalBalanceRepo(t *testing.T, id int64, literal string) *decimalBalanceRepoStub {
	t.Helper()
	balance, err := decimal.NewFromString(literal)
	require.NoError(t, err)
	repo := &decimalBalanceRepoStub{
		userRepoStub: &userRepoStub{user: &User{ID: id}},
		balance:      balance,
	}
	repo.syncUser()
	return repo
}

// syncUser 复刻 "Scan numeric 到 float64" 这一步的精度损失：
// GetByID 之后服务层拿到的余额只是 float64，不是列里的精确值。
func (s *decimalBalanceRepoStub) syncUser() {
	value, _ := s.balance.Float64()
	s.userRepoStub.user.Balance = value
}

// AdjustBalance 复刻 user_repo.go:943-964。
func (s *decimalBalanceRepoStub) AdjustBalance(_ context.Context, _ int64, delta float64) (BalanceChange, error) {
	if s.userRepoStub == nil || s.userRepoStub.user == nil {
		return BalanceChange{}, ErrUserNotFound
	}
	// Postgres 侧：精确十进制的 balance + $1 >= 0
	next := s.balance.Add(decimal.NewFromFloat(delta))
	if next.IsNegative() {
		// UPDATE 影响 0 行 → 仓储层另起一条 SELECT，再用 float64 重算 New。
		current, _ := s.balance.Float64()
		return BalanceChange{Old: current, New: current + delta}, ErrBalanceNegative
	}
	old, _ := s.balance.Float64()
	s.balance = next
	s.syncUser()
	newValue, _ := s.balance.Float64()
	change := BalanceChange{Old: old, New: newValue}
	s.changes = append(s.changes, change)
	return change, nil
}

// SetBalance 复刻 user_repo.go:967-991。
func (s *decimalBalanceRepoStub) SetBalance(_ context.Context, _ int64, value float64) (BalanceChange, error) {
	if s.userRepoStub == nil || s.userRepoStub.user == nil {
		return BalanceChange{}, ErrUserNotFound
	}
	current, _ := s.balance.Float64()
	if value < 0 {
		return BalanceChange{Old: current, New: value}, ErrBalanceNegative
	}
	s.balance = decimal.NewFromFloat(value)
	s.syncUser()
	newValue, _ := s.balance.Float64()
	change := BalanceChange{Old: current, New: newValue}
	s.changes = append(s.changes, change)
	return change, nil
}

func newDecimalBalanceService(repo *decimalBalanceRepoStub) *adminServiceImpl {
	return &adminServiceImpl{
		userRepo:       repo,
		redeemCodeRepo: &balanceRedeemRepoStub{redeemRepoStub: &redeemRepoStub{}},
	}
}

// 生产那条余额的真值（由 redeem_codes 里 -999999876.00000000 / -0.73707879 / +10000
// 三条调账记录反推），以及前端 float64 往返后回传的值。
const (
	prodBalanceLiteral = "999999876.73707879"
	prodWithdrawAll    = 999999876.7370788
)

// ---------------------------------------------------------------------------
// G1 正对照：先证明仪器照得到，再信它的读数
// ---------------------------------------------------------------------------

// 这个 stub 只有在"绑定参数的十进制文本"与 lib/pq 一致时才算复刻了生产语义。
func TestDecimalBalanceStub_EncodesParamsLikeLibPQ(t *testing.T) {
	for _, value := range []float64{prodWithdrawAll, 0.73707879, 12.5, -4, 1e-8} {
		require.Equal(t,
			strconv.FormatFloat(value, 'f', -1, 64), // lib/pq encode.go 的写法
			decimal.NewFromFloat(value).String(),
			"stub 的参数编码必须和 lib/pq 一致，否则复现的不是生产语义",
		)
	}
}

// 正对照：同一组输入，float64 同构的仓储会放行，精确十进制的仓储会拒绝。
// 这就是既有 admin_service_update_balance_test.go 抓不到这个缺陷的原因。
func TestDecimalBalanceStub_ReproducesProductionRejection(t *testing.T) {
	require.Equal(t, prodWithdrawAll, 999999876.73707879,
		"两个十进制字面量落到同一个 float64，前端回传时的精度损失正发生在这里")

	repo := newDecimalBalanceRepo(t, 7, prodBalanceLiteral)
	_, err := repo.AdjustBalance(context.Background(), 7, -prodWithdrawAll)
	require.ErrorIs(t, err, ErrBalanceNegative, "精确十进制下 balance+delta = -0.00000001，必须拒绝")

	residual := repo.balance.Add(decimal.NewFromFloat(-prodWithdrawAll))
	require.Equal(t, "-0.00000001", residual.String(), "真实残差就是 1e-8，不是舍入伪影")

	// 同一组输入喂给旧的 float64 stub：它会放行，证明旧仪器对这个缺陷完全失明。
	floatRepo := &balanceUserRepoStub{userRepoStub: &userRepoStub{user: &User{ID: 7, Balance: 999999876.73707879}}}
	_, floatErr := floatRepo.AdjustBalance(context.Background(), 7, -prodWithdrawAll)
	require.NoError(t, floatErr)
}

// ---------------------------------------------------------------------------
// 阳性：复现 2026-09-16 18:38 的生产失败
// ---------------------------------------------------------------------------

// 生产表现：管理员在用户详情里点 UI 自带的"提取全部"，前端把 GET 回来的余额
// （float64 往返后的 999999876.7370788）当扣减量回传，后端返回 500。
// audit_logs id=1709/1710，2026-09-16 18:38:35.896+08 与 18:38:37.949+08，
// body {"balance":999999876.7370788,"notes":"","operation":"subtract"}，status_code 都是 500。
//
// 这里钉死两件事：
//  1. 它是 400（用户输入错误），不是 500。旧实现 fmt.Errorf 另造裸 error 丢掉了
//     ErrBalanceNegative 的分类，errors.ToHTTP 只能回落 500。
//  2. 错误信息里不能再出现那个编造的结果值 "0.00"。
func TestAdminService_UpdateUserBalance_PrecisionRejectionIsClientErrorNotServerError(t *testing.T) {
	repo := newDecimalBalanceRepo(t, 7, prodBalanceLiteral)
	svc := newDecimalBalanceService(repo)

	_, err := svc.UpdateUserBalance(context.Background(), 7, prodWithdrawAll, BalanceOperationSubtract, "")
	require.Error(t, err)

	require.ErrorIs(t, err, ErrBalanceNegative, "必须保留 BALANCE_NEGATIVE 分类")
	status, body := infraerrors.ToHTTP(err) // response.ErrorFrom 走的就是这一条
	require.Equal(t, 400, status, "纯管理员输入错误不该记成服务端内部错误（生产实测 500）")
	require.Equal(t, "BALANCE_NEGATIVE", body.Reason)

	require.Equal(t, prodBalanceLiteral, repo.balance.StringFixed(balanceScale), "被拒的调整不得落库")
	require.Empty(t, repo.changes)
}

// 错误信息里的数字必须都能被核对，不能是 Go 侧 float64 重算出来的"结果"。
//
// 旧信息是 "balance cannot be negative, current balance: 999999876.74,
// requested operation would result in: 0.00"——那个 0.00 是 change.Old+delta 的
// float64 结果（该量级 ulp≈2.4e-7 ≫ 1e-8，差额被抹平成 +0），既不是"设为 0"也不是
// 数据库的真实结果。指纹：真负数 %.2f 会带负号打成 "-0.00"，生产日志里没有负号。
func TestAdminService_UpdateUserBalance_ErrorReportsCheckableNumbers(t *testing.T) {
	repo := newDecimalBalanceRepo(t, 7, prodBalanceLiteral)
	svc := newDecimalBalanceService(repo)

	_, err := svc.UpdateUserBalance(context.Background(), 7, prodWithdrawAll, BalanceOperationSubtract, "")
	require.Error(t, err)

	_, body := infraerrors.ToHTTP(err)

	require.NotContains(t, body.Message, "0.00,", "不得再输出 float64 重算出来的结果值")
	require.NotContains(t, body.Message, "result in: 0.00")
	require.NotContains(t, body.Message, "0.00000000", "补到 %.8f 也只是把假数字换个写法")

	// 发给 Postgres 的那串文本必须原样出现，运维拿它和 current 相减就能看见 1e-8。
	require.Contains(t, body.Message, "-999999876.7370788")
	require.Equal(t, "-999999876.7370788", body.Metadata["requested_delta"])
	require.Equal(t, BalanceOperationSubtract, body.Metadata["operation"])

	// current 按列标度渲染（%.2f 的 999999876.74 把 8 位小数全抹了）。
	require.Equal(t, "999999876.73707879", body.Metadata["current_balance"])
	require.Contains(t, body.Message, "999999876.73707879")
	require.NotContains(t, body.Message, "999999876.74")
}

// 治本路径：清零不经过客户端的余额往返。
// 前端"提取全部"应改调 operation=set_zero（前端改动不在本次文件域内）。
func TestAdminService_UpdateUserBalance_SetZeroClearsExactlyWithoutClientAmount(t *testing.T) {
	repo := newDecimalBalanceRepo(t, 7, prodBalanceLiteral)
	svc := newDecimalBalanceService(repo)

	user, err := svc.UpdateUserBalance(context.Background(), 7, 0, BalanceOperationSetZero, "withdraw all")
	require.NoError(t, err, "清零不该受 float64 往返影响")
	require.True(t, repo.balance.IsZero(), "余额必须是精确的 0，实得 %s", repo.balance.String())
	require.Equal(t, 0.0, user.Balance)
	require.Equal(t, []BalanceChange{{Old: 999999876.7370788, New: 0}}, repo.changes)
}

// set_zero 的值只能由服务端给：客户端仍然送了金额也一律忽略，
// 否则这条路径就又变成"客户端说了算"，缺陷原样回来。
func TestAdminService_UpdateUserBalance_SetZeroIgnoresClientAmount(t *testing.T) {
	for _, clientAmount := range []float64{0, prodWithdrawAll, 123.45, -7} {
		repo := newDecimalBalanceRepo(t, 7, prodBalanceLiteral)
		svc := newDecimalBalanceService(repo)

		_, err := svc.UpdateUserBalance(context.Background(), 7, clientAmount, BalanceOperationSetZero, "")
		require.NoError(t, err)
		require.True(t, repo.balance.IsZero(), "client amount %v 不该影响清零结果", clientAmount)
	}
}

// ---------------------------------------------------------------------------
// 阴性/差分：修复不能变成"无条件放行"，正常路径也不能受影响
// ---------------------------------------------------------------------------

// 真的会把余额扣成负数时，必须照样拒绝、照样不落库，且报的数字仍然可核对。
// （少了这条，把 AdjustBalance 的拒绝分支整个删掉也能让上面的阳性用例变绿。）
func TestAdminService_UpdateUserBalance_GenuineOverdraftStillRejected(t *testing.T) {
	tests := []struct {
		name        string
		balance     string
		amount      float64
		wantCurrent string
		wantDelta   string
	}{
		{name: "small", balance: "3.00000000", amount: 4, wantCurrent: "3.00000000", wantDelta: "-4"},
		{
			name:        "large magnitude, clearly over",
			balance:     prodBalanceLiteral,
			amount:      999999877,
			wantCurrent: "999999876.73707879",
			wantDelta:   "-999999877",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			repo := newDecimalBalanceRepo(t, 7, tt.balance)
			svc := newDecimalBalanceService(repo)

			_, err := svc.UpdateUserBalance(context.Background(), 7, tt.amount, BalanceOperationSubtract, "")
			require.Error(t, err)
			require.ErrorIs(t, err, ErrBalanceNegative)

			status, body := infraerrors.ToHTTP(err)
			require.Equal(t, 400, status)
			require.Equal(t, tt.wantCurrent, body.Metadata["current_balance"])
			require.Equal(t, tt.wantDelta, body.Metadata["requested_delta"])

			require.Equal(t, tt.balance, repo.balance.StringFixed(balanceScale), "被拒的调整不得落库")
			require.Empty(t, repo.changes)
		})
	}
}

// 正常量级、正常输入必须一字不差地照旧工作：精确十进制下的加减结果没有漂移，
// 也没有被新的 set_zero 分支或新的错误构造顺手改掉。
func TestAdminService_UpdateUserBalance_NormalOperationsUnaffected(t *testing.T) {
	tests := []struct {
		name      string
		operation string
		amount    float64
		want      string
	}{
		{name: "add", operation: BalanceOperationAdd, amount: 0.00000001, want: "10.50000001"},
		{name: "subtract", operation: BalanceOperationSubtract, amount: 4.25, want: "6.25"},
		{name: "subtract to exact zero", operation: BalanceOperationSubtract, amount: 10.5, want: "0"},
		{name: "set", operation: BalanceOperationSet, amount: 2, want: "2"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			repo := newDecimalBalanceRepo(t, 7, "10.50000000")
			svc := newDecimalBalanceService(repo)

			user, err := svc.UpdateUserBalance(context.Background(), 7, tt.amount, tt.operation, "")
			require.NoError(t, err)
			require.Equal(t, tt.want, repo.balance.String())
			require.Len(t, repo.changes, 1)

			wantFloat, _ := repo.balance.Float64()
			require.Equal(t, wantFloat, user.Balance)
		})
	}
}

// set 一个负数仍然被拒（走 SetBalance 的拒绝分支），并且信息里报的是
// "requested_balance" 而不是增量——两条拒绝路径的措辞不能串。
func TestAdminService_UpdateUserBalance_SetNegativeRejected(t *testing.T) {
	repo := newDecimalBalanceRepo(t, 7, "10.50000000")
	svc := newDecimalBalanceService(repo)

	_, err := svc.UpdateUserBalance(context.Background(), 7, -1.5, BalanceOperationSet, "")
	require.Error(t, err)
	require.ErrorIs(t, err, ErrBalanceNegative)

	status, body := infraerrors.ToHTTP(err)
	require.Equal(t, 400, status)
	require.Equal(t, "-1.5", body.Metadata["requested_balance"])
	require.NotContains(t, body.Metadata, "requested_delta")
	require.Equal(t, "10.50000000", repo.balance.StringFixed(balanceScale))
}
