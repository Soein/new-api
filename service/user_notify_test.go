package service

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/i18n"
	"github.com/QuantumNous/new-api/model"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/relaykit/types"
	"github.com/QuantumNous/new-api/setting/system_setting"

	"github.com/gin-gonic/gin"
	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func TestWalletQuotaReminderConcurrentCharges(t *testing.T) {
	truncate(t)
	previousEnabled, previousThreshold := common.QuotaRemindEnabled, common.QuotaRemindThreshold
	previousClient, previousWorker := httpClient, system_setting.WorkerUrl
	previousNotifyLimit := constant.NotifyLimitCount
	fetch := system_setting.GetFetchSetting()
	previousFetch := *fetch
	t.Cleanup(func() {
		common.QuotaRemindEnabled, common.QuotaRemindThreshold = previousEnabled, previousThreshold
		httpClient, system_setting.WorkerUrl = previousClient, previousWorker
		constant.NotifyLimitCount = previousNotifyLimit
		*fetch = previousFetch
	})
	common.QuotaRemindEnabled, common.QuotaRemindThreshold = true, 100
	constant.NotifyLimitCount = 100
	fetch.EnableSSRFProtection = false

	received := make(chan WebhookPayload, 2)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload WebhookPayload
		if err := common.DecodeJson(r.Body, &payload); err == nil {
			received <- payload
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(server.Close)
	httpClient, system_setting.WorkerUrl = server.Client(), ""

	const userID = 2901
	seedUser(t, userID, 120)
	settings := dto.UserSetting{NotifyType: dto.NotifyTypeWebhook, WebhookUrl: server.URL}
	// Both requests have already read the same initial balance before either
	// mutation starts, so scheduling cannot hide a stale-snapshot regression.
	infos := []*relaycommon.RelayInfo{
		{UserId: userID, UserQuota: 120, UserSetting: settings, IsPlayground: true},
		{UserId: userID, UserQuota: 120, UserSetting: settings, IsPlayground: true},
	}
	start := make(chan struct{})
	errors := make(chan error, len(infos))
	var workers sync.WaitGroup
	for _, info := range infos {
		workers.Go(func() {
			<-start
			errors <- PostConsumeQuota(info, 15, 0, true)
		})
	}
	close(start)
	workers.Wait()
	close(errors)
	for err := range errors {
		require.NoError(t, err)
	}
	quota, err := model.GetUserQuota(userID, true)
	require.NoError(t, err)
	assert.Equal(t, 90, quota)
	select {
	case notice := <-received:
		assert.Equal(t, dto.NotifyTypeQuotaExceed, notice.Type)
	case <-time.After(3 * time.Second):
		t.Fatal("no reminder after concurrent charges crossed the threshold")
	}

	// Recovery can win confirmation after acceptance is persisted but before
	// the foreground notifier runs. Its winner must still deliver the notice.
	require.NoError(t, model.SetUserQuota(userID, 120))
	require.NoError(t, model.UpdateUserSetting(userID, settings))
	info := &relaycommon.RelayInfo{UserId: userID, UserSetting: settings, IsPlayground: true}
	task := &model.Midjourney{UserId: userID}
	applied, err := ReserveMidjourneyTaskBilling(info, task, 25)
	require.NoError(t, err)
	require.True(t, applied)
	task.Status, task.Progress = "SUCCESS", "100%"
	require.NoError(t, task.Update())
	var recovered model.Midjourney
	require.NoError(t, model.DB.First(&recovered, task.Id).Error)
	require.True(t, ReconcileMidjourneyTaskBilling(context.Background(), &recovered))
	NotifyMidjourneyQuota(info, task)
	select {
	case notice := <-received:
		assert.Equal(t, dto.NotifyTypeQuotaExceed, notice.Type)
	case <-time.After(3 * time.Second):
		t.Fatal("recovery consumed confirmation without delivering its reminder")
	}
	confirmed, err := model.GetWalletSettledQuota(userID)
	require.NoError(t, err)
	assert.Equal(t, 95, confirmed)
}

func TestWalletQuotaReminderUsesCommittedFundingChanges(t *testing.T) {
	truncate(t)
	previousEnabled, previousThreshold := common.QuotaRemindEnabled, common.QuotaRemindThreshold
	previousBatch := common.BatchUpdateEnabled
	t.Cleanup(func() {
		common.QuotaRemindEnabled, common.QuotaRemindThreshold = previousEnabled, previousThreshold
		common.BatchUpdateEnabled = previousBatch
	})
	common.QuotaRemindEnabled, common.QuotaRemindThreshold = true, 100
	for index, tc := range []struct {
		name    string
		reserve int
		batch   bool
	}{
		{name: "no pre-consume", reserve: 0},
		{name: "full pre-consume", reserve: 15},
		{name: "positive settlement delta", reserve: 5, batch: true},
		{name: "partial refund", reserve: 30, batch: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			truncate(t)
			common.BatchUpdateEnabled = tc.batch
			userID := 2910 + index
			seedUser(t, userID, 120)
			sessions := make([]*BillingSession, 2)
			for i := range sessions {
				info := &relaycommon.RelayInfo{UserId: userID, UserQuota: 120, IsPlayground: true}
				session := &BillingSession{
					relayInfo: info,
					funding:   &WalletFunding{userId: userID, captureBalance: true},
				}
				ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
				require.Nil(t, session.preConsume(ctx, tc.reserve))
				info.Billing = session
				sessions[i] = session
			}
			start := make(chan struct{})
			errors := make(chan error, len(sessions))
			var workers sync.WaitGroup
			for _, session := range sessions {
				workers.Go(func() {
					<-start
					errors <- session.Settle(15)
				})
			}
			close(start)
			workers.Wait()
			close(errors)
			for err := range errors {
				require.NoError(t, err)
			}
			notifications := 0
			for _, session := range sessions {
				balance, due := claimWalletQuotaReminder(session.relayInfo, 15)
				if due {
					notifications++
					assert.Equal(t, 90, balance)
				}
				_, repeated := claimWalletQuotaReminder(session.relayInfo, 15)
				assert.False(t, repeated, "re-entering settlement must not repeat a reminder")
			}
			assert.Equal(t, 1, notifications)
			quota, err := model.GetUserQuota(userID, true)
			require.NoError(t, err)
			assert.Equal(t, 90, quota)
		})
	}
}

