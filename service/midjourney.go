package service

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/logger"
	"github.com/QuantumNous/new-api/model"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	relayconstant "github.com/QuantumNous/new-api/relay/constant"
	"github.com/QuantumNous/new-api/setting"

	"github.com/gin-gonic/gin"
)

func CovertMjpActionToModelName(mjAction string) string {
	modelName := "mj_" + strings.ToLower(mjAction)
	if mjAction == constant.MjActionSwapFace {
		modelName = "swap_face"
	}
	return modelName
}

// PrepareMidjourneyTaskBilling sets the durable refund marker before the task is inserted.
func PrepareMidjourneyTaskBilling(relayInfo *relaycommon.RelayInfo, task *model.Midjourney, quota int, shouldBill bool) (bool, error) {
	if task == nil {
		return false, errors.New("Midjourney task is nil")
	}
	task.Quota = 0
	task.TokenId = 0
	task.BillingChannelId = 0
	task.BillingStatus = ""
	task.BillingQuotaDelta = 0
	if !shouldBill {
		return false, nil
	}
	if relayInfo == nil {
		return false, errors.New("relay info is nil")
	}
	if quota < 0 {
		return false, errors.New("quota cannot be negative")
	}
	if relayInfo.BillingSource == BillingSourceSubscription {
		return false, errors.New("legacy Midjourney billing does not support subscriptions")
	}

	task.Quota = quota
	task.BillingStatus = model.MidjourneyBillingStatusPrepared
	if !relayInfo.IsPlayground {
		task.TokenId = relayInfo.TokenId
	}
	task.BillingChannelId = task.ChannelId
	if relayInfo.ChannelMeta != nil && relayInfo.ChannelId > 0 {
		task.BillingChannelId = relayInfo.ChannelId
	}
	return true, nil
}

// ReserveMidjourneyTaskBilling persists and charges a placeholder before the
// request may reach an upstream Midjourney service.
func ReserveMidjourneyTaskBilling(relayInfo *relaycommon.RelayInfo, task *model.Midjourney, quota int) (bool, error) {
	if task == nil {
		return false, errors.New("Midjourney task is nil")
	}
	task.Status = constant.MjStatusReserving
	prepared, err := PrepareMidjourneyTaskBilling(relayInfo, task, quota, true)
	if err != nil {
		return false, err
	}
	if err := task.Insert(); err != nil {
		return false, err
	}

	applied, billingErr := SettleMidjourneyTaskBilling(relayInfo, task, prepared)
	if billingErr != nil {
		task.Status = "FAILURE"
		task.Progress = "100%"
		updateErr := task.Update()
		if updateErr == nil {
			ReconcileMidjourneyTaskBilling(context.Background(), task)
		}
		return false, errors.Join(billingErr, updateErr)
	}
	if !applied {
		return false, errors.New("Midjourney quota reservation was not applied")
	}

	task.Status = constant.MjStatusSubmitting
	if err := task.Update(); err != nil {
		_, refundErr := task.RefundBilling(relayInfo.TokenKey)
		return false, errors.Join(err, refundErr)
	}
	return true, nil
}

// SettleMidjourneyTaskBilling charges a persisted legacy task and records the applied stages.
func SettleMidjourneyTaskBilling(relayInfo *relaycommon.RelayInfo, task *model.Midjourney, prepared bool) (bool, error) {
	if !prepared {
		return false, nil
	}
	if relayInfo == nil {
		return false, errors.New("relay info is nil")
	}
	if task == nil || task.Id == 0 {
		return false, errors.New("Midjourney task must be persisted before billing")
	}

	result, billingErr := task.SettleBilling(relayInfo.TokenKey)
	if billingErr != nil {
		return false, billingErr
	}
	return result.Applied, nil
}

// NotifyMidjourneyQuota sends the low-balance notification only after the
// upstream has explicitly accepted the charged task.
func NotifyMidjourneyQuota(relayInfo *relaycommon.RelayInfo, quota int) {
	checkAndSendQuotaNotify(relayInfo, quota, 0)
}

