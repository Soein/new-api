package model

import (
	"errors"
	"sync/atomic"
	"time"

	"github.com/QuantumNous/new-api/common"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// WalletQuotaReservation is a leased reminder marker owned by one live request.
// It is independent of real quota/debt and durable Midjourney reservations.
type WalletQuotaReservation struct {
	RequestID string `json:"-" gorm:"type:varchar(64);primaryKey"`
	UserID    int    `json:"-" gorm:"not null;index:,composite:user_expiry,priority:1"`
	Amount    int    `json:"-" gorm:"type:bigint;not null;index:,composite:user_expiry,priority:3"`
	ExpiresAt int64  `json:"-" gorm:"type:bigint;not null;index;index:,composite:user_expiry,priority:2"`
}

var lastWalletReservationCleanup atomic.Int64

// walletSettledBalanceTx excludes pending reservations and outstanding debt.
// All values are read in one statement, including on read-only callers.
func walletSettledBalanceTx(tx *gorm.DB, userID int) (int, error) {
	var wallet struct {
		Quota           int
		Reserved        int
		Debt            int64
		Pending         int64
		RefundedPending int64
	}
	debt := &gorm.Statement{DB: tx}
	if err := debt.Parse(&UserQuotaDebt{}); err != nil {
		return 0, err
	}
	user := &gorm.Statement{DB: tx}
	if err := user.Parse(&User{}); err != nil {
		return 0, err
	}
	pending := tx.Model(&WalletQuotaReservation{}).Select("COALESCE(SUM(amount), 0)").
		Where(clause.Eq{Column: clause.Column{Name: "user_id"}, Value: clause.Column{Table: user.Table, Name: "id"}}).
		Where("expires_at > ?", time.Now().UnixMilli())
	// An old poller can credit a new task's money without clearing its marker.
	// refund_pending already means the SQL refund committed, including when
	// cache finalization still needs recovery. Never add that money back twice.
	refundedPending := tx.Model(&Midjourney{}).Select("COALESCE(SUM(billing_quota_reserved), 0)").
		Where(clause.Eq{Column: clause.Column{Name: "user_id"}, Value: clause.Column{Table: user.Table, Name: "id"}}).
		Where(clause.Gt{Column: clause.Column{Table: user.Table, Name: "quota_reserved"}, Value: 0}).
		Where("billing_status IN ? AND billing_quota_reserved > 0", []string{MidjourneyBillingStatusRefundPending, MidjourneyBillingStatusRefunded})
	query := tx.Model(&User{}).Clauses(
		clause.Select{Expression: clause.Expr{SQL: "?, ? AS ?, COALESCE(?, 0) AS ?, (?) AS ?, (?) AS ?", Vars: []any{
			clause.Column{Table: clause.CurrentTable, Name: "quota"},
			clause.Column{Table: clause.CurrentTable, Name: "quota_reserved"}, clause.Column{Name: "reserved"},
			clause.Column{Table: debt.Table, Name: "amount"}, clause.Column{Name: "debt"},
			pending, clause.Column{Name: "pending"},
			refundedPending, clause.Column{Name: "refunded_pending"},
		}}},
		clause.From{Tables: []clause.Table{{Name: clause.CurrentTable}}, Joins: []clause.Join{{Type: clause.LeftJoin, Table: clause.Table{Name: debt.Table}, ON: clause.Where{Exprs: []clause.Expression{
			clause.Eq{Column: clause.Column{Table: debt.Table, Name: "user_id"}, Value: clause.Column{Table: clause.CurrentTable, Name: "id"}},
		}}}}},
	)
	err := query.Where(clause.Eq{Column: clause.Column{Table: clause.CurrentTable, Name: "id"}, Value: userID}).Take(&wallet).Error
	if err != nil {
		return 0, err
	}
	available := int64(wallet.Quota) + int64(wallet.Reserved) + wallet.Pending - wallet.RefundedPending
	floor := -int64(common.GetUserQuotaMaxDebtQuota())
	if wallet.Debt >= available-floor {
		return int(floor), nil
	}
	return int(available - wallet.Debt), nil
}

// GetWalletSettledQuota returns the balance after confirmed charges, excluding
// other requests' pending reservations. It never supplies quota admission checks.
func GetWalletSettledQuota(userID int) (int, error) {
	return walletSettledBalanceTx(DB, userID)
}

// SettleWalletQuotaReservation removes this request's reminder marker and applies
// actualQuota-reserved to real money in one transaction. A missing or expired
// marker does not skip settlement. Callers must retain the session's real
// reserved amount and guard the financial operation against repeated calls.
func SettleWalletQuotaReservation(userID, reserved, actualQuota int, requestID string, captureBalance bool) (common.WalletQuotaChange, error) {
	var change common.WalletQuotaChange
	if reserved < 0 || actualQuota < 0 {
		return change, errors.New("wallet settlement quota cannot be negative")
	}
	if reserved == 0 && actualQuota == 0 {
		return change, nil
	}
	err := withUserQuotaMutation(userID, func() error {
		var quotaDelta int
		err := DB.Transaction(func(tx *gorm.DB) error {
			var err error
			change, quotaDelta, err = settleWalletQuotaReservationTx(tx, userID, reserved, actualQuota, captureBalance, &requestID)
			return err
		})
		if err == nil {
			updateUserQuotaCacheDelta(userID, quotaDelta)
		}
		return err
	})
	return change, err
}

// settleWalletQuotaReservationTx shares finalization with durable task billing.
func settleWalletQuotaReservationTx(tx *gorm.DB, userID, reserved, actualQuota int, captureBalance bool, requestID *string) (common.WalletQuotaChange, int, error) {
	var change common.WalletQuotaChange
	var user User
	if err := lockForUpdate(tx).Select("id", "quota", "quota_reserved").First(&user, userID).Error; err != nil {
		return change, 0, err
	}
	if requestID == nil && user.QuotaReserved < reserved {
		return change, 0, errors.New("wallet reservation has already been released")
	}
	if captureBalance {
		before, err := walletSettledBalanceTx(tx, userID)
		if err != nil {
			return change, 0, err
		}
		change.Before = before
	}
	delta := actualQuota - reserved
	var quotaDelta int
	if delta > 0 {
		var err error
		quotaDelta, err = debitUserQuotaForSettlementTx(tx, &user, delta)
		if err != nil {
			return change, 0, err
		}
	} else if delta < 0 {
		var err error
		quotaDelta, err = CreditUserQuotaWithTx(tx, userID, -delta)
		if err != nil {
			return change, 0, err
		}
	}
	if requestID != nil {
		// Missing or expired reminder markers do not suppress a real settlement
		// or refund, and cannot release another request's pending amount.
		if *requestID != "" {
			if err := tx.Where("request_id = ? AND user_id = ?", *requestID, userID).Delete(&WalletQuotaReservation{}).Error; err != nil {
				return change, 0, err
			}
		}
	} else if reserved > 0 {
		if err := tx.Model(&User{}).Where("id = ?", userID).
			Clauses(clause.Assignments(map[string]any{"quota_reserved": gorm.Expr("quota_reserved - ?", reserved)})).Updates(nil).Error; err != nil {
			return change, 0, err
		}
	}
	if captureBalance {
		after, err := walletSettledBalanceTx(tx, userID)
		if err != nil {
			return change, 0, err
		}
		change.After = after
	}
	return change, quotaDelta, nil
}

// RefundWalletQuotaReservation cancels a reservation without a confirmed charge.
func RefundWalletQuotaReservation(userID, reserved int, requestID string) error {
	_, err := SettleWalletQuotaReservation(userID, reserved, 0, requestID, false)
	return err
}

// RollbackWalletQuotaReservation reverses only a failed incremental reservation;
// earlier successful reservations belonging to the same request remain live.
func RollbackWalletQuotaReservation(userID, amount int, requestID string) error {
	return withUserQuotaMutation(userID, func() error {
		var quotaDelta int
		err := DB.Transaction(func(tx *gorm.DB) error {
			var err error
			quotaDelta, err = CreditUserQuotaWithTx(tx, userID, amount)
			if err != nil {
				return err
			}
			var marker WalletQuotaReservation
			err = tx.Where("request_id = ? AND user_id = ?", requestID, userID).Take(&marker).Error
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return nil
			}
			if err != nil {
				return err
			}
			if marker.Amount <= amount {
				return tx.Delete(&marker).Error
			}
			return tx.Model(&marker).Update("amount", marker.Amount-amount).Error
		})
		if err == nil {
			updateUserQuotaCacheDelta(userID, quotaDelta)
		}
		return err
	})
}