func TestWalletQuotaReminderRefundAndAdditionalReservations(t *testing.T) {
	truncate(t)
	previousEnabled, previousThreshold := common.QuotaRemindEnabled, common.QuotaRemindThreshold
	t.Cleanup(func() {
		common.QuotaRemindEnabled, common.QuotaRemindThreshold = previousEnabled, previousThreshold
	})
	common.QuotaRemindEnabled, common.QuotaRemindThreshold = true, 100
	const userID = 2920
	seedUser(t, userID, 120)
	ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	info := &relaycommon.RelayInfo{UserId: userID, IsPlayground: true, UserSetting: dto.UserSetting{BillingPreference: "wallet_only"}}
	session, apiErr := NewBillingSession(ctx, info, 30)
	require.Nil(t, apiErr)
	info.Billing = session
	require.NoError(t, session.Settle(15))
	_, due := claimWalletQuotaReminder(info, 15)
	assert.False(t, due, "a refund restored the wallet above the threshold")
	quota, err := model.GetUserQuota(userID, true)
	require.NoError(t, err)
	assert.Equal(t, 105, quota)

	info = &relaycommon.RelayInfo{UserId: userID, IsPlayground: true, UserSetting: dto.UserSetting{BillingPreference: "wallet_only"}}
	session, apiErr = NewBillingSession(ctx, info, 5)
	require.Nil(t, apiErr)
	info.Billing = session
	require.NoError(t, session.ReserveBeforeSubmit(15))
	require.NoError(t, session.ReserveBeforeSubmit(25))
	require.NoError(t, session.Settle(20))
	balance, due := claimWalletQuotaReminder(info, 20)
	assert.True(t, due)
	assert.Equal(t, 85, balance)
	_, repeated := claimWalletQuotaReminder(info, 20)
	assert.False(t, repeated, "multiple reservations still earn one reminder")

	require.NoError(t, model.IncreaseUserQuota(userID, 35, false))
	info = &relaycommon.RelayInfo{UserId: userID, IsPlayground: true, UserSetting: dto.UserSetting{BillingPreference: "wallet_only"}}
	session, apiErr = NewBillingSession(ctx, info, 30)
	require.Nil(t, apiErr)
	info.Billing = session
	require.NoError(t, session.Settle(0))
	_, due = claimWalletQuotaReminder(info, 0)
	assert.False(t, due, "a fully refunded request earns no reminder")
	quota, err = model.GetUserQuota(userID, true)
	require.NoError(t, err)
	assert.Equal(t, 120, quota)
}

