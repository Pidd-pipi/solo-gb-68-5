package controllers

import (
	"errors"
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"

	"irrigation/internal/services"
	"irrigation/pkg/response"
)

type DecisionController struct {
	decisionService *services.DecisionService
}

func NewDecisionController() *DecisionController {
	return &DecisionController{
		decisionService: services.NewDecisionService(),
	}
}

// Evaluate godoc
// @Summary 智能灌溉决策
// @Description 提交区域、目标湿度和最长时长，结合最新土壤湿度、近期降雨和预报降雨，返回是否浇水、原因和建议时长；决策结果持久化并在有效期内可用于确认执行
// @Tags 智能决策
// @Security ApiKeyAuth
// @Accept json
// @Produce json
// @Param request body services.DecisionRequest true "决策请求"
// @Success 200 {object} services.DecisionResult
// @Failure 404 {object} response.Response "区域不存在"
// @Router /api/irrigation/decision [post]
func (c *DecisionController) Evaluate(ctx *gin.Context) {
	var req services.DecisionRequest
	if err := ctx.ShouldBindJSON(&req); err != nil {
		response.BadRequest(ctx, "Invalid request body: "+err.Error())
		return
	}

	result, err := c.decisionService.Evaluate(req)
	if err != nil {
		if errors.Is(err, services.ErrZoneNotFound) {
			response.NotFound(ctx, "区域不存在")
			return
		}
		response.InternalServerError(ctx, err.Error())
		return
	}

	response.Success(ctx, result)
}

// Confirm godoc
// @Summary 确认执行灌溉
// @Description 引用同一次决策的有效结果确认执行灌溉。决策过期、已使用、区域不一致或计划时长超过决策最长时长都会被拒绝；同时要求区域存在、有可用灌溉设备且无执行中任务
// @Tags 智能决策
// @Security ApiKeyAuth
// @Accept json
// @Produce json
// @Param decision_id body int true "决策记录ID"
// @Param zone_id body int true "区域ID（须与决策一致）"
// @Param duration body int true "计划灌溉时长（秒，不超过决策最长时长）"
// @Success 201 {object} models.IrrigationLog
// @Failure 400 {object} response.Response "计划时长不是正值"
// @Failure 404 {object} response.Response "决策结果或区域不存在"
// @Failure 409 {object} response.Response "决策已被使用或已有执行中的灌溉任务"
// @Failure 410 {object} response.Response "决策结果已过期"
// @Failure 422 {object} response.Response "区域与决策不一致、时长超限或设备不可用"
// @Router /api/irrigation/decision/confirm [post]
func (c *DecisionController) Confirm(ctx *gin.Context) {
	var req struct {
		DecisionID uint `json:"decision_id" binding:"required"`
		ZoneID     uint `json:"zone_id" binding:"required"`
		Duration   int  `json:"duration" binding:"required,gt=0"`
	}
	if err := ctx.ShouldBindJSON(&req); err != nil {
		response.BadRequest(ctx, "Invalid request body: "+err.Error())
		return
	}

	log, err := c.decisionService.ConfirmExecution(req.DecisionID, req.ZoneID, req.Duration)
	if err != nil {
		switch {
		case errors.Is(err, services.ErrDecisionNotFound):
			response.NotFound(ctx, "决策结果不存在")
		case errors.Is(err, services.ErrDecisionExpired):
			response.Error(ctx, http.StatusGone, "决策结果已过期")
		case errors.Is(err, services.ErrDecisionAlreadyUsed):
			response.Error(ctx, http.StatusConflict, "该决策结果已被使用")
		case errors.Is(err, services.ErrDecisionZoneMismatch):
			response.Error(ctx, http.StatusUnprocessableEntity, "区域与决策结果不一致")
		case errors.Is(err, services.ErrDurationExceedsLimit):
			response.Error(ctx, http.StatusUnprocessableEntity, "计划时长超过决策的最长时长")
		case errors.Is(err, services.ErrInvalidDuration):
			response.BadRequest(ctx, "计划时长必须为正值")
		case errors.Is(err, services.ErrZoneNotFound):
			response.NotFound(ctx, "区域不存在")
		case errors.Is(err, services.ErrDeviceUnavailable):
			response.Error(ctx, http.StatusUnprocessableEntity, "区域内无可用灌溉设备")
		case errors.Is(err, services.ErrIrrigationInProgress):
			response.Error(ctx, http.StatusConflict, "该区域已有执行中的灌溉任务")
		default:
			response.InternalServerError(ctx, err.Error())
		}
		return
	}

	response.Created(ctx, log)
}

// Complete godoc
// @Summary 结束灌溉执行
// @Description 结束执行中的灌溉任务，写回实际时长和估算用水量。实际时长必须为正值且不超过确认时的计划时长，超限或重复完成将被拒绝且原记录不变
// @Tags 智能决策
// @Security ApiKeyAuth
// @Accept json
// @Produce json
// @Param id path int true "执行记录ID"
// @Param actual_duration body int true "实际灌溉时长（秒）"
// @Success 200 {object} models.IrrigationLog
// @Failure 400 {object} response.Response "实际时长不是正值"
// @Failure 404 {object} response.Response "执行记录不存在"
// @Failure 409 {object} response.Response "记录不是执行中状态"
// @Failure 422 {object} response.Response "实际时长超过确认的计划时长"
// @Router /api/irrigation/logs/{id}/complete [post]
func (c *DecisionController) Complete(ctx *gin.Context) {
	id, err := strconv.ParseUint(ctx.Param("id"), 10, 32)
	if err != nil {
		response.BadRequest(ctx, "Invalid log id")
		return
	}

	var req struct {
		ActualDuration int `json:"actual_duration" binding:"required,gt=0"`
	}
	if err := ctx.ShouldBindJSON(&req); err != nil {
		response.BadRequest(ctx, "Invalid request body: "+err.Error())
		return
	}

	log, err := c.decisionService.CompleteExecution(uint(id), req.ActualDuration)
	if err != nil {
		switch {
		case errors.Is(err, services.ErrLogNotFound):
			response.NotFound(ctx, "执行记录不存在")
		case errors.Is(err, services.ErrLogNotInProgress):
			response.Error(ctx, http.StatusConflict, "该记录不是执行中状态")
		case errors.Is(err, services.ErrInvalidDuration):
			response.BadRequest(ctx, "实际时长必须为正值")
		case errors.Is(err, services.ErrDurationExceedsPlan):
			response.Error(ctx, http.StatusUnprocessableEntity, "实际时长超过确认的计划时长")
		default:
			response.InternalServerError(ctx, err.Error())
		}
		return
	}

	response.Success(ctx, log)
}
