package model

import (
	"errors"
	"fmt"

	"github.com/QuantumNous/new-api/common"
	"gorm.io/gorm"
)

const (
	// MidjourneyBillingStatusPrepared marks a persisted task whose charge has not committed.
	MidjourneyBillingStatusPrepared = "prepared"
	// MidjourneyBillingStatusCharging marks a retryable cache-first charge.
	MidjourneyBillingStatusCharging = "charging"
	// MidjourneyBillingStatusChargePending marks a committed charge awaiting cache finalization.
	MidjourneyBillingStatusChargePending = "charge_pending"
	// MidjourneyBillingStatusCharged marks a task whose quota and usage committed atomically.
	MidjourneyBillingStatusCharged = "charged"
	// MidjourneyBillingStatusRefunding marks a retryable cache-first refund.
	MidjourneyBillingStatusRefunding = "refunding"
	// MidjourneyBillingStatusRefundPending marks a committed refund awaiting cache finalization.
	MidjourneyBillingStatusRefundPending = "refund_pending"
	// MidjourneyBillingStatusRefunded marks a charged task whose refund committed atomically.
	MidjourneyBillingStatusRefunded = "refunded"
)

var errMidjourneyBillingNotPrepared = errors.New("midjourney billing is not prepared")

// ErrMidjourneyBillingRetryable means durable billing state was retained and
// the operation must be retried instead of clearing its quota marker.
var ErrMidjourneyBillingRetryable = errors.New("midjourney billing requires retry")

// MidjourneyBillingSettleResult reports whether this call applied the charge.
type MidjourneyBillingSettleResult struct {
	Applied bool
}

// MidjourneyBillingRefundResult reports whether refund processing completed
// and whether this call applied a financial transition.
type MidjourneyBillingRefundResult struct {
	Applied          bool
	Completed        bool
	Quota            int
	TokenId          int
	BillingChannelId int
}

