package controller

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/Calcium-Ion/go-epay/epay"
	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/setting"
	"github.com/QuantumNous/new-api/setting/operation_setting"
	"github.com/QuantumNous/new-api/setting/system_setting"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestStripeWebhookEnabledRequiresTopUpAndWebhookConfig(t *testing.T) {
	originalAPISecret := setting.StripeApiSecret
	originalWebhookSecret := setting.StripeWebhookSecret
	originalPriceID := setting.StripePriceId
	t.Cleanup(func() {
		setting.StripeApiSecret = originalAPISecret
		setting.StripeWebhookSecret = originalWebhookSecret
		setting.StripePriceId = originalPriceID
	})

	setting.StripeWebhookSecret = ""
	setting.StripeApiSecret = "sk_test_123"
	setting.StripePriceId = "price_123"
	require.False(t, isStripeWebhookEnabled())

	setting.StripeWebhookSecret = "whsec_test"
	require.True(t, isStripeWebhookEnabled())

	setting.StripePriceId = ""
	require.False(t, isStripeWebhookEnabled())
}

func TestStripeTopUpEnabledRequiresConfiguredStripe(t *testing.T) {
	originalAPISecret := setting.StripeApiSecret
	originalWebhookSecret := setting.StripeWebhookSecret
	originalPriceID := setting.StripePriceId
	t.Cleanup(func() {
		setting.StripeApiSecret = originalAPISecret
		setting.StripeWebhookSecret = originalWebhookSecret
		setting.StripePriceId = originalPriceID
	})

	setting.StripeApiSecret = "sk_test_123"
	setting.StripeWebhookSecret = "whsec_test"
	setting.StripePriceId = "price_123"

	require.True(t, isStripeTopUpEnabled())
}

func TestCreemWebhookEnabledRequiresTopUpAndWebhookConfig(t *testing.T) {
	originalAPIKey := setting.CreemApiKey
	originalProducts := setting.CreemProducts
	originalWebhookSecret := setting.CreemWebhookSecret
	t.Cleanup(func() {
		setting.CreemApiKey = originalAPIKey
		setting.CreemProducts = originalProducts
		setting.CreemWebhookSecret = originalWebhookSecret
	})

	setting.CreemWebhookSecret = ""
	setting.CreemApiKey = "creem_api_key"
	setting.CreemProducts = `[{"productId":"prod_123"}]`
	require.False(t, isCreemWebhookEnabled())

	setting.CreemWebhookSecret = "creem_secret"
	require.True(t, isCreemWebhookEnabled())

	setting.CreemProducts = "[]"
	require.False(t, isCreemWebhookEnabled())
}

func TestWaffoWebhookEnabledRequiresTopUpAndWebhookConfig(t *testing.T) {
	originalEnabled := setting.WaffoEnabled
	originalSandbox := setting.WaffoSandbox
	originalAPIKey := setting.WaffoApiKey
	originalPrivateKey := setting.WaffoPrivateKey
	originalPublicCert := setting.WaffoPublicCert
	originalSandboxAPIKey := setting.WaffoSandboxApiKey
	originalSandboxPrivateKey := setting.WaffoSandboxPrivateKey
	originalSandboxPublicCert := setting.WaffoSandboxPublicCert
	t.Cleanup(func() {
		setting.WaffoEnabled = originalEnabled
		setting.WaffoSandbox = originalSandbox
		setting.WaffoApiKey = originalAPIKey
		setting.WaffoPrivateKey = originalPrivateKey
		setting.WaffoPublicCert = originalPublicCert
		setting.WaffoSandboxApiKey = originalSandboxAPIKey
		setting.WaffoSandboxPrivateKey = originalSandboxPrivateKey
		setting.WaffoSandboxPublicCert = originalSandboxPublicCert
	})

	setting.WaffoEnabled = true
	setting.WaffoSandbox = false
	setting.WaffoApiKey = ""
	setting.WaffoPrivateKey = "private"
	setting.WaffoPublicCert = "public"
	require.False(t, isWaffoWebhookEnabled())

	setting.WaffoApiKey = "api"
	require.True(t, isWaffoWebhookEnabled())

	setting.WaffoEnabled = false
	require.False(t, isWaffoWebhookEnabled())

	setting.WaffoEnabled = true
	setting.WaffoSandbox = true
	setting.WaffoSandboxApiKey = ""
	setting.WaffoSandboxPrivateKey = "sandbox_private"
	setting.WaffoSandboxPublicCert = "sandbox_public"
	require.False(t, isWaffoWebhookEnabled())

	setting.WaffoSandboxApiKey = "sandbox_api"
	require.True(t, isWaffoWebhookEnabled())
}