// ReconcileMidjourneyTaskBilling resumes a durable charge or refund left
// incomplete by a process or cache failure. Terminal failed tasks are never
// charged solely by recovery; an uncommitted charge is cancelled instead.
func ReconcileMidjourneyTaskBilling(ctx context.Context, task *model.Midjourney) bool {
	if task == nil {
		return true
	}
	if ctx == nil {
		ctx = context.Background()
	}

	failed := task.Status == "FAILURE"
	switch task.BillingStatus {
	case model.MidjourneyBillingStatusPrepared, model.MidjourneyBillingStatusCharging:
		if failed {
			return RefundMidjourneyQuota(ctx, task, "计费恢复时任务已失败")
		}
		return reconcileMidjourneyCharge(ctx, task)
	case model.MidjourneyBillingStatusChargePending:
		if !reconcileMidjourneyCharge(ctx, task) {
			return false
		}
		if task.Status == "FAILURE" {
			return RefundMidjourneyQuota(ctx, task, "计费恢复时任务已失败")
		}
		return true
	case model.MidjourneyBillingStatusCharged:
		if failed && task.Quota > 0 {
			return RefundMidjourneyQuota(ctx, task, "任务失败退款重试")
		}
		return true
	case model.MidjourneyBillingStatusRefunding, model.MidjourneyBillingStatusRefundPending:
		return RefundMidjourneyQuota(ctx, task, "退款状态恢复")
	case "":
		if failed && task.Quota > 0 {
			return RefundMidjourneyQuota(ctx, task, "历史任务失败退款重试")
		}
		return true
	default:
		logger.LogWarn(ctx, fmt.Sprintf("未知的 Midjourney 计费状态 task %s: %s", task.MjId, task.BillingStatus))
		return false
	}
}

func reconcileMidjourneyCharge(ctx context.Context, task *model.Midjourney) bool {
	tokenKey := ""
	if task.TokenId > 0 {
		tokenKey = resolveTokenKey(ctx, task.TokenId, task.MjId)
	}
	result, err := task.SettleBilling(tokenKey)
	if err != nil {
		logger.LogWarn(ctx, fmt.Sprintf("恢复 Midjourney 扣费失败 task %s: %s", task.MjId, err.Error()))
		return false
	}
	if !result.Applied {
		return true
	}

	model.RecordTaskBillingLog(model.RecordTaskBillingLogParams{
		UserId:    task.UserId,
		LogType:   model.LogTypeConsume,
		Content:   "Midjourney 计费状态恢复",
		ChannelId: task.GetBillingChannelId(),
		ModelName: CovertMjpActionToModelName(task.Action),
		Quota:     task.Quota,
		TokenId:   task.TokenId,
		Other: map[string]interface{}{
			"task_id": task.MjId,
			"reason":  "billing_recovery",
		},
	})
	return true
}

// RefundMidjourneyQuota reverses every accounting element recorded for a billed legacy task.
func RefundMidjourneyQuota(ctx context.Context, task *model.Midjourney, reason string) bool {
	tokenKey := ""
	if task.TokenId > 0 {
		tokenKey = resolveTokenKey(ctx, task.TokenId, task.MjId)
	}
	result, err := task.RefundBilling(tokenKey)
	if err != nil {
		logger.LogWarn(ctx, fmt.Sprintf("退还 Midjourney 额度失败 task %s: %s", task.MjId, err.Error()))
		return false
	}
	if !result.Completed {
		return false
	}
	if !result.Applied {
		return true
	}

	model.RecordTaskBillingLog(model.RecordTaskBillingLogParams{
		UserId:    task.UserId,
		LogType:   model.LogTypeRefund,
		Content:   "",
		ChannelId: result.BillingChannelId,
		ModelName: CovertMjpActionToModelName(task.Action),
		Quota:     result.Quota,
		TokenId:   result.TokenId,
		Other: map[string]interface{}{
			"task_id": task.MjId,
			"reason":  reason,
		},
	})

	return true
}