func TestWalletQuotaReminderInterleavedReservations(t *testing.T) {
	previousEnabled, previousThreshold := common.QuotaRemindEnabled, common.QuotaRemindThreshold
	t.Cleanup(func() {
		common.QuotaRemindEnabled, common.QuotaRemindThreshold = previousEnabled, previousThreshold
	})
	common.QuotaRemindEnabled, common.QuotaRemindThreshold = true, 100
	for _, tc := range []struct {
		name        string
		aFirst      bool
		aActual     int
		bActual     int
		wantBalance int
		wantNotices int
	}{
		{name: "charge then zero settlement", bActual: 25, wantBalance: 95, wantNotices: 1},
		{name: "zero settlement then charge", aFirst: true, bActual: 25, wantBalance: 95, wantNotices: 1},
		{name: "temporary reservation cannot cause a reminder", aFirst: true, aActual: 5, wantBalance: 115},
	} {
		t.Run(tc.name, func(t *testing.T) {
			truncate(t)
			const userID = 2940
			seedUser(t, userID, 120)
			ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
			ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
			a := &relaycommon.RelayInfo{UserId: userID, IsPlayground: true, UserSetting: dto.UserSetting{BillingPreference: "wallet_only"}}
			b := &relaycommon.RelayInfo{UserId: userID, IsPlayground: true, UserSetting: dto.UserSetting{BillingPreference: "wallet_only"}}
			require.Nil(t, PreConsumeBilling(ctx, 30, a))
			require.Nil(t, PreConsumeBilling(ctx, 25, b))
			confirmed, err := model.GetWalletSettledQuota(userID)
			require.NoError(t, err)
			assert.Equal(t, 120, confirmed, "pending reservations are not confirmed spending")
			var staleUser model.User
			require.NoError(t, model.DB.First(&staleUser, userID).Error)
			staleUser.QuotaReserved = 0
			require.NoError(t, model.DB.Model(&staleUser).Select("*").Updates(&staleUser).Error)
			confirmed, err = model.GetWalletSettledQuota(userID)
			require.NoError(t, err)
			assert.Equal(t, 120, confirmed, "ordinary user updates cannot overwrite pending reservation accounting")
			encoded, err := common.Marshal(staleUser)
			require.NoError(t, err)
			assert.NotContains(t, string(encoded), "quota_reserved")
			order := []struct {
				info   *relaycommon.RelayInfo
				actual int
			}{{b, tc.bActual}, {a, tc.aActual}}
			if tc.aFirst {
				order[0], order[1] = order[1], order[0]
			}
			notices := 0
			for _, item := range order {
				require.NoError(t, item.info.Billing.Settle(item.actual))
				_, due := claimWalletQuotaReminder(item.info, item.actual)
				if due {
					notices++
				}
			}
			balance, err := model.GetUserQuota(userID, true)
			require.NoError(t, err)
			assert.Equal(t, tc.wantBalance, balance)
			assert.Equal(t, tc.wantNotices, notices)
		})
	}
}