func TestWaffoPancakeWebhookEnabledRequiresTopUpAndWebhookConfig(t *testing.T) {
	originalMerchantID := setting.WaffoPancakeMerchantID
	originalPrivateKey := setting.WaffoPancakePrivateKey
	originalProductID := setting.WaffoPancakeProductID
	t.Cleanup(func() {
		setting.WaffoPancakeMerchantID = originalMerchantID
		setting.WaffoPancakePrivateKey = originalPrivateKey
		setting.WaffoPancakeProductID = originalProductID
	})

	// Presence of all three credentials enables the gateway. Webhook public
	// keys are bundled in the SDK and there is no separate Enabled toggle —
	// clear any of the three fields to disable.
	setting.WaffoPancakeMerchantID = ""
	setting.WaffoPancakePrivateKey = "private"
	setting.WaffoPancakeProductID = "product"
	require.False(t, isWaffoPancakeWebhookEnabled())

	setting.WaffoPancakeMerchantID = "merchant"
	require.True(t, isWaffoPancakeWebhookEnabled())

	setting.WaffoPancakeProductID = ""
	require.False(t, isWaffoPancakeWebhookEnabled())

	setting.WaffoPancakeProductID = "product"
	setting.WaffoPancakePrivateKey = ""
	require.False(t, isWaffoPancakeWebhookEnabled())
}

func TestEpayCheckoutAndWebhookUseTheirOwnCredentialRequirements(t *testing.T) {
	originalPayAddress := operation_setting.PayAddress
	originalEpayID := operation_setting.EpayId
	originalEpayKey := operation_setting.EpayKey
	paymentSetting := operation_setting.GetPaymentSetting()
	originalSignType := paymentSetting.EpaySignType
	originalMerchantPrivateKey := paymentSetting.EpayMerchantPrivateKey
	originalPlatformPublicKey := paymentSetting.EpayPlatformPublicKey
	originalPayMethods := operation_setting.PayMethods
	t.Cleanup(func() {
		operation_setting.PayAddress = originalPayAddress
		operation_setting.EpayId = originalEpayID
		operation_setting.EpayKey = originalEpayKey
		paymentSetting.EpaySignType = originalSignType
		paymentSetting.EpayMerchantPrivateKey = originalMerchantPrivateKey
		paymentSetting.EpayPlatformPublicKey = originalPlatformPublicKey
		operation_setting.PayMethods = originalPayMethods
	})

	operation_setting.PayAddress = "https://pay.example.com"
	operation_setting.EpayId = "epay_id"
	operation_setting.EpayKey = ""
	paymentSetting.EpaySignType = operation_setting.EpaySignTypeMD5
	operation_setting.PayMethods = []map[string]string{{"type": "alipay"}}
	require.False(t, isEpayTopUpEnabled())
	require.False(t, isEpayWebhookEnabled())

	operation_setting.EpayKey = "epay_key"
	require.True(t, isEpayTopUpEnabled())
	require.True(t, isEpayWebhookEnabled())

	operation_setting.PayMethods = nil
	require.False(t, isEpayTopUpEnabled())
	require.False(t, isEpayWebhookEnabled())

	// Existing MD5 orders remain eligible even before RSA checkout is configured.
	operation_setting.PayMethods = []map[string]string{{"type": "alipay"}}
	paymentSetting.EpaySignType = operation_setting.EpaySignTypeRSA
	paymentSetting.EpayMerchantPrivateKey = ""
	paymentSetting.EpayPlatformPublicKey = ""
	require.False(t, isEpayTopUpEnabled())
	require.True(t, isEpayWebhookEnabled())

	paymentSetting.EpayPlatformPublicKey = "platform_public_key"
	operation_setting.EpayKey = ""
	require.False(t, isEpayTopUpEnabled())
	require.True(t, isEpayWebhookEnabled())

	paymentSetting.EpayMerchantPrivateKey = "merchant_private_key"
	require.True(t, isEpayTopUpEnabled())
	require.True(t, isEpayWebhookEnabled())
}

// epayCallbackCase is one callback sent to every Epay callback endpoint.
type epayCallbackCase struct {
	name         string
	params       map[string]string
	validPending bool
}

