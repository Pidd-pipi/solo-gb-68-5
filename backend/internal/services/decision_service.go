package services

import (
	"errors"
	"fmt"
	"math"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/jackc/pgx/v5/pgconn"

	"irrigation/internal/models"
	"irrigation/pkg/database"
)

// 决策阈值与估算系数
const (
	RecentRainThresholdMm   = 5.0              // 近 24 小时降雨量阈值（mm），达到则不再灌溉
	ForecastRainThresholdMm = 5.0              // 未来 24 小时预报降雨阈值（mm），达到则推迟灌溉
	RecentRainWindow        = 24 * time.Hour   // 近期降雨统计窗口
	HumidityDataWindow      = 24 * time.Hour   // 土壤湿度数据有效窗口
	HumidityGainPerMinute   = 0.5              // 每分钟灌溉约提升的土壤湿度（%）
	WaterFlowPerSecond      = 0.1              // 估算用水量系数（升/秒），与调度器保持一致
	DecisionTTL             = 10 * time.Minute // 决策结果有效期，过期后不可用于确认执行
)

var (
	ErrZoneNotFound         = errors.New("zone not found")
	ErrDeviceUnavailable    = errors.New("no available irrigation device in zone")
	ErrIrrigationInProgress = errors.New("an irrigation task is already in progress for this zone")
	ErrLogNotFound          = errors.New("irrigation log not found")
	ErrLogNotInProgress     = errors.New("irrigation log is not in progress")
	ErrInvalidDuration      = errors.New("duration must be a positive number of seconds")
	ErrDurationExceedsPlan  = errors.New("actual duration exceeds the confirmed planned duration")
	ErrDecisionNotFound     = errors.New("irrigation decision not found")
	ErrDecisionExpired      = errors.New("irrigation decision has expired")
	ErrDecisionAlreadyUsed  = errors.New("irrigation decision has already been used")
	ErrDecisionZoneMismatch = errors.New("zone does not match the irrigation decision")
	ErrDurationExceedsLimit = errors.New("duration exceeds the max duration of the decision")
)

// DecisionRequest 智能灌溉决策请求
type DecisionRequest struct {
	ZoneID         uint    `json:"zone_id" binding:"required"`
	TargetHumidity float64 `json:"target_humidity" binding:"required,gt=0,lte=100"` // 目标土壤湿度（%）
	MaxDuration    int     `json:"max_duration" binding:"required,gt=0"`            // 最长灌溉时长（秒）
}

// DecisionResult 智能灌溉决策结果
type DecisionResult struct {
	DecisionID        uint      `json:"decision_id"` // 决策记录ID，确认执行时引用
	ZoneID            uint      `json:"zone_id"`
	ShouldIrrigate    bool      `json:"should_irrigate"`
	Reason            string    `json:"reason"`
	SuggestedDuration int       `json:"suggested_duration"` // 建议灌溉时长（秒）
	CurrentHumidity   *float64  `json:"current_humidity"`
	TargetHumidity    float64   `json:"target_humidity"`
	RecentRainfall    float64   `json:"recent_rainfall"`
	ForecastRainfall  float64   `json:"forecast_rainfall"`
	MaxDuration       int       `json:"max_duration"`
	ExpiresAt         time.Time `json:"expires_at"` // 决策有效期，过期后不可确认执行
}

type DecisionService struct {
	Forecast ForecastProvider
}

func NewDecisionService() *DecisionService {
	return &DecisionService{
		Forecast: NewSimulatedForecastProvider(),
	}
}