func GetMjRequestModel(relayMode int, midjRequest *dto.MidjourneyRequest) (string, *dto.MidjourneyResponse, bool) {
	action := ""
	if relayMode == relayconstant.RelayModeMidjourneyAction {
		// plus request
		err := CoverPlusActionToNormalAction(midjRequest)
		if err != nil {
			return "", err, false
		}
		action = midjRequest.Action
	} else {
		switch relayMode {
		case relayconstant.RelayModeMidjourneyImagine:
			action = constant.MjActionImagine
		case relayconstant.RelayModeMidjourneyVideo:
			action = constant.MjActionVideo
		case relayconstant.RelayModeMidjourneyEdits:
			action = constant.MjActionEdits
		case relayconstant.RelayModeMidjourneyDescribe:
			action = constant.MjActionDescribe
		case relayconstant.RelayModeMidjourneyBlend:
			action = constant.MjActionBlend
		case relayconstant.RelayModeMidjourneyShorten:
			action = constant.MjActionShorten
		case relayconstant.RelayModeMidjourneyChange:
			action = midjRequest.Action
		case relayconstant.RelayModeMidjourneyModal:
			action = constant.MjActionModal
		case relayconstant.RelayModeSwapFace:
			action = constant.MjActionSwapFace
		case relayconstant.RelayModeMidjourneyUpload:
			action = constant.MjActionUpload
		case relayconstant.RelayModeMidjourneySimpleChange:
			params := ConvertSimpleChangeParams(midjRequest.Content)
			if params == nil {
				return "", MidjourneyErrorWrapper(constant.MjRequestError, "invalid_request"), false
			}
			action = params.Action
		case relayconstant.RelayModeMidjourneyTaskFetch, relayconstant.RelayModeMidjourneyTaskFetchByCondition, relayconstant.RelayModeMidjourneyNotify:
			return "", nil, true
		default:
			return "", MidjourneyErrorWrapper(constant.MjRequestError, "unknown_relay_action"), false
		}
	}
	modelName := CovertMjpActionToModelName(action)
	return modelName, nil, true
}

func CoverPlusActionToNormalAction(midjRequest *dto.MidjourneyRequest) *dto.MidjourneyResponse {
	// "customId": "MJ::JOB::upsample::2::3dbbd469-36af-4a0f-8f02-df6c579e7011"
	customId := midjRequest.CustomId
	if customId == "" {
		return MidjourneyErrorWrapper(constant.MjRequestError, "custom_id_is_required")
	}
	splits := strings.Split(customId, "::")
	var action string
	if splits[1] == "JOB" {
		action = splits[2]
	} else {
		action = splits[1]
	}

	if action == "" {
		return MidjourneyErrorWrapper(constant.MjRequestError, "unknown_action")
	}
	if strings.Contains(action, "upsample") {
		index, err := strconv.Atoi(splits[3])
		if err != nil {
			return MidjourneyErrorWrapper(constant.MjRequestError, "index_parse_failed")
		}
		midjRequest.Index = index
		midjRequest.Action = constant.MjActionUpscale
	} else if strings.Contains(action, "variation") {
		midjRequest.Index = 1
		if action == "variation" {
			index, err := strconv.Atoi(splits[3])
			if err != nil {
				return MidjourneyErrorWrapper(constant.MjRequestError, "index_parse_failed")
			}
			midjRequest.Index = index
			midjRequest.Action = constant.MjActionVariation
		} else if action == "low_variation" {
			midjRequest.Action = constant.MjActionLowVariation
		} else if action == "high_variation" {
			midjRequest.Action = constant.MjActionHighVariation
		}
	} else if strings.Contains(action, "pan") {
		midjRequest.Action = constant.MjActionPan
		midjRequest.Index = 1
	} else if strings.Contains(action, "reroll") {
		midjRequest.Action = constant.MjActionReRoll
		midjRequest.Index = 1
	} else if action == "Outpaint" {
		midjRequest.Action = constant.MjActionZoom
		midjRequest.Index = 1
	} else if action == "CustomZoom" {
		midjRequest.Action = constant.MjActionCustomZoom
		midjRequest.Index = 1
	} else if action == "Inpaint" {
		midjRequest.Action = constant.MjActionInPaint
		midjRequest.Index = 1
	} else {
		return MidjourneyErrorWrapper(constant.MjRequestError, "unknown_action:"+customId)
	}
	return nil
}

func ConvertSimpleChangeParams(content string) *dto.MidjourneyRequest {
	split := strings.Split(content, " ")
	if len(split) != 2 {
		return nil
	}

	action := strings.ToLower(split[1])
	changeParams := &dto.MidjourneyRequest{}
	changeParams.TaskId = split[0]

	if action[0] == 'u' {
		changeParams.Action = "UPSCALE"
	} else if action[0] == 'v' {
		changeParams.Action = "VARIATION"
	} else if action == "r" {
		changeParams.Action = "REROLL"
		return changeParams
	} else {
		return nil
	}

	index, err := strconv.Atoi(action[1:2])
	if err != nil || index < 1 || index > 4 {
		return nil
	}
	changeParams.Index = index
	return changeParams
}

// ErrMidjourneySubmissionUnknown means the request may have reached upstream,
// so the durable reservation must not be refunded automatically.
var ErrMidjourneySubmissionUnknown = errors.New("midjourney submission outcome is unknown")

