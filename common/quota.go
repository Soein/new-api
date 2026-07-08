package common

var WalletTrustBypassEnabled = false
var WalletTrustBypassMinUsd = 100.0
var WalletTrustBypassMaxInflightUsd = 20.0

func GetTrustQuota() int {
	return QuotaFromFloat(10 * QuotaPerUnit)
}

func GetWalletTrustBypassMinQuota() int {
	return QuotaFromFloat(WalletTrustBypassMinUsd * QuotaPerUnit)
}

func GetWalletTrustBypassMaxInflightQuota() int {
	return QuotaFromFloat(WalletTrustBypassMaxInflightUsd * QuotaPerUnit)
}