// Evaluate 结合最新土壤湿度、近期降雨和预报降雨，决策是否灌溉及建议时长
func (s *DecisionService) Evaluate(req DecisionRequest) (*DecisionResult, error) {
	var zone models.IrrigationZone
	if err := database.DB.First(&zone, req.ZoneID).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrZoneNotFound
		}
		return nil, err
	}

	result := &DecisionResult{
		ZoneID:         req.ZoneID,
		TargetHumidity: req.TargetHumidity,
		MaxDuration:    req.MaxDuration,
	}

	humidity, err := s.latestZoneHumidity(req.ZoneID)
	if err != nil {
		return nil, err
	}
	result.CurrentHumidity = humidity

	recentRain, err := s.zoneRecentRainfall(req.ZoneID, RecentRainWindow)
	if err != nil {
		return nil, err
	}
	result.RecentRainfall = recentRain

	forecast, err := s.Forecast.GetRainForecast(req.ZoneID)
	if err != nil {
		return nil, err
	}
	result.ForecastRainfall = forecast.ExpectedRainfall

	switch {
	case humidity == nil:
		result.Reason = "区域缺少土壤湿度数据，无法评估灌溉需求"
	case *humidity >= req.TargetHumidity:
		result.Reason = fmt.Sprintf("当前土壤湿度 %.1f%% 已达到目标 %.1f%%，无需灌溉", *humidity, req.TargetHumidity)
	case recentRain >= RecentRainThresholdMm:
		result.Reason = fmt.Sprintf("近24小时降雨 %.1fmm，土壤水分充足，无需灌溉", recentRain)
	case forecast.ExpectedRainfall >= ForecastRainThresholdMm:
		result.Reason = fmt.Sprintf("未来24小时预报降雨 %.1fmm（%s），建议推迟灌溉", forecast.ExpectedRainfall, forecast.Description)
	default:
		deficit := req.TargetHumidity - *humidity
		suggested := int(math.Ceil(deficit/HumidityGainPerMinute)) * 60
		if suggested > req.MaxDuration {
			suggested = req.MaxDuration
		}
		result.ShouldIrrigate = true
		result.SuggestedDuration = suggested
		result.Reason = fmt.Sprintf("当前土壤湿度 %.1f%% 低于目标 %.1f%%，建议灌溉 %d 分钟",
			*humidity, req.TargetHumidity, suggested/60)
	}

	// 决策结果持久化：确认执行必须引用有效（未过期、未使用、区域一致）的决策，
	// 计划时长不得超过该次决策的最长时长
	decision := &models.IrrigationDecision{
		ZoneID:            req.ZoneID,
		TargetHumidity:    req.TargetHumidity,
		MaxDuration:       req.MaxDuration,
		SuggestedDuration: result.SuggestedDuration,
		ShouldIrrigate:    result.ShouldIrrigate,
		Reason:            result.Reason,
		CurrentHumidity:   result.CurrentHumidity,
		RecentRainfall:    result.RecentRainfall,
		ForecastRainfall:  result.ForecastRainfall,
		Status:            models.DecisionStatusPending,
		ExpiresAt:         time.Now().Add(DecisionTTL),
	}
	if err := database.DB.Create(decision).Error; err != nil {
		return nil, err
	}
	result.DecisionID = decision.ID
	result.ExpiresAt = decision.ExpiresAt

	return result, nil
}

// ConfirmExecution 确认执行灌溉：必须引用同一次决策的有效结果（未过期、未使用、区域一致），
// 且计划时长不超过该次决策的最长时长；同时要求区域存在、区域内有可用灌溉设备且没有执行中的任务。
// 决策在同一事务内被标记为已使用，任何校验失败都会回滚，不落执行记录也不消耗决策。
func (s *DecisionService) ConfirmExecution(decisionID, zoneID uint, duration int) (*models.IrrigationLog, error) {
	if duration <= 0 {
		return nil, ErrInvalidDuration
	}

	var log *models.IrrigationLog

	err := database.DB.Transaction(func(tx *gorm.DB) error {
		// 锁定并校验决策结果
		var decision models.IrrigationDecision
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&decision, decisionID).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return ErrDecisionNotFound
			}
			return err
		}
		if !time.Now().Before(decision.ExpiresAt) {
			return ErrDecisionExpired
		}
		if decision.Status != models.DecisionStatusPending {
			return ErrDecisionAlreadyUsed
		}
		if decision.ZoneID != zoneID {
			return ErrDecisionZoneMismatch
		}
		if duration > decision.MaxDuration {
			return ErrDurationExceedsLimit
		}

		var zone models.IrrigationZone
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&zone, zoneID).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return ErrZoneNotFound
			}
			return err
		}

		var deviceCount int64
		if err := tx.Model(&models.Device{}).
			Where("zone_id = ? AND type IN ? AND status = ?",
				zoneID,
				[]models.DeviceType{models.DeviceTypeValve, models.DeviceTypePump},
				models.DeviceStatusOnline).
			Count(&deviceCount).Error; err != nil {
			return err
		}
		if deviceCount == 0 {
			return ErrDeviceUnavailable
		}

		var runningCount int64
		if err := tx.Model(&models.IrrigationLog{}).
			Where("zone_id = ? AND status = ?", zoneID, models.ExecutionStatusInProgress).
			Count(&runningCount).Error; err != nil {
			return err
		}
		if runningCount > 0 {
			return ErrIrrigationInProgress
		}

		log = &models.IrrigationLog{
			ZoneID:          &zoneID,
			TriggerType:     models.TriggerTypeManual,
			StartTime:       time.Now(),
			Status:          models.ExecutionStatusInProgress,
			PlannedDuration: &duration,
		}
		if err := tx.Create(log).Error; err != nil {
			if isUniqueViolation(err) {
				return ErrIrrigationInProgress
			}
			return err
		}

		// 标记决策已使用（条件更新兜底并发）
		res := tx.Model(&models.IrrigationDecision{}).
			Where("id = ? AND status = ?", decision.ID, models.DecisionStatusPending).
			Updates(map[string]interface{}{
				"status":           models.DecisionStatusConfirmed,
				"confirmed_log_id": log.ID,
			})
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected == 0 {
			return ErrDecisionAlreadyUsed
		}
		return nil
	})

	if err != nil {
		return nil, err
	}
	return log, nil
}

