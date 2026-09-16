package admin

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type balanceOperationCall struct {
	userID    int64
	balance   float64
	operation string
	notes     string
}

type balanceOperationAdminServiceStub struct {
	*stubAdminService
	calls   []balanceOperationCall
	execErr error
	balance float64
}

func (s *balanceOperationAdminServiceStub) UpdateUserBalance(
	_ context.Context,
	userID int64,
	balance float64,
	operation string,
	notes string,
) (*service.User, error) {
	s.calls = append(s.calls, balanceOperationCall{userID: userID, balance: balance, operation: operation, notes: notes})
	if s.execErr != nil {
		return nil, s.execErr
	}
	return &service.User{ID: userID, Balance: s.balance, Status: service.StatusActive}, nil
}

func setupBalanceRouter(serviceStub service.AdminService) *gin.Engine {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	handler := NewUserHandler(serviceStub, nil, nil, nil, nil, nil, nil)
	router.POST("/api/v1/admin/users/:id/balance", handler.UpdateBalance)
	return router
}

func postBalance(t *testing.T, router *gin.Engine, body string) *httptest.ResponseRecorder {
	t.Helper()
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/api/v1/admin/users/3/balance", bytes.NewReader([]byte(body)))
	request.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(recorder, request)
	return recorder
}

// set_zero 是"提取全部/清空余额"的服务端动作：请求体里没有金额也必须被接受，
// 因为金额正是那条缺陷的来源——前端把 GET 到的余额（float64 往返后的
// 999999876.7370788，真值 999999876.73707879）当扣减量回传，Postgres 用精确十进制
// 算出 balance+delta = -0.00000001 而拒绝整条 UPDATE。
// 生产 2026-09-16 18:38:35.896+08 与 18:38:37.949+08 两次点击都栽在这里
// （audit_logs 1709/1710，action=admin.users.balance.create，status_code=500）。
func TestUserHandlerUpdateBalanceAcceptsSetZeroWithoutAmount(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{name: "amount omitted", body: `{"operation":"set_zero"}`},
		{name: "amount zero", body: `{"operation":"set_zero","balance":0}`},
		{name: "amount echoed back by the client is ignored", body: `{"operation":"set_zero","balance":999999876.7370788}`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			serviceStub := &balanceOperationAdminServiceStub{stubAdminService: newStubAdminService()}
			recorder := postBalance(t, setupBalanceRouter(serviceStub), tt.body)

			require.Equal(t, http.StatusOK, recorder.Code, "body=%s", recorder.Body.String())
			require.Len(t, serviceStub.calls, 1)
			require.Equal(t, "set_zero", serviceStub.calls[0].operation)
			require.Equal(t, int64(3), serviceStub.calls[0].userID)
		})
	}
}

// 差分阴性：set_zero 的放宽必须只作用于 set_zero。
// 其余动作的契约仍然是原来的 `required,gt=0`——顺手把 gt=0 删掉会连带放开
// set/add/subtract 的 0 与负数，那是另一个缺陷而不是修复。
func TestUserHandlerUpdateBalanceKeepsPositiveAmountRuleForOtherOperations(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{name: "add zero", body: `{"operation":"add","balance":0}`},
		{name: "add negative", body: `{"operation":"add","balance":-5}`},
		{name: "subtract zero", body: `{"operation":"subtract","balance":0}`},
		{name: "subtract negative", body: `{"operation":"subtract","balance":-0.00000001}`},
		{name: "set zero amount still needs the set_zero operation", body: `{"operation":"set","balance":0}`},
		{name: "set negative", body: `{"operation":"set","balance":-1}`},
		{name: "amount omitted", body: `{"operation":"subtract"}`},
		{name: "unknown operation", body: `{"operation":"multiply","balance":1}`},
		{name: "operation omitted", body: `{"balance":1}`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			serviceStub := &balanceOperationAdminServiceStub{stubAdminService: newStubAdminService()}
			recorder := postBalance(t, setupBalanceRouter(serviceStub), tt.body)

			require.Equal(t, http.StatusBadRequest, recorder.Code, "body=%s", recorder.Body.String())
			require.Empty(t, serviceStub.calls, "被 binding 拒掉的请求不该到达 service")
		})
	}
}

// 差分阴性：正常的调账请求原样透传，没有被新动作或新校验改掉。
func TestUserHandlerUpdateBalancePassesThroughNormalOperations(t *testing.T) {
	tests := []struct {
		name string
		body string
		want balanceOperationCall
	}{
		{
			name: "add",
			body: `{"operation":"add","balance":12.5,"notes":"topup"}`,
			want: balanceOperationCall{userID: 3, balance: 12.5, operation: "add", notes: "topup"},
		},
		{
			name: "subtract",
			body: `{"operation":"subtract","balance":0.73707879}`,
			want: balanceOperationCall{userID: 3, balance: 0.73707879, operation: "subtract"},
		},
		{
			name: "set",
			body: `{"operation":"set","balance":10000}`,
			want: balanceOperationCall{userID: 3, balance: 10000, operation: "set"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			serviceStub := &balanceOperationAdminServiceStub{stubAdminService: newStubAdminService()}
			recorder := postBalance(t, setupBalanceRouter(serviceStub), tt.body)

			require.Equal(t, http.StatusOK, recorder.Code, "body=%s", recorder.Body.String())
			require.Equal(t, []balanceOperationCall{tt.want}, serviceStub.calls)
		})
	}
}

// 把服务层错误 → HTTP 状态码这一段钉死，它是"生产上返回 500"的另一半机制。
//
// 旧实现在 admin_user.go 里用 fmt.Errorf 重造错误，把 ErrBalanceNegative 的分类整个丢了，
// 于是走到这里只能回落 500（infraerrors.FromError 的 UnknownCode）。
// 下面两条互为对照：带分类的 → 400，裸 error（旧写法的形状）→ 500。
func TestUserHandlerUpdateBalanceMapsClassifiedServiceErrorTo400(t *testing.T) {
	classified := infraerrors.Clone(service.ErrBalanceNegative)
	classified.Message = "balance cannot be negative: current balance 999999876.73707879, requested change -999999876.7370788 (value as sent to the database) would make it negative"

	serviceStub := &balanceOperationAdminServiceStub{stubAdminService: newStubAdminService(), execErr: classified}
	recorder := postBalance(t, setupBalanceRouter(serviceStub), `{"operation":"subtract","balance":999999876.7370788}`)

	require.Equal(t, http.StatusBadRequest, recorder.Code, "body=%s", recorder.Body.String())
	require.Contains(t, recorder.Body.String(), "BALANCE_NEGATIVE")
	require.Len(t, serviceStub.calls, 1)

	// 对照：旧实现返回的那种裸 error，走同一条路会变成 500。
	legacyShaped := fmt.Errorf(
		"balance cannot be negative, current balance: %.2f, requested operation would result in: %.2f",
		999999876.7370788, 0.0,
	)
	legacyStub := &balanceOperationAdminServiceStub{stubAdminService: newStubAdminService(), execErr: legacyShaped}
	legacyRecorder := postBalance(t, setupBalanceRouter(legacyStub), `{"operation":"subtract","balance":999999876.7370788}`)
	require.Equal(t, http.StatusInternalServerError, legacyRecorder.Code,
		"这正是生产 audit_logs 1709/1710 记成 500 的机制")
}
