package model

import (
	"errors"

	"github.com/QuantumNous/new-api/common"
	"github.com/shopspring/decimal"
	"gorm.io/gorm"
)

var (
	ErrInvalidUserQuotaAdjustment = errors.New("invalid user quota adjustment")
	ErrUserQuotaPermission        = errors.New("cannot adjust quota for this user role")
)

// UserQuotaAdjustment is the immutable database snapshot of a committed manual
// adjustment. Pending relay deductions in the quota cache are not part of it.
type UserQuotaAdjustment struct {
	UserID   int
	Username string
	Before   int
	After    int
}

// AdjustUserQuota atomically checks operator permissions and adjusts a wallet.
// Credits repay debt first, subtraction cannot overdraw, and explicit overrides
// clear outstanding debt. Cache synchronization failures do not undo a commit.
func AdjustUserQuota(userID, operatorRole int, mode string, value int) (*UserQuotaAdjustment, error) {
	if userID <= 0 || (mode != "add" && mode != "subtract" && mode != "override") {
		return nil, ErrInvalidUserQuotaAdjustment
	}
	if mode != "override" && value <= 0 {
		return nil, ErrInvalidUserQuotaAdjustment
	}
	if value > common.MaxWalletQuota || value < -common.MaxWalletQuota {
		return nil, ErrWalletQuotaLimitExceeded
	}

	var adjustment UserQuotaAdjustment
	err := withUserQuotaMutation(userID, func() error {
		err := DB.Transaction(func(tx *gorm.DB) error {
			var user User
			if err := lockForUpdate(tx).First(&user, userID).Error; err != nil {
				return err
			}
			if operatorRole != common.RoleRootUser && operatorRole <= user.Role {
				return ErrUserQuotaPermission
			}
			if user.Quota > common.MaxWalletQuota || user.Quota < -common.MaxWalletQuota {
				return ErrWalletQuotaLimitExceeded
			}
			quota := decimal.NewFromInt(int64(value))
			switch mode {
			case "add":
				delta, err := creditUserQuotaWithLimitTx(tx, userID, value, common.MaxWalletQuota)
				if errors.Is(err, errUserQuotaCreditLimitExceeded) {
					return ErrWalletQuotaLimitExceeded
				}
				if err != nil {
					return err
				}
				adjustment = UserQuotaAdjustment{UserID: user.Id, Username: user.Username, Before: user.Quota, After: user.Quota + delta}
				return nil
			case "subtract":
				quota = decimal.NewFromInt(int64(user.Quota)).Sub(quota)
			case "override":
				// Explicit overrides replace the wallet and forgive outstanding debt.
				if err := tx.Where("user_id = ?", userID).Delete(&UserQuotaDebt{}).Error; err != nil {
					return err
				}
			}
			after, err := common.WalletQuotaFromDecimalStrict(quota)
			if err != nil {
				return ErrWalletQuotaLimitExceeded
			}
			if mode == "subtract" && after < 0 {
				return ErrUserQuotaInsufficient
			}
			// An unchanged override is a successful operation, including on MySQL
			// configurations that count only changed rows in RowsAffected.
			if after != user.Quota {
				result := tx.Model(&User{}).Where("id = ?", userID).Update("quota", after)
				if result.Error != nil {
					return result.Error
				}
				if result.RowsAffected != 1 {
					return gorm.ErrRecordNotFound
				}
			}
			adjustment = UserQuotaAdjustment{UserID: user.Id, Username: user.Username, Before: user.Quota, After: after}
			return nil
		})
		if err != nil {
			return err
		}
		// Apply only the committed difference, preserving outstanding reservations.
		updateUserQuotaCacheDelta(userID, adjustment.After-adjustment.Before)
		return nil
	})
	if err != nil {
		return nil, err
	}

	return &adjustment, nil
}