func TestWalletQuotaReminderAbandonedReservationExpires(t *testing.T) {
	truncate(t)
	previousEnabled, previousThreshold := common.QuotaRemindEnabled, common.QuotaRemindThreshold
	t.Cleanup(func() { common.QuotaRemindEnabled, common.QuotaRemindThreshold = previousEnabled, previousThreshold })
	common.QuotaRemindEnabled, common.QuotaRemindThreshold = true, 100
	const userID = 2970
	seedUser(t, userID, 120)
	requestContext, abandon := context.WithCancel(context.Background())
	ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil).WithContext(requestContext)
	info := &relaycommon.RelayInfo{UserId: userID, IsPlayground: true, UserSetting: dto.UserSetting{BillingPreference: "wallet_only"}}
	require.Nil(t, PreConsumeBilling(ctx, 30, info))
	abandon()
	// Simulate elapsed owner lifetime without sleeping or changing real money.
	require.NoError(t, model.DB.Table(model.DB.NamingStrategy.TableName("WalletQuotaReservation")).Where("user_id = ?", userID).
		Update("expires_at", time.Now().UnixMilli()-1).Error)
	confirmed, err := model.GetWalletSettledQuota(userID)
	require.NoError(t, err)
	assert.Equal(t, 90, confirmed)
	require.NoError(t, model.IncreaseUserQuota(userID, 30, false))
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	charged := &relaycommon.RelayInfo{UserId: userID, IsPlayground: true, UserSetting: dto.UserSetting{BillingPreference: "wallet_only"}}
	require.Nil(t, PreConsumeBilling(ctx, 30, charged))
	require.NoError(t, charged.Billing.Settle(30))
	balance, due := claimWalletQuotaReminder(charged, 30)
	assert.True(t, due)
	assert.Equal(t, 90, balance)
	// A late owner may still refund its own real reservation after its reminder
	// marker expired; it must not release somebody else's marker or skip money.
	require.NoError(t, info.Billing.Settle(0))
	physical, err := model.GetUserQuota(userID, true)
	require.NoError(t, err)
	assert.Equal(t, 120, physical)
}

func useWalletReservationLease(t *testing.T, duration time.Duration) {
	t.Helper()
	previousLease, previousManager := common.WalletQuotaReservationLease, walletReservationLeases
	common.WalletQuotaReservationLease = duration
	// Lease configuration is fixed for each process; isolate its worker when a
	// test models a different process-level configuration.
	walletReservationLeases = &walletReservationLeaseManager{entries: map[string]func() bool{}, wake: make(chan struct{}, 1)}
	t.Cleanup(func() { common.WalletQuotaReservationLease, walletReservationLeases = previousLease, previousManager })
}

func TestWalletReservationLeaseFollowsLiveRequest(t *testing.T) {
	truncate(t)
	useWalletReservationLease(t, 300*time.Millisecond)
	const userID = 2971
	seedUser(t, userID, 120)
	requestContext, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil).WithContext(requestContext)
	info := &relaycommon.RelayInfo{UserId: userID, IsPlayground: true, UserSetting: dto.UserSetting{BillingPreference: "wallet_only"}}
	require.Nil(t, PreConsumeBilling(ctx, 30, info))
	var initial model.WalletQuotaReservation
	require.NoError(t, model.DB.Where("user_id = ?", userID).Take(&initial).Error)
	// Observe actual renewal across the original lease duration, not a local
	// liveness flag or an assumed goroutine count.
	require.Eventually(t, func() bool {
		var renewed model.WalletQuotaReservation
		err := model.DB.First(&renewed, "request_id = ?", initial.RequestID).Error
		return err == nil && renewed.ExpiresAt > initial.ExpiresAt+300
	}, 2*time.Second, 10*time.Millisecond)
	confirmed, err := model.GetWalletSettledQuota(userID)
	require.NoError(t, err)
	assert.Equal(t, 120, confirmed)
	cancel()
	require.Eventually(t, func() bool {
		confirmed, err := model.GetWalletSettledQuota(userID)
		return err == nil && confirmed == 90
	}, 2*time.Second, 10*time.Millisecond)
	removed, err := model.CleanupExpiredWalletQuotaReservations(time.Now().UnixMilli(), 1)
	require.NoError(t, err)
	assert.EqualValues(t, 1, removed)
	physical, err := model.GetUserQuota(userID, true)
	require.NoError(t, err)
	assert.Equal(t, 90, physical, "expiry and cleanup cannot refund money")
	require.NoError(t, info.Billing.Settle(25))
	require.NoError(t, info.Billing.Settle(25))
	physical, err = model.GetUserQuota(userID, true)
	require.NoError(t, err)
	assert.Equal(t, 95, physical, "late settlement must still refund the difference once")
}