// setupEpayCallbackTest configures an Epay gateway and leaves model.DB nil:
// rejected callbacks and valid non-success events must never reach settlement,
// so an accidental attempt to settle fails the test.
func setupEpayCallbackTest(t *testing.T) {
	t.Helper()
	originalPayAddress := operation_setting.PayAddress
	originalEpayID := operation_setting.EpayId
	originalEpayKey := operation_setting.EpayKey
	paymentSetting := operation_setting.GetPaymentSetting()
	originalSignType := paymentSetting.EpaySignType
	originalMerchantPrivateKey := paymentSetting.EpayMerchantPrivateKey
	originalPlatformPublicKey := paymentSetting.EpayPlatformPublicKey
	originalPayMethods := operation_setting.PayMethods
	originalServerAddress := system_setting.ServerAddress
	originalDB, originalLogDB := model.DB, model.LOG_DB
	originalMainDatabaseType, originalLogDatabaseType := common.MainDatabaseType(), common.LogDatabaseType()
	originalRedisEnabled := common.RedisEnabled
	t.Cleanup(func() {
		operation_setting.PayAddress = originalPayAddress
		operation_setting.EpayId = originalEpayID
		operation_setting.EpayKey = originalEpayKey
		paymentSetting.EpaySignType = originalSignType
		paymentSetting.EpayMerchantPrivateKey = originalMerchantPrivateKey
		paymentSetting.EpayPlatformPublicKey = originalPlatformPublicKey
		operation_setting.PayMethods = originalPayMethods
		system_setting.ServerAddress = originalServerAddress
		model.DB, model.LOG_DB = originalDB, originalLogDB
		common.SetDatabaseTypes(originalMainDatabaseType, originalLogDatabaseType)
		common.RedisEnabled = originalRedisEnabled
	})
	operation_setting.PayAddress = "https://pay.example.com"
	operation_setting.EpayId = "epay_id"
	operation_setting.PayMethods = []map[string]string{{"type": "alipay"}}
	system_setting.ServerAddress = "https://dashboard.example.com"
	model.DB = nil
}

// assertEpayCallbacksVerifyBeforeSettlement sends each callback to the top-up
// and subscription endpoints by GET and POST. Only a valid pending callback is
// accepted; every other callback is rejected.
func assertEpayCallbacksVerifyBeforeSettlement(t *testing.T, cases []epayCallbackCase) {
	t.Helper()
	endpoints := []struct {
		name             string
		path             string
		handler          gin.HandlerFunc
		status           int
		rejectedBody     string
		pendingBody      string
		rejectedLocation string
		pendingLocation  string
	}{
		{
			name: "topup notify", path: "/api/user/epay/notify", handler: EpayNotify,
			status: http.StatusOK, rejectedBody: "fail", pendingBody: "success",
		},
		{
			name: "subscription notify", path: "/api/subscription/epay/notify", handler: SubscriptionEpayNotify,
			status: http.StatusOK, rejectedBody: "fail", pendingBody: "fail",
		},
		{
			name: "subscription return", path: "/api/subscription/epay/return", handler: SubscriptionEpayReturn,
			status:           http.StatusFound,
			rejectedLocation: "https://dashboard.example.com/wallet?pay=fail",
			pendingLocation:  "https://dashboard.example.com/wallet?pay=pending",
		},
	}
	router := gin.New()
	for _, endpoint := range endpoints {
		router.Any(endpoint.path, endpoint.handler)
	}
	for _, endpoint := range endpoints {
		for _, method := range []string{http.MethodGet, http.MethodPost} {
			for _, tc := range cases {
				t.Run(endpoint.name+"/"+method+"/"+tc.name, func(t *testing.T) {
					params := url.Values{}
					for key, value := range tc.params {
						params.Set(key, value)
					}
					request := httptest.NewRequest(method, endpoint.path+"?"+params.Encode(), nil)
					if method == http.MethodPost {
						request = httptest.NewRequest(method, endpoint.path, strings.NewReader(params.Encode()))
						request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
					}
					recorder := httptest.NewRecorder()
					require.NotPanics(t, func() { router.ServeHTTP(recorder, request) })
					assert.Equal(t, endpoint.status, recorder.Code)
					wantBody, wantLocation := endpoint.rejectedBody, endpoint.rejectedLocation
					if tc.validPending {
						wantBody, wantLocation = endpoint.pendingBody, endpoint.pendingLocation
					}
					if wantBody != "" {
						assert.Equal(t, wantBody, recorder.Body.String())
					}
					assert.Equal(t, wantLocation, recorder.Header().Get("Location"))
				})
			}
		}
	}
}