// SettleBilling atomically charges wallet and token quota, records usage, and
// promotes a persisted task from prepared to charged.
func (midjourney *Midjourney) SettleBilling(tokenKey string) (result MidjourneyBillingSettleResult, err error) {
	if midjourney == nil || midjourney.Id == 0 {
		return result, errors.New("midjourney task must be persisted before billing")
	}
	if midjourney.UserId <= 0 {
		return result, ErrUserNotFound
	}

	err = withUserQuotaMutation(midjourney.UserId, func() error {
		stored, loadErr := getMidjourneyBillingTask(midjourney.Id)
		if loadErr != nil {
			return loadErr
		}
		if stored.UserId != midjourney.UserId {
			return fmt.Errorf("midjourney billing user changed from %d to %d", midjourney.UserId, stored.UserId)
		}
		mutation := newMidjourneyBillingCacheMutation(&stored, tokenKey, "charge")
		switch stored.BillingStatus {
		case MidjourneyBillingStatusCharged:
			_ = mutation.release()
			mutation.cleanupOperationKeys()
			return nil
		case MidjourneyBillingStatusChargePending:
			if prepareErr := mutation.prepare(true); prepareErr != nil {
				return prepareErr
			}
			if releaseErr := mutation.release(); releaseErr != nil {
				return releaseErr
			}
			if finalizeErr := updateMidjourneyBillingStatus(stored.Id, MidjourneyBillingStatusChargePending, MidjourneyBillingStatusCharged); finalizeErr != nil {
				return fmt.Errorf("%w: finalize Midjourney charge: %v", ErrMidjourneyBillingRetryable, finalizeErr)
			}
			mutation.cleanupOperationKeys()
			stored.BillingStatus = MidjourneyBillingStatusCharged
			result.Applied = true
			return nil
		case MidjourneyBillingStatusPrepared, MidjourneyBillingStatusCharging:
		default:
			return fmt.Errorf("%w: status %q", errMidjourneyBillingNotPrepared, stored.BillingStatus)
		}

		if prepareErr := mutation.prepare(false); prepareErr != nil {
			return prepareErr
		}
		if stored.BillingStatus == MidjourneyBillingStatusPrepared {
			if transitionErr := updateMidjourneyBillingStatus(stored.Id, MidjourneyBillingStatusPrepared, MidjourneyBillingStatusCharging); transitionErr != nil {
				_ = mutation.release()
				return fmt.Errorf("%w: prepare Midjourney charge: %v", ErrMidjourneyBillingRetryable, transitionErr)
			}
			stored.BillingStatus = MidjourneyBillingStatusCharging
		}

		if cacheErr := mutation.apply(-stored.Quota, -stored.Quota, true); cacheErr != nil {
			if errors.Is(cacheErr, ErrUserQuotaInsufficient) {
				rollbackErr := rollbackMidjourneyChargePreparation(&stored, mutation)
				return errors.Join(cacheErr, rollbackErr)
			}
			return cacheErr
		}
		if settleErr := settleMidjourneyBillingInDB(&stored); settleErr != nil {
			current, verifyErr := getMidjourneyBillingTask(stored.Id)
			if verifyErr != nil {
				return fmt.Errorf("%w: verify Midjourney charge outcome: %v", ErrMidjourneyBillingRetryable, errors.Join(settleErr, verifyErr))
			}
			if current.BillingStatus == MidjourneyBillingStatusChargePending || current.BillingStatus == MidjourneyBillingStatusCharged {
				stored = current
			} else {
				rollbackErr := rollbackMidjourneyChargePreparation(&stored, mutation)
				if rollbackErr != nil {
					return fmt.Errorf("%w: settle Midjourney charge: %v", ErrMidjourneyBillingRetryable, errors.Join(settleErr, rollbackErr))
				}
				return settleErr
			}
		}
		if stored.BillingStatus == MidjourneyBillingStatusCharged {
			_ = mutation.release()
			mutation.cleanupOperationKeys()
			result.Applied = true
			return nil
		}
		if releaseErr := mutation.release(); releaseErr != nil {
			return releaseErr
		}
		if finalizeErr := updateMidjourneyBillingStatus(stored.Id, MidjourneyBillingStatusChargePending, MidjourneyBillingStatusCharged); finalizeErr != nil {
			return fmt.Errorf("%w: finalize Midjourney charge: %v", ErrMidjourneyBillingRetryable, finalizeErr)
		}
		mutation.cleanupOperationKeys()
		stored.BillingStatus = MidjourneyBillingStatusCharged
		result.Applied = true
		return nil
	})
	if err != nil {
		return result, err
	}

	refreshed, refreshErr := getMidjourneyBillingTask(midjourney.Id)
	if refreshErr != nil {
		return result, refreshErr
	}
	copyMidjourneyBillingState(midjourney, &refreshed)
	return result, nil
}

func getMidjourneyBillingTask(taskId int) (Midjourney, error) {
	var task Midjourney
	err := DB.Select("id", "user_id", "channel_id", "quota", "token_id", "billing_channel_id", "billing_status", "billing_quota_delta").
		Where("id = ?", taskId).
		First(&task).Error
	return task, err
}

func updateMidjourneyBillingStatus(taskId int, from string, to string) error {
	updated := DB.Model(&Midjourney{}).
		Where("id = ? AND billing_status = ?", taskId, from).
		Update("billing_status", to)
	if updated.Error != nil {
		return updated.Error
	}
	if updated.RowsAffected != 1 {
		return fmt.Errorf("midjourney billing status changed concurrently from %q", from)
	}
	return nil
}