func TestWalletReservationLeaseStopsAfterSettlementFailure(t *testing.T) {
	truncate(t)
	useWalletReservationLease(t, 300*time.Millisecond)
	const userID = 2972
	seedUser(t, userID, 120)
	requestContext, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil).WithContext(requestContext)
	info := &relaycommon.RelayInfo{UserId: userID, IsPlayground: true, UserSetting: dto.UserSetting{BillingPreference: "wallet_only"}}
	require.Nil(t, PreConsumeBilling(ctx, 30, info))
	expected := errors.New("wallet settlement write failed")
	var failOnce sync.Once
	callback := "test:wallet-settlement-write-failure"
	require.NoError(t, model.DB.Callback().Update().Before("gorm:update").Register(callback, func(tx *gorm.DB) {
		if tx.Statement.Schema != nil && tx.Statement.Schema.Name == "User" {
			failOnce.Do(func() { tx.AddError(expected) })
		}
	}))
	t.Cleanup(func() { require.NoError(t, model.DB.Callback().Update().Remove(callback)) })
	require.ErrorIs(t, info.Billing.Settle(35), expected)
	// Even with the request context still alive, a failed final write must not
	// renew a reminder marker forever after its owner discards the session.
	require.Eventually(t, func() bool {
		confirmed, err := model.GetWalletSettledQuota(userID)
		return err == nil && confirmed == 90
	}, 2*time.Second, 10*time.Millisecond)
	require.NoError(t, info.Billing.Settle(0))
	require.NoError(t, info.Billing.Settle(0))
	physical, err := model.GetUserQuota(userID, true)
	require.NoError(t, err)
	assert.Equal(t, 120, physical)
}

func TestWalletFundingRefundWithoutTokenConsumption(t *testing.T) {
	truncate(t)
	const userID = 2973
	seedUser(t, userID, 120)
	funding := &WalletFunding{userId: userID}
	require.NoError(t, funding.PreConsume(30))
	info := &relaycommon.RelayInfo{UserId: userID, IsPlayground: true}
	session := &BillingSession{relayInfo: info, funding: funding, preConsumedQuota: 30}
	info.Billing = session
	require.True(t, session.NeedsRefund())
	ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
	session.Refund(ctx)
	session.Refund(ctx)
	require.Eventually(t, func() bool {
		quota, err := model.GetUserQuota(userID, true)
		return err == nil && quota == 120
	}, time.Second, 10*time.Millisecond)
	require.NoError(t, session.Settle(30))
	physical, err := model.GetUserQuota(userID, true)
	require.NoError(t, err)
	assert.Equal(t, 120, physical)
}

func TestWalletQuotaReminderMidjourneyUsesCommittedChanges(t *testing.T) {
	truncate(t)
	previousEnabled, previousThreshold := common.QuotaRemindEnabled, common.QuotaRemindThreshold
	t.Cleanup(func() {
		common.QuotaRemindEnabled, common.QuotaRemindThreshold = previousEnabled, previousThreshold
	})
	common.QuotaRemindEnabled, common.QuotaRemindThreshold = true, 100
	const userID = 2930
	seedUser(t, userID, 120)
	infos := make([]*relaycommon.RelayInfo, 2)
	for i := range infos {
		info := &relaycommon.RelayInfo{UserId: userID, UserQuota: 120, IsPlayground: true}
		task := &model.Midjourney{UserId: userID}
		applied, err := ReserveMidjourneyTaskBilling(info, task, 15)
		require.NoError(t, err)
		require.True(t, applied)
		task.Status = ""
		require.NoError(t, task.Update())
		change, err := task.ConfirmBilling(true)
		require.NoError(t, err)
		require.NotNil(t, change)
		info.WalletQuotaChanges = append(info.WalletQuotaChanges, *change)
		infos[i] = info
	}
	notifications := 0
	for _, info := range infos {
		balance, due := claimWalletQuotaReminder(info, 15)
		if due {
			notifications++
			assert.Equal(t, 90, balance)
		}
		_, repeated := claimWalletQuotaReminder(info, 15)
		assert.False(t, repeated)
	}
	assert.Equal(t, 1, notifications)
}