// newEpayRSAKeyPairForTest returns a throwaway key pair as single-line Base64
// text, the format Epay merchant consoles show.
func newEpayRSAKeyPairForTest(t *testing.T) (string, string) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	privateDER, err := x509.MarshalPKCS8PrivateKey(key)
	require.NoError(t, err)
	publicDER, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	require.NoError(t, err)
	return base64.StdEncoding.EncodeToString(privateDER), base64.StdEncoding.EncodeToString(publicDER)
}

func TestEpayCallbacksVerifyBeforeSettlement(t *testing.T) {
	setupEpayCallbackTest(t)
	paymentSetting := operation_setting.GetPaymentSetting()
	paymentSetting.EpaySignType = operation_setting.EpaySignTypeMD5
	operation_setting.EpayKey = "test-key"
	require.True(t, isEpayWebhookEnabled())

	// Both ambiguous maps below had this same valid signing string in v0.0.4.
	// This must fail because of parameter ambiguity, not a mismatched signature.
	ambiguousSign := epay.MD5String("money=1.00&name=item&out_trade_no=USR1NOother&out_trade_no=USR1NOorder&pid=epay_id&trade_status=TRADE_SUCCESS&type=alipay", "test-key")
	pendingSign := epay.MD5String("money=1.00&name=item%26other&out_trade_no=USR1NOorder&pid=epay_id&trade_status=WAIT_BUYER_PAY&type=alipay", "test-key")
	rows := []struct {
		name         string
		productName  string
		tradeNo      string
		status       string
		sign         string
		validPending bool
	}{
		{"separator in product name", "item&out_trade_no=USR1NOother", "USR1NOorder", epay.StatusTradeSuccess, ambiguousSign, false},
		{"separator in order number", "item", "USR1NOother&out_trade_no=USR1NOorder", epay.StatusTradeSuccess, ambiguousSign, false},
		{"invalid signature", "item", "USR1NOorder", epay.StatusTradeSuccess, "invalid", false},
		{"missing signature", "item", "USR1NOorder", epay.StatusTradeSuccess, "", false},
		{"valid pending with literal percent escape", "item%26other", "USR1NOorder", "WAIT_BUYER_PAY", pendingSign, true},
	}
	cases := make([]epayCallbackCase, 0, len(rows))
	for _, row := range rows {
		params := map[string]string{
			"pid": "epay_id", "type": "alipay", "money": "1.00",
			"name": row.productName, "out_trade_no": row.tradeNo,
			"trade_status": row.status, "sign_type": "MD5",
		}
		if row.sign != "" {
			params["sign"] = row.sign
		}
		cases = append(cases, epayCallbackCase{name: row.name, params: params, validPending: row.validPending})
	}
	assertEpayCallbacksVerifyBeforeSettlement(t, cases)
}