// CompleteExecution 结束灌溉执行：写回实际时长和估算用水量。
// 仅执行中的记录可完结；实际时长必须为正值且不超过确认时的计划时长，
// 超限或重复完成都会被拒绝，原记录保持不变。
func (s *DecisionService) CompleteExecution(logID uint, actualDuration int) (*models.IrrigationLog, error) {
	if actualDuration <= 0 {
		return nil, ErrInvalidDuration
	}

	err := database.DB.Transaction(func(tx *gorm.DB) error {
		var log models.IrrigationLog
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&log, logID).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return ErrLogNotFound
			}
			return err
		}
		if log.Status != models.ExecutionStatusInProgress {
			return ErrLogNotInProgress
		}
		if log.PlannedDuration != nil && actualDuration > *log.PlannedDuration {
			return ErrDurationExceedsPlan
		}

		waterUsage := float64(actualDuration) * WaterFlowPerSecond
		return tx.Model(&models.IrrigationLog{}).Where("id = ?", logID).Updates(map[string]interface{}{
			"end_time":    time.Now(),
			"duration":    actualDuration,
			"water_usage": waterUsage,
			"status":      models.ExecutionStatusSuccess,
		}).Error
	})
	if err != nil {
		return nil, err
	}

	var log models.IrrigationLog
	if err := database.DB.First(&log, logID).Error; err != nil {
		return nil, err
	}
	return &log, nil
}

// latestZoneHumidity 区域内各土壤传感器最新读数的平均值，无数据时返回 nil
func (s *DecisionService) latestZoneHumidity(zoneID uint) (*float64, error) {
	var avg *float64
	err := database.DB.Raw(`
		SELECT AVG(t.value) FROM (
			SELECT DISTINCT ON (device_id) value
			FROM sensor_data
			WHERE device_id IN (
				SELECT id FROM devices WHERE zone_id = ? AND type = ?
			) AND timestamp >= ?
			ORDER BY device_id, timestamp DESC
		) t`,
		zoneID, models.DeviceTypeSoilSensor, time.Now().Add(-HumidityDataWindow)).Scan(&avg).Error
	return avg, err
}

// zoneRecentRainfall 区域内全部雨量传感器在统计窗口内的累计降雨量（mm）
func (s *DecisionService) zoneRecentRainfall(zoneID uint, window time.Duration) (float64, error) {
	var total float64
	rainSensors := database.DB.Model(&models.Device{}).
		Select("id").
		Where("zone_id = ? AND type = ?", zoneID, models.DeviceTypeRainSensor)

	err := database.DB.Model(&models.SensorData{}).
		Select("COALESCE(SUM(value), 0)").
		Where("device_id IN (?) AND data_type = ? AND timestamp >= ?",
			rainSensors, "rainfall", time.Now().Add(-window)).
		Scan(&total).Error
	return total, err
}

func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}
