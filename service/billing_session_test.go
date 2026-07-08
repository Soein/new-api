package service

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/model"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestBillingSessionTrustDisabledForResponsesImageGenerationTool(t *testing.T) {
	gin.SetMode(gin.TestMode)
	setWalletTrustBypassConfig(t, true, 100, 20)

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)

	session := &BillingSession{
		relayInfo: &relaycommon.RelayInfo{
			UserId:         385,
			TokenUnlimited: true,
			UserQuota:      common.GetWalletTrustBypassMinQuota() + 1,
			ResponsesUsageInfo: &relaycommon.ResponsesUsageInfo{
				BuiltInTools: map[string]*relaycommon.BuildInToolInfo{
					dto.BuildInToolImageGeneration: {
						ToolName: dto.BuildInToolImageGeneration,
					},
				},
			},
		},
		funding: &WalletFunding{userId: 385},
	}

	require.False(t, session.shouldTrust(ctx, 1))
}

func TestBillingSessionTrustDisabledForWalletFundingByDefault(t *testing.T) {
	gin.SetMode(gin.TestMode)
	setWalletTrustBypassConfig(t, false, 100, 20)

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)

	session := &BillingSession{
		relayInfo: &relaycommon.RelayInfo{
			UserId:         385,
			TokenUnlimited: true,
			UserQuota:      common.GetWalletTrustBypassMinQuota() + 1,
		},
		funding: &WalletFunding{userId: 385},
	}

	require.False(t, session.shouldTrust(ctx, 1))
}

func TestBillingSessionTrustEnabledForHighQuotaWalletFunding(t *testing.T) {
	gin.SetMode(gin.TestMode)
	setWalletTrustBypassConfig(t, true, 100, 20)

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)

	session := &BillingSession{
		relayInfo: &relaycommon.RelayInfo{
			UserId:         385,
			TokenUnlimited: true,
			UserQuota:      common.GetWalletTrustBypassMinQuota() + 1,
		},
		funding: &WalletFunding{userId: 385},
	}

	require.True(t, session.shouldTrust(ctx, common.GetWalletTrustBypassMaxInflightQuota()))
}

func TestWalletTrustBypassFallsBackToPreConsumeWhenInflightCapExceeded(t *testing.T) {
	truncate(t)
	gin.SetMode(gin.TestMode)
	setWalletTrustBypassConfig(t, true, 100, 20)

	const userID = 2002
	initialQuota := common.GetWalletTrustBypassMinQuota() + 1000
	seedUser(t, userID, initialQuota)

	maxInflight := common.GetWalletTrustBypassMaxInflightQuota()
	require.True(t, reserveWalletTrustInflight(userID, maxInflight-1, maxInflight))

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)

	session := &BillingSession{
		relayInfo: &relaycommon.RelayInfo{
			UserId:         userID,
			TokenUnlimited: true,
			UserQuota:      initialQuota,
			IsPlayground:   true,
		},
		funding: &WalletFunding{userId: userID},
	}

	apiErr := session.preConsume(ctx, 2)
	require.Nil(t, apiErr)
	require.False(t, session.trusted)
	require.Equal(t, 2, session.preConsumedQuota)

	var quota int
	require.NoError(t, model.DB.Model(&model.User{}).Where("id = ?", userID).Select("quota").Scan(&quota).Error)
	require.Equal(t, initialQuota-2, quota)
}

func TestWalletTrustBypassReleasesInflightAfterSettle(t *testing.T) {
	truncate(t)
	gin.SetMode(gin.TestMode)
	setWalletTrustBypassConfig(t, true, 100, 20)

	const userID = 2003
	initialQuota := common.GetWalletTrustBypassMinQuota() + 1000
	seedUser(t, userID, initialQuota)

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)

	session := &BillingSession{
		relayInfo: &relaycommon.RelayInfo{
			UserId:         userID,
			TokenUnlimited: true,
			UserQuota:      initialQuota,
			IsPlayground:   true,
		},
		funding: &WalletFunding{userId: userID},
	}

	apiErr := session.preConsume(ctx, 100)
	require.Nil(t, apiErr)
	require.True(t, session.trusted)
	require.True(t, session.NeedsRefund())
	require.Equal(t, 100, walletTrustInflightQuotaForTest(userID))

	require.NoError(t, session.Settle(150))
	require.False(t, session.NeedsRefund())
	require.Zero(t, walletTrustInflightQuotaForTest(userID))
}

func TestWalletFundingSettleRecordsOverageAsNegativeBalance(t *testing.T) {
	truncate(t)

	const userID = 2001
	seedUser(t, userID, 10)

	funding := &WalletFunding{userId: userID}
	require.NoError(t, funding.Settle(25))

	var quota int
	require.NoError(t, model.DB.Model(&model.User{}).Where("id = ?", userID).Select("quota").Scan(&quota).Error)
	require.Equal(t, -15, quota)
}

func setWalletTrustBypassConfig(t *testing.T, enabled bool, minUsd float64, maxInflightUsd float64) {
	t.Helper()

	oldEnabled := common.WalletTrustBypassEnabled
	oldMinUsd := common.WalletTrustBypassMinUsd
	oldMaxInflightUsd := common.WalletTrustBypassMaxInflightUsd
	oldInflight := walletTrustInflight

	common.WalletTrustBypassEnabled = enabled
	common.WalletTrustBypassMinUsd = minUsd
	common.WalletTrustBypassMaxInflightUsd = maxInflightUsd
	walletTrustInflight = map[int]walletTrustInflightEntry{}

	t.Cleanup(func() {
		common.WalletTrustBypassEnabled = oldEnabled
		common.WalletTrustBypassMinUsd = oldMinUsd
		common.WalletTrustBypassMaxInflightUsd = oldMaxInflightUsd
		walletTrustInflight = oldInflight
	})
}

func walletTrustInflightQuotaForTest(userID int) int {
	walletTrustInflightMu.Lock()
	defer walletTrustInflightMu.Unlock()
	return walletTrustInflight[userID].quota
}