// BuildMidjourneyHttpRequest completes all local parsing and request creation
// before quota is reserved or the upstream can be contacted.
func BuildMidjourneyHttpRequest(c *gin.Context, fullRequestURL string) (*http.Request, error) {
	var mapResult map[string]interface{}
	if c.Request.Method != "GET" {
		if err := common.DecodeJson(c.Request.Body, &mapResult); err != nil {
			return nil, err
		}
		if !setting.MjAccountFilterEnabled {
			delete(mapResult, "accountFilter")
		}
		if !setting.MjNotifyEnabled {
			delete(mapResult, "notifyHook")
		}
	}
	if setting.MjModeClearEnabled {
		if prompt, ok := mapResult["prompt"].(string); ok {
			prompt = strings.Replace(prompt, "--fast", "", -1)
			prompt = strings.Replace(prompt, "--relax", "", -1)
			prompt = strings.Replace(prompt, "--turbo", "", -1)

			mapResult["prompt"] = prompt
		}
	}
	reqBody, err := common.Marshal(mapResult)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequest(c.Request.Method, fullRequestURL, strings.NewReader(string(reqBody)))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", c.Request.Header.Get("Content-Type"))
	req.Header.Set("Accept", c.Request.Header.Get("Accept"))
	auth := common.GetContextKeyString(c, constant.ContextKeyChannelKey)
	if auth != "" {
		auth = strings.TrimPrefix(auth, "Bearer ")
		req.Header.Set("mj-api-secret", auth)
	}
	return req, nil
}

// DoPreparedMidjourneyHttpRequest sends an already-built request. Any error
// after this boundary is ambiguous and must retain the pre-reserved charge.
func DoPreparedMidjourneyHttpRequest(c *gin.Context, timeout time.Duration, req *http.Request) (*dto.MidjourneyResponseWithStatusCode, []byte, error) {
	var nullBytes []byte
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	resp, err := GetHttpClient().Do(req.WithContext(ctx))
	if err != nil {
		common.SysLog("do request failed: " + err.Error())
		return MidjourneyErrorWithStatusCodeWrapper(constant.MjErrorUnknown, "do_request_failed", http.StatusInternalServerError), nullBytes, fmt.Errorf("%w: %v", ErrMidjourneySubmissionUnknown, err)
	}
	statusCode := resp.StatusCode
	var midjResponse dto.MidjourneyResponse
	var midjourneyUploadsResponse dto.MidjourneyUploadResponse
	responseBody, err := io.ReadAll(resp.Body)
	if err != nil {
		CloseResponseBodyGracefully(resp)
		return MidjourneyErrorWithStatusCodeWrapper(constant.MjErrorUnknown, "read_response_body_failed", statusCode), nullBytes, fmt.Errorf("%w: %v", ErrMidjourneySubmissionUnknown, err)
	}
	CloseResponseBodyGracefully(resp)
	logger.LogDebug(c, "midjourney response body: %s", responseBody)
	if len(responseBody) == 0 {
		err := errors.New("empty response body")
		return MidjourneyErrorWithStatusCodeWrapper(constant.MjErrorUnknown, "empty_response_body", statusCode), responseBody, fmt.Errorf("%w: %v", ErrMidjourneySubmissionUnknown, err)
	}
	if err := common.Unmarshal(responseBody, &midjResponse); err != nil {
		if uploadErr := common.Unmarshal(responseBody, &midjourneyUploadsResponse); uploadErr != nil {
			return MidjourneyErrorWithStatusCodeWrapper(constant.MjErrorUnknown, "unmarshal_response_body_failed", statusCode), responseBody, fmt.Errorf("%w: %v", ErrMidjourneySubmissionUnknown, errors.Join(err, uploadErr))
		}
	}
	return &dto.MidjourneyResponseWithStatusCode{
		StatusCode: statusCode,
		Response:   midjResponse,
	}, responseBody, nil
}

func DoMidjourneyHttpRequest(c *gin.Context, timeout time.Duration, fullRequestURL string) (*dto.MidjourneyResponseWithStatusCode, []byte, error) {
	request, err := BuildMidjourneyHttpRequest(c, fullRequestURL)
	if err != nil {
		return MidjourneyErrorWithStatusCodeWrapper(constant.MjErrorUnknown, "build_request_failed", http.StatusInternalServerError), nil, err
	}
	return DoPreparedMidjourneyHttpRequest(c, timeout, request)
}