func TestWalletQuotaReminderMidjourneyPendingReservations(t *testing.T) {
	previousEnabled, previousThreshold := common.QuotaRemindEnabled, common.QuotaRemindThreshold
	t.Cleanup(func() { common.QuotaRemindEnabled, common.QuotaRemindThreshold = previousEnabled, previousThreshold })
	common.QuotaRemindEnabled, common.QuotaRemindThreshold = true, 100
	for _, mjAccepted := range []bool{false, true} {
		t.Run(fmt.Sprintf("midjourney accepted %t", mjAccepted), func(t *testing.T) {
			truncate(t)
			const userID = 2950
			seedUser(t, userID, 120)
			ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
			ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
			info := &relaycommon.RelayInfo{UserId: userID, IsPlayground: true, UserSetting: dto.UserSetting{BillingPreference: "wallet_only"}}
			mjInfo := &relaycommon.RelayInfo{UserId: userID, IsPlayground: true}
			task := &model.Midjourney{UserId: userID}
			normalReserve, mjReserve := 25, 30
			if mjAccepted {
				normalReserve, mjReserve = 30, 25
			}
			require.Nil(t, PreConsumeBilling(ctx, normalReserve, info))
			applied, err := ReserveMidjourneyTaskBilling(mjInfo, task, mjReserve)
			require.NoError(t, err)
			require.True(t, applied)
			confirmed, err := model.GetWalletSettledQuota(userID)
			require.NoError(t, err)
			assert.Equal(t, 120, confirmed)
			if mjAccepted {
				task.Status = "SUCCESS"
				task.Progress = "100%"
				require.NoError(t, task.Update())
				// Reload the durable task to represent confirmation after restart.
				var reloaded model.Midjourney
				require.NoError(t, model.DB.First(&reloaded, task.Id).Error)
				change, err := reloaded.ConfirmBilling(true)
				require.NoError(t, err)
				require.NotNil(t, change)
				mjInfo.WalletQuotaChanges = append(mjInfo.WalletQuotaChanges, *change)
				balance, due := claimWalletQuotaReminder(mjInfo, mjReserve)
				assert.True(t, due)
				assert.Equal(t, 95, balance)
				change, err = reloaded.ConfirmBilling(true)
				require.NoError(t, err)
				assert.Nil(t, change)
				require.NoError(t, info.Billing.Settle(0))
			} else {
				require.NoError(t, info.Billing.Settle(normalReserve))
				balance, due := claimWalletQuotaReminder(info, normalReserve)
				assert.True(t, due)
				assert.Equal(t, 95, balance)
				task.Status = "FAILURE"
				require.NoError(t, task.Update())
				refunded, err := task.RefundBilling("")
				require.NoError(t, err)
				assert.True(t, refunded.Completed)
				refunded, err = task.RefundBilling("")
				require.NoError(t, err)
				assert.True(t, refunded.Completed)
				assert.False(t, refunded.Applied)
			}
			physical, err := model.GetUserQuota(userID, true)
			require.NoError(t, err)
			assert.Equal(t, 95, physical)
			confirmed, err = model.GetWalletSettledQuota(userID)
			require.NoError(t, err)
			assert.Equal(t, 95, confirmed)
		})
	}
}

func TestMidjourneyWalletConfirmationRejectsUncommittedStates(t *testing.T) {
	truncate(t)
	const userID = 2960
	seedUser(t, userID, 120)
	info := &relaycommon.RelayInfo{UserId: userID, IsPlayground: true}
	task := &model.Midjourney{UserId: userID}
	applied, err := ReserveMidjourneyTaskBilling(info, task, 25)
	require.NoError(t, err)
	require.True(t, applied)
	for _, status := range []string{model.MidjourneyBillingStatusPrepared, model.MidjourneyBillingStatusCharging, model.MidjourneyBillingStatusChargePending, model.MidjourneyBillingStatusRefunding, model.MidjourneyBillingStatusRefundPending, "invalid", ""} {
		require.NoError(t, model.DB.Table("midjourneys").Where("id = ?", task.Id).
			Updates(map[string]any{"billing_status": status, "status": "SUCCESS", "quota": 0}).Error)
		change, err := task.ConfirmBilling(true)
		require.Error(t, err, "uncommitted billing status %q", status)
		assert.Nil(t, change)
		confirmed, err := model.GetWalletSettledQuota(userID)
		require.NoError(t, err)
		assert.Equal(t, 120, confirmed)
	}
	require.NoError(t, model.DB.Table("midjourneys").Where("id = ?", task.Id).
		Updates(map[string]any{"billing_status": model.MidjourneyBillingStatusCharged, "status": constant.MjStatusSubmitUnknown, "progress": "100%", "quota": 25}).Error)
	change, err := task.ConfirmBilling(true)
	require.NoError(t, err)
	assert.Nil(t, change)
	assert.False(t, model.HasUnfinishedMidjourneyTasks(), "an unknown submission with no upstream ID must not spin the scheduler")
}