func settleMidjourneyBillingInDB(task *Midjourney) error {
	if task.Quota < 0 {
		return errors.New("midjourney quota cannot be negative")
	}
	return DB.Transaction(func(tx *gorm.DB) error {
		var current Midjourney
		if err := lockForUpdate(tx).
			Select("id", "billing_status").
			Where("id = ?", task.Id).
			First(&current).Error; err != nil {
			return err
		}
		if current.BillingStatus != MidjourneyBillingStatusCharging {
			return fmt.Errorf("midjourney charge cannot commit from status %q", current.BillingStatus)
		}
		if task.Quota > 0 {
			updated := tx.Model(&User{}).
				Where("id = ? AND quota >= ?", task.UserId, task.Quota).
				Update("quota", gorm.Expr("quota - ?", task.Quota))
			if updated.Error != nil {
				return updated.Error
			}
			if updated.RowsAffected == 0 {
				return ErrUserQuotaInsufficient
			}
		}
		if task.TokenId > 0 && task.Quota > 0 {
			if err := tx.Model(&Token{}).Where("id = ?", task.TokenId).Updates(map[string]interface{}{
				"remain_quota":  gorm.Expr("remain_quota - ?", task.Quota),
				"used_quota":    gorm.Expr("used_quota + ?", task.Quota),
				"accessed_time": common.GetTimestamp(),
			}).Error; err != nil {
				return err
			}
		}
		if err := tx.Model(&User{}).Where("id = ?", task.UserId).Updates(map[string]interface{}{
			"used_quota":    gorm.Expr("used_quota + ?", task.Quota),
			"request_count": gorm.Expr("request_count + 1"),
		}).Error; err != nil {
			return err
		}
		if task.BillingChannelId > 0 && task.Quota != 0 {
			if err := tx.Model(&Channel{}).
				Where("id = ?", task.BillingChannelId).
				Update("used_quota", gorm.Expr("used_quota + ?", task.Quota)).Error; err != nil {
				return err
			}
		}
		return tx.Model(&Midjourney{}).
			Where("id = ? AND billing_status = ?", task.Id, MidjourneyBillingStatusCharging).
			Update("billing_status", MidjourneyBillingStatusChargePending).Error
	})
}

func rollbackMidjourneyChargePreparation(task *Midjourney, mutation midjourneyBillingCacheMutation) error {
	compensateErr := mutation.compensate(-task.Quota, -task.Quota)
	releaseErr := mutation.release()
	statusErr := updateMidjourneyBillingStatus(task.Id, MidjourneyBillingStatusCharging, MidjourneyBillingStatusPrepared)
	if compensateErr == nil && releaseErr == nil && statusErr == nil {
		mutation.cleanupOperationKeys()
	}
	return errors.Join(compensateErr, releaseErr, statusErr)
}

func copyMidjourneyBillingState(target *Midjourney, source *Midjourney) {
	target.Quota = source.Quota
	target.TokenId = source.TokenId
	target.BillingChannelId = source.BillingChannelId
	target.BillingStatus = source.BillingStatus
	target.BillingQuotaDelta = source.BillingQuotaDelta
}

// CancelPreparedBilling clears a marker that never reached the charged state.
func (midjourney *Midjourney) CancelPreparedBilling() error {
	if midjourney == nil || midjourney.Id == 0 {
		return errors.New("midjourney task must be persisted before cancelling billing")
	}
	updated := DB.Model(&Midjourney{}).
		Where("id = ? AND billing_status = ?", midjourney.Id, MidjourneyBillingStatusPrepared).
		Updates(map[string]interface{}{
			"quota":               0,
			"token_id":            0,
			"billing_channel_id":  0,
			"billing_status":      "",
			"billing_quota_delta": 0,
		})
	if updated.Error != nil {
		return updated.Error
	}
	if updated.RowsAffected > 0 {
		midjourney.Quota = 0
		midjourney.TokenId = 0
		midjourney.BillingChannelId = 0
		midjourney.BillingStatus = ""
		midjourney.BillingQuotaDelta = 0
	}
	return nil
}