func TestEpayRSAModeSignsOrdersAndVerifiesCallbacks(t *testing.T) {
	setupEpayCallbackTest(t)
	paymentSetting := operation_setting.GetPaymentSetting()
	merchantPrivateKey, merchantPublicKey := newEpayRSAKeyPairForTest(t)
	platformPrivateKey, platformPublicKey := newEpayRSAKeyPairForTest(t)
	paymentSetting.EpaySignType = operation_setting.EpaySignTypeRSA
	// A leftover MD5 key must not be accepted once RSA is selected.
	operation_setting.EpayKey = "test-key"
	paymentSetting.EpayMerchantPrivateKey = merchantPrivateKey
	paymentSetting.EpayPlatformPublicKey = platformPublicKey
	require.True(t, isEpayWebhookEnabled())

	client := GetEpayClient()
	require.NotNil(t, client)
	tradeNo := newEpayTradeNo(1, client, false)
	notifyURL, err := url.Parse("https://dashboard.example.com/api/user/epay/notify")
	require.NoError(t, err)
	returnURL, err := url.Parse("https://dashboard.example.com/usage-logs")
	require.NoError(t, err)
	paymentURL, order, err := client.Purchase(&epay.PurchaseArgs{
		Type: "alipay", ServiceTradeNo: tradeNo, Name: "TUC1", Money: "1.00",
		Device: epay.PC, NotifyUrl: notifyURL, ReturnUrl: returnURL,
	})
	require.NoError(t, err)
	assert.Equal(t, "https://pay.example.com/api/pay/submit", paymentURL)
	assert.Equal(t, epay.SignTypeRSA, order["sign_type"])

	// The platform side holds the merchant public key and signs callbacks with its own private key.
	platform, err := epay.NewClient(&epay.Config{
		PartnerID: "epay_id", SignType: epay.SignTypeRSA,
		MerchantPrivateKey: platformPrivateKey, PlatformPublicKey: merchantPublicKey,
	}, "https://pay.example.com")
	require.NoError(t, err)
	verifiedOrder, err := platform.Verify(order)
	require.NoError(t, err)
	assert.True(t, verifiedOrder.VerifyStatus)

	pending, err := platform.SignParams(map[string]string{
		"type": "alipay", "money": "1.00", "name": "item", "out_trade_no": tradeNo, "trade_status": "WAIT_BUYER_PAY",
	})
	require.NoError(t, err)
	paidFields := map[string]string{
		"type": "alipay", "money": "1.00", "name": "item", "out_trade_no": tradeNo, "trade_status": epay.StatusTradeSuccess,
	}
	paid, err := platform.SignParams(paidFields)
	require.NoError(t, err)
	otherMerchantConfig := *platform.Config
	otherMerchantConfig.PartnerID = "different_merchant"
	otherMerchant, err := epay.NewClient(&otherMerchantConfig, "https://pay.example.com")
	require.NoError(t, err)
	otherMerchantSigned, err := otherMerchant.SignParams(paidFields)
	require.NoError(t, err)
	tampered := maps.Clone(paid)
	tampered["money"] = "100.00"
	merchantSigned, err := client.SignParams(paidFields)
	require.NoError(t, err)
	md5Client, err := epay.NewClient(&epay.Config{PartnerID: "epay_id", Key: "test-key"}, "https://pay.example.com")
	require.NoError(t, err)
	md5Signed, err := md5Client.SignParams(paidFields)
	require.NoError(t, err)
	unsigned := maps.Clone(paid)
	delete(unsigned, "sign")

	assertEpayCallbacksVerifyBeforeSettlement(t, []epayCallbackCase{
		{name: "valid pending signed by platform", params: pending, validPending: true},
		{name: "tampered amount", params: tampered},
		{name: "signed with merchant key", params: merchantSigned},
		{name: "valid platform signature for a different merchant", params: otherMerchantSigned},
		{name: "MD5 signature for an RSA order", params: md5Signed},
		{name: "missing signature", params: unsigned},
	})
}

