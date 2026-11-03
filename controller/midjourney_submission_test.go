package controller

import (
	"context"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
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
		progress          string
		nullStatus        bool
	}{
		{
			name:          "legacy null status and upstream ID are repairable",
			billingStatus: model.MidjourneyBillingStatusPrepared,
			submitTime:    time.Now().Add(-time.Hour).UnixMilli(),
			wantStatus:    "FAILURE",
			wantQuota:     0,
			wantFailed:    1,
			wantProgress:  "100%",
			nullStatus:    true,
		},
		{
			name:              "terminal unknown old submission is adopted once",
			status:            constant.MjStatusSubmitUnknown,
			billingStatus:     model.MidjourneyBillingStatusCharged,
			submitTime:        time.Now().Add(-time.Hour).UnixMilli(),
			wantStatus:        constant.MjStatusSubmitUnknown,
			wantBillingStatus: model.MidjourneyBillingStatusCharged,
			wantQuota:         3000,
			wantProgress:      "100%",
			progress:          "100%",
		},
		{
			name:              "old charged reservation is adopted before failure refund",
			status:            constant.MjStatusReserving,
			billingStatus:     model.MidjourneyBillingStatusCharged,
			submitTime:        time.Now().Add(-midjourneySubmissionGrace).Add(-time.Second).UnixMilli(),
			wantStatus:        "FAILURE",
			wantBillingStatus: model.MidjourneyBillingStatusRefunded,
			wantFailed:        1,
			wantProgress:      "100%",
		},
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
			require.NoError(t, db.AutoMigrate(&model.Midjourney{}, &model.UserQuotaDebt{}, &model.Log{}))
			physical := 10000
			if test.billingStatus == model.MidjourneyBillingStatusCharged {
				physical -= 3000
			}
			require.NoError(t, db.Create(&model.User{Id: 1, Username: "midjourney-lifecycle", Quota: physical}).Error)
			progress := test.progress
			if progress == "" {
				progress = "0%"
			}
			task := &model.Midjourney{
				UserId:           1,
				Action:           "IMAGINE",
				SubmitTime:       test.submitTime,
				Status:           test.status,
				Progress:         progress,
				Quota:            3000,
				BillingStatus:    test.billingStatus,
				BillingChannelId: 1,
			}
			require.NoError(t, task.Insert())
			if test.nullStatus {
				require.NoError(t, db.Model(task).Updates(map[string]any{"status": nil, "mj_id": nil}).Error)
			}

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
			if !test.wantUnfinished {
				assert.Zero(t, runMidjourneyTaskUpdateOnce(context.Background(), nil).UnfinishedTasks, "adopted unknown submissions must not keep scheduling")
			}
			var user model.User
			require.NoError(t, db.First(&user, 1).Error)
			if test.wantBillingStatus == model.MidjourneyBillingStatusRefunded {
				assert.Equal(t, 10000, user.Quota)
				assert.Zero(t, user.QuotaReserved)
			} else {
				assert.Equal(t, physical, user.Quota, "adoption cannot change real quota")
				assert.Equal(t, persisted.BillingQuotaReserved, user.QuotaReserved)
			}
		})
	}
}

func TestMidjourneyPollerPreservesConcurrentLegacyAcceptance(t *testing.T) {
	previousEnabled := common.QuotaRemindEnabled
	common.QuotaRemindEnabled = false
	t.Cleanup(func() { common.QuotaRemindEnabled = previousEnabled })
	for _, status := range []string{constant.MjStatusReserving, constant.MjStatusSubmitting} {
		for _, duringAdoption := range []bool{false, true} {
			t.Run(status+"/"+map[bool]string{true: "before_adoption", false: "before_transition"}[duringAdoption], func(t *testing.T) {
				db := setupModelListControllerTestDB(t)
				require.NoError(t, db.AutoMigrate(&model.Midjourney{}, &model.UserQuotaDebt{}, &model.Log{}))
				require.NoError(t, db.Create(&model.User{Id: 1, Username: "old-acceptance", Quota: 90}).Error)
				task := &model.Midjourney{UserId: 1, Quota: 30, BillingStatus: model.MidjourneyBillingStatusCharged, Status: status, Progress: "0%", SubmitTime: time.Now().Add(-time.Hour).UnixMilli()}
				require.NoError(t, task.Insert())
				accepted := false
				const callback = "test:old_midjourney_acceptance"
				if duringAdoption {
					require.NoError(t, db.Callback().Query().After("gorm:query").Register(callback, func(tx *gorm.DB) {
						if _, selected := tx.Statement.Dest.(*[]*model.Midjourney); !selected || accepted {
							return
						}
						accepted = true
						require.NoError(t, db.Model(task).Updates(map[string]any{"mj_id": "accepted-old-task", "status": "SUCCESS", "progress": "100%"}).Error)
					}))
					t.Cleanup(func() { require.NoError(t, db.Callback().Query().Remove(callback)) })
				} else {
					require.NoError(t, db.Callback().Update().Before("gorm:update").Register(callback, func(tx *gorm.DB) {
						updates, selected := tx.Statement.Dest.(map[string]any)
						if !selected || updates["status"] == nil || accepted {
							return
						}
						accepted = true
						require.NoError(t, tx.Session(&gorm.Session{NewDB: true}).Model(&model.Midjourney{}).Where("id = ?", task.Id).
							Updates(map[string]any{"mj_id": "accepted-old-task", "status": "SUCCESS", "progress": "100%"}).Error)
					}))
					t.Cleanup(func() { require.NoError(t, db.Callback().Update().Remove(callback)) })
				}
				summary := runMidjourneyTaskUpdateOnce(context.Background(), nil)
				require.True(t, accepted)
				assert.Zero(t, summary.NullTasksFailed, "only a winning no-ID transition may refund")
				var current model.Midjourney
				require.NoError(t, db.First(&current, task.Id).Error)
				assert.Equal(t, "accepted-old-task", current.MjId)
				assert.Equal(t, "SUCCESS", current.Status)
				assert.Equal(t, model.MidjourneyBillingStatusCharged, current.BillingStatus)
				assert.Equal(t, 30, current.Quota)
				physical, err := model.GetUserQuota(1, true)
				require.NoError(t, err)
				assert.Equal(t, 90, physical, "a stale poller cannot refund an accepted task")
			})
		}
	}
}