// RefundBilling atomically refunds charged quota, reverses usage accounting,
// and marks the task refunded. Legacy tasks with a non-zero quota and no
// billing status are treated as charged.
func (midjourney *Midjourney) RefundBilling(tokenKey string) (result MidjourneyBillingRefundResult, err error) {
	if midjourney == nil || midjourney.Id == 0 {
		return result, errors.New("midjourney task must be persisted before refunding billing")
	}
	if midjourney.UserId <= 0 {
		return result, ErrUserNotFound
	}

	err = withUserQuotaMutation(midjourney.UserId, func() error {
		stored, loadErr := getMidjourneyBillingTask(midjourney.Id)
		if loadErr != nil {
			return loadErr
		}
		if stored.UserId != midjourney.UserId {
			return fmt.Errorf("midjourney billing user changed from %d to %d", midjourney.UserId, stored.UserId)
		}
		if stored.Quota == 0 || stored.BillingStatus == MidjourneyBillingStatusRefunded {
			result.Completed = true
			return nil
		}
		if stored.BillingStatus == MidjourneyBillingStatusPrepared || stored.BillingStatus == MidjourneyBillingStatusCharging {
			completed, cancelErr := cancelUnchargedMidjourneyBilling(&stored, tokenKey)
			result.Completed = completed
			return cancelErr
		}

		mutation := newMidjourneyBillingCacheMutation(&stored, tokenKey, "refund")
		switch stored.BillingStatus {
		case MidjourneyBillingStatusRefundPending:
			if prepareErr := mutation.prepare(true); prepareErr != nil {
				return prepareErr
			}
		case MidjourneyBillingStatusCharged, "":
			if prepareErr := mutation.prepare(false); prepareErr != nil {
				return prepareErr
			}
			if transitionErr := startMidjourneyRefund(&stored); transitionErr != nil {
				_ = mutation.release()
				return fmt.Errorf("%w: prepare Midjourney refund: %v", ErrMidjourneyBillingRetryable, transitionErr)
			}
			stored.BillingStatus = MidjourneyBillingStatusRefunding
		case MidjourneyBillingStatusRefunding:
			if prepareErr := mutation.prepare(false); prepareErr != nil {
				return prepareErr
			}
		default:
			return fmt.Errorf("midjourney billing cannot be refunded from status %q", stored.BillingStatus)
		}
		if stored.BillingStatus == MidjourneyBillingStatusRefunding {
			quotaDelta, refundErr := refundMidjourneyBillingInDB(&stored)
			if refundErr != nil {
				current, verifyErr := getMidjourneyBillingTask(stored.Id)
				if verifyErr != nil {
					return fmt.Errorf("%w: verify Midjourney refund outcome: %v", ErrMidjourneyBillingRetryable, errors.Join(refundErr, verifyErr))
				}
				if current.BillingStatus != MidjourneyBillingStatusRefundPending && current.BillingStatus != MidjourneyBillingStatusRefunded {
					return fmt.Errorf("%w: refund Midjourney billing: %v", ErrMidjourneyBillingRetryable, refundErr)
				}
				stored = current
			} else {
				stored.BillingQuotaDelta = quotaDelta
				stored.BillingStatus = MidjourneyBillingStatusRefundPending
			}
		}
		if stored.BillingStatus == MidjourneyBillingStatusRefunded {
			_ = mutation.release()
			mutation.cleanupOperationKeys()
			result.Completed = true
			return nil
		}
		if cacheErr := mutation.apply(stored.BillingQuotaDelta, stored.Quota, false); cacheErr != nil {
			return cacheErr
		}

		if releaseErr := mutation.release(); releaseErr != nil {
			return releaseErr
		}
		if finalizeErr := finalizeMidjourneyRefund(&stored); finalizeErr != nil {
			return fmt.Errorf("%w: finalize Midjourney refund: %v", ErrMidjourneyBillingRetryable, finalizeErr)
		}
		mutation.cleanupOperationKeys()
		result.Applied = true
		result.Completed = true
		result.Quota = stored.Quota
		result.TokenId = stored.TokenId
		result.BillingChannelId = stored.GetBillingChannelId()
		return nil
	})
	if err != nil {
		return result, err
	}

	refreshed, refreshErr := getMidjourneyBillingTask(midjourney.Id)
	if refreshErr != nil {
		return result, refreshErr
	}
	copyMidjourneyBillingState(midjourney, &refreshed)
	return result, nil
}

