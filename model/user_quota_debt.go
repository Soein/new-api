package model

import (
	"errors"
	"time"

	"github.com/QuantumNous/new-api/common"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// UserQuotaDebt stores already-incurred usage that exceeds the configured
// users.quota floor. Keeping it separate prevents an unbounded negative
// balance without forgiving the upstream cost.
type UserQuotaDebt struct {
	UserId    int   `json:"user_id" gorm:"primaryKey;autoIncrement:false"`
	Amount    int64 `json:"amount" gorm:"type:bigint;not null;default:0"`
	UpdatedAt int64 `json:"updated_at" gorm:"autoUpdateTime"`
}

func GetUserQuotaDebt(userID int) (int64, error) {
	if userID <= 0 {
		return 0, ErrUserNotFound
	}
	var debt UserQuotaDebt
	err := DB.Where("user_id = ?", userID).Limit(1).Find(&debt).Error
	return debt.Amount, err
}

// creditUserQuota applies a refund, top-up, or administrative credit. Existing
// outstanding debt is repaid first; only the remainder becomes spendable
// quota. The returned delta is the amount actually added to users.quota and is
// used to keep the Redis user cache consistent.
func creditUserQuota(userID int, amount int) (int, error) {
	if amount == 0 {
		return 0, nil
	}
	var quotaDelta int
	err := DB.Transaction(func(tx *gorm.DB) error {
		var err error
		quotaDelta, err = CreditUserQuotaWithTx(tx, userID, amount)
		return err
	})
	return quotaDelta, err
}

// CreditUserQuotaWithTx is the transaction-aware form used by payment,
// redemption, and check-in flows that already own a database transaction.
// Callers should update the Redis user cache with the returned quota delta only
// after their surrounding transaction commits.
func CreditUserQuotaWithTx(tx *gorm.DB, userID int, amount int) (int, error) {
	if tx == nil {
		return 0, errors.New("quota credit transaction is nil")
	}
	if userID <= 0 {
		return 0, ErrUserNotFound
	}
	if amount < 0 {
		return 0, errors.New("quota credit cannot be negative")
	}
	if amount == 0 {
		return 0, nil
	}

	var user User
	if err := lockForUpdate(tx).Select("id", "quota").Where("id = ?", userID).First(&user).Error; err != nil {
		return 0, err
	}

	var debt UserQuotaDebt
	if err := lockForUpdate(tx).Where("user_id = ?", userID).Limit(1).Find(&debt).Error; err != nil {
		return 0, err
	}

	credit := int64(amount)
	if debt.Amount > 0 {
		paid := minInt64(credit, debt.Amount)
		credit -= paid
		remainingDebt := debt.Amount - paid
		if remainingDebt == 0 {
			if err := tx.Where("user_id = ?", userID).Delete(&UserQuotaDebt{}).Error; err != nil {
				return 0, err
			}
		} else {
			if err := tx.Model(&UserQuotaDebt{}).
				Where("user_id = ?", userID).
				Updates(map[string]interface{}{"amount": remainingDebt, "updated_at": time.Now().Unix()}).Error; err != nil {
				return 0, err
			}
		}
	}

	quotaDelta := int(credit)
	if quotaDelta == 0 {
		return 0, nil
	}
	if err := tx.Model(&User{}).
		Where("id = ?", userID).
		Update("quota", gorm.Expr("quota + ?", quotaDelta)).Error; err != nil {
		return 0, err
	}
	return quotaDelta, nil
}

// SetUserQuota replaces the spendable balance and clears outstanding debt.
// This is intentionally reserved for the explicit administrative override
// action; ordinary credits must repay debt first.
func SetUserQuota(userID int, quota int) error {
	return withUserQuotaMutation(userID, func() error {
		if err := DB.Transaction(func(tx *gorm.DB) error {
			if err := lockForUpdate(tx).Select("id").Where("id = ?", userID).First(&User{}).Error; err != nil {
				return err
			}
			if err := tx.Where("user_id = ?", userID).Delete(&UserQuotaDebt{}).Error; err != nil {
				return err
			}
			return tx.Model(&User{}).Where("id = ?", userID).Update("quota", quota).Error
		}); err != nil {
			return err
		}
		return invalidateUserCache(userID)
	})
}

// debitUserQuotaForSettlement records usage that has already occurred. The
// visible quota is clamped to -USER_QUOTA_MAX_DEBT_USD; any remainder is
// durably accumulated as outstanding debt in the same database transaction.
// The returned delta is the exact users.quota change for cache maintenance.
func debitUserQuotaForSettlement(userID int, amount int) (int, error) {
	if amount == 0 {
		return 0, nil
	}
	var quotaDelta int
	err := DB.Transaction(func(tx *gorm.DB) error {
		var user User
		if err := lockForUpdate(tx).Select("id", "quota").Where("id = ?", userID).First(&user).Error; err != nil {
			return err
		}

		current := int64(user.Quota)
		target := current - int64(amount)
		floor := -int64(common.GetUserQuotaMaxDebtQuota())
		bounded := target
		if bounded < floor {
			bounded = floor
		}
		quotaDelta = int(bounded - current)

		if quotaDelta != 0 {
			if err := tx.Model(&User{}).Where("id = ?", userID).Update("quota", bounded).Error; err != nil {
				return err
			}
		}

		overflow := bounded - target
		if overflow <= 0 {
			return nil
		}
		now := time.Now().Unix()
		return tx.Clauses(clause.OnConflict{
			Columns: []clause.Column{{Name: "user_id"}},
			DoUpdates: clause.Assignments(map[string]interface{}{
				"amount":     gorm.Expr("user_quota_debts.amount + ?", overflow),
				"updated_at": now,
			}),
		}).Create(&UserQuotaDebt{UserId: userID, Amount: overflow, UpdatedAt: now}).Error
	})
	return quotaDelta, err
}

func minInt64(a, b int64) int64 {
	if a < b {
		return a
	}
	return b
}

func invalidateUserQuotaCacheAfterCommit(userID int) {
	if userID <= 0 {
		return
	}
	if err := invalidateUserCache(userID); err != nil {
		common.SysLog("failed to invalidate user cache after committed quota credit: " + err.Error())
	}
}