func TestUpdateOptionValidatesEpaySignSettings(t *testing.T) {
	db := setupModelListControllerTestDB(t)
	require.NoError(t, db.AutoMigrate(&model.Option{}, &model.SystemTask{}))
	originalOptionMap := common.OptionMap
	paymentSetting := operation_setting.GetPaymentSetting()
	originalSignType := paymentSetting.EpaySignType
	originalMerchantPrivateKey := paymentSetting.EpayMerchantPrivateKey
	originalPlatformPublicKey := paymentSetting.EpayPlatformPublicKey
	t.Cleanup(func() {
		common.OptionMap = originalOptionMap
		paymentSetting.EpaySignType = originalSignType
		paymentSetting.EpayMerchantPrivateKey = originalMerchantPrivateKey
		paymentSetting.EpayPlatformPublicKey = originalPlatformPublicKey
	})
	// A site upgraded from an MD5-only release has no saved RSA settings.
	common.OptionMap = map[string]string{
		operation_setting.EpaySignTypeOptionKey:           operation_setting.EpaySignTypeMD5,
		operation_setting.EpayMerchantPrivateKeyOptionKey: "",
		operation_setting.EpayPlatformPublicKeyOptionKey:  "",
	}
	paymentSetting.EpaySignType = operation_setting.EpaySignTypeMD5
	paymentSetting.EpayMerchantPrivateKey = ""
	paymentSetting.EpayPlatformPublicKey = ""
	signTypeKey := operation_setting.EpaySignTypeOptionKey
	privateKeyKey := operation_setting.EpayMerchantPrivateKeyOptionKey
	publicKeyKey := operation_setting.EpayPlatformPublicKeyOptionKey
	merchantPrivateKey, _ := newEpayRSAKeyPairForTest(t)
	_, platformPublicKey := newEpayRSAKeyPairForTest(t)
	const privateKeyError = "The Epay merchant private key is missing or invalid"
	const publicKeyError = "The Epay platform public key is missing or invalid"

	steps := []struct {
		name      string
		key       string
		value     string
		wantError string
	}{
		{"unknown signature type", signTypeKey, "SHA1", "Epay signature type must be MD5 or RSA"},
		{"RSA before keys are saved", signTypeKey, "RSA", privateKeyError},
		{"malformed private key", privateKeyKey, "not a key", privateKeyError},
		{"public key in the private key field", privateKeyKey, platformPublicKey, privateKeyError},
		{"save private key", privateKeyKey, merchantPrivateKey, ""},
		{"RSA before the public key is saved", signTypeKey, "RSA", publicKeyError},
		{"private key in the public key field", publicKeyKey, merchantPrivateKey, publicKeyError},
		{"save public key", publicKeyKey, platformPublicKey, ""},
		{"switch to RSA", signTypeKey, "RSA", ""},
		{"clear private key while RSA is active", privateKeyKey, "", privateKeyError},
		{"switch back to MD5", signTypeKey, "MD5", ""},
	}
	for _, step := range steps {
		err := model.UpdateOption(step.key, step.value)
		if step.wantError == "" {
			require.NoError(t, err, step.name)
			continue
		}
		require.Error(t, err, step.name)
		assert.Equal(t, step.wantError, err.Error(), step.name)
	}
	assert.Equal(t, operation_setting.EpaySignTypeMD5, paymentSetting.EpaySignType)
	assert.Equal(t, merchantPrivateKey, paymentSetting.EpayMerchantPrivateKey)
	assert.Equal(t, platformPublicKey, paymentSetting.EpayPlatformPublicKey)

	// The private key is never sent back to the settings page; the public key may be.
	recorder := httptest.NewRecorder()
	context, _ := gin.CreateTestContext(recorder)
	context.Request = httptest.NewRequest(http.MethodGet, "/api/option/", nil)
	GetOptions(context)
	var response struct {
		Success bool           `json:"success"`
		Data    []model.Option `json:"data"`
	}
	require.NoError(t, common.Unmarshal(recorder.Body.Bytes(), &response))
	require.True(t, response.Success)
	returned := make(map[string]string, len(response.Data))
	for _, option := range response.Data {
		returned[option.Key] = option.Value
	}
	assert.Equal(t, operation_setting.EpaySignTypeMD5, returned[signTypeKey])
	assert.Equal(t, platformPublicKey, returned[publicKeyKey])
	assert.NotContains(t, returned, privateKeyKey)
}

