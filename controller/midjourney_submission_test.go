package controller

import (
	"context"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMidjourneyPollerProtectsSubmissionLifecycle(t *testing.T) {
	tests := []struct {
		name              string
		status            string
		billingStatus     string
		submitTime        int64
		wantStatus        string
		wantBillingStatus string
		wantQuota         int
		wantFailed        int
		wantProgress      string
		wantUnfinished    bool
	}{
		{
			name:              "fresh reservation is not cancelled",
			status:            constant.MjStatusReserving,
			billingStatus:     model.MidjourneyBillingStatusPrepared,
			submitTime:        time.Now().UnixMilli(),
			wantStatus:        constant.MjStatusReserving,
			wantBillingStatus: model.MidjourneyBillingStatusPrepared,
			wantQuota:         3000,
			wantProgress:      "0%",
			wantUnfinished:    true,
		},
		{
			name:              "fresh submission is not refunded",
			status:            constant.MjStatusSubmitting,
			billingStatus:     model.MidjourneyBillingStatusCharged,
			submitTime:        time.Now().UnixMilli(),
			wantStatus:        constant.MjStatusSubmitting,
			wantBillingStatus: model.MidjourneyBillingStatusCharged,
			wantQuota:         3000,
			wantProgress:      "0%",
			wantUnfinished:    true,
		},
		{
			name:              "stale reservation is cancelled",
			status:            constant.MjStatusReserving,
			billingStatus:     model.MidjourneyBillingStatusPrepared,
			submitTime:        time.Now().Add(-midjourneySubmissionGrace).Add(-time.Second).UnixMilli(),
			wantStatus:        "FAILURE",
			wantBillingStatus: "",
			wantQuota:         0,
			wantFailed:        1,
			wantProgress:      "100%",
		},
		{
			name:              "stale submission becomes unknown without refund",
			status:            constant.MjStatusSubmitting,
			billingStatus:     model.MidjourneyBillingStatusCharged,
			submitTime:        time.Now().Add(-midjourneySubmissionGrace).Add(-time.Second).UnixMilli(),
			wantStatus:        constant.MjStatusSubmitUnknown,
			wantBillingStatus: model.MidjourneyBillingStatusCharged,
			wantQuota:         3000,
			wantProgress:      "100%",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			db := setupModelListControllerTestDB(t)
			require.NoError(t, db.AutoMigrate(&model.Midjourney{}))
			task := &model.Midjourney{
				UserId:           1,
				Action:           "IMAGINE",
				SubmitTime:       test.submitTime,
				Status:           test.status,
				Progress:         "0%",
				Quota:            3000,
				BillingStatus:    test.billingStatus,
				BillingChannelId: 1,
			}
			require.NoError(t, task.Insert())

			summary := runMidjourneyTaskUpdateOnce(context.Background(), nil)

			assert.Equal(t, 1, summary.UnfinishedTasks)
			assert.Equal(t, test.wantFailed, summary.NullTasksFailed)
			var persisted model.Midjourney
			require.NoError(t, db.First(&persisted, task.Id).Error)
			assert.Equal(t, test.wantStatus, persisted.Status)
			assert.Equal(t, test.wantBillingStatus, persisted.BillingStatus)
			assert.Equal(t, test.wantQuota, persisted.Quota)
			assert.Equal(t, test.wantProgress, persisted.Progress)
			assert.Equal(t, test.wantUnfinished, model.HasUnfinishedMidjourneyTasks())
		})
	}
}