func cancelUnchargedMidjourneyBilling(task *Midjourney, tokenKey string) (bool, error) {
	mutation := newMidjourneyBillingCacheMutation(task, tokenKey, "charge")
	if prepareErr := mutation.prepare(false); prepareErr != nil {
		return false, prepareErr
	}
	if compensateErr := mutation.compensate(-task.Quota, -task.Quota); compensateErr != nil {
		return false, fmt.Errorf("%w: cancel Midjourney charge cache: %v", ErrMidjourneyBillingRetryable, compensateErr)
	}
	if releaseErr := mutation.release(); releaseErr != nil {
		return false, releaseErr
	}
	updated := DB.Model(&Midjourney{}).
		Where("id = ? AND billing_status = ?", task.Id, task.BillingStatus).
		Updates(map[string]interface{}{
			"quota":               0,
			"token_id":            0,
			"billing_channel_id":  0,
			"billing_status":      "",
			"billing_quota_delta": 0,
		})
	if updated.Error != nil {
		return false, updated.Error
	}
	if updated.RowsAffected != 1 {
		return false, fmt.Errorf("%w: cancel Midjourney charge state changed concurrently", ErrMidjourneyBillingRetryable)
	}
	mutation.cleanupOperationKeys()
	return true, nil
}

func startMidjourneyRefund(task *Midjourney) error {
	query := DB.Model(&Midjourney{}).Where("id = ? AND quota = ?", task.Id, task.Quota)
	if task.BillingStatus == MidjourneyBillingStatusCharged {
		query = query.Where("billing_status = ?", MidjourneyBillingStatusCharged)
	} else {
		query = query.Where("billing_status = ? OR billing_status IS NULL", "")
	}
	updated := query.Update("billing_status", MidjourneyBillingStatusRefunding)
	if updated.Error != nil {
		return updated.Error
	}
	if updated.RowsAffected != 1 {
		return errors.New("midjourney refund state changed concurrently")
	}
	return nil
}

func refundMidjourneyBillingInDB(task *Midjourney) (quotaDelta int, err error) {
	err = DB.Transaction(func(tx *gorm.DB) error {
		var current Midjourney
		if err := lockForUpdate(tx).
			Select("id", "billing_status").
			Where("id = ?", task.Id).
			First(&current).Error; err != nil {
			return err
		}
		if current.BillingStatus != MidjourneyBillingStatusRefunding {
			return fmt.Errorf("midjourney refund cannot commit from status %q", current.BillingStatus)
		}
		var creditErr error
		quotaDelta, creditErr = CreditUserQuotaWithTx(tx, task.UserId, task.Quota)
		if creditErr != nil {
			return creditErr
		}
		if task.TokenId > 0 && task.Quota > 0 {
			if err := tx.Model(&Token{}).Where("id = ?", task.TokenId).Updates(map[string]interface{}{
				"remain_quota":  gorm.Expr("remain_quota + ?", task.Quota),
				"used_quota":    gorm.Expr("used_quota - ?", task.Quota),
				"accessed_time": common.GetTimestamp(),
			}).Error; err != nil {
				return err
			}
		}
		if err := tx.Model(&User{}).
			Where("id = ?", task.UserId).
			Update("used_quota", gorm.Expr("used_quota - ?", task.Quota)).Error; err != nil {
			return err
		}
		billingChannelId := task.GetBillingChannelId()
		if billingChannelId > 0 && task.Quota != 0 {
			if err := tx.Model(&Channel{}).
				Where("id = ?", billingChannelId).
				Update("used_quota", gorm.Expr("used_quota - ?", task.Quota)).Error; err != nil {
				return err
			}
		}
		updated := tx.Model(&Midjourney{}).
			Where("id = ? AND billing_status = ?", task.Id, MidjourneyBillingStatusRefunding).
			Updates(map[string]interface{}{
				"billing_status":      MidjourneyBillingStatusRefundPending,
				"billing_quota_delta": quotaDelta,
			})
		if updated.Error != nil {
			return updated.Error
		}
		if updated.RowsAffected != 1 {
			return errors.New("midjourney refund state changed concurrently")
		}
		return nil
	})
	return quotaDelta, err
}

func finalizeMidjourneyRefund(task *Midjourney) error {
	updated := DB.Model(&Midjourney{}).
		Where("id = ? AND billing_status = ? AND quota = ?", task.Id, MidjourneyBillingStatusRefundPending, task.Quota).
		Updates(map[string]interface{}{
			"quota":               0,
			"billing_status":      MidjourneyBillingStatusRefunded,
			"billing_quota_delta": 0,
		})
	if updated.Error != nil {
		return updated.Error
	}
	if updated.RowsAffected != 1 {
		return errors.New("midjourney refund finalization changed concurrently")
	}
	return nil
}