func TestEpayPendingOrdersSettleAfterSigningModeSwitch(t *testing.T) {
	setupEpayCallbackTest(t)
	db := setupModelListControllerTestDB(t)
	require.NoError(t, db.AutoMigrate(&model.TopUp{}, &model.SubscriptionPlan{}, &model.SubscriptionOrder{}, &model.UserSubscription{}, &model.UserQuotaDebt{}, &model.Log{}, &model.Option{}, &model.SystemTask{}))
	originalOptionMap := common.OptionMap
	originalPrice, originalMinTopUp, originalQuotaPerUnit := operation_setting.Price, operation_setting.MinTopUp, common.QuotaPerUnit
	generalSetting := operation_setting.GetGeneralSetting()
	originalDisplayType := generalSetting.QuotaDisplayType
	t.Cleanup(func() {
		common.OptionMap = originalOptionMap
		operation_setting.Price, operation_setting.MinTopUp, common.QuotaPerUnit = originalPrice, originalMinTopUp, originalQuotaPerUnit
		generalSetting.QuotaDisplayType = originalDisplayType
	})
	common.OptionMap = make(map[string]string)
	operation_setting.Price, operation_setting.MinTopUp, common.QuotaPerUnit = 1, 1, 500000
	generalSetting.QuotaDisplayType = operation_setting.QuotaDisplayTypeUSD

	merchantPrivateKey, merchantPublicKey := newEpayRSAKeyPairForTest(t)
	platformPrivateKey, platformPublicKey := newEpayRSAKeyPairForTest(t)
	paymentSetting := operation_setting.GetPaymentSetting()
	operation_setting.EpayKey = "test-key"
	paymentSetting.EpayMerchantPrivateKey = merchantPrivateKey
	paymentSetting.EpayPlatformPublicKey = platformPublicKey
	address := operation_setting.PayAddress
	partnerID := operation_setting.EpayId
	md5Platform, err := epay.NewClient(&epay.Config{PartnerID: partnerID, Key: operation_setting.EpayKey}, address)
	require.NoError(t, err)
	rsaPlatform, err := epay.NewClient(&epay.Config{
		PartnerID: partnerID, SignType: epay.SignTypeRSA,
		MerchantPrivateKey: platformPrivateKey, PlatformPublicKey: merchantPublicKey,
	}, address)
	require.NoError(t, err)
	plan := model.SubscriptionPlan{Title: "Epay test plan", PriceAmount: 1.00, Enabled: true}
	require.NoError(t, db.Create(&plan).Error)
	model.InvalidateSubscriptionPlanCache(plan.Id)
	t.Cleanup(func() { model.InvalidateSubscriptionPlanCache(plan.Id) })

	for _, tc := range []struct {
		issuedMode, activeMode        string
		orderSigner, activeModeSigner *epay.Client
	}{
		{epay.SignTypeMD5, epay.SignTypeRSA, md5Platform, rsaPlatform},
		{epay.SignTypeRSA, epay.SignTypeMD5, rsaPlatform, md5Platform},
	} {
		t.Run(tc.issuedMode+" to "+tc.activeMode, func(t *testing.T) {
			user := model.User{Username: "epay-" + tc.issuedMode, AffCode: tc.issuedMode, Group: "default"}
			require.NoError(t, db.Create(&user).Error)
			for _, flow := range []struct {
				name, payload, notifyPath string
				checkout, notify          gin.HandlerFunc
			}{
				{"top-up", `{"amount":2,"payment_method":"alipay"}`, "/api/user/epay/notify", RequestEpay, EpayNotify},
				{"subscription", fmt.Sprintf(`{"plan_id":%d,"payment_method":"alipay"}`, plan.Id), "/api/subscription/epay/notify", SubscriptionRequestEpay, SubscriptionEpayNotify},
			} {
				t.Run(flow.name, func(t *testing.T) {
					require.NoError(t, model.UpdateOption(operation_setting.EpaySignTypeOptionKey, tc.issuedMode))
					checkout := httptest.NewRecorder()
					ctx, _ := gin.CreateTestContext(checkout)
					ctx.Request = httptest.NewRequest(http.MethodPost, "/", strings.NewReader(flow.payload))
					ctx.Request.Header.Set("Content-Type", "application/json")
					ctx.Set("id", user.Id)
					flow.checkout(ctx)
					var response struct {
						Message string            `json:"message"`
						Data    map[string]string `json:"data"`
					}
					require.NoError(t, common.Unmarshal(checkout.Body.Bytes(), &response), checkout.Body.String())
					require.Equal(t, "success", response.Message, checkout.Body.String())
					require.Equal(t, tc.issuedMode, response.Data["sign_type"])
					tradeNo := response.Data["out_trade_no"]
					require.NoError(t, model.UpdateOption(operation_setting.EpaySignTypeOptionKey, tc.activeMode))

					callback := map[string]string{
						"type": "alipay", "out_trade_no": tradeNo, "money": response.Data["money"], "trade_status": epay.StatusTradeSuccess,
					}
					for _, attempt := range []struct {
						signer *epay.Client
						body   string
					}{
						{tc.activeModeSigner, "fail"},
						{tc.orderSigner, "success"},
						{tc.orderSigner, "success"},
					} {
						signed, err := attempt.signer.SignParams(callback)
						require.NoError(t, err)
						params := url.Values{}
						for key, value := range signed {
							params.Set(key, value)
						}
						notify := httptest.NewRecorder()
						ctx, _ := gin.CreateTestContext(notify)
						ctx.Request = httptest.NewRequest(http.MethodPost, flow.notifyPath, strings.NewReader(params.Encode()))
						ctx.Request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
						flow.notify(ctx)
						require.Equal(t, attempt.body, notify.Body.String())
					}
				})
			}

			var storedUser model.User
			require.NoError(t, db.First(&storedUser, user.Id).Error)
			assert.Equal(t, 1000000, storedUser.Quota)
			var subscriptions int64
			require.NoError(t, db.Model(&model.UserSubscription{}).Where("user_id = ?", user.Id).Count(&subscriptions).Error)
			assert.EqualValues(t, 1, subscriptions)
		})
	}
}