// RenewWalletQuotaReservations extends only the caller's still-valid markers.
// Request IDs are unguessable owner credentials; an expired row is not revived.
func RenewWalletQuotaReservations(requestIDs []string, now, expiresAt int64) error {
	if len(requestIDs) == 0 || expiresAt <= now {
		return nil
	}
	return DB.Model(&WalletQuotaReservation{}).Where("request_id IN ? AND expires_at > ?", requestIDs, now).
		Update("expires_at", expiresAt).Error
}

// CleanupExpiredWalletQuotaReservations removes at most limit reminder rows.
// Its expiry guard preserves concurrently renewed leases and never touches money.
func CleanupExpiredWalletQuotaReservations(now int64, limit int) (int64, error) {
	if limit <= 0 {
		return 0, nil
	}
	var requestIDs []string
	if err := DB.Model(&WalletQuotaReservation{}).Where("expires_at <= ?", now).Order("expires_at").Limit(limit).Pluck("request_id", &requestIDs).Error; err != nil {
		return 0, err
	}
	if len(requestIDs) == 0 {
		return 0, nil
	}
	var removed int64
	for start := 0; start < len(requestIDs); start += 128 {
		result := DB.Where("request_id IN ? AND expires_at <= ?", requestIDs[start:min(start+128, len(requestIDs))], now).Delete(&WalletQuotaReservation{})
		removed += result.RowsAffected
		if result.Error != nil {
			return removed, result.Error
		}
	}
	return removed, nil
}

func maybeCleanupWalletQuotaReservations(now int64) {
	previous := lastWalletReservationCleanup.Load()
	if now-previous < 30_000 || !lastWalletReservationCleanup.CompareAndSwap(previous, now) {
		return
	}
	if _, err := CleanupExpiredWalletQuotaReservations(now, 128); err != nil {
		common.SysLog(common.LogText("failed to clean expired wallet reminder reservations: %s", err.Error()))
	}
}