func TestMidjourneyPollerResumesCommittedNoIDBilling(t *testing.T) {
	previousEnabled := common.QuotaRemindEnabled
	common.QuotaRemindEnabled = false
	t.Cleanup(func() { common.QuotaRemindEnabled = previousEnabled })
	for _, tc := range []struct {
		name, billing, status, wantBilling string
		quota, physical, marker, wantQuota int
		wantUnfinished                     bool
	}{
		{"persisted failure retries refund", model.MidjourneyBillingStatusCharged, "FAILURE", model.MidjourneyBillingStatusRefunded, 30, 90, 0, 120, false},
		{"refunding unknown", model.MidjourneyBillingStatusRefunding, constant.MjStatusSubmitUnknown, model.MidjourneyBillingStatusRefunded, 30, 90, 30, 120, false},
		{"refund pending submitting", model.MidjourneyBillingStatusRefundPending, constant.MjStatusSubmitting, model.MidjourneyBillingStatusRefunded, 30, 120, 30, 120, false},
		{"refunded unknown retains marker", model.MidjourneyBillingStatusRefunded, constant.MjStatusSubmitUnknown, model.MidjourneyBillingStatusRefunded, 0, 120, 30, 120, false},
		{"charge pending unknown finishes cache only", model.MidjourneyBillingStatusChargePending, constant.MjStatusSubmitUnknown, model.MidjourneyBillingStatusCharged, 30, 90, 0, 90, false},
		{"prepared unknown cannot start charge", model.MidjourneyBillingStatusPrepared, constant.MjStatusSubmitUnknown, model.MidjourneyBillingStatusPrepared, 30, 120, 0, 120, true},
		{"charging unknown cannot start charge", model.MidjourneyBillingStatusCharging, constant.MjStatusSubmitUnknown, model.MidjourneyBillingStatusCharging, 30, 120, 0, 120, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := setupModelListControllerTestDB(t)
			require.NoError(t, db.AutoMigrate(&model.Midjourney{}, &model.UserQuotaDebt{}, &model.Log{}))
			require.NoError(t, db.Create(&model.User{Id: 1, Username: "old-financial-transition", Quota: tc.physical}).Error)
			task := &model.Midjourney{UserId: 1, Quota: tc.quota, BillingStatus: tc.billing, Status: tc.status, Progress: "100%", SubmitTime: time.Now().Add(-time.Hour).UnixMilli()}
			if tc.billing == model.MidjourneyBillingStatusRefundPending {
				task.Progress, task.BillingQuotaDelta = "0%", 30
			}
			require.NoError(t, task.Insert())
			if tc.marker > 0 {
				require.NoError(t, db.Model(&model.User{}).Where("id = ?", 1).
					Clauses(clause.Assignments(map[string]any{"quota_reserved": tc.marker})).Updates(nil).Error)
				require.NoError(t, db.Model(task).Where("id = ?", task.Id).
					Clauses(clause.Assignments(map[string]any{"billing_quota_reserved": tc.marker})).Updates(nil).Error)
			}
			if tc.status == "FAILURE" {
				// Match MySQL's default no-op affected-row result deterministically;
				// the independent engine matrix also exercises its real driver.
				const callback = "test:mysql_unchanged_failed_submission"
				require.NoError(t, db.Callback().Update().After("gorm:update").Register(callback, func(tx *gorm.DB) {
					if updates, ok := tx.Statement.Dest.(map[string]any); ok && updates["status"] == "FAILURE" {
						tx.RowsAffected = 0
					}
				}))
				t.Cleanup(func() { require.NoError(t, db.Callback().Update().Remove(callback)) })
			}
			for range 2 {
				runMidjourneyTaskUpdateOnce(context.Background(), nil)
				var current model.Midjourney
				require.NoError(t, db.First(&current, task.Id).Error)
				assert.Equal(t, tc.status, current.Status, "financial recovery must not infer an upstream acceptance")
				assert.Equal(t, tc.wantBilling, current.BillingStatus)
				physical, err := model.GetUserQuota(1, true)
				require.NoError(t, err)
				assert.Equal(t, tc.wantQuota, physical)
				if tc.wantBilling == model.MidjourneyBillingStatusRefunded {
					assert.Zero(t, current.Quota)
					assert.Zero(t, current.BillingQuotaReserved)
					assert.Equal(t, "100%", current.Progress)
				}
				assert.Equal(t, tc.wantUnfinished, model.HasUnfinishedMidjourneyTasks())
			}
		})
	}
}