func TestMidjourneyWalletReservationRepairsLegacyTerminalRefund(t *testing.T) {
	truncate(t)
	const userID = 2961
	seedUser(t, userID, 120)
	info := &relaycommon.RelayInfo{UserId: userID, IsPlayground: true}
	task := &model.Midjourney{UserId: userID}
	applied, err := ReserveMidjourneyTaskBilling(info, task, 30)
	require.NoError(t, err)
	require.True(t, applied)
	// An older process can complete a refund without knowing the new marker.
	// Recovery must retire that marker without crediting the money a second time.
	require.NoError(t, model.DB.Model(&model.User{}).Where("id = ?", userID).Update("quota", 120).Error)
	require.NoError(t, model.DB.Table("midjourneys").Where("id = ?", task.Id).
		Updates(map[string]any{"quota": 0, "billing_status": model.MidjourneyBillingStatusRefunded, "status": "FAILURE", "progress": "100%"}).Error)
	require.True(t, model.HasUnfinishedMidjourneyTasks())
	var reloaded model.Midjourney
	require.NoError(t, model.DB.First(&reloaded, task.Id).Error)
	require.True(t, ReconcileMidjourneyTaskBilling(context.Background(), &reloaded))
	assert.False(t, model.HasUnfinishedMidjourneyTasks())
	confirmed, err := model.GetWalletSettledQuota(userID)
	require.NoError(t, err)
	assert.Equal(t, 120, confirmed)
	physical, err := model.GetUserQuota(userID, true)
	require.NoError(t, err)
	assert.Equal(t, 120, physical)
}

// A low-balance reminder goes out once, on the charge that takes a wallet or
// subscription balance below the threshold. A balance already below it earns
// nothing more until a top-up lifts it back above the line. The site switch
// and the user's switch both turn reminders off; a user who never chose gets
// them.
func TestQuotaReminderFiresOncePerDrop(t *testing.T) {
	previousEnabled, previousThreshold := common.QuotaRemindEnabled, common.QuotaRemindThreshold
	t.Cleanup(func() {
		common.QuotaRemindEnabled, common.QuotaRemindThreshold = previousEnabled, previousThreshold
	})
	common.QuotaRemindThreshold = 1000
	on, off := true, false
	// changes move the balance from start: negative ones are charges, which
	// may remind; positive ones are top-ups. want holds one entry per charge.
	for _, tc := range []struct {
		name        string
		siteEnabled bool
		setting     dto.UserSetting
		start       int64
		changes     []int64
		want        []bool
	}{
		{
			name:        "never chosen: one reminder per drop, again after a top-up",
			siteEnabled: true,
			start:       1500,
			changes:     []int64{-400, -200, -300, +1500, -1200},
			want:        []bool{false, true, false, true},
		},
		{
			name:        "switched on with its own threshold",
			siteEnabled: true,
			setting:     dto.UserSetting{QuotaWarningEnabled: &on, QuotaWarningThreshold: 5000},
			start:       6000,
			changes:     []int64{-500, -600, -2000},
			want:        []bool{false, true, false},
		},
		{
			name:        "a charge that lands exactly on the threshold does not remind",
			siteEnabled: true,
			start:       1200,
			changes:     []int64{-200, -1},
			want:        []bool{false, true},
		},
		{
			name:        "user switched off",
			siteEnabled: true,
			setting:     dto.UserSetting{QuotaWarningEnabled: &off},
			start:       1500,
			changes:     []int64{-600},
			want:        []bool{false},
		},
		{
			name:    "site switched off",
			setting: dto.UserSetting{QuotaWarningEnabled: &on},
			start:   1500,
			changes: []int64{-600},
			want:    []bool{false},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			common.QuotaRemindEnabled = tc.siteEnabled
			got := make([]bool, 0, len(tc.want))
			balance := tc.start
			for _, change := range tc.changes {
				if change < 0 {
					got = append(got, quotaReminderDue(tc.setting, balance, balance+change))
				}
				balance += change
			}
			assert.Equal(t, tc.want, got)
		})
	}
}

