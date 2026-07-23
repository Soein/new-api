package common

import "time"

var WalletTrustBypassEnabled = false
var WalletTrustBypassMinUsd = 100.0
var WalletTrustBypassMaxInflightUsd = 20.0

// UserQuotaRedisLockEnabled enables the cross-node portion of the user quota
// write gate. The per-process gate remains active when Redis is unavailable.
var UserQuotaRedisLockEnabled = true
var UserQuotaLockWaitTimeout = time.Minute
var UserQuotaLockLease = 2 * time.Minute

// UserQuotaMaxDebtUsd bounds the negative value kept on users.quota. Charges
// beyond this floor remain payable and are recorded in user_quota_debts.
var UserQuotaMaxDebtUsd = 0.0

func GetTrustQuota() int {
	return QuotaFromFloat(10 * QuotaPerUnit)
}

func GetWalletTrustBypassMinQuota() int {
	return QuotaFromFloat(WalletTrustBypassMinUsd * QuotaPerUnit)
}

func GetWalletTrustBypassMaxInflightQuota() int {
	return QuotaFromFloat(WalletTrustBypassMaxInflightUsd * QuotaPerUnit)
}

func GetUserQuotaMaxDebtQuota() int {
	if UserQuotaMaxDebtUsd <= 0 {
		return 0
	}
	return QuotaFromFloat(UserQuotaMaxDebtUsd * QuotaPerUnit)
}