// Channel notices go to every enabled administrator whose channel switch is
// on, in that administrator's saved language. Until an administrator chooses,
// root receives them and other administrators do not; other users never do.
func TestChannelNoticesFollowEachAdministratorsSwitch(t *testing.T) {
	previousDB := model.DB
	previousCache, previousRedis := common.MemoryCacheEnabled, common.RedisEnabled
	previousNotifyLimit := constant.NotifyLimitCount
	previousClient, previousWorker := httpClient, system_setting.WorkerUrl
	fetch := system_setting.GetFetchSetting()
	previousFetch := *fetch
	t.Cleanup(func() {
		model.DB = previousDB
		common.MemoryCacheEnabled, common.RedisEnabled = previousCache, previousRedis
		constant.NotifyLimitCount = previousNotifyLimit
		httpClient, system_setting.WorkerUrl = previousClient, previousWorker
		*fetch = previousFetch
	})
	database, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	sqlDB, err := database.DB()
	require.NoError(t, err)
	sqlDB.SetMaxOpenConns(1)
	t.Cleanup(func() { require.NoError(t, sqlDB.Close()) })
	require.NoError(t, database.AutoMigrate(&model.Channel{}, &model.Ability{}, &model.User{}))
	model.DB = database
	common.MemoryCacheEnabled, common.RedisEnabled = false, false
	constant.NotifyLimitCount = 100

	var mutex sync.Mutex
	received := map[string][]string{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload WebhookPayload
		if err := common.DecodeJson(r.Body, &payload); err == nil {
			mutex.Lock()
			recipient := strings.TrimPrefix(r.URL.Path, "/")
			received[recipient] = append(received[recipient], payload.Type+" "+payload.Title)
			mutex.Unlock()
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(server.Close)
	httpClient, system_setting.WorkerUrl = server.Client(), ""
	fetch.EnableSSRFProtection = false

	on, off := true, false
	for _, user := range []struct {
		name     string
		role     int
		status   int
		receives *bool
		language string
	}{
		{"root-never-chose", common.RoleRootUser, common.UserStatusEnabled, nil, ""},
		{"root-turned-off", common.RoleRootUser, common.UserStatusEnabled, &off, ""},
		{"admin-never-chose", common.RoleAdminUser, common.UserStatusEnabled, nil, ""},
		{"admin-turned-on", common.RoleAdminUser, common.UserStatusEnabled, &on, "zhCN"},
		{"disabled-admin-turned-on", common.RoleAdminUser, common.UserStatusDisabled, &on, ""},
		{"user-with-switch-stored-on", common.RoleCommonUser, common.UserStatusEnabled, &on, ""},
	} {
		setting, err := common.Marshal(dto.UserSetting{
			NotifyType:           dto.NotifyTypeWebhook,
			WebhookUrl:           server.URL + "/" + user.name,
			ChannelNotifyEnabled: user.receives,
			Language:             user.language,
		})
		require.NoError(t, err)
		require.NoError(t, database.Create(&model.User{Username: user.name, Role: user.role, Status: user.status, Setting: string(setting), AffCode: user.name}).Error)
	}
	channel := &model.Channel{Name: "notice-review", Key: "fixture-key", Type: 1, Status: common.ChannelStatusEnabled, Group: "default", Models: "test-model"}
	require.NoError(t, channel.Insert())

	DisableChannel(types.ChannelError{ChannelId: channel.Id, ChannelName: channel.Name, AutoBan: true}, "upstream failed")
	EnableChannel(channel.Id, "", channel.Name)
	NotifyChannelWatchers(dto.NotifyTypeChannelTest, func(lang string) (string, string) {
		return i18n.Translate(lang, i18n.MsgChannelTestCompletedSubject), i18n.Translate(lang, i18n.MsgChannelTestCompletedContent)
	})

	disabledType := formatNotifyType(channel.Id, common.ChannelStatusAutoDisabled)
	enabledType := formatNotifyType(channel.Id, common.ChannelStatusEnabled)
	assert.Equal(t, map[string][]string{
		"root-never-chose": {
			fmt.Sprintf(`%s Channel "notice-review" (#%d) has been disabled`, disabledType, channel.Id),
			fmt.Sprintf(`%s Channel "notice-review" (#%d) has been enabled`, enabledType, channel.Id),
			"channel_test Channel test completed",
		},
		"admin-turned-on": {
			fmt.Sprintf("%s 通道「notice-review」（#%d）已被禁用", disabledType, channel.Id),
			fmt.Sprintf("%s 通道「notice-review」（#%d）已被启用", enabledType, channel.Id),
			"channel_test 通道测试完成",
		},
	}, received)
}
